#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
from pathlib import Path
import os

from swap_wire import encode_message


BASE = Path(
    os.environ.get(
        "BLAKE_SWAP_STATE_DIR",
        Path.home() / ".blake-swaps-v2",
    )
).expanduser()

MESSAGE_DIR = Path(
    os.environ.get(
        "BLAKE_SWAP_MESSAGE_DIR",
        BASE / "messages",
    )
).expanduser()


def main():
    p = argparse.ArgumentParser()

    p.add_argument("--swap-id", required=True)
    p.add_argument("--blake-amount", required=True)
    p.add_argument("--btc-sats", required=True, type=int)
    p.add_argument("--secret-hash", required=True)
    p.add_argument("--btc-timeout", required=True, type=int)
    p.add_argument("--blake-timeout", required=True, type=int)
    p.add_argument("--created-at", required=True, type=int)

    p.add_argument("--blake-alice-pub", required=True)
    p.add_argument("--btc-alice-pub", required=True)

    p.add_argument(
        "--timeout-policy-version",
        default="1.5",
    )

    p.add_argument(
        "--btc-timeout-window-seconds",
        required=True,
        type=int,
    )

    p.add_argument(
        "--blake-timeout-window-seconds",
        required=True,
        type=int,
    )

    p.add_argument(
        "--bob-recovery-seconds",
        required=True,
        type=int,
    )

    args = p.parse_args()

    message = {
        "type": "offer",
        "swap_id": args.swap_id,
        "blake_amount": args.blake_amount,
        "btc_sats": args.btc_sats,
        "secret_hash": args.secret_hash,
        "btc_timeout": args.btc_timeout,
        "blake_timeout": args.blake_timeout,
        "created_at": args.created_at,
        "blake_alice_pub": args.blake_alice_pub,
        "btc_alice_pub": args.btc_alice_pub,
        "timeout_policy_version":
            args.timeout_policy_version,
        "btc_timeout_window_seconds":
            args.btc_timeout_window_seconds,
        "blake_timeout_window_seconds":
            args.blake_timeout_window_seconds,
        "bob_recovery_seconds":
            args.bob_recovery_seconds,
    }

    canonical = encode_message(
        message,
        expected_type="offer",
    )

    MESSAGE_DIR.mkdir(
        parents=True,
        exist_ok=True,
    )

    path = (
        MESSAGE_DIR
        / f"{args.swap_id}.offer.json"
    )

    path.write_text(
        json.dumps(
            json.loads(canonical),
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )

    print(path)


if __name__ == "__main__":
    main()
