#!/usr/bin/env python3

"""
BTC/BLAKE atomic-swap wire-format validation.

This module deliberately knows nothing about:

    wallets
    Bitcoin RPC
    BLAKE RPC
    Nostr relays
    local swap files
    protocol state machines

Its job is narrower:

    bytes / JSON
        ->
    structurally valid protocol message

Economic truth remains the responsibility of the swap engine and chains.
"""

from __future__ import annotations

import hashlib
import json
import re
from decimal import Decimal, InvalidOperation
from typing import Any


class WireError(ValueError):
    """A message is not valid according to the wire protocol."""


SWAP_ID_RE = re.compile(r"^[0-9a-f]{12}$")
HEX_32_RE = re.compile(r"^[0-9a-f]{64}$")
PUBKEY_RE = re.compile(r"^(02|03)[0-9a-f]{64}$")


SETTLEMENT_TYPES = {
    "offer",
    "accept",
    "blake-funded",
    "btc-funded",
    "btc-claimed",
    "blake-claimed",
    "btc-refunded",
}


BASE_FIELDS = {
    "type",
    "swap_id",
}


SCHEMAS = {
    "offer": {
        #
        # The v1.5 timeout metadata is mandatory, not optional:
        # _validate_offer rejects an offer without it, and the engine's
        # validate_v15_offer_timeouts does the same. It is listed here as
        # required so that a consumer building from SCHEMAS alone cannot
        # produce an offer the protocol will refuse.
        #
        "required": {
            "type",
            "swap_id",
            "blake_amount",
            "btc_sats",
            "secret_hash",
            "btc_timeout",
            "blake_timeout",
            "created_at",
            "blake_alice_pub",
            "btc_alice_pub",
            "timeout_policy_version",
            "btc_timeout_window_seconds",
            "blake_timeout_window_seconds",
            "bob_recovery_seconds",
        },
        "optional": set(),
    },

    "accept": {
        "required": {
            "type",
            "swap_id",
            "secret_hash",
            "blake_bob_pub",
            "btc_bob_pub",
        },
        "optional": set(),
    },

    "blake-funded": {
        "required": {
            "type",
            "swap_id",
            "secret_hash",
            "blake_amount",
            "blake_timeout",
            "blake_funding_txid",
            "blake_funding_vout",
        },
        "optional": set(),
    },

    "btc-funded": {
        "required": {
            "type",
            "swap_id",
            "secret_hash",
            "btc_sats",
            "btc_timeout",
            "btc_funding_txid",
            "btc_funding_vout",
        },
        "optional": set(),
    },

    "btc-claimed": {
        "required": {
            "type",
            "swap_id",
            "secret_hash",
            "btc_claim_txid",
            "btc_claim_blockhash",
        },
        "optional": set(),
    },

    "blake-claimed": {
        "required": {
            "type",
            "swap_id",
            "secret_hash",
            "blake_claim_txid",
            "blake_claim_blockhash",
        },
        "optional": set(),
    },

    "btc-refunded": {
        "required": {
            "type",
            "swap_id",
            "secret_hash",
            "btc_refund_txid",
            "btc_refund_blockhash",
        },
        "optional": set(),
    },
}


def _require_dict(message: Any) -> dict:
    if not isinstance(message, dict):
        raise WireError("message must be a JSON object")

    return message


def _require_string(
    message: dict,
    field: str,
) -> str:
    value = message.get(field)

    if not isinstance(value, str):
        raise WireError(
            f"{field} must be a string"
        )

    return value


def _require_positive_int(
    message: dict,
    field: str,
) -> int:
    value = message.get(field)

    # bool is an int subclass in Python.
    if (
        isinstance(value, bool)
        or not isinstance(value, int)
    ):
        raise WireError(
            f"{field} must be an integer"
        )

    if value <= 0:
        raise WireError(
            f"{field} must be positive"
        )

    return value


