#!/usr/bin/env python3

import sys
import time
from pathlib import Path

import swap_protocol_tests as h


def banner(title):
    print()
    print("=" * 72)
    print(title)
    print("=" * 72)


def prod_cmd(*args, role, expect_ok=True):
    """
    Run one swap command with production confirmation semantics while
    otherwise mirroring the canonical test harness participant env.
    """
    env = {
        "BLAKE_SWAP_AUTO_MINE": "0",
        "BLAKE_SWAP_NOSTR_RELAYS": "ws://127.0.0.1:1",
        "BLAKE_SWAP_MESSAGE_DIR": str(h.MESSAGE_DIR),
        "BLAKE_SWAP_ROLE": role,
    }

    if role == "alice":
        env["BLAKE_SWAP_STATE_DIR"] = str(h.ALICE_BASE)
        env["BLAKE_SWAP_CHAIN_WALLET"] = h.ALICE_WALLET
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.ALICE_PARTICIPANT_KEY
        )

    elif role == "bob":
        env["BLAKE_SWAP_STATE_DIR"] = str(h.BOB_BASE)
        env["BLAKE_SWAP_CHAIN_WALLET"] = h.BOB_WALLET
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.BOB_PARTICIPANT_KEY
        )

    else:
        raise h.TestFailure(
            f"unknown swap role: {role}"
        )

    return h.run(
        [sys.executable, h.SWAP_V2, *args],
        env=env,
        expect_ok=expect_ok,
    )


def require_pending(
    *,
    state,
    expected_state,
    tx_field,
    block_field,
    label,
):
    h.require(
        state.get("state") == expected_state,
        (
            f"unconfirmed {label} advanced protocol state: "
            f"{state.get('state')!r}"
        ),
    )

    txid = state.get(tx_field)

    h.require(
        txid,
        f"pending {label} did not persist txid",
    )

    h.require(
        state.get(block_field) is None,
        f"pending {label} unexpectedly recorded blockhash",
    )

    return txid


def require_same_pending(
    *,
    state,
    expected_state,
    tx_field,
    block_field,
    txid,
    label,
):
    h.require(
        state.get("state") == expected_state,
        f"pending {label} retry changed protocol state",
    )

    h.require(
        state.get(tx_field) == txid,
        f"pending {label} retry changed txid",
    )

    h.require(
        state.get(block_field) is None,
        f"pending {label} retry unexpectedly recorded blockhash",
    )


def require_confirmed(
    *,
    state,
    expected_state,
    tx_field,
    block_field,
    txid,
    label,
):
    h.require(
        state.get("state") == expected_state,
        f"confirmed {label} did not advance protocol state",
    )

    h.require(
        state.get(tx_field) == txid,
        f"confirmed {label} txid changed",
    )

    h.require(
        state.get(block_field),
        f"confirmed {label} did not record blockhash",
    )


def test_pending_btc_claim():
    banner("PRODUCTION BTC CLAIM: PENDING CONFIRMATION")

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    first = prod_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    h.require(
        "awaiting active-chain confirmation"
        in (first.stdout + first.stderr).lower(),
        "first production BTC claim did not report pending confirmation",
    )

    pending = h.load_state(
        swap_id,
        "alice",
    )

    first_txid = require_pending(
        state=pending,
        expected_state="BTC_FUNDED",
        tx_field="btc_claim_txid",
        block_field="btc_claim_blockhash",
        label="BTC claim",
    )

    # Broadcasting S into the mempool is NOT sufficient authority
    # for Bob's durable state.
    bob = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        bob.get("state") == "BTC_FUNDED",
        "Bob advanced from pending BTC claim",
    )

    h.require(
        bob.get("btc_claim_txid") is None,
        "Bob recorded unconfirmed BTC claim as settlement",
    )

    retry = prod_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    h.require(
        "BTC CLAIM PENDING"
        in (retry.stdout + retry.stderr),
        "BTC claim retry did not recognize exact pending transaction",
    )

    require_same_pending(
        state=h.load_state(swap_id, "alice"),
        expected_state="BTC_FUNDED",
        tx_field="btc_claim_txid",
        block_field="btc_claim_blockhash",
        txid=first_txid,
        label="BTC claim",
    )

    h.mine_btc(1)

    prod_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    require_confirmed(
        state=h.load_state(swap_id, "alice"),
        expected_state="BTC_CLAIMED",
        tx_field="btc_claim_txid",
        block_field="btc_claim_blockhash",
        txid=first_txid,
        label="BTC claim",
    )

    print("PASS: production BTC claim waits for confirmation")


