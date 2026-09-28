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

        #
        # The absolute cap is the control a percentage cannot express.
        # 200 sats is under 10% of 2,500, so only this knob can refuse it.
        #
        absolute_name = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_SATS"
        old_absolute = os.environ.pop(absolute_name, None)

        # The previous case left an excessive override set; clear it so the
        # absolute cap is what this section is testing.
        os.environ.pop(override_name, None)

        try:
            os.environ[absolute_name] = "100"

            try:
                swap_v2.build_dynamic_settlement_spend(
                    chain="btc",
                    cli=cli,
                    input_sats=2500,
                    build_finalized=lambda fee: "btc-fee-200",
                )
            except ValueError as exc:
                assert "safety cap" in str(exc), exc
                assert "BLAKE_SWAP_MAX_SETTLEMENT_FEE_SATS" in str(exc), exc
            else:
                raise AssertionError(
                    "the absolute settlement fee cap did not bind"
                )

            # A cap that permits the dynamic answer changes nothing else:
            # 131 vB at 1.19 sat/vB is 156 sats either way.
            os.environ[absolute_name] = "250"

            permitted = swap_v2.build_dynamic_settlement_spend(
                chain="btc",
                cli=cli,
                input_sats=2500,
                build_finalized=lambda fee: "btc-fee-200",
            )

            assert permitted["fee_sats"] == 156, permitted
            assert permitted["fee_sats"] <= 250

            #
            # And it applies to the explicit override path too: the
            # percentage ceiling already does, so an operator cannot use
            # `BLAKE_SWAP_BTC_SPEND_FEE_SATS` to get past this one either.
            #
            os.environ[override_name] = "200"
            os.environ[absolute_name] = "100"

            try:
                swap_v2.build_dynamic_settlement_spend(
                    chain="btc",
                    cli=cli,
                    input_sats=2500,
                    build_finalized=lambda fee: "btc-fee-200",
                )
            except ValueError as exc:
                assert "safety cap" in str(exc), exc
            else:
                raise AssertionError(
                    "an explicit override bypassed the absolute fee cap"
                )

            os.environ[absolute_name] = "250"

            overridden = swap_v2.build_dynamic_settlement_spend(
                chain="btc",
                cli=cli,
                input_sats=2500,
                build_finalized=lambda fee: "btc-fee-200",
            )

            assert overridden["source"] == "override", overridden
            assert overridden["fee_sats"] == 200, overridden
            os.environ.pop(override_name, None)

            for bad in ("0", "-5", "abc"):
                os.environ[absolute_name] = bad

                try:
                    swap_v2.build_dynamic_settlement_spend(
                        chain="btc",
                        cli=cli,
                        input_sats=2500,
                        build_finalized=lambda fee: "btc-fee-200",
                    )
                except ValueError as exc:
                    assert absolute_name in str(exc), exc
                else:
                    raise AssertionError(
                        f"an absolute cap of {bad!r} was accepted"
                    )
        finally:
            if old_absolute is None:
                os.environ.pop(absolute_name, None)
            else:
                os.environ[absolute_name] = old_absolute

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
