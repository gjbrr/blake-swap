import os

import swap_v2


class FakeCli:
    def __init__(self):
        self.decoded = {
            "hex-fee-1": 131,
            "hex-fee-131": 132,
            "hex-fee-132": 132,
        }

    def __call__(self, method, *args, **kwargs):
        if method == "estimatesmartfee":
            return {
                "feerate": 0.00001000,
                "blocks": 2,
            }

        if method == "getmempoolinfo":
            return {
                "mempoolminfee": 0.00001000,
                "minrelaytxfee": 0.00001000,
            }

        if method == "decoderawtransaction":
            tx_hex = args[0]
            return {
                "vsize": self.decoded[tx_hex],
                "vout": [],
            }

        raise AssertionError(
            f"unexpected RPC method {method!r}"
        )


def main():
    name = "BLAKE_SWAP_BLAKE_SPEND_FEE_SATS"
    old = os.environ.pop(name, None)

    try:
        cli = FakeCli()
        built = []

        def build(fee_sats):
            built.append(fee_sats)
            return f"hex-fee-{fee_sats}"

        plan = swap_v2.build_dynamic_settlement_spend(
            chain="blake",
            cli=cli,
            input_sats=20_000,
            build_finalized=build,
        )

        # Pass 1 measures 131 vB -> 131 fee.
        # Changing the tx changes signature/vsize to 132.
        # Pass 3 converges at 132.
        assert built == [1, 131, 132]
        assert plan["fee_sats"] == 132
        assert plan["vsize"] == 132
        assert plan["source"] == "dynamic"

        # Explicit emergency override still bypasses estimator math.
        os.environ[name] = "200"
        built.clear()

        override = swap_v2.build_dynamic_settlement_spend(
            chain="blake",
            cli=cli,
            input_sats=20_000,
            build_finalized=lambda fee: (
                built.append(fee)
                or "hex-fee-132"
            ),
        )

        assert built == [200]
        assert override["fee_sats"] == 200
        assert override["source"] == "override"

        #
        # A change output that appears and disappears across the dust
        # threshold makes vsize a *decreasing* step function of the fee:
        # paying more can remove the output, which lowers the requirement,
        # which puts the output back. That two-cycle never reaches
        # equality, so the builder must accept the transaction it has
        # already measured instead of oscillating until it gives up.
        #
        oscillating = FakeCli()
        oscillating.decoded = {
            "hex-fee-1": 200,
            "hex-fee-200": 150,
            "hex-fee-150": 200,
        }
        built.clear()

        # The explicit override above is still set; clear it so the
        # estimator path (the loop under test) is what runs.
        os.environ.pop(name, None)

        plan = swap_v2.build_dynamic_settlement_spend(
            chain="blake",
            cli=oscillating,
            input_sats=20_000,
            build_finalized=lambda fee_sats: (
                built.append(fee_sats)
                or f"hex-fee-{fee_sats}"
            ),
        )

        assert built == [1, 200], built
        assert plan["fee_sats"] == 200
        assert plan["vsize"] == 150

        # What the returned transaction paid must cover what it needs:
        # the effective rate here is 1 sat/vB.
        assert plan["fee_sats"] >= plan["vsize"], plan

        print(
            "PASS: dynamic settlement builder tests"
        )

    finally:
        if old is None:
            os.environ.pop(name, None)
        else:
            os.environ[name] = old


if __name__ == "__main__":
    main()
