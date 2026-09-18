import os

import swap_v2


class FakeBtcCli:
    def __init__(self):
        self.vsizes = {
            "btc-fee-1": 131,
            "btc-fee-156": 131,
            "btc-fee-200": 131,
        }

    def __call__(self, method, *args, **kwargs):
        if method == "estimatesmartfee":
            assert args == (2,)
            return {
                "feerate": 0.00001190,
                "blocks": 2,
            }

        if method == "getmempoolinfo":
            return {
                "mempoolminfee": 0.00000100,
                "minrelaytxfee": 0.00000100,
            }

        if method == "decoderawtransaction":
            tx_hex = args[0]
            return {
                "vsize": self.vsizes[tx_hex],
                "vout": [],
            }

        raise AssertionError(
            f"unexpected RPC method {method!r}"
        )


def main():
    override_name = "BLAKE_SWAP_BTC_SPEND_FEE_SATS"
    percent_name = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_PERCENT"

    old_override = os.environ.pop(
        override_name,
        None,
    )
    old_percent = os.environ.pop(
        percent_name,
        None,
    )

    try:
        cli = FakeBtcCli()
        built = []

        def build(fee_sats):
            built.append(fee_sats)
            return f"btc-fee-{fee_sats}"

        plan = swap_v2.build_dynamic_settlement_spend(
            chain="btc",
            cli=cli,
            input_sats=2500,
            build_finalized=build,
        )

        # Real live observation:
        # 131 vB × 1.19 sat/vB = 155.89 -> 156 sats.
        assert built == [1, 156]
        assert plan["fee_sats"] == 156
        assert plan["vsize"] == 131
        assert plan["source"] == "dynamic"

        # 156 / 2500 = 6.24%, therefore below the default
        # 10% emergency safety ceiling.
        assert plan["fee_sats"] < 250

        # Emergency explicit override remains possible.
        os.environ[override_name] = "200"
        built.clear()

        override = swap_v2.build_dynamic_settlement_spend(
            chain="btc",
            cli=cli,
            input_sats=2500,
            build_finalized=lambda fee: (
                built.append(fee)
                or "btc-fee-200"
            ),
        )

        assert built == [200]
        assert override["fee_sats"] == 200
        assert override["source"] == "override"

        # But an operator cannot accidentally override the
        # percentage safety ceiling.
        os.environ[override_name] = "300"

        try:
            swap_v2.build_dynamic_settlement_spend(
                chain="btc",
                cli=cli,
                input_sats=2500,
                build_finalized=lambda fee: "btc-fee-200",
            )
        except ValueError as exc:
            assert "safety cap" in str(exc)
        else:
            raise AssertionError(
                "excessive BTC override bypassed safety cap"
            )

        print(
            "PASS: BTC dynamic settlement tests"
        )

    finally:
        if old_override is None:
            os.environ.pop(override_name, None)
        else:
            os.environ[override_name] = old_override

        if old_percent is None:
            os.environ.pop(percent_name, None)
        else:
            os.environ[percent_name] = old_percent


if __name__ == "__main__":
    main()
