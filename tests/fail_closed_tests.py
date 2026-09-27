#!/usr/bin/env python3
"""
Fail-closed behaviour: the commands and validators must refuse.

Most of the engine's code volume is not the happy path — it is the guards
that stop a swap from doing something unsafe. A happy-path flow test never
enters them, so this suite deliberately supplies malformed, ambiguous,
out-of-order and unsafe input and asserts that each call **refuses**.

It needs no chain nodes: the one place a command would reach a chain
(`build_reconcile_report`) is stubbed, and the Nostr calls are answered by
the in-process relay stub with nothing in it.
"""

from __future__ import annotations

import hashlib
import contextlib
import io
import json
import os
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

sys.path.insert(0, str(HERE))
sys.path.insert(0, str(ROOT / "src"))

from nostr_relay_stub import NostrRelayStub  # noqa: E402

PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print(f"PASS  {name}")


def fail(name, detail):
    global FAIL
    FAIL += 1
    print(f"FAIL  {name}")
    for line in str(detail).splitlines():
        print(f"      {line}")


def refuses(name, fn):
    """Assert `fn` raises: the operation was refused."""
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            fn()
    except Exception:
        ok(name)
        return

    fail(name, "the operation was accepted instead of refused")


def accepts(name, fn):
    """Assert `fn` completes: read-only or benign operation."""
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
        return

    ok(name)


def captures(name, fn, *expected):
    """Assert `fn` completes and prints every expected fragment."""

    buffer = io.StringIO()

    try:
        with contextlib.redirect_stdout(buffer):
            fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
        return

    text = buffer.getvalue()
    missing = [item for item in expected if item not in text]

    if missing:
        fail(name, f"output omitted {missing}:\n{text[-600:]}")
        return

    ok(name)


def with_report(decision_class, fn):
    """
    Run `fn` with `build_reconcile_report` pinned to one decision class.

    `resume` and `resume-all` are entirely driven by that report, so
    pinning it is how each of their branches is reached without a chain.
    """

    original = swap_v2.build_reconcile_report

    swap_v2.build_reconcile_report = lambda swap: {
        "protocol_role": "alice",
        "decision_class": decision_class,
        "next_action": "wait",
        "reason": "stubbed",
        "read_only": True,
        "blake": {"status": "absent"},
        "btc": {"status": "absent"},
    }

    try:
        return fn()
    finally:
        swap_v2.build_reconcile_report = original


def did_not_act(name, fn, postcondition):
    """
    Assert an operation did not act.

    Several commands deliberately *report* "nothing to do" and return
    rather than raising, which is the correct contract for an empty work
    queue. Either outcome is a refusal; what matters is that the
    post-condition still shows nothing happened.
    """
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            fn()
    except Exception:
        ok(name)
        return

    if postcondition():
        ok(name)
    else:
        fail(name, "the operation acted when it should have refused")


def no_state_files():
    return not list(SCRATCH.glob("*.json"))


def no_funding_intents():
    intent_dir = SCRATCH / "funding-intents"

    if not intent_dir.exists():
        return True

    return not list(intent_dir.glob("*.json"))


# The relay stub is empty on purpose: an empty relay must not be mistaken
# for an error, and every "nothing found" path must refuse cleanly.
STUB = NostrRelayStub()
RELAY_URL = STUB.start()

SCRATCH = Path(tempfile.mkdtemp(prefix="blake-fail-closed-"))
KEY_FILE = SCRATCH / "participant.key"
KEY_FILE.write_text("11" * 32 + "\n")
KEY_FILE.chmod(0o600)

