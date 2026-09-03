#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time


SWAP = str(
    Path(os.environ.get("SWAP_V2", Path(__file__).resolve().parent / "swap_v2.py"))
)

BASE = (
    Path.home()
    / ".blake-swaps-v2"
)


def state_path(
    swap_id,
    role,
):
    return (
        BASE
        / role
        / f"{swap_id}.json"
    )


def load_state(
    swap_id,
    role,
):
    path = state_path(
        swap_id,
        role,
    )

    with path.open() as f:
        return json.load(f)


def run_swap(
    role,
    *args,
):
    env = os.environ.copy()

    env[
        "BLAKE_SWAP_ROLE"
    ] = role

    env[
        "BLAKE_SWAP_TRANSPORT"
    ] = "file"

    cmd = [
        sys.executable,
        SWAP,
        *args,
    ]

    print(
        "$",
        " ".join(cmd),
        flush=True,
    )

    p = subprocess.run(
        cmd,
        env=env,
        text=True,
        capture_output=True,
    )

    if p.stdout:
        print(
            p.stdout.rstrip(),
            flush=True,
        )

    if p.stderr:
        print(
            p.stderr.rstrip(),
            file=sys.stderr,
            flush=True,
        )

    if p.returncode != 0:

        combined = (
            p.stdout
            + "\n"
            + p.stderr
        )

        if (
            "SECURITY FAILURE"
            in combined
        ):
            raise RuntimeError(
                "Agent stopped on SECURITY FAILURE"
            )

        return False

    return True


def alice_step(
    swap_id,
):
    state = load_state(
        swap_id,
        "alice",
    )["state"]

    #
    # Alice is the BLAKE seller.
    #
    if state == "OFFERED":

        return run_swap(
            "alice",
            "fund-blake",
            swap_id,
        )

    if state == "BLAKE_FUNDED":

        return run_swap(
            "alice",
            "sync-funding",
            swap_id,
            "--role",
            "alice",
        )

    if state == "BTC_FUNDED":

        #
        # Reconcile first. This is important after restart:
        # Alice may already have broadcast her claim.
        #
        run_swap(
            "alice",
            "sync-settlement",
            swap_id,
            "--role",
            "alice",
        )

        state = load_state(
            swap_id,
            "alice",
        )["state"]

        if state == "BTC_FUNDED":

            return run_swap(
                "alice",
                "claim-btc",
                swap_id,
            )

        return True

    if state == "BTC_CLAIMED":

        return run_swap(
            "alice",
            "sync-settlement",
            swap_id,
            "--role",
            "alice",
        )

    if state == "BLAKE_CLAIMED":

        return run_swap(
            "alice",
            "complete",
            swap_id,
            "--role",
            "alice",
        )

    if state in {
        "COMPLETE",
        "BLAKE_REFUNDED",
    }:
        return True

    if state == "BTC_REFUNDED":

        return run_swap(
            "alice",
            "sync-settlement",
            swap_id,
            "--role",
            "alice",
        )

    raise RuntimeError(
        f"Alice does not know how to act from state {state}"
    )


def bob_step(
    swap_id,
):
    state = load_state(
        swap_id,
        "bob",
    )["state"]

    #
    # Bob begins by watching BLAKE for Alice's funding.
    #
    if state == "OFFERED":

        return run_swap(
            "bob",
            "sync-funding",
            swap_id,
            "--role",
            "bob",
        )

    if state == "BLAKE_FUNDED":

        #
        # First see whether BTC funding already exists.
        # This matters after a crash/restart.
        #
        run_swap(
            "bob",
            "sync-funding",
            swap_id,
            "--role",
            "bob",
        )

        state = load_state(
            swap_id,
            "bob",
        )["state"]

        if state == "BLAKE_FUNDED":

            return run_swap(
                "bob",
                "fund-btc",
                swap_id,
            )

        return True

    if state == "BTC_FUNDED":

        #
        # Wait for / discover Alice's Bitcoin claim.
        #
        return run_swap(
            "bob",
            "sync-settlement",
            swap_id,
            "--role",
            "bob",
        )

    if state == "BTC_CLAIMED":

        state_data = load_state(
            swap_id,
            "bob",
        )

        if not state_data.get(
            "revealed_secret_hex"
        ):
            #
            # Usually sync-settlement recovered S already.
            # Retain compatibility with an older durable state.
            #
            if not run_swap(
                "bob",
                "extract-secret",
                swap_id,
            ):
                return False

        #
        # Reconcile before attempting the irreversible action.
        #
        run_swap(
            "bob",
            "sync-settlement",
            swap_id,
            "--role",
            "bob",
        )

        state = load_state(
            swap_id,
            "bob",
        )["state"]

        if state == "BTC_CLAIMED":

            return run_swap(
                "bob",
                "claim-blake",
                swap_id,
            )

        return True

    if state == "BLAKE_CLAIMED":

        return run_swap(
            "bob",
            "complete",
            swap_id,
            "--role",
            "bob",
        )

    if state == "BTC_REFUNDED":

        return run_swap(
            "bob",
            "sync-settlement",
            swap_id,
            "--role",
            "bob",
        )

    if state in {
        "COMPLETE",
        "BLAKE_REFUNDED",
    }:
        return True

    raise RuntimeError(
        f"Bob does not know how to act from state {state}"
    )


def main():
    parser = argparse.ArgumentParser()

    parser.add_argument(
        "swap_id",
    )

    parser.add_argument(
        "--role",
        required=True,
        choices=[
            "alice",
            "bob",
        ],
    )

    parser.add_argument(
        "--interval",
        type=float,
        default=0.25,
    )

    args = parser.parse_args()

    print(
        f"AUTONOMOUS {args.role.upper()} STARTED "
        f"swap={args.swap_id}",
        flush=True,
    )

    failures = 0

    while True:

        state = load_state(
            args.swap_id,
            args.role,
        )["state"]

        print(
            f"[{args.role}] state={state}",
            flush=True,
        )

        if state in {
            "COMPLETE",
            "BLAKE_REFUNDED",
        }:
            print(
                f"AUTONOMOUS {args.role.upper()} FINISHED "
                f"state={state}",
                flush=True,
            )

            return 0

        try:

            if args.role == "alice":
                ok = alice_step(
                    args.swap_id
                )
            else:
                ok = bob_step(
                    args.swap_id
                )

            if ok:
                failures = 0
            else:
                failures += 1

        except Exception as e:

            failures += 1

            print(
                f"[{args.role}] ERROR: {e}",
                file=sys.stderr,
                flush=True,
            )

            if (
                "SECURITY FAILURE"
                in str(e)
            ):
                return 2

        #
        # Don't silently spin forever on a genuinely broken protocol.
        #
        if failures >= 20:

            print(
                f"[{args.role}] too many consecutive failures",
                file=sys.stderr,
                flush=True,
            )

            return 3

        time.sleep(
            args.interval
        )


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
