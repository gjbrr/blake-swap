#!/usr/bin/env python3
"""
Black-box regtest integration harness for swap_v2.py.

This intentionally drives the public CLI instead of importing internal
functions, so it tests the same command surface a real operator uses.

It uses FILE transport only. It does not contact public Nostr relays.
It creates fresh regtest swaps and mines regtest blocks as needed.

Usage:
    python3 ~/swap_protocol_tests.py happy
    python3 ~/swap_protocol_tests.py refund
    python3 ~/swap_protocol_tests.py all

Optional:
    SWAP_V2=/path/to/swap_v2.py python3 ~/swap_protocol_tests.py all
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path

HOME = Path.home()

PROJECT_DIR = Path(__file__).resolve().parent.parent

SWAP_V2 = Path(
    os.environ.get(
        "SWAP_V2",
        PROJECT_DIR / "src" / "swap_v2.py",
    )
)

TEST_ROOT = Path(
    tempfile.mkdtemp(
        prefix="blake-swap-tests-"
    )
)

ALICE_BASE = TEST_ROOT / "alice-state"
BOB_BASE = TEST_ROOT / "bob-state"

# Compatibility alias for tests that need a generic BASE.
# Alice owns the secret-bearing side of ordinary test swaps.
BASE = ALICE_BASE

ALICE_DIR = ALICE_BASE
BOB_DIR = BOB_BASE

MESSAGE_DIR = TEST_ROOT / "messages"


BTC_CLI = Path("/home/gary/bitcoin-core/build/bin/bitcoin-cli")
BTC_DATADIR = Path("/home/gary/bitcoin-swap-regtest")

BLAKE_CLI = Path("/home/gary/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli")
BLAKE_DATADIR = Path("/home/gary/blake-swap-regtest")

ALICE_WALLET = "alice"
BOB_WALLET = "bob"

ALICE_PARTICIPANT_KEY = ALICE_BASE / "test-participant.key"
BOB_PARTICIPANT_KEY = BOB_BASE / "test-participant.key"

PASS_COUNT = 0
FAIL_COUNT = 0


class TestFailure(RuntimeError):
    pass


def ensure_test_participant_keys() -> None:
    ALICE_BASE.mkdir(parents=True, exist_ok=True)
    BOB_BASE.mkdir(parents=True, exist_ok=True)

    # Deterministic non-production test secrets.
    # They exist only to let FILE-transport tests derive stable
    # participant identities without contacting Nostr relays.
    if not ALICE_PARTICIPANT_KEY.exists():
        ALICE_PARTICIPANT_KEY.write_text("11" * 32 + "\n")
        ALICE_PARTICIPANT_KEY.chmod(0o600)

    if not BOB_PARTICIPANT_KEY.exists():
        BOB_PARTICIPANT_KEY.write_text("22" * 32 + "\n")
        BOB_PARTICIPANT_KEY.chmod(0o600)


def banner(text: str) -> None:
    print()
    print("=" * 72)
    print(text)
    print("=" * 72)


def ok(name: str) -> None:
    global PASS_COUNT
    PASS_COUNT += 1
    print(f"PASS  {name}")


def fail(name: str, detail: str) -> None:
    global FAIL_COUNT
    FAIL_COUNT += 1
    print(f"FAIL  {name}")
    print(f"      {detail}")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise TestFailure(message)


def run(cmd, *, env=None, expect_ok=True, echo=False) -> subprocess.CompletedProcess:
    merged = os.environ.copy()

    #
    # Tests must never depend on the operator's current
    # participant/session configuration.
    #
    for key in (
        "BLAKE_SWAP_STATE_DIR",
        "BLAKE_SWAP_MESSAGE_DIR",
        "BLAKE_SWAP_ROLE",
        "BLAKE_SWAP_CHAIN_WALLET",
        "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
        "BLAKE_SWAP_NOSTR_KEY_FILE",
        "BLAKE_SWAP_NOSTR_KEY_DIR",
    ):
        merged.pop(key, None)

    merged["BLAKE_SWAP_TRANSPORT"] = "file"

    if env:
        merged.update(env)

    #
    # Direct test subprocesses may specify only the
    # protocol role. Resolve that role to the corresponding
    # participant's private state root unless the test
    # intentionally supplied an explicit state directory.
    #
    role = merged.get(
        "BLAKE_SWAP_ROLE"
    )

    if role == "alice":
        if "BLAKE_SWAP_STATE_DIR" not in merged:
            merged["BLAKE_SWAP_STATE_DIR"] = str(
                ALICE_BASE
            )

        merged.setdefault(
            "BLAKE_SWAP_CHAIN_WALLET",
            ALICE_WALLET,
        )
        merged.setdefault(
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
            str(ALICE_PARTICIPANT_KEY),
        )

    elif role == "bob":
        if "BLAKE_SWAP_STATE_DIR" not in merged:
            merged["BLAKE_SWAP_STATE_DIR"] = str(
                BOB_BASE
            )

        merged.setdefault(
            "BLAKE_SWAP_CHAIN_WALLET",
            BOB_WALLET,
        )
        merged.setdefault(
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
            str(BOB_PARTICIPANT_KEY),
        )

    #
    # All file-transport participants communicate through
    # the same test transport directory unless a particular
    # test intentionally overrides it.
    #
    merged.setdefault(
        "BLAKE_SWAP_MESSAGE_DIR",
        str(MESSAGE_DIR),
    )

    p = subprocess.run(
        [str(x) for x in cmd],
        text=True,
        capture_output=True,
        env=merged,
    )

    if echo or (expect_ok and p.returncode != 0):
        if p.stdout:
            print(p.stdout, end="" if p.stdout.endswith("\n") else "\n")
        if p.stderr:
            print(p.stderr, end="" if p.stderr.endswith("\n") else "\n")

    if expect_ok and p.returncode != 0:
        raise TestFailure(
            f"command failed ({p.returncode}): {' '.join(map(str, cmd))}\n"
            f"{p.stderr.strip() or p.stdout.strip()}"
        )

    return p


ensure_test_participant_keys()


def swap_cmd(*args, role=None, expect_ok=True, echo=False):
    env = {}

    # Tests must never discover or publish against the
    # operator's configured public Nostr relays.
    env["BLAKE_SWAP_NOSTR_RELAYS"] = "ws://127.0.0.1:1"

    env["BLAKE_SWAP_MESSAGE_DIR"] = str(
        MESSAGE_DIR
    )

    if role:
        env["BLAKE_SWAP_ROLE"] = role

        if role == "alice":
            env["BLAKE_SWAP_STATE_DIR"] = str(
                ALICE_BASE
            )
            env["BLAKE_SWAP_CHAIN_WALLET"] = ALICE_WALLET
            env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
                ALICE_PARTICIPANT_KEY
            )

        elif role == "bob":
            env["BLAKE_SWAP_STATE_DIR"] = str(
                BOB_BASE
            )
            env["BLAKE_SWAP_CHAIN_WALLET"] = BOB_WALLET
            env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
                BOB_PARTICIPANT_KEY
            )
        else:
            raise TestFailure(
                f"unknown swap role: {role}"
            )

    return run(
        [sys.executable, SWAP_V2, *args],
        env=env,
        expect_ok=expect_ok,
        echo=echo,
    )


def btc_cli(*args, wallet=None):
    cmd = [
        BTC_CLI,
        f"-datadir={BTC_DATADIR}",
    ]
    if wallet:
        cmd.append(f"-rpcwallet={wallet}")
    cmd.extend(str(x) for x in args)
    p = run(cmd)
    out = p.stdout.strip()
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return out


def blake_cli(*args, wallet=None):
    cmd = [
        BLAKE_CLI,
        "-regtest",
        f"-datadir={BLAKE_DATADIR}",
    ]
    if wallet:
        cmd.append(f"-rpcwallet={wallet}")
    cmd.extend(str(x) for x in args)
    p = run(cmd)
    out = p.stdout.strip()
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return out


def mine_btc(blocks: int) -> None:
    if blocks <= 0:
        return
    addr = btc_cli("getnewaddress", "", "bech32", wallet=ALICE_WALLET)
    btc_cli("generatetoaddress", blocks, addr)


def mine_blake(blocks: int) -> None:
    if blocks <= 0:
        return
    addr = blake_cli("getnewaddress", "", "bech32", wallet=ALICE_WALLET)
    blake_cli("generatetoaddress", blocks, addr)


def load_state(swap_id: str, role: str) -> dict:
    path = (ALICE_DIR if role == "alice" else BOB_DIR) / f"{swap_id}.json"
    require(path.exists(), f"missing {role} state file: {path}")
    return json.loads(path.read_text())


def parse_swap_id(output: str) -> str:
    m = re.search(r"swap id:\s*([0-9a-fA-F]{12})", output)
    if not m:
        raise TestFailure("could not parse swap id from `new` output")
    return m.group(1).lower()


def make_fresh_swap(*, blake=0.1, sats=25_000) -> str:
    btc_height = int(btc_cli("getblockcount"))
    blake_height = int(blake_cli("getblockcount"))

    # Regtest-only spacing, matching the current swap_v2 policy.
    btc_timeout = btc_height + 8
    blake_timeout = max(blake_height + 16, btc_timeout + 30)

    p = swap_cmd(
        "new",
        "--blake", str(blake),
        "--sats", str(sats),
        "--btc-timeout", str(btc_timeout),
        "--blake-timeout", str(blake_timeout),
        role="alice",
    )
    swap_id = parse_swap_id(p.stdout)
    return swap_id


def assert_state(swap_id: str, role: str, expected: str) -> None:
    actual = load_state(swap_id, role)["state"]
    require(actual == expected, f"{role} state {actual!r}, expected {expected!r}")


def assert_secret_separation(swap_id: str) -> None:
    alice = load_state(swap_id, "alice")
    bob = load_state(swap_id, "bob")

    require(alice.get("secret_hex"), "Alice lost original secret")
    require(bob.get("secret_hex") is None, "Bob improperly received Alice secret_hex")

    for path in MESSAGE_DIR.glob(f"{swap_id}.*.json"):
        data = json.loads(path.read_text())

        if (
            isinstance(data, dict)
            and isinstance(data.get("message"), dict)
        ):
            payload = data["message"]
        else:
            payload = data

        require(
            "secret_hex" not in payload,
            f"secret_hex leaked through transport message {path.name}",
        )


def handshake(swap_id: str) -> None:
    swap_cmd("publish-offer", swap_id, role="alice")
    swap_cmd("accept-offer", swap_id, role="bob")
    swap_cmd("receive-accept", swap_id, role="alice")

    a = load_state(swap_id, "alice")
    b = load_state(swap_id, "bob")

    require(a.get("blake_bob_pub"), "Alice did not learn Bob BLAKE pubkey")
    require(a.get("btc_bob_pub"), "Alice did not learn Bob BTC pubkey")
    require(b.get("blake_alice_pub"), "Bob did not learn Alice BLAKE pubkey")
    require(b.get("btc_alice_pub"), "Bob did not learn Alice BTC pubkey")


def fund_blake_and_sync(swap_id: str) -> None:
    swap_cmd("fund-blake", swap_id, role="alice")
    assert_state(swap_id, "alice", "BLAKE_FUNDED")

    swap_cmd("publish-blake-funded", swap_id, role="alice")
    swap_cmd("receive-blake-funded", swap_id, role="bob")
    assert_state(swap_id, "bob", "BLAKE_FUNDED")

    # Replay should be a safe no-op.
    swap_cmd("receive-blake-funded", swap_id, role="bob")
    assert_state(swap_id, "bob", "BLAKE_FUNDED")


def fund_btc_and_sync(swap_id: str) -> None:
    swap_cmd("fund-btc", swap_id, role="bob")
    assert_state(swap_id, "bob", "BTC_FUNDED")

    swap_cmd("publish-btc-funded", swap_id, role="bob")
    swap_cmd("receive-btc-funded", swap_id, role="alice")
    assert_state(swap_id, "alice", "BTC_FUNDED")

    # Replay should be a safe no-op.
    swap_cmd("receive-btc-funded", swap_id, role="alice")
    assert_state(swap_id, "alice", "BTC_FUNDED")


def test_timeout_refresh() -> None:
    banner("TIMEOUT REFRESH SUITE V1")

    #
    # 1. Refresh an unfunded v1.5 swap.
    #
    swap_id = make_fresh_swap()

    before = load_state(
        swap_id,
        "alice",
    )

    original_secret = before["secret_hex"]
    original_hash = before["secret_hash"]
    original_blake = before["blake_amount"]
    original_sats = before["btc_sats"]

    swap_cmd(
        "refresh-timeouts",
        swap_id,
        role="alice",
    )

    after = load_state(
        swap_id,
        "alice",
    )

    require(
        after["swap_id"] == swap_id,
        "timeout refresh changed swap_id",
    )

    require(
        after["secret_hex"] == original_secret,
        "timeout refresh changed Alice secret S",
    )

    require(
        after["secret_hash"] == original_hash,
        "timeout refresh changed hash H",
    )

    require(
        after["blake_amount"] == original_blake,
        "timeout refresh changed BLAKE amount",
    )

    require(
        after["btc_sats"] == original_sats,
        "timeout refresh changed BTC amount",
    )

    require(
        after["state"] == "OFFERED",
        "timeout refresh changed swap state",
    )

    require(
        after.get("timeout_policy_version")
        == "1.5",
        "timeout refresh did not attach v1.5 policy",
    )

    ok("unfunded refresh preserves swap_id / S / H / terms")

    #
    # 2. Repeated refresh while unfunded is safe.
    #
    first = load_state(
        swap_id,
        "alice",
    )

    swap_cmd(
        "refresh-timeouts",
        swap_id,
        role="alice",
    )

    second = load_state(
        swap_id,
        "alice",
    )

    require(
        second["secret_hex"]
        == first["secret_hex"],
        "repeated refresh changed S",
    )

    require(
        second["secret_hash"]
        == first["secret_hash"],
        "repeated refresh changed H",
    )

    require(
        second["state"] == "OFFERED",
        "repeated refresh changed state",
    )

    ok("repeated unfunded refresh is safe")

    #
    # 3. A BLAKE funding-intent marker must block
    #    timeout mutation even while state is OFFERED.
    #
    intent_dir = ALICE_BASE / "funding-intents"
    intent_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    blake_intent = (
        intent_dir
        / f"{swap_id}.blake.json"
    )

    blake_intent.write_text(
        json.dumps(
            {
                "swap_id": swap_id,
                "chain": "blake",
                "address": "test-only",
                "amount": str(
                    second["blake_amount"]
                ),
                "secret_hash":
                    second["secret_hash"],
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )

    try:
        proc = swap_cmd(
            "refresh-timeouts",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        output = (
            proc.stdout
            + proc.stderr
        )

        require(
            proc.returncode != 0,
            "refresh unexpectedly accepted "
            "BLAKE funding intent",
        )

        require(
            "blake funding intent exists"
            in output,
            "wrong failure for BLAKE "
            "funding-intent refresh",
        )

    finally:
        blake_intent.unlink(
            missing_ok=True
        )

    ok("BLAKE funding intent blocks refresh")

    #
    # 4. A BTC funding-intent marker must do the same.
    #
    btc_intent = (
        intent_dir
        / f"{swap_id}.btc.json"
    )

    btc_intent.write_text(
        json.dumps(
            {
                "swap_id": swap_id,
                "chain": "btc",
                "address": "test-only",
                "amount": str(
                    second["btc_sats"]
                ),
                "secret_hash":
                    second["secret_hash"],
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )

    try:
        proc = swap_cmd(
            "refresh-timeouts",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        output = (
            proc.stdout
            + proc.stderr
        )

        require(
            proc.returncode != 0,
            "refresh unexpectedly accepted "
            "BTC funding intent",
        )

        require(
            "btc funding intent exists"
            in output,
            "wrong failure for BTC "
            "funding-intent refresh",
        )

    finally:
        btc_intent.unlink(
            missing_ok=True
        )

    ok("BTC funding intent blocks refresh")

    #
    # 5. Once Bob accepts the refreshed offer,
    #    Alice may no longer alter the deadlines.
    #
    handshake(
        swap_id
    )

    proc = swap_cmd(
        "refresh-timeouts",
        swap_id,
        role="alice",
        expect_ok=False,
    )

    output = (
        proc.stdout
        + proc.stderr
    )

    require(
        proc.returncode != 0,
        "refresh succeeded after counterparty acceptance",
    )

    require(
        "after counterparty acceptance"
        in output,
        "wrong failure after counterparty acceptance",
    )

    ok("refresh rejected after counterparty acceptance")

    #
    # 6. Once BLAKE really funds, Alice must no
    #    longer be allowed to alter deadlines.
    #
    fund_blake_and_sync(
        swap_id
    )

    proc = swap_cmd(
        "refresh-timeouts",
        swap_id,
        role="alice",
        expect_ok=False,
    )

    output = (
        proc.stdout
        + proc.stderr
    )

    require(
        proc.returncode != 0,
        "refresh succeeded after BLAKE funding",
    )

    require(
        "requires state OFFERED"
        in output,
        "wrong failure after BLAKE funding",
    )

    ok("refresh rejected after BLAKE funding")

    #
    # 6. Also prove rejection after both sides fund.
    #
    fund_btc_and_sync(
        swap_id
    )

    proc = swap_cmd(
        "refresh-timeouts",
        swap_id,
        role="alice",
        expect_ok=False,
    )

    output = (
        proc.stdout
        + proc.stderr
    )

    require(
        proc.returncode != 0,
        "refresh succeeded after BTC funding",
    )

    require(
        "requires state OFFERED"
        in output,
        "wrong failure after BTC funding",
    )

    ok("refresh rejected after BTC funding")

    print(
        "timeout-refresh swap:",
        swap_id,
    )


def test_happy() -> None:
    banner("HAPPY PATH")
    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    ok("handshake")

    fund_blake_and_sync(swap_id)
    ok("BLAKE funding + duplicate receive")

    fund_btc_and_sync(swap_id)
    ok("BTC funding + duplicate receive")

    assert_secret_separation(swap_id)
    ok("secret absent from Bob state and transport")

    swap_cmd("claim-btc", swap_id, role="alice")
    assert_state(swap_id, "alice", "BTC_CLAIMED")
    ok("Alice claims BTC")

    swap_cmd("publish-btc-claimed", swap_id, role="alice")
    swap_cmd("receive-btc-claimed", swap_id, role="bob")
    assert_state(swap_id, "bob", "BTC_CLAIMED")
    ok("Bob independently imports BTC claim")

    # Duplicate claim announcement must remain safe.
    swap_cmd("receive-btc-claimed", swap_id, role="bob")
    assert_state(swap_id, "bob", "BTC_CLAIMED")
    ok("duplicate BTC claim receive")

    before = load_state(swap_id, "bob")
    require(before.get("revealed_secret_hex") is None, "Bob knew S before extraction")

    swap_cmd("extract-secret", swap_id, role="bob")
    bob = load_state(swap_id, "bob")
    revealed = bob.get("revealed_secret_hex")
    require(revealed, "Bob failed to persist revealed secret")
    require(bob.get("secret_hex") is None, "Bob's original-secret field became populated")
    ok("Bob extracts S from Bitcoin only")

    swap_cmd("claim-blake", swap_id, role="bob")
    assert_state(swap_id, "bob", "BLAKE_CLAIMED")
    ok("Bob claims BLAKE with recovered S")

    swap_cmd("publish-blake-claimed", swap_id, role="bob")
    swap_cmd("receive-blake-claimed", swap_id, role="alice")
    assert_state(swap_id, "alice", "BLAKE_CLAIMED")
    ok("Alice independently imports BLAKE claim")

    # Duplicate claim announcement must remain safe.
    swap_cmd("receive-blake-claimed", swap_id, role="alice")
    assert_state(swap_id, "alice", "BLAKE_CLAIMED")
    ok("duplicate BLAKE claim receive")

    swap_cmd(
        "complete",
        swap_id,
        "--role",
        "alice",
        role="alice",
    )

    swap_cmd(
        "complete",
        swap_id,
        "--role",
        "bob",
        role="bob",
    )
    assert_state(swap_id, "alice", "COMPLETE")
    assert_state(swap_id, "bob", "COMPLETE")
    ok("both sides COMPLETE")

    assert_secret_separation(swap_id)
    ok("secret never transported")

    print("happy swap:", swap_id)



def test_reorg_btc_claim() -> None:
    banner("REORG SAFETY: BTC CLAIM ROLLBACK")

    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)

    swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    alice = load_state(
        swap_id,
        "alice",
    )

    claim_blockhash = alice.get(
        "btc_claim_blockhash"
    )

    require(
        bool(claim_blockhash),
        "Alice has no BTC claim blockhash",
    )

    swap_cmd(
        "publish-btc-claimed",
        swap_id,
        role="alice",
    )

    swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    bob = load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BTC_CLAIMED",
        "Bob did not reach BTC_CLAIMED",
    )

    require(
        bool(bob.get("revealed_secret_hex")),
        "Bob did not recover S",
    )

    btc_cli(
        "invalidateblock",
        claim_blockhash,
    )

    try:
        proc = swap_cmd(
            "claim-blake",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        output = (
            (proc.stdout or "")
            + "\n"
            + (proc.stderr or "")
        )

        require(
            proc.returncode != 0,
            "Bob claimed BLAKE after BTC claim reorg",
        )

        require(
            (
                "Bitcoin claim is no longer on the active chain"
                in output
                or
                "Bitcoin claim is not confirmed"
                in output
            ),
            (
                "claim-blake failed for unexpected reason:\n"
                + output[-1600:]
            ),
        )

        ok(
            "Bob refuses BLAKE claim after "
            "BTC claim confirmation rollback"
        )

    finally:
        btc_cli(
            "reconsiderblock",
            claim_blockhash,
        )

    print("reorg swap:", swap_id)



def test_reorg_btc_claim() -> None:
    banner("REORG SAFETY: BTC CLAIM ROLLBACK")

    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)

    swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    alice = load_state(
        swap_id,
        "alice",
    )

    claim_blockhash = alice.get(
        "btc_claim_blockhash"
    )

    require(
        bool(claim_blockhash),
        "Alice has no BTC claim blockhash",
    )

    swap_cmd(
        "publish-btc-claimed",
        swap_id,
        role="alice",
    )

    swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    bob = load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BTC_CLAIMED",
        "Bob did not reach BTC_CLAIMED",
    )

    require(
        bool(bob.get("revealed_secret_hex")),
        "Bob did not recover S",
    )

    btc_cli(
        "invalidateblock",
        claim_blockhash,
    )

    try:
        proc = swap_cmd(
            "claim-blake",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        output = (
            (proc.stdout or "")
            + "\n"
            + (proc.stderr or "")
        )

        require(
            proc.returncode != 0,
            "Bob claimed BLAKE after BTC claim reorg",
        )

        require(
            (
                "Bitcoin claim is no longer on the active chain"
                in output
                or
                "Bitcoin claim is not confirmed"
                in output
            ),
            (
                "claim-blake failed for unexpected reason:\n"
                + output[-1600:]
            ),
        )

        ok(
            "Bob refuses BLAKE claim after "
            "BTC claim confirmation rollback"
        )

    finally:
        btc_cli(
            "reconsiderblock",
            claim_blockhash,
        )

    print("reorg swap:", swap_id)



def test_reorg_btc_refund() -> None:
    banner("REORG SAFETY: BTC REFUND ROLLBACK")

    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)

    bob = load_state(
        swap_id,
        "bob",
    )

    btc_height = int(
        btc_cli("getblockcount")
    )

    blocks = (
        int(bob["btc_timeout"])
        - btc_height
    )

    if blocks > 0:
        mine_btc(blocks)

    swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
    )

    bob = load_state(
        swap_id,
        "bob",
    )

    refund_blockhash = bob.get(
        "btc_refund_blockhash"
    )

    require(
        bool(refund_blockhash),
        "Bob has no BTC refund blockhash",
    )

    swap_cmd(
        "publish-btc-refunded",
        swap_id,
        role="bob",
    )

    swap_cmd(
        "receive-btc-refunded",
        swap_id,
        role="alice",
    )

    alice = load_state(
        swap_id,
        "alice",
    )

    require(
        alice["state"] == "BTC_REFUNDED",
        "Alice did not reach BTC_REFUNDED",
    )

    btc_cli(
        "invalidateblock",
        refund_blockhash,
    )

    try:
        alice = load_state(
            swap_id,
            "alice",
        )

        require(
            alice["state"] == "BTC_REFUNDED",
            "Alice's durable state unexpectedly changed",
        )

        blake_height = int(
            blake_cli("getblockcount")
        )

        blake_blocks = (
            int(alice["blake_timeout"])
            - blake_height
        )

        if blake_blocks > 0:
            mine_blake(blake_blocks)

        proc = swap_cmd(
            "refund-blake",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        output = (
            (proc.stdout or "")
            + "\n"
            + (proc.stderr or "")
        )

        require(
            proc.returncode != 0,
            "Alice refunded BLAKE after BTC refund reorg",
        )

        require(
            (
                "Bitcoin refund is no longer on the active chain"
                in output
                or
                "Bitcoin refund is not confirmed"
                in output
            ),
            (
                "refund-blake failed for unexpected reason:\n"
                + output[-1600:]
            ),
        )

        ok(
            "Alice refuses BLAKE refund after "
            "BTC refund confirmation rollback"
        )

    finally:
        btc_cli(
            "reconsiderblock",
            refund_blockhash,
        )

    print("reorg refund swap:", swap_id)


def test_refund() -> None:
    banner("TWO-FUNDED REFUND PATH")
    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)
    ok("reached BTC_FUNDED on both sides")

    # Preparing/refusing the exact refund before timeout is itself a test.
    early = swap_cmd("refund-btc", swap_id, role="bob")
    require(
        "early Bitcoin refund rejected as non-final" in early.stdout,
        "early BTC refund did not prove non-final rejection",
    )
    assert_state(swap_id, "bob", "BTC_FUNDED")
    ok("early BTC refund rejected")

    bob = load_state(swap_id, "bob")
    current_btc = int(btc_cli("getblockcount"))
    to_btc_timeout = int(bob["btc_timeout"]) - current_btc
    if to_btc_timeout > 0:
        mine_btc(to_btc_timeout)

    swap_cmd("refund-btc", swap_id, role="bob")
    assert_state(swap_id, "bob", "BTC_REFUNDED")
    ok("Bob BTC refund")

    swap_cmd("publish-btc-refunded", swap_id, role="bob")
    swap_cmd("receive-btc-refunded", swap_id, role="alice")
    assert_state(swap_id, "alice", "BTC_REFUNDED")
    ok("Alice independently imports BTC refund")

    # Duplicate refund receive must be independently re-verified.
    swap_cmd("receive-btc-refunded", swap_id, role="alice")
    assert_state(swap_id, "alice", "BTC_REFUNDED")
    ok("duplicate BTC refund receive")

    alice = load_state(swap_id, "alice")
    current_blake = int(blake_cli("getblockcount"))
    to_blake_timeout = int(alice["blake_timeout"]) - current_blake

    if to_blake_timeout > 0:
        # Exercise the pre-timeout BLAKE refund behavior first.
        early_blake = swap_cmd("refund-blake", swap_id, role="alice")
        require(
            "Refund is correctly timelocked." in early_blake.stdout,
            "early BLAKE refund did not report timelock",
        )
        assert_state(swap_id, "alice", "BTC_REFUNDED")
        ok("early BLAKE refund remains timelocked")

        mine_blake(to_blake_timeout)

    swap_cmd("refund-blake", swap_id, role="alice")
    assert_state(swap_id, "alice", "BLAKE_REFUNDED")
    ok("Alice BLAKE refund")

    assert_secret_separation(swap_id)
    bob = load_state(swap_id, "bob")
    require(
        bob.get("revealed_secret_hex") is None,
        "Bob learned S on refund path",
    )
    ok("refund path revealed no secret")

    print("refund swap:", swap_id)



def message_file(swap_id: str, message_type: str) -> Path:
    return MESSAGE_DIR / f"{swap_id}.{message_type}.json"


def mutate_message(path: Path, mutator):
    if not path.exists():
        raise TestFailure(
            f"message file not found: {path}"
        )

    original = path.read_text()
    data = json.loads(original)

    # FILE transport v2 wraps the protocol payload so tests
    # must mutate the actual message, not transport metadata.
    if (
        isinstance(data, dict)
        and data.get("_file_transport_version") == 2
        and isinstance(data.get("message"), dict)
    ):
        payload = data["message"]
    else:
        payload = data

    mutator(payload)

    path.write_text(
        json.dumps(
            data,
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )

    return original


def restore_message(path: Path, original: str) -> None:
    path.write_text(original)


def expect_failure(name, proc, contains=None):
    require(proc.returncode != 0, f"{name}: unexpectedly succeeded")
    text = (proc.stdout or "") + "\n" + (proc.stderr or "")
    if contains:
        require(
            contains in text,
            f"{name}: expected {contains!r} in failure output, got:\n{text}",
        )


def test_mallory() -> None:
    banner("MALLORY SUITE V1")

    # Illegal transition
    swap_id = make_fresh_swap()
    proc = swap_cmd(
        "advance",
        swap_id,
        "BTC_FUNDED",
        "--role",
        "alice",
        role="alice",
        expect_ok=False,
    )
    expect_failure("illegal transition", proc, "Illegal transition")
    assert_state(swap_id, "alice", "OFFERED")
    ok("illegal OFFERED -> BTC_FUNDED transition rejected")

    # Secret injection + fake BLAKE funding
    swap_id = make_fresh_swap()
    handshake(swap_id)
    swap_cmd("fund-blake", swap_id, role="alice")
    swap_cmd("publish-blake-funded", swap_id, role="alice")

    msg = message_file(swap_id, "blake-funded")

    original = mutate_message(
        msg,
        lambda d: d.__setitem__("secret_hex", "00" * 32),
    )
    try:
        proc = swap_cmd(
            "receive-blake-funded",
            swap_id,
            role="bob",
            expect_ok=False,
        )
        expect_failure("secret injection", proc)
        assert_state(swap_id, "bob", "OFFERED")
    finally:
        restore_message(msg, original)
    ok("secret_hex injection rejected")

    def fake_blake(d):
        d["blake_funding_txid"] = "00" * 32

    original = mutate_message(msg, fake_blake)
    try:
        proc = swap_cmd(
            "receive-blake-funded",
            swap_id,
            role="bob",
            expect_ok=False,
        )
        expect_failure("fake BLAKE funding", proc)
        assert_state(swap_id, "bob", "OFFERED")
    finally:
        restore_message(msg, original)
    ok("fake BLAKE funding txid rejected")

    swap_cmd("receive-blake-funded", swap_id, role="bob")
    assert_state(swap_id, "bob", "BLAKE_FUNDED")

    original = mutate_message(msg, fake_blake)
    try:
        proc = swap_cmd(
            "receive-blake-funded",
            swap_id,
            role="bob",
            expect_ok=False,
        )
        expect_failure("conflicting BLAKE replay", proc)
        assert_state(swap_id, "bob", "BLAKE_FUNDED")
    finally:
        restore_message(msg, original)
    ok("conflicting BLAKE funding replay rejected")

    # Fake BTC funding
    swap_cmd("fund-btc", swap_id, role="bob")
    swap_cmd("publish-btc-funded", swap_id, role="bob")

    btc_msg = message_file(swap_id, "btc-funded")

    def fake_btc(d):
        d["btc_funding_txid"] = "11" * 32

    original = mutate_message(btc_msg, fake_btc)
    try:
        proc = swap_cmd(
            "receive-btc-funded",
            swap_id,
            role="alice",
            expect_ok=False,
        )
        expect_failure("fake BTC funding", proc)
        assert_state(swap_id, "alice", "BLAKE_FUNDED")
    finally:
        restore_message(btc_msg, original)
    ok("fake BTC funding txid rejected")

    swap_cmd("receive-btc-funded", swap_id, role="alice")
    assert_state(swap_id, "alice", "BTC_FUNDED")

    original = mutate_message(btc_msg, fake_btc)
    try:
        proc = swap_cmd(
            "receive-btc-funded",
            swap_id,
            role="alice",
            expect_ok=False,
        )
        expect_failure("conflicting BTC replay", proc)
        assert_state(swap_id, "alice", "BTC_FUNDED")
    finally:
        restore_message(btc_msg, original)
    ok("conflicting BTC funding replay rejected")

    # Wrong-role signer tests. Dead relay ensures proper failure
    # must happen before networking.
    dead = {
        "BLAKE_SWAP_TRANSPORT": "nostr",
        "BLAKE_SWAP_NOSTR_RELAYS": "wss://127.0.0.1:1",
        "BLAKE_SWAP_NOSTR_RELAY": "wss://127.0.0.1:1",
    }

    # Dedicated swap at Alice BLAKE_FUNDED.
    signer_blake_id = make_fresh_swap()
    handshake(signer_blake_id)
    swap_cmd("fund-blake", signer_blake_id, role="alice")

    proc = run(
        [sys.executable, SWAP_V2, "publish-blake-funded", signer_blake_id],
        env={
            **dead,
            "BLAKE_SWAP_ROLE": "bob",
            "BLAKE_SWAP_STATE_DIR": str(ALICE_BASE),
            "BLAKE_SWAP_MESSAGE_DIR": str(MESSAGE_DIR),
        },
        expect_ok=False,
    )
    expect_failure(
        "wrong-role BLAKE funded",
        proc,
        "protocol requires 'alice'",
    )
    ok("wrong-role BLAKE-funded signer rejected")

    # Dedicated swap at Bob BTC_FUNDED.
    signer_btc_id = make_fresh_swap()
    handshake(signer_btc_id)
    fund_blake_and_sync(signer_btc_id)
    swap_cmd("fund-btc", signer_btc_id, role="bob")

    proc = run(
        [sys.executable, SWAP_V2, "publish-btc-funded", signer_btc_id],
        env={
            **dead,
            "BLAKE_SWAP_ROLE": "alice",
            "BLAKE_SWAP_STATE_DIR": str(BOB_BASE),
            "BLAKE_SWAP_MESSAGE_DIR": str(MESSAGE_DIR),
        },
        expect_ok=False,
    )
    expect_failure(
        "wrong-role BTC funded",
        proc,
        "protocol requires 'bob'",
    )
    ok("wrong-role BTC-funded signer rejected")

    # Move that swap to Alice BTC_CLAIMED.
    swap_cmd("publish-btc-funded", signer_btc_id, role="bob")
    swap_cmd("receive-btc-funded", signer_btc_id, role="alice")
    swap_cmd("claim-btc", signer_btc_id, role="alice")

    proc = run(
        [sys.executable, SWAP_V2, "publish-btc-claimed", signer_btc_id],
        env={
            **dead,
            "BLAKE_SWAP_ROLE": "bob",
            "BLAKE_SWAP_STATE_DIR": str(ALICE_BASE),
            "BLAKE_SWAP_MESSAGE_DIR": str(MESSAGE_DIR),
        },
        expect_ok=False,
    )
    expect_failure(
        "wrong-role BTC claimed",
        proc,
        "protocol requires 'alice'",
    )
    ok("wrong-role BTC-claimed signer rejected")

    # Known suspected gap: BTC refund publisher
    refund_id = make_fresh_swap()
    handshake(refund_id)
    fund_blake_and_sync(refund_id)
    fund_btc_and_sync(refund_id)

    bob = load_state(refund_id, "bob")
    now = int(btc_cli("getblockcount"))
    blocks = int(bob["btc_timeout"]) - now
    if blocks > 0:
        mine_btc(blocks)

    swap_cmd("refund-btc", refund_id, role="bob")
    assert_state(refund_id, "bob", "BTC_REFUNDED")

    proc = run(
        [sys.executable, SWAP_V2, "publish-btc-refunded", refund_id],
        env={
            **dead,
            "BLAKE_SWAP_ROLE": "alice",
            "BLAKE_SWAP_STATE_DIR": str(BOB_BASE),
            "BLAKE_SWAP_MESSAGE_DIR": str(MESSAGE_DIR),
        },
        expect_ok=False,
    )

    text = (proc.stdout or "") + "\n" + (proc.stderr or "")

    if "protocol requires 'bob'" in text:
        ok("wrong-role BTC-refunded signer rejected")
    else:
        fail(
            "BTC-refunded signer ownership",
            "wrong-role Alice was not rejected at signing boundary; "
            "publisher likely needs expected_role='bob'",
        )

    print("mallory swaps:", swap_id, refund_id)


def state_file(swap_id: str, role: str) -> Path:
    return (
        ALICE_DIR if role == "alice" else BOB_DIR
    ) / f"{swap_id}.json"


def test_recovery() -> None:
    banner("RECOVERY / CRASH SUITE V1")

    # ------------------------------------------------------------
    # 1. Corrupt state must fail closed.
    # ------------------------------------------------------------
    swap_id = make_fresh_swap()
    path = state_file(swap_id, "alice")
    original = path.read_text()

    try:
        path.write_text('{"this is": ')
        proc = swap_cmd(
            "show",
            swap_id,
            role="alice",
            expect_ok=False,
        )
        require(
            proc.returncode != 0,
            "corrupt state file was unexpectedly accepted",
        )
    finally:
        path.write_text(original)

    assert_state(swap_id, "alice", "OFFERED")
    ok("corrupt state file fails closed")

    # ------------------------------------------------------------
    # 2. Missing state must fail closed.
    # ------------------------------------------------------------
    original = path.read_text()
    path.unlink()

    try:
        proc = swap_cmd(
            "show",
            swap_id,
            role="alice",
            expect_ok=False,
        )
        require(
            proc.returncode != 0,
            "missing state file was unexpectedly accepted",
        )
    finally:
        path.write_text(original)

    assert_state(swap_id, "alice", "OFFERED")
    ok("missing state file fails closed")

    # ------------------------------------------------------------
    # 3. Simulate crash AFTER BLAKE funding broadcast but BEFORE
    #    local state save.
    #
    # Save Alice's OFFERED state, fund normally, then put the old
    # state back and run fund-blake again.
    #
    # A recovery-safe implementation should discover/reconcile the
    # existing HTLC rather than blindly fund another one.
    # ------------------------------------------------------------
    crash_blake = make_fresh_swap()
    handshake(crash_blake)

    apath = state_file(crash_blake, "alice")
    before = apath.read_text()

    swap_cmd("fund-blake", crash_blake, role="alice")
    funded_once = load_state(crash_blake, "alice")

    first_txid = funded_once.get("blake_funding_txid")
    require(first_txid, "first BLAKE funding did not record txid")

    # Simulated crash: chain knows about funding; disk does not.
    apath.write_text(before)
    assert_state(crash_blake, "alice", "OFFERED")

    proc = swap_cmd(
        "fund-blake",
        crash_blake,
        role="alice",
        expect_ok=False,
    )

    if proc.returncode == 0:
        after = load_state(crash_blake, "alice")
        second_txid = after.get("blake_funding_txid")

        if second_txid and second_txid != first_txid:
            fail(
                "BLAKE crash-window recovery",
                "after simulated crash-before-save, fund-blake "
                "created a SECOND funding transaction instead of "
                "recovering the first one",
            )
        else:
            ok("BLAKE funding recovered idempotently")
    else:
        # A hard refusal is safer than silently funding twice,
        # although later we may want automatic recovery.
        ok("BLAKE crash restart refused duplicate funding")

    # ------------------------------------------------------------
    # 4. Same experiment for Bob's BTC funding.
    # ------------------------------------------------------------
    crash_btc = make_fresh_swap()
    handshake(crash_btc)
    fund_blake_and_sync(crash_btc)

    bpath = state_file(crash_btc, "bob")
    before = bpath.read_text()

    swap_cmd("fund-btc", crash_btc, role="bob")
    funded_once = load_state(crash_btc, "bob")

    first_txid = funded_once.get("btc_funding_txid")
    require(first_txid, "first BTC funding did not record txid")

    # Simulated crash: Bitcoin has funding; Bob's saved state does not.
    bpath.write_text(before)
    assert_state(crash_btc, "bob", "BLAKE_FUNDED")

    proc = swap_cmd(
        "fund-btc",
        crash_btc,
        role="bob",
        expect_ok=False,
    )

    if proc.returncode == 0:
        after = load_state(crash_btc, "bob")
        second_txid = after.get("btc_funding_txid")

        if second_txid and second_txid != first_txid:
            fail(
                "BTC crash-window recovery",
                "after simulated crash-before-save, fund-btc "
                "created a SECOND funding transaction instead of "
                "recovering the first one",
            )
        else:
            ok("BTC funding recovered idempotently")
    else:
        ok("BTC crash restart refused duplicate funding")

    # ------------------------------------------------------------
    # 5. Ordinary restart/reload proof.
    #
    # Every command here is already a new Python process. This
    # explicitly records that fact as a regression result.
    # ------------------------------------------------------------
    restart_id = make_fresh_swap()
    handshake(restart_id)
    fund_blake_and_sync(restart_id)

    # Fresh subprocess reads Bob state and continues.
    swap_cmd("fund-btc", restart_id, role="bob")
    assert_state(restart_id, "bob", "BTC_FUNDED")
    ok("clean restart after BLAKE funding")

    swap_cmd("publish-btc-funded", restart_id, role="bob")
    swap_cmd("receive-btc-funded", restart_id, role="alice")
    assert_state(restart_id, "alice", "BTC_FUNDED")
    ok("clean restart after BTC funding")

    print(
        "recovery swaps:",
        crash_blake,
        crash_btc,
        restart_id,
    )


def test_race() -> None:
    import os
    import sys
    import tempfile
    import time
    from decimal import Decimal

    banner("CONCURRENT BTC FUNDING RACE V1")

    race_swaps = []

    for trial in range(1, 4):
        swap_id = make_fresh_swap()
        race_swaps.append(swap_id)

        handshake(swap_id)
        fund_blake_and_sync(swap_id)

        assert_state(
            swap_id,
            "bob",
            "BLAKE_FUNDED",
        )

        with tempfile.TemporaryDirectory(
            prefix="blake-swap-race-"
        ) as td:
            td = Path(td)
            gate = td / "GO"
            runner = td / "runner.py"

            runner.write_text(
                "\n".join([
                    "import os",
                    "import sys",
                    "import time",
                    "gate = sys.argv[1]",
                    "script = sys.argv[2]",
                    "swap_id = sys.argv[3]",
                    "deadline = time.time() + 15",
                    "while not os.path.exists(gate):",
                    "    if time.time() >= deadline:",
                    "        raise SystemExit(97)",
                    "    time.sleep(0.001)",
                    "os.execv(sys.executable, [",
                    "    sys.executable,",
                    "    script,",
                    "    'fund-btc',",
                    "    swap_id,",
                    "])",
                    "",
                ])
            )

            env = os.environ.copy()
            env["BLAKE_SWAP_TRANSPORT"] = "file"

            env["BLAKE_SWAP_ROLE"] = "bob"
            env["BLAKE_SWAP_CHAIN_WALLET"] = BOB_WALLET
            env["BLAKE_SWAP_STATE_DIR"] = str(
                BOB_BASE
            )

            env["BLAKE_SWAP_MESSAGE_DIR"] = str(
                MESSAGE_DIR
            )
            env["BLAKE_SWAP_TRANSPORT"] = "file"

            cmd = [
                sys.executable,
                str(runner),
                str(gate),
                str(SWAP_V2),
                swap_id,
            ]

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

            out1, err1 = p1.communicate(timeout=60)
            out2, err2 = p2.communicate(timeout=60)

        results = [
            (p1.returncode, out1, err1),
            (p2.returncode, out2, err2),
        ]

        successes = [
            r for r in results
            if r[0] == 0
        ]

        require(
            len(successes) == 1,
            (
                "expected exactly one successful fund-btc; "
                f"return codes={[r[0] for r in results]}\n"
                f"process 1 stdout:\n{out1[-1200:]}\n"
                f"process 1 stderr:\n{err1[-1200:]}\n"
                f"process 2 stdout:\n{out2[-1200:]}\n"
                f"process 2 stderr:\n{err2[-1200:]}"
            ),
        )

        loser = next(
            r for r in results
            if r[0] != 0
        )

        loser_text = (
            (loser[1] or "")
            + "\n"
            + (loser[2] or "")
        )

        require(
            "fund-btc requires state BLAKE_FUNDED"
            in loser_text,
            (
                "losing process failed for unexpected reason:\n"
                + loser_text[-1500:]
            ),
        )

        bob = load_state(
            swap_id,
            "bob",
        )

        require(
            bob["state"] == "BTC_FUNDED",
            f"Bob ended in state {bob['state']}",
        )

        canonical_txid = bob.get(
            "btc_funding_txid"
        )

        canonical_vout = bob.get(
            "btc_funding_vout"
        )

        require(
            bool(canonical_txid),
            "Bob has no canonical BTC funding txid",
        )

        require(
            canonical_vout is not None,
            "Bob has no canonical BTC funding vout",
        )

        wallet_tx = btc_cli(
            "gettransaction",
            canonical_txid,
            wallet="bob",
        )

        raw_hex = wallet_tx.get("hex")

        require(
            bool(raw_hex),
            "canonical funding tx has no wallet hex",
        )

        decoded = btc_cli(
            "decoderawtransaction",
            raw_hex,
        )

        outputs = [
            o for o in decoded.get("vout", [])
            if int(o["n"]) == int(canonical_vout)
        ]

        require(
            len(outputs) == 1,
            "could not locate canonical funding vout",
        )

        output = outputs[0]

        contract_address = (
            output
            .get("scriptPubKey", {})
            .get("address")
        )

        require(
            bool(contract_address),
            "canonical funding output has no address",
        )

        output_sats = int(
            Decimal(str(output["value"]))
            * Decimal(100_000_000)
        )

        require(
            output_sats == int(bob["btc_sats"]),
            (
                "canonical funding amount mismatch: "
                f"{output_sats}"
            ),
        )

        entries = btc_cli(
            "listtransactions",
            "*",
            5000,
            0,
            "true",
            wallet="bob",
        )

        matching_txids = set()

        for entry in entries:
            if entry.get("category") != "send":
                continue

            if entry.get("address") != contract_address:
                continue

            amount = entry.get("amount")

            if amount is None:
                continue

            sats = int(
                Decimal(str(abs(amount)))
                * Decimal(100_000_000)
            )

            if sats != int(bob["btc_sats"]):
                continue

            txid = entry.get("txid")

            if txid:
                matching_txids.add(txid)

        require(
            matching_txids == {canonical_txid},
            (
                "duplicate BTC funding detected; "
                f"canonical={canonical_txid} "
                f"matching={sorted(matching_txids)}"
            ),
        )

        ok(
            f"concurrent BTC funding serialized "
            f"(trial {trial})"
        )

    print(
        "race swaps:",
        " ".join(race_swaps),
    )



def test_market_take_retry_idempotence():
    banner(
        "MARKET TAKE RETRY IDEMPOTENCE V1"
    )

    import importlib.util
    from types import SimpleNamespace

    src_dir = str(SWAP_V2.parent)

    if src_dir not in sys.path:
        sys.path.insert(0, src_dir)

    spec = importlib.util.spec_from_file_location(
        "swap_v2_market_retry_test",
        SWAP_V2,
    )
    require(
        spec is not None
        and spec.loader is not None,
        "could not load swap_v2 module",
    )

    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    offer_id = "11" * 32
    maker_pubkey = "22" * 32
    taker_pubkey = "33" * 32
    take_id = "44" * 32

    offer_event = {
        "id": offer_id,
        "pubkey": maker_pubkey,
        "content": json.dumps(
            {
                "type": "market-offer",
                "version": 2,
                "direction": "blake-for-btc",
                "blake_amount": "0.01",
                "btc_sats": 2000,
                "expires_at": int(time.time()) + 3600,
            }
        ),
    }

    take_event = {
        "id": take_id,
        "pubkey": taker_pubkey,
        "content": json.dumps(
            {
                "type": "market-take",
                "version": 2,
                "offer_id": offer_id,
                "maker": maker_pubkey,
                "taker": taker_pubkey,
                "blake_amount": "0.01",
                "btc_sats": 2000,
            }
        ),
    }

    take_message = json.loads(
        take_event["content"]
    )

    build_count = 0
    cached_take = None
    publish_ids = []

    async def fake_fetch_market_offers(limit=200):
        return [
            {
                "event_id": offer_id,
                "maker": maker_pubkey,
                "blake_amount": "0.01",
                "btc_sats": 2000,
            }
        ]

    async def fake_fetch_event_by_id(event_id):
        require(
            event_id == offer_id,
            "wrong offer requested",
        )
        return offer_event

    def fake_build_market_take_event(
        original,
        offer_message,
    ):
        nonlocal build_count
        build_count += 1

        require(
            original is offer_event,
            "take built from wrong offer event",
        )

        return take_event, take_message

    def fake_prior_market_take(
        requested_offer_id,
        taker_pubkey=None,
    ):
        require(
            requested_offer_id == offer_id,
            "prior-take lookup used wrong offer id",
        )
        return cached_take

    async def fake_publish_event(event):
        nonlocal cached_take

        publish_ids.append(
            event["id"]
        )

        # Model the real publish path:
        # durable local cache happens before relay success.
        if cached_take is None:
            cached_take = event

        # Simulate every configured relay failing.
        raise RuntimeError(
            "No configured Nostr relay accepted the event"
        )

    originals = {
        "resolve_cached_nostr_prefix":
            mod.resolve_cached_nostr_prefix,
        "nostr_fetch_market_offers":
            mod.nostr_fetch_market_offers,
        "nostr_fetch_event_by_id":
            mod.nostr_fetch_event_by_id,
        "verify_nostr_event":
            mod.verify_nostr_event,
        "validate_btc_trade_amount":
            mod.validate_btc_trade_amount,
        "build_market_take_event":
            mod.build_market_take_event,
        "nostr_prior_market_take":
            mod.nostr_prior_market_take,
        "nostr_publish_event":
            mod.nostr_publish_event,
    }

    try:
        mod.resolve_cached_nostr_prefix = (
            lambda value, **kwargs: offer_id
        )
        mod.nostr_fetch_market_offers = (
            fake_fetch_market_offers
        )
        mod.nostr_fetch_event_by_id = (
            fake_fetch_event_by_id
        )
        mod.verify_nostr_event = (
            lambda event: None
        )
        mod.validate_btc_trade_amount = (
            lambda *args, **kwargs: None
        )
        mod.build_market_take_event = (
            fake_build_market_take_event
        )
        mod.nostr_prior_market_take = (
            fake_prior_market_take
        )
        mod.nostr_publish_event = (
            fake_publish_event
        )

        args = SimpleNamespace(
            offer_id=offer_id
        )

        #
        # First attempt creates exactly one signed take.
        # Publication then fails after the event has become
        # locally durable.
        #
        try:
            mod.command_take_market_offer(
                args
            )
        except RuntimeError as e:
            require(
                "No configured Nostr relay accepted"
                in str(e),
                (
                    "first publication failed for "
                    f"unexpected reason: {e}"
                ),
            )
        else:
            raise TestFailure(
                "first failed relay publication "
                "unexpectedly succeeded"
            )

        require(
            build_count == 1,
            (
                "first take did not create exactly "
                f"one event; build_count={build_count}"
            ),
        )

        require(
            cached_take is take_event,
            "first signed take was not retained",
        )

        first_id = cached_take["id"]

        #
        # Second and third attempts must reuse the exact
        # signed event already retained above.
        #
        mod.command_take_market_offer(
            SimpleNamespace(
                offer_id=offer_id
            )
        )

        mod.command_take_market_offer(
            SimpleNamespace(
                offer_id=offer_id
            )
        )

        require(
            build_count == 1,
            (
                "retry created another market-take; "
                f"build_count={build_count}"
            ),
        )

        require(
            cached_take["id"] == first_id,
            "cached take id changed during retry",
        )

        require(
            publish_ids == [
                first_id,
                first_id,
                first_id,
            ],
            (
                "retry did not republish the exact "
                f"same event id: {publish_ids}"
            ),
        )

        ok(
            "first attempt created exactly one signed take"
        )
        ok(
            "second attempt reused identical take id"
        )
        ok(
            "third attempt reused identical take id"
        )
        ok(
            "relay failure cannot create duplicate take events"
        )
        ok(
            "retry path moves no funds"
        )

    finally:
        for name, value in originals.items():
            setattr(
                mod,
                name,
                value,
            )



def preflight() -> None:
    for path, label in [
        (SWAP_V2, "swap_v2.py"),
        (BTC_CLI, "Bitcoin CLI"),
        (BLAKE_CLI, "BLAKE CLI"),
    ]:
        require(path.exists(), f"{label} not found: {path}")

    # Syntax check the target implementation before touching regtest.
    p = run([sys.executable, "-m", "py_compile", SWAP_V2])
    require(p.returncode == 0, "swap_v2.py does not compile")

    # RPC reachability.
    int(btc_cli("getblockcount"))
    int(blake_cli("getblockcount"))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "suite",
        choices=["happy", "refund", "mallory", "recovery", "race", "market-retry", "all"],
        nargs="?",
        default="all",
    )
    args = parser.parse_args()

    started = time.time()

    try:
        preflight()
        ok("preflight")

        if args.suite == "all":
            try:
                test_timeout_refresh()
            except Exception as e:
                fail(
                    "TIMEOUT REFRESH SUITE",
                    str(e),
                )

        if args.suite in ("happy", "all"):
            try:
                test_happy()
            except Exception as e:
                fail("HAPPY PATH", str(e))
                if args.suite != "all":
                    raise

        if args.suite in ("mallory", "all"):
            try:
                test_mallory()
            except Exception as e:
                fail("MALLORY SUITE", str(e))
                if args.suite != "all":
                    raise

        if args.suite in ("recovery", "all"):
            try:
                test_recovery()
            except Exception as e:
                fail("RECOVERY SUITE", str(e))
                if args.suite != "all":
                    raise

        if args.suite in ("race", "all"):
            try:
                test_race()
            except Exception as e:
                fail("CONCURRENT BTC FUNDING RACE", str(e))
                if args.suite != "all":
                    raise

        if args.suite in ("market-retry", "all"):
            try:
                test_market_take_retry_idempotence()
            except Exception as e:
                fail(
                    "MARKET TAKE RETRY IDEMPOTENCE",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite == "all":
            try:
                test_reorg_btc_claim()
            except Exception as e:
                fail(
                    "BTC CLAIM REORG SAFETY",
                    str(e),
                )

        if args.suite == "all":
            try:
                test_reorg_btc_refund()
            except Exception as e:
                fail(
                    "BTC REFUND REORG SAFETY",
                    str(e),
                )

        if args.suite in ("refund", "all"):
            try:
                test_refund()
            except Exception as e:
                fail("REFUND PATH", str(e))
                if args.suite != "all":
                    raise

    except Exception as e:
        if FAIL_COUNT == 0:
            fail("HARNESS", str(e))

    elapsed = time.time() - started

    banner("SUMMARY")
    print(f"passes:  {PASS_COUNT}")
    print(f"failures:{FAIL_COUNT}")
    print(f"elapsed: {elapsed:.1f}s")
    print()
    print("NOTE: this suite uses file transport and mines only regtest blocks.")
    print("It does not publish to public Nostr relays.")

    return 1 if FAIL_COUNT else 0


if __name__ == "__main__":
    raise SystemExit(main())
