from decimal import Decimal

from fee_policy import (
    coin_per_kvb_to_sat_vb,
    choose_fee_rate_sat_vb,
    fee_for_vsize,
    enforce_fee_cap,
)


def main():
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
