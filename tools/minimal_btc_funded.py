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
    p.add_argument("--secret-hash", required=True)
    p.add_argument("--btc-sats", required=True, type=int)
    p.add_argument("--btc-timeout", required=True, type=int)
    p.add_argument("--txid", required=True)
    p.add_argument("--vout", required=True, type=int)

    args = p.parse_args()

    message = {
        "type": "btc-funded",
        "swap_id": args.swap_id,
        "secret_hash": args.secret_hash,
        "btc_sats": args.btc_sats,
        "btc_timeout": args.btc_timeout,
        "btc_funding_txid": args.txid,
        "btc_funding_vout": args.vout,
    }

    canonical = encode_message(
        message,
        expected_type="btc-funded",
    )

    MESSAGE_DIR.mkdir(
        parents=True,
        exist_ok=True,
    )

    path = (
        MESSAGE_DIR
        / f"{args.swap_id}.btc-funded.json"
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
