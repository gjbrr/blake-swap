#!/usr/bin/env python3
"""
Concurrent settlement race tests.

Two independent engine processes are released at the same instant against
the same swap and participant state.

The security invariant is NOT that exactly one process must exit 0.
Either process may win, and a late process may safely discover/reuse the
winner.

The invariant is:

  * exactly one economic spend of the HTLC may enter the mempool;
  * durable settlement identity must point at that exact spend;
  * no competing transaction identity may survive locally;
  * after confirmation, retry must converge to the correct final state.

Covered economic actions:

  * claim-btc
  * claim-blake
  * refund-btc
  * refund-blake
"""

import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import swap_protocol_tests as h


PASS = 0
FAIL = 0


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
    env["BLAKE_SWAP_AUTO_MINE"] = "0"
    env["BLAKE_SWAP_NOSTR_RELAYS"] = "ws://127.0.0.1:1"
    env["BLAKE_SWAP_MESSAGE_DIR"] = str(h.MESSAGE_DIR)
    env["BLAKE_SWAP_ROLE"] = role

    if role == "alice":
        env["BLAKE_SWAP_STATE_DIR"] = str(h.ALICE_BASE)
        env["BLAKE_SWAP_CHAIN_WALLET"] = h.ALICE_WALLET
        env["BLAKE_SWAP_BLAKE_WALLET"] = h.ALICE_WALLET
        env["BLAKE_SWAP_BTC_WALLET"] = h.ALICE_WALLET
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.ALICE_PARTICIPANT_KEY
        )

    elif role == "bob":
        env["BLAKE_SWAP_STATE_DIR"] = str(h.BOB_BASE)
        env["BLAKE_SWAP_CHAIN_WALLET"] = h.BOB_WALLET
        env["BLAKE_SWAP_BLAKE_WALLET"] = h.BOB_WALLET
        env["BLAKE_SWAP_BTC_WALLET"] = h.BOB_WALLET
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.BOB_PARTICIPANT_KEY
        )

    else:
        raise AssertionError(f"unknown role: {role}")

    return env


def race(command, swap_id, role):
    """
    Release two engine processes through the same filesystem gate.
    Return both subprocess results.
    """
    with tempfile.TemporaryDirectory(
        prefix=f"blake-swap-{command}-race-"
    ) as td:
        td = Path(td)
        gate = td / "GO"
        runner = td / "runner.py"

        runner.write_text(
            "\n".join(
                [
                    "import os",
                    "import sys",
                    "import time",
                    "gate = sys.argv[1]",
                    "engine = sys.argv[2]",
                    "command = sys.argv[3]",
                    "swap_id = sys.argv[4]",
                    "deadline = time.time() + 15",
                    "while not os.path.exists(gate):",
                    "    if time.time() >= deadline:",
                    "        raise SystemExit(97)",
                    "    time.sleep(0.001)",
                    "os.execv(sys.executable, [",
                    "    sys.executable,",
                    "    engine,",
                    "    command,",
                    "    swap_id,",
                    "])",
                    "",
                ]
            )
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

    return [
        {
            "returncode": p1.returncode,
            "stdout": out1,
            "stderr": err1,
        },
        {
            "returncode": p2.returncode,
            "stdout": out2,
            "stderr": err2,
        },
    ]


def exact_mempool_spenders(chain, funding_txid, funding_vout):
    """
    Return every mempool transaction spending this exact HTLC outpoint.
    """
    if chain == "btc":
        cli = h.btc_cli
    elif chain == "blake":
        cli = h.blake_cli
    else:
        raise AssertionError(chain)

    txids = cli("getrawmempool")
    matches = []

    for txid in txids:
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


def race_diagnostics(results):
    chunks = []

    for i, result in enumerate(results, 1):
        chunks.append(
            f"\n--- process {i} rc={result['returncode']} ---\n"
            f"stdout:\n{result['stdout'][-2500:]}\n"
            f"stderr:\n{result['stderr'][-2500:]}\n"
        )

    return "".join(chunks)


def assert_pending_race(
    *,
    label,
    command,
    swap_id,
    role,
    chain,
    funding_txid_field,
    funding_vout_field,
    settlement_txid_field,
    settlement_blockhash_field,
    expected_pending_state,
    expected_final_state,
):
    before = h.load_state(
        swap_id,
        role,
    )

    require(
        before["state"] == expected_pending_state,
        (
            f"{label}: wrong starting state "
            f"{before['state']}; expected "
            f"{expected_pending_state}"
        ),
    )

    funding_txid = before[funding_txid_field]
    funding_vout = before[funding_vout_field]

    require(
        funding_txid,
        f"{label}: no funding txid",
    )

    require(
        funding_vout is not None,
        f"{label}: no funding vout",
    )

    results = race(
        command,
        swap_id,
        role,
    )

    spenders = exact_mempool_spenders(
        chain,
        funding_txid,
        funding_vout,
    )

    require(
        len(spenders) == 1,
        (
            f"{label}: expected exactly one mempool spend "
            f"of the HTLC, found {spenders}"
            + race_diagnostics(results)
        ),
    )

    winner = spenders[0]

    after = h.load_state(
        swap_id,
        role,
    )

    require(
        after["state"] == expected_pending_state,
        (
            f"{label}: unconfirmed race advanced state "
            f"to {after['state']}"
            + race_diagnostics(results)
        ),
    )

    durable_txid = after.get(
        settlement_txid_field
    )

    require(
        durable_txid == winner,
        (
            f"{label}: durable settlement identity diverged "
            f"from the actual mempool winner\n"
            f"durable: {durable_txid}\n"
            f"winner:  {winner}"
            + race_diagnostics(results)
        ),
    )

    require(
        after.get(settlement_blockhash_field) is None,
        (
            f"{label}: unconfirmed settlement acquired "
            "a durable blockhash"
        ),
    )

    #
    # Confirm exactly the surviving transaction.
    #
    if chain == "btc":
        h.mine_btc(1)
    else:
        h.mine_blake(1)

    #
    # Retry through the normal engine command. It must discover the
    # confirmed exact spend and converge, rather than construct another.
    #
    h.swap_cmd(
        command,
        swap_id,
        role=role,
    )

    final = h.load_state(
        swap_id,
        role,
    )

    require(
        final["state"] == expected_final_state,
        (
            f"{label}: confirmation/retry did not converge; "
            f"state={final['state']}"
        ),
    )

    require(
        final.get(settlement_txid_field) == winner,
        (
            f"{label}: confirmed durable txid changed\n"
            f"expected: {winner}\n"
            f"actual:   {final.get(settlement_txid_field)}"
        ),
    )

    require(
        bool(final.get(settlement_blockhash_field)),
        (
            f"{label}: confirmed settlement has no "
            "durable blockhash"
        ),
    )

    ok(
        f"{label} converged on one economic transaction"
    )


def setup_two_funded_swap():
    swap_id = h.make_fresh_swap()
    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)
    return swap_id