def test_pending_blake_claim():
    banner("PRODUCTION BLAKE CLAIM: PENDING CONFIRMATION")

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    # Alice's Bitcoin claim must first become confirmed so Bob can
    # independently receive it and recover S from Bitcoin.
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

    h.assert_state(
        swap_id,
        "bob",
        "BTC_CLAIMED",
    )

    first = prod_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    h.require(
        "awaiting active-chain confirmation"
        in (first.stdout + first.stderr).lower(),
        "first production BLAKE claim did not report pending confirmation",
    )

    pending = h.load_state(
        swap_id,
        "bob",
    )

    first_txid = require_pending(
        state=pending,
        expected_state="BTC_CLAIMED",
        tx_field="blake_claim_txid",
        block_field="blake_claim_blockhash",
        label="BLAKE claim",
    )

    retry = prod_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    h.require(
        "BLAKE CLAIM PENDING"
        in (retry.stdout + retry.stderr),
        "BLAKE claim retry did not recognize exact pending transaction",
    )

    require_same_pending(
        state=h.load_state(swap_id, "bob"),
        expected_state="BTC_CLAIMED",
        tx_field="blake_claim_txid",
        block_field="blake_claim_blockhash",
        txid=first_txid,
        label="BLAKE claim",
    )

    h.mine_blake(1)

    prod_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    require_confirmed(
        state=h.load_state(swap_id, "bob"),
        expected_state="BLAKE_CLAIMED",
        tx_field="blake_claim_txid",
        block_field="blake_claim_blockhash",
        txid=first_txid,
        label="BLAKE claim",
    )

    print("PASS: production BLAKE claim waits for confirmation")


def test_pending_btc_refund():
    banner("PRODUCTION BTC REFUND: PENDING CONFIRMATION")

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    current = int(
        h.btc_cli("getblockcount")
    )

    remaining = (
        int(bob["btc_timeout"])
        - current
    )

    if remaining > 0:
        h.mine_btc(remaining)

    first = prod_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    h.require(
        "awaiting active-chain confirmation"
        in (first.stdout + first.stderr).lower(),
        "first production BTC refund did not report pending confirmation",
    )

    pending = h.load_state(
        swap_id,
        "bob",
    )

    first_txid = require_pending(
        state=pending,
        expected_state="BTC_FUNDED",
        tx_field="btc_refund_txid",
        block_field="btc_refund_blockhash",
        label="BTC refund",
    )

    retry = prod_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    h.require(
        "BTC REFUND PENDING"
        in (retry.stdout + retry.stderr),
        "BTC refund retry did not recognize exact pending transaction",
    )

    require_same_pending(
        state=h.load_state(swap_id, "bob"),
        expected_state="BTC_FUNDED",
        tx_field="btc_refund_txid",
        block_field="btc_refund_blockhash",
        txid=first_txid,
        label="BTC refund",
    )

    h.mine_btc(1)

    prod_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    require_confirmed(
        state=h.load_state(swap_id, "bob"),
        expected_state="BTC_REFUNDED",
        tx_field="btc_refund_txid",
        block_field="btc_refund_blockhash",
        txid=first_txid,
        label="BTC refund",
    )

    print("PASS: production BTC refund waits for confirmation")



