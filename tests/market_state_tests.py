#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "src"
ENGINE = SRC / "swap_v2.py"

if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))


def load_engine():
    spec = importlib.util.spec_from_file_location(
        "swap_v2_market_state_test",
        ENGINE,
    )
    module = importlib.util.module_from_spec(
        spec
    )
    spec.loader.exec_module(module)
    return module


m = load_engine()


def require(value, message):
    if not value:
        raise AssertionError(message)


def fake_event(
    *,
    event_id,
    pubkey,
    created_at,
    content,
    tags,
):
    return {
        "id": event_id,
        "pubkey": pubkey,
        "created_at": created_at,
        "kind": 1,
        "content": json.dumps(
            content,
            separators=(",", ":"),
            sort_keys=True,
        ),
        "tags": tags,
        #
        # Signature validation is stubbed below.
        #
        "sig": "00" * 64,
    }


MAKER = "11" * 32
TAKER = "22" * 32

OFFER_ID = "aa" * 32
TAKE_ID = "bb" * 32
MATCH_ID = "cc" * 32
CANCEL_ID = "dd" * 32

SWAP_ID = "123456abcdef"

CREATED = 1_000
EXPIRES = 2_000


offer_message = {
    "type": "market-offer",
    "version": 2,
    "created_at": CREATED,
    "expires_at": EXPIRES,
    "base": "BLAKE",
    "quote": "BTC",
    "direction":
        m.MARKET_DIRECTION_BLAKE_FOR_BTC,
    "blake_amount": 10.0,
    "btc_sats": 25_000,
}

offer_event = fake_event(
    event_id=OFFER_ID,
    pubkey=MAKER,
    created_at=CREATED,
    content=offer_message,
    tags=[],
)


#
# Keep the test focused on market-state semantics rather
# than cryptographic primitive implementation.
#
m.verify_nostr_event = lambda event: True


def parse_offer(event):
    require(
        event["id"] == OFFER_ID,
        "unexpected offer",
    )

    roles = m.market_protocol_roles(
        m.MARKET_DIRECTION_BLAKE_FOR_BTC
    )

    return (
        offer_message,
        {
            "event_id": OFFER_ID,
            "maker": MAKER,
            "created_at": CREATED,
            "expires_at": EXPIRES,
            "blake_amount": 10.0,
            "btc_sats": 25_000,
            "direction":
                m.MARKET_DIRECTION_BLAKE_FOR_BTC,
            "maker_role": roles["maker_role"],
            "taker_role": roles["taker_role"],
        },
    )


m.nostr_parse_market_offer_event = parse_offer


def valid_take_item(
    take_event,
    offer_event_arg,
    offer_message_arg,
    offer_id,
):
    require(
        take_event["id"] == TAKE_ID,
        "wrong take evidence",
    )
    require(
        offer_id == OFFER_ID,
        "wrong offer id",
    )

    roles = m.market_protocol_roles(
        m.MARKET_DIRECTION_BLAKE_FOR_BTC
    )

    return {
        "event_id": TAKE_ID,
        "offer_id": OFFER_ID,
        "maker": MAKER,
        "taker": TAKER,
        "blake_amount": 10.0,
        "btc_sats": 25_000,
        "direction":
            m.MARKET_DIRECTION_BLAKE_FOR_BTC,
        "maker_role": roles["maker_role"],
        "taker_role": roles["taker_role"],
        "created_at": 1_100,
    }


m.nostr_validate_market_take_event = (
    valid_take_item
)


take_event = fake_event(
    event_id=TAKE_ID,
    pubkey=TAKER,
    created_at=1_100,
    content={
        "type": "market-take",
    },
    tags=[],
)


def make_match(
    *,
    event_id=MATCH_ID,
    created_at=1_200,
    maker=MAKER,
    offer_id=OFFER_ID,
    take_id=TAKE_ID,
    taker=TAKER,
    blake=10.0,
    sats=25_000,
):
    roles = m.market_protocol_roles(
        m.MARKET_DIRECTION_BLAKE_FOR_BTC
    )

    return fake_event(
        event_id=event_id,
        pubkey=maker,
        created_at=created_at,
        content={
            "type": "market-match",
            "version": 2,
            "created_at": created_at,
            "base": "BLAKE",
            "quote": "BTC",
            "direction":
                m.MARKET_DIRECTION_BLAKE_FOR_BTC,
            "maker_role": roles["maker_role"],
            "taker_role": roles["taker_role"],
            "offer_id": offer_id,
            "take_id": take_id,
            "swap_id": SWAP_ID,
            "maker": maker,
            "taker": taker,
            "blake_amount": blake,
            "btc_sats": sats,
        },
        tags=[
            ["t", "blake-swap-market-match"],
            [
                "direction",
                m.MARKET_DIRECTION_BLAKE_FOR_BTC,
            ],
            ["e", offer_id],
            ["e", take_id],
            ["p", taker],
            ["d", SWAP_ID],
        ],
    )


