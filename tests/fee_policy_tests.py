from decimal import Decimal

from fee_policy import (
    coin_per_kvb_to_sat_vb,
    choose_fee_rate_sat_vb,
    fee_for_vsize,
    enforce_fee_cap,
    minimum_input_for_rate,
)



def edge_cases():
    """
    The refusal and fallback branches.

    These are the paths that matter when a node answers with something
    unexpected: each one either falls back to the policy floor or fails
    closed with a named error, and never silently picks a fee.
    """

    print("PASS: feerate conversion rejects non-positive values")

    for bad in (0, "0", -1, "-0.00001"):
        try:
            coin_per_kvb_to_sat_vb(bad)
        except ValueError:
            pass
        else:
            raise AssertionError(f"feerate {bad!r} was accepted")

    print("PASS: a malformed estimator feerate is ignored, not fatal")

    # decimal.InvalidOperation is an ArithmeticError, not a ValueError:
    # a node that returns a non-numeric feerate must not abort the
    # settlement, it must fall back to the policy floor.
    rate = choose_fee_rate_sat_vb(
        {"feerate": "unavailable"},
        {"mempoolminfee": 0.00001000},
    )

    assert rate == Decimal("1.00000000"), rate

    print("PASS: a malformed policy floor is ignored, not fatal")

    rate = choose_fee_rate_sat_vb(
        {"feerate": 0.00001190},
        {"mempoolminfee": "nonsense", "minrelaytxfee": None},
    )

    assert rate == Decimal("1.19000000"), rate

    print("PASS: an unavailable estimator falls back to the policy floor")

    # No "feerate" key at all (estimatesmartfee reports "errors" instead),
    # and a zero estimate, which is not a usable rate.
    for estimate in ({}, {"feerate": 0}, {"blocks": 2}, None):
        rate = choose_fee_rate_sat_vb(
            estimate,
            {"mempoolminfee": 0.00001000, "minrelaytxfee": 0.00000500},
        )

        assert rate == Decimal("1.00000000"), (estimate, rate)

    print("PASS: the policy floor wins when the estimator is unusable")

    rate = choose_fee_rate_sat_vb(
        None,
        {"minrelaytxfee": 0.00003000},
    )

    assert rate == Decimal("3.00000000"), rate

    print("PASS: no usable fee data fails closed")

    for estimate, mempool in (
        ({}, {}),
        (None, None),
        ({"feerate": 0}, {"mempoolminfee": None, "minrelaytxfee": None}),
    ):
        try:
            choose_fee_rate_sat_vb(estimate, mempool)
        except RuntimeError as e:
            assert "no usable fee estimate" in str(e), e
        else:
            raise AssertionError(
                f"a missing fee floor was accepted: {estimate} {mempool}"
            )

    print("PASS: a multiplier below 1.0 is refused")

    for bad in (Decimal("0.99"), Decimal("0"), 1.0 if False else Decimal("-2")):
        try:
            choose_fee_rate_sat_vb(
                {"feerate": 0.00001000},
                {},
                multiplier=bad,
            )
        except ValueError as e:
            assert "multiplier" in str(e), e
        else:
            raise AssertionError(f"multiplier {bad} was accepted")

    print("PASS: vsize and feerate are validated")

    for bad_vsize in (0, -1):
        try:
            fee_for_vsize(bad_vsize, Decimal("1"))
        except ValueError as e:
            assert "vsize" in str(e), e
        else:
            raise AssertionError(f"vsize {bad_vsize} was accepted")

    for bad_rate in (0, "-1"):
        try:
            fee_for_vsize(131, bad_rate)
        except ValueError as e:
            assert "feerate" in str(e), e
        else:
            raise AssertionError(f"feerate {bad_rate!r} was accepted")

    print("PASS: the fee cap validates its own inputs")

    for fee, amount in ((0, 100), (1, 0), (-5, 100), (1, -5)):
        try:
            enforce_fee_cap(fee, amount)
        except ValueError:
            pass
        else:
            raise AssertionError(
                f"fee={fee} input={amount} was accepted"
            )

    for bad_percent in (0, -1, 100, 120):
        try:
            enforce_fee_cap(1, 1000, max_percent=Decimal(str(bad_percent)))
        except ValueError as e:
            assert "percent" in str(e), e
        else:
            raise AssertionError(f"max_percent {bad_percent} was accepted")

    print("PASS: an absolute cap can only make the limit stricter")

    # Stricter than the percentage cap.
    try:
        enforce_fee_cap(
            50,
            100000,
            absolute_cap_sats=10,
        )
    except ValueError as e:
        assert "safety cap" in str(e), e
    else:
        raise AssertionError("an absolute cap did not bind")

    # Looser than the percentage cap, so the percentage still binds:
    # 20 000 exceeds the 10% cap of 10 000 even though the absolute cap
    # of 100 000 would have allowed it.
    try:
        enforce_fee_cap(
            20000,
            100000,
            absolute_cap_sats=100000,
        )
    except ValueError as e:
        assert "safety cap" in str(e), e
    else:
        raise AssertionError("the percentage cap stopped binding")

    # Permitted under both.
    assert enforce_fee_cap(50, 100000, absolute_cap_sats=100) == 50

    try:
        enforce_fee_cap(50, 100000, absolute_cap_sats=0)
    except ValueError as e:
        assert "absolute fee cap" in str(e), e
    else:
        raise AssertionError("a zero absolute cap was accepted")

    print("PASS: a fee that would consume the whole input is refused")

    # The percentage cap rounds up to exactly the input, so the cap does
    # not catch this; the input-consumption guard must.
    try:
        enforce_fee_cap(100, 100, max_percent=Decimal("99.9"))
    except ValueError as e:
        assert "entire HTLC input" in str(e), e
    else:
        raise AssertionError("a fee equal to the input was accepted")



