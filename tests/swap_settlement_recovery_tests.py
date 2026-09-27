#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import importlib.util
import sys
import time
from types import SimpleNamespace

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


def test_confirmed_recorded_refund_without_txindex():
    """
    Regression for the live settlement-recovery failure:

    A locally broadcast settlement can be confirmed while durable state
    still records its txid but no blockhash.  A node without txindex may
    then fail getrawtransaction(txid).

    Reconciliation must fall through to the hardened wallet recovery
    path, independently decode the exact recorded transaction, prove
    that it spends this swap's HTLC via the expected branch, and report
    confirmed chain truth rather than pending-or-ambiguous.
    """

    h.banner(
        "RECONCILE REGRESSION: CONFIRMED REFUND WITHOUT TXINDEX"
    )

    spec = importlib.util.spec_from_file_location(
        "swap_v2_no_txindex_regression",
        str(h.SWAP_V2),
    )

    h.require(
        spec is not None and spec.loader is not None,
        "could not create import spec for swap_v2.py",
    )

    engine = importlib.util.module_from_spec(spec)

    # swap_v2.py imports sibling modules from src/, so reproduce the
    # normal script import environment while loading it in-process.
    src_dir = str(h.SWAP_V2.parent)
    inserted_src = False

    if src_dir not in sys.path:
        sys.path.insert(0, src_dir)
        inserted_src = True

    # dataclasses and similar module machinery expect the module to be
    # registered while its top-level code executes.
    sys.modules[spec.name] = engine

    try:
        spec.loader.exec_module(engine)

        funding_txid = "11" * 32
        funding_vout = 1
        # Regression also covers the live condition where funding was
        # recorded while pending, but its later confirmed blockhash was
        # never synchronized before the HTLC was spent.
        funding_blockhash = None

        refund_txid = "22" * 32
        refund_blockhash = "bb" * 32

        secret = bytes.fromhex("33" * 32)
        secret_hash = hashlib.sha256(secret).hexdigest()

        swap = SimpleNamespace(
            swap_id="regression-no-txindex",

            blake_funding_txid=funding_txid,
            blake_funding_vout=funding_vout,
            blake_funding_blockhash=funding_blockhash,

            blake_claim_txid=None,
            blake_claim_blockhash=None,

            # Exact settlement identity was persisted immediately after
            # broadcast, but the process has not yet synchronized the
            # subsequently confirmed blockhash.
            blake_refund_txid=refund_txid,
            blake_refund_blockhash=None,

            secret_hash=secret_hash,

            # These are not expected to be used in this spent-output path,
            # but keeping realistic swap fields makes accidental future
            # dependencies fail clearly rather than mysteriously.
            blake_amount=0.0001,
            btc_sats=2850,
        )

        calls = []

        def fake_blake_cli(*args, wallet=None):
            h.require(
                bool(args),
                "mock BLAKE RPC called without method",
            )

            method = args[0]
            calls.append(
                (method, args[1:], wallet)
            )

            if method == "gettxout":
                h.require(
                    args[1] == funding_txid,
                    "gettxout used wrong funding txid",
                )
                h.require(
                    int(args[2]) == funding_vout,
                    "gettxout used wrong funding vout",
                )

                # Funding outpoint is gone: settlement has spent it.
                return None

            if method == "getrawtransaction":
                h.require(
                    args[1] in {
                        funding_txid,
                        refund_txid,
                    },
                    "direct lookup used unexpected txid",
                )

                # Neither the spent funding transaction nor the confirmed
                # settlement is available through this no-txindex route.
                # Recovery must continue through spender/wallet evidence.
                raise RuntimeError(
                    "No such mempool or blockchain transaction. "
                    "Use -txindex or provide a block hash."
                )

            if method == "gettransaction":
                h.require(
                    args[1] == refund_txid,
                    "wallet recovery used wrong settlement txid",
                )
                h.require(
                    wallet == "mock-blake-wallet",
                    "wallet recovery used wrong wallet",
                )

                return {
                    "hex": "deadbeef",
                    "confirmations": 3,
                    "blockhash": refund_blockhash,
                }

            if method == "decoderawtransaction":
                h.require(
                    args[1] == "deadbeef",
                    "wallet recovery decoded unexpected transaction hex",
                )

                return {
                    "txid": refund_txid,
                    "vin": [
                        {
                            "txid": funding_txid,
                            "vout": funding_vout,

                            # A realistic refund-branch witness:
                            #   <sig> <empty or_i selector> <witnessScript>
                            # The empty selector takes OP_ELSE and no
                            # preimage is present, so this is a refund.
                            "txinwitness": [
                                "30" * 71,
                                "",
                                "51",
                            ],
                        }
                    ],
                }

            raise h.TestFailure(
                f"unexpected mock BLAKE RPC method: {method}"
            )

        original_blake_cli = engine.blake_cli
        original_blake_wallet = engine.BLAKE_WALLET

        try:
            engine.blake_cli = fake_blake_cli
            engine.BLAKE_WALLET = "mock-blake-wallet"

            observed = engine._reconcile_observe_htlc(
                swap,
                "blake",
            )

        finally:
            engine.blake_cli = original_blake_cli
            engine.BLAKE_WALLET = original_blake_wallet

        h.require(
            observed.get("status") == "spent",
            (
                "confirmed recorded refund was not recovered as spent: "
                f"{observed}"
            ),
        )

        h.require(
            observed.get("branch") == "refund",
            (
                "confirmed recorded refund recovered wrong branch: "
                f"{observed}"
            ),
        )

        h.require(
            observed.get("spend_txid") == refund_txid,
            (
                "confirmed recorded refund recovered wrong txid: "
                f"{observed}"
            ),
        )

        h.require(
            observed.get("spend_blockhash") == refund_blockhash,
            (
                "confirmed recorded refund recovered wrong blockhash: "
                f"{observed}"
            ),
        )

        h.require(
            observed.get("status") != "pending-or-ambiguous",
            (
                "no-txindex recovery regressed to pending-or-ambiguous: "
                f"{observed}"
            ),
        )

        methods = [
            method
            for method, _args, _wallet
            in calls
        ]

        h.require(
            "gettransaction" in methods,
            (
                "regression did not exercise wallet gettransaction "
                "fallback"
            ),
        )

        h.require(
            "decoderawtransaction" in methods,
            (
                "regression did not independently decode wallet tx"
            ),
        )

        h.ok(
            "confirmed recorded BLAKE refund recovered with missing "
            "funding blockhash and without txindex"
        )

    finally:
        sys.modules.pop(spec.name, None)

        if inserted_src:
            try:
                sys.path.remove(src_dir)
            except ValueError:
                pass


