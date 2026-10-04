#!/usr/bin/env python3

import sys
import time
from pathlib import Path

import swap_protocol_tests as h


CRASH_EXIT = 97


CASES = (
    {
        "name": "BTC CLAIM",
        "command": "claim-btc",
        "role": "alice",
        "chain": "btc",
        "branch": "claim",
        "source_state": "BTC_FUNDED",
        "target_state": "BTC_CLAIMED",
        "candidate_point": "after_btc_claim_candidate_save_before_broadcast",
        "broadcast_point": "after_btc_claim_broadcast",
        "confirm_point": "after_btc_claim_confirmation_before_state_save",
    },
    {
        "name": "BLAKE CLAIM",
        "command": "claim-blake",
        "role": "bob",
        "chain": "blake",
        "branch": "claim",
        "source_state": "BTC_CLAIMED",
        "target_state": "BLAKE_CLAIMED",
        "candidate_point": "after_blake_claim_candidate_save_before_broadcast",
        "broadcast_point": "after_blake_claim_broadcast",
        "confirm_point": "after_blake_claim_confirmation_before_state_save",
    },
    {
        "name": "BTC REFUND",
        "command": "refund-btc",
        "role": "bob",
        "chain": "btc",
        "branch": "refund",
        "source_state": "BTC_FUNDED",
        "target_state": "BTC_REFUNDED",
        "candidate_point": "after_btc_refund_candidate_save_before_broadcast",
        "broadcast_point": "after_btc_refund_broadcast",
        "confirm_point": "after_btc_refund_confirmation_before_state_save",
    },
    {
        "name": "BLAKE REFUND",
        "command": "refund-blake",
        "role": "alice",
        "chain": "blake",
        "branch": "refund",
        "source_state": "BLAKE_FUNDED",
        "target_state": "BLAKE_REFUNDED",
        "candidate_point": "after_blake_refund_candidate_save_before_broadcast",
        "broadcast_point": "after_blake_refund_broadcast",
        "confirm_point": "after_blake_refund_confirmation_before_state_save",
    },
)


def cli_for(chain):
    return h.btc_cli if chain == "btc" else h.blake_cli


def mine_for(chain, blocks=1):
    if chain == "btc":
        return h.mine_btc(blocks)
    return h.mine_blake(blocks)


def setup_case(case):
    swap_id = h.make_fresh_swap()
    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)

    if case["branch"] == "claim":
        h.fund_btc_and_sync(swap_id)

        if case["chain"] == "blake":
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

    elif case["chain"] == "btc":
        h.fund_btc_and_sync(swap_id)

        state = h.load_state(
            swap_id,
            "bob",
        )

        current = int(
            h.btc_cli("getblockcount")
        )
        remaining = int(state["btc_timeout"]) - current

        if remaining > 0:
            h.mine_btc(remaining)

    else:
        #
        # Exercise Alice's abort-before-BTC-funding refund.
        # This keeps the BLAKE refund crash matrix independent
        # from the BTC-refund settlement path.
        #
        state = h.load_state(
            swap_id,
            "alice",
        )

        current = int(
            h.blake_cli("getblockcount")
        )
        remaining = int(state["blake_timeout"]) - current

        if remaining > 0:
            h.mine_blake(remaining)

    h.assert_state(
        swap_id,
        case["role"],
        case["source_state"],
    )

    return swap_id


def settlement_fields(case):
    prefix = (
        f"{case['chain']}_{case['branch']}"
    )

    return (
        f"{prefix}_txid",
        f"{prefix}_blockhash",
        f"{prefix}_hex",
    )


def assert_durable_candidate(case, swap_id):
    txid_field, block_field, hex_field = (
        settlement_fields(case)
    )

    state = h.load_state(
        swap_id,
        case["role"],
    )

    h.require(
        state["state"] == case["source_state"],
        (
            f"{case['name']}: crash advanced durable state "
            f"to {state['state']}"
        ),
    )

    txid = state.get(txid_field)
    raw_hex = state.get(hex_field)

    h.require(
        bool(txid),
        f"{case['name']}: crash lost durable settlement txid",
    )

    h.require(
        bool(raw_hex),
        f"{case['name']}: crash lost durable signed transaction",
    )

    h.require(
        state.get(block_field) is None,
        (
            f"{case['name']}: pre-final-save state unexpectedly "
            "contains authoritative blockhash"
        ),
    )

    cli = cli_for(case["chain"])

    decoded = cli(
        "decoderawtransaction",
        raw_hex,
    )

    h.require(
        decoded.get("txid") == txid,
        (
            f"{case['name']}: durable signed transaction does "
            "not match durable txid"
        ),
    )

    funding_txid = state.get(
        f"{case['chain']}_funding_txid"
    )
    funding_vout = state.get(
        f"{case['chain']}_funding_vout"
    )

    exact_inputs = [
        vin
        for vin in decoded.get("vin", [])
        if (
            vin.get("txid") == funding_txid
            and int(vin.get("vout", -1))
            == int(funding_vout)
        )
    ]

    h.require(
        len(exact_inputs) == 1,
        (
            f"{case['name']}: durable settlement candidate "
            "does not spend exact HTLC outpoint once"
        ),
    )

    return txid, raw_hex


