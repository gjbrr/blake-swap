from decimal import Decimal

from fee_policy import (
    coin_per_kvb_to_sat_vb,
    choose_fee_rate_sat_vb,
    fee_for_vsize,
    enforce_fee_cap,
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


def main():
    edge_cases()

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
