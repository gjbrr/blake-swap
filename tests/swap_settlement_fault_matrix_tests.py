#!/usr/bin/env python3
"""
Systematic settlement crash/fault matrix.

For every economic settlement operation:

  claim-btc
  claim-blake
  refund-btc
  refund-blake

crash at three durability boundaries:

  1. candidate transaction durably saved, before broadcast
  2. transaction broadcast, before confirmation handling
  3. confirmation proven / in-memory transition performed,
     before final durable state save

After every deliberate process death, restart must converge to the
same exact economic transaction identity without producing a competing
HTLC spend.
"""

import time

import swap_protocol_tests as h


PASS = 0
FAIL = 0
CRASH_EXIT = 97


def ok(message):
    global PASS
    PASS += 1
    print("PASS ", message)


def fail(message, detail):
    global FAIL
    FAIL += 1
    print("FAIL ", message)
    print("     ", detail)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def exact_mempool_spenders(
    chain,
    funding_txid,
    funding_vout,
):
    cli = (
        h.btc_cli
        if chain == "btc"
        else h.blake_cli
    )

    matches = []

    for txid in cli("getrawmempool"):
        tx = cli(
            "getrawtransaction",
            txid,
            True,
        )

        for vin in tx.get("vin", []):
            if (
                vin.get("txid") == funding_txid
                and int(vin.get("vout", -1))
                == int(funding_vout)
            ):
                matches.append(txid)
                break

    return matches


def mine(chain, blocks=1):
    if chain == "btc":
        return h.mine_btc(blocks)

    return h.mine_blake(blocks)


def setup_two_funded():
    swap_id = h.make_fresh_swap()

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    return swap_id


def setup_btc_claim():
    return setup_two_funded()


def setup_blake_claim():
    swap_id = setup_two_funded()

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

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BTC_CLAIMED",
        "Bob did not reach BTC_CLAIMED",
    )

    require(
        bool(bob.get("revealed_secret_hex")),
        "Bob did not recover S from Bitcoin",
    )

    return swap_id


def setup_btc_refund():
    swap_id = setup_two_funded()

    bob = h.load_state(
        swap_id,
        "bob",
    )

    height = int(
        h.btc_cli("getblockcount")
    )

    blocks = (
        int(bob["btc_timeout"])
        - height
    )

    if blocks > 0:
        h.mine_btc(blocks)

    return swap_id


def setup_blake_refund():
    #
    # Abort-before-BTC-funding path gives us the cleanest independent
    # BLAKE refund setup and also re-proves Bitcoin funding absence.
    #
    swap_id = h.make_fresh_swap()

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)

    alice = h.load_state(
        swap_id,
        "alice",
    )

    height = int(
        h.blake_cli("getblockcount")
    )

    blocks = (
        int(alice["blake_timeout"])
        - height
    )

    if blocks > 0:
        h.mine_blake(blocks)

    return swap_id


ACTIONS = [
    {
        "label": "BTC claim",
        "command": "claim-btc",
        "role": "alice",
        "chain": "btc",
        "setup": setup_btc_claim,
        "starting_state": "BTC_FUNDED",
        "final_state": "BTC_CLAIMED",
        "funding_txid": "btc_funding_txid",
        "funding_vout": "btc_funding_vout",
        "txid": "btc_claim_txid",
        "blockhash": "btc_claim_blockhash",
        "hex": "btc_claim_hex",
        "points": {
            "candidate": (
                "after_btc_claim_candidate_save_before_broadcast"
            ),
            "broadcast": "after_btc_claim_broadcast",
            "confirmed": (
                "after_btc_claim_confirmation_before_state_save"
            ),
        },
    },
    {
        "label": "BLAKE claim",
        "command": "claim-blake",
        "role": "bob",
        "chain": "blake",
        "setup": setup_blake_claim,
        "starting_state": "BTC_CLAIMED",
        "final_state": "BLAKE_CLAIMED",
        "funding_txid": "blake_funding_txid",
        "funding_vout": "blake_funding_vout",
        "txid": "blake_claim_txid",
        "blockhash": "blake_claim_blockhash",
        "hex": "blake_claim_hex",
        "points": {
            "candidate": (
                "after_blake_claim_candidate_save_before_broadcast"
            ),
            "broadcast": "after_blake_claim_broadcast",
            "confirmed": (
                "after_blake_claim_confirmation_before_state_save"
            ),
        },
    },
    {
        "label": "BTC refund",
        "command": "refund-btc",
        "role": "bob",
        "chain": "btc",
        "setup": setup_btc_refund,
        "starting_state": "BTC_FUNDED",
        "final_state": "BTC_REFUNDED",
        "funding_txid": "btc_funding_txid",
        "funding_vout": "btc_funding_vout",
        "txid": "btc_refund_txid",
        "blockhash": "btc_refund_blockhash",
        "hex": "btc_refund_hex",
        "points": {
            "candidate": (
                "after_btc_refund_candidate_save_before_broadcast"
            ),
            "broadcast": "after_btc_refund_broadcast",
            "confirmed": (
                "after_btc_refund_confirmation_before_state_save"
            ),
        },
    },
    {
        "label": "BLAKE refund",
        "command": "refund-blake",
        "role": "alice",
        "chain": "blake",
        "setup": setup_blake_refund,
        "starting_state": "BLAKE_FUNDED",
        "final_state": "BLAKE_REFUNDED",
        "funding_txid": "blake_funding_txid",
        "funding_vout": "blake_funding_vout",
        "txid": "blake_refund_txid",
        "blockhash": "blake_refund_blockhash",
        "hex": "blake_refund_hex",
        "points": {
            "candidate": (
                "after_blake_refund_candidate_save_before_broadcast"
            ),
            "broadcast": "after_blake_refund_broadcast",
            "confirmed": (
                "after_blake_refund_confirmation_before_state_save"
            ),
        },
    },
]


