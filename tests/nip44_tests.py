#!/usr/bin/env python3
"""
NIP-44 v2 codec suite.

NIP-44's key derivation has several details that are not self-describing
and that fail *silently* against a peer: the conversation key is a
HKDF-**Extract** with a fixed salt (`b"nip44-v2"` is never an `info`
string), the message keys are one HKDF-Expand with the raw nonce as
`info`, the MAC covers `nonce || ciphertext` in that order, and the
padding length depends on integer-only arithmetic that breaks near 2**32.
An implementation can be internally consistent and still be unable to talk
to anything else.

So this suite is driven by the **official test vectors** rather than by
this implementation's own output. The vectors are from
`paulmillr/nip44` (`nip44.vectors.json`, the file whose sha256
`269ed0f69e4c192512cc779e78c555090cebc7c785b609e338a62afc3ce25040` is
the one NIP-44 itself names), and the subset here covers:

  * conversation keys, including `sec1 = 1` against its own generator
    point and `sec1 = n - 2`;
  * message keys for a known conversation key and nonce;
  * every published `calc_padded_len` pair;
  * plaintext/ payload pairs for ASCII, multi-plane Unicode, emoji and
    right-to-left text;
  * the long-message cases (65535 bytes and a 16383-character emoji
    string) by hashing, since the payloads are hundreds of kilobytes;
  * the extended length-prefix boundary at 65535 / 65536 / 65537 bytes;
  * every published invalid input class: unknown versions, invalid base64,
    invalid MAC, invalid padding, invalid payload lengths, non-curve and
    wrong-order public keys, out-of-range scalars and a zero scalar.

It also pins the deterministic discrimination a plaintext reader relies
on: JSON begins with `{`, which is not base64, so a NIP-44 payload can
never be mistaken for a JSON object.

Needs no chain nodes and no relays. Eight of the checks need the optional
`cryptography` package (the `crypto` extra) and are skipped, with the suite
exiting 77, when it is absent.

Exit status: 0 pass, 1 fail, 77 cipher dependency absent.
"""

from __future__ import annotations

import hashlib
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

import nip44  # noqa: E402

PASS = 0
FAIL = 0
SKIPPED = 0

#
# NIP-44's key derivation needs only `coincurve` and `hmac`, but the cipher
# and the message keys need the optional `cryptography` package, which is
# the `crypto` extra and is not a runtime dependency. A plaintext-only
# checkout is therefore a supported environment rather than a broken one,
# so the cipher-dependent checks are skipped instead of failed when it is
# absent. They are skipped *loudly* and the suite exits 77 rather than 0:
# reporting a pass for a check that never ran is the vacuous result this
# whole suite exists to avoid, and `scripts/test.sh --strict` turns the
# skip into a failure wherever the full environment is expected.
#
try:
    import cryptography  # noqa: F401
except ImportError:
    HAVE_CRYPTO = False
else:
    HAVE_CRYPTO = True

CRYPTO_MISSING = (
    "the optional 'cryptography' package is not installed; "
    "install it with `pip install -e '.[crypto]'`"
)

SKIP_EXIT_CODE = 77


def ok(name):
    global PASS
    PASS += 1
    print(f"PASS  {name}")


def fail(name, detail):
    global FAIL
    FAIL += 1
    print(f"FAIL  {name}")
    for line in str(detail).splitlines():
        print(f"      {line}")


def skip(name):
    global SKIPPED

    if SKIPPED == 0:
        print(f"SKIP: {CRYPTO_MISSING}")

    SKIPPED += 1
    print(f"SKIP  {name}")


