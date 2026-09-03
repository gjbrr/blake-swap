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
import time
from pathlib import Path

HOME = Path.home()
SWAP_V2 = Path(os.environ.get("SWAP_V2", HOME / "swap_v2.py"))

BASE = HOME / ".blake-swaps-v2"
ALICE_DIR = BASE / "alice"
BOB_DIR = BASE / "bob"
MESSAGE_DIR = BASE / "messages"

BTC_CLI = Path("/home/gary/bitcoin-core/build/bin/bitcoin-cli")
BTC_DATADIR = Path("/home/gary/bitcoin-swap-regtest")

BLAKE_CLI = Path("/home/gary/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli")
BLAKE_DATADIR = Path("/home/gary/blake-swap-regtest")

ALICE_WALLET = "alice"

PASS_COUNT = 0
FAIL_COUNT = 0


class TestFailure(RuntimeError):
    pass


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
    merged["BLAKE_SWAP_TRANSPORT"] = "file"
    if env:
        merged.update(env)

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


def swap_cmd(*args, role=None, expect_ok=True, echo=False):
    env = {}
    if role:
        env["BLAKE_SWAP_ROLE"] = role
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


def make_fresh_swap(*, blake=10.0, sats=25_000) -> str:
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
        require(
            "secret_hex" not in data,
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

    swap_cmd("complete", swap_id, "--role", "alice")
    swap_cmd("complete", swap_id, "--role", "bob")
    assert_state(swap_id, "alice", "COMPLETE")
    assert_state(swap_id, "bob", "COMPLETE")
    ok("both sides COMPLETE")

    assert_secret_separation(swap_id)
    ok("secret never transported")

    print("happy swap:", swap_id)


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
        raise TestFailure(f"message file not found: {path}")
    original = path.read_text()
    data = json.loads(original)
    mutator(data)
    path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
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
        env={**dead, "BLAKE_SWAP_ROLE": "bob"},
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
        env={**dead, "BLAKE_SWAP_ROLE": "alice"},
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
        env={**dead, "BLAKE_SWAP_ROLE": "bob"},
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
        env={**dead, "BLAKE_SWAP_ROLE": "alice"},
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
                f"return codes={[r[0] for r in results]}"
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
        choices=["happy", "refund", "mallory", "recovery", "race", "all"],
        nargs="?",
        default="all",
    )
    args = parser.parse_args()

    started = time.time()

    try:
        preflight()
        ok("preflight")

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
