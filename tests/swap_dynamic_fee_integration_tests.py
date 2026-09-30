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
        "BLAKE_SWAP_BTC_SPEND_FEE_RATE",
        "BLAKE_SWAP_FEE_TARGET_BLOCKS",
        "BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE",
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

        # Wallet-built BTC funding transactions must also satisfy the
        # Bitcoin Core wallet transaction-creation floor. Core's default
        # -mintxfee is 1 sat/vB.
        assert swap_v2.btc_funding_min_fee_rate() == Decimal("1.0")

        os.environ[
            "BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE"
        ] = "2.5"

        assert (
            swap_v2.btc_funding_min_fee_rate()
            == Decimal("2.5")
        )

        os.environ.pop(
            "BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE",
            None,
        )

        # Invalid funding floors fail closed.
        os.environ[
            "BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE"
        ] = "0"

        try:
            swap_v2.btc_funding_min_fee_rate()
        except ValueError:
            pass
        else:
            raise AssertionError(
                "zero BTC funding minimum was accepted"
            )

        os.environ.pop(
            "BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE",
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

        #
        # The rate override drives the same derivation: a client with its
        # own view of the fee market hands over sat/vB, and the trade
        # minimum follows the rate it named rather than the estimator's
        # 1.19.
        #
        os.environ[
            "BLAKE_SWAP_BTC_SPEND_FEE_RATE"
        ] = "10"

        # ceil(200 × 10) = 2000; minimum = 2001. Note the 1.10 multiplier
        # from above is still set and is deliberately *not* applied: an
        # explicit rate is the client's answer, and scaling it would be
        # this engine answering the same question twice.
        assert swap_v2.btc_trade_minimum_sats() == 2001, (
            swap_v2.btc_trade_minimum_sats()
        )

        os.environ.pop(
            "BLAKE_SWAP_BTC_SPEND_FEE_RATE",
            None,
        )

        os.environ.pop(
            "BLAKE_SWAP_FEE_RATE_MULTIPLIER",
            None,
        )

        #
        # The knobs are per chain, and `chain` is what selects one. If
        # this helper were pinned to BTC -- as it was -- a BLAKE caller
        # would be priced by the BTC knob.
        #
        os.environ[
            "BLAKE_SWAP_BTC_SPEND_FEE_RATE"
        ] = "50"

        fee, rate = swap_v2.estimate_dynamic_settlement_fee(
            fake_btc_cli,
            vsize=200,
            chain="blake",
        )

        assert rate == Decimal("1.19"), rate
        assert fee == 238, fee

        os.environ.pop(
            "BLAKE_SWAP_BTC_SPEND_FEE_RATE",
            None,
        )

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
