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
    p.add_argument("--txid", required=True)
    p.add_argument("--blockhash", required=True)

    args = p.parse_args()

    message = {
        "type": "blake-claimed",
        "swap_id": args.swap_id,
        "secret_hash": args.secret_hash,
        "blake_claim_txid": args.txid,
        "blake_claim_blockhash": args.blockhash,
    }

    canonical = encode_message(
        message,
        expected_type="blake-claimed",
    )

    MESSAGE_DIR.mkdir(
        parents=True,
        exist_ok=True,
    )

    path = (
        MESSAGE_DIR
        / f"{args.swap_id}.blake-claimed.json"
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