def state(events, now=1_500):
    return m.nostr_derive_market_offer_state(
        offer_event,
        events,
        now=now,
    )["state"]


print("=" * 72)
print("CANONICAL MARKET STATE REGRESSION")
print("=" * 72)


require(
    state([offer_event]) == "OPEN",
    "fresh offer not OPEN",
)
print("PASS  fresh offer is OPEN")


require(
    state(
        [
            offer_event,
            take_event,
        ]
    )
    == "OPEN",
    "market-take alone closed offer",
)
print("PASS  take alone does not close offer")


match = make_match()

require(
    state(
        [
            offer_event,
            take_event,
            match,
        ]
    )
    == "MATCHED",
    "valid complete match evidence did not close offer",
)
print("PASS  valid offer+take+match => MATCHED")


bad_terms = make_match(
    event_id="ce" * 32,
    blake=99.0,
)

require(
    state(
        [
            offer_event,
            take_event,
            bad_terms,
        ]
    )
    == "OPEN",
    "economic mismatch hid open offer",
)
print("PASS  inconsistent match does not hide offer")


missing_take = make_match(
    event_id="cf" * 32,
    take_id="ef" * 32,
)

require(
    state(
        [
            offer_event,
            missing_take,
        ]
    )
    == "OPEN",
    "missing referenced take hid open offer",
)
print("PASS  nonexistent take does not close offer")


wrong_maker = make_match(
    event_id="ca" * 32,
    maker="33" * 32,
)

require(
    state(
        [
            offer_event,
            take_event,
            wrong_maker,
        ]
    )
    == "OPEN",
    "wrong-maker match hid offer",
)
print("PASS  wrong maker cannot close offer")


at_expiry = make_match(
    event_id="cb" * 32,
    created_at=EXPIRES,
)

require(
    state(
        [
            offer_event,
            take_event,
            at_expiry,
        ],
        now=EXPIRES,
    )
    == "EXPIRED",
    "match at expiry beat expiry",
)
print("PASS  expiry wins at exact boundary")


require(
    state(
        [
            offer_event,
            take_event,
            match,
        ],
        now=EXPIRES + 500,
    )
    == "MATCHED",
    "wall-clock expiry rewrote prior MATCHED state",
)
print("PASS  prior MATCHED remains terminal after expiry")


print()
print("PASS: canonical market-state regression")


#
# ------------------------------------------------------------
# Competing terminal events.
# ------------------------------------------------------------
#

def make_cancel(
    *,
    event_id=CANCEL_ID,
    created_at=1_300,
    maker=MAKER,
    offer_id=OFFER_ID,
):
    return fake_event(
        event_id=event_id,
        pubkey=maker,
        created_at=created_at,
        content={
            "type": "market-cancel",
            "version": 1,
            "created_at": created_at,
            "offer_id": offer_id,
        },
        tags=[
            ["t", "blake-swap-market-cancel"],
            ["e", offer_id],
        ],
    )


cancel_after_match = make_cancel(
    event_id="d1" * 32,
    created_at=1_300,
)

require(
    state(
        [
            offer_event,
            take_event,
            match,
            cancel_after_match,
        ]
    )
    == "MATCHED",
    "later cancellation rewrote earlier MATCHED state",
)
print("PASS  earlier MATCHED beats later CANCELLED")


cancel_before_match = make_cancel(
    event_id="d2" * 32,
    created_at=1_150,
)

require(
    state(
        [
            offer_event,
            take_event,
            match,
            cancel_before_match,
        ]
    )
    == "CANCELLED",
    "earlier cancellation did not beat later match",
)
print("PASS  earlier CANCELLED beats later MATCHED")


#
# Same timestamp: event ID is the deterministic tie-break.
#
tie_time = 1_250

match_low_id = make_match(
    event_id="10" * 32,
    created_at=tie_time,
)

cancel_high_id = make_cancel(
    event_id="f0" * 32,
    created_at=tie_time,
)

require(
    state(
        [
            offer_event,
            take_event,
            cancel_high_id,
            match_low_id,
        ]
    )
    == "MATCHED",
    "lower event ID did not win timestamp tie",
)
print("PASS  same-time terminal tie uses event ID ordering")


match_high_id = make_match(
    event_id="f1" * 32,
    created_at=tie_time,
)

cancel_low_id = make_cancel(
    event_id="11" * 32,
    created_at=tie_time,
)

require(
    state(
        [
            offer_event,
            take_event,
            match_high_id,
            cancel_low_id,
        ]
    )
    == "CANCELLED",
    "lower cancellation event ID did not win timestamp tie",
)
print("PASS  tie-break is state-neutral and deterministic")


late_cancel = make_cancel(
    event_id="d3" * 32,
    created_at=EXPIRES,
)

require(
    state(
        [
            offer_event,
            late_cancel,
        ],
        now=EXPIRES,
    )
    == "EXPIRED",
    "cancellation at expiry beat expiry",
)
print("PASS  cancellation at expiry cannot override EXPIRED")


print()
print("PASS: competing terminal-event ordering")
