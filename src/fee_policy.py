from decimal import Decimal, ROUND_CEILING, ROUND_FLOOR


SAT_UNITS_PER_COIN = Decimal("100000000")
VBYTES_PER_KVB = Decimal("1000")

#
# The knobs this policy is configured by. Named here because the refusal
# message has to tell an operator which one to change.
#
PERCENT_CAP_ENV = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_PERCENT"
ABSOLUTE_CAP_ENV = "BLAKE_SWAP_MAX_SETTLEMENT_FEE_SATS"
FEE_RATE_MULTIPLIER_ENV = "BLAKE_SWAP_FEE_RATE_MULTIPLIER"


def minimum_input_for_rate(rate_sat_vb, vsize, *, max_percent=Decimal("10")):
    """
    The smallest HTLC input that can pay `rate_sat_vb` for `vsize` vB.

    The percentage cap is what usually binds, not the estimator, so this
    is the number that decides whether a swap is settleable at all in a
    given fee environment:

        required_fee  = ceil(vsize * rate)
        cap           = ceil(input * percent / 100)
        input_minimum = the smallest input whose cap reaches required_fee

    The cap rounds *up*, so the boundary is not exactly
    `rate * vsize * 100 / percent`: for a 140 vB claim at 2 sat/vB and a
    10% cap the smallest viable input is 2,791 satoshis, where the rough
    formula says 2,800. Being exact here is what lets the documented table
    and the enforced cap be checked against each other.

    Equivalently, an input of `input` can afford `cap / vsize` sat/vB: a
    2,600 satoshi HTLC can afford about 1.86 sat/vB for a 140 vB claim.
    """
    vsize = int(vsize)
    rate = Decimal(str(rate_sat_vb))
    max_percent = Decimal(str(max_percent))

    if vsize < 1:
        raise ValueError("vsize must be >= 1")

    if rate <= 0:
        raise ValueError("feerate must be positive")

    if max_percent <= 0 or max_percent >= 100:
        raise ValueError(
            "maximum settlement fee percent must be > 0 and < 100"
        )

    required_fee = fee_for_vsize(vsize, rate)

    return int(
        (
            (Decimal(required_fee) - 1) * 100 / max_percent
        ).to_integral_value(rounding=ROUND_FLOOR)
    ) + 1


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
            except (ValueError, TypeError, ArithmeticError):
                #
                # ArithmeticError covers decimal.InvalidOperation, which
                # is what a non-numeric node field produces. It is not a
                # ValueError, so without it a malformed feerate would
                # escape and abort the whole settlement instead of being
                # ignored in favour of the policy floor.
                #
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
            except (ValueError, TypeError, ArithmeticError):
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
    vsize=None,
):
    """
    Fail closed rather than silently consume an excessive share
    of the HTLC input.

    The percentage cap is applied first. An optional absolute cap
    may make the limit stricter.

    `vsize`, when known, is used only to make the refusal actionable: the
    percentage cap translates to a maximum affordable feerate, and an
    operator staring at "fee exceeds cap" needs that number to decide
    whether to raise the cap, wait for a cheaper mempool, or accept that
    this swap is too small to settle at the current rate.
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
        detail = (
            "Settlement fee exceeds configured safety cap: "
            f"fee={fee_sats}, cap={cap}, input={input_sats}"
        )

        if vsize:
            affordable = Decimal(cap) / Decimal(int(vsize))

            detail += (
                f", vsize={int(vsize)} vB "
                f"(the cap permits {affordable:.3f} sat/vB)"
            )

        if vsize:
            implied_rate = Decimal(fee_sats) / Decimal(int(vsize))

            needed = minimum_input_for_rate(
                implied_rate,
                int(vsize),
                max_percent=max_percent,
            )

            detail += (
                f"; this {input_sats} satoshi HTLC is below the {needed} "
                f"satoshi minimum a {int(vsize)} vB spend needs at "
                f"{implied_rate:.3f} sat/vB"
            )

        detail += (
            f". Raise {PERCENT_CAP_ENV} (currently {max_percent}), "
            f"set {ABSOLUTE_CAP_ENV} to bound the absolute fee, or wait "
            "for a cheaper mempool."
        )

        raise ValueError(detail)

    if fee_sats >= input_sats:
        raise ValueError(
            "Settlement fee would consume the entire HTLC input"
        )

    return fee_sats
