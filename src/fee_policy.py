from decimal import Decimal, ROUND_CEILING


SAT_UNITS_PER_COIN = Decimal("100000000")
VBYTES_PER_KVB = Decimal("1000")


def coin_per_kvb_to_sat_vb(value):
    """
    Convert node RPC feerate units (coin/kvB) to sat-units/vB.
    """
    rate = (
        Decimal(str(value))
        * SAT_UNITS_PER_COIN
        / VBYTES_PER_KVB
    )

    if rate <= 0:
        raise ValueError("feerate must be positive")

    return rate


def choose_fee_rate_sat_vb(
    estimate_result,
    mempool_info,
    *,
    multiplier=Decimal("1.0"),
):
    """
    Select a settlement feerate from live node data.

    Uses the maximum of:
      - estimatesmartfee feerate, when available
      - mempoolminfee
      - minrelaytxfee

    dustrelayfee is intentionally not used as a transaction-fee floor.
    """
    candidates = []

    if isinstance(estimate_result, dict):
        value = estimate_result.get("feerate")
        if value is not None:
            try:
                candidates.append(
                    coin_per_kvb_to_sat_vb(value)
                )
            except (ValueError, TypeError):
                pass

    if isinstance(mempool_info, dict):
        for field in (
            "mempoolminfee",
            "minrelaytxfee",
        ):
            value = mempool_info.get(field)

            if value is None:
                continue

            try:
                candidates.append(
                    coin_per_kvb_to_sat_vb(value)
                )
            except (ValueError, TypeError):
                pass

    if not candidates:
        raise RuntimeError(
            "Node returned no usable fee estimate or policy floor"
        )

    multiplier = Decimal(str(multiplier))

    if multiplier < Decimal("1.0"):
        raise ValueError(
            "fee-rate multiplier must be >= 1.0"
        )

    return max(candidates) * multiplier


def fee_for_vsize(vsize, rate_sat_vb):
    """
    Return the minimum integer sat-unit fee needed for vsize.
    """
    vsize = int(vsize)
    rate = Decimal(str(rate_sat_vb))

    if vsize < 1:
        raise ValueError("vsize must be >= 1")

    if rate <= 0:
        raise ValueError("feerate must be positive")

    return int(
        (Decimal(vsize) * rate).to_integral_value(
            rounding=ROUND_CEILING
        )
    )


def enforce_fee_cap(
    fee_sats,
    input_sats,
    *,
    max_percent=Decimal("10"),
    absolute_cap_sats=None,
):
    """
    Fail closed rather than silently consume an excessive share
    of the HTLC input.

    The percentage cap is applied first. An optional absolute cap
    may make the limit stricter.
    """
    fee_sats = int(fee_sats)
    input_sats = int(input_sats)
    max_percent = Decimal(str(max_percent))

    if fee_sats < 1:
        raise ValueError("fee must be >= 1 sat-unit")

    if input_sats < 1:
        raise ValueError("input amount must be >= 1 sat-unit")

    if max_percent <= 0 or max_percent >= 100:
        raise ValueError(
            "maximum settlement fee percent must be > 0 and < 100"
        )

    percent_cap = int(
        (
            Decimal(input_sats)
            * max_percent
            / Decimal("100")
        ).to_integral_value(
            rounding=ROUND_CEILING
        )
    )

    cap = percent_cap

    if absolute_cap_sats is not None:
        absolute_cap_sats = int(absolute_cap_sats)

        if absolute_cap_sats < 1:
            raise ValueError(
                "absolute fee cap must be >= 1 sat-unit"
            )

        cap = min(cap, absolute_cap_sats)

    if fee_sats > cap:
        raise ValueError(
            "Settlement fee exceeds configured safety cap: "
            f"fee={fee_sats}, cap={cap}, input={input_sats}"
        )

    if fee_sats >= input_sats:
        raise ValueError(
            "Settlement fee would consume the entire HTLC input"
        )

    return fee_sats
