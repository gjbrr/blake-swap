#!/usr/bin/env python3
"""
A second, independent NIP-44 v2 implementation.

`src/nip44.py` is the engine's codec, tested against the official vectors.
This tool exists to answer a different question: whether something that did
not come from that module can talk to it. It is deliberately standalone --
it imports `cryptography` and `coincurve` directly and never `nip44` -- so
a change to the engine's codec that broke interoperability would show up as
a disagreement here rather than as agreement between a module and its own
tests.

Modes
-----

    selftest
        Check this implementation against the published vectors:
        conversation keys, message keys, padded lengths and three
        plaintext/payload pairs.

    encrypt --peer-pub <hex> [--sender-priv <hex>] [--nonce <hex>] <plaintext>
    decrypt --peer-pub <hex> [--sender-priv <hex>] <payload>

        The sender key comes from --sender-priv, or from the engine's
        BLAKE_SWAP_PARTICIPANT_KEY_FILE when omitted.

    event --peer-pub <hex> --swap-id <id> --type <type> --message-json <json>
        Sign a kind-1 NIP-01 event whose content is the encrypted message
        and which carries the `["encryption", "nip44_v2"]` tag, exactly as
        the engine's own encrypted coordination events do.

Needs `coincurve` and the optional `cryptography` package.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import json
import os
import secrets
import sys
import time
from pathlib import Path


VERSION = 2
DOMAIN_SALT = b"nip44-v2"
ENCRYPTION_TAG = "nip44_v2"

MIN_PLAINTEXT_BYTES = 1
EXTENDED_PREFIX_MIN = 65536


class ToolError(ValueError):
    """Any refusal from this tool."""


# ------------------------------------------------------------ primitives


def calc_padded_len(unpadded_length):
    if unpadded_length < 1:
        raise ToolError("plaintext length must be at least 1")

    if unpadded_length <= 32:
        return 32

    next_power = 1 << (unpadded_length - 1).bit_length()
    chunk = 32 if next_power <= 256 else next_power // 8

    return chunk * ((unpadded_length - 1) // chunk + 1)


def pad(plaintext):
    if not isinstance(plaintext, bytes):
        raise ToolError("plaintext must be bytes")

    if not plaintext:
        raise ToolError("plaintext must not be empty")

    length = len(plaintext)

    if length >= EXTENDED_PREFIX_MIN:
        prefix = b"\x00\x00" + length.to_bytes(4, "big")
    else:
        prefix = length.to_bytes(2, "big")

    return prefix + plaintext + b"\x00" * (calc_padded_len(length) - length)


def unpad(padded):
    if len(padded) < 2:
        raise ToolError("padded plaintext is too short to hold a length")

    if padded[:2] == b"\x00\x00":
        if len(padded) < 6:
            raise ToolError("padded plaintext is too short for an extended prefix")

        length = int.from_bytes(padded[2:6], "big")
        start = 6
    else:
        length = int.from_bytes(padded[:2], "big")
        start = 2

    if length < 1:
        raise ToolError("padded plaintext declares a zero length")

    if start + length > len(padded):
        raise ToolError("padded plaintext declares a length past its end")

    return padded[start:start + length]


def _load_crypto():
    try:
        from cryptography.hazmat.primitives import hashes
        from cryptography.hazmat.primitives.ciphers import Cipher, algorithms
        from cryptography.hazmat.primitives.kdf.hkdf import HKDFExpand
    except ImportError as e:  # pragma: no cover - environment dependent
        raise ToolError(
            "this tool needs the 'cryptography' package; install it with "
            "`pip install -e '.[crypto]'`"
        ) from e

    return hashes, Cipher, algorithms, HKDFExpand


def _load_coincurve():
    try:
        from coincurve import PrivateKey, PublicKey
    except ImportError as e:  # pragma: no cover - declared dependency
        raise ToolError("this tool needs the 'coincurve' package") from e

    return PrivateKey, PublicKey


def sender_private_key(hex_key=None):
    PrivateKey, _ = _load_coincurve()

    if hex_key is None:
        path = os.environ.get(
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
            "",
        ).strip()

        if not path:
            raise ToolError(
                "no sender key: pass --sender-priv or set "
                "BLAKE_SWAP_PARTICIPANT_KEY_FILE"
            )

        hex_key = Path(path).expanduser().read_text().strip()

    try:
        secret = bytes.fromhex(hex_key)
    except ValueError as e:
        raise ToolError("sender key is not hex") from e

    if len(secret) != 32:
        raise ToolError("sender key must be 32 bytes of hex")

    return PrivateKey(secret)


def shared_x(private_key, peer_pubkey_hex):
    """
    The raw x-coordinate of the ECDH point.

    NIP-44 hashes this x-coordinate and nothing else. `coincurve`'s own
    `ecdh()` cannot be used for it: it returns sha256 of the compressed
    point, which is a different value and derives a conversation key no
    other implementation would agree with.
    """
    _, PublicKey = _load_coincurve()

    if not isinstance(peer_pubkey_hex, str):
        raise ToolError("peer public key must be hex")

    if len(peer_pubkey_hex) == 64:
        #
        # An x-only key: lift it to the even-y point.
        #
        compressed = b"\x02" + bytes.fromhex(peer_pubkey_hex)
    elif len(peer_pubkey_hex) == 66:
        compressed = bytes.fromhex(peer_pubkey_hex)
    else:
        raise ToolError("peer public key must be 32-byte x-only or 33-byte compressed")

    try:
        peer = PublicKey(compressed)
        product = peer.multiply(private_key.secret)
    except Exception as e:  # noqa: BLE001
        raise ToolError(f"peer public key is not usable: {e}") from e

    encoded = product.format(compressed=True)

    return encoded[1:33]


def conversation_key(private_key, peer_pubkey_hex):
    """HKDF-Extract over the shared x-coordinate, with the fixed salt."""

    return hmac.new(
        DOMAIN_SALT,
        shared_x(private_key, peer_pubkey_hex),
        hashlib.sha256,
    ).digest()


def message_keys(key, nonce):
    hashes, _, _, HKDFExpand = _load_crypto()

    if len(key) != 32:
        raise ToolError("conversation key must be 32 bytes")

    if len(nonce) != 32:
        raise ToolError("nonce must be 32 bytes")

    material = HKDFExpand(
        algorithm=hashes.SHA256(),
        length=76,
        info=nonce,
    ).derive(key)

    return material[:32], material[32:44], material[44:76]


def _chacha20(key, nonce12, data):
    _, Cipher, algorithms, _ = _load_crypto()

    #
    # `cryptography` takes a 16-byte nonce for ChaCha20 (the OpenSSL
    # layout): the 12-byte IETF nonce with a zero block counter in front.
    #
    cipher = Cipher(
        algorithms.ChaCha20(key, b"\x00" * 4 + nonce12),
        mode=None,
    )

    return cipher.encryptor().update(data)


def encrypt(plaintext, key, nonce=None):
    if not isinstance(plaintext, str):
        raise ToolError("plaintext must be a string")

    encoded = plaintext.encode("utf-8")

    if nonce is None:
        nonce = secrets.token_bytes(32)
    elif isinstance(nonce, str):
        nonce = bytes.fromhex(nonce)

    if len(nonce) != 32:
        raise ToolError("nonce must be 32 bytes")

    chacha_key, chacha_nonce, hmac_key = message_keys(key, nonce)

    ciphertext = _chacha20(chacha_key, chacha_nonce, pad(encoded))

    mac = hmac.new(
        hmac_key,
        nonce + ciphertext,
        hashlib.sha256,
    ).digest()

    return base64.b64encode(
        bytes([VERSION]) + nonce + ciphertext + mac
    ).decode("ascii")


def decrypt(payload, key):
    if not isinstance(payload, str) or not payload:
        raise ToolError("payload must be a non-empty string")

    try:
        raw = base64.b64decode(payload, validate=True)
    except Exception as e:  # noqa: BLE001
        raise ToolError(f"payload is not valid base64: {e}") from e

    if len(raw) < 99:
        raise ToolError("payload is too short to be a NIP-44 v2 payload")

    if raw[0] != VERSION:
        raise ToolError(f"unsupported NIP-44 version {raw[0]}")

    nonce = raw[1:33]
    ciphertext = raw[33:-32]
    mac = raw[-32:]

    chacha_key, chacha_nonce, hmac_key = message_keys(key, nonce)

    expected = hmac.new(
        hmac_key,
        nonce + ciphertext,
        hashlib.sha256,
    ).digest()

    if not hmac.compare_digest(expected, mac):
        raise ToolError("payload authentication failed")

    try:
        return unpad(_chacha20(chacha_key, chacha_nonce, ciphertext)).decode(
            "utf-8"
        )
    except UnicodeDecodeError as e:
        raise ToolError("plaintext is not valid UTF-8") from e


# ------------------------------------------------------------- the event


def event_id(event):
    serialized = json.dumps(
        [
            0,
            event["pubkey"],
            event["created_at"],
            event["kind"],
            event["tags"],
            event["content"],
        ],
        separators=(",", ":"),
        ensure_ascii=False,
    )

    return hashlib.sha256(serialized.encode("utf-8")).digest()


def build_event(*, sender_priv, peer_pub, swap_id, message_type, message):
    PrivateKey, _ = _load_coincurve()

    private_key = PrivateKey(bytes.fromhex(sender_priv))

    content = json.dumps(
        message,
        separators=(",", ":"),
        sort_keys=True,
    )

    key = conversation_key(private_key, peer_pub)

    event = {
        "pubkey": private_key.public_key.format(compressed=True)[1:].hex(),
        "created_at": int(time.time()),
        "kind": 1,
        "tags": [
            ["t", "blake-swap"],
            ["d", swap_id],
            ["type", message_type],
            ["encryption", ENCRYPTION_TAG],
        ],
        "content": encrypt(content, key),
    }

    digest = event_id(event)

    event["id"] = digest.hex()
    event["sig"] = private_key.sign_schnorr(digest).hex()

    return event


# --------------------------------------------------------------- vectors

CONVERSATION_KEY_VECTORS = [
    (
        "315e59ff51cb9209768cf7da80791ddcaae56ac9775eb25b6dee1234bc5d2268",
        "c2f9d9948dc8c7c38321e4b85c8558872eafa0641cd269db76848a6073e69133",
        "3dfef0ce2a4d80a25e7a328accf73448ef67096f65f79588e358d9a0eb9013f1",
    ),
    (
        "0000000000000000000000000000000000000000000000000000000000000001",
        "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
        "3b4610cb7189beb9cc29eb3716ecc6102f1247e8f3101a03a1787d8908aeb54e",
    ),
]

MESSAGE_KEY_VECTOR = {
    "conversation_key":
        "a1a3d60f3470a8612633924e91febf96dc5366ce130f658b1f0fc652c20b3b54",
    "nonce": "e1e6f880560d6d149ed83dcc7e5861ee62a5ee051f7fde9975fe5d25d2a02d72",
    "chacha_key": "f145f3bed47cb70dbeaac07f3a3fe683e822b3715edb7c4fe310829014ce7d76",
    "chacha_nonce": "c4ad129bb01180c0933a160c",
    "hmac_key": "027c1db445f05e2eee864a0975b0ddef5b7110583c8c192de3732571ca5838c4",
}

PADDED_LENGTH_VECTORS = [
    (16, 32), (32, 32), (33, 64), (37, 64), (65, 96), (100, 128), (200, 224),
    (256, 256), (320, 320), (512, 512), (515, 640), (1020, 1024),
    (65536, 65536),
]

ENCRYPT_DECRYPT_VECTORS = [
    (
        "c41c775356fd92eadc63ff5a0dc1da211b268cbea22316767095b2871ea1412d",
        "00" * 31 + "01",
        "a",
        "AgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABee0G5VSK0/9YypIObAtDKfYEAjD35uVkHyB0F4DwrcNaCXlCWZKaArsGrY6M9wnuTMxWfp1RTN9Xga8no+kF5Vsb",
    ),
    (
        "c41c775356fd92eadc63ff5a0dc1da211b268cbea22316767095b2871ea1412d",
        "f00000000000000000000000000000f00000000000000000000000000000000f",
        "🍕🫃",
        "AvAAAAAAAAAAAAAAAAAAAPAAAAAAAAAAAAAAAAAAAAAPSKSK6is9ngkX2+cSq85Th16oRTISAOfhStnixqZziKMDvB0QQzgFZdjLTPicCJaV8nDITO+QfaQ61+KbWQIOO2Yj",
    ),
]


def selftest():
    failures = []

    PrivateKey, _ = _load_coincurve()

    for secret_hex, peer_x, expected in CONVERSATION_KEY_VECTORS:
        actual = conversation_key(
            PrivateKey(bytes.fromhex(secret_hex)),
            peer_x,
        ).hex()

        if actual != expected:
            failures.append(
                f"conversation_key({secret_hex[:8]}…) is {actual}, "
                f"expected {expected}"
            )

    key = bytes.fromhex(MESSAGE_KEY_VECTOR["conversation_key"])
    nonce = bytes.fromhex(MESSAGE_KEY_VECTOR["nonce"])

    actual_key, actual_nonce, actual_mac = message_keys(key, nonce)

    for name, actual, expected in (
        ("chacha_key", actual_key.hex(), MESSAGE_KEY_VECTOR["chacha_key"]),
        ("chacha_nonce", actual_nonce.hex(), MESSAGE_KEY_VECTOR["chacha_nonce"]),
        ("hmac_key", actual_mac.hex(), MESSAGE_KEY_VECTOR["hmac_key"]),
    ):
        if actual != expected:
            failures.append(f"message {name} is {actual}, expected {expected}")

    for unpadded, expected in PADDED_LENGTH_VECTORS:
        actual = calc_padded_len(unpadded)

        if actual != expected:
            failures.append(
                f"calc_padded_len({unpadded}) is {actual}, expected {expected}"
            )

    for key_hex, nonce_hex, plaintext, payload in ENCRYPT_DECRYPT_VECTORS:
        key = bytes.fromhex(key_hex)

        actual = encrypt(plaintext, key, nonce_hex)

        if actual != payload:
            failures.append(
                f"encrypt({plaintext!r}) did not match the vector:\n"
                f"  got      {actual}\n  expected {payload}"
            )
            continue

        if decrypt(payload, key) != plaintext:
            failures.append(f"decrypt did not round-trip {plaintext!r}")

    if failures:
        for failure in failures:
            print(f"FAIL  {failure}", file=sys.stderr)

        return 1

    print(
        "PASS: the independent NIP-44 implementation matches "
        f"{len(CONVERSATION_KEY_VECTORS)} conversation keys, the message keys, "
        f"{len(PADDED_LENGTH_VECTORS)} padded lengths and "
        f"{len(ENCRYPT_DECRYPT_VECTORS)} payloads"
    )

    return 0


# ------------------------------------------------------------------ main


def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[1])

    sub = p.add_subparsers(dest="mode", required=True)

    sub.add_parser("selftest", help="check this implementation against the vectors")

    enc = sub.add_parser("encrypt", help="encrypt one UTF-8 string")
    enc.add_argument("plaintext")
    enc.add_argument("--peer-pub", required=True)
    enc.add_argument("--sender-priv", default=None)
    enc.add_argument("--nonce", default=None)

    dec = sub.add_parser("decrypt", help="decrypt one payload")
    dec.add_argument("payload")
    dec.add_argument("--peer-pub", required=True)
    dec.add_argument("--sender-priv", default=None)

    ev = sub.add_parser("event", help="sign an encrypted coordination event")
    ev.add_argument("--peer-pub", required=True)
    ev.add_argument("--sender-priv", default=None)
    ev.add_argument("--swap-id", required=True)
    ev.add_argument("--type", required=True)
    ev.add_argument("--message-json", required=True)

    args = p.parse_args()

    if args.mode == "selftest":
        return selftest()

    if args.mode == "event":
        private_key = sender_private_key(args.sender_priv)

        event = build_event(
            sender_priv=private_key.secret.hex(),
            peer_pub=args.peer_pub,
            swap_id=args.swap_id,
            message_type=args.type,
            message=json.loads(args.message_json),
        )

        print(json.dumps(event, indent=2, sort_keys=True))

        return 0

    private_key = sender_private_key(args.sender_priv)

    key = conversation_key(private_key, args.peer_pub)

    if args.mode == "encrypt":
        print(encrypt(args.plaintext, key, args.nonce))
        return 0

    print(decrypt(args.payload, key))

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
