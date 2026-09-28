import contextlib
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


class RecordingCli(FakeCli):
    """
    A node that records which RPCs were asked of it.

    Its estimator answers 1 sat/vB, deliberately unlike every rate an
    override below uses, so a plan built from the estimator cannot be
    mistaken for one built from the knob.
    """

    def __init__(self, decoded=None, mempool=None):
        super().__init__()

        if decoded is not None:
            self.decoded = decoded

        self.mempool = mempool
        self.calls = []

    def __call__(self, method, *args, **kwargs):
        self.calls.append(method)

        if method == "getmempoolinfo" and self.mempool is not None:
            return dict(self.mempool)

        return super().__call__(method, *args, **kwargs)

    def called(self, method):
        return method in self.calls


@contextlib.contextmanager
def environment(**values):
    saved = {
        key: os.environ.get(key)
        for key in values
    }

    for key, value in values.items():
        if value is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = value

    try:
        yield
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def check_rate_override():
    """
    The protocol must let the user override the fee rate.

    A client with its own view of the fee market -- the requirement this
    engine exists to serve -- reads current rates, adds the margin its
    urgency deserves, and hands the engine an exact rate. The engine must
    then build at that rate rather than second-guess it with its own
    estimator.
    """
    blake_rate = "BLAKE_SWAP_BLAKE_SPEND_FEE_RATE"
    btc_rate = "BLAKE_SWAP_BTC_SPEND_FEE_RATE"
    blake_sats = "BLAKE_SWAP_BLAKE_SPEND_FEE_SATS"
    percent_cap = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_PERCENT"
    absolute_cap = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_SATS"

    clear = {
        blake_rate: None,
        btc_rate: None,
        blake_sats: None,
        percent_cap: None,
        absolute_cap: None,
    }

    #
    # Every fee built below measures 100 vB, so the fee and the rate are
    # readable off the built value directly.
    #
    decoded = {
        "hex-fee-1": 100,
        "hex-fee-100": 100,
        "hex-fee-200": 100,
        "hex-fee-250": 100,
        "hex-fee-300": 100,
        "hex-fee-500": 100,
        "hex-fee-1000": 100,
        "hex-fee-10000": 100,
    }

    built = []

    def build(fee_sats):
        built.append(fee_sats)
        return f"hex-fee-{fee_sats}"

    with environment(**clear):
        #
        # 10 sat/vB: the loop must converge on 10 x 100 = 1000 sats, not
        # on the estimator's 1 x 100.
        #
        cli = RecordingCli(decoded)

        with environment(**{blake_rate: "10"}):
            plan = swap_v2.build_dynamic_settlement_spend(
                chain="blake",
                cli=cli,
                input_sats=20_000,
                build_finalized=build,
            )

        assert built == [1, 1000], built
        assert plan["fee_sats"] == 1000, plan
        assert plan["vsize"] == 100, plan
        assert plan["rate_sat_vb"] == 10, plan
        assert plan["source"] == "rate-override", plan

        # The estimator is not consulted at all: the operator already
        # said what to pay, so asking the node would be second-guessing.
        assert cli.called("decoderawtransaction"), cli.calls
        assert not cli.called("estimatesmartfee"), cli.calls

        #
        # A fractional rate is a rate: 100 vB at 2.5 sat/vB is 250 sats,
        # which no integer-only knob could express.
        #
        built.clear()

        with environment(**{blake_rate: "2.5"}):
            plan = swap_v2.build_dynamic_settlement_spend(
                chain="blake",
                cli=RecordingCli(decoded),
                input_sats=20_000,
                build_finalized=build,
            )

        assert plan["fee_sats"] == 250, plan
        assert plan["source"] == "rate-override", plan

        #
        # The knobs are per chain. A BTC rate must not steer the BLAKE
        # leg; if the chain were ignored the source tag would say so.
        #
        with environment(**{btc_rate: "50"}):
            plan = swap_v2.build_dynamic_settlement_spend(
                chain="blake",
                cli=RecordingCli(decoded),
                input_sats=20_000,
                build_finalized=build,
            )

        assert plan["source"] == "dynamic", plan
        assert plan["fee_sats"] == 100, plan

        #
        # The exact-satoshis knob still wins: it predates this one, and
        # the emergency procedures depend on it being the most specific.
        #
        with environment(**{blake_sats: "200", blake_rate: "10"}):
            plan = swap_v2.build_dynamic_settlement_spend(
                chain="blake",
                cli=RecordingCli(decoded),
                input_sats=20_000,
                build_finalized=build,
            )

        assert plan["source"] == "override", plan
        assert plan["fee_sats"] == 200, plan

        #
        # An unparseable or impossible rate is refused by name, and no
        # transaction is ever built from it.
        #
        for bad in ("abc", "0", "-1", "nan", "inf", " "):
            built.clear()

            try:
                with environment(**{blake_rate: bad}):
                    swap_v2.build_dynamic_settlement_spend(
                        chain="blake",
                        cli=RecordingCli(decoded),
                        input_sats=20_000,
                        build_finalized=build,
                    )
            except ValueError as exc:
                assert blake_rate in str(exc), exc
            else:
                raise AssertionError(
                    f"a settlement fee rate of {bad!r} was accepted"
                )

            assert built == [], (bad, built)

        #
        # A rate the node will not relay or admit is refused here, with
        # the node's own number and the binding field named. Letting the
        # broadcast fail later would surface as an unrelated send error.
        #
        below_relay = {
            "minrelaytxfee": 0.00005000,
            "mempoolminfee": 0.00000100,
        }

        with environment(**{blake_rate: "3"}):
            try:
                swap_v2.build_dynamic_settlement_spend(
                    chain="blake",
                    cli=RecordingCli(decoded, below_relay),
                    input_sats=20_000,
                    build_finalized=build,
                )
            except ValueError as exc:
                assert blake_rate in str(exc), exc
                assert "3" in str(exc), exc
                assert "minrelaytxfee" in str(exc), exc
            else:
                raise AssertionError(
                    "a rate below minrelaytxfee was accepted"
                )

        #
        # `mempoolminfee` rises above relay policy when the mempool is
        # congested, and a transaction below it is rejected just as
        # surely. Checking only minrelaytxfee would let this through.
        #
        congested = {
            "minrelaytxfee": 0.00001000,
            "mempoolminfee": 0.00005000,
        }

        with environment(**{blake_rate: "3"}):
            try:
                swap_v2.build_dynamic_settlement_spend(
                    chain="blake",
                    cli=RecordingCli(decoded, congested),
                    input_sats=20_000,
                    build_finalized=build,
                )
            except ValueError as exc:
                assert "mempoolminfee" in str(exc), exc
                assert "5" in str(exc), exc
            else:
                raise AssertionError(
                    "a rate below mempoolminfee was accepted"
                )

        # A rate exactly at the floor is accepted: a floor is a floor,
        # not a rate that must be beaten.
        at_floor = {
            "minrelaytxfee": 0.00001000,
            "mempoolminfee": 0.00001000,
        }

        built.clear()

        with environment(**{blake_rate: "1"}):
            plan = swap_v2.build_dynamic_settlement_spend(
                chain="blake",
                cli=RecordingCli(decoded, at_floor),
                input_sats=20_000,
                build_finalized=build,
            )

        assert plan["source"] == "rate-override", plan
        assert plan["fee_sats"] == 100, plan

        #
        # An override says what to pay, not what may be paid: both caps
        # still bind. 100 vB at 100 sat/vB is 10,000 sats against a
        # 20,000-sat input -- half of it -- so the percentage ceiling
        # must refuse.
        #
        with environment(**{blake_rate: "100"}):
            try:
                swap_v2.build_dynamic_settlement_spend(
                    chain="blake",
                    cli=RecordingCli(decoded),
                    input_sats=20_000,
                    build_finalized=build,
                )
            except ValueError as exc:
                assert "safety cap" in str(exc), exc
            else:
                raise AssertionError(
                    "a rate override bypassed the percentage cap"
                )

        with environment(**{blake_rate: "10", absolute_cap: "500"}):
            try:
                swap_v2.build_dynamic_settlement_spend(
                    chain="blake",
                    cli=RecordingCli(decoded),
                    input_sats=20_000,
                    build_finalized=build,
                )
            except ValueError as exc:
                assert absolute_cap in str(exc), exc
            else:
                raise AssertionError(
                    "a rate override bypassed the absolute cap"
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

        check_rate_override()

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
