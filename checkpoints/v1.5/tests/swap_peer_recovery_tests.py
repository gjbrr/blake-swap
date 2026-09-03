#!/usr/bin/env python3

from __future__ import annotations

import sys
import time

import swap_protocol_tests as h


CRASH_EXIT = 86


def crash_action(
    point: str,
    *args,
    role: str,
):
    env = {
        "BLAKE_SWAP_ROLE": role,
        "BLAKE_SWAP_TEST_MODE": "1",
        "BLAKE_SWAP_TEST_CRASH_POINT": point,
    }

    p = h.run(
        [
            sys.executable,
            h.SWAP_V2,
            *args,
        ],
        env=env,
        expect_ok=False,
    )

    h.require(
        p.returncode == CRASH_EXIT,
        (
            f"{point}: expected deliberate exit "
            f"{CRASH_EXIT}, got {p.returncode}\n"
            f"{p.stderr.strip() or p.stdout.strip()}"
        ),
    )

    return p


def make_two_funded_swap() -> str:
    swap_id = h.make_fresh_swap()

    h.handshake(
        swap_id
    )

    h.fund_blake_and_sync(
        swap_id
    )

    h.fund_btc_and_sync(
        swap_id
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_FUNDED",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_FUNDED",
    )

    return swap_id


def settlement_message(
    swap_id: str,
    message_type: str,
):
    return (
        h.MESSAGE_DIR
        / f"{swap_id}.{message_type}.json"
    )


