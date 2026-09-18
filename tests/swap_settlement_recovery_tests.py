#!/usr/bin/env python3

from __future__ import annotations

import sys
import time

import swap_protocol_tests as h


CRASH_EXIT = 86


def crash_swap_cmd(point: str, *args, role: str):
    env = {
        "BLAKE_SWAP_ROLE": role,
        "BLAKE_SWAP_TEST_MODE": "1",
        "BLAKE_SWAP_TEST_CRASH_POINT": point,
    }

    p = h.run(
        [sys.executable, h.SWAP_V2, *args],
        env=env,
        expect_ok=False,
    )

    h.require(
        p.returncode == CRASH_EXIT,
        (
            f"{point}: expected deliberate exit {CRASH_EXIT}, "
            f"got {p.returncode}\n"
            f"{p.stderr.strip() or p.stdout.strip()}"
        ),
    )

    return p


def cli_for(chain: str):
    if chain == "btc":
        return h.btc_cli
    if chain == "blake":
        return h.blake_cli
    raise ValueError(chain)


def find_mempool_spenders(
    chain: str,
    funding_txid: str,
    funding_vout: int,
) -> list[str]:

    cli = cli_for(chain)

    spenders = []

    for txid in cli("getrawmempool"):
        tx = cli(
            "getrawtransaction",
            txid,
            "true",
        )

        for vin in tx.get("vin", []):
            if (
                vin.get("txid") == funding_txid
                and int(vin.get("vout", -1)) == int(funding_vout)
            ):
                spenders.append(txid)
                break

    return spenders


def require_one_spender(
    chain: str,
    funding_txid: str,
    funding_vout: int,
) -> str:

    spenders = find_mempool_spenders(
        chain,
        funding_txid,
        funding_vout,
    )

    h.require(
        len(spenders) == 1,
        (
            f"{chain}: expected exactly one mempool spender, "
            f"found {len(spenders)}: {spenders}"
        ),
    )

    return spenders[0]


def retry_must_remain_pending(
    *,
    action: str,
    swap_id: str,
    role: str,
    expected_state: str,
    chain: str,
    funding_txid: str,
    funding_vout: int,
    original_spender: str,
):
    p = h.swap_cmd(
        action,
        swap_id,
        role=role,
    )

    output = p.stdout + p.stderr

    h.require(
        "PENDING" in output,
        (
            f"{action}: retry did not report exact settlement "
            "as pending"
        ),
    )

    h.assert_state(
        swap_id,
        role,
        expected_state,
    )

    state = h.load_state(
        swap_id,
        role,
    )

    fields = {
        "claim-btc": (
            "btc_claim_txid",
            "btc_claim_blockhash",
        ),
        "refund-btc": (
            "btc_refund_txid",
            "btc_refund_blockhash",
        ),
        "claim-blake": (
            "blake_claim_txid",
            "blake_claim_blockhash",
        ),
        "refund-blake": (
            "blake_refund_txid",
            "blake_refund_blockhash",
        ),
    }

    if action not in fields:
        raise h.TestFailure(
            f"unknown settlement action: {action}"
        )

    tx_field, block_field = fields[action]

    h.require(
        state.get(tx_field) == original_spender,
        (
            f"{action}: retry changed recorded settlement txid; "
            f"expected {original_spender}, "
            f"got {state.get(tx_field)}"
        ),
    )

    h.require(
        state.get(block_field) is None,
        (
            f"{action}: pending retry unexpectedly recorded "
            "a blockhash"
        ),
    )

    spenders = find_mempool_spenders(
        chain,
        funding_txid,
        funding_vout,
    )

    h.require(
        spenders == [original_spender],
        (
            f"{action}: retry changed mempool spender set\n"
            f"expected: {[original_spender]}\n"
            f"actual:   {spenders}"
        ),
    )


def make_two_funded_swap() -> str:
    swap_id = h.make_fresh_swap()

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)
    h.fund_btc_and_sync(swap_id)

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