def mempool_contains(case, txid):
    cli = cli_for(case["chain"])
    return txid in cli("getrawmempool")


def locate_confirmed_tx(case, txid):
    """
    Locate our locally-created settlement without requiring txindex.

    getrawtransaction(txid, true) cannot find an arbitrary confirmed
    transaction on a Bitcoin Core node with txindex disabled.  The
    signing participant's wallet can give us its blockhash; once that
    anchor is known, getrawtransaction(txid, true, blockhash) performs
    the exact active-block lookup we actually want to test.
    """
    cli = cli_for(case["chain"])

    wallet = (
        h.ALICE_WALLET
        if case["role"] == "alice"
        else h.BOB_WALLET
    )

    wallet_tx = cli(
        "gettransaction",
        txid,
        wallet=wallet,
    )

    blockhash = wallet_tx.get("blockhash")

    h.require(
        bool(blockhash),
        f"{case['name']}: wallet has no confirmed blockhash for settlement",
    )

    tx = cli(
        "getrawtransaction",
        txid,
        True,
        blockhash,
    )

    h.require(
        tx.get("txid") == txid,
        f"{case['name']}: anchored transaction lookup returned wrong txid",
    )

    h.require(
        int(tx.get("confirmations", 0)) >= 1,
        f"{case['name']}: canonical settlement not confirmed",
    )

    h.require(
        tx.get("blockhash") == blockhash,
        f"{case['name']}: transaction block anchor mismatch",
    )

    return tx, blockhash


def assert_confirmed(case, txid):
    _, blockhash = locate_confirmed_tx(
        case,
        txid,
    )

    return blockhash


def crash_at(case, swap_id, point):
    proc = h.swap_cmd(
        case["command"],
        swap_id,
        role=case["role"],
        expect_ok=False,
        extra_env={
            "BLAKE_SWAP_FAILPOINT": point,
            "BLAKE_SWAP_AUTO_MINE": "1",
        },
    )

    h.require(
        proc.returncode == CRASH_EXIT,
        (
            f"{case['name']} {point}: expected exit "
            f"{CRASH_EXIT}, got {proc.returncode}\n"
            f"stdout:\n{proc.stdout[-1500:]}\n"
            f"stderr:\n{proc.stderr[-1500:]}"
        ),
    )