def assert_no_message(
    swap_id: str,
    message_type: str,
):
    path = settlement_message(
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


def sync(
    swap_id: str,
    role: str,
):
    return h.swap_cmd(
        "sync-settlement",
        swap_id,
        "--role",
        role,
        role=role,
    )


def test_idle_poll():
    h.banner(
        "PEER RECOVERY: IDLE CHAIN POLL"
    )

    swap_id = make_two_funded_swap()

    sync(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_FUNDED",
    )

    h.ok(
        "Bob chain sync is a safe no-op before settlement"
    )

    sync(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_FUNDED",
    )

    h.ok(
        "Alice chain sync is a safe no-op before settlement"
    )

    print("swap:", swap_id)


def test_claim_path_actor_disappears():
    h.banner(
        "PEER RECOVERY: BOTH CLAIM ACTORS DISAPPEAR"
    )

    swap_id = make_two_funded_swap()

    #
    # Alice broadcasts BTC claim and dies before mining/save.
    #
    crash_action(
        "btc-claim-after-broadcast",
        "claim-btc",
        swap_id,
        role="alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_FUNDED",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_FUNDED",
    )

    h.ok(
        "Alice disappeared after BTC claim broadcast"
    )

    assert_no_message(
        swap_id,
        "btc-claimed",
    )

    h.ok(
        "no BTC claim coordination message exists"
    )

    #
    # Confirm Alice's transaction externally.
    #
    h.mine_btc(1)

    #
    # Alice stays dead.
    # Bob learns everything necessary from Bitcoin.
    #
    sync(
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
        bob.get("btc_claim_txid"),
        "Bob did not recover BTC claim txid",
    )

    h.require(
        bob.get("btc_claim_blockhash"),
        "Bob did not recover BTC claim blockhash",
    )

    h.require(
        bob.get("revealed_secret_hex"),
        "Bob did not recover S from Bitcoin",
    )

    h.require(
        bob.get("secret_hex") is None,
        "Bob improperly obtained Alice original secret field",
    )

    h.ok(
        "Bob recovered Alice BTC claim and S without Alice"
    )

    #
    # Bob now claims BLAKE, then Bob dies before save/message.
    #
    crash_action(
        "blake-claim-after-broadcast",
        "claim-blake",
        swap_id,
        role="bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_CLAIMED",
    )

    h.ok(
        "Bob disappeared after BLAKE claim broadcast"
    )

    assert_no_message(
        swap_id,
        "blake-claimed",
    )

    h.ok(
        "no BLAKE claim coordination message exists"
    )

    #
    # Confirm Bob's claim externally.
    #
    h.mine_blake(1)

    #
    # Alice comes back with state still BTC_FUNDED because she died
    # before saving her own BTC claim.
    #
    # One sync invocation should:
    #
    #   BTC_FUNDED
    #      -> recover own BTC claim
    #      -> BTC_CLAIMED
    #      -> recover Bob BLAKE claim
    #      -> BLAKE_CLAIMED
    #
    sync(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_CLAIMED",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    h.require(
        alice.get("btc_claim_txid"),
        "Alice did not recover her own forgotten BTC claim",
    )

    h.require(
        alice.get("blake_claim_txid"),
        "Alice did not independently recover Bob BLAKE claim",
    )

    h.ok(
        "Alice caught up across both chains in one sync"
    )

    #
    # Bob can independently recover his own forgotten BLAKE claim too.
    #
    sync(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_CLAIMED",
    )

    h.ok(
        "Bob recovered his own forgotten BLAKE claim"
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
        "both parties reached COMPLETE with no settlement announcements"
    )

    h.assert_secret_separation(
        swap_id
    )

    h.ok(
        "S came from Bitcoin, never transport"
    )

    print("swap:", swap_id)


def test_refund_path_actor_disappears():
    h.banner(
        "PEER RECOVERY: BOTH REFUND ACTORS DISAPPEAR"
    )

    swap_id = make_two_funded_swap()

    #
    # Mature Bitcoin refund.
    #
    bob = h.load_state(
        swap_id,
        "bob",
    )

    current = int(
        h.btc_cli("getblockcount")
    )

    timeout = int(
        bob["btc_timeout"]
    )

    if timeout > current:
        h.mine_btc(
            timeout - current
        )

    #
    # Bob broadcasts refund and disappears before save/message.
    #
    crash_action(
        "btc-refund-after-broadcast",
        "refund-btc",
        swap_id,
        role="bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_FUNDED",
    )

    h.ok(
        "Bob disappeared after BTC refund broadcast"
    )

    assert_no_message(
        swap_id,
        "btc-refunded",
    )

    h.ok(
        "no BTC refund coordination message exists"
    )

    h.mine_btc(1)

    #
    # Alice independently discovers Bob's refund.
    #
    sync(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_REFUNDED",
    )

    h.ok(
        "Alice recovered Bob BTC refund from Bitcoin only"
    )

    #
    # Mature BLAKE refund.
    #
    alice = h.load_state(
        swap_id,
        "alice",
    )

    current = int(
        h.blake_cli("getblockcount")
    )

    timeout = int(
        alice["blake_timeout"]
    )

    if timeout > current:
        h.mine_blake(
            timeout - current
        )

    #
    # Alice broadcasts BLAKE refund and disappears too.
    #
    crash_action(
        "blake-refund-after-broadcast",
        "refund-blake",
        swap_id,
        role="alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_REFUNDED",
    )

    h.ok(
        "Alice disappeared after BLAKE refund broadcast"
    )

    assert_no_message(
        swap_id,
        "blake-refunded",
    )

    h.ok(
        "no BLAKE refund coordination message exists"
    )

    h.mine_blake(1)

    #
    # Bob is especially stale:
    #
    #   BTC_FUNDED
    #
    # because he died before saving his own BTC refund.
    #
    # One reconciliation should discover BOTH:
    #
    #   Bitcoin refund
    #   BLAKE refund
    #
    sync(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_REFUNDED",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        bob.get("btc_refund_txid"),
        "Bob did not recover forgotten BTC refund",
    )

    h.require(
        bob.get("blake_refund_txid"),
        "Bob did not recover Alice BLAKE refund",
    )

    h.ok(
        "Bob caught up across both refund chains in one sync"
    )

    #
    # Alice can repair her own final state too.
    #
    sync(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_REFUNDED",
    )

    h.ok(
        "Alice recovered her forgotten BLAKE refund"
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
        bob.get("revealed_secret_hex") is None,
        "Bob learned S on refund path",
    )

    h.require(
        alice.get("secret_hex"),
        "Alice lost original S",
    )

    h.ok(
        "refund recovery revealed no secret"
    )

    print("swap:", swap_id)


def main():
    h.PASS_COUNT = 0
    h.FAIL_COUNT = 0

    started = time.time()

    tests = [
        (
            "IDLE POLL",
            test_idle_poll,
        ),
        (
            "CLAIM PEER RECOVERY",
            test_claim_path_actor_disappears,
        ),
        (
            "REFUND PEER RECOVERY",
            test_refund_path_actor_disappears,
        ),
    ]

    try:
        h.preflight()
        h.ok("preflight")

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

    elapsed = time.time() - started

    h.banner(
        "PEER CHAIN RECOVERY SUMMARY"
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
        "Local regtest only. "
        "File transport configured, but settlement "
        "announcements were deliberately omitted."
    )

    return 1 if h.FAIL_COUNT else 0


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
