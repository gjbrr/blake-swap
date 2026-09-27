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


def main():
    h.preflight()
    ok("preflight")

    #
    # Alice engine creates and publishes a normal v1.5 offer.
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

    h.swap_cmd(
        "publish-offer",
        swap_id,
        role="alice",
    )

    #
    # Let the real Bob engine accept once only so it generates
    # Bob-owned settlement keys.
    #
    # Then remove the real acceptance message and reconstruct
    # that message with the independent implementation.
    #
    h.swap_cmd(
        "accept-offer",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    accept_path = h.message_file(
        swap_id,
        "accept",
    )

    if accept_path.exists():
        accept_path.unlink()

    cmd = [
        sys.executable,
        str(
            TOOLS
            / "minimal_bob_accept.py"
        ),
        "--swap-id",
        swap_id,
        "--secret-hash",
        bob["secret_hash"],
        "--blake-bob-pub",
        bob["blake_bob_pub"],
        "--btc-bob-pub",
        bob["btc_bob_pub"],
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
        "independent Bob implementation wrote accept"
    )

    #
    # Existing Alice engine must import that independently
    # generated wire message.
    #
    h.swap_cmd(
        "receive-accept",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    if (
        alice["blake_bob_pub"]
        != bob["blake_bob_pub"]
    ):
        raise RuntimeError(
            "Alice imported different Bob BLAKE key"
        )

    if (
        alice["btc_bob_pub"]
        != bob["btc_bob_pub"]
    ):
        raise RuntimeError(
            "Alice imported different Bob BTC key"
        )

    if (
        alice["secret_hash"]
        != bob["secret_hash"]
    ):
        raise RuntimeError(
            "Alice/Bob secret hash binding changed"
        )

    ok(
        "Alice accepted independent Bob wire implementation"
    )

    ok(
        "settlement public keys survived cross-implementation handshake"
    )

    print()
    print("=" * 72)
    print("CROSS IMPLEMENTATION ACCEPT SUMMARY")
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
            "cross implementation accept",
            str(exc),
        )
        raise
