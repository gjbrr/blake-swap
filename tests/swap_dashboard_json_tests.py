#!/usr/bin/env python3

import contextlib
import io
import json
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import swap_v2


SWAP_ID = "aaaaaaaaaaaa"
ME = "11" * 32

SECRET = "22" * 32
REVEALED_SECRET = "33" * 32


def fake_swap():
    return SimpleNamespace(
        swap_id=SWAP_ID,
        state=swap_v2.State.BTC_FUNDED.value,
        blake_amount=10.0,
        blake_amount_base_units=1_000_000_000,
        btc_sats=25_000,

        # Identity needed by dashboard ownership filtering.
        nostr_alice_pub=ME,
        nostr_bob_pub="44" * 32,

        # Deliberately present secret material. The dashboard JSON
        # must never serialize either field.
        secret_hex=SECRET,
        revealed_secret_hex=REVEALED_SECRET,

        blake_funding_txid="aa" * 32,
        btc_funding_txid="bb" * 32,
        btc_claim_txid=None,
        blake_claim_txid=None,
        btc_refund_txid=None,
        blake_refund_txid=None,
    )


def fake_report():
    return {
        "protocol_role": "alice",
        "decision_class": "ready",
        "next_action": "claim-btc",
        "reason": "both HTLCs are confirmed and Alice can claim Bitcoin",
        "blake": {
            "status": "funded",
        },
        "btc": {
            "status": "funded",
        },
    }


def run_dashboard(*, json_mode):
    with tempfile.TemporaryDirectory() as td:
        state_dir = Path(td)

        # command_dashboard only needs a correctly named local state
        # path before load_swap() is called.
        (state_dir / f"{SWAP_ID}.json").write_text("{}\n")

        stdout = io.StringIO()

        with (
            patch.object(
                swap_v2,
                "BASE_SWAP_DIR",
                state_dir,
            ),
            patch.object(
                swap_v2,
                "current_nostr_pubkey",
                return_value=ME,
            ),
            patch.object(
                swap_v2,
                "load_swap",
                return_value=fake_swap(),
            ),
            patch.object(
                swap_v2,
                "build_reconcile_report",
                return_value=fake_report(),
            ),
            contextlib.redirect_stdout(stdout),
        ):
            swap_v2.command_dashboard(
                SimpleNamespace(json=json_mode)
            )

        return stdout.getvalue()


def test_json_dashboard():
    stdout = run_dashboard(json_mode=True)
    parsed = json.loads(stdout)

    assert parsed["version"] == 1
    assert parsed["summary"] == {
        "active": 1,
        "attention": 0,
        "settled": 0,
        "archived": 0,
    }

    assert len(parsed["active"]) == 1

    item = parsed["active"][0]

    assert item["swap_id"] == SWAP_ID
    assert item["state"] == "BTC_FUNDED"
    assert item["protocol_role"] == "alice"
    assert item["decision_class"] == "ready"
    assert item["next_action"] == "claim-btc"
    assert item["status"] == "READY TO CLAIM BTC"
    assert item["action"] == "run resume"

    #
    # Both forms are emitted on purpose: the JSON number has always been
    # there and changing its type would break consumers, and the integer
    # is the exact value the state file and the wire carry.
    #
    assert item["trade"] == {
        "blake_amount": 10.0,
        "blake_amount_base_units": 1_000_000_000,
        "btc_sats": 25_000,
    }

    assert item["chains"] == {
        "blake_status": "funded",
        "btc_status": "funded",
    }

    assert item["transactions"]["blake_funding_txid"] == "aa" * 32
    assert item["transactions"]["btc_funding_txid"] == "bb" * 32

    # Most important privacy invariant:
    # neither locally generated nor revealed secret material may
    # appear anywhere in this UI/API representation.
    encoded = json.dumps(parsed)

    assert SECRET not in encoded
    assert REVEALED_SECRET not in encoded
    assert "secret_hex" not in encoded
    assert "revealed_secret_hex" not in encoded


def test_human_dashboard_preserved():
    stdout = run_dashboard(json_mode=False)

    assert "BLAKE SWAP DASHBOARD" in stdout
    assert SWAP_ID in stdout
    assert "READY TO CLAIM BTC" in stdout
    assert "run resume" in stdout


def main():
    test_json_dashboard()
    print("PASS: dashboard --json exposes safe reconciled swap status")

    test_human_dashboard_preserved()
    print("PASS: dashboard human-readable output is preserved")


if __name__ == "__main__":
    main()