def _require_nonnegative_int(
    message: dict,
    field: str,
) -> int:
    value = message.get(field)

    if (
        isinstance(value, bool)
        or not isinstance(value, int)
    ):
        raise WireError(
            f"{field} must be an integer"
        )

    if value < 0:
        raise WireError(
            f"{field} must be non-negative"
        )

    return value


def _require_hex32(
    message: dict,
    field: str,
) -> str:
    value = _require_string(
        message,
        field,
    )

    if not HEX_32_RE.fullmatch(value):
        raise WireError(
            f"{field} must be 32-byte lowercase hex"
        )

    return value


def _require_pubkey(
    message: dict,
    field: str,
) -> str:
    value = _require_string(
        message,
        field,
    )

    if not PUBKEY_RE.fullmatch(value):
        raise WireError(
            f"{field} must be a compressed secp256k1 "
            "public key"
        )

    return value


def _require_amount(
    message: dict,
    field: str,
) -> Decimal:
    value = _require_string(
        message,
        field,
    )

    try:
        amount = Decimal(value)
    except InvalidOperation as e:
        raise WireError(
            f"{field} must be a decimal string"
        ) from e

    if not amount.is_finite():
        raise WireError(
            f"{field} must be finite"
        )

    if amount <= 0:
        raise WireError(
            f"{field} must be positive"
        )

    return amount


def _validate_common(
    message: dict,
    expected_type: str | None,
) -> str:
    if "secret_hex" in message:
        raise WireError(
            "secret_hex is forbidden on the wire"
        )

    msg_type = _require_string(
        message,
        "type",
    )

    if msg_type not in SETTLEMENT_TYPES:
        raise WireError(
            f"unsupported settlement message type: "
            f"{msg_type!r}"
        )

    if (
        expected_type is not None
        and msg_type != expected_type
    ):
        raise WireError(
            "message type does not match expected type"
        )

    swap_id = _require_string(
        message,
        "swap_id",
    )

    if not SWAP_ID_RE.fullmatch(swap_id):
        raise WireError(
            "swap_id must be 12 lowercase hex characters"
        )

    schema = SCHEMAS[msg_type]

    keys = set(message)

    missing = schema["required"] - keys

    if missing:
        raise WireError(
            "missing required field(s): "
            + ", ".join(sorted(missing))
        )

    unknown = (
        keys
        - schema["required"]
        - schema["optional"]
    )

    if unknown:
        raise WireError(
            "unknown field(s): "
            + ", ".join(sorted(unknown))
        )

    return msg_type


def _validate_offer(message: dict) -> None:
    _require_amount(
        message,
        "blake_amount",
    )

    _require_positive_int(
        message,
        "btc_sats",
    )

    _require_hex32(
        message,
        "secret_hash",
    )

    _require_positive_int(
        message,
        "btc_timeout",
    )

    _require_positive_int(
        message,
        "blake_timeout",
    )

    _require_nonnegative_int(
        message,
        "created_at",
    )

    _require_pubkey(
        message,
        "blake_alice_pub",
    )

    _require_pubkey(
        message,
        "btc_alice_pub",
    )

    version = message.get(
        "timeout_policy_version"
    )

    #
    # The four v1.5 fields are `required` in SCHEMAS["offer"], so the
    # generic required-field check has already proved that the version is
    # present and that the three window fields are. Only the version's
    # *value* is left to check here.
    #
    if version != "1.5":
        raise WireError(
            "unsupported timeout_policy_version"
        )

    btc_window = _require_positive_int(
        message,
        "btc_timeout_window_seconds",
    )

    blake_window = _require_positive_int(
        message,
        "blake_timeout_window_seconds",
    )

    recovery = _require_positive_int(
        message,
        "bob_recovery_seconds",
    )

    if blake_window - btc_window < recovery:
        raise WireError(
            "v1.5 timeout metadata does not "
            "preserve recovery budget"
        )


def _validate_accept(message: dict) -> None:
    _require_hex32(
        message,
        "secret_hash",
    )

    _require_pubkey(
        message,
        "blake_bob_pub",
    )

    _require_pubkey(
        message,
        "btc_bob_pub",
    )