#
# Real fee environments.
#
# Per-block fee percentiles from mempool.space
# (`/api/v1/mining/blocks/fee-rates/{1m,6m,1y,2y,3y}`, fetched 2026-09-28).
# `marginal` is `avgFee_0`: the lowest feerate that block actually included,
# which is the rate a settlement has to beat to land in a block like it.
# `median` is `avgFee_50`.
#
# The point of pinning these numbers is that the *percentage cap*, not the
# estimator, decides whether a small swap can be settled at all, and that
# is a property the policy should keep honest against real markets rather
# than against a number someone liked.
#
HISTORICAL_FEE_ENVIRONMENTS = {
    "1m":  {"blocks": 1425, "marginal": {"p50": 0.0,  "p90": 0.0,  "p99": 2.0,   "max": 5.0}},
    "6m":  {"blocks": 1473, "marginal": {"p50": 0.0,  "p90": 1.0,  "p99": 2.0,   "max": 4.0}},
    "1y":  {"blocks": 1096, "marginal": {"p50": 0.0,  "p90": 1.0,  "p99": 2.0,   "max": 3.0}},
    "2y":  {"blocks": 2191, "marginal": {"p50": 1.0,  "p90": 4.0,  "p99": 15.0,  "max": 57.0}},
    "3y":  {"blocks": 2193, "marginal": {"p50": 2.0,  "p90": 33.8, "p99": 171.08, "max": 889.0}},
}

#
# Measured from real settlements on the regtest rig, not estimated: a claim
# carries the preimage as well as the signature, a refund only the
# signature.
#
MEASURED_CLAIM_VSIZE = 140
MEASURED_REFUND_VSIZE = 131


