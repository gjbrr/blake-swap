#!/usr/bin/env python3

from __future__ import annotations

import json
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


def main():
    h.preflight()
    ok("preflight")

    #
    # Let the real engine create local Alice swap state,
    # but DO NOT let it publish the offer.
    #
    p = h.swap_cmd(
        "new",
        "--blake",
        "10",
        "--sats",
        "25000",
        role="alice",
    )

    swap_id = h.parse_swap_id(
        p.stdout
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    #
    # publish-offer normally creates Alice's settlement
    # pubkeys, so create them first by calling it once,
    # then delete the resulting message and rebuild the
    # wire offer independently.
    #
    h.swap_cmd(
        "publish-offer",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    msg_path = h.message_file(
        swap_id,
        "offer",
    )

    if msg_path.exists():
        msg_path.unlink()

    cmd = [
        sys.executable,
        str(
            TOOLS
            / "minimal_alice_offer.py"
        ),
        "--swap-id",
        swap_id,
        "--blake-amount",
        str(alice["blake_amount"]),
        "--btc-sats",
        str(alice["btc_sats"]),
        "--secret-hash",
        alice["secret_hash"],
        "--btc-timeout",
        str(alice["btc_timeout"]),
        "--blake-timeout",
        str(alice["blake_timeout"]),
        "--created-at",
        str(alice["created_at"]),
        "--blake-alice-pub",
        alice["blake_alice_pub"],
        "--btc-alice-pub",
        alice["btc_alice_pub"],
        "--btc-timeout-window-seconds",
        str(
            alice[
                "btc_timeout_window_seconds"
            ]
        ),
        "--blake-timeout-window-seconds",
        str(
            alice[
                "blake_timeout_window_seconds"
            ]
        ),
        "--bob-recovery-seconds",
        str(
            alice[
                "bob_recovery_seconds"
            ]
        ),
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

    ok(
        "independent implementation wrote offer"
    )

    h.swap_cmd(
        "accept-offer",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    if (
        bob["secret_hash"]
        != alice["secret_hash"]
    ):
        raise RuntimeError(
            "Bob imported different secret hash"
        )

    if (
        float(bob["blake_amount"])
        != float(alice["blake_amount"])
    ):
        raise RuntimeError(
            "Bob imported different BLAKE amount"
        )

    if (
        int(bob["btc_sats"])
        != int(alice["btc_sats"])
    ):
        raise RuntimeError(
            "Bob imported different BTC amount"
        )

    ok(
        "Bob accepted independent wire implementation"
    )

    if (
        bob.get("timeout_policy_version")
        != "1.5"
    ):
        raise RuntimeError(
            "Bob lost timeout policy metadata"
        )

    ok(
        "v1.5 timeout policy survived cross-implementation handshake"
    )

    print()
    print("=" * 72)
    print("CROSS IMPLEMENTATION OFFER SUMMARY")
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
            "cross implementation offer",
            str(exc),
        )
        raise
