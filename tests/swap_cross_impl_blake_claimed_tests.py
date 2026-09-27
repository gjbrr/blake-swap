#!/usr/bin/env python3

from __future__ import annotations

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

    # Reach BTC_FUNDED.
    p = h.swap_cmd(
        "new",
        "--blake",
        "10",
        "--sats",
        "25000",
        role="alice",
    )

    swap_id = h.parse_swap_id(p.stdout)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    # Alice claims BTC.
    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "publish-btc-claimed",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    # Bob performs the real BLAKE claim.
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
        "Bob did not reach BLAKE_CLAIMED",
    )

    require(
        bob.get("blake_claim_txid"),
        "Bob has no BLAKE claim txid",
    )

    require(
        bob.get("blake_claim_blockhash"),
        "Bob has no BLAKE claim blockhash",
    )

    ok("real BLAKE claim completed")

    # Remove any reference announcement.
    msg_path = h.message_file(
        swap_id,
        "blake-claimed",
    )

    if msg_path.exists():
        msg_path.unlink()

    # Independent wire producer.
    cmd = [
        sys.executable,
        str(
            TOOLS
            / "minimal_blake_claimed.py"
        ),
        "--swap-id",
        swap_id,
        "--secret-hash",
        bob["secret_hash"],
        "--txid",
        bob["blake_claim_txid"],
        "--blockhash",
        bob["blake_claim_blockhash"],
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
        "independent producer created no blake-claimed message",
    )

    ok("independent implementation wrote blake-claimed")

    # Alice receives and independently verifies.
    h.swap_cmd(
        "receive-blake-claimed",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice["state"] == "BLAKE_CLAIMED",
        "Alice did not import BLAKE claim",
    )

    require(
        alice["blake_claim_txid"]
        == bob["blake_claim_txid"],
        "Alice recorded different BLAKE claim txid",
    )

    ok(
        "Alice accepted independent BLAKE claim announcement after chain verification"
    )

    # Finish both sides. Note both the engine's --role argument and the
    # harness `role` keyword: the latter is what selects the
    # participant's state root for the subprocess.
    h.swap_cmd(
        "complete",
        swap_id,
        "--role",
        "alice",
        role="alice",
    )

    h.swap_cmd(
        "complete",
        swap_id,
        "--role",
        "bob",
        role="bob",
    )

    require(
        h.load_state(
            swap_id,
            "alice",
        )["state"] == "COMPLETE",
        "Alice did not reach COMPLETE",
    )

    require(
        h.load_state(
            swap_id,
            "bob",
        )["state"] == "COMPLETE",
        "Bob did not reach COMPLETE",
    )

    ok("cross-implementation settlement reached COMPLETE")

    print()
    print("=" * 72)
    print("CROSS IMPLEMENTATION BLAKE CLAIM SUMMARY")
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
            "cross implementation BLAKE claim",
            str(exc),
        )
        raise
