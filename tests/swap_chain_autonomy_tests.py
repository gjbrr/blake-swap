#!/usr/bin/env python3

from __future__ import annotations

import sys
import time
from pathlib import Path

import swap_protocol_tests as h


MESSAGE_DIR = h.MESSAGE_DIR


def message_path(
    swap_id,
    message_type,
):
    return (
        MESSAGE_DIR
        / f"{swap_id}.{message_type}.json"
    )


def require_no_message(
    swap_id,
    message_type,
):
    path = message_path(
        swap_id,
        message_type,
    )

    h.require(
        not path.exists(),
        (
            f"unexpected coordination message exists: "
            f"{path}"
        ),
    )


def sync_funding(
    swap_id,
    role,
):
    return h.swap_cmd(
        "sync-funding",
        swap_id,
        "--role",
        role,
        role=role,
    )


def sync_settlement(
    swap_id,
    role,
):
    return h.swap_cmd(
        "sync-settlement",
        swap_id,
        "--role",
        role,
        role=role,
    )


def test_idle_funding_poll():
    h.banner(
        "CHAIN AUTONOMY: IDLE FUNDING POLL"
    )

    swap_id = h.make_fresh_swap()

    h.handshake(
        swap_id
    )

    h.assert_state(
        swap_id,
        "alice",
        "OFFERED",
    )

    h.assert_state(
        swap_id,
        "bob",
        "OFFERED",
    )

    sync_funding(
        swap_id,
        "alice",
    )

    sync_funding(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "alice",
        "OFFERED",
    )

    h.assert_state(
        swap_id,
        "bob",
        "OFFERED",
    )

    h.ok(
        "funding reconciliation is a safe no-op before funding"
    )

    proc = h.swap_cmd(
        "next",
        role="bob",
    )

    output = (
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or "")
    )

    h.require(
        "SETTLEMENT WORK AVAILABLE" not in output,
        (
            "unfunded OFFERED swap was incorrectly "
            "surfaced as resumable settlement work"
        ),
    )

    h.assert_state(
        swap_id,
        "bob",
        "OFFERED",
    )

    h.ok(
        "next does not surface resume before chain funding exists"
    )

    print(
        "swap:",
        swap_id,
    )


def test_no_funding_announcements():
    h.banner(
        "CHAIN AUTONOMY: FUNDING WITHOUT ANNOUNCEMENTS"
    )

    swap_id = h.make_fresh_swap()

    h.handshake(
        swap_id
    )

    #
    # Alice funds BLAKE normally but deliberately does not publish
    # blake-funded.
    #
    h.swap_cmd(
        "fund-blake",
        swap_id,
        role="alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_FUNDED",
    )

    h.assert_state(
        swap_id,
        "bob",
        "OFFERED",
    )

    proc = h.swap_cmd(
        "next",
        role="bob",
    )

    output = (
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or "")
    )

    h.require(
        "SETTLEMENT WORK AVAILABLE" in output,
        (
            "funded-ahead OFFERED swap was not "
            "surfaced as resumable settlement work"
        ),
    )

    h.require(
        "sync-funding" in output,
        (
            "funded-ahead OFFERED swap did not "
            "surface sync-funding as the next action"
        ),
    )

    h.assert_state(
        swap_id,
        "bob",
        "OFFERED",
    )

    h.ok(
        "next surfaces safe funding reconciliation without changing state"
    )

    require_no_message(
        swap_id,
        "blake-funded",
    )

    h.ok(
        "Alice funded BLAKE without funding announcement"
    )

    #
    # Bob discovers the exact confirmed HTLC from BLAKE itself.
    #
    sync_funding(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_FUNDED",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        bob.get(
            "blake_funding_txid"
        )
        ==
        alice.get(
            "blake_funding_txid"
        ),
        "Bob discovered wrong BLAKE funding txid",
    )

    h.require(
        bob.get(
            "blake_funding_vout"
        )
        ==
        alice.get(
            "blake_funding_vout"
        ),
        "Bob discovered wrong BLAKE funding vout",
    )

    h.require(
        bob.get(
            "blake_funding_blockhash"
        )
        ==
        alice.get(
            "blake_funding_blockhash"
        ),
        "Bob discovered wrong BLAKE funding blockhash",
    )

    h.ok(
        "Bob reconstructed BLAKE funding from chain only"
    )

    #
    # Bob now funds Bitcoin but sends no btc-funded message.
    #
    h.swap_cmd(
        "fund-btc",
        swap_id,
        role="bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_FUNDED",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_FUNDED",
    )

    require_no_message(
        swap_id,
        "btc-funded",
    )

    h.ok(
        "Bob funded Bitcoin without funding announcement"
    )

    #
    # Alice is still locally BLAKE_FUNDED and does not know
    # Bob's Bitcoin funding outpoint.  Reconcile/next must NOT
    # defer Bitcoin discovery merely because the timeout has
    # not expired.  That optimization is valid only for Bob,
    # who is the participant responsible for funding Bitcoin.
    #
    alice_before = h.load_state(
        swap_id,
        "alice",
    )

    h.require(
        alice_before.get(
            "btc_funding_txid"
        ) is None,
        (
            "Alice unexpectedly knew BTC funding txid "
            "before chain discovery"
        ),
    )

    h.require(
        alice_before.get(
            "btc_funding_vout"
        ) is None,
        (
            "Alice unexpectedly knew BTC funding vout "
            "before chain discovery"
        ),
    )

    proc = h.swap_cmd(
        "next",
        role="alice",
    )

    output = (
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or "")
    )

    h.require(
        "SETTLEMENT WORK AVAILABLE" in output,
        (
            "Alice did not surface confirmed Bitcoin "
            "funding as resumable work"
        ),
    )

    h.require(
        "sync-funding" in output,
        (
            "Alice deferred Bitcoin funding discovery "
            "instead of surfacing sync-funding"
        ),
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_FUNDED",
    )

    h.ok(
        "Alice surfaces BTC chain discovery before timeout"
    )

    #
    # Alice discovers the exact Bitcoin HTLC herself.
    #
    sync_funding(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_FUNDED",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        alice.get(
            "btc_funding_txid"
        )
        ==
        bob.get(
            "btc_funding_txid"
        ),
        "Alice discovered wrong BTC funding txid",
    )

    h.require(
        alice.get(
            "btc_funding_vout"
        )
        ==
        bob.get(
            "btc_funding_vout"
        ),
        "Alice discovered wrong BTC funding vout",
    )

    h.require(
        alice.get(
            "btc_funding_blockhash"
        )
        ==
        bob.get(
            "btc_funding_blockhash"
        ),
        "Alice discovered wrong BTC funding blockhash",
    )

    h.ok(
        "Alice reconstructed Bitcoin funding from chain only"
    )

    print(
        "swap:",
        swap_id,
    )

    return swap_id


