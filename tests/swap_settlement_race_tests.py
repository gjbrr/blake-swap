#!/usr/bin/env python3

import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import swap_protocol_tests as h


def participant_env(role):
    env = os.environ.copy()

    for key in (
        "BLAKE_SWAP_STATE_DIR",
        "BLAKE_SWAP_MESSAGE_DIR",
        "BLAKE_SWAP_ROLE",
        "BLAKE_SWAP_CHAIN_WALLET",
        "BLAKE_SWAP_BLAKE_WALLET",
        "BLAKE_SWAP_BTC_WALLET",
        "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
        "BLAKE_SWAP_NOSTR_KEY_FILE",
        "BLAKE_SWAP_NOSTR_KEY_DIR",
    ):
        env.pop(key, None)

    for variable, value in h.node_env().items():
        env.setdefault(variable, value)

    env["BLAKE_SWAP_TRANSPORT"] = "file"
    env["BLAKE_SWAP_TEST_MODE"] = "1"
    env["BLAKE_SWAP_NOSTR_RELAYS"] = "ws://127.0.0.1:1"
    env["BLAKE_SWAP_MESSAGE_DIR"] = str(h.MESSAGE_DIR)
    env["BLAKE_SWAP_ROLE"] = role

    if role == "alice":
        base = h.ALICE_BASE
        wallet = h.ALICE_WALLET
        keyfile = h.ALICE_PARTICIPANT_KEY
    elif role == "bob":
        base = h.BOB_BASE
        wallet = h.BOB_WALLET
        keyfile = h.BOB_PARTICIPANT_KEY
    else:
        raise h.TestFailure(f"unknown role: {role}")

    env["BLAKE_SWAP_STATE_DIR"] = str(base)
    env["BLAKE_SWAP_CHAIN_WALLET"] = wallet
    env["BLAKE_SWAP_BLAKE_WALLET"] = wallet
    env["BLAKE_SWAP_BTC_WALLET"] = wallet
    env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(keyfile)

    return env