def restart_local_blake_without_mempool():
    """
    Restart the local BLAKE regtest daemon with mempool persistence
    disabled.

    This intentionally reproduces the production recovery condition:
    the swap state survives, but the node forgets its pending
    transaction and all wallets start unloaded.

    Remote-node configurations cannot safely be restarted by this
    black-box suite, so they skip this specific regression.
    """

    if h.BLAKE_DATADIR is None:
        h.skip(
            "durable BLAKE claim rebroadcast test needs a local BLAKE datadir"
        )

    if h.BLAKE_RPC_ARGS:
        h.skip(
            "durable BLAKE claim rebroadcast test does not restart remote/custom-RPC BLAKE nodes"
        )

    bitcoind = h.BLAKE_CLI.parent / "bitcoind"

    if not bitcoind.exists():
        h.skip(
            "BLAKE bitcoind executable not found next to bitcoin-cli",
            details=[f"expected: {bitcoind}"],
        )

    h.blake_cli("stop")

    deadline = time.time() + 20

    while time.time() < deadline:
        still_running = False
        datadir_arg = f"-datadir={h.BLAKE_DATADIR}"

        for proc in Path("/proc").iterdir():
            if not proc.name.isdigit():
                continue

            try:
                cmdline = (
                    proc / "cmdline"
                ).read_bytes().replace(
                    b"\x00",
                    b" ",
                ).decode(
                    "utf-8",
                    errors="replace",
                )
            except (OSError, PermissionError):
                continue

            if (
                "bitcoind" in cmdline
                and datadir_arg in cmdline
            ):
                still_running = True
                break

        if not still_running:
            break

        time.sleep(0.2)
    else:
        raise h.TestFailure(
            "BLAKE daemon still holds the test datadir after stop"
        )

    h.run([
        bitcoind,
        "-regtest",
        f"-datadir={h.BLAKE_DATADIR}",
        "-server=1",
        "-fallbackfee=0.0001",
        "-txindex=0",
        "-persistmempool=0",
        "-blake2b_headline=",
        "-daemon",
    ])

    deadline = time.time() + 20

    while time.time() < deadline:
        try:
            h.blake_cli("getblockcount")
            return
        except Exception:
            time.sleep(0.2)

    raise h.TestFailure(
        "BLAKE daemon did not restart"
    )