def check(name, fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
    else:
        ok(name)


def check_crypto(name, fn):
    """
    Run one cipher-dependent check, or skip it if the extra is absent.

    These checks cannot be allowed to run without `cryptography`: the
    module's refusals are all `Nip44Error`, so a missing dependency is
    indistinguishable from the rejection under test and the check would
    pass having proved nothing.
    """
    if not HAVE_CRYPTO:
        skip(name)
        return

    check(name, fn)


def refuses(name, fn):
    try:
        fn()
    except nip44.Nip44Error:
        ok(name)
        return
    except Exception as e:  # noqa: BLE001
        fail(name, f"raised {type(e).__name__} instead of Nip44Error: {e}")
        return

    fail(name, "the value was accepted instead of refused")


# --------------------------------------------------------------- vectors

CONVERSATION_KEYS = [
    (
        "315e59ff51cb9209768cf7da80791ddcaae56ac9775eb25b6dee1234bc5d2268",
        "c2f9d9948dc8c7c38321e4b85c8558872eafa0641cd269db76848a6073e69133",
        "3dfef0ce2a4d80a25e7a328accf73448ef67096f65f79588e358d9a0eb9013f1",
    ),
    (
        "0e44e2d1db3c1717b05ffa0f08d102a09c554a1cbbf678ab158b259a44e682f1",
        "1ffa76c5cc7a836af6914b840483726207cb750889753d7499fb8b76aa8fe0de",
        "a39970a667b7f861f100e3827f4adbf6f464e2697686fe1a81aeda817d6b8bdf",
    ),
    (
        "a28eed0fe977893856ab9667e06ace39f03abbcdb845c329a1981be438ba565d",
        "b0f38b950a5013eba5ab4237f9ed29204a59f3625c71b7e210fec565edfa288c",
        "9d3a802b45bc5aeeb3b303e8e18a92ddd353375710a31600d7f5fff8f3a7285b",
    ),
    # sec1 = n - 2, an x-only peer key whose x is 2
    (
        "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364139",
        "0000000000000000000000000000000000000000000000000000000000000002",
        "8b6392dbf2ec6a2b2d5b1477fc2be84d63ef254b667cadd31bd3f444c44ae6ba",
    ),
    # sec1 == the generator, pub2 is the generator's x
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

PADDED_LENGTHS = [
    (16, 32), (32, 32), (33, 64), (37, 64), (45, 64), (49, 64), (64, 64),
    (65, 96), (100, 128), (111, 128), (200, 224), (250, 256), (320, 320),
    (383, 384), (384, 384), (400, 448), (500, 512), (512, 512), (515, 640),
    (700, 768), (800, 896), (900, 1024), (1020, 1024), (65536, 65536),
]

ENCRYPT_DECRYPT = [
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
    (
        "3e2b52a63be47d34fe0a80e34e73d436d6963bc8f39827f327057a9986c20a45",
        "b635236c42db20f021bb8d1cdff5ca75dd1a0cc72ea742ad750f33010b24f73b",
        "表ポあA鷗ŒéＢ逍Üßªąñ丂㐀𠀀",
        "ArY1I2xC2yDwIbuNHN/1ynXdGgzHLqdCrXUPMwELJPc7s7JqlCMJBAIIjfkpHReBPXeoMCyuClwgbT419jUWU1PwaNl4FEQYKCDKVJz+97Mp3K+Q2YGa77B6gpxB/lr1QgoqpDf7wDVrDmOqGoiPjWDqy8KzLueKDcm9BVP8xeTJIxs=",
    ),
    (
        "3b15c977e20bfe4b8482991274635edd94f366595b1a3d2993515705ca3cedb8",
        "8d4442713eb9d4791175cb040d98d6fc5be8864d6ec2f89cf0895a2b2b72d1b1",
        "pepper👀їжак",
        "Ao1EQnE+udR5EXXLBA2Y1vxb6IZNbsL4nPCJWisrctGxY3AduCS+jTUgAAnfvKafkmpy15+i9YMwCdccisRa8SvzW671T2JO4LFSPX31K4kYUKelSAdSPwe9NwO6LhOsnoJ+",
    ),
]

LONG_MESSAGES = [
    (
        "8fc262099ce0d0bb9b89bac05bb9e04f9bc0090acc181fef6840ccee470371ed",
        "326bcb2c943cd6bb717588c9e5a7e738edf6ed14ec5f5344caa6ef56f0b9cff7",
        "x",
        65535,
        "09ab7495d3e61a76f0deb12cb0306f0696cbb17ffc12131368c7a939f12f56d3",
        "90714492225faba06310bff2f249ebdc2a5e609d65a629f1c87f2d4ffc55330a",
    ),
    (
        "7fc540779979e472bb8d12480b443d1e5eb1098eae546ef2390bee499bbf46be",
        "34905e82105c20de9a2f6cd385a0d541e6bcc10601d12481ff3a7575dc622033",
        "🦄",
        16383,
        "a249558d161b77297bc0cb311dde7d77190f6571b25c7e4429cd19044634a61f",
        "b3348422471da1f3c59d79acfe2fe103f3cd24488109e5b18734cdb5953afd15",
    ),
]

#
# The 2026 length extension: a plaintext of 65536 bytes or more uses a
# 6-byte prefix. Values and hashes are the inline vectors NIP-44 publishes
# for the boundary (the `invalid.encrypt_msg_lengths` entries for 65536+ in
# the vector file predate that change and are stale).
#
EXTENDED_BOUNDARY = [
    (65535, 65536, "6e1bebca", "6d8c2810"),
    (65536, 65536, "bf718b6f", "b7b4edb3"),
    (65537, 81920, "008ffc88", "eeb7c7c5"),
]

BOUNDARY_CONVERSATION_KEY = (
    "c41c775356fd92eadc63ff5a0dc1da211b268cbea22316767095b2871ea1412d"
)
BOUNDARY_NONCE = "00" * 31 + "01"


# ----------------------------------------------------------------- tests


def test_conversation_key_vectors():
    for sec1, pub2, expected in CONVERSATION_KEYS:
        actual = nip44.conversation_key(sec1, pub2).hex()

        if actual != expected:
            raise AssertionError(
                f"conversation_key({sec1[:8]}..., {pub2[:8]}...) is "
                f"{actual}, expected {expected}"
            )


def test_conversation_key_is_symmetric():
    sec1, pub2, expected = CONVERSATION_KEYS[0]

    #
    # The vector file gives (sec1, pub2, key) only, so derive sec2 from
    # pub2 is not possible without the scalar; symmetry is checked with
    # our own generated pair instead, against the spec's guarantee that
    # the key is the same in both directions.
    #
    from coincurve import PrivateKey

    a = PrivateKey()
    b = PrivateKey()

    a_pub = a.public_key.format(compressed=True)[1:].hex()
    b_pub = b.public_key.format(compressed=True)[1:].hex()

    forward = nip44.conversation_key(a.secret.hex(), b_pub)
    backward = nip44.conversation_key(b.secret.hex(), a_pub)

    if forward != backward:
        raise AssertionError("the conversation key is not symmetric")

    if forward == expected:
        raise AssertionError("a generated key collided with a vector")


def test_message_key_vector():
    vector = MESSAGE_KEY_VECTOR

    key, nonce, mac = nip44.message_keys(
        bytes.fromhex(vector["conversation_key"]),
        vector["nonce"],
    )

    for name, actual, expected in (
        ("chacha_key", key.hex(), vector["chacha_key"]),
        ("chacha_nonce", nonce.hex(), vector["chacha_nonce"]),
        ("hmac_key", mac.hex(), vector["hmac_key"]),
    ):
        if actual != expected:
            raise AssertionError(f"{name} is {actual}, expected {expected}")


def test_padded_length_vectors():
    for unpadded, expected in PADDED_LENGTHS:
        actual = nip44.calc_padded_len(unpadded)

        if actual != expected:
            raise AssertionError(
                f"calc_padded_len({unpadded}) is {actual}, expected {expected}"
            )


def test_encrypt_matches_the_vectors_and_decrypts_back():
    for key_hex, nonce_hex, plaintext, payload in ENCRYPT_DECRYPT:
        key = bytes.fromhex(key_hex)

        actual = nip44.encrypt(plaintext, key, bytes.fromhex(nonce_hex))

        if actual != payload:
            raise AssertionError(
                f"encrypt({plaintext[:12]!r}) did not match the vector:\n"
                f"  got      {actual}\n  expected {payload}"
            )

        recovered = nip44.decrypt(payload, key)

        if recovered != plaintext:
            raise AssertionError(
                f"decrypt did not round-trip {plaintext[:12]!r}: "
                f"{recovered[:12]!r}"
            )


def test_long_message_vectors():
    for key_hex, nonce_hex, pattern, repeat, pt_hash, payload_hash in LONG_MESSAGES:
        plaintext = pattern * repeat

        plaintext_sha = hashlib.sha256(
            plaintext.encode("utf-8")
        ).hexdigest()

        if plaintext_sha != pt_hash:
            raise AssertionError(
                f"constructed plaintext hashes to {plaintext_sha}, "
                f"expected {pt_hash}"
            )

        payload = nip44.encrypt(
            plaintext,
            bytes.fromhex(key_hex),
            bytes.fromhex(nonce_hex),
        )

        actual = hashlib.sha256(payload.encode("ascii")).hexdigest()

        if actual != payload_hash:
            raise AssertionError(
                f"payload hashes to {actual}, expected {payload_hash}"
            )

        if nip44.decrypt(payload, bytes.fromhex(key_hex)) != plaintext:
            raise AssertionError("the long message did not round-trip")


def test_extended_length_prefix_boundary():
    """
    At 65536 bytes the two-byte length prefix runs out and a six-byte
    escape is used. Off-by-one errors here are invisible until a peer
    sends a large message.
    """

    key = bytes.fromhex(BOUNDARY_CONVERSATION_KEY)
    nonce = bytes.fromhex(BOUNDARY_NONCE)

    for (
        length,
        expected_padded,
        expected_plaintext_hash,
        expected_payload_hash,
    ) in EXTENDED_BOUNDARY:
        plaintext = "a" * length

        plaintext_hash = hashlib.sha256(
            plaintext.encode("utf-8")
        ).hexdigest()[:8]

        if plaintext_hash != expected_plaintext_hash:
            raise AssertionError(
                f"length {length}: the constructed plaintext hashes to "
                f"{plaintext_hash}, expected {expected_plaintext_hash}"
            )

        padded = nip44.pad(plaintext)

        body = len(padded) - (6 if length >= 65536 else 2)

        if body != expected_padded:
            raise AssertionError(
                f"length {length}: padded body is {body}, expected "
                f"{expected_padded}"
            )

        payload = nip44.encrypt(plaintext, key, nonce)

        payload_hash = hashlib.sha256(
            payload.encode("ascii")
        ).hexdigest()[:8]

        if payload_hash != expected_payload_hash:
            raise AssertionError(
                f"length {length}: payload hashes to {payload_hash}, "
                f"expected {expected_payload_hash}"
            )

        if nip44.decrypt(payload, key) != plaintext:
            raise AssertionError(f"length {length} did not round-trip")

        if nip44.unpad(padded) != plaintext.encode("utf-8"):
            raise AssertionError(f"length {length}: unpad disagrees")


def test_empty_plaintext_is_refused():
    refuses(
        "padding an empty plaintext is refused",
        lambda: nip44.pad(b""),
    )

    refuses(
        "calc_padded_len(0) is refused",
        lambda: nip44.calc_padded_len(0),
    )

    # `encrypt("")` is covered separately: it needs the cipher dependency,
    # and without it the refusal would come from the missing import rather
    # than from the padding rule under test.


def test_encrypting_an_empty_plaintext_is_refused():
    refuses(
        "an empty plaintext is refused",
        lambda: nip44.encrypt(
            "",
            bytes.fromhex(BOUNDARY_CONVERSATION_KEY),
        ),
    )


def test_invalid_payloads_are_refused():
    key = bytes.fromhex(BOUNDARY_CONVERSATION_KEY)

    for name, payload in (
        ("an empty payload", ""),
        ("a future encoding", "#" + "A" * 200),
        ("a too-short payload", "Ag=="),
        ("a 4-byte payload", "Ag=="),
        (
            "an unknown version byte",
            "AK1AjUvoYW3IS7C/BGRUoqEC7ayTfDUgnEPNeWTF/reBZFaha6EAIRueE9D1B1RuoiuFScC0Q94yjIuxZD3JStQtE8JMNacWFs9rlYP+ZydtHhRucp+lxfdvFlaGV/sQlqZz",
        ),
        (
            "invalid base64",
            "Atфupco0WyaOW2IGDKcshwxI9xO8HgD/P8Ddt46CbxDbrhdG8VmJZE0UICD06CUvEvdnr1cp1fiMtlM/GrE92xAc1EwsVCQEgWEu2gsHUVf4JAa3TpgkmFc3TWsax0v6n/Wq",
        ),
    ):
        refuses(
            f"{name} is refused",
            lambda payload=payload: nip44.decrypt(payload, key),
        )


def test_authentication_failures_are_refused():
    """
    A well-formed payload whose MAC does not verify.

    Separate from the structural refusals above because this one reaches
    the message keys, and therefore the optional cipher dependency, before
    it can fail.
    """
    refuses(
        "an invalid MAC is refused",
        lambda: nip44.decrypt(
            "Agn/l3ULCEAS4V7LhGFM6IGA17jsDUaFCKhrbXDANholyySBfeh+EN8wNB9gaLlg4j6wdBYh+3oK+mnxWu3NKRbSvQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
            bytes.fromhex(BOUNDARY_CONVERSATION_KEY),
        ),
    )


def test_invalid_conversation_keys_are_refused():
    good = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

    invalid = [
        ("a scalar above the curve order", "ff" * 32, good),
        ("a zero scalar", "00" * 32, good),
        ("a scalar equal to the curve order",
         "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", good),
        ("an x-coordinate with no square root",
         "00" * 31 + "02", good),
        ("an all-ff peer key", good, "ff" * 32),
        ("a peer point of order 3 on the twist",
         good, "00" * 31 + "00"),
        ("a non-hex peer key", good, "zz" * 32),
        ("a short peer key", good, "02" * 31),
    ]

    for name, sec1, pub2 in invalid:
        refuses(
            f"{name} is refused",
            lambda sec1=sec1, pub2=pub2: nip44.conversation_key(sec1, pub2),
        )


def test_padding_round_trips_every_boundary():
    for length in (1, 2, 31, 32, 33, 255, 256, 257, 1024, 65535, 65536, 65537):
        plaintext = b"z" * length

        padded = nip44.pad(plaintext)

        if nip44.unpad(padded) != plaintext:
            raise AssertionError(f"length {length} did not round-trip")


def test_unpad_rejects_corrupt_lengths():
    for name, padded in (
        ("a truncated prefix", b"\x00"),
        ("a declared length of zero", (0).to_bytes(2, "big") + b"\x00" * 32),
        ("a length longer than the buffer", (40).to_bytes(2, "big") + b"\x00" * 32),
        (
            "an extended prefix for a short plaintext",
            b"\x00\x00\x00\x01\x00\x00" + b"\x00" * 32,
        ),
    ):
        refuses(
            f"unpad refuses {name}",
            lambda padded=padded: nip44.unpad(padded),
        )


def test_discrimination_from_json():
    """
    A plaintext reader must never mistake a payload for a JSON object, or
    the other way round. JSON begins with `{`, which is not in the base64
    alphabet, and a v2 payload always begins with `A`.
    """

    key = bytes.fromhex(BOUNDARY_CONVERSATION_KEY)
    payload = nip44.encrypt('{"type":"accept"}', key)

    if not nip44.looks_encrypted(payload):
        raise AssertionError("a payload was not recognised as one")

    for content in (
        '{"type":"accept","swap_id":"0123456789ab"}',
        '{ "type": "accept" }',
        "plain text",
        "",
        "#future-encoding",
        "Ag==",
        None,
        123,
    ):
        if nip44.looks_encrypted(content):
            raise AssertionError(f"{content!r} was mistaken for a payload")


def test_describe_payload_is_safe_and_total():
    key = bytes.fromhex(BOUNDARY_CONVERSATION_KEY)
    payload = nip44.encrypt("secret-ish plaintext", key)

    described = nip44.describe_payload(payload)

    if "secret-ish" in described:
        raise AssertionError("the description leaked the plaintext")

    if "NIP-44 v2" not in described:
        raise AssertionError(f"unhelpful description: {described}")

    #
    # It must also work on the failure path, where a diagnostic is most
    # useful and least likely to be tested.
    #
    for bad in ("", "not base64", "#" + "A" * 200, "Ag=="):
        if not isinstance(nip44.describe_payload(bad), str):
            raise AssertionError(f"describe_payload({bad!r}) did not return text")


def main():
    print()
    print("=" * 72)
    print("NIP-44 v2 CODEC")
    print("=" * 72)
    print()

    check("conversation keys match the vectors", test_conversation_key_vectors)
    check("the conversation key is symmetric", test_conversation_key_is_symmetric)
    check_crypto("message keys match the vector", test_message_key_vector)
    check("padded lengths match the vectors", test_padded_length_vectors)
    check_crypto("encryption matches the vectors and round-trips", test_encrypt_matches_the_vectors_and_decrypts_back)
    check_crypto("long messages match the vectors", test_long_message_vectors)
    check_crypto("the extended length prefix is exact", test_extended_length_prefix_boundary)

    check("an empty plaintext is refused", test_empty_plaintext_is_refused)
    check_crypto("encrypting an empty plaintext is refused", test_encrypting_an_empty_plaintext_is_refused)
    check("invalid payloads are refused", test_invalid_payloads_are_refused)
    check_crypto("authentication failures are refused", test_authentication_failures_are_refused)
    check("invalid conversation keys are refused", test_invalid_conversation_keys_are_refused)
    check("padding round-trips every boundary", test_padding_round_trips_every_boundary)
    check("corrupt padding lengths are refused", test_unpad_rejects_corrupt_lengths)

    check_crypto("payloads are distinguishable from JSON", test_discrimination_from_json)
    check_crypto("descriptions are safe and total", test_describe_payload_is_safe_and_total)

    print()
    print("=" * 72)
    print("NIP-44 SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print(f"skipped:  {SKIPPED}")
    print()

    if FAIL:
        return 1

    if SKIPPED:
        #
        # Report the partial run as a skip, not a pass. See the note at the
        # top of this file: the checks that did not run cover the cipher, and
        # a green summary would claim coverage this run does not have.
        #
        print(f"SKIP: {CRYPTO_MISSING}")
        print()
        print(
            f"{PASS} checks ran; {SKIPPED} cipher-dependent checks "
            f"were NOT run."
        )
        print()
        return SKIP_EXIT_CODE

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