def run_race(command, swap_id, role):
    with tempfile.TemporaryDirectory(
        prefix="blake-swap-settlement-race-"
    ) as td:
        td = Path(td)
        gate = td / "GO"
        runner = td / "runner.py"

        runner.write_text(
            "\n".join([
                "import os",
                "import sys",
                "import time",
                "",
                "gate = sys.argv[1]",
                "script = sys.argv[2]",
                "command = sys.argv[3]",
                "swap_id = sys.argv[4]",
                "",
                "deadline = time.time() + 20",
                "while not os.path.exists(gate):",
                "    if time.time() >= deadline:",
                "        raise SystemExit(97)",
                "    time.sleep(0.001)",
                "",
                "os.execv(",
                "    sys.executable,",
                "    [",
                "        sys.executable,",
                "        script,",
                "        command,",
                "        swap_id,",
                "    ],",
                ")",
                "",
            ])
        )

        cmd = [
            sys.executable,
            str(runner),
            str(gate),
            str(h.SWAP_V2),
            command,
            swap_id,
        ]

        env = participant_env(role)

        p1 = subprocess.Popen(
            cmd,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        p2 = subprocess.Popen(
            cmd,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        time.sleep(0.20)
        gate.write_text("GO\n")

        out1, err1 = p1.communicate(timeout=90)
        out2, err2 = p2.communicate(timeout=90)

    results = [
        (p1.returncode, out1, err1),
        (p2.returncode, out2, err2),
    ]

    h.require(
        any(rc == 0 for rc, _, _ in results),
        (
            f"{command}: both concurrent processes failed\n\n"
            f"process 1 rc={p1.returncode}\n"
            f"stdout:\n{out1[-2000:]}\n"
            f"stderr:\n{err1[-2000:]}\n\n"
            f"process 2 rc={p2.returncode}\n"
            f"stdout:\n{out2[-2000:]}\n"
            f"stderr:\n{err2[-2000:]}"
        ),
    )

    return results


def exact_input_spenders(block, funding_txid, funding_vout):
    found = []

    for tx in block.get("tx", []):
        if not isinstance(tx, dict):
            continue

        for vin in tx.get("vin", []):
            if (
                vin.get("txid") == funding_txid
                and int(vin.get("vout", -1))
                == int(funding_vout)
            ):
                found.append(tx.get("txid"))
                break

    return found


def assert_canonical_settlement(
    *,
    swap_id,
    role,
    chain,
    branch,
    expected_state,
    command,
):
    state = h.load_state(swap_id, role)

    h.require(
        state["state"] == expected_state,
        (
            f"{command}: state is {state['state']}, "
            f"expected {expected_state}"
        ),
    )

    tx_field = f"{chain}_{branch}_txid"
    block_field = f"{chain}_{branch}_blockhash"
    funding_tx_field = f"{chain}_funding_txid"
    funding_vout_field = f"{chain}_funding_vout"

    canonical_txid = state.get(tx_field)
    canonical_blockhash = state.get(block_field)
    funding_txid = state.get(funding_tx_field)
    funding_vout = state.get(funding_vout_field)

    h.require(
        bool(canonical_txid),
        f"{command}: no durable canonical settlement txid",
    )

    h.require(
        bool(canonical_blockhash),
        f"{command}: no durable settlement blockhash",
    )

    h.require(
        bool(funding_txid),
        f"{command}: no funding txid",
    )

    h.require(
        funding_vout is not None,
        f"{command}: no funding vout",
    )

    cli = h.btc_cli if chain == "btc" else h.blake_cli

    tx = cli(
        "getrawtransaction",
        canonical_txid,
        True,
        canonical_blockhash,
    )

    h.require(
        int(tx.get("confirmations", 0)) >= 1,
        f"{command}: canonical transaction is not confirmed",
    )

    matching_inputs = [
        vin
        for vin in tx.get("vin", [])
        if (
            vin.get("txid") == funding_txid
            and int(vin.get("vout", -1))
            == int(funding_vout)
        )
    ]

    h.require(
        len(matching_inputs) == 1,
        (
            f"{command}: canonical settlement does not "
            "spend the exact HTLC once"
        ),
    )

    block = cli(
        "getblock",
        canonical_blockhash,
        2,
    )

    spenders = exact_input_spenders(
        block,
        funding_txid,
        funding_vout,
    )

    h.require(
        spenders == [canonical_txid],
        (
            f"{command}: active settlement block contains "
            f"unexpected HTLC spenders: {spenders}"
        ),
    )

    #
    # There must not be a second live mempool transaction spending
    # the same HTLC after the canonical transaction confirmed.
    #
    competing = []

    for mempool_txid in cli("getrawmempool"):
        memtx = cli(
            "getrawtransaction",
            mempool_txid,
            True,
        )

        for vin in memtx.get("vin", []):
            if (
                vin.get("txid") == funding_txid
                and int(vin.get("vout", -1))
                == int(funding_vout)
            ):
                competing.append(mempool_txid)
                break

    h.require(
        not competing,
        (
            f"{command}: competing HTLC settlement remains "
            f"in mempool: {competing}"
        ),
    )

    #
    # Retry semantics may be "already done" success or safe refusal.
    # What matters is that durable economic identity cannot change.
    #
    before = h.load_state(swap_id, role)

    h.swap_cmd(
        command,
        swap_id,
        role=role,
        expect_ok=False,
    )

    after = h.load_state(swap_id, role)

    h.require(
        after.get(tx_field) == canonical_txid,
        f"{command}: retry changed canonical settlement txid",
    )

    h.require(
        after.get(block_field) == canonical_blockhash,
        f"{command}: retry changed canonical settlement block",
    )

    h.require(
        after["state"] == before["state"],
        f"{command}: retry changed settled protocol state",
    )

    return canonical_txid


def test_btc_claim_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BTC CLAIM")

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

    h.assert_state(
        swap_id,
        "alice",
        "BTC_FUNDED",
    )

    run_race(
        "claim-btc",
        swap_id,
        "alice",
    )

    txid = assert_canonical_settlement(
        swap_id=swap_id,
        role="alice",
        chain="btc",
        branch="claim",
        expected_state="BTC_CLAIMED",
        command="claim-btc",
    )

    h.ok(
        "concurrent BTC claims converge on one canonical txid "
        + txid[:12]
    )


def test_blake_claim_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BLAKE CLAIM")

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

    run_race(
        "claim-blake",
        swap_id,
        "bob",
    )

    txid = assert_canonical_settlement(
        swap_id=swap_id,
        role="bob",
        chain="blake",
        branch="claim",
        expected_state="BLAKE_CLAIMED",
        command="claim-blake",
    )

    h.ok(
        "concurrent BLAKE claims converge on one canonical txid "
        + txid[:12]
    )


def test_btc_refund_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BTC REFUND")

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

    remaining = int(bob["btc_timeout"]) - current

    if remaining > 0:
        h.mine_btc(remaining)

    run_race(
        "refund-btc",
        swap_id,
        "bob",
    )

    txid = assert_canonical_settlement(
        swap_id=swap_id,
        role="bob",
        chain="btc",
        branch="refund",
        expected_state="BTC_REFUNDED",
        command="refund-btc",
    )

    h.ok(
        "concurrent BTC refunds converge on one canonical txid "
        + txid[:12]
    )


def test_blake_refund_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BLAKE REFUND")

    swap_id = h.make_fresh_swap()
    print("swap:", swap_id)

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)

    alice = h.load_state(
        swap_id,
        "alice",
    )

    current = int(
        h.blake_cli("getblockcount")
    )

    remaining = int(alice["blake_timeout"]) - current

    if remaining > 0:
        h.mine_blake(remaining)

    run_race(
        "refund-blake",
        swap_id,
        "alice",
    )

    txid = assert_canonical_settlement(
        swap_id=swap_id,
        role="alice",
        chain="blake",
        branch="refund",
        expected_state="BLAKE_REFUNDED",
        command="refund-blake",
    )

    h.ok(
        "concurrent BLAKE refunds converge on one canonical txid "
        + txid[:12]
    )


def main():
    started = time.time()

    try:
        h.preflight()
        h.ok("preflight")

        test_btc_claim_race()
        test_blake_claim_race()
        test_btc_refund_race()
        test_blake_refund_race()

    except Exception as e:
        h.fail(
            "SETTLEMENT RACE HARNESS",
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