def test_claim_btc_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BTC CLAIM")

    swap_id = setup_two_funded_swap()
    print("swap:", swap_id)

    assert_pending_race(
        label="BTC claim race",
        command="claim-btc",
        swap_id=swap_id,
        role="alice",
        chain="btc",
        funding_txid_field="btc_funding_txid",
        funding_vout_field="btc_funding_vout",
        settlement_txid_field="btc_claim_txid",
        settlement_blockhash_field="btc_claim_blockhash",
        expected_pending_state="BTC_FUNDED",
        expected_final_state="BTC_CLAIMED",
    )


def setup_blake_claim_ready_swap():
    swap_id = setup_two_funded_swap()

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
        bob.get("revealed_secret_hex"),
        "Bob did not recover S from Bitcoin",
    )

    return swap_id


def test_claim_blake_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BLAKE CLAIM")

    swap_id = setup_blake_claim_ready_swap()
    print("swap:", swap_id)

    assert_pending_race(
        label="BLAKE claim race",
        command="claim-blake",
        swap_id=swap_id,
        role="bob",
        chain="blake",
        funding_txid_field="blake_funding_txid",
        funding_vout_field="blake_funding_vout",
        settlement_txid_field="blake_claim_txid",
        settlement_blockhash_field="blake_claim_blockhash",
        expected_pending_state="BTC_CLAIMED",
        expected_final_state="BLAKE_CLAIMED",
    )


def test_refund_btc_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BTC REFUND")

    swap_id = setup_two_funded_swap()
    print("swap:", swap_id)

    bob = h.load_state(
        swap_id,
        "bob",
    )

    current = int(
        h.btc_cli("getblockcount")
    )

    blocks = int(
        bob["btc_timeout"]
    ) - current

    if blocks > 0:
        h.mine_btc(blocks)

    assert_pending_race(
        label="BTC refund race",
        command="refund-btc",
        swap_id=swap_id,
        role="bob",
        chain="btc",
        funding_txid_field="btc_funding_txid",
        funding_vout_field="btc_funding_vout",
        settlement_txid_field="btc_refund_txid",
        settlement_blockhash_field="btc_refund_blockhash",
        expected_pending_state="BTC_FUNDED",
        expected_final_state="BTC_REFUNDED",
    )


def setup_blake_refund_ready_swap():
    #
    # Abort-before-BTC-funding path. This additionally exercises the
    # requirement that Alice independently prove Bitcoin funding absent.
    #
    swap_id = h.make_fresh_swap()
    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)

    alice = h.load_state(
        swap_id,
        "alice",
    )

    current = int(
        h.blake_cli("getblockcount")
    )

    blocks = int(
        alice["blake_timeout"]
    ) - current

    if blocks > 0:
        h.mine_blake(blocks)

    return swap_id


def test_refund_blake_race():
    h.banner("CONCURRENT SETTLEMENT RACE: BLAKE REFUND")

    swap_id = setup_blake_refund_ready_swap()
    print("swap:", swap_id)

    assert_pending_race(
        label="BLAKE refund race",
        command="refund-blake",
        swap_id=swap_id,
        role="alice",
        chain="blake",
        funding_txid_field="blake_funding_txid",
        funding_vout_field="blake_funding_vout",
        settlement_txid_field="blake_refund_txid",
        settlement_blockhash_field="blake_refund_blockhash",
        expected_pending_state="BLAKE_FUNDED",
        expected_final_state="BLAKE_REFUNDED",
    )


def main():
    global PASS, FAIL

    started = time.time()

    h.preflight()
    ok("preflight")

    tests = [
        (
            "BTC CLAIM RACE",
            test_claim_btc_race,
        ),
        (
            "BLAKE CLAIM RACE",
            test_claim_blake_race,
        ),
        (
            "BTC REFUND RACE",
            test_refund_btc_race,
        ),
        (
            "BLAKE REFUND RACE",
            test_refund_blake_race,
        ),
    ]

    for name, test in tests:
        try:
            test()
        except Exception as exc:
            fail(
                name,
                str(exc),
            )

    h.banner("CONCURRENT SETTLEMENT RACE SUMMARY")
    print("passes:  ", PASS)
    print("failures:", FAIL)
    print(
        "elapsed:  ",
        f"{time.time() - started:.1f}s",
    )

    if FAIL:
        print()
        print(
            "A failure here may represent a real concurrency "
            "safety bug, not merely a flaky test."
        )

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
