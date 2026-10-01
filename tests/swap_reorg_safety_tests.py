#!/usr/bin/env python3

import json
import sys
import time

import swap_protocol_tests as h


PASS = 0
FAIL = 0


def banner(title):
    print()
    print("=" * 72)
    print(title)
    print("=" * 72)


def ok(message):
    global PASS
    PASS += 1
    print("PASS ", message)


def fail(name, exc):
    global FAIL
    FAIL += 1
    print("FAIL ", name)
    print("     ", exc)


def output_of(proc):
    return (
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or "")
    )


def test_blake_funding_reorg():
    banner(
        "REORG: BLAKE FUNDING ROLLBACK "
        "BLOCKS BTC FUNDING"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)

    alice = h.load_state(
        swap_id,
        "alice",
    )

    blockhash = alice[
        "blake_funding_blockhash"
    ]

    h.blake_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        bob = h.load_state(
            swap_id,
            "bob",
        )

        h.require(
            bob["state"] == "BLAKE_FUNDED",
            "Bob durable state did not remain "
            "BLAKE_FUNDED for stale-state probe",
        )

        proc = h.swap_cmd(
            "fund-btc",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        text = output_of(proc)

        h.require(
            proc.returncode != 0,
            "Bob funded BTC after BLAKE "
            "confirmation rollback",
        )

        h.require(
            (
                "not confirmed" in text
                or "unavailable" in text
                or "already spent" in text
            ),
            (
                "fund-btc failed for unexpected reason:\n"
                + text[-1600:]
            ),
        )

        ok(
            "Bob refuses BTC funding after "
            "BLAKE funding reorg"
        )

    finally:
        h.blake_cli(
            "reconsiderblock",
            blockhash,
        )


def test_btc_funding_reorg():
    banner(
        "REORG: BTC FUNDING ROLLBACK "
        "BLOCKS BTC CLAIM"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    blockhash = bob[
        "btc_funding_blockhash"
    ]

    h.btc_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        alice = h.load_state(
            swap_id,
            "alice",
        )

        h.require(
            alice["state"] == "BTC_FUNDED",
            "Alice durable state did not remain "
            "BTC_FUNDED for stale-state probe",
        )

        proc = h.swap_cmd(
            "claim-btc",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        text = output_of(proc)

        h.require(
            proc.returncode != 0,
            "Alice claimed BTC after BTC "
            "funding confirmation rollback",
        )

        h.require(
            "not confirmed" in text,
            (
                "claim-btc failed for unexpected reason:\n"
                + text[-1600:]
            ),
        )

        ok(
            "Alice refuses BTC claim after "
            "BTC funding reorg"
        )

    finally:
        h.btc_cli(
            "reconsiderblock",
            blockhash,
        )


def test_btc_claim_reorg():
    banner(
        "REORG: BTC CLAIM ROLLBACK "
        "BLOCKS BLAKE CLAIM"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    blockhash = alice[
        "btc_claim_blockhash"
    ]

    h.swap_cmd(
        "publish-btc-claimed",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    h.btc_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        bob = h.load_state(
            swap_id,
            "bob",
        )

        h.require(
            bob["state"] == "BTC_CLAIMED",
            "Bob durable state did not remain "
            "BTC_CLAIMED for stale-state probe",
        )

        h.require(
            bool(
                bob.get(
                    "revealed_secret_hex"
                )
            ),
            "Bob lost S before reorg safety probe",
        )

        proc = h.swap_cmd(
            "claim-blake",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        text = output_of(proc)

        h.require(
            proc.returncode != 0,
            "Bob claimed BLAKE after BTC "
            "claim confirmation rollback",
        )

        h.require(
            (
                "Bitcoin claim is no longer "
                "on the active chain"
                in text
                or
                "Bitcoin claim is not confirmed"
                in text
            ),
            (
                "claim-blake failed for "
                "unexpected reason:\n"
                + text[-1600:]
            ),
        )

        ok(
            "Bob refuses BLAKE claim after "
            "BTC claim reorg"
        )

    finally:
        h.btc_cli(
            "reconsiderblock",
            blockhash,
        )


def test_btc_refund_reorg():
    banner(
        "REORG: BTC REFUND ROLLBACK "
        "BLOCKS BLAKE REFUND"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    blocks = (
        int(bob["btc_timeout"])
        - int(
            h.btc_cli(
                "getblockcount"
            )
        )
    )

    if blocks > 0:
        h.mine_btc(blocks)

    h.swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    blockhash = bob[
        "btc_refund_blockhash"
    ]

    h.swap_cmd(
        "publish-btc-refunded",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "receive-btc-refunded",
        swap_id,
        role="alice",
    )

    h.btc_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        alice = h.load_state(
            swap_id,
            "alice",
        )

        h.require(
            alice["state"] == "BTC_REFUNDED",
            "Alice durable state did not remain "
            "BTC_REFUNDED for stale-state probe",
        )

        blocks = (
            int(alice["blake_timeout"])
            - int(
                h.blake_cli(
                    "getblockcount"
                )
            )
        )

        if blocks > 0:
            h.mine_blake(blocks)

        proc = h.swap_cmd(
            "refund-blake",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        text = output_of(proc)

        h.require(
            proc.returncode != 0,
            "Alice refunded BLAKE after BTC "
            "refund confirmation rollback",
        )

        h.require(
            (
                "Bitcoin refund is no longer "
                "on the active chain"
                in text
                or
                "Bitcoin refund is not confirmed"
                in text
            ),
            (
                "refund-blake failed for "
                "unexpected reason:\n"
                + text[-1600:]
            ),
        )

        ok(
            "Alice refuses BLAKE refund after "
            "BTC refund reorg"
        )

    finally:
        h.btc_cli(
            "reconsiderblock",
            blockhash,
        )


def settle_happy_through_blake_claim():
    swap_id = h.make_fresh_swap()

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "publish-btc-claimed",
        swap_id,
        role="alice",
    )

    h.swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "publish-blake-claimed",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "receive-blake-claimed",
        swap_id,
        role="alice",
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

    return swap_id


def test_final_blake_claim_pending_reorg():
    banner(
        "REORG: FINAL BLAKE CLAIM "
        "PENDING AFTER ROLLBACK"
    )

    swap_id = (
        settle_happy_through_blake_claim()
    )

    print("swap:", swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    txid = bob[
        "blake_claim_txid"
    ]

    blockhash = bob[
        "blake_claim_blockhash"
    ]

    h.blake_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        #
        # Disconnected claim should normally be
        # returned to the mempool.
        #
        entry = h.blake_cli(
            "getmempoolentry",
            txid,
        )

        h.require(
            bool(entry),
            "reorged final BLAKE claim "
            "did not return to mempool",
        )

        for role in (
            "alice",
            "bob",
        ):
            proc = h.swap_cmd(
                "sync-settlement",
                swap_id,
                "--role",
                role,
                role=role,
            )

            h.require(
                (
                    "FINAL BLAKE CLAIM "
                    "PENDING AFTER REORG"
                    in proc.stdout
                ),
                (
                    f"{role} did not report "
                    "pending final claim:\n"
                    + proc.stdout[-1600:]
                ),
            )

            state = h.load_state(
                swap_id,
                role,
            )

            h.require(
                state["state"] == "COMPLETE",
                f"{role} incorrectly rolled "
                "COMPLETE backward while exact "
                "claim remained pending",
            )

        ok(
            "final BLAKE claim is treated as "
            "pending rather than silently final "
            "or prematurely rolled back"
        )

    finally:
        h.blake_cli(
            "reconsiderblock",
            blockhash,
        )



def test_final_blake_claim_reappears_after_rollback():
    banner(
        "REORG: FINAL BLAKE CLAIM "
        "REAPPEARS AFTER DURABLE ROLLBACK"
    )

    swap_id = settle_happy_through_blake_claim()
    print("swap:", swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    txid = bob["blake_claim_txid"]
    blockhash = bob["blake_claim_blockhash"]

    h.blake_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        #
        # The disconnected transaction normally returns to mempool.
        # Remove it temporarily by restarting semantics in the harness
        # are awkward, so model the exact durable state produced by the
        # recovery path after the claim has vanished.
        #
        state = h.load_state(
            swap_id,
            "bob",
        )

        state["blake_reorg_claim_txid"] = txid
        state["blake_claim_txid"] = None
        state["blake_claim_blockhash"] = None
        state["state"] = "BTC_CLAIMED"

        state_path = (
            h.BOB_DIR
            / f"{swap_id}.json"
        )

        state_path.write_text(
            json.dumps(
                state,
                indent=2,
            )
            + "\n"
        )

        #
        # The old exact claim is already present in mempool after the
        # invalidateblock above. claim-blake must recognize it rather
        # than attempting a competing spend.
        #
        proc = h.swap_cmd(
            "claim-blake",
            swap_id,
            role="bob",
        )

        text = output_of(proc)

        h.require(
            "BLAKE CLAIM PENDING AFTER REORG"
            in text,
            (
                "reappeared claim was not recognized:\n"
                + text[-1600:]
            ),
        )

        h.require(
            "exact reorged BLAKE claim"
            in text,
            (
                "reappeared claim did not report "
                "exact-transaction recovery:\n"
                + text[-1600:]
            ),
        )

        after = h.load_state(
            swap_id,
            "bob",
        )

        h.require(
            after["state"] == "BTC_CLAIMED",
            "pending reorged claim advanced durable state",
        )

        h.require(
            after["blake_claim_txid"] == txid,
            "pending reorged claim txid was not restored",
        )

        h.require(
            after["blake_claim_blockhash"] is None,
            "pending reorged claim incorrectly has a blockhash",
        )

        h.require(
            after.get("blake_reorg_claim_txid") is None,
            "temporary reorg claim hint was not consumed",
        )

        ok(
            "wallet-rebroadcast final BLAKE claim "
            "is recognized without competing spend"
        )

    finally:
        h.blake_cli(
            "reconsiderblock",
            blockhash,
        )


def test_final_blake_refund_pending_reorg():
    banner(
        "REORG: FINAL BLAKE REFUND "
        "PENDING AFTER ROLLBACK"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    blocks = (
        int(bob["btc_timeout"])
        - int(
            h.btc_cli(
                "getblockcount"
            )
        )
    )

    if blocks > 0:
        h.mine_btc(blocks)

    h.swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "publish-btc-refunded",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "receive-btc-refunded",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    blocks = (
        int(alice["blake_timeout"])
        - int(
            h.blake_cli(
                "getblockcount"
            )
        )
    )

    if blocks > 0:
        h.mine_blake(blocks)

    h.swap_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    txid = alice[
        "blake_refund_txid"
    ]

    blockhash = alice[
        "blake_refund_blockhash"
    ]

    h.blake_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        entry = h.blake_cli(
            "getmempoolentry",
            txid,
        )

        h.require(
            bool(entry),
            "reorged final BLAKE refund "
            "did not return to mempool",
        )

        proc = h.swap_cmd(
            "sync-settlement",
            swap_id,
            "--role",
            "alice",
            role="alice",
        )

        h.require(
            (
                "FINAL BLAKE REFUND "
                "PENDING AFTER REORG"
                in proc.stdout
            ),
            (
                "Alice did not report pending "
                "final refund:\n"
                + proc.stdout[-1600:]
            ),
        )

        state = h.load_state(
            swap_id,
            "alice",
        )

        h.require(
            state["state"]
            == "BLAKE_REFUNDED",
            "Alice incorrectly rolled "
            "BLAKE_REFUNDED backward while "
            "exact refund remained pending",
        )

        ok(
            "final BLAKE refund is treated as "
            "pending rather than silently final "
            "or prematurely rolled back"
        )

    finally:
        h.blake_cli(
            "reconsiderblock",
            blockhash,
        )



def test_final_blake_refund_reappears_after_rollback():
    banner(
        "REORG: FINAL BLAKE REFUND "
        "REAPPEARS AFTER DURABLE ROLLBACK"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    blocks = (
        int(bob["btc_timeout"])
        - int(h.btc_cli("getblockcount"))
    )

    if blocks > 0:
        h.mine_btc(blocks)

    h.swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "publish-btc-refunded",
        swap_id,
        role="bob",
    )

    h.swap_cmd(
        "receive-btc-refunded",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    blocks = (
        int(alice["blake_timeout"])
        - int(h.blake_cli("getblockcount"))
    )

    if blocks > 0:
        h.mine_blake(blocks)

    h.swap_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    txid = alice["blake_refund_txid"]
    blockhash = alice["blake_refund_blockhash"]

    h.blake_cli(
        "invalidateblock",
        blockhash,
    )

    try:
        #
        # invalidateblock returns the exact old refund to mempool.
        #
        entry = h.blake_cli(
            "getmempoolentry",
            txid,
        )

        h.require(
            bool(entry),
            "reorged BLAKE refund did not return to mempool",
        )

        #
        # Model the authoritative durable rollback:
        # Bitcoin refund remains confirmed, while the final
        # BLAKE refund is no longer confirmed.
        #
        state = h.load_state(
            swap_id,
            "alice",
        )

        state["blake_reorg_refund_txid"] = txid
        state["blake_refund_txid"] = None
        state["blake_refund_blockhash"] = None
        state["state"] = "BTC_REFUNDED"

        state_path = (
            h.ALICE_DIR
            / f"{swap_id}.json"
        )

        state_path.write_text(
            json.dumps(
                state,
                indent=2,
            )
            + "\n"
        )

        proc = h.swap_cmd(
            "refund-blake",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        text = output_of(proc)

        h.require(
            "BLAKE REFUND PENDING AFTER REORG"
            in text,
            (
                "reappeared refund was not recognized:\n"
                + text[-1600:]
            ),
        )

        after = h.load_state(
            swap_id,
            "alice",
        )

        h.require(
            after["state"] == "BTC_REFUNDED",
            "pending reorged refund advanced durable state",
        )

        h.require(
            after["blake_refund_txid"] == txid,
            "pending reorged refund txid was not restored",
        )

        h.require(
            after["blake_refund_blockhash"] is None,
            "pending reorged refund incorrectly has a blockhash",
        )

        ok(
            "wallet-rebroadcast final BLAKE refund "
            "is recognized without competing spend"
        )

    finally:
        h.blake_cli(
            "reconsiderblock",
            blockhash,
        )


def main():
    global FAIL

    started = time.time()

    h.preflight()
    ok("preflight")

    tests = [
        (
            "BLAKE FUNDING REORG",
            test_blake_funding_reorg,
        ),
        (
            "BTC FUNDING REORG",
            test_btc_funding_reorg,
        ),
        (
            "BTC CLAIM REORG",
            test_btc_claim_reorg,
        ),
        (
            "BTC REFUND REORG",
            test_btc_refund_reorg,
        ),
        (
            "FINAL BLAKE CLAIM REORG",
            test_final_blake_claim_pending_reorg,
        ),
        (
            "FINAL BLAKE CLAIM REAPPEARS",
            test_final_blake_claim_reappears_after_rollback,
        ),
        (
            "FINAL BLAKE REFUND REORG",
            test_final_blake_refund_pending_reorg,
        ),
        (
            "FINAL BLAKE REFUND REAPPEARS",
            test_final_blake_refund_reappears_after_rollback,
        ),
    ]

    for name, fn in tests:
        try:
            fn()
        except Exception as e:
            fail(
                name,
                str(e),
            )

    elapsed = time.time() - started

    banner("REORG SAFETY SUMMARY")
    print("passes:  ", PASS)
    print("failures:", FAIL)
    print(f"elapsed:  {elapsed:.1f}s")
    print()
    print(
        "Local regtest only. "
        "File transport only."
    )

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
