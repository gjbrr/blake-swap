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

    p.add_argument("--type", required=True)
    p.add_argument("--swap-id", required=True)
    p.add_argument(
        "--json",
        required=True,
        help="JSON object containing all remaining fields",
    )

    args = p.parse_args()

    extra = json.loads(args.json)

    if not isinstance(extra, dict):
        raise ValueError("--json must decode to an object")

    message = {
        "type": args.type,
        "swap_id": args.swap_id,
        **extra,
    }

    canonical = encode_message(
        message,
        expected_type=args.type,
    )

    MESSAGE_DIR.mkdir(
        parents=True,
        exist_ok=True,
    )

    path = (
        MESSAGE_DIR
        / f"{args.swap_id}.{args.type}.json"
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