os.environ["BLAKE_SWAP_STATE_DIR"] = str(SCRATCH)
os.environ["BLAKE_SWAP_MESSAGE_DIR"] = str(SCRATCH / "messages")
os.environ["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(KEY_FILE)
os.environ["BLAKE_SWAP_TRANSPORT"] = "nostr"
os.environ["BLAKE_SWAP_NOSTR_RELAYS"] = RELAY_URL
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_FILE", None)
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_DIR", None)

import swap_v2  # noqa: E402

ME = swap_v2.current_nostr_pubkey()
OTHER = "bb" * 32

# Funding readiness and dismissal both read chain state; neither guard
# under test depends on it, so pin it to a settled report.
swap_v2.build_reconcile_report = lambda swap: {
    "protocol_role": "alice",
    "decision_class": "settled",
    "next_action": "none",
    "reason": "stubbed",
    "read_only": True,
    "blake": {"status": "absent"},
    "btc": {"status": "absent"},
}

swap_v2.btc_trade_minimum_sats = lambda: 1000


def write_swap(name, **overrides):
    record = {
        "swap_id": name,
        "state": "OFFERED",
        "created_at": 1700000000,
        "blake_amount": 0.1,
        "btc_sats": 25000,
        "secret_hex": None,
        "secret_hash": "cc" * 32,
        "blake_timeout": 1000,
        "btc_timeout": 900,
        "nostr_alice_pub": ME,
        "nostr_bob_pub": OTHER,
    }
    record.update(overrides)
    (SCRATCH / f"{name}.json").write_text(
        json.dumps(record, indent=2, sort_keys=True) + "\n"
    )
    return record


def clear_state():
    for path in SCRATCH.glob("*.json"):
        path.unlink()


# ---------------------------------------------------------------- tests


def test_new_rejects_half_specified_timeouts():
    refuses(
        "new refuses only one explicit timeout",
        lambda: swap_v2.command_new(
            SimpleNamespace(
                blake=1.0,
                sats=25000,
                btc_timeout=None,
                blake_timeout=500,
            )
        ),
    )

    refuses(
        "new refuses only the other explicit timeout",
        lambda: swap_v2.command_new(
            SimpleNamespace(
                blake=1.0,
                sats=25000,
                btc_timeout=500,
                blake_timeout=None,
            )
        ),
    )


def test_dismiss_prefix_guards():
    clear_state()

    refuses(
        "dismiss refuses a too-short prefix",
        lambda: swap_v2.command_dismiss(SimpleNamespace(swap_id="ab")),
    )

    refuses(
        "dismiss refuses an unknown prefix",
        lambda: swap_v2.command_dismiss(SimpleNamespace(swap_id="deadbeef")),
    )


def test_dismiss_ambiguous_prefix():
    clear_state()

    write_swap("aaaa11112222")
    write_swap("aaaa33334444")

    refuses(
        "dismiss refuses an ambiguous prefix",
        lambda: swap_v2.command_dismiss(SimpleNamespace(swap_id="aaaa")),
    )


def test_dismiss_refuses_funding_evidence():
    clear_state()

    write_swap(
        "bbbb11112222",
        blake_funding_txid="dd" * 32,
    )

    # Reconciliation is stubbed to report no chain funds, but the durable
    # record still shows funding evidence, so dismissal must refuse.
    refuses(
        "dismiss refuses a swap with recorded funding evidence",
        lambda: swap_v2.command_dismiss(SimpleNamespace(swap_id="bbbb11112222")),
    )


def test_accept_only_match_refuses_with_no_matches():
    clear_state()

    did_not_act(
        "accept-match imports nothing when no match exists",
        lambda: swap_v2.command_accept_only_match(
            SimpleNamespace(match_id=None)
        ),
        no_state_files,
    )

    refuses(
        "accept-match refuses an unknown prefix",
        lambda: swap_v2.command_accept_only_match(
            SimpleNamespace(match_id="deadbeef")
        ),
    )


def test_accept_reverse_swap_offer_refuses_unknown_match():
    clear_state()

    refuses(
        "accept-reverse-swap-offer refuses an unknown match",
        lambda: swap_v2.command_accept_reverse_swap_offer(
            SimpleNamespace(match_id="ff" * 32)
        ),
    )


def test_market_take_refuses_unknown_ids():
    clear_state()

    refuses(
        "take-market-offer refuses an unknown offer",
        lambda: swap_v2.command_take_market_offer(
            SimpleNamespace(offer_id="ff" * 32)
        ),
    )

    refuses(
        "accept-market-take refuses an unknown take",
        lambda: swap_v2.command_accept_market_take(
            SimpleNamespace(take_id="ff" * 32)
        ),
    )


def test_fund_ready_swap_refuses_with_nothing_ready():
    clear_state()

    did_not_act(
        "fund does nothing when no acceptance is ready",
        lambda: swap_v2.command_fund_ready_swap(SimpleNamespace()),
        no_funding_intents,
    )

    # A local swap that is not yet ready must still not fund anything.
    write_swap("cccc11112222", state="OFFERED")

    did_not_act(
        "fund does nothing for an unready local swap",
        lambda: swap_v2.command_fund_ready_swap(SimpleNamespace()),
        no_funding_intents,
    )


def test_trade_amount_floor():
    refuses(
        "validate_btc_trade_amount refuses below the floor",
        lambda: swap_v2.validate_btc_trade_amount(10),
    )

    accepts(
        "validate_btc_trade_amount accepts above the floor",
        lambda: swap_v2.validate_btc_trade_amount(1_000_000),
    )


def test_prefix_resolution_guards():
    refuses(
        "resolve_cached_nostr_prefix refuses a short prefix",
        lambda: swap_v2.resolve_cached_nostr_prefix(
            "abc", label="Nostr event", announce=False
        ),
    )

    refuses(
        "resolve_cached_nostr_prefix refuses a non-hex prefix",
        lambda: swap_v2.resolve_cached_nostr_prefix(
            "zzzzzzzzzzzz", label="Nostr event", announce=False
        ),
    )

    refuses(
        "resolve_cached_nostr_prefix refuses an unknown prefix",
        lambda: swap_v2.resolve_cached_nostr_prefix(
            "abcdef012345", label="Nostr event", announce=False
        ),
    )


def test_cache_guards():
    refuses(
        "the event cache refuses a short id",
        lambda: swap_v2.nostr_event_cache_path("abcd"),
    )

    refuses(
        "the event cache refuses a non-hex id",
        lambda: swap_v2.nostr_event_cache_path("z" * 64),
    )

    #
    # Settlement reconciliation must refuse a recovery result it cannot
    # trust. Each case stubs the one chain helper that produces it.
    #
    good_spend = {
        "txid": "aa" * 32,
        "blockhash": "bb" * 32,
        "branch": "claim",
    }

    for name, state, recovered, expected in (
        (
            "sync-settlement refuses a malformed recovery result",
            "BTC_FUNDED",
            "not a dict",
            "malformed result",
        ),
        (
            "sync-settlement refuses a result without txid/blockhash",
            "BTC_FUNDED",
            {"branch": "claim"},
            "missing txid/blockhash",
        ),
        (
            "sync-settlement refuses an unknown spend branch",
            "BTC_FUNDED",
            {
                "txid": "aa" * 32,
                "blockhash": "bb" * 32,
                "branch": "teleport",
            },
            "unknown branch",
        ),
        (
            "sync-settlement refuses a spend inconsistent with local state",
            "BTC_CLAIMED",
            {
                "txid": "aa" * 32,
                "blockhash": "bb" * 32,
                "branch": "refund",
            },
            "inconsistent",
        ),
    ):
        clear_state()

        write_swap(
            "eeee11112222",
            state=state,
            blake_funding_txid="11" * 32,
            blake_funding_vout=0,
            btc_funding_txid="22" * 32,
            btc_funding_vout=1,
        )

        original = swap_v2.find_confirmed_htlc_spend
        swap_v2.find_confirmed_htlc_spend = (
            lambda swap, chain, value=recovered: value
        )

        try:
            refuses(
                name,
                lambda: swap_v2.command_sync_settlement(
                    SimpleNamespace(
                        swap_id="eeee11112222",
                        role="alice",
                    )
                ),
            )
        finally:
            swap_v2.find_confirmed_htlc_spend = original

    #
    # A state the settlement reconciler has no rule for must be refused by
    # name rather than guessed at.
    #
    clear_state()
    write_swap("ffff11112222", state="BLAKE_CLAIMED")

    refuses(
        "sync-settlement refuses a state it has no rule for",
        lambda: swap_v2.command_sync_settlement(
            SimpleNamespace(
                swap_id="ffff11112222",
                role="alice",
            )
        ),
    )

    #
    # A half-populated recorded outpoint is corruption, not a normal
    # condition, and must fail closed rather than fall back to a scan.
    #
    clear_state()

    write_swap(
        "999911112222",
        state="OFFERED",
        btc_funding_txid="33" * 32,
    )

    original = swap_v2.recover_recorded_confirmed_funding
    original_scan = swap_v2.scan_chain_for_expected_funding

    swap_v2.recover_recorded_confirmed_funding = (
        lambda swap, chain: {
            "txid": "44" * 32,
            "vout": 0,
            "blockhash": "55" * 32,
        }
    )
    swap_v2.scan_chain_for_expected_funding = lambda swap, chain: None

    try:
        refuses(
            "funding reconciliation refuses a half-populated outpoint",
            lambda: swap_v2.command_sync_funding(
                SimpleNamespace(
                    swap_id="999911112222",
                    role="alice",
                )
            ),
        )
    finally:
        swap_v2.recover_recorded_confirmed_funding = original
        swap_v2.scan_chain_for_expected_funding = original_scan

    #
    # Reorg recovery of a final settlement. A recorded final claim or
    # refund that has disappeared must be rolled back only after the
    # other chain proves the earlier state is authoritative again; when
    # it does, the rollback is reported and the swap continues
    # reconciling from the recovered state.
    #
    for name, state, hook, expected in (
        (
            "sync-settlement reports a recovered final BLAKE claim",
            "COMPLETE",
            "recover_final_blake_claim_reorg",
            "FINAL BLAKE CLAIM REORG RECOVERED",
        ),
        (
            "sync-settlement reports a recovered final BLAKE refund",
            "BLAKE_REFUNDED",
            "recover_final_blake_refund_reorg",
            "FINAL BLAKE REFUND REORG RECOVERED",
        ),
    ):
        clear_state()

        write_swap(
            "777711112222",
            state=state,
            blake_funding_txid="11" * 32,
            blake_funding_vout=0,
            btc_funding_txid="22" * 32,
            btc_funding_vout=1,
            blake_claim_txid="66" * 32,
            blake_claim_blockhash="77" * 32,
            blake_refund_txid="88" * 32,
            blake_refund_blockhash="99" * 32,
        )

        original_hook = getattr(swap_v2, hook)
        original_spend = swap_v2.find_confirmed_htlc_spend
        original_scan = swap_v2.scan_chain_for_expected_funding

        setattr(swap_v2, hook, lambda swap: True)
        # Nothing further is on chain in this scratch state.
        swap_v2.find_confirmed_htlc_spend = lambda swap, chain: None
        swap_v2.scan_chain_for_expected_funding = lambda swap, chain: None

        try:
            captures(
                name,
                lambda: swap_v2.command_sync_settlement(
                    SimpleNamespace(
                        swap_id="777711112222",
                        role="alice",
                    )
                ),
                expected,
            )
        finally:
            setattr(swap_v2, hook, original_hook)
            swap_v2.find_confirmed_htlc_spend = original_spend
            swap_v2.scan_chain_for_expected_funding = original_scan

    #
    # `resume`'s terminal and ambiguous-prefix branches. These read the
    # local records directly, so they need no chain and no relay.
    #
    clear_state()

    for name, state in (
        ("aaaa11112222", "BLAKE_REFUNDED"),
        ("aaaa33334444", "BLAKE_REFUNDED"),
    ):
        (SCRATCH / f"{name}.json").write_text(
            json.dumps({"swap_id": name, "state": state}) + "\n"
        )

    captures(
        "resume reports an already-refunded swap",
        lambda: swap_v2.command_resume(
            SimpleNamespace(swap_id="aaaa11112222")
        ),
        "SWAP ALREADY REFUNDED",
    )

    refuses(
        "resume refuses a prefix matching two terminal swaps",
        lambda: swap_v2.command_resume(
            SimpleNamespace(swap_id="aaaa")
        ),
    )

    clear_state()
    write_swap("bbbb11112222", state="OFFERED")
    write_swap("bbbb33334444", state="OFFERED")

    with_report(
        "waiting",
        lambda: refuses(
            "resume refuses an ambiguous live prefix",
            lambda: swap_v2.command_resume(
                SimpleNamespace(swap_id="bbbb")
            ),
        ),
    )

    clear_state()
    write_swap("cccc11112222", state="OFFERED")

    with_report(
        "manual-attention",
        lambda: captures(
            "resume lists attention when nothing is resumable",
            lambda: swap_v2.command_resume(SimpleNamespace(swap_id=None)),
            "NO RESUMABLE SWAPS",
            "NEEDS MANUAL ATTENTION",
        ),
    )

    clear_state()

    # SECRET_REVEALED is an intermediate state that must never exist
    # independently on disk; settlement reconciliation must refuse it by
    # name rather than trying to scan a chain from it. This runs last
    # because it leaves a state file behind on purpose.
    clear_state()
    write_swap("dddd11112222", state="SECRET_REVEALED")

    refuses(
        "sync-settlement refuses the SECRET_REVEALED intermediate state",
        lambda: swap_v2.command_sync_settlement(
            SimpleNamespace(
                swap_id="dddd11112222",
                role="alice",
            )
        ),
    )

    # A cache miss is a normal "not cached" answer, not an error.
    if swap_v2.nostr_load_cached_event("ab" * 32) is None:
        ok("a cache miss returns None rather than a stale event")
    else:
        fail(
            "a cache miss returns None rather than a stale event",
            "an unknown id produced an event",
        )


def test_market_offer_parser_guards():
    """Structurally invalid offers must be rejected, not accepted."""

    def offer_event(content, tags):
        event = {
            "pubkey": ME,
            "created_at": 1700000000,
            "kind": 1,
            "tags": tags,
            "content": json.dumps(content),
        }
        event_id = swap_v2.nostr_event_id(event)

        from coincurve import PrivateKey

        key = PrivateKey(bytes.fromhex("11" * 32))
        event["id"] = event_id.hex()
        event["sig"] = key.sign_schnorr(event_id).hex()
        return event

    good = {
        "type": "market-offer",
        "version": 2,
        "base": "BLAKE",
        "quote": "BTC",
        "direction": "blake-for-btc",
        "blake_amount": "1.0",
        "btc_sats": 25000,
        "created_at": 1700000000,
        "expires_at": 1700003600,
        "nonce": "00" * 8,
    }

    good_tags = [
        ["t", "blake-swap-market"],
        ["base", "BLAKE"],
        ["quote", "BTC"],
        ["direction", "blake-for-btc"],
    ]

    accepts(
        "a well-formed market offer parses",
        lambda: swap_v2.nostr_parse_market_offer_event(
            offer_event(good, good_tags)
        ),
    )

    for label, mutate, tags in (
        (
            "a wrong base",
            {"base": "BTC"},
            good_tags,
        ),
        (
            "a wrong direction",
            {"direction": "sideways"},
            good_tags,
        ),
        (
            "a missing direction tag",
            {},
            [t for t in good_tags if t[0] != "direction"],
        ),
        (
            "a non-positive amount",
            {"blake_amount": "0"},
            good_tags,
        ),
        (
            "an expiry before creation",
            {"expires_at": 1},
            good_tags,
        ),
        (
            "smuggled settlement material",
            {"secret_hash": "cc" * 32},
            good_tags,
        ),
    ):
        broken = dict(good)
        broken.update(mutate)

        refuses(
            f"the market offer parser refuses {label}",
            lambda broken=broken, tags=tags: swap_v2.nostr_parse_market_offer_event(
                offer_event(broken, tags)
            ),
        )


def test_market_take_validator_guards():
    """A take validated against the wrong offer must be refused."""

    from coincurve import PrivateKey

    key = PrivateKey(bytes.fromhex("11" * 32))

    def signed(content, tags, created_at=1700000000):
        event = {
            "pubkey": ME,
            "created_at": created_at,
            "kind": 1,
            "tags": tags,
            "content": json.dumps(content),
        }
        event_id = swap_v2.nostr_event_id(event)
        event["id"] = event_id.hex()
        event["sig"] = key.sign_schnorr(event_id).hex()
        return event

    offer = signed(
        {
            "type": "market-offer",
            "version": 2,
            "base": "BLAKE",
            "quote": "BTC",
            "direction": "blake-for-btc",
            "blake_amount": "1.0",
            "btc_sats": 25000,
            "created_at": 1700000000,
            "expires_at": 1700003600,
            "nonce": "00" * 8,
        },
        [
            ["t", "blake-swap-market"],
            ["base", "BLAKE"],
            ["quote", "BTC"],
            ["direction", "blake-for-btc"],
        ],
    )

    offer_message, _parsed = swap_v2.nostr_parse_market_offer_event(offer)

    good_take = {
        "type": "market-take",
        "version": 2,
        "offer_id": offer["id"],
        "maker": ME,
        "taker": ME,
        "base": "BLAKE",
        "quote": "BTC",
        "direction": "blake-for-btc",
        "maker_role": "alice",
        "taker_role": "bob",
        "blake_amount": "1.0",
        "btc_sats": 25000,
        "created_at": 1700000100,
        "nonce": "11" * 8,
    }

    good_tags = [
        ["t", "blake-swap-market-take"],
        ["e", offer["id"]],
        ["p", ME],
        ["direction", "blake-for-btc"],
    ]

    def validate(take):
        return swap_v2.nostr_validate_market_take_event(
            take,
            offer,
            offer_message,
            offer["id"],
        )

    refuses(
        "the take validator refuses a take for another offer",
        lambda: validate(
            signed(
                dict(good_take, offer_id="ab" * 32),
                good_tags,
                1700000100,
            )
        ),
    )

    refuses(
        "the take validator refuses changed terms",
        lambda: validate(
            signed(
                dict(good_take, btc_sats=1),
                good_tags,
                1700000100,
            )
        ),
    )

    refuses(
        "the take validator refuses a mismatched version",
        lambda: validate(
            signed(dict(good_take, version=1), good_tags, 1700000100)
        ),
    )

    refuses(
        "the take validator refuses an inverted role",
        lambda: validate(
            signed(
                dict(good_take, maker_role="bob", taker_role="alice"),
                good_tags,
                1700000100,
            )
        ),
    )


def test_next_is_safe_with_nothing_to_do():
    clear_state()

    accepts(
        "next reports nothing to do on an empty participant",
        lambda: swap_v2.command_next(SimpleNamespace()),
    )


def guard(name, fn):
    """Run one test function, surfacing a crash as a single failure."""
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"crashed: {type(e).__name__}: {e}")


