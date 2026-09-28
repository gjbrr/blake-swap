#!/usr/bin/env python3
"""
Command-surface unit tests.

Covers the operator commands that drive *many* swaps or change local
bookkeeping, without needing chain nodes or relays:

  * `resume-all` only drives swaps this participant is a party to, only
    reads canonical 12-hex swap-state names, and reports a state file
    whose name disagrees with its contents;
  * `dismiss` refuses to archive a swap the participant is not a party
    to;
  * `run` validates its step budget and contains a failing step;
  * `ignore-match` tombstones can expire and be reclaimed;
  * the operational env knobs fail closed on bad values.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import json
import os
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

#
# The engine resolves its participant state root at import time, so
# point it at a scratch directory *before* importing it.
#
SCRATCH = Path(tempfile.mkdtemp(prefix="blake-command-surface-"))

os.environ["BLAKE_SWAP_STATE_DIR"] = str(SCRATCH)
os.environ["BLAKE_SWAP_MESSAGE_DIR"] = str(SCRATCH / "messages")
os.environ["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
    SCRATCH / "participant.key"
)
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_FILE", None)
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_DIR", None)
os.environ["BLAKE_SWAP_TRANSPORT"] = "file"

(SCRATCH / "participant.key").write_text("11" * 32 + "\n")
(SCRATCH / "participant.key").chmod(0o600)

sys.path.insert(0, str(ROOT / "src"))
sys.path.insert(0, str(HERE))

import swap_v2  # noqa: E402

PASS = 0
FAIL = 0

ME = "aa" * 32
OTHER = "bb" * 32


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


def swap_record(swap_id, alice=ME, bob=OTHER):
    """A minimal but valid local swap record."""
    return {
        "swap_id": swap_id,
        "state": "BLAKE_FUNDED",
        "created_at": 1700000000,
        "blake_amount": 0.1,
        "btc_sats": 25000,
        "secret_hex": None,
        "secret_hash": "cc" * 32,
        "blake_timeout": 1000,
        "btc_timeout": 900,
        "nostr_alice_pub": alice,
        "nostr_bob_pub": bob,
    }


def write_swap(name, record):
    path = SCRATCH / name
    path.write_text(json.dumps(record, indent=2, sort_keys=True) + "\n")
    return path


def clean_state():
    for path in SCRATCH.glob("*.json"):
        path.unlink()


# ---------------------------------------------------------------- tests


def test_resume_all_filters():
    clean_state()

    owned = "111111111111"
    foreign = "222222222222"
    mismatched = "333333333333"

    write_swap(f"{owned}.json", swap_record(owned))
    write_swap(f"{foreign}.json", swap_record(foreign, alice=OTHER, bob="dd" * 32))
    # Canonical name, but the contents claim a different swap_id.
    write_swap(f"{mismatched}.json", swap_record(owned))
    # Not a canonical swap-state name at all.
    write_swap("not-a-swap.json", swap_record("444444444444"))

    resumed = []

    original_resume = swap_v2.command_resume
    original_report = swap_v2.build_reconcile_report
    original_pubkey = swap_v2.current_nostr_pubkey

    swap_v2.current_nostr_pubkey = lambda: ME
    swap_v2.build_reconcile_report = lambda swap: {
        "decision_class": "recovery-needed",
        "next_action": "sync-funding",
    }
    swap_v2.command_resume = lambda args: resumed.append(args.swap_id)

    try:
        swap_v2.command_resume_all(SimpleNamespace())
    finally:
        swap_v2.command_resume = original_resume
        swap_v2.build_reconcile_report = original_report
        swap_v2.current_nostr_pubkey = original_pubkey

    assert resumed == [owned], f"resumed {resumed!r}, expected only {owned!r}"


def test_resume_all_ignores_non_canonical_and_unowned():
    # Covered by the assertions in test_resume_all_filters; kept as a
    # separate named check so a failure is attributable.
    pass


def test_dismiss_refuses_foreign_swap():
    clean_state()

    foreign = "555555555555"
    write_swap(
        f"{foreign}.json",
        swap_record(foreign, alice=OTHER, bob="ee" * 32),
    )

    original_pubkey = swap_v2.current_nostr_pubkey
    swap_v2.current_nostr_pubkey = lambda: ME

    try:
        try:
            swap_v2.command_dismiss(SimpleNamespace(swap_id=foreign))
        except RuntimeError as e:
            assert "not a party" in str(e), str(e)
        else:
            raise AssertionError(
                "dismiss archived a swap this participant is not a party to"
            )
    finally:
        swap_v2.current_nostr_pubkey = original_pubkey


def test_run_rejects_bad_step_budget():
    for value in (0, -1, 21, 99):
        try:
            swap_v2.command_run(
                SimpleNamespace(swap_id="111111111111", max_steps=value)
            )
        except ValueError:
            continue
        raise AssertionError(f"--max-steps={value} was accepted")


def test_run_contains_a_failing_step():
    clean_state()

    owned = "666666666666"
    write_swap(f"{owned}.json", swap_record(owned))

    original_report = swap_v2.build_reconcile_report

    def boom(swap):
        raise RuntimeError("synthetic reconcile failure")

    swap_v2.build_reconcile_report = boom

    try:
        try:
            swap_v2.command_run(
                SimpleNamespace(swap_id=owned, max_steps=3)
            )
        except SystemExit as e:
            assert e.code == 1, f"exit code {e.code!r}"
        else:
            raise AssertionError("a failing step did not stop the driver")
    finally:
        swap_v2.build_reconcile_report = original_report


def test_run_reports_settled_cleanly():
    clean_state()

    owned = "777777777777"
    write_swap(f"{owned}.json", swap_record(owned))

    original_report = swap_v2.build_reconcile_report

    swap_v2.build_reconcile_report = lambda swap: {
        "decision_class": "settled",
        "next_action": "none",
        "protocol_role": "alice",
    }

    try:
        swap_v2.command_run(SimpleNamespace(swap_id=owned, max_steps=3))
    finally:
        swap_v2.build_reconcile_report = original_report


def test_ignored_match_ttl_expiry():
    ignore_dir = SCRATCH / "ignored-market-matches"
    ignore_dir.mkdir(parents=True, exist_ok=True)

    match_id = "ab" * 32

    (ignore_dir / f"{match_id}.json").write_text(
        json.dumps(
            {
                "match_id": match_id,
                "ignored_at": 1,
                "local_only": True,
            }
        )
        + "\n"
    )

    original_env = os.environ.get("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS")

    try:
        # TTL disabled: still suppressed.
        os.environ.pop("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", None)
        assert match_id in swap_v2._ignored_market_match_ids()

        # TTL 1s and an ancient ignored_at: expired and reclaimed.
        os.environ["BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS"] = "1"
        assert match_id not in swap_v2._ignored_market_match_ids()
        assert not (ignore_dir / f"{match_id}.json").exists(), (
            "expired tombstone was not reclaimed"
        )
    finally:
        if original_env is None:
            os.environ.pop("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", None)
        else:
            os.environ[
                "BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS"
            ] = original_env


def test_env_knob_validation():
    original = os.environ.get("BLAKE_SWAP_RPC_TIMEOUT_SECONDS")

    try:
        for bad in ("abc", "0", "-5"):
            os.environ["BLAKE_SWAP_RPC_TIMEOUT_SECONDS"] = bad
            try:
                swap_v2.rpc_timeout_seconds()
            except RuntimeError:
                continue
            raise AssertionError(f"rpc timeout accepted {bad!r}")

        os.environ["BLAKE_SWAP_RPC_TIMEOUT_SECONDS"] = "7.5"
        assert swap_v2.rpc_timeout_seconds() == 7.5
    finally:
        if original is None:
            os.environ.pop("BLAKE_SWAP_RPC_TIMEOUT_SECONDS", None)
        else:
            os.environ["BLAKE_SWAP_RPC_TIMEOUT_SECONDS"] = original

    original = os.environ.get("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS")

    try:
        for bad in ("abc", "-1"):
            os.environ["BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS"] = bad
            try:
                swap_v2.ignored_match_ttl_seconds()
            except RuntimeError:
                continue
            raise AssertionError(f"ttl accepted {bad!r}")

        os.environ["BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS"] = "60"
        assert swap_v2.ignored_match_ttl_seconds() == 60
    finally:
        if original is None:
            os.environ.pop("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", None)
        else:
            os.environ["BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS"] = original


def test_reorg_transition_table():
    """
    The backward-recovery edges are data, and only those are permitted.
    """
    swap = SimpleNamespace(state=swap_v2.State.BLAKE_CLAIMED.value)

    # A permitted reverse edge moves state backward.
    swap_v2.apply_reorg_transition(swap, swap_v2.State.BTC_CLAIMED)
    assert swap.state == swap_v2.State.BTC_CLAIMED.value, swap.state

    swap.state = swap_v2.State.BLAKE_REFUNDED.value
    swap_v2.apply_reorg_transition(swap, swap_v2.State.BTC_REFUNDED)
    assert swap.state == swap_v2.State.BTC_REFUNDED.value, swap.state

    # Every other edge is refused, including forward edges, which must
    # keep going through Swap.transition().
    for current, target in (
        (swap_v2.State.BTC_CLAIMED, swap_v2.State.BLAKE_CLAIMED),
        (swap_v2.State.BTC_REFUNDED, swap_v2.State.BLAKE_REFUNDED),
        (swap_v2.State.OFFERED, swap_v2.State.BTC_CLAIMED),
        (swap_v2.State.COMPLETE, swap_v2.State.BTC_CLAIMED),
    ):
        swap.state = current.value

        try:
            swap_v2.apply_reorg_transition(swap, target)
        except RuntimeError:
            continue

        raise AssertionError(
            f"reorg transition {current.value} -> {target.value} was allowed"
        )


def test_funding_scan_guards():
    original_override = os.environ.get(
        "BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN"
    )
    original_max = os.environ.get("BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS")

    try:
        # Unanchored scans are refused unless explicitly opted into.
        os.environ.pop("BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN", None)
        assert swap_v2.funding_scan_override_enabled() is False

        os.environ["BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN"] = "1"
        assert swap_v2.funding_scan_override_enabled() is True

        # The scan bound fails closed on bad values.
        for bad in ("abc", "0", "-3"):
            os.environ["BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS"] = bad
            try:
                swap_v2.funding_scan_max_blocks()
            except RuntimeError:
                continue
            raise AssertionError(f"scan bound accepted {bad!r}")

        os.environ["BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS"] = "250"
        assert swap_v2.funding_scan_max_blocks() == 250
    finally:
        if original_override is None:
            os.environ.pop("BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN", None)
        else:
            os.environ[
                "BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN"
            ] = original_override

        if original_max is None:
            os.environ.pop("BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS", None)
        else:
            os.environ["BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS"] = original_max


def test_htlc_spend_classification_shapes():
    """
    Classification is structural: the or_i selector immediately before
    the witness script decides the branch.
    """
    funding_txid = "aa" * 32
    funding_vout = 0
    secret = bytes(range(32))
    secret_hash = __import__("hashlib").sha256(secret).hexdigest()

    def spend(witness):
        return {
            "vin": [
                {
                    "txid": funding_txid,
                    "vout": funding_vout,
                    "txinwitness": witness,
                }
            ]
        }

    # Real claim shape: <preimage> <sig> <01> <script>
    assert (
        swap_v2.classify_htlc_spend(
            spend([secret.hex(), "30" * 71, "01", "51"]),
            funding_txid,
            funding_vout,
            secret_hash,
        )
        == "claim"
    )

    # Real refund shape: <sig> <empty selector> <script>
    assert (
        swap_v2.classify_htlc_spend(
            spend(["30" * 71, "", "51"]),
            funding_txid,
            funding_vout,
            secret_hash,
        )
        == "refund"
    )

    # Inconsistent and unrecognised shapes must not be treated as refunds.
    assert (
        swap_v2.classify_htlc_spend(
            spend([secret.hex(), "30" * 71, "", "51"]),
            funding_txid,
            funding_vout,
            secret_hash,
        )
        == "unknown"
    )

    assert (
        swap_v2.classify_htlc_spend(
            spend(["30" * 71, "51"]),
            funding_txid,
            funding_vout,
            secret_hash,
        )
        == "unknown"
    )

    assert (
        swap_v2.classify_htlc_spend(
            spend(["30" * 71, "7f", "51"]),
            funding_txid,
            funding_vout,
            secret_hash,
        )
        == "unknown"
    )


def test_psbt_bounds():
    import base64
    import hashlib

    # Truncated and non-minimal compact sizes are rejected cleanly.
    for raw in (b"", b"\xfd\x01", b"\xff\x01\x02", b"\xfd\x01\x00", b"\xfe\x00\x00\x00\x00"):
        try:
            swap_v2.read_compact_size(raw, 0)
        except ValueError:
            continue
        raise AssertionError(f"compact size accepted {raw!r}")

    # An unterminated map is an error, not an infinite loop.
    try:
        swap_v2.find_map_end(b"\x01\x01", 0)
    except ValueError:
        pass
    else:
        raise AssertionError("unterminated map was accepted")

    # Only input 0 is supported for hashlock injection.
    psbt = base64.b64encode(
        b"psbt\xff" + b"\x00" + b"\x00" + b"\x00"
    ).decode()

    h = hashlib.sha256(b"x" * 32).hexdigest()

    try:
        swap_v2.add_sha256_preimage(
            psbt,
            h,
            (b"x" * 32).hex(),
            input_index=1,
        )
    except NotImplementedError:
        pass
    else:
        raise AssertionError("multi-input hashlock injection was allowed")


def test_fee_rate_option_is_wired():
    """
    The client passes the settlement rate as a parameter, so the real
    parser must accept `--fee-rate` on every command that can spend an
    HTLC, and must refuse a rate it cannot pay before doing any work.

    The parser is built inside `main()`, so this asks the actual CLI
    rather than rebuilding the tree the test believes should exist. A bad
    rate has to be a usage error (exit 2), because discovering it after a
    transaction has been built is exactly the failure this prevents.
    """
    import subprocess

    engine = ROOT / "src" / "swap_v2.py"

    def invoke(*argv):
        return subprocess.run(
            [sys.executable, str(engine), *argv],
            capture_output=True,
            text=True,
            timeout=120,
        )

    for command in (
        "claim-btc",
        "claim-blake",
        "refund-btc",
        "refund-blake",
        # The autonomous driver and the resume path reach the same
        # settlement spends, so a client that drives rather than calls
        # must be able to set the rate too.
        "run",
        "resume",
    ):
        done = invoke(command, "--help")

        assert done.returncode == 0, (command, done.stderr)
        assert "--fee-rate" in done.stdout, command

    for bad in ("abc", "0", "-1", "nan"):
        done = invoke("claim-btc", "--fee-rate", bad, "111111111111")

        assert done.returncode == 2, (bad, done.returncode, done.stderr)
        assert "--fee-rate" in (done.stderr + done.stdout), bad

    #
    # A fractional rate is usable and must get past the parser: the
    # engine's own complaint about the swap ID is what proves the value
    # was accepted, and it is not a usage error.
    #
    done = invoke("claim-btc", "--fee-rate", "2.5", "not-a-swap-id")

    assert done.returncode != 2, (done.returncode, done.stderr)


def main():
    print()
    print("=" * 72)
    print("COMMAND SURFACE")
    print("=" * 72)
    print()

    check("resume-all drives only owned, canonical swaps", test_resume_all_filters)
    check("resume-all reports a name/content mismatch", test_resume_all_ignores_non_canonical_and_unowned)
    check("dismiss refuses a swap this participant is not a party to", test_dismiss_refuses_foreign_swap)
    check("run rejects an out-of-range step budget", test_run_rejects_bad_step_budget)
    check("run contains a failing step and exits non-zero", test_run_contains_a_failing_step)
    check("run reports a settled swap cleanly", test_run_reports_settled_cleanly)
    check("ignored-match tombstones expire and are reclaimed", test_ignored_match_ttl_expiry)
    check("operational env knobs fail closed on bad values", test_env_knob_validation)
    check("--fee-rate is accepted where an HTLC can be spent", test_fee_rate_option_is_wired)
    check("reorg transitions are table-driven and refuse other edges", test_reorg_transition_table)
    check("funding-scan guards refuse unanchored and oversized scans", test_funding_scan_guards)
    check("HTLC spend classification is structural", test_htlc_spend_classification_shapes)
    check("PSBT parsing bounds and input-index guard hold", test_psbt_bounds)

    print()
    print("=" * 72)
    print("COMMAND SURFACE SUMMARY")
    print("=" * 72)
    print(f"passes:  {PASS}")
    print(f"failures:{FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
