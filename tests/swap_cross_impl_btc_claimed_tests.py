#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import os
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
TOOLS = ROOT / "tools"
TESTS = ROOT / "tests"

sys.path.insert(0, str(SRC))
sys.path.insert(0, str(TESTS))

import swap_protocol_tests as h


PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print("PASS ", name)


def fail(name, detail):
    global FAIL
    FAIL += 1
    print("FAIL ", name)
    print("      ", detail)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    h.preflight()
    ok("preflight")

    #
    # Build a normal reference swap through BTC_FUNDED.
    #
    p = h.swap_cmd(
        "new",
        "--blake",
        "0.1",
        "--sats",
        "25000",
        role="alice",
    )

    swap_id = h.parse_swap_id(
        p.stdout
    )

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    require(
        h.load_state(
            swap_id,
            "alice",
        )["state"]
        == "BTC_FUNDED",
        "Alice did not reach BTC_FUNDED",
    )

    require(
        h.load_state(
            swap_id,
            "bob",
        )["state"]
        == "BTC_FUNDED",
        "Bob did not reach BTC_FUNDED",
    )

    ok("reference swap reached BTC_FUNDED")

    #
    # Alice performs the REAL Bitcoin claim.
    #
    # DO NOT publish btc-claimed through the reference engine.
    #
    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice["state"] == "BTC_CLAIMED",
        "Alice did not reach BTC_CLAIMED",
    )

    require(
        alice.get("btc_claim_txid"),
        "Alice claim produced no txid",
    )

    require(
        alice.get("btc_claim_blockhash"),
        "Alice claim produced no blockhash",
    )

    ok("real Bitcoin claim revealed S on-chain")

    #
    # Bob must still know no secret locally.
    #
    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob.get("secret_hex") is None,
        "Bob improperly received Alice original secret",
    )

    require(
        bob.get("revealed_secret_hex") is None,
        "Bob already had revealed secret before chain extraction",
    )

    ok("Bob still knows no secret before chain extraction")

    #
    # Ensure reference publisher did not create the message.
    #
    msg_path = h.message_file(
        swap_id,
        "btc-claimed",
    )

    if msg_path.exists():
        msg_path.unlink()

    #
    # Independent implementation announces ONLY public
    # settlement location data.
    #
    cmd = [
        sys.executable,
        str(
            TOOLS
            / "minimal_btc_claimed.py"
        ),
        "--swap-id",
        swap_id,
        "--secret-hash",
        alice["secret_hash"],
        "--txid",
        alice["btc_claim_txid"],
        "--blockhash",
        alice["btc_claim_blockhash"],
    ]

    env = h.tool_env()

    result = subprocess.run(
        cmd,
        text=True,
        capture_output=True,
        env=env,
    )

    if result.returncode != 0:
        raise RuntimeError(
            result.stderr
            or result.stdout
        )

    require(
        msg_path.exists(),
        "independent producer created no btc-claimed message",
    )

    ok("independent implementation wrote btc-claimed")

    #
    # Bob imports the independent announcement.
    #
    h.swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BTC_CLAIMED",
        "Bob did not import Bitcoin claim state",
    )

    require(
        bob.get("secret_hex") is None,
        "btc-claimed wire message populated original secret field",
    )

    require(
        bob.get("revealed_secret_hex") is None,
        (
            "btc-claimed announcement transported or "
            "prematurely populated revealed secret"
        ),
    )

    ok(
        "Bob accepted independent claim announcement without receiving S"
    )

    #
    # This is the atomicity hinge:
    # Bob now extracts S from Bitcoin itself.
    #
    h.swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    recovered = bob.get(
        "revealed_secret_hex"
    )

    require(
        recovered,
        "Bob failed to recover S from Bitcoin",
    )

    require(
        bob.get("secret_hex") is None,
        "Bob original secret field became populated",
    )

    recovered_hash = hashlib.sha256(
        bytes.fromhex(recovered)
    ).hexdigest()

    require(
        recovered_hash
        == bob["secret_hash"],
        "recovered S does not satisfy negotiated hashlock",
    )

    ok(
        "Bob independently recovered valid S from Bitcoin witness"
    )

    #
    # Final causal proof: the recovered chain secret must
    # actually be sufficient for Bob to claim BLAKE.
    #
    h.swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BLAKE_CLAIMED",
        "Bob could not claim BLAKE with recovered S",
    )

    require(
        bob.get("blake_claim_txid"),
        "Bob BLAKE claim produced no txid",
    )

    ok(
        "Bitcoin-recovered S completed Bob's BLAKE claim"
    )

    print()
    print("=" * 72)
    print("CROSS IMPLEMENTATION BTC CLAIM SUMMARY")
    print("=" * 72)
    print("passes:  ", PASS)
    print("failures:", FAIL)
    print("swap:    ", swap_id)

    if FAIL:
        raise SystemExit(1)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        fail(
            "cross implementation BTC claim",
            str(exc),
        )

        print()
        print("=" * 72)
        print("CROSS IMPLEMENTATION BTC CLAIM SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)

        raise