def test_unknown_btc_spender_without_funding_blockhash():
    """
    Regression for live Bob recovery:

    Bob knows the exact Bitcoin funding txid:vout, but durable state never
    learned the funding blockhash before Alice spent the HTLC. Bob also
    does not yet know Alice's claim txid.

    Reconciliation must use the persisted Bitcoin scan anchor, discover
    the confirmed spender directly from blocks, classify the claim branch,
    and report spent chain truth.
    """

    h.banner(
        "RECONCILE REGRESSION: UNKNOWN BTC SPENDER "
        "WITHOUT FUNDING BLOCKHASH"
    )

    spec = importlib.util.spec_from_file_location(
        "swap_v2_unknown_btc_spender_regression",
        str(h.SWAP_V2),
    )

    h.require(
        spec is not None and spec.loader is not None,
        "could not create import spec for swap_v2.py",
    )

    engine = importlib.util.module_from_spec(spec)

    src_dir = str(h.SWAP_V2.parent)
    inserted_src = False

    if src_dir not in sys.path:
        sys.path.insert(0, src_dir)
        inserted_src = True

    sys.modules[spec.name] = engine

    try:
        spec.loader.exec_module(engine)

        funding_txid = "44" * 32
        funding_vout = 0

        claim_txid = "55" * 32
        claim_blockhash = "66" * 32

        secret = bytes.fromhex("77" * 32)
        secret_hash = hashlib.sha256(secret).hexdigest()

        swap = SimpleNamespace(
            swap_id="regression-unknown-btc-spender",

            btc_funding_txid=funding_txid,
            btc_funding_vout=funding_vout,

            # Exact live condition: Bob persisted the pending outpoint
            # but never synchronized the later funding blockhash.
            btc_funding_blockhash=None,

            btc_claim_txid=None,
            btc_claim_blockhash=None,
            btc_refund_txid=None,
            btc_refund_blockhash=None,

            # Independent bounded recovery anchor captured during
            # negotiation.
            btc_scan_start_height=100,

            secret_hash=secret_hash,
            btc_sats=3000,
            blake_amount=0.0001,
        )

        calls = []

        def fake_btc_cli(*args, wallet=None):
            h.require(
                bool(args),
                "mock Bitcoin RPC called without method",
            )

            method = args[0]
            calls.append(
                (method, args[1:], wallet)
            )

            if method == "gettxout":
                h.require(
                    args[1] == funding_txid,
                    "gettxout used wrong funding txid",
                )
                h.require(
                    int(args[2]) == funding_vout,
                    "gettxout used wrong funding vout",
                )

                # Alice already spent the HTLC.
                return None

            if method == "getrawtransaction":
                h.require(
                    args[1] == funding_txid,
                    "unexpected direct transaction lookup",
                )

                # Simulate a provider/node without txindex access to the
                # now-spent funding transaction.
                raise RuntimeError(
                    "No such mempool or blockchain transaction. "
                    "Use -txindex or provide a block hash."
                )

            if method == "getblockcount":
                return 101

            if method == "getblockhash":
                height = int(args[1])

                if height == 100:
                    return "88" * 32

                if height == 101:
                    return claim_blockhash

                raise h.TestFailure(
                    f"unexpected Bitcoin block height: {height}"
                )

            if method == "getblock":
                blockhash = args[1]

                if blockhash == "88" * 32:
                    return {
                        "height": 100,
                        "tx": [],
                    }

                if blockhash == claim_blockhash:
                    return {
                        "height": 101,
                        "tx": [
                            {
                                "txid": claim_txid,
                                "vin": [
                                    {
                                        "txid": funding_txid,
                                        "vout": funding_vout,
                                        # A realistic claim-branch witness:
                                        #   <preimage> <sig> <01> <witnessScript>
                                        # The 0x01 selector takes OP_IF.
                                        "txinwitness": [
                                            secret.hex(),
                                            "30" * 71,
                                            "01",
                                            "51",
                                        ],
                                    }
                                ],
                            }
                        ],
                    }

                raise h.TestFailure(
                    f"unexpected Bitcoin blockhash: {blockhash}"
                )

            raise h.TestFailure(
                f"unexpected mock Bitcoin RPC method: {method}"
            )

        original_btc_cli = engine.btc_cli

        try:
            engine.btc_cli = fake_btc_cli

            observed = engine._reconcile_observe_htlc(
                swap,
                "btc",
            )

        finally:
            engine.btc_cli = original_btc_cli

        h.require(
            observed.get("status") == "spent",
            (
                "unknown Bitcoin spender was not recovered as spent: "
                f"{observed}"
            ),
        )

        h.require(
            observed.get("branch") == "claim",
            (
                "unknown Bitcoin spender recovered wrong branch: "
                f"{observed}"
            ),
        )

        h.require(
            observed.get("spend_txid") == claim_txid,
            (
                "unknown Bitcoin spender recovered wrong txid: "
                f"{observed}"
            ),
        )

        h.require(
            observed.get("spend_blockhash") == claim_blockhash,
            (
                "unknown Bitcoin spender recovered wrong blockhash: "
                f"{observed}"
            ),
        )

        methods = [
            method
            for method, _args, _wallet
            in calls
        ]

        h.require(
            "getblockcount" in methods,
            "regression did not exercise bounded block scan",
        )

        h.require(
            methods.count("getblockhash") == 2,
            (
                "spender recovery did not stay bounded to scan anchor: "
                f"{methods}"
            ),
        )

        h.ok(
            "unknown confirmed BTC claim recovered from scan anchor "
            "without funding blockhash"
        )

    finally:
        sys.modules.pop(spec.name, None)

        if inserted_src:
            try:
                sys.path.remove(src_dir)
            except ValueError:
                pass



def main():
    h.PASS_COUNT = 0
    h.FAIL_COUNT = 0

    started = time.time()

    tests = [
        ("BTC CLAIM", test_btc_claim),
        ("BLAKE CLAIM", test_blake_claim),
        ("BTC REFUND", test_btc_refund),
        ("BLAKE REFUND", test_blake_refund),
        (
            "CONFIRMED REFUND WITHOUT TXINDEX",
            test_confirmed_recorded_refund_without_txindex,
        ),
        (
            "UNKNOWN BTC SPENDER WITHOUT FUNDING BLOCKHASH",
            test_unknown_btc_spender_without_funding_blockhash,
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
