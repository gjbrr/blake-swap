#!/usr/bin/env python3

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"

SWAP_V2 = SRC / "swap_v2.py"


PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print("PASS ", name)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run_swap(
    *args,
    role,
    state_dir,
    expect_ok=True,
):
    env = os.environ.copy()

    env.update({
        "BLAKE_SWAP_ROLE": role,
        "BLAKE_SWAP_STATE_DIR": str(state_dir),
        "BLAKE_SWAP_TRANSPORT": "nostr",
        "PYTHONPATH": str(SRC),
    })

    p = subprocess.run(
        [
            sys.executable,
            str(SWAP_V2),
            *map(str, args),
        ],
        text=True,
        capture_output=True,
        env=env,
    )

    if expect_ok and p.returncode != 0:
        raise RuntimeError(
            p.stderr
            or p.stdout
        )

    return p


def main():
    base = Path(
        tempfile.mkdtemp(
            prefix="blake-two-host-"
        )
    )

    alice_root = base / "alice-host"
    bob_root = base / "bob-host"

    alice_root.mkdir()
    bob_root.mkdir()

    try:
        require(
            alice_root != bob_root,
            "participant state roots are identical",
        )

        ok("Alice and Bob have distinct state roots")

        #
        # Alice creates a swap in her own universe.
        #
        p = run_swap(
            "new",
            "--blake",
            "10",
            "--sats",
            "25000",
            role="alice",
            state_dir=alice_root,
        )

        import re

        m = re.search(
            r"swap id:\s*([0-9a-f]{12})",
            p.stdout,
        )

        require(
            m is not None,
            "could not parse Alice swap id",
        )

        swap_id = m.group(1)

        alice_state = (
            alice_root
            / "alice"
            / f"{swap_id}.json"
        )

        bob_state_in_alice_root = (
            alice_root
            / "bob"
            / f"{swap_id}.json"
        )

        require(
            alice_state.exists(),
            "Alice local state was not created",
        )

        require(
            not bob_state_in_alice_root.exists(),
            "Alice root already contains Bob state",
        )

        ok("Alice state exists only in Alice root")

        #
        # Bob's root must know nothing yet.
        #
        bob_state = (
            bob_root
            / "bob"
            / f"{swap_id}.json"
        )

        alice_state_in_bob_root = (
            bob_root
            / "alice"
            / f"{swap_id}.json"
        )

        require(
            not bob_state.exists(),
            "Bob already has swap state before negotiation",
        )

        require(
            not alice_state_in_bob_root.exists(),
            "Bob root contains Alice private state",
        )

        ok("Bob root contains no Alice or Bob swap state initially")

        #
        # Neither participant should rely on shared file transport.
        #
        alice_messages = alice_root / "messages"
        bob_messages = bob_root / "messages"

        require(
            not alice_messages.exists()
            or not any(alice_messages.iterdir()),
            "Alice file transport directory is in use",
        )

        require(
            not bob_messages.exists()
            or not any(bob_messages.iterdir()),
            "Bob file transport directory is in use",
        )

        ok("isolated roots have no active file-transport messages")

        #
        # State-root override should also affect Nostr-key location.
        #
        #
        # At this point we don't require actual relay negotiation;
        # this first test is only proving filesystem isolation.
        #
        print()
        print("=" * 72)
        print("PARTICIPANT ISOLATION SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)
        print("alice:   ", alice_root)
        print("bob:     ", bob_root)
        print("swap:    ", swap_id)

    finally:
        shutil.rmtree(
            base,
            ignore_errors=True,
        )


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        FAIL += 1
        print("FAIL ", exc)
        raise
