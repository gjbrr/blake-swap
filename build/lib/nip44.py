#!/usr/bin/env python3
"""
NIP-44 v2 payload encryption for coordination content.

This module is deliberately standalone: it knows about secp256k1, HKDF,
ChaCha20 and HMAC, and nothing about swaps. `swap_v2.py` imports it only
when the encrypted coordination mode is enabled.

**What this protects.** The `content` field of a coordination event, and
nothing else. The payload is authenticated (HMAC-SHA256) and the whole
payload is committed by the NIP-01 event signature.

**What it does not protect.** The event's author, kind, tags and timestamp;
which relay it was published to; when; how often; and its approximate size.
To hide participants or timing you would need gift-wrap with a fresh key per
message and authenticated relays. There is no forward secrecy: the
conversation key is a long-lived shared secret, so compromise of either
identity key decrypts every message that pair ever exchanged.

**Specification.** NIP-44 version 2, as published in
`nostr-protocol/nips` (44.md). The companion test suite verifies this
implementation against the official vectors from `paulmillr/nip44`
(`nip44.vectors.json`), so the KDF details — which are not
self-describing and would fail silently against a peer — are pinned.

The optional dependency is imported lazily: a plaintext-only deployment
does not need `cryptography`.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import os
from decimal import Decimal, InvalidOperation


VERSION = 2

#
# A payload is version(1) || nonce(32) || ciphertext(>=34) || mac(32).
# The smallest legal ciphertext is a padded plaintext of 32 bytes plus the
# 2-byte length prefix, so 34. Anything shorter cannot be valid, and is
# rejected before any decryption work.
#
MIN_PAYLOAD_BYTES = 1 + 32 + 34 + 32
MIN_BASE64_CHARS = 132

#
# The protocol never needs a coordination message anywhere near this size,
# and a base64 decode allocates several times the input length. Refuse
# before decoding rather than after.
#
MAX_BASE64_CHARS = 1 << 20

DEFAULT_NONCE_BYTES = 32

DOMAIN_SALT = b"nip44-v2"

#
# The `#` prefix marks a payload that is deliberately not base64 (a future
# encoding). It must be reported as an unsupported version, never as a
# base64 error.
#
FUTURE_ENCODING_PREFIX = "#"


class Nip44Error(ValueError):
    """Any refusal from this module."""


def _crypto():
    """
    Import the optional cryptography dependency on first use.

    Raises a named error rather than an ImportError traceback, because the
    only caller is a configuration path where the message matters.
    """

    try:
        from cryptography.hazmat.primitives import hashes
        from cryptography.hazmat.primitives.ciphers import Cipher, algorithms
        from cryptography.hazmat.primitives.kdf.hkdf import HKDFExpand
    except ImportError as e:  # pragma: no cover - depends on the environment
        raise Nip44Error(
            "NIP-44 needs the 'cryptography' package; install it with "
            "`pip install cryptography` or leave coordination encryption "
            "disabled (the default)"
        ) from e

    return hashes, Cipher, algorithms, HKDFExpand


def _coincurve():
    try:
        from coincurve import PublicKey
    except ImportError as e:  # pragma: no cover - declared dependency
        raise Nip44Error(
            "NIP-44 needs 'coincurve' for secp256k1 ECDH"
        ) from e

    return PublicKey


def _hex_bytes(value, length, field):
    if isinstance(value, bytes):
        raw = value
    elif isinstance(value, str):
        try:
            raw = bytes.fromhex(value)
        except ValueError:
            raise Nip44Error(f"{field} must be hex") from None
    else:
        raise Nip44Error(f"{field} must be hex or bytes")

    if len(raw) != length:
        raise Nip44Error(f"{field} must be {length} bytes")

    return raw


def shared_x(private_key, peer_pubkey):
    """
    The raw 32-byte x-coordinate of the ECDH shared point.

    `coincurve`'s own `PrivateKey.ecdh()` is unusable here: it returns
    sha256 of the compressed shared point to avoid malleability, and NIP-44
    needs the unhashed x-coordinate. NIP-44 also uses **x-only** public
    keys, which have to be lifted to a full point before any EC operation.
    """

    PublicKey = _coincurve()

    private = _hex_bytes(private_key, 32, "private key")
    peer = _hex_bytes(peer_pubkey, 32, "peer public key")

    if not any(private):
        raise Nip44Error("private key must not be zero")

    try:
        point = PublicKey(b"\x02" + peer)
    except Exception as e:  # noqa: BLE001 - coincurve raises many types
        raise Nip44Error(
            "peer public key is not a valid secp256k1 x-coordinate"
        ) from e

    try:
        product = point.multiply(private)
    except Exception as e:  # noqa: BLE001
        raise Nip44Error("private key is not a valid secp256k1 scalar") from e

    compressed = product.format(compressed=True)

    return compressed[1:]


def conversation_key(private_key, peer_pubkey):
    """
    The symmetric conversation key for one pair of participants.

    HKDF-Extract only, with the fixed domain salt. The `cryptography`
    HKDF class cannot do extract-only, so this is an HMAC.
    """

    return hmac.new(
        DOMAIN_SALT,
        shared_x(private_key, peer_pubkey),
        hashlib.sha256,
    ).digest()


def message_keys(key, nonce):
    """
    The per-message ChaCha20 key and nonce and the HMAC key.

    One HKDF-Expand, with the raw 32-byte nonce as `info`. The constant
    `b"nip44-v2"` is the *salt of the extract above*, never an `info`
    string; using it here is the intermediate draft and produces payloads
    that no current implementation can decrypt.
    """

    hashes, _, _, HKDFExpand = _crypto()

    if len(key) != 32:
        raise Nip44Error("conversation key must be 32 bytes")

    nonce = _hex_bytes(nonce, 32, "nonce")

    material = HKDFExpand(
        algorithm=hashes.SHA256(),
        length=76,
        info=nonce,
    ).derive(key)

    return material[:32], material[32:44], material[44:76]


def calc_padded_len(unpadded_length):
    """
    The padded length, excluding the length prefix.

    Integer arithmetic only: lengths approach 2**32 and floats lose
    precision there. The `n <= 32` case is handled before any logarithm,
    because n = 1 would be log2(0).
    """

    n = int(unpadded_length)

    if n < 1:
        raise Nip44Error("plaintext must not be empty")

    if n <= 32:
        return 32

    next_power = 1 << ((n - 1).bit_length())

    chunk = 32 if next_power <= 256 else next_power // 8

    return chunk * (((n - 1) // chunk) + 1)


def pad(plaintext):
    """Plaintext bytes to the padded form, with its length prefix."""

    if isinstance(plaintext, str):
        plaintext = plaintext.encode("utf-8")

    if not isinstance(plaintext, (bytes, bytearray)):
        raise Nip44Error("plaintext must be str or bytes")

    plaintext = bytes(plaintext)

    n = len(plaintext)

    if n < 1:
        raise Nip44Error("plaintext must not be empty")

    if n >= 1 << 32:
        raise Nip44Error("plaintext is too long for the length prefix")

    if n < 65536:
        prefix = n.to_bytes(2, "big")
    else:
        #
        # A u16 of zero is the escape marker, which is unambiguous
        # because n >= 1.
        #
        prefix = b"\x00\x00" + n.to_bytes(4, "big")

    return prefix + plaintext + bytes(calc_padded_len(n) - n)


def unpad(padded):
    """The inverse of `pad`, with every length relationship checked."""

    if not isinstance(padded, (bytes, bytearray)):
        raise Nip44Error("padded plaintext must be bytes")

    padded = bytes(padded)

    if len(padded) < 34:
        raise Nip44Error("padded plaintext is too short")

    if padded[0] == 0 and padded[1] == 0:
        length = int.from_bytes(padded[2:6], "big")
        prefix_length = 6

        if length < 65536:
            raise Nip44Error(
                "extended length prefix used for a short plaintext"
            )
    else:
        length = int.from_bytes(padded[:2], "big")
        prefix_length = 2

    if length < 1:
        raise Nip44Error("plaintext length must not be zero")

    expected = prefix_length + calc_padded_len(length)

    if len(padded) != expected:
        raise Nip44Error(
            f"padded length {len(padded)} does not match the declared "
            f"plaintext length {length} (expected {expected})"
        )

    return padded[prefix_length:prefix_length + length]


def _chacha20(key, nonce12, data):
    _, Cipher, algorithms, _ = _crypto()

    #
    # `cryptography` wants a 16-byte nonce: a 4-byte little-endian block
    # counter followed by the RFC 8439 12-byte nonce. NIP-44 always starts
    # the counter at zero.
    #
    return Cipher(
        algorithms.ChaCha20(key, b"\x00" * 4 + nonce12),
        mode=None,
    ).encryptor().update(data)


def encrypt(plaintext, key, nonce=None):
    """
    Encrypt one UTF-8 string into a NIP-44 payload.

    `nonce` is for tests and for reproducing a vector; production callers
    must leave it unset so a fresh random nonce is used. Reusing a nonce
    under one conversation key destroys confidentiality.
    """

    if nonce is None:
        nonce = os.urandom(DEFAULT_NONCE_BYTES)

    nonce = _hex_bytes(nonce, 32, "nonce")

    chacha_key, chacha_nonce, hmac_key = message_keys(key, nonce)

    ciphertext = _chacha20(
        chacha_key,
        chacha_nonce,
        pad(plaintext),
    )

    mac = hmac.new(
        hmac_key,
        nonce + ciphertext,
        hashlib.sha256,
    ).digest()

    return base64.b64encode(
        bytes([VERSION]) + nonce + ciphertext + mac
    ).decode("ascii")


def _decode_payload(payload):
    if not isinstance(payload, str):
        raise Nip44Error("payload must be a string")

    if not payload:
        raise Nip44Error("payload is empty")

    if payload[0] == FUTURE_ENCODING_PREFIX:
        raise Nip44Error(
            "payload declares a future encoding, which this "
            "implementation does not support"
        )

    if len(payload) < MIN_BASE64_CHARS:
        raise Nip44Error("payload is too short to be a NIP-44 v2 payload")

    if len(payload) > MAX_BASE64_CHARS:
        raise Nip44Error("payload is implausibly large")

    try:
        raw = base64.b64decode(payload, validate=True)
    except (binascii.Error, ValueError) as e:
        raise Nip44Error(f"payload is not valid base64: {e}") from e

    if len(raw) < MIN_PAYLOAD_BYTES:
        raise Nip44Error("decoded payload is too short")

    if raw[0] != VERSION:
        raise Nip44Error(
            f"unsupported NIP-44 version {raw[0]}; expected {VERSION}"
        )

    return raw[1:33], raw[33:-32], raw[-32:]


def decrypt(payload, key):
    """
    Decrypt one NIP-44 payload to its UTF-8 string.

    Validation order matters: the MAC is checked before the ciphertext is
    touched, and the padding and UTF-8 are checked before the plaintext is
    returned. Every failure raises rather than returning a partial value,
    so a caller can treat "did not decrypt" as "refuse the event".
    """

    nonce, ciphertext, mac = _decode_payload(payload)

    chacha_key, chacha_nonce, hmac_key = message_keys(key, nonce)

    expected = hmac.new(
        hmac_key,
        nonce + ciphertext,
        hashlib.sha256,
    ).digest()

    if not hmac.compare_digest(expected, mac):
        raise Nip44Error("payload authentication failed")

    padded = _chacha20(chacha_key, chacha_nonce, ciphertext)

    plaintext = unpad(padded)

    try:
        return plaintext.decode("utf-8")
    except UnicodeDecodeError as e:
        raise Nip44Error("plaintext is not valid UTF-8") from e


def looks_encrypted(content):
    """
    Whether a coordination `content` string is plausibly a NIP-44 payload.

    Deterministic, and the reason a plaintext reader cannot mistake one
    for the other: a JSON object begins with `{`, which is not in the
    base64 alphabet, and a v2 payload always begins with `A` (the base64
    of the 0x02 version byte).

    This is a *candidate* test, not authentication. The MAC decides
    whether a payload is genuine, and the signed `encryption` tag records
    the sender's intent; neither is implied by this function.
    """

    if not isinstance(content, str) or not content:
        return False

    if content[0].isspace() or content[0] == "{":
        return False

    if content[0] == FUTURE_ENCODING_PREFIX:
        return False

    if len(content) < MIN_BASE64_CHARS or len(content) > MAX_BASE64_CHARS:
        return False

    try:
        raw = base64.b64decode(content, validate=True)
    except (binascii.Error, ValueError):
        return False

    return len(raw) >= MIN_PAYLOAD_BYTES and raw[0] == VERSION


def describe_payload(payload):
    """
    A short, safe description of a payload for diagnostics.

    Never includes plaintext or key material, and never raises: it is used
    on the failure path.
    """

    try:
        nonce, ciphertext, _ = _decode_payload(payload)
    except Nip44Error as e:
        return f"invalid NIP-44 payload: {e}"

    return (
        f"NIP-44 v{VERSION} payload, {len(ciphertext)} bytes of "
        f"ciphertext, nonce {nonce[:4].hex()}..."
    )
