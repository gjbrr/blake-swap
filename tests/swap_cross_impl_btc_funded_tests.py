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

    #
    # Reference negotiation.
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

    ok("reference negotiation completed")

    #
    # Reach BLAKE_FUNDED normally.
    #
    h.swap_cmd(
        "fund-blake",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "publish-blake-funded",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "receive-blake-funded",
        swap_id,
        role="bob",
    )

    require(
        h.load_state(
            swap_id,
            "bob",
        )["state"]
        == "BLAKE_FUNDED",
        "Bob did not reach BLAKE_FUNDED",
    )

    ok("BLAKE prerequisite established")

    #
    # Bob performs the REAL Bitcoin economic action.
    #
    # DO NOT call publish-btc-funded.
    #
    h.swap_cmd(
        "fund-btc",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob.get("btc_funding_txid"),
        "Bob funding produced no BTC txid",
    )

    require(
        bob.get("btc_funding_vout")
        is not None,
        "Bob funding produced no BTC vout",
    )

    require(
        bob["state"] == "BTC_FUNDED",
        "Bob did not reach BTC_FUNDED",
    )

    ok("real Bitcoin HTLC funded")

    #
    # Make absolutely sure the reference publisher did
    # not supply the message under test.
    #
    msg_path = h.message_file(
        swap_id,
        "btc-funded",
    )

    if msg_path.exists():
        msg_path.unlink()

    #
    # Independent implementation creates the wire
    # announcement from the resulting public facts.
    #
    cmd = [
        sys.executable,
        str(
            TOOLS
            / "minimal_btc_funded.py"
        ),
        "--swap-id",
        swap_id,
        "--secret-hash",
        bob["secret_hash"],
        "--btc-sats",
        str(bob["btc_sats"]),
        "--btc-timeout",
        str(bob["btc_timeout"]),
        "--txid",
        bob["btc_funding_txid"],
        "--vout",
        str(bob["btc_funding_vout"]),
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
        "independent producer created no btc-funded message",
    )

    ok(
        "independent implementation wrote btc-funded"
    )

    #
    # Alice consumes the independent announcement.
    #
    # Her reference engine must verify the actual HTLC
    # through her Bitcoin node before advancing state.
    #
    h.swap_cmd(
        "receive-btc-funded",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice["state"] == "BTC_FUNDED",
        (
            "Alice did not advance to BTC_FUNDED "
            "after independent chain verification"
        ),
    )

    require(
        alice["btc_funding_txid"]
        == bob["btc_funding_txid"],
        "Alice recorded different BTC funding txid",
    )

    require(
        int(alice["btc_funding_vout"])
        == int(bob["btc_funding_vout"]),
        "Alice recorded different BTC funding vout",
    )

    ok(
        "Alice accepted independent BTC funding announcement after chain verification"
    )

    #
    # Stronger proof: Alice must actually be able to
    # continue into the claim path using that imported
    # and independently verified funding evidence.
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
        "Alice could not continue to BTC claim",
    )

    require(
        alice.get("btc_claim_txid"),
        "Alice claim produced no BTC claim txid",
    )

    ok(
        "cross-implementation BTC funding preserved protocol progress"
    )

    print()
    print("=" * 72)
    print("CROSS IMPLEMENTATION BTC FUNDING SUMMARY")
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
            "cross implementation BTC funding",
            str(exc),
        )

        print()
        print("=" * 72)
        print("CROSS IMPLEMENTATION BTC FUNDING SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)

        raise