def main():
    print()
    print("=" * 72)
    print("FAIL-CLOSED BEHAVIOUR")
    print("=" * 72)
    print()

    try:
        guard("new timeout pairing", test_new_rejects_half_specified_timeouts)
        guard("dismiss prefix guards", test_dismiss_prefix_guards)
        guard("dismiss ambiguous prefix", test_dismiss_ambiguous_prefix)
        guard("dismiss funding evidence", test_dismiss_refuses_funding_evidence)
        guard("accept-match with no matches", test_accept_only_match_refuses_with_no_matches)
        guard("accept-reverse-swap-offer unknown match", test_accept_reverse_swap_offer_refuses_unknown_match)
        guard("market take unknown ids", test_market_take_refuses_unknown_ids)
        guard("fund readiness", test_fund_ready_swap_refuses_with_nothing_ready)
        guard("trade amount floor", test_trade_amount_floor)
        guard("prefix resolution guards", test_prefix_resolution_guards)
        guard("cache guards", test_cache_guards)
        guard("market offer parser guards", test_market_offer_parser_guards)
        guard("market take validator guards", test_market_take_validator_guards)
        guard("next with nothing to do", test_next_is_safe_with_nothing_to_do)
    finally:
        STUB.stop()

    print()
    print("=" * 72)
    print("FAIL-CLOSED SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
