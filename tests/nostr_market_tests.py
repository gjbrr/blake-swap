#!/usr/bin/env python3
"""
Nostr transport and marketplace tests, against an in-process relay stub.

These exercise the coordination layer end to end without contacting any
public relay:

  * publishing and reading back a signed settlement message;
  * author pinning, so a wrong signer is refused;
  * the verified-event cache and prefix resolution;
  * the full market lifecycle: offer -> take -> match -> cancel, with
    the canonical OPEN / MATCHED / CANCELLED state derived from events.

No chain nodes and no network are required. The one engine call that
would reach a node (the minimum-trade fee check) is stubbed, because
this suite is about the coordination layer, not about fees.

Discovery is asynchronous and best-effort by design: the engine fans out
to every relay, tolerates failures, and treats a match as valid only once
its referenced offer and take can also be resolved. Every assertion here
therefore identifies its own event by set difference and polls briefly
for it, rather than assuming a single immediate query has converged.
"""

from __future__ import annotations

import asyncio
import contextlib
import io
import json
import os
import sys
import tempfile
import time
from pathlib import Path
from types import SimpleNamespace


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

sys.path.insert(0, str(HERE))
sys.path.insert(0, str(ROOT / "src"))

from nostr_relay_stub import NostrRelayStub  # noqa: E402

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


def quiet(fn, *args, **kwargs):
    with contextlib.redirect_stdout(io.StringIO()):
        return fn(*args, **kwargs)


def eventually(fn, attempts=20, delay=0.1):
    """
    Poll `fn` until it returns a non-empty result.

    Bounds the wait for asynchronous relay discovery without weakening
    the assertion: the expected event must still appear.
    """
    result = None

    for _ in range(attempts):
        result = fn()

        if result:
            return result

        time.sleep(delay)

    return result


# The relay stub must be listening before the engine reads its relay
# configuration at import time.
STUB = NostrRelayStub()
STUB_URL = STUB.start()

SCRATCH = Path(tempfile.mkdtemp(prefix="blake-nostr-market-"))
KEY_FILE = SCRATCH / "participant.key"
KEY_FILE.write_text("11" * 32 + "\n")
KEY_FILE.chmod(0o600)