def crash_command(
    action,
    swap_id,
    failpoint,
    *,
    auto_mine,
):
    proc = h.swap_cmd(
        action["command"],
        swap_id,
        role=action["role"],
        expect_ok=False,
        extra_env={
            "BLAKE_SWAP_FAILPOINT": failpoint,
            "BLAKE_SWAP_AUTO_MINE": (
                "1"
                if auto_mine
                else "0"
            ),
        },
    )

    require(
        proc.returncode == CRASH_EXIT,
        (
            f"{action['label']} {failpoint}: "
            f"expected deliberate exit {CRASH_EXIT}, "
            f"got {proc.returncode}\\n"
            f"stdout:\\n{proc.stdout[-2000:]}\\n"
            f"stderr:\\n{proc.stderr[-2000:]}"
        ),
    )

    require(
        f"TEST FAILPOINT TRIGGERED: {failpoint}"
        in (proc.stdout or ""),
        (
            f"{action['label']} {failpoint}: "
            "process exited without named failpoint evidence"
        ),
    )


def assert_candidate_durable(
    action,
    swap_id,
):
    state = h.load_state(
        swap_id,
        action["role"],
    )

    require(
        state["state"] == action["starting_state"],
        (
            f"{action['label']}: crash advanced durable "
            f"state to {state['state']}"
        ),
    )

    txid = state.get(action["txid"])

    require(
        bool(txid),
        (
            f"{action['label']}: crash lost durable "
            "candidate txid"
        ),
    )

    require(
        bool(state.get(action["hex"])),
        (
            f"{action['label']}: crash lost durable "
            "signed transaction"
        ),
    )

    require(
        state.get(action["blockhash"]) is None,
        (
            f"{action['label']}: pre-final durable state "
            "unexpectedly has blockhash"
        ),
    )

    return state, txid


