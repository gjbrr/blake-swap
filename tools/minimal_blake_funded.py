#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
from pathlib import Path

from swap_wire import encode_message


BASE = Path.home() / ".blake-swaps-v2"
MESSAGE_DIR = BASE / "messages"


def main():
    p = argparse.ArgumentParser()

    p.add_argument("--swap-id", required=True)
    p.add_argument("--secret-hash", required=True)
    p.add_argument("--blake-amount", required=True)
    p.add_argument("--blake-timeout", required=True, type=int)
    p.add_argument("--txid", required=True)
    p.add_argument("--vout", required=True, type=int)

    args = p.parse_args()

    message = {
        "type": "blake-funded",
        "swap_id": args.swap_id,
        "secret_hash": args.secret_hash,
        "blake_amount": args.blake_amount,
        "blake_timeout": args.blake_timeout,
        "blake_funding_txid": args.txid,
        "blake_funding_vout": args.vout,
    }

    canonical = encode_message(
        message,
        expected_type="blake-funded",
    )

    MESSAGE_DIR.mkdir(
        parents=True,
        exist_ok=True,
    )

    path = (
        MESSAGE_DIR
        / f"{args.swap_id}.blake-funded.json"
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
