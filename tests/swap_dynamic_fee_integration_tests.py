import os
from decimal import Decimal

import swap_v2


def fake_btc_cli(method, *args, **kwargs):
    if method == "estimatesmartfee":
        assert len(args) == 1
        target = int(args[0])

        assert target in (1, 2, 3)

        return {
            "feerate": 0.00001190,
            "blocks": target,
        }

    if method == "getmempoolinfo":
        return {
            "mempoolminfee": 0.00000100,
            "minrelaytxfee": 0.00000100,
        }

    raise AssertionError(
        f"unexpected fake RPC method: {method}"
    )


def main():
    original_cli = swap_v2.btc_cli

    names = [
        "BLAKE_SWAP_BTC_SPEND_FEE_SATS",
        "BLAKE_SWAP_FEE_TARGET_BLOCKS",
        "BLAKE_SWAP_BTC_FUNDING_FEE_TARGET_BLOCKS",
        "BLAKE_SWAP_FEE_RATE_MULTIPLIER",
        "BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE",
    ]

    old_env = {
        name: os.environ.get(name)
        for name in names
    }

    try:
        swap_v2.btc_cli = fake_btc_cli

        for name in names:
            os.environ.pop(name, None)

        # BTC funding is intentionally more urgent than settlement:
        # one-block target by default.
        assert swap_v2.btc_funding_fee_target_blocks() == 1

        os.environ[
            "BLAKE_SWAP_BTC_FUNDING_FEE_TARGET_BLOCKS"
        ] = "3"

        assert swap_v2.btc_funding_fee_target_blocks() == 3

        os.environ.pop(
            "BLAKE_SWAP_BTC_FUNDING_FEE_TARGET_BLOCKS",
            None,
        )

        # 200 vB × 1.19 sat/vB = 238 sats,
        # therefore minimum trade = 239 sats.
        assert swap_v2.btc_trade_minimum_sats() == 239

        # Emergency explicit override remains supported.
        os.environ[
            "BLAKE_SWAP_BTC_SPEND_FEE_SATS"
        ] = "1000"

        assert swap_v2.btc_trade_minimum_sats() == 1001

        os.environ.pop(
            "BLAKE_SWAP_BTC_SPEND_FEE_SATS",
            None,
        )

        # Configurable target and multiplier.
        os.environ[
            "BLAKE_SWAP_FEE_RATE_MULTIPLIER"
        ] = "1.10"

        # ceil(200 × 1.19 × 1.10) = 262;
        # minimum amount = 263.
        assert swap_v2.btc_trade_minimum_sats() == 263

        print(
            "PASS: swap dynamic fee integration tests"
        )

    finally:
        swap_v2.btc_cli = original_cli

        for name, value in old_env.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value


if __name__ == "__main__":
    main()
