#!/usr/bin/env python3
"""
End-to-end market swap: the marketplace path, on regtest, over a local
relay stub.

Every other integration suite drives the *direct* protocol path
(new -> publish-offer -> accept-offer). This one drives the marketplace
path instead, which is where most of the acceptance machinery lives:

    maker  publish-market-offer
    taker  take-market-offer
    maker  accept-market-take      (creates the swap, publishes the
                                    protocol offer and the match)
    taker  accept-market-match     (imports the swap, publishes accept)
    ...    then the ordinary settlement sequence to COMPLETE

It needs the two-chain regtest rig for settlement, and the in-process
relay stub for coordination, so it exercises both together.
"""

from __future__ import annotations

import asyncio
import contextlib
import io
import json
import os
import sys
import tempfile
import time
from pathlib import Path


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

sys.path.insert(0, str(ROOT / "src"))
sys.path.insert(0, str(HERE))

from nostr_relay_stub import NostrRelayStub  # noqa: E402

import swap_protocol_tests as h  # noqa: E402

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


def check(name, fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
    else:
        ok(name)


# ---------------------------------------------------------------- setup

STUB = NostrRelayStub()
RELAY_URL = STUB.start()

BASE = Path(tempfile.mkdtemp(prefix="blake-market-flow-"))

PROFILES = {
    "alice": {
        "state": BASE / "alice",
        "key": BASE / "alice" / "participant.key",
        "wallet": h.ALICE_WALLET,
        "secret": "11" * 32,
    },
    "bob": {
        "state": BASE / "bob",
        "key": BASE / "bob" / "participant.key",
        "wallet": h.BOB_WALLET,
        "secret": "22" * 32,
    },
}

for profile in PROFILES.values():
    profile["state"].mkdir(parents=True, exist_ok=True)
    profile["key"].write_text(profile["secret"] + "\n")
    profile["key"].chmod(0o600)


def env_for(role):
    profile = PROFILES[role]

    return {
        "BLAKE_SWAP_TRANSPORT": "nostr",
        "BLAKE_SWAP_NOSTR_RELAYS": RELAY_URL,
        "BLAKE_SWAP_ROLE": role,
        "BLAKE_SWAP_STATE_DIR": str(profile["state"]),
        "BLAKE_SWAP_MESSAGE_DIR": str(profile["state"] / "messages"),
        "BLAKE_SWAP_CHAIN_WALLET": profile["wallet"],
        "BLAKE_SWAP_PARTICIPANT_KEY_FILE": str(profile["key"]),
    }


def run(role, *args, expect_ok=True):
    return h.run(
        [sys.executable, h.SWAP_V2, *args],
        env=env_for(role),
        expect_ok=expect_ok,
    )


def stub_events(tag):
    return [
        event
        for event in STUB.events.values()
        if any(
            len(t) >= 2 and t[0] == "t" and t[1] == tag
            for t in event.get("tags", [])
        )
    ]


def newest(tag):
    events = stub_events(tag)

    if not events:
        raise AssertionError(f"no {tag} event was published")

    return sorted(
        events,
        key=lambda e: (int(e.get("created_at", 0)), e.get("id", "")),
    )[-1]


def load_state(role, swap_id):
    path = PROFILES[role]["state"] / f"{swap_id}.json"

    if not path.exists():
        raise AssertionError(f"no {role} state for {swap_id}")

    return json.loads(path.read_text())


def eventually(fn, attempts=20, delay=0.1):
    result = None

    for _ in range(attempts):
        result = fn()

        if result:
            return result

        time.sleep(delay)

    return result


# ------------------------------------------------------------ the flow


def drive_market_swap():
    """Run the whole marketplace swap and return the swap id."""
    quiet = contextlib.redirect_stdout(io.StringIO())

    # 1. Maker advertises.
    with quiet:
        run(
            "alice",
            "publish-market-offer",
            "--blake", "0.2",
            "--sats", "30000",
            "--minutes", "30",
            "--direction", "blake-for-btc",
        )

    offer_event = newest("blake-swap-market")
    offer_id = offer_event["id"]

    # 2. Canonical market state reports it OPEN for the maker.
    with quiet:
        run("alice", "market-state", "--limit", "50", "--json")

    # 3. A read-only inspection of the offer builds a take without publishing.
    with quiet:
        run("alice", "inspect-market-take", offer_id)

    # 4. Taker takes it.
    with quiet:
        run("bob", "take-market-offer", offer_id)

    take_event = newest("blake-swap-market-take")
    take_id = take_event["id"]

    # 5. Maker lists takes in machine-readable form.
    with quiet:
        run("alice", "market-takes", offer_id, "--json")

    # 6. Maker accepts the take: this creates the swap and publishes both
    #    the protocol offer and the market match.
    with quiet:
        run("alice", "accept-market-take", take_id)

    match_event = newest("blake-swap-market-match")
    match_id = match_event["id"]
    swap_id = json.loads(match_event["content"])["swap_id"]

    #
    # Re-accepting the same take must be idempotent: a retry after a
    # crash must not reserve a second swap, and must say so.
    #
    with quiet:
        repeat = run("alice", "accept-market-take", take_id)

    assert "MARKET TAKE ALREADY ACCEPTED" in repeat.stdout, repeat.stdout

    assert json.loads(
        newest("blake-swap-market-match")["content"]
    )["swap_id"] == swap_id, "a repeat acceptance reserved a second swap"

    # 7. Maker lists authenticated matches.
    with quiet:
        run("alice", "market-matches")

    # 8. Taker imports the match and publishes accept.
    with quiet:
        run("bob", "accept-market-match", match_id)

    assert load_state("bob", swap_id)["state"] == "OFFERED", load_state("bob", swap_id)

    #
    # Re-importing the same match must not overwrite live state either.
    #
    with quiet:
        repeat = run("bob", "accept-market-match", match_id)

    assert "MARKET MATCH ALREADY IMPORTED" in repeat.stdout, repeat.stdout

    assert load_state("bob", swap_id)["state"] == "OFFERED", (
        "a repeat import changed Bob's checkpoint"
    )

    # 9. Ordinary settlement from here.
    sequence = [
        ("alice", "receive-accept"),
        ("alice", "fund-blake"),
        ("alice", "publish-blake-funded"),
        ("bob", "receive-blake-funded"),
        ("bob", "fund-btc"),
        ("bob", "publish-btc-funded"),
        ("alice", "receive-btc-funded"),
        ("alice", "claim-btc"),
        ("alice", "publish-btc-claimed"),
        ("bob", "receive-btc-claimed"),
        ("bob", "extract-secret"),
        ("bob", "claim-blake"),
        ("bob", "publish-blake-claimed"),
        ("alice", "receive-blake-claimed"),
    ]

    for role, command in sequence:
        with quiet:
            run(role, command, swap_id)

    return swap_id, offer_id


SWAP_ID = None
OFFER_ID = None
TAKE_ID = None
MATCH_ID = None


def test_full_market_swap():
    global SWAP_ID, OFFER_ID, TAKE_ID, MATCH_ID

    SWAP_ID, OFFER_ID = drive_market_swap()
    MATCH_ID = newest("blake-swap-market-match")["id"]
    TAKE_ID = json.loads(newest("blake-swap-market-match")["content"]).get("take_id")

    alice = load_state("alice", SWAP_ID)
    bob = load_state("bob", SWAP_ID)

    assert alice["state"] == "BLAKE_CLAIMED", alice["state"]
    assert bob["state"] == "BLAKE_CLAIMED", bob["state"]


def test_secret_learned_only_from_chain():
    bob = load_state("bob", SWAP_ID)
    alice = load_state("alice", SWAP_ID)

    # Bob recovered S from the confirmed Bitcoin claim, not from Nostr.
    assert bob.get("revealed_secret_hex"), "Bob did not recover the secret"
    assert bob["revealed_secret_hex"] == alice["secret_hex"], (
        "recovered secret does not match Alice's"
    )

    # Bob must never hold Alice's own secret field.
    assert not bob.get("secret_hex"), "Bob holds secret_hex"


def test_no_message_carried_the_secret():
    for event in STUB.events.values():
        assert "secret_hex" not in event.get("content", ""), (
            "a coordination event mentioned secret_hex"
        )


def test_both_complete():
    with contextlib.redirect_stdout(io.StringIO()):
        run("alice", "complete", SWAP_ID, "--role", "alice")
        run("bob", "complete", SWAP_ID, "--role", "bob")

    assert load_state("alice", SWAP_ID)["state"] == "COMPLETE"
    assert load_state("bob", SWAP_ID)["state"] == "COMPLETE"


def test_reconcile_is_read_only_and_settled():
    # h.run captures the subprocess stdout, so read it from the result
    # rather than redirecting this process's stdout.
    result = run("alice", "reconcile", SWAP_ID, "--json")

    text = result.stdout.strip()
    payload = json.loads(text[text.index("{"):])

    assert payload["read_only"] is True, payload
    assert payload["checkpoint_state"] == "COMPLETE", payload


def test_cancel_flow():
    quiet = contextlib.redirect_stdout(io.StringIO())

    with quiet:
        run(
            "alice",
            "publish-market-offer",
            "--blake", "0.5",
            "--sats", "90000",
            "--minutes", "30",
            "--direction", "blake-for-btc",
        )

    offer_id = newest("blake-swap-market")["id"]

    with quiet:
        run("alice", "cancel-market-offer", offer_id)

    assert newest("blake-swap-market-cancel")["id"], "cancel was not published"


def test_publish_market_match_is_available_explicitly():
    """
    `publish-market-match` is the operator-facing form of the step
    `accept-market-take` performs internally. It must be usable on its
    own and must not corrupt the coordination record when re-run.
    """

    assert TAKE_ID, "the flow did not record a take id"

    before = len(stub_events("blake-swap-market-match"))

    result = run("alice", "publish-market-match", TAKE_ID)
    text = result.stdout + result.stderr

    assert "MARKET" in text, text

    events = stub_events("blake-swap-market-match")

    assert len(events) >= before, "publish-market-match removed the match"

    swaps = {
        json.loads(event["content"]).get("swap_id")
        for event in events
    }

    assert SWAP_ID in swaps, swaps

    # Re-running it must not disturb the swap or the relay.
    state_before = load_state("alice", SWAP_ID)
    again = run("alice", "publish-market-match", TAKE_ID)

    assert json.loads(
        newest("blake-swap-market-match")["content"]
    )["swap_id"] == SWAP_ID, again.stdout

    assert load_state("alice", SWAP_ID)["state"] == state_before["state"], (
        "publish-market-match changed the swap state"
    )


def main():
    print()
    print("=" * 72)
    print("END-TO-END MARKET SWAP (regtest + local relay stub)")
    print("=" * 72)
    print()
    print("relay:", RELAY_URL)
    print("state:", BASE)
    print()

    try:
        h.preflight()
        ok("preflight")

        check("the marketplace path completes a full atomic swap", test_full_market_swap)
        check("Bob learns the secret from the chain, not from Nostr", test_secret_learned_only_from_chain)
        check("no coordination event ever carried secret_hex", test_no_message_carried_the_secret)
        check("both participants reach COMPLETE", test_both_complete)
        check("read-only reconciliation reports a settled swap", test_reconcile_is_read_only_and_settled)
        check("a market offer can be cancelled", test_cancel_flow)
        check("publish-market-match is available explicitly", test_publish_market_match_is_available_explicitly)
    finally:
        STUB.stop()

    print()
    print("=" * 72)
    print("MARKET FLOW SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()
    print(f"swap: {SWAP_ID}")
    print(
        f"stub traffic: connections={STUB.connections} "
        f"published={STUB.published} requests={STUB.requests}"
    )
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