def finish_from_pending(
    action,
    swap_id,
    txid,
):
    before = h.load_state(
        swap_id,
        action["role"],
    )

    funding_txid = before[
        action["funding_txid"]
    ]
    funding_vout = before[
        action["funding_vout"]
    ]

    spenders = exact_mempool_spenders(
        action["chain"],
        funding_txid,
        funding_vout,
    )

    require(
        spenders == [txid],
        (
            f"{action['label']}: expected exact durable "
            f"transaction in mempool, found {spenders}; "
            f"durable={txid}"
        ),
    )

    mine(
        action["chain"],
        1,
    )

    h.swap_cmd(
        action["command"],
        swap_id,
        role=action["role"],
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    final = h.load_state(
        swap_id,
        action["role"],
    )

    require(
        final["state"] == action["final_state"],
        (
            f"{action['label']}: restart did not converge; "
            f"state={final['state']}"
        ),
    )

    require(
        final.get(action["txid"]) == txid,
        (
            f"{action['label']}: restart changed economic "
            "transaction identity"
        ),
    )

    require(
        bool(final.get(action["blockhash"])),
        (
            f"{action['label']}: converged state has no "
            "confirmed blockhash"
        ),
    )


def run_candidate_crash(action):
    swap_id = action["setup"]()

    crash_command(
        action,
        swap_id,
        action["points"]["candidate"],
        auto_mine=False,
    )

    state, txid = assert_candidate_durable(
        action,
        swap_id,
    )

    funding_txid = state[
        action["funding_txid"]
    ]
    funding_vout = state[
        action["funding_vout"]
    ]

    spenders = exact_mempool_spenders(
        action["chain"],
        funding_txid,
        funding_vout,
    )

    require(
        spenders == [],
        (
            f"{action['label']}: pre-broadcast crash "
            f"already moved money: {spenders}"
        ),
    )

    #
    # Restart without auto-mining. Recovery must independently verify
    # and rebroadcast the exact durable transaction.
    #
    h.swap_cmd(
        action["command"],
        swap_id,
        role=action["role"],
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    finish_from_pending(
        action,
        swap_id,
        txid,
    )

    ok(
        f"{action['label']} recovers from candidate-save/pre-broadcast crash"
    )


def run_broadcast_crash(action):
    swap_id = action["setup"]()

    crash_command(
        action,
        swap_id,
        action["points"]["broadcast"],
        auto_mine=False,
    )

    state, txid = assert_candidate_durable(
        action,
        swap_id,
    )

    spenders = exact_mempool_spenders(
        action["chain"],
        state[action["funding_txid"]],
        state[action["funding_vout"]],
    )

    require(
        spenders == [txid],
        (
            f"{action['label']}: post-broadcast crash "
            f"mempool mismatch: {spenders}; durable={txid}"
        ),
    )

    #
    # A restart while still pending must be a safe no-op/recovery,
    # preserving exactly the same transaction.
    #
    h.swap_cmd(
        action["command"],
        swap_id,
        role=action["role"],
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    finish_from_pending(
        action,
        swap_id,
        txid,
    )

    ok(
        f"{action['label']} recovers from post-broadcast crash"
    )


def run_confirmed_crash(action):
    swap_id = action["setup"]()

    crash_command(
        action,
        swap_id,
        action["points"]["confirmed"],
        auto_mine=True,
    )

    state, txid = assert_candidate_durable(
        action,
        swap_id,
    )

    #
    # It was mined before the deliberate crash, so the exact transaction
    # must now be independently visible as confirmed even though durable
    # protocol state still reflects the old checkpoint.
    #
    cli = (
        h.btc_cli
        if action["chain"] == "btc"
        else h.blake_cli
    )

    wallet = (
        h.ALICE_WALLET
        if action["role"] == "alice"
        else h.BOB_WALLET
    )

    #
    # Do not require txindex here. Once the settlement leaves the
    # mempool, getrawtransaction(txid) without a blockhash is not
    # available on an ordinary node. The signing wallet can still
    # independently prove its own exact transaction and confirmation.
    #
    tx = cli(
        "gettransaction",
        txid,
        wallet=wallet,
    )

    require(
        int(tx.get("confirmations", 0) or 0) >= 1,
        (
            f"{action['label']}: confirmation failpoint "
            "did not leave exact settlement confirmed"
        ),
    )

    require(
        bool(tx.get("blockhash")),
        (
            f"{action['label']}: confirmed wallet transaction "
            "has no blockhash"
        ),
    )

    #
    # Restart must recover that chain fact and perform the previously
    # interrupted durable transition without building another spend.
    #
    h.swap_cmd(
        action["command"],
        swap_id,
        role=action["role"],
        extra_env={
            "BLAKE_SWAP_AUTO_MINE": "0",
        },
    )

    final = h.load_state(
        swap_id,
        action["role"],
    )

    require(
        final["state"] == action["final_state"],
        (
            f"{action['label']}: confirmed crash recovery "
            f"ended in {final['state']}"
        ),
    )

    require(
        final.get(action["txid"]) == txid,
        (
            f"{action['label']}: confirmed crash recovery "
            "changed settlement txid"
        ),
    )

    require(
        bool(final.get(action["blockhash"])),
        (
            f"{action['label']}: confirmed crash recovery "
            "did not persist blockhash"
        ),
    )

    ok(
        f"{action['label']} recovers from confirmation/pre-save crash"
    )


def main():
    global PASS, FAIL

    started = time.time()

    h.preflight()
    ok("preflight")

    for action in ACTIONS:
        h.banner(
            "FAULT MATRIX: "
            + action["label"].upper()
        )

        tests = [
            (
                "candidate-save/pre-broadcast",
                run_candidate_crash,
            ),
            (
                "post-broadcast",
                run_broadcast_crash,
            ),
            (
                "confirmation/pre-final-save",
                run_confirmed_crash,
            ),
        ]

        for name, fn in tests:
            try:
                print()
                print(
                    f"--- {action['label']}: {name} ---"
                )
                fn(action)

            except Exception as exc:
                fail(
                    f"{action['label']} {name}",
                    str(exc),
                )

    h.banner("SETTLEMENT FAULT MATRIX SUMMARY")
    print("passes:  ", PASS)
    print("failures:", FAIL)
    print(
        "elapsed:  ",
        f"{time.time() - started:.1f}s",
    )

    print()
    print(
        "Crash windows exercised:",
        len(ACTIONS) * 3,
    )

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
