#!/usr/bin/env python3

import hashlib

from swap_v2 import Swap, State


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def make_swap(**overrides):
    values = {
        "swap_id": "0123456789ab",
        "state": State.BTC_FUNDED.value,
        "created_at": 1,

        "blake_amount_base_units": 10_000,
        "btc_sats": 2730,

        "secret_hex": None,
        "secret_hash": "11" * 32,

        "blake_timeout": 100,
        "btc_timeout": 200,

        "timeout_policy_version": "1.5",
        "btc_timeout_window_seconds": 12_600,
        "blake_timeout_window_seconds": 27_600,
        "bob_recovery_seconds": 14_700,
    }

    values.update(overrides)

    return Swap(**values)


def require_rejected(swap, expected_text):
    try:
        swap.validate()
    except ValueError as exc:
        require(
            expected_text in str(exc),
            (
                f"unexpected validation error: {exc}; "
                f"expected text: {expected_text!r}"
            ),
        )
        return

    raise AssertionError(
        f"Swap.validate unexpectedly accepted: {expected_text}"
    )


def test_old_record_without_signed_hex_is_valid():
    swap = make_swap()

    swap.validate()

    print(
        "PASS: legacy settlement state without durable signed hex remains valid"
    )


def test_signed_btc_claim_requires_identity_anchor():
    swap = make_swap(
        btc_claim_hex="00",
    )

    require_rejected(
        swap,
        "btc_claim_hex requires",
    )

    print(
        "PASS: durable BTC claim hex requires a txid identity anchor"
    )


def test_signed_btc_refund_requires_identity_anchor():
    swap = make_swap(
        btc_refund_hex="00",
    )

    require_rejected(
        swap,
        "btc_refund_hex requires",
    )

    print(
        "PASS: durable BTC refund hex requires a txid identity anchor"
    )


def test_signed_blake_claim_accepts_canonical_txid():
    swap = make_swap(
        blake_claim_txid="22" * 32,
        blake_claim_hex="00",
    )

    swap.validate()

    print(
        "PASS: durable BLAKE claim hex accepts canonical txid anchor"
    )


def test_signed_blake_claim_accepts_reorg_hint():
    swap = make_swap(
        state=State.BTC_CLAIMED.value,
        blake_claim_hex="00",
        blake_reorg_claim_txid="22" * 32,
    )

    swap.validate()

    print(
        "PASS: durable BLAKE claim hex survives canonical rollback "
        "when reorg txid remains"
    )


def test_signed_blake_refund_accepts_reorg_hint():
    swap = make_swap(
        state=State.BTC_REFUNDED.value,
        blake_refund_hex="00",
        blake_reorg_refund_txid="33" * 32,
    )

    swap.validate()

    print(
        "PASS: durable BLAKE refund hex survives canonical rollback "
        "when reorg txid remains"
    )


def test_signed_hex_must_be_valid_hex():
    swap = make_swap(
        btc_claim_txid="44" * 32,
        btc_claim_hex="not-hex",
    )

    require_rejected(
        swap,
        "btc_claim_hex must be valid hex",
    )

    print(
        "PASS: malformed durable settlement hex is rejected"
    )


def test_revealed_secret_must_match_hash():
    good_secret = "11" * 32

    swap = make_swap(
        secret_hash=hashlib.sha256(
            bytes.fromhex(good_secret)
        ).hexdigest(),
        revealed_secret_hex=good_secret,
    )

    swap.validate()

    swap.revealed_secret_hex = "22" * 32

    require_rejected(
        swap,
        "revealed_secret_hex does not match secret_hash",
    )

    print(
        "PASS: mismatched revealed secret is rejected"
    )


def test_revealed_secret_must_be_32_bytes():
    swap = make_swap(
        revealed_secret_hex="11",
    )

    require_rejected(
        swap,
        "revealed_secret_hex must be 32 bytes",
    )

    print(
        "PASS: malformed revealed secret length is rejected"
    )


def main():
    test_old_record_without_signed_hex_is_valid()
    test_signed_btc_claim_requires_identity_anchor()
    test_signed_btc_refund_requires_identity_anchor()
    test_signed_blake_claim_accepts_canonical_txid()
    test_signed_blake_claim_accepts_reorg_hint()
    test_signed_blake_refund_accepts_reorg_hint()
    test_signed_hex_must_be_valid_hex()
    test_settlement_txid_must_be_32_byte_hex()
    test_funding_txid_must_be_32_byte_hex()
    test_revealed_secret_must_match_hash()
    test_revealed_secret_must_be_32_bytes()

    print()
    print(
        "PASS: swap model settlement identity regression suite"
    )


def test_settlement_txid_must_be_32_byte_hex():
    swap = make_swap(
        btc_claim_txid="banana",
    )

    require_rejected(
        swap,
        "btc_claim_txid must be 32-byte hex",
    )

    print(
        "PASS: malformed settlement txid is rejected"
    )


def test_funding_txid_must_be_32_byte_hex():
    swap = make_swap(
        btc_funding_txid="1234",
        btc_funding_vout=0,
    )

    require_rejected(
        swap,
        "btc_funding_txid must be 32-byte hex",
    )

    print(
        "PASS: malformed funding txid is rejected"
    )

if __name__ == "__main__":
    main()