def historical_fee_environments():
    """
    What the cap permits, and needs, in each real fee environment.

    This is the arithmetic an operator needs before choosing a swap size:
    at a 10% cap a 140 vB claim can pay 1.857 sat/vB on a 2,600 satoshi
    HTLC, and 55.1% of the last three years' blocks wanted more than that,
    so such a swap cannot be settled in them at all -- the engine refuses
    rather than paying a much larger share of the HTLC in fees.
    """
    percent = Decimal("10")

    for name, environment in HISTORICAL_FEE_ENVIRONMENTS.items():
        for rate in (
            environment["marginal"]["p50"],
            environment["marginal"]["p90"],
            environment["marginal"]["p99"],
        ):
            if rate <= 0:
                continue

            needed = minimum_input_for_rate(
                Decimal(str(rate)),
                MEASURED_CLAIM_VSIZE,
                max_percent=percent,
            )

            #
            # The formula has to agree with what the cap actually enforces,
            # in both directions: the needed input is accepted, and one
            # satoshi less is refused.
            #
            cap_at_needed = (
                Decimal(needed) * percent / Decimal("100")
            ).to_integral_value(rounding="ROUND_CEILING")

            fee_at_needed = (
                Decimal(MEASURED_CLAIM_VSIZE) * Decimal(str(rate))
            ).to_integral_value(rounding="ROUND_CEILING")

            enforce_fee_cap(
                int(fee_at_needed),
                needed,
                max_percent=percent,
                vsize=MEASURED_CLAIM_VSIZE,
            )

            assert int(fee_at_needed) <= int(cap_at_needed), (
                environment,
                rate,
            )

            try:
                enforce_fee_cap(
                    int(fee_at_needed),
                    needed - 1,
                    max_percent=percent,
                    vsize=MEASURED_CLAIM_VSIZE,
                )
            except ValueError:
                pass
            else:
                raise AssertionError(
                    "one satoshi below the documented minimum was accepted "
                    f"({name}, {rate} sat/vB, {needed - 1} sats)"
                )

    print(
        "PASS: the minimum viable HTLC size matches the enforced cap in "
        f"{len(HISTORICAL_FEE_ENVIRONMENTS)} real fee environments"
    )


def the_cap_refusal_is_actionable():
    """
    The refusal has to name the rate the cap permits and the knob to change.

    "fee exceeds cap" on its own leaves an operator unable to tell whether
    to raise the cap, wait, or give up on the swap size.
    """
    needed = minimum_input_for_rate(
        Decimal("33.8"),
        MEASURED_CLAIM_VSIZE,
        max_percent=Decimal("10"),
    )

    try:
        enforce_fee_cap(
            4732,
            needed - 1,
            max_percent=Decimal("10"),
            vsize=MEASURED_CLAIM_VSIZE,
        )
    except ValueError as e:
        message = str(e)

        for expected in (
            "safety cap",
            "vsize=140",
            "permits 33.79",
            "below the 47311 satoshi minimum",
            "BLAKE_SWAP_MAX_SETTLEMENT_FEE_PERCENT",
            "BLAKE_SWAP_MAX_SETTLEMENT_FEE_SATS",
        ):
            assert expected in message, (expected, message)
    else:
        raise AssertionError("the cap did not bind where it should have")

    print("PASS: the cap refusal names the affordable rate and the knobs")


def the_documented_sizes_behave_as_documented():
    """
    The sizes this repository uses in its own docs and tests.

    A 2,600 satoshi HTLC is the small end the regtest suites use, and it
    affords 1.857 sat/vB for a 140 vB claim: in 55.1% of the last three
    years' blocks the market wanted more than that, including the median
    block. A 25,000 satoshi HTLC affords 17.857 sat/vB and clears 83.7% of
    them. If a future change to the cap makes these numbers wrong, the
    documentation is wrong with it.
    """
    percent = Decimal("10")

    cases = (
        #
        # (input sats, rate sat/vB, settleable)
        #
        # Each pair is the same HTLC either side of the boundary its cap
        # implies: 2,600 sats afford 1.857 sat/vB, 10,000 afford 7.143,
        # 25,000 afford 17.857, 100,000 afford 71.428, 1,000,000 afford
        # 714.286.
        #
        (2600, "1.8", True),
        (2600, "1.86", False),
        (10000, "7.0", True),
        (10000, "7.15", False),
        (25000, "17.8", True),
        (25000, "17.9", False),
        (100000, "71.4", True),
        (100000, "71.5", False),
        (1000000, "700.0", True),
        (1000000, "720.0", False),
        #
        # And the historical percentiles from the table above, which is
        # where these sizes come from: the median block of the last three
        # years needed 2.0 sat/vB, which a 2,600 satoshi HTLC cannot pay.
        #
        (2600, "2.0", False),
        (47311, "33.8", True),
        (239511, "171.08", True),
    )

    for input_sats, rate, settleable in cases:
        fee = int(
            (
                Decimal(MEASURED_CLAIM_VSIZE) * Decimal(rate)
            ).to_integral_value(rounding="ROUND_CEILING")
        )

        try:
            enforce_fee_cap(
                fee,
                input_sats,
                max_percent=percent,
                vsize=MEASURED_CLAIM_VSIZE,
            )
        except ValueError:
            settled = False
        else:
            settled = True

        assert settled == settleable, (
            f"{input_sats} sats at {rate} sat/vB: "
            f"expected settleable={settleable}, got {settled}"
        )

    print("PASS: documented swap sizes are settleable exactly where documented")