def test_full_post_negotiation_chain_autonomy():
    h.banner(
        "CHAIN AUTONOMY: FULL SWAP WITHOUT ECONOMIC MESSAGES"
    )

    swap_id = h.make_fresh_swap()

    #
    # Negotiation/public-key exchange is still permitted.
    #
    h.handshake(
        swap_id
    )

    #
    # From here forward there will be NO funding or settlement
    # coordination messages.
    #
    h.swap_cmd(
        "fund-blake",
        swap_id,
        role="alice",
    )

    require_no_message(
        swap_id,
        "blake-funded",
    )

    sync_funding(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_FUNDED",
    )

    h.ok(
        "Bob discovered BLAKE funding without Alice"
    )

    h.swap_cmd(
        "fund-btc",
        swap_id,
        role="bob",
    )

    require_no_message(
        swap_id,
        "btc-funded",
    )

    sync_funding(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_FUNDED",
    )

    h.ok(
        "Alice discovered BTC funding without Bob"
    )

    #
    # Alice claims Bitcoin.
    #
    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    require_no_message(
        swap_id,
        "btc-claimed",
    )

    #
    # Bob discovers the claim AND S from Bitcoin.
    #
    sync_settlement(
        swap_id,
        "bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_CLAIMED",
    )

    h.require(
        bob.get(
            "revealed_secret_hex"
        ),
        "Bob did not recover S from Bitcoin",
    )

    h.require(
        bob.get(
            "secret_hex"
        ) is None,
        "Bob improperly received Alice original secret",
    )

    h.ok(
        "Bob discovered BTC claim and S without Alice"
    )

    #
    # Bob claims BLAKE.
    #
    h.swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    require_no_message(
        swap_id,
        "blake-claimed",
    )

    #
    # Alice independently discovers Bob's BLAKE claim.
    #
    sync_settlement(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_CLAIMED",
    )

    h.ok(
        "Alice discovered BLAKE claim without Bob"
    )

    #
    # Bob can independently reconcile his own state too.
    #
    sync_settlement(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_CLAIMED",
    )

    h.swap_cmd(
        "complete",
        swap_id,
        "--role",
        "alice",
        role="alice",
    )

    h.swap_cmd(
        "complete",
        swap_id,
        "--role",
        "bob",
        role="bob",
    )

    h.assert_state(
        swap_id,
        "alice",
        "COMPLETE",
    )

    h.assert_state(
        swap_id,
        "bob",
        "COMPLETE",
    )

    h.ok(
        "full swap completed with no post-negotiation coordination messages"
    )

    for message_type in (
        "blake-funded",
        "btc-funded",
        "btc-claimed",
        "blake-claimed",
    ):
        require_no_message(
            swap_id,
            message_type,
        )

    h.ok(
        "all four economic announcements were absent"
    )

    h.assert_secret_separation(
        swap_id
    )

    h.ok(
        "secret still crossed chains only through Bitcoin witness"
    )

    print(
        "swap:",
        swap_id,
    )


def main():
    h.PASS_COUNT = 0
    h.FAIL_COUNT = 0

    started = time.time()

    tests = [
        (
            "IDLE FUNDING",
            test_idle_funding_poll,
        ),
        (
            "FUNDING DISCOVERY",
            test_no_funding_announcements,
        ),
        (
            "FULL CHAIN AUTONOMY",
            test_full_post_negotiation_chain_autonomy,
        ),
    ]

    try:
        h.preflight()
        h.ok(
            "preflight"
        )

        for name, fn in tests:
            try:
                fn()

            except Exception as e:
                h.fail(
                    name,
                    str(e),
                )

    except Exception as e:
        h.fail(
            "HARNESS",
            str(e),
        )

    elapsed = (
        time.time()
        - started
    )

    h.banner(
        "CHAIN AUTONOMY SUMMARY"
    )

    print(
        f"passes:   {h.PASS_COUNT}"
    )

    print(
        f"failures: {h.FAIL_COUNT}"
    )

    print(
        f"elapsed:  {elapsed:.1f}s"
    )

    print()
    print(
        "Regtest only. Negotiation messages were allowed; "
        "funding and settlement announcements were deliberately absent."
    )

    return (
        1
        if h.FAIL_COUNT
        else 0
    )


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