def finish_pending(case, swap_id, canonical_txid):
    #
    # Restart without automatic mining. This forces recovery to
    # recognize/rebroadcast the exact durable candidate first.
    #
    proc = h.swap_cmd(
        case["command"],
        swap_id,
        role=case["role"],
        expect_ok=False,
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    h.require(
        proc.returncode == 0,
        (
            f"{case['name']}: restart failed while recovering "
            f"durable settlement\nstdout:\n{proc.stdout[-1500:]}\n"
            f"stderr:\n{proc.stderr[-1500:]}"
        ),
    )

    pending = h.load_state(
        swap_id,
        case["role"],
    )

    txid_field, block_field, _ = settlement_fields(case)

    h.require(
        pending.get(txid_field) == canonical_txid,
        f"{case['name']}: restart changed canonical txid",
    )

    h.require(
        pending["state"] == case["source_state"],
        (
            f"{case['name']}: unconfirmed restart advanced state "
            f"to {pending['state']}"
        ),
    )

    h.require(
        pending.get(block_field) is None,
        (
            f"{case['name']}: unconfirmed restart persisted "
            "a settlement blockhash"
        ),
    )

    h.require(
        mempool_contains(
            case,
            canonical_txid,
        ),
        (
            f"{case['name']}: exact durable settlement was not "
            "present in mempool after restart"
        ),
    )

    #
    # External block production confirms the exact recovered tx.
    #
    mine_for(
        case["chain"],
        1,
    )

    h.swap_cmd(
        case["command"],
        swap_id,
        role=case["role"],
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    final = h.load_state(
        swap_id,
        case["role"],
    )

    h.require(
        final["state"] == case["target_state"],
        (
            f"{case['name']}: confirmation recovery ended in "
            f"{final['state']}, expected {case['target_state']}"
        ),
    )

    h.require(
        final.get(txid_field) == canonical_txid,
        (
            f"{case['name']}: confirmation recovery replaced "
            "canonical txid"
        ),
    )

    expected_block = assert_confirmed(
        case,
        canonical_txid,
    )

    h.require(
        final.get(block_field) == expected_block,
        (
            f"{case['name']}: durable blockhash does not match "
            "active-chain confirmation"
        ),
    )


def finish_confirmed_crash(case, swap_id, canonical_txid):
    #
    # The transaction was already mined before the injected crash.
    # Restart must discover that exact active-chain spend and advance
    # durable protocol state without constructing another settlement.
    #
    expected_block = assert_confirmed(
        case,
        canonical_txid,
    )

    h.require(
        not mempool_contains(
            case,
            canonical_txid,
        ),
        (
            f"{case['name']}: confirmed transaction unexpectedly "
            "remains in mempool"
        ),
    )

    h.swap_cmd(
        case["command"],
        swap_id,
        role=case["role"],
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    final = h.load_state(
        swap_id,
        case["role"],
    )

    txid_field, block_field, _ = settlement_fields(case)

    h.require(
        final["state"] == case["target_state"],
        (
            f"{case['name']}: restart after confirmed crash ended "
            f"in {final['state']}"
        ),
    )

    h.require(
        final.get(txid_field) == canonical_txid,
        (
            f"{case['name']}: restart after confirmation "
            "changed canonical txid"
        ),
    )

    h.require(
        final.get(block_field) == expected_block,
        (
            f"{case['name']}: restart after confirmation "
            "recorded wrong blockhash"
        ),
    )


def test_boundary(case, boundary, point):
    h.banner(
        f"CRASH MATRIX: {case['name']} / {boundary}"
    )

    swap_id = setup_case(case)
    print("swap:", swap_id)
    print("failpoint:", point)

    crash_at(
        case,
        swap_id,
        point,
    )

    canonical_txid, _ = assert_durable_candidate(
        case,
        swap_id,
    )

    if boundary == "candidate-before-broadcast":
        h.require(
            not mempool_contains(
                case,
                canonical_txid,
            ),
            (
                f"{case['name']}: candidate appeared in mempool "
                "before broadcast boundary"
            ),
        )

        finish_pending(
            case,
            swap_id,
            canonical_txid,
        )

    elif boundary == "after-broadcast":
        h.require(
            mempool_contains(
                case,
                canonical_txid,
            ),
            (
                f"{case['name']}: broadcast transaction missing "
                "from mempool after crash"
            ),
        )

        finish_pending(
            case,
            swap_id,
            canonical_txid,
        )

    elif boundary == "confirmation-before-save":
        finish_confirmed_crash(
            case,
            swap_id,
            canonical_txid,
        )

    else:
        raise h.TestFailure(
            f"unknown boundary: {boundary}"
        )

    #
    # One more retry after final settlement must not alter identity.
    #
    before = h.load_state(
        swap_id,
        case["role"],
    )

    retry = h.swap_cmd(
        case["command"],
        swap_id,
        role=case["role"],
        expect_ok=False,
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    after = h.load_state(
        swap_id,
        case["role"],
    )

    txid_field, block_field, _ = settlement_fields(case)

    h.require(
        after.get(txid_field) == before.get(txid_field),
        (
            f"{case['name']}: settled retry changed "
            "canonical txid"
        ),
    )

    h.require(
        after.get(block_field) == before.get(block_field),
        (
            f"{case['name']}: settled retry changed "
            "canonical blockhash"
        ),
    )

    h.require(
        after["state"] == before["state"],
        (
            f"{case['name']}: settled retry changed "
            "terminal settlement state"
        ),
    )

    h.ok(
        f"{case['name']} {boundary} recovered exact txid "
        + canonical_txid[:12]
    )


def main():
    started = time.time()

    try:
        h.preflight()
        h.ok("preflight")

        for case in CASES:
            test_boundary(
                case,
                "candidate-before-broadcast",
                case["candidate_point"],
            )

            test_boundary(
                case,
                "after-broadcast",
                case["broadcast_point"],
            )

            test_boundary(
                case,
                "confirmation-before-save",
                case["confirm_point"],
            )

    except Exception as e:
        h.fail(
            "SETTLEMENT CRASH MATRIX",
            str(e),
        )

    elapsed = time.time() - started

    h.banner("SUMMARY")
    print(f"passes:  {h.PASS_COUNT}")
    print(f"failures:{h.FAIL_COUNT}")
    print(f"elapsed: {elapsed:.1f}s")

    return 1 if h.FAIL_COUNT else 0


if __name__ == "__main__":
    raise SystemExit(main())