os.environ["BLAKE_SWAP_STATE_DIR"] = str(SCRATCH)
os.environ["BLAKE_SWAP_MESSAGE_DIR"] = str(SCRATCH / "messages")
os.environ["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(KEY_FILE)
os.environ["BLAKE_SWAP_TRANSPORT"] = "nostr"
os.environ["BLAKE_SWAP_NOSTR_RELAYS"] = STUB_URL
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_FILE", None)
os.environ.pop("BLAKE_SWAP_NOSTR_KEY_DIR", None)

import swap_v2  # noqa: E402


# The trade-minimum check would reach a chain node; this suite is about
# the coordination layer, so pin it to the smallest legal value.
swap_v2.btc_trade_minimum_sats = lambda: 1

ME = swap_v2.current_nostr_pubkey()


# ------------------------------------------------------------ helpers


def fetch_offers():
    return asyncio.run(swap_v2.nostr_fetch_market_offers(limit=100))


def fetch_takes(offer_id):
    _original, _offer_message, valid = asyncio.run(
        swap_v2.nostr_fetch_market_takes(offer_id, limit=100)
    )
    return valid


def fetch_matches():
    return asyncio.run(swap_v2.nostr_fetch_market_matches(ME, limit=100))


def market_state():
    buf = io.StringIO()

    with contextlib.redirect_stdout(buf):
        swap_v2.command_market_state(
            SimpleNamespace(limit=100, json=True)
        )

    text = buf.getvalue().strip()
    return json.loads(text[text.index("{"):])


def state_for(offer_id):
    for offer in market_state()["offers"]:
        if offer["event_id"] == offer_id:
            return offer["state"]

    return None


def publish_offer(blake, sats, minutes=30, direction="blake-for-btc"):
    """
    Publish an offer and return exactly that offer.

    Identified by set difference, not by position: several open offers
    from earlier tests can share a created_at second, in which case
    ordering falls back to the (random) event id.
    """
    before = {offer["event_id"] for offer in fetch_offers()}

    quiet(
        swap_v2.command_publish_market_offer,
        SimpleNamespace(
            blake=blake,
            sats=sats,
            minutes=minutes,
            direction=direction,
        ),
    )

    new = eventually(
        lambda: [
            offer
            for offer in fetch_offers()
            if offer["event_id"] not in before
        ]
    )

    if not new:
        raise AssertionError("published offer was not discoverable")

    return new[0]


# -------------------------------------------------------------- tests


def test_message_round_trip():
    swap_id = "aa11bb22cc33"

    message = {
        "type": "offer",
        "swap_id": swap_id,
        "blake_amount": "1.0",
        "btc_sats": 370000,
    }

    event = swap_v2.build_nostr_event(swap_id, "offer", message)
    event_id = asyncio.run(swap_v2.nostr_publish_event(event))

    assert event_id == event["id"], "publish returned a different event id"

    got, _location, signer = asyncio.run(
        swap_v2.nostr_fetch_message(swap_id, "offer")
    )

    assert got["type"] == "offer", got
    assert got["swap_id"] == swap_id, got
    assert signer == event["pubkey"], "signer mismatch"


def test_wrong_author_is_refused():
    swap_id = "bb22cc33dd44"

    event = swap_v2.build_nostr_event(
        swap_id,
        "accept",
        {"type": "accept", "swap_id": swap_id},
    )
    asyncio.run(swap_v2.nostr_publish_event(event))

    try:
        asyncio.run(
            swap_v2.nostr_fetch_message(
                swap_id,
                "accept",
                expected_pubkey="ff" * 32,
            )
        )
    except Exception:
        return

    raise AssertionError("a wrong-author lookup succeeded")


def test_event_cache_and_prefix_resolution():
    swap_id = "cc33dd44ee55"

    event = swap_v2.build_nostr_event(
        swap_id,
        "offer",
        {"type": "offer", "swap_id": swap_id},
    )
    asyncio.run(swap_v2.nostr_publish_event(event))

    cached = swap_v2.nostr_load_cached_event(event["id"])
    assert cached["id"] == event["id"], "cache round trip failed"

    resolved = swap_v2.resolve_cached_nostr_prefix(
        event["id"][:16],
        label="Nostr event",
        announce=False,
    )
    assert resolved == event["id"], resolved

    fetched = asyncio.run(swap_v2.nostr_fetch_event_by_id(event["id"]))
    assert fetched["id"] == event["id"], "fetch by id failed"


def test_market_offer_lifecycle():
    offer = publish_offer(blake=1.0, sats=370000)

    assert offer["direction"] == "blake-for-btc", offer
    assert offer["maker"] == ME, offer
    assert state_for(offer["event_id"]) == "OPEN", market_state()


def test_market_take_and_match():
    offer = publish_offer(blake=2.0, sats=740000)
    offer_id = offer["event_id"]

    before = {take["event_id"] for take in fetch_takes(offer_id)}

    quiet(
        swap_v2.command_take_market_offer,
        SimpleNamespace(offer_id=offer_id),
    )

    new_takes = eventually(
        lambda: [
            take
            for take in fetch_takes(offer_id)
            if take["event_id"] not in before
        ]
    )

    assert new_takes, "take was not discoverable"

    take_item = new_takes[0]
    assert take_item["taker"] == ME, take_item

    # The match builder validates the raw take event, not the summary.
    take_event = swap_v2.nostr_load_cached_event(take_item["event_id"])
    offer_event = swap_v2.nostr_load_cached_event(offer_id)

    match_event, _match_message = swap_v2.build_market_match_event(
        offer_event,
        take_event,
        SimpleNamespace(
            swap_id="1234567890ab",
            blake_amount=2.0,
            btc_sats=740000,
        ),
    )

    asyncio.run(swap_v2.nostr_publish_event(match_event))

    matches = eventually(
        lambda: [
            match
            for match in fetch_matches()
            if match["event_id"] == match_event["id"]
        ]
    )

    assert matches, (
        "match was not discoverable "
        f"(stored at relay: {match_event['id'] in STUB.events})"
    )
    assert matches[0]["swap_id"] == "1234567890ab", matches[0]

    assert state_for(offer_id) == "MATCHED", market_state()


def test_market_cancel():
    offer = publish_offer(blake=3.0, sats=1110000)
    offer_id = offer["event_id"]

    cancel = swap_v2.build_market_cancel_event(offer_id)
    asyncio.run(swap_v2.nostr_publish_event(cancel))

    deadline = time.time() + 5.0

    while time.time() < deadline:
        if state_for(offer_id) == "CANCELLED":
            return
        time.sleep(0.1)

    raise AssertionError(
        f"offer did not become CANCELLED (state={state_for(offer_id)!r})"
    )


def test_market_takes_json_command():
    offer = publish_offer(blake=4.0, sats=1480000)
    offer_id = offer["event_id"]

    before = {take["event_id"] for take in fetch_takes(offer_id)}

    quiet(
        swap_v2.command_take_market_offer,
        SimpleNamespace(offer_id=offer_id),
    )

    eventually(
        lambda: [
            take
            for take in fetch_takes(offer_id)
            if take["event_id"] not in before
        ]
    )

    buf = io.StringIO()

    with contextlib.redirect_stdout(buf):
        swap_v2.command_market_takes(
            SimpleNamespace(
                offer_id=offer_id,
                limit=100,
                json=True,
            )
        )

    text = buf.getvalue().strip()
    payload = json.loads(text[text.index("{"):])

    assert payload["offer_id"] == offer_id, payload
    assert payload["takes"], "no takes in JSON output"


def test_stub_observed_traffic():
    assert STUB.published > 0, "nothing was published to the stub"
    assert STUB.requests > 0, "nothing was queried from the stub"
    assert STUB.connections > 0, "no client ever connected"



def test_market_offer_carries_an_expiration_tag():
    """
    A live offer is a stored advertisement, so it tells a relay when it may
    forget it (NIP-40). The tag and the content's expires_at are held
    together: a signed event whose tag disagrees is refused, because the
    relay's retention would otherwise outlive the terms the maker signed.
    """
    offer = publish_offer(blake=4.0, sats=1480000)

    event = swap_v2.nostr_load_cached_event(offer["event_id"])
    message = json.loads(event["content"])

    tags = [tag for tag in event["tags"] if tag and tag[0] == "expiration"]
    assert tags, f"offer {offer['event_id']} carries no expiration tag: {event['tags']}"
    assert tags[0][1] == str(message["expires_at"]), (tags[0], message["expires_at"])

    # The kind is a stored one, not an ephemeral kind: a relay that drops
    # ephemeral events would lose every offer.
    assert event["kind"] == 1, event["kind"]

    # The tag is checked *after* the signature, so the tampered event has to be signed by the
    # same key: the case that matters is a maker publishing different terms to the relay than
    # the ones in the content, not a forgery by somebody else.
    from coincurve import PrivateKey

    private_key = PrivateKey(bytes.fromhex(swap_v2.nostr_private_key_hex()))
    tampered = dict(event)
    tampered["tags"] = [
        ["expiration", str(message["expires_at"] + 3600)] if tag and tag[0] == "expiration" else tag
        for tag in event["tags"]
    ]
    event_id = swap_v2.nostr_event_id(tampered)
    tampered["id"] = event_id.hex()
    tampered["sig"] = private_key.sign_schnorr(event_id).hex()

    try:
        swap_v2.nostr_parse_market_offer_event(tampered)
    except RuntimeError as error:
        assert "expiration tag" in str(error), error
    else:
        raise AssertionError("an offer whose expiration tag disagrees with its content was accepted")


def main():
    print()
    print("=" * 72)
    print("NOSTR TRANSPORT AND MARKETPLACE (local relay stub)")
    print("=" * 72)
    print()
    print("relay:", STUB_URL)
    print("identity:", ME[:16])
    print()

    try:
        check("a signed settlement message round-trips over Nostr", test_message_round_trip)
        check("a wrong-author lookup is refused", test_wrong_author_is_refused)
        check("the event cache and prefix resolution work", test_event_cache_and_prefix_resolution)
        check("a published market offer is discoverable and OPEN", test_market_offer_lifecycle)
        check("a stored offer carries a NIP-40 expiration tag", test_market_offer_carries_an_expiration_tag)
        check("take -> match binds a swap and yields MATCHED", test_market_take_and_match)
        check("a cancelled offer derives CANCELLED", test_market_cancel)
        check("market-takes --json emits validated takes", test_market_takes_json_command)
        check("the stub observed real publish/query traffic", test_stub_observed_traffic)
    finally:
        STUB.stop()

    print()
    print("=" * 72)
    print("NOSTR TRANSPORT SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()
    print(
        f"stub traffic: connections={STUB.connections} "
        f"published={STUB.published} requests={STUB.requests}"
    )
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