def _validate_blake_funded(
    message: dict,
) -> None:
    _require_hex32(
        message,
        "secret_hash",
    )

    _require_amount(
        message,
        "blake_amount",
    )

    _require_positive_int(
        message,
        "blake_timeout",
    )

    _require_hex32(
        message,
        "blake_funding_txid",
    )

    _require_nonnegative_int(
        message,
        "blake_funding_vout",
    )


def _validate_btc_funded(
    message: dict,
) -> None:
    _require_hex32(
        message,
        "secret_hash",
    )

    _require_positive_int(
        message,
        "btc_sats",
    )

    _require_positive_int(
        message,
        "btc_timeout",
    )

    _require_hex32(
        message,
        "btc_funding_txid",
    )

    _require_nonnegative_int(
        message,
        "btc_funding_vout",
    )


def _validate_btc_claimed(
    message: dict,
) -> None:
    _require_hex32(
        message,
        "secret_hash",
    )

    _require_hex32(
        message,
        "btc_claim_txid",
    )

    _require_hex32(
        message,
        "btc_claim_blockhash",
    )


def _validate_blake_claimed(
    message: dict,
) -> None:
    _require_hex32(
        message,
        "secret_hash",
    )

    _require_hex32(
        message,
        "blake_claim_txid",
    )

    _require_hex32(
        message,
        "blake_claim_blockhash",
    )


def _validate_btc_refunded(
    message: dict,
) -> None:
    _require_hex32(
        message,
        "secret_hash",
    )

    _require_hex32(
        message,
        "btc_refund_txid",
    )

    _require_hex32(
        message,
        "btc_refund_blockhash",
    )


VALIDATORS = {
    "offer": _validate_offer,
    "accept": _validate_accept,
    "blake-funded": _validate_blake_funded,
    "btc-funded": _validate_btc_funded,
    "btc-claimed": _validate_btc_claimed,
    "blake-claimed": _validate_blake_claimed,
    "btc-refunded": _validate_btc_refunded,
}


def validate_message(
    message: Any,
    *,
    expected_type: str | None = None,
) -> dict:
    """
    Validate one decoded settlement message.

    Returns a shallow copy so callers do not accidentally
    treat mutation of their input as part of validation.
    """

    message = _require_dict(message)

    msg_type = _validate_common(
        message,
        expected_type,
    )

    VALIDATORS[msg_type](message)

    return dict(message)


def decode_message(
    raw: str | bytes,
    *,
    expected_type: str | None = None,
) -> dict:
    """
    Decode and validate one JSON wire message.

    Duplicate JSON object keys are rejected.
    """

    if isinstance(raw, bytes):
        try:
            raw = raw.decode("utf-8")
        except UnicodeDecodeError as e:
            raise WireError(
                "wire message is not valid UTF-8"
            ) from e

    if not isinstance(raw, str):
        raise WireError(
            "wire input must be str or bytes"
        )

    def object_pairs(pairs):
        result = {}

        for key, value in pairs:
            if key in result:
                raise WireError(
                    f"duplicate JSON key: {key}"
                )

            result[key] = value

        return result

    try:
        message = json.loads(
            raw,
            object_pairs_hook=object_pairs,
        )
    except WireError:
        raise
    except json.JSONDecodeError as e:
        raise WireError(
            "invalid JSON"
        ) from e

    return validate_message(
        message,
        expected_type=expected_type,
    )


def encode_message(
    message: Any,
    *,
    expected_type: str | None = None,
) -> str:
    """
    Validate then serialize to the canonical JSON form
    currently used inside signed Nostr event content.
    """

    validated = validate_message(
        message,
        expected_type=expected_type,
    )

    return json.dumps(
        validated,
        separators=(",", ":"),
        sort_keys=True,
        ensure_ascii=False,
    )


def sha256_hex(data: bytes) -> str:
    """Small helper useful to independent implementations."""
    return hashlib.sha256(data).hexdigest()