def test_btc_claim():
    h.banner("SETTLEMENT CRASH: BTC CLAIM")

    swap_id = make_two_funded_swap()

    before = h.load_state(
        swap_id,
        "alice",
    )

    funding_txid = before["btc_funding_txid"]
    funding_vout = before["btc_funding_vout"]

    crash_swap_cmd(
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
    h.ok("BTC claim crash left local state unchanged")

    spender = require_one_spender(
        "btc",
        funding_txid,
        funding_vout,
    )
    h.ok("BTC claim exists exactly once in mempool")

    retry_must_remain_pending(
        action="claim-btc",
        swap_id=swap_id,
        role="alice",
        expected_state="BTC_FUNDED",
        chain="btc",
        funding_txid=funding_txid,
        funding_vout=funding_vout,
        original_spender=spender,
    )
    h.ok("BTC claim retry recognizes exact pending spend")

    h.mine_btc(1)

    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    after = h.load_state(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_CLAIMED",
    )

    h.require(
        after.get("btc_claim_txid") == spender,
        "BTC claim recovery recorded wrong txid",
    )

    h.require(
        after.get("btc_claim_blockhash"),
        "BTC claim recovery did not persist blockhash",
    )

    h.ok("BTC claim recovered from blockchain")

    print("swap:", swap_id)


def test_blake_claim():
    h.banner("SETTLEMENT CRASH: BLAKE CLAIM")

    swap_id = make_two_funded_swap()

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

    before = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        before.get("revealed_secret_hex"),
        "Bob has no recovered secret",
    )

    h.require(
        before.get("secret_hex") is None,
        "Bob improperly has Alice original secret",
    )

    funding_txid = before["blake_funding_txid"]
    funding_vout = before["blake_funding_vout"]

    crash_swap_cmd(
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
    h.ok("BLAKE claim crash left local state unchanged")

    spender = require_one_spender(
        "blake",
        funding_txid,
        funding_vout,
    )
    h.ok("BLAKE claim exists exactly once in mempool")

    retry_must_remain_pending(
        action="claim-blake",
        swap_id=swap_id,
        role="bob",
        expected_state="BTC_CLAIMED",
        chain="blake",
        funding_txid=funding_txid,
        funding_vout=funding_vout,
        original_spender=spender,
    )
    h.ok("BLAKE claim retry recognizes exact pending spend")

    h.mine_blake(1)

    h.swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    after = h.load_state(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_CLAIMED",
    )

    h.require(
        after.get("blake_claim_txid") == spender,
        "BLAKE claim recovery recorded wrong txid",
    )

    h.require(
        after.get("blake_claim_blockhash"),
        "BLAKE claim recovery did not persist blockhash",
    )

    h.ok("BLAKE claim recovered from blockchain")

    print("swap:", swap_id)


def test_btc_refund():
    h.banner("SETTLEMENT CRASH: BTC REFUND")

    swap_id = make_two_funded_swap()

    before = h.load_state(
        swap_id,
        "bob",
    )

    current = int(
        h.btc_cli("getblockcount")
    )

    timeout = int(
        before["btc_timeout"]
    )

    if timeout > current:
        h.mine_btc(
            timeout - current
        )

    funding_txid = before["btc_funding_txid"]
    funding_vout = before["btc_funding_vout"]

    crash_swap_cmd(
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
    h.ok("BTC refund crash left local state unchanged")

    spender = require_one_spender(
        "btc",
        funding_txid,
        funding_vout,
    )
    h.ok("BTC refund exists exactly once in mempool")

    retry_must_remain_pending(
        action="refund-btc",
        swap_id=swap_id,
        role="bob",
        expected_state="BTC_FUNDED",
        chain="btc",
        funding_txid=funding_txid,
        funding_vout=funding_vout,
        original_spender=spender,
    )
    h.ok("BTC refund retry recognizes exact pending spend")

    h.mine_btc(1)

    h.swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    after = h.load_state(
        swap_id,
        "bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BTC_REFUNDED",
    )

    h.require(
        after.get("btc_refund_txid") == spender,
        "BTC refund recovery recorded wrong txid",
    )

    h.require(
        after.get("btc_refund_blockhash"),
        "BTC refund recovery did not persist blockhash",
    )

    h.ok("BTC refund recovered from blockchain")

    print("swap:", swap_id)


def test_blake_refund():
    h.banner("SETTLEMENT CRASH: BLAKE REFUND")

    swap_id = h.make_fresh_swap()

    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)

    before = h.load_state(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_FUNDED",
    )

    current = int(
        h.blake_cli("getblockcount")
    )

    timeout = int(
        before["blake_timeout"]
    )

    if timeout > current:
        h.mine_blake(
            timeout - current
        )

    funding_txid = before["blake_funding_txid"]
    funding_vout = before["blake_funding_vout"]

    crash_swap_cmd(
        "blake-refund-after-broadcast",
        "refund-blake",
        swap_id,
        role="alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_FUNDED",
    )
    h.ok("BLAKE refund crash left local state unchanged")

    spender = require_one_spender(
        "blake",
        funding_txid,
        funding_vout,
    )
    h.ok("BLAKE refund exists exactly once in mempool")

    retry_must_remain_pending(
        action="refund-blake",
        swap_id=swap_id,
        role="alice",
        expected_state="BLAKE_FUNDED",
        chain="blake",
        funding_txid=funding_txid,
        funding_vout=funding_vout,
        original_spender=spender,
    )
    h.ok("BLAKE refund retry recognizes exact pending spend")

    h.mine_blake(1)

    h.swap_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    after = h.load_state(
        swap_id,
        "alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_REFUNDED",
    )

    h.require(
        after.get("blake_refund_txid") == spender,
        "BLAKE refund recovery recorded wrong txid",
    )

    h.require(
        after.get("blake_refund_blockhash"),
        "BLAKE refund recovery did not persist blockhash",
    )

    h.ok("BLAKE refund recovered from blockchain")

    print("swap:", swap_id)


def main():
    h.PASS_COUNT = 0
    h.FAIL_COUNT = 0

    started = time.time()

    tests = [
        ("BTC CLAIM", test_btc_claim),
        ("BLAKE CLAIM", test_blake_claim),
        ("BTC REFUND", test_btc_refund),
        ("BLAKE REFUND", test_blake_refund),
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
        "SETTLEMENT RECOVERY SUMMARY"
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
        "Local regtest only. File transport only."
    )

    return 1 if h.FAIL_COUNT else 0


if __name__ == "__main__":
    raise SystemExit(main())