def test_blake_claim_durable_rebroadcast_after_mempool_loss():
    banner(
        "PRODUCTION BLAKE CLAIM: DURABLE REBROADCAST AFTER MEMPOOL LOSS"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    #
    # Reach Bob's BTC_CLAIMED state with S independently recovered
    # from confirmed Bitcoin.
    #
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

    h.assert_state(
        swap_id,
        "bob",
        "BTC_CLAIMED",
    )

    #
    # Broadcast but do not mine Bob's BLAKE claim.
    #
    first = prod_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    h.require(
        "awaiting active-chain confirmation"
        in (first.stdout + first.stderr).lower(),
        "initial BLAKE claim did not remain pending",
    )

    pending = h.load_state(
        swap_id,
        "bob",
    )

    first_txid = require_pending(
        state=pending,
        expected_state="BTC_CLAIMED",
        tx_field="blake_claim_txid",
        block_field="blake_claim_blockhash",
        label="BLAKE claim",
    )

    signed_hex = pending.get(
        "blake_claim_hex"
    )

    h.require(
        signed_hex,
        "pending BLAKE claim did not persist signed transaction hex",
    )

    decoded = h.blake_cli(
        "decoderawtransaction",
        signed_hex,
    )

    h.require(
        decoded.get("txid") == first_txid,
        "persisted BLAKE claim hex does not match persisted txid",
    )

    h.require(
        h.blake_cli(
            "getmempoolentry",
            first_txid,
        ),
        "initial BLAKE claim is not in the mempool",
    )

    #
    # Destroy node mempool state. Wallets are deliberately NOT
    # reloaded after restart.
    #
    restart_local_blake_without_mempool()

    try:
        h.blake_cli(
            "getmempoolentry",
            first_txid,
        )
    except Exception:
        pass
    else:
        raise h.TestFailure(
            "BLAKE claim unexpectedly survived mempool-loss restart"
        )

    loaded = h.blake_cli(
        "listwallets"
    )

    h.require(
        h.BOB_WALLET not in loaded,
        "Bob BLAKE wallet unexpectedly loaded after restart",
    )

    #
    # This is the property under test: the engine must independently
    # verify the durable signed transaction and rebroadcast that exact
    # transaction without the signing wallet.
    #
    retry = prod_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    output = retry.stdout + retry.stderr

    h.require(
        "BLAKE CLAIM REBROADCAST FROM DURABLE STATE"
        in output,
        "claim-blake did not use durable signed-transaction recovery",
    )

    h.require(
        "PASS: exact stored signed transaction was rebroadcast"
        in output,
        "claim-blake did not report exact durable rebroadcast",
    )

    after = h.load_state(
        swap_id,
        "bob",
    )

    require_same_pending(
        state=after,
        expected_state="BTC_CLAIMED",
        tx_field="blake_claim_txid",
        block_field="blake_claim_blockhash",
        txid=first_txid,
        label="BLAKE claim durable rebroadcast",
    )

    h.require(
        after.get("blake_claim_hex") == signed_hex,
        "durable rebroadcast changed signed BLAKE claim hex",
    )

    mempool = h.blake_cli(
        "getmempoolentry",
        first_txid,
    )

    h.require(
        bool(mempool),
        "exact durable BLAKE claim was not restored to mempool",
    )

    h.ensure_test_wallets()

    print(
        "PASS: BLAKE claim survived mempool loss without signing wallet"
    )



def test_blake_refund_durable_rebroadcast_after_mempool_loss():
    banner(
        "PRODUCTION BLAKE REFUND: DURABLE REBROADCAST AFTER MEMPOOL LOSS"
    )

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    #
    # Reach Alice's BTC_REFUNDED state through Bob's normal
    # confirmed Bitcoin refund.
    #
    bob = h.load_state(
        swap_id,
        "bob",
    )

    current_btc = int(
        h.btc_cli("getblockcount")
    )

    remaining_btc = (
        int(bob["btc_timeout"])
        - current_btc
    )

    if remaining_btc > 0:
        h.mine_btc(remaining_btc)

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

    h.assert_state(
        swap_id,
        "alice",
        "BTC_REFUNDED",
    )

    #
    # Mature Alice's BLAKE refund timelock.
    #
    alice = h.load_state(
        swap_id,
        "alice",
    )

    current_blake = int(
        h.blake_cli("getblockcount")
    )

    remaining_blake = (
        int(alice["blake_timeout"])
        - current_blake
    )

    if remaining_blake > 0:
        h.mine_blake(remaining_blake)

    #
    # Broadcast the refund but deliberately leave it unconfirmed.
    #
    first = prod_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    h.require(
        "awaiting active-chain confirmation"
        in (first.stdout + first.stderr).lower(),
        "initial BLAKE refund did not remain pending",
    )

    pending = h.load_state(
        swap_id,
        "alice",
    )

    first_txid = require_pending(
        state=pending,
        expected_state="BTC_REFUNDED",
        tx_field="blake_refund_txid",
        block_field="blake_refund_blockhash",
        label="BLAKE refund",
    )

    signed_hex = pending.get(
        "blake_refund_hex"
    )

    h.require(
        signed_hex,
        "pending BLAKE refund did not persist signed transaction hex",
    )

    decoded = h.blake_cli(
        "decoderawtransaction",
        signed_hex,
    )

    h.require(
        decoded.get("txid") == first_txid,
        "persisted BLAKE refund hex does not match persisted txid",
    )

    h.require(
        h.blake_cli(
            "getmempoolentry",
            first_txid,
        ),
        "initial BLAKE refund is not in the mempool",
    )

    #
    # Destroy node mempool state. Wallets are deliberately NOT
    # reloaded after restart.
    #
    restart_local_blake_without_mempool()

    try:
        h.blake_cli(
            "getmempoolentry",
            first_txid,
        )
    except Exception:
        pass
    else:
        raise h.TestFailure(
            "BLAKE refund unexpectedly survived mempool-loss restart"
        )

    loaded = h.blake_cli(
        "listwallets"
    )

    h.require(
        h.ALICE_WALLET not in loaded,
        "Alice BLAKE wallet unexpectedly loaded after restart",
    )

    #
    # The engine must verify the durable signed refund and rebroadcast
    # that exact transaction without Alice's signing wallet.
    #
    retry = prod_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    output = retry.stdout + retry.stderr

    h.require(
        "BLAKE REFUND REBROADCAST FROM DURABLE STATE"
        in output,
        "refund-blake did not use durable signed-transaction recovery",
    )

    h.require(
        "PASS: exact stored signed transaction was rebroadcast"
        in output,
        "refund-blake did not report exact durable rebroadcast",
    )

    after = h.load_state(
        swap_id,
        "alice",
    )

    require_same_pending(
        state=after,
        expected_state="BTC_REFUNDED",
        tx_field="blake_refund_txid",
        block_field="blake_refund_blockhash",
        txid=first_txid,
        label="BLAKE refund durable rebroadcast",
    )

    h.require(
        after.get("blake_refund_hex") == signed_hex,
        "durable rebroadcast changed signed BLAKE refund hex",
    )

    mempool = h.blake_cli(
        "getmempoolentry",
        first_txid,
    )

    h.require(
        bool(mempool),
        "exact durable BLAKE refund was not restored to mempool",
    )

    h.ensure_test_wallets()

    print(
        "PASS: BLAKE refund survived mempool loss without signing wallet"
    )


def test_pending_blake_refund():
    banner("PRODUCTION BLAKE REFUND: PENDING CONFIRMATION")

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    # First settle Bitcoin through Bob's normal confirmed refund.
    bob = h.load_state(
        swap_id,
        "bob",
    )

    current_btc = int(
        h.btc_cli("getblockcount")
    )

    remaining_btc = (
        int(bob["btc_timeout"])
        - current_btc
    )

    if remaining_btc > 0:
        h.mine_btc(remaining_btc)

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

    h.assert_state(
        swap_id,
        "alice",
        "BTC_REFUNDED",
    )

    # Mature Alice's BLAKE refund.
    alice = h.load_state(
        swap_id,
        "alice",
    )

    current_blake = int(
        h.blake_cli("getblockcount")
    )

    remaining_blake = (
        int(alice["blake_timeout"])
        - current_blake
    )

    if remaining_blake > 0:
        h.mine_blake(remaining_blake)

    first = prod_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    h.require(
        "awaiting active-chain confirmation"
        in (first.stdout + first.stderr).lower(),
        "first production BLAKE refund did not report pending confirmation",
    )

    pending = h.load_state(
        swap_id,
        "alice",
    )

    first_txid = require_pending(
        state=pending,
        expected_state="BTC_REFUNDED",
        tx_field="blake_refund_txid",
        block_field="blake_refund_blockhash",
        label="BLAKE refund",
    )

    retry = prod_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    h.require(
        "BLAKE REFUND PENDING"
        in (retry.stdout + retry.stderr),
        "BLAKE refund retry did not recognize exact pending transaction",
    )

    require_same_pending(
        state=h.load_state(swap_id, "alice"),
        expected_state="BTC_REFUNDED",
        tx_field="blake_refund_txid",
        block_field="blake_refund_blockhash",
        txid=first_txid,
        label="BLAKE refund",
    )

    h.mine_blake(1)

    prod_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    require_confirmed(
        state=h.load_state(swap_id, "alice"),
        expected_state="BLAKE_REFUNDED",
        tx_field="blake_refund_txid",
        block_field="blake_refund_blockhash",
        txid=first_txid,
        label="BLAKE refund",
    )

    print("PASS: production BLAKE refund waits for confirmation")


def main():
    h.preflight()
    h.ok("preflight")

    test_pending_btc_claim()
    test_pending_blake_claim()
    test_pending_btc_refund()
    test_pending_blake_refund()
    test_blake_claim_durable_rebroadcast_after_mempool_loss()
    test_blake_refund_durable_rebroadcast_after_mempool_loss()

    print()
    print("=" * 72)
    print("ALL PRODUCTION SETTLEMENT PENDING TESTS PASSED")
    print("=" * 72)


if __name__ == "__main__":
    main()