def main():
    edge_cases()
    historical_fee_environments()
    the_cap_refusal_is_actionable()
    the_documented_sizes_behave_as_documented()

    # Bitcoin live observation:
    # 0.00001190 coin/kvB == 1.19 sat/vB.
    assert (
        coin_per_kvb_to_sat_vb("0.00001190")
        == Decimal("1.19000000")
    )

    # BLAKE live observation:
    # 0.00001000 coin/kvB == 1 sat/vB.
    assert (
        coin_per_kvb_to_sat_vb("0.00001000")
        == Decimal("1.00000000")
    )

    btc_rate = choose_fee_rate_sat_vb(
        {
            "feerate": 0.00001190,
            "blocks": 2,
        },
        {
            "mempoolminfee": 0.00000100,
            "minrelaytxfee": 0.00000100,
        },
    )

    assert btc_rate == Decimal("1.19000000")

    # The confirmed live refund was 131 vB.
    assert fee_for_vsize(131, btc_rate) == 156

    blake_rate = choose_fee_rate_sat_vb(
        {
            "feerate": 0.00001000,
            "blocks": 2,
        },
        {
            "mempoolminfee": 0.00001000,
            "minrelaytxfee": 0.00001000,
            # Must NOT force the chosen tx feerate to 3 sat/vB.
            "dustrelayfee": 0.00003000,
        },
    )

    assert blake_rate == Decimal("1.00000000")
    assert fee_for_vsize(131, blake_rate) == 131

    # Policy floor wins when the estimator is lower.
    floor_rate = choose_fee_rate_sat_vb(
        {"feerate": 0.00000100},
        {
            "mempoolminfee": 0.00001000,
            "minrelaytxfee": 0.00001000,
        },
    )

    assert floor_rate == Decimal("1.00000000")

    # Missing estimator is allowed when policy information exists.
    fallback_rate = choose_fee_rate_sat_vb(
        {"errors": ["Insufficient data or no feerate found"]},
        {
            "mempoolminfee": 0.00001000,
            "minrelaytxfee": 0.00001000,
        },
    )

    assert fallback_rate == Decimal("1.00000000")

    # Optional multiplier rounds upward at final fee calculation.
    boosted = choose_fee_rate_sat_vb(
        {"feerate": 0.00001000},
        {
            "mempoolminfee": 0.00001000,
            "minrelaytxfee": 0.00001000,
        },
        multiplier=Decimal("1.10"),
    )

    assert boosted == Decimal("1.100000000")

    assert fee_for_vsize(131, boosted) == 145

    # 10% default-style cap permits ordinary settlement fee.
    assert (
        enforce_fee_cap(
            156,
            2500,
            max_percent=Decimal("10"),
        )
        == 156
    )

    # But recreates protection against the BLAKE live-run problem:
    # 10,000 sat-units on a 20,000 sat-unit input must fail.
    try:
        enforce_fee_cap(
            10000,
            20000,
            max_percent=Decimal("10"),
        )
    except ValueError:
        pass
    else:
        raise AssertionError(
            "50% settlement fee was not rejected"
        )

    print("PASS: dynamic fee policy tests")


if __name__ == "__main__":
    main()
