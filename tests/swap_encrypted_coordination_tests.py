#!/usr/bin/env python3
"""
Encrypted coordination (NIP-44) suite.

Coordination content is plaintext by default, and the protocol does not
depend on confidentiality: S never travels, and every fact a message
carries is re-verified against a chain. With
`BLAKE_SWAP_NOSTR_ENCRYPTION=nip44_v2` on both participants, settlement
content is encrypted as well, so the metadata is shielded from relays and
from anyone reading them.

This suite drives a real offer/accept handshake over the in-process relay
stub and asserts:

  * the encrypted mode works end to end, with a signed
    `["encryption","nip44_v2"]` tag on the encrypted messages;
  * the ciphertext carries no plaintext fields — the raw event content is
    a base64 payload, and none of the message's JSON is visible in it;
  * the `offer` stays plaintext in both directions, because it is first
    contact: the maker does not yet know the taker's identity, and NIP-44
    is pairwise by construction; refusing it would make the protocol
    unusable rather than safer;
  * **an encrypted participant refuses a plaintext message after first
    contact** — a downgrade attempt;
  * **a plaintext participant refuses an encrypted message** rather than
    guessing at content it cannot read;
  * an event whose content looks like a payload but carries no
    `encryption` tag is refused either way.

Needs the two-chain regtest rig (the offer path resolves HTLC keys from a
wallet) and the relay stub. No public relay is contacted.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import contextlib
import io
import json
import sys
import tempfile
from pathlib import Path


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

sys.path.insert(0, str(ROOT / "src"))
sys.path.insert(0, str(HERE))

from nostr_relay_stub import NostrRelayStub  # noqa: E402

import swap_protocol_tests as h  # noqa: E402

PASS = 0
FAIL = 0


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


def check(name, fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
    else:
        ok(name)


# ---------------------------------------------------------------- setup

STUB = NostrRelayStub()
RELAY_URL = STUB.start()

BASE = Path(tempfile.mkdtemp(prefix="blake-encrypted-"))

PROFILES = {
    "alice": {
        "state": BASE / "alice",
        "key": BASE / "alice" / "participant.key",
        "wallet": h.ALICE_WALLET,
        "secret": "41" * 32,
    },
    "bob": {
        "state": BASE / "bob",
        "key": BASE / "bob" / "participant.key",
        "wallet": h.BOB_WALLET,
        "secret": "42" * 32,
    },
}

for profile in PROFILES.values():
    profile["state"].mkdir(parents=True, exist_ok=True)
    profile["key"].write_text(profile["secret"] + "\n")
    profile["key"].chmod(0o600)


def env_for(role, encryption):
    profile = PROFILES[role]

    return {
        "BLAKE_SWAP_TRANSPORT": "nostr",
        "BLAKE_SWAP_NOSTR_RELAYS": RELAY_URL,
        "BLAKE_SWAP_ROLE": role,
        "BLAKE_SWAP_STATE_DIR": str(profile["state"]),
        "BLAKE_SWAP_MESSAGE_DIR": str(profile["state"] / "messages"),
        "BLAKE_SWAP_CHAIN_WALLET": profile["wallet"],
        "BLAKE_SWAP_PARTICIPANT_KEY_FILE": str(profile["key"]),
        "BLAKE_SWAP_NOSTR_ENCRYPTION": encryption,
    }


def run(role, *args, encryption="nip44_v2", expect_ok=True):
    return h.run(
        [sys.executable, h.SWAP_V2, *map(str, args)],
        env=env_for(role, encryption),
        expect_ok=expect_ok,
    )


def load_state(role, swap_id):
    path = PROFILES[role]["state"] / f"{swap_id}.json"

    if not path.exists():
        raise AssertionError(f"no {role} state for {swap_id}")

    return json.loads(path.read_text())


def quiet(role, *args, encryption="nip44_v2"):
    with contextlib.redirect_stdout(io.StringIO()):
        return run(role, *args, encryption=encryption)


def settlement_events():
    """Kind-1 swap coordination events, newest last."""

    return sorted(
        (
            event
            for event in STUB.events.values()
            if any(
                isinstance(tag, list)
                and len(tag) >= 2
                and tag[0] == "t"
                and tag[1] == "blake-swap"
                for tag in event.get("tags", [])
            )
        ),
        key=lambda event: (int(event.get("created_at", 0)), event.get("id", "")),
    )


def event_type(event):
    for tag in event.get("tags", []):
        if isinstance(tag, list) and len(tag) >= 2 and tag[0] == "type":
            return tag[1]

    return None


def encryption_tag(event):
    for tag in event.get("tags", []):
        if isinstance(tag, list) and len(tag) >= 2 and tag[0] == "encryption":
            return tag[1]

    return None


def newest_of(type_name):
    for event in reversed(settlement_events()):
        if event_type(event) == type_name:
            return event

    raise AssertionError(f"no {type_name} event was published")


def publish_offer():
    #
    # Derived timeouts (no explicit heights): the advertised v1.5
    # windows are computed from the current chain tips, which is what a
    # published offer must carry.
    #
    quiet(
        "alice",
        "new",
        "--blake", "0.2",
        "--sats", "30000",
    )

    swap_id = sorted(
        path.stem
        for path in PROFILES["alice"]["state"].glob("*.json")
        if len(path.stem) == 12
    )[-1]

    quiet("alice", "publish-offer", swap_id)

    return swap_id


# ----------------------------------------------------------------- tests

SWAP_ID = None


def test_encrypted_handshake_works():
    """
    A real offer/accept over the stub with encryption enabled on both
    sides. This is the whole point: it has to interoperate.
    """

    global SWAP_ID

    SWAP_ID = publish_offer()

    #
    # Author pinning is mandatory for a Nostr acceptance (B8), so Bob
    # names Alice explicitly. The author is read from the offer event
    # rather than from her key file, so the test also proves the pinned
    # identity is the one that actually signed.
    #
    alice_pubkey = newest_of("offer")["pubkey"]

    run(
        "bob",
        "accept-offer",
        SWAP_ID,
        "--expected-alice-pub",
        alice_pubkey,
    )

    bob = load_state("bob", SWAP_ID)

    if bob["state"] != "OFFERED":
        raise AssertionError(f"Bob is in {bob['state']}, expected OFFERED")

    if not (bob.get("blake_bob_pub") and bob.get("btc_bob_pub")):
        raise AssertionError("Bob published no settlement keys")

    #
    # And Alice decrypts Bob's encrypted acceptance. The payload has to
    # bind Bob's identity, so this proves the decrypted message is the
    # real one rather than merely parseable.
    #
    run("alice", "receive-accept", SWAP_ID)

    alice = load_state("alice", SWAP_ID)

    if not (alice.get("blake_bob_pub") and alice.get("btc_bob_pub")):
        raise AssertionError("Alice did not import Bob's acceptance")

    if alice.get("nostr_bob_pub") != newest_of("accept")["pubkey"]:
        raise AssertionError(
            "Alice pinned an identity that did not sign the acceptance"
        )


def test_the_offer_stays_plaintext():
    """
    First contact cannot be encrypted: the maker does not know the taker
    yet. It must remain readable, or the two could never meet.
    """

    event = newest_of("offer")

    if encryption_tag(event) is not None:
        raise AssertionError("the offer declared encryption")

    try:
        message = json.loads(event["content"])
    except Exception as e:  # noqa: BLE001
        raise AssertionError(f"the offer content is not JSON: {e}") from e

    if message.get("swap_id") != SWAP_ID:
        raise AssertionError("the plaintext offer is not the one published")


def test_settlement_content_is_encrypted_and_opaque():
    event = newest_of("accept")

    if encryption_tag(event) != "nip44_v2":
        raise AssertionError(
            f"the accept declared {encryption_tag(event)!r}, not nip44_v2"
        )

    content = event["content"]

    if content.lstrip().startswith("{"):
        raise AssertionError("the accept content is plaintext JSON")

    #
    # Nothing from the message may survive in the ciphertext. `swap_id`
    # is the field a relay reader would most want, and it is 12 hex
    # characters that would appear literally if encryption were a no-op.
    #
    for leaked in (SWAP_ID, "secret_hash", "blake_bob_pub", "accept"):
        if leaked in content:
            raise AssertionError(
                f"the ciphertext contains {leaked!r}"
            )

    import nip44

    if not nip44.looks_encrypted(content):
        raise AssertionError("the accept content is not a NIP-44 payload")


def test_the_plaintext_peer_refuses_an_encrypted_message():
    """
    A participant that does not speak NIP-44 must refuse rather than
    guess at content it cannot read.
    """

    result = run(
        "alice",
        "receive-accept",
        SWAP_ID,
        encryption="off",
        expect_ok=False,
    )

    if result.returncode == 0:
        raise AssertionError(
            "a plaintext participant accepted an encrypted message:\n"
            + result.stdout
        )

    combined = result.stdout + result.stderr

    if "encrypted but" not in combined and "not enabled" not in combined:
        raise AssertionError(
            f"the refusal did not explain itself:\n{combined}"
        )


def test_an_encrypted_peer_refuses_a_plaintext_downgrade():
    """
    The other direction, and the one that actually matters: once first
    contact is past, a participant that requires encryption must refuse a
    plaintext message rather than treat it as ordinary.
    """

    #
    # Run a second negotiation entirely in plaintext, then read its
    # acceptance with encryption required.
    #
    swap_id = publish_offer_plaintext()

    run(
        "bob",
        "accept-offer",
        swap_id,
        "--expected-alice-pub",
        newest_of("offer")["pubkey"],
        encryption="off",
    )

    event = newest_of("accept")

    if encryption_tag(event) is not None:
        raise AssertionError("the plaintext accept declared encryption")

    result = run(
        "alice",
        "receive-accept",
        swap_id,
        encryption="nip44_v2",
        expect_ok=False,
    )

    if result.returncode == 0:
        raise AssertionError(
            "an encrypted participant accepted a plaintext downgrade:\n"
            + result.stdout
        )

    combined = result.stdout + result.stderr

    if "downgrade" not in combined and "plaintext" not in combined:
        raise AssertionError(
            f"the refusal did not explain itself:\n{combined}"
        )


def publish_offer_plaintext():
    quiet(
        "alice",
        "new",
        "--blake", "0.3",
        "--sats", "40000",
        encryption="off",
    )

    swap_id = sorted(
        path.stem
        for path in PROFILES["alice"]["state"].glob("*.json")
        if len(path.stem) == 12
    )[-1]

    quiet("alice", "publish-offer", swap_id, encryption="off")

    return swap_id


def test_an_untagged_payload_is_refused():
    """
    Content that looks like a payload but carries no `encryption` tag is
    neither readable plaintext nor a declared encrypted message. It must
    be refused rather than parsed as text.
    """

    import nip44

    import swap_v2

    payload = nip44.encrypt(
        json.dumps({"type": "accept", "swap_id": SWAP_ID}),
        b"\x11" * 32,
    )

    event = {
        "content": payload,
        "tags": [["t", "blake-swap"], ["type", "accept"]],
        "pubkey": "aa" * 32,
    }

    try:
        swap_v2.decode_coordination_content(
            event,
            swap_id=SWAP_ID,
            message_type="accept",
        )
    except RuntimeError as e:
        if "no encryption tag" not in str(e):
            raise AssertionError(f"unhelpful refusal: {e}") from e
    else:
        raise AssertionError("an untagged payload was accepted")


def test_a_plaintext_message_still_parses_when_encryption_is_off():
    """
    The default mode is unchanged, including the encrypted-mode tag being
    absent. This is the compatibility guarantee: an existing deployment
    must not notice the feature at all.
    """

    import swap_v2

    event = {
        "content": json.dumps(
            {"type": "accept", "swap_id": SWAP_ID},
            separators=(",", ":"),
            sort_keys=True,
        ),
        "tags": [["t", "blake-swap"], ["type", "accept"]],
        "pubkey": "aa" * 32,
    }

    message = swap_v2.decode_coordination_content(
        event,
        swap_id=SWAP_ID,
        message_type="accept",
    )

    if message is None or message.get("type") != "accept":
        raise AssertionError(f"a plaintext message did not parse: {message}")


def test_a_bad_mode_is_refused():
    import swap_v2

    import os

    original = os.environ.get("BLAKE_SWAP_NOSTR_ENCRYPTION")

    os.environ["BLAKE_SWAP_NOSTR_ENCRYPTION"] = "rot13"

    try:
        try:
            swap_v2.nostr_encryption_mode()
        except RuntimeError as e:
            if "BLAKE_SWAP_NOSTR_ENCRYPTION" not in str(e):
                raise AssertionError(f"unhelpful error: {e}") from e
        else:
            raise AssertionError("an unknown encryption mode was accepted")
    finally:
        if original is None:
            os.environ.pop("BLAKE_SWAP_NOSTR_ENCRYPTION", None)
        else:
            os.environ["BLAKE_SWAP_NOSTR_ENCRYPTION"] = original


def main():
    print()
    print("=" * 72)
    print("ENCRYPTED COORDINATION (NIP-44)")
    print("=" * 72)
    print()
    print("relay:", RELAY_URL)
    print("state:", BASE)
    print()

    try:
        h.preflight()
        ok("preflight")

        check("an encrypted handshake completes", test_encrypted_handshake_works)
        check("the offer stays plaintext", test_the_offer_stays_plaintext)
        check("settlement content is encrypted and opaque", test_settlement_content_is_encrypted_and_opaque)
        check("a plaintext peer refuses an encrypted message", test_the_plaintext_peer_refuses_an_encrypted_message)
        check("an encrypted peer refuses a plaintext downgrade", test_an_encrypted_peer_refuses_a_plaintext_downgrade)
        check("an untagged payload is refused", test_an_untagged_payload_is_refused)
        check("plaintext still parses when encryption is off", test_a_plaintext_message_still_parses_when_encryption_is_off)
        check("an unknown mode is refused", test_a_bad_mode_is_refused)
    finally:
        STUB.stop()

    print()
    print("=" * 72)
    print("ENCRYPTED COORDINATION SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()
    print(f"swap: {SWAP_ID}")
    print(
        f"stub traffic: connections={STUB.connections} "
        f"published={STUB.published} requests={STUB.requests}"
    )
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
