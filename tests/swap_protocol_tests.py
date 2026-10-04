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
from decimal import Decimal
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path

#
# This module is also imported as a library by other suites. Make sure
# its sibling helper module is importable regardless of the caller's
# working directory or how the suite was invoked.
#
sys.path.insert(0, str(Path(__file__).resolve().parent))

import node_config  # noqa: E402

HOME = node_config.HOME

PROJECT_DIR = node_config.PROJECT_DIR

SWAP_V2 = node_config.SWAP_V2

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


#
# Chain node configuration.
#
# Resolved by tests/node_config.py: environment first, then conventional
# locations. The harness re-exports these names so dependent suites can
# keep using h.BTC_CLI and friends. See tests/README.md.
#
# The BLAKE CLI is deliberately NOT resolved through PATH, because a
# generic `bitcoin-cli` on PATH is almost certainly Bitcoin Core.
#
# Every resolved value is injected into each engine subprocess (see
# `run`), so the harness and the engine always talk to the same nodes.
#
BTC_RPC_ARGS = node_config.BTC_RPC_ARGS
BLAKE_RPC_ARGS = node_config.BLAKE_RPC_ARGS

BTC_CLI = node_config.BTC_CLI
BLAKE_CLI = node_config.BLAKE_CLI
BTC_DATADIR = node_config.BTC_DATADIR
BLAKE_DATADIR = node_config.BLAKE_DATADIR

SKIP_EXIT_CODE = node_config.SKIP_EXIT_CODE

# Re-exported helpers.
skip = node_config.skip
node_env = node_config.node_env
node_config_summary = node_config.node_config_summary
node_config_problems = node_config.node_config_problems

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

    #
    # Point every subprocess — engine or chain CLI — at exactly the same
    # nodes this harness resolved for itself. Without this, the harness
    # and the engine could silently use different nodes whenever the
    # host layout differs from the engine's built-in defaults.
    #
    # setdefault, so an explicit `env` still wins.
    #
    for variable, value in node_env().items():
        merged.setdefault(variable, value)

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


def swap_cmd(
    *args,
    role=None,
    expect_ok=True,
    echo=False,
    extra_env=None,
):
    env = {}

    # Tests must never discover or publish against the
    # operator's configured public Nostr relays.
    env["BLAKE_SWAP_NOSTR_RELAYS"] = "ws://127.0.0.1:1"

    # Test-only CLI surfaces such as `advance` must never be exposed
    # in normal production execution.
    env["BLAKE_SWAP_TEST_MODE"] = "1"

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
            env["BLAKE_SWAP_BLAKE_WALLET"] = ALICE_WALLET
            env["BLAKE_SWAP_BTC_WALLET"] = ALICE_WALLET
            env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
                ALICE_PARTICIPANT_KEY
            )

        elif role == "bob":
            env["BLAKE_SWAP_STATE_DIR"] = str(
                BOB_BASE
            )
            env["BLAKE_SWAP_CHAIN_WALLET"] = BOB_WALLET
            env["BLAKE_SWAP_BLAKE_WALLET"] = BOB_WALLET
            env["BLAKE_SWAP_BTC_WALLET"] = BOB_WALLET
            env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
                BOB_PARTICIPANT_KEY
            )
        else:
            raise TestFailure(
                f"unknown swap role: {role}"
            )

    if extra_env:
        env.update(extra_env)

    return run(
        [sys.executable, SWAP_V2, *args],
        env=env,
        expect_ok=expect_ok,
        echo=echo,
    )


def chain_rpc_arg(value):
    """
    Render one CLI argument the way the engine does.

    bitcoin-cli requires JSON-style lowercase booleans; str(True) sends
    "True", which the node rejects with "Error parsing JSON".
    """
    if value is True:
        return "true"

    if value is False:
        return "false"

    return str(value)


def btc_cli(*args, wallet=None):
    #
    # Mirrors the engine's btc_cli: no hardcoded -regtest, so remote or
    # alternate-chain targets are reachable by putting the required
    # arguments in BLAKE_SWAP_BTC_RPC_ARGS.
    #
    cmd = [str(BTC_CLI)]
    cmd.extend(BTC_RPC_ARGS)
    if BTC_DATADIR is not None:
        cmd.append(f"-datadir={BTC_DATADIR}")
    if wallet:
        cmd.append(f"-rpcwallet={wallet}")
    cmd.extend(chain_rpc_arg(x) for x in args)
    p = run(cmd)
    out = p.stdout.strip()
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return out


def blake_cli(*args, wallet=None):
    #
    # Mirrors the engine's blake_cli, which always targets regtest.
    #
    cmd = [str(BLAKE_CLI), "-regtest"]
    cmd.extend(BLAKE_RPC_ARGS)
    if BLAKE_DATADIR is not None:
        cmd.append(f"-datadir={BLAKE_DATADIR}")
    if wallet:
        cmd.append(f"-rpcwallet={wallet}")
    cmd.extend(chain_rpc_arg(x) for x in args)
    p = run(cmd)
    out = p.stdout.strip()
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return out


def tool_env():
    """
    Environment for the independent-implementation tools in tools/.

    Those tools are separate programs that address the FILE transport
    themselves, so they must be told the same message directory the
    engine under test is using. The runner deliberately exports no
    message directory, so simply inheriting the environment is not
    enough: the tool would write into the default state tree and the
    engine would never see the message.
    """
    env = os.environ.copy()
    env["BLAKE_SWAP_TRANSPORT"] = "file"
    env["BLAKE_SWAP_MESSAGE_DIR"] = str(MESSAGE_DIR)
    env["PYTHONPATH"] = os.pathsep.join([
        str(PROJECT_DIR / "src"),
        str(PROJECT_DIR / "tests"),
    ])

    #
    # The tools address the chain themselves when they verify a
    # settlement, so they need the same nodes the harness resolved.
    # Without this they would fall back to the engine's built-in
    # author-host defaults and fail on any other machine.
    #
    for variable, value in node_env().items():
        env.setdefault(variable, value)

    return env


def ensure_wallet(cli, label: str, wallet: str) -> None:
    """
    Ensure `wallet` exists and is loaded on one chain.

    The suites need two distinct signing wallets per chain (alice and
    bob). Creating them here means a fresh regtest node is enough to run
    the integration suites; no manual wallet setup is required.
    """
    loaded = cli("listwallets")

    if isinstance(loaded, list) and wallet in loaded:
        return

    listing = cli("listwalletdir")
    names = []

    if isinstance(listing, dict):
        names = [
            entry.get("name")
            for entry in listing.get("wallets", [])
            if isinstance(entry, dict)
        ]

    if wallet in names:
        cli("loadwallet", wallet)
        return

    cli("createwallet", wallet)


def trusted_balance(cli, wallet: str) -> float:
    """
    Trusted (mature, spendable) balance of one wallet, in coins.

    The engine refuses to fund an HTLC from untrusted balance, and
    coinbase outputs need 100 confirmations before they mature, so the
    bootstrap below has to mine a maturity run rather than a few blocks.
    """
    try:
        info = cli("getbalances", wallet=wallet)

        if isinstance(info, dict):
            return float(info.get("mine", {}).get("trusted", 0.0))
    except Exception:
        pass

    try:
        info = cli("getwalletinfo", wallet=wallet)

        if isinstance(info, dict):
            return float(info.get("balance", 0.0))
    except Exception:
        pass

    return 0.0


def ensure_wallet_funds(cli, label: str, minimum: float = 10.0) -> None:
    """
    Give the alice and bob wallets mature, spendable coins on one chain.

    The suites use the same node for both participants and rely on
    separate funded wallets, so both must be able to fund an HTLC.
    Alice is funded by mining a maturity run to her own address
    (regtest coinbase matures after 100 blocks); Bob receives a
    transfer, which is trusted after one confirmation.

    Once both wallets are funded this is a no-op, so repeated runs
    against a persistent rig stay cheap.
    """
    alice_ok = trusted_balance(cli, ALICE_WALLET) >= minimum
    bob_ok = trusted_balance(cli, BOB_WALLET) >= minimum

    if alice_ok and bob_ok:
        return

    if not alice_ok:
        addr = cli("getnewaddress", "", "bech32", wallet=ALICE_WALLET)
        cli("generatetoaddress", 101, addr)

    if not bob_ok:
        bob_addr = cli("getnewaddress", "", "bech32", wallet=BOB_WALLET)

        try:
            cli(
                "sendtoaddress",
                bob_addr,
                f"{minimum:.8f}",
                wallet=ALICE_WALLET,
            )
        except Exception as e:
            raise TestFailure(
                f"could not fund the {label} bob wallet from alice: {e}"
            ) from e

        # One confirmation makes the received amount trusted.
        addr = cli("getnewaddress", "", "bech32", wallet=ALICE_WALLET)
        cli("generatetoaddress", 1, addr)

    print(
        f"  {label}: alice="
        f"{trusted_balance(cli, ALICE_WALLET):.8f} "
        f"bob={trusted_balance(cli, BOB_WALLET):.8f}"
    )


def ensure_test_wallets() -> None:
    for wallet in (ALICE_WALLET, BOB_WALLET):
        ensure_wallet(btc_cli, "Bitcoin", wallet)
        ensure_wallet(blake_cli, "BLAKE", wallet)

    # Both wallets must hold mature coins on both chains, otherwise the
    # engine refuses to fund an HTLC.
    ensure_wallet_funds(btc_cli, "Bitcoin")
    ensure_wallet_funds(blake_cli, "BLAKE")


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
    #
    # All negotiable test swaps use the same mandatory v1.5
    # timeout-policy path as production.  Absolute BTC and BLAKE
    # heights are independent; tests that need expiry should read
    # the persisted deadlines and mine to them explicitly.
    #
    p = swap_cmd(
        "new",
        "--blake", str(blake),
        "--sats", str(sats),
        role="alice",
    )

    swap_id = parse_swap_id(
        p.stdout
    )

    state = load_state(
        swap_id,
        "alice",
    )

    require(
        state.get("timeout_policy_version") == "1.5",
        "fresh test swap did not use mandatory v1.5 timeout policy",
    )

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
    original_blake = before["blake_amount_base_units"]
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
        after["blake_amount_base_units"] == original_blake,
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
                "amount": blake_amount_wire(second),
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

    # S is learned from the Bitcoin witness only. A settlement
    # announcement that tries to transport secret_hex must be rejected
    # before durable state advances or Bob learns the secret.
    claim_msg = message_file(
        swap_id,
        "btc-claimed",
    )

    original = mutate_message(
        claim_msg,
        lambda d: d.__setitem__(
            "secret_hex",
            "00" * 32,
        ),
    )

    try:
        proc = swap_cmd(
            "receive-btc-claimed",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        expect_failure(
            "BTC claim secret injection",
            proc,
            "secret_hex",
        )

        assert_state(
            swap_id,
            "bob",
            "BTC_FUNDED",
        )

        rejected = load_state(
            swap_id,
            "bob",
        )

        require(
            rejected.get("revealed_secret_hex") is None,
            "rejected BTC claim transport leaked S into Bob state",
        )

        require(
            rejected.get("btc_claim_txid") is None,
            "rejected BTC claim transport persisted claim txid",
        )

    finally:
        restore_message(
            claim_msg,
            original,
        )

    ok("BTC claim secret_hex injection rejected")

    # A settlement announcement must remain bound to this swap's
    # negotiated hashlock. A different secret_hash must be rejected
    # before any settlement identity or durable state is accepted.
    original = mutate_message(
        claim_msg,
        lambda d: d.__setitem__(
            "secret_hash",
            "11" * 32,
        ),
    )

    try:
        proc = swap_cmd(
            "receive-btc-claimed",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        expect_failure(
            "BTC claim hashlock mismatch",
            proc,
            "secret_hash mismatch",
        )

        assert_state(
            swap_id,
            "bob",
            "BTC_FUNDED",
        )

        rejected = load_state(
            swap_id,
            "bob",
        )

        require(
            rejected.get("btc_claim_txid") is None,
            "wrong-hash BTC claim persisted claim txid",
        )

        require(
            rejected.get("revealed_secret_hex") is None,
            "wrong-hash BTC claim changed Bob's secret knowledge",
        )

    finally:
        restore_message(
            claim_msg,
            original,
        )

    ok("BTC claim secret_hash mismatch rejected")

    # Settlement announcements are also bound to the exact public swap
    # identifier. A message for another swap must not be imported here.
    original = mutate_message(
        claim_msg,
        lambda d: d.__setitem__(
            "swap_id",
            "deadbeefcafe",
        ),
    )

    try:
        proc = swap_cmd(
            "receive-btc-claimed",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        expect_failure(
            "BTC claim swap_id mismatch",
            proc,
            "swap_id",
        )

        assert_state(
            swap_id,
            "bob",
            "BTC_FUNDED",
        )

        rejected = load_state(
            swap_id,
            "bob",
        )

        require(
            rejected.get("btc_claim_txid") is None,
            "wrong-swap BTC claim persisted claim txid",
        )

        require(
            rejected.get("revealed_secret_hex") is None,
            "wrong-swap BTC claim changed Bob's secret knowledge",
        )

    finally:
        restore_message(
            claim_msg,
            original,
        )

    ok("BTC claim swap_id mismatch rejected")

    swap_cmd("receive-btc-claimed", swap_id, role="bob")
    assert_state(swap_id, "bob", "BTC_CLAIMED")
    ok("Bob independently imports BTC claim")

    # Duplicate claim announcement must remain safe.
    swap_cmd("receive-btc-claimed", swap_id, role="bob")
    assert_state(swap_id, "bob", "BTC_CLAIMED")
    ok("duplicate BTC claim receive")

    # A later authenticated message for the same settlement type must
    # not be allowed to replace the already-recorded claim identity.
    claim_msg = message_file(
        swap_id,
        "btc-claimed",
    )

    original = mutate_message(
        claim_msg,
        lambda d: d.__setitem__(
            "btc_claim_txid",
            "cd" * 32,
        ),
    )

    try:
        proc = swap_cmd(
            "receive-btc-claimed",
            swap_id,
            role="bob",
            expect_ok=False,
        )

        expect_failure(
            "conflicting BTC claim replay",
            proc,
            "conflicting BTC claim",
        )

        assert_state(
            swap_id,
            "bob",
            "BTC_CLAIMED",
        )

        after_conflict = load_state(
            swap_id,
            "bob",
        )

        require(
            after_conflict.get("btc_claim_txid")
            != "cd" * 32,
            "conflicting BTC claim replay replaced durable txid",
        )

    finally:
        restore_message(
            claim_msg,
            original,
        )

    ok("conflicting BTC claim replay rejected")

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

    blake_claim_msg = message_file(
        swap_id,
        "blake-claimed",
    )

    # BLAKE settlement announcements must remain bound to this swap's
    # negotiated hashlock.
    original = mutate_message(
        blake_claim_msg,
        lambda d: d.__setitem__(
            "secret_hash",
            "22" * 32,
        ),
    )

    try:
        proc = swap_cmd(
            "receive-blake-claimed",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        expect_failure(
            "BLAKE claim hashlock mismatch",
            proc,
            "secret_hash",
        )

        assert_state(
            swap_id,
            "alice",
            "BTC_CLAIMED",
        )

        rejected = load_state(
            swap_id,
            "alice",
        )

        require(
            rejected.get("blake_claim_txid") is None,
            "wrong-hash BLAKE claim persisted claim txid",
        )

    finally:
        restore_message(
            blake_claim_msg,
            original,
        )

    ok("BLAKE claim secret_hash mismatch rejected")

    # The same announcement must also remain bound to the exact
    # public swap identifier.
    original = mutate_message(
        blake_claim_msg,
        lambda d: d.__setitem__(
            "swap_id",
            "feedfacecafe",
        ),
    )

    try:
        proc = swap_cmd(
            "receive-blake-claimed",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        expect_failure(
            "BLAKE claim swap_id mismatch",
            proc,
            "swap_id",
        )

        assert_state(
            swap_id,
            "alice",
            "BTC_CLAIMED",
        )

        rejected = load_state(
            swap_id,
            "alice",
        )

        require(
            rejected.get("blake_claim_txid") is None,
            "wrong-swap BLAKE claim persisted claim txid",
        )

    finally:
        restore_message(
            blake_claim_msg,
            original,
        )

    ok("BLAKE claim swap_id mismatch rejected")

    swap_cmd("receive-blake-claimed", swap_id, role="alice")
    assert_state(swap_id, "alice", "BLAKE_CLAIMED")
    ok("Alice independently imports BLAKE claim")

    # Duplicate claim announcement must remain safe.
    swap_cmd("receive-blake-claimed", swap_id, role="alice")
    assert_state(swap_id, "alice", "BLAKE_CLAIMED")
    ok("duplicate BLAKE claim receive")

    # A later authenticated message for the same settlement type must
    # not replace the already-recorded BLAKE claim identity.
    blake_claim_msg = message_file(
        swap_id,
        "blake-claimed",
    )

    original = mutate_message(
        blake_claim_msg,
        lambda d: d.__setitem__(
            "blake_claim_txid",
            "ef" * 32,
        ),
    )

    try:
        proc = swap_cmd(
            "receive-blake-claimed",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        expect_failure(
            "conflicting BLAKE claim replay",
            proc,
            "conflicting BLAKE claim",
        )

        assert_state(
            swap_id,
            "alice",
            "BLAKE_CLAIMED",
        )

        after_conflict = load_state(
            swap_id,
            "alice",
        )

        require(
            after_conflict.get("blake_claim_txid")
            != "ef" * 32,
            "conflicting BLAKE claim replay replaced durable txid",
        )

    finally:
        restore_message(
            blake_claim_msg,
            original,
        )

    ok("conflicting BLAKE claim replay rejected")

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


def test_cross_swap_btc_claim_replay() -> None:
    banner("CROSS-SWAP BTC CLAIM REPLAY")

    swap_a = make_fresh_swap()
    print("swap A:", swap_a)

    #
    # Announce/accept A before creating B. `new` deliberately archives
    # superseded unannounced placeholders, so creating both first would
    # retire A before this adversarial test could use it.
    #
    handshake(swap_a)

    swap_b = make_fresh_swap()
    print("swap B:", swap_b)
    handshake(swap_b)

    #
    # Establish two independently valid swaps through BTC_FUNDED.
    #
    for swap_id in (swap_a, swap_b):
        fund_blake_and_sync(swap_id)
        fund_btc_and_sync(swap_id)

    assert_state(swap_a, "bob", "BTC_FUNDED")
    assert_state(swap_b, "bob", "BTC_FUNDED")

    #
    # Alice legitimately claims swap A's Bitcoin HTLC and publishes
    # the resulting confirmed settlement announcement.
    #
    swap_cmd(
        "claim-btc",
        swap_a,
        role="alice",
    )

    swap_cmd(
        "publish-btc-claimed",
        swap_a,
        role="alice",
    )

    source_path = message_file(
        swap_a,
        "btc-claimed",
    )

    require(
        source_path.exists(),
        "swap A BTC claim announcement missing",
    )

    source_envelope = json.loads(
        source_path.read_text()
    )

    #
    # Reframe A's genuine authenticated chain settlement as an
    # announcement for swap B.  B's public metadata remains internally
    # consistent, so rejection must come from independent chain proof:
    # A's claim does not spend B's exact Bitcoin HTLC outpoint.
    #
    replay = json.loads(
        json.dumps(source_envelope)
    )

    if (
        isinstance(replay, dict)
        and replay.get("_file_transport_version") == 2
        and isinstance(replay.get("message"), dict)
    ):
        payload = replay["message"]
    else:
        payload = replay

    b_state = load_state(
        swap_b,
        "bob",
    )

    payload["swap_id"] = swap_b
    payload["secret_hash"] = b_state["secret_hash"]

    destination_path = message_file(
        swap_b,
        "btc-claimed",
    )

    original_destination = (
        destination_path.read_text()
        if destination_path.exists()
        else None
    )

    try:
        destination_path.write_text(
            json.dumps(
                replay,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        )

        proc = swap_cmd(
            "receive-btc-claimed",
            swap_b,
            role="bob",
            expect_ok=False,
        )

        expect_failure(
            "cross-swap BTC claim replay",
            proc,
            "does not spend this swap's HTLC",
        )

        assert_state(
            swap_b,
            "bob",
            "BTC_FUNDED",
        )

        rejected = load_state(
            swap_b,
            "bob",
        )

        require(
            rejected.get("btc_claim_txid") is None,
            "cross-swap BTC claim replay persisted claim txid",
        )

        require(
            rejected.get("btc_claim_blockhash") is None,
            "cross-swap BTC claim replay persisted claim blockhash",
        )

        require(
            rejected.get("revealed_secret_hex") is None,
            "cross-swap BTC claim replay changed Bob's secret knowledge",
        )

    finally:
        if original_destination is None:
            destination_path.unlink(
                missing_ok=True
            )
        else:
            destination_path.write_text(
                original_destination
            )

    ok(
        "genuine BTC claim from another swap rejected by exact HTLC binding"
    )

    #
    # Now advance A legitimately far enough for Bob to claim its BLAKE
    # HTLC.  Bob learns A's secret only from A's confirmed Bitcoin claim.
    #
    swap_cmd(
        "receive-btc-claimed",
        swap_a,
        role="bob",
    )

    swap_cmd(
        "extract-secret",
        swap_a,
        role="bob",
    )

    swap_cmd(
        "claim-blake",
        swap_a,
        role="bob",
    )

    swap_cmd(
        "publish-blake-claimed",
        swap_a,
        role="bob",
    )

    #
    # Alice must independently reach BTC_CLAIMED on B before a BLAKE
    # settlement announcement could legitimately advance her state.
    #
    swap_cmd(
        "claim-btc",
        swap_b,
        role="alice",
    )

    assert_state(
        swap_b,
        "alice",
        "BTC_CLAIMED",
    )

    source_path = message_file(
        swap_a,
        "blake-claimed",
    )

    require(
        source_path.exists(),
        "swap A BLAKE claim announcement missing",
    )

    source_envelope = json.loads(
        source_path.read_text()
    )

    #
    # Present A's genuine confirmed BLAKE settlement as though it
    # belonged to B.  Keep B's public swap metadata valid so the
    # decisive rejection must come from exact on-chain HTLC binding.
    #
    replay = json.loads(
        json.dumps(source_envelope)
    )

    if (
        isinstance(replay, dict)
        and replay.get("_file_transport_version") == 2
        and isinstance(replay.get("message"), dict)
    ):
        payload = replay["message"]
    else:
        payload = replay

    b_alice = load_state(
        swap_b,
        "alice",
    )

    payload["swap_id"] = swap_b
    payload["secret_hash"] = b_alice["secret_hash"]

    destination_path = message_file(
        swap_b,
        "blake-claimed",
    )

    original_destination = (
        destination_path.read_text()
        if destination_path.exists()
        else None
    )

    try:
        destination_path.write_text(
            json.dumps(
                replay,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        )

        proc = swap_cmd(
            "receive-blake-claimed",
            swap_b,
            role="alice",
            expect_ok=False,
        )

        expect_failure(
            "cross-swap BLAKE claim replay",
            proc,
            "does not spend this swap's HTLC",
        )

        assert_state(
            swap_b,
            "alice",
            "BTC_CLAIMED",
        )

        rejected = load_state(
            swap_b,
            "alice",
        )

        require(
            rejected.get("blake_claim_txid") is None,
            "cross-swap BLAKE claim replay persisted claim txid",
        )

        require(
            rejected.get("blake_claim_blockhash") is None,
            "cross-swap BLAKE claim replay persisted claim blockhash",
        )

    finally:
        if original_destination is None:
            destination_path.unlink(
                missing_ok=True
            )
        else:
            destination_path.write_text(
                original_destination
            )

    ok(
        "genuine BLAKE claim from another swap rejected by exact HTLC binding"
    )


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


def test_cross_swap_btc_refund_replay() -> None:
    banner("CROSS-SWAP BTC REFUND REPLAY")

    swap_a = make_fresh_swap()
    print("swap A:", swap_a)

    # A must be announced before B is created, otherwise `new` may
    # archive A as a superseded unannounced placeholder.
    handshake(swap_a)

    swap_b = make_fresh_swap()
    print("swap B:", swap_b)
    handshake(swap_b)

    for swap_id in (swap_a, swap_b):
        fund_blake_and_sync(swap_id)
        fund_btc_and_sync(swap_id)

    assert_state(
        swap_a,
        "alice",
        "BTC_FUNDED",
    )
    assert_state(
        swap_b,
        "alice",
        "BTC_FUNDED",
    )

    #
    # Produce a genuine confirmed Bitcoin refund for A.
    #
    a_bob = load_state(
        swap_a,
        "bob",
    )

    current_btc = int(
        btc_cli("getblockcount")
    )

    to_timeout = (
        int(a_bob["btc_timeout"])
        - current_btc
    )

    if to_timeout > 0:
        mine_btc(to_timeout)

    swap_cmd(
        "refund-btc",
        swap_a,
        role="bob",
    )

    assert_state(
        swap_a,
        "bob",
        "BTC_REFUNDED",
    )

    swap_cmd(
        "publish-btc-refunded",
        swap_a,
        role="bob",
    )

    source_path = message_file(
        swap_a,
        "btc-refunded",
    )

    require(
        source_path.exists(),
        "swap A BTC refund announcement missing",
    )

    source_envelope = json.loads(
        source_path.read_text()
    )

    #
    # Reframe A's real confirmed refund as a message for B.  Keep B's
    # public bindings valid so rejection must come from independent
    # proof that the transaction spends B's exact HTLC.
    #
    replay = json.loads(
        json.dumps(source_envelope)
    )

    if (
        isinstance(replay, dict)
        and replay.get("_file_transport_version") == 2
        and isinstance(replay.get("message"), dict)
    ):
        payload = replay["message"]
    else:
        payload = replay

    b_alice = load_state(
        swap_b,
        "alice",
    )

    payload["swap_id"] = swap_b
    payload["secret_hash"] = b_alice["secret_hash"]

    destination_path = message_file(
        swap_b,
        "btc-refunded",
    )

    original_destination = (
        destination_path.read_text()
        if destination_path.exists()
        else None
    )

    try:
        destination_path.write_text(
            json.dumps(
                replay,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        )

        proc = swap_cmd(
            "receive-btc-refunded",
            swap_b,
            role="alice",
            expect_ok=False,
        )

        expect_failure(
            "cross-swap BTC refund replay",
            proc,
            "does not spend this swap's HTLC",
        )

        assert_state(
            swap_b,
            "alice",
            "BTC_FUNDED",
        )

        rejected = load_state(
            swap_b,
            "alice",
        )

        require(
            rejected.get("btc_refund_txid") is None,
            "cross-swap BTC refund replay persisted refund txid",
        )

        require(
            rejected.get("btc_refund_blockhash") is None,
            "cross-swap BTC refund replay persisted refund blockhash",
        )

    finally:
        if original_destination is None:
            destination_path.unlink(
                missing_ok=True
            )
        else:
            destination_path.write_text(
                original_destination
            )

    ok(
        "genuine BTC refund from another swap rejected by exact HTLC binding"
    )


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

    # A later authenticated message for the same settlement type must
    # not be allowed to replace the already-recorded refund identity.
    refund_msg = message_file(
        swap_id,
        "btc-refunded",
    )

    original = mutate_message(
        refund_msg,
        lambda d: d.__setitem__(
            "btc_refund_txid",
            "ab" * 32,
        ),
    )

    try:
        proc = swap_cmd(
            "receive-btc-refunded",
            swap_id,
            role="alice",
            expect_ok=False,
        )

        expect_failure(
            "conflicting BTC refund replay",
            proc,
            "conflicting BTC refund",
        )

        assert_state(
            swap_id,
            "alice",
            "BTC_REFUNDED",
        )

        after_conflict = load_state(
            swap_id,
            "alice",
        )

        require(
            after_conflict.get("btc_refund_txid")
            != "ab" * 32,
            "conflicting BTC refund replay replaced durable txid",
        )

    finally:
        restore_message(
            refund_msg,
            original,
        )

    ok("conflicting BTC refund replay rejected")

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


def blake_amount_wire(record) -> str:
    """
    The canonical wire rendering of a stored swap's BLAKE amount.

    State records carry `blake_amount_base_units` as an integer; earlier
    records carried `blake_amount` as a JSON number. Suites that hand
    build wire messages must render the *same* string the engine would,
    so the formatting lives here once instead of being re-derived (and
    drifting) in each suite.
    """
    if record.get("blake_amount_base_units") is not None:
        units = int(record["blake_amount_base_units"])
    else:
        units = int(
            Decimal(str(record["blake_amount"])) * Decimal(100_000_000)
        )

    value = Decimal(units) / Decimal(100_000_000)

    return format(value.normalize(), "f")


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

            #
            # Race subprocesses intentionally bypass swap_cmd(),
            # so reproduce the same participant isolation here.
            #
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

            #
            # Same node resolution as run(): the engine child must not
            # fall back to its built-in author-host defaults.
            #
            for variable, value in node_env().items():
                env.setdefault(variable, value)

            env["BLAKE_SWAP_TRANSPORT"] = "file"
            env["BLAKE_SWAP_NOSTR_RELAYS"] = "ws://127.0.0.1:1"

            env["BLAKE_SWAP_ROLE"] = "bob"
            env["BLAKE_SWAP_STATE_DIR"] = str(
                BOB_BASE
            )

            env["BLAKE_SWAP_CHAIN_WALLET"] = BOB_WALLET
            env["BLAKE_SWAP_BLAKE_WALLET"] = BOB_WALLET
            env["BLAKE_SWAP_BTC_WALLET"] = BOB_WALLET

            env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
                BOB_PARTICIPANT_KEY
            )

            env["BLAKE_SWAP_MESSAGE_DIR"] = str(
                MESSAGE_DIR
            )

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
    """
    Verify that this suite can run.

    Missing node configuration or an unreachable node is a *prerequisite*
    problem, not a test failure, so it skips with exit code 77 and an
    actionable diagnostic. A broken repository or an engine that does
    not compile is a genuine failure.
    """

    problems = node_config_problems()

    if problems:
        skip(
            "chain node prerequisites are not satisfied",
            details=node_config_summary() + [""] + problems,
            hint=(
                "See tests/README.md for how to point the suites at "
                "your nodes."
            ),
        )

    require(
        SWAP_V2.exists(),
        f"swap_v2.py not found: {SWAP_V2}",
    )

    # Syntax check the target implementation before touching regtest.
    p = run([sys.executable, "-m", "py_compile", SWAP_V2])
    require(p.returncode == 0, "swap_v2.py does not compile")

    # RPC reachability: configured but not running is a prerequisite
    # problem, not a test failure.
    try:
        int(btc_cli("getblockcount"))
    except Exception as e:
        skip(
            "Bitcoin RPC is not reachable",
            details=node_config_summary() + ["", str(e)],
        )

    try:
        int(blake_cli("getblockcount"))
    except Exception as e:
        skip(
            "BLAKE RPC is not reachable",
            details=node_config_summary() + ["", str(e)],
        )

    #
    # The suites need two separate signing wallets on each chain. Create
    # them if they are missing so that a fresh regtest node is enough.
    #
    try:
        ensure_test_wallets()
    except Exception as e:
        require(
            False,
            "could not create or load the alice/bob test wallets: "
            f"{e}",
        )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "suite",
        choices=[
            "happy",
            "refund",
            "mallory",
            "recovery",
            "race",
            "market-retry",
            "market-roles",
            "reverse-market",
            "role-guard",
            "pending-btc-claim",
            "pending-blake-claim",
            "pending-btc-refund",
            "pending-blake-refund",
            "reorg-btc-claim",
            "reorg-btc-refund",
            "cross-swap-claim",
            "cross-swap-refund",
            "all",
        ],
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

        if args.suite in ("cross-swap-claim", "all"):
            try:
                test_cross_swap_btc_claim_replay()
            except Exception as e:
                fail(
                    "CROSS-SWAP BTC CLAIM REPLAY",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("cross-swap-refund", "all"):
            try:
                test_cross_swap_btc_refund_replay()
            except Exception as e:
                fail(
                    "CROSS-SWAP BTC REFUND REPLAY",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("pending-blake-refund", "all"):
            try:
                test_pending_blake_refund()
            except Exception as e:
                fail(
                    "PENDING BLAKE REFUND CONFIRMATION",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("pending-btc-refund", "all"):
            try:
                test_pending_btc_refund()
            except Exception as e:
                fail(
                    "PENDING BTC REFUND CONFIRMATION",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("pending-blake-claim", "all"):
            try:
                test_pending_blake_claim()
            except Exception as e:
                fail(
                    "PENDING BLAKE CLAIM CONFIRMATION",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("pending-btc-claim", "all"):
            try:
                test_pending_btc_claim()
            except Exception as e:
                fail(
                    "PENDING BTC CLAIM CONFIRMATION",
                    str(e),
                )
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

        if args.suite in ("market-roles", "all"):
            try:
                test_market_role_inversion()
            except Exception as e:
                fail(
                    "MARKET ROLE INVERSION",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("reverse-market", "all"):
            try:
                test_reverse_market_bob_selection()
            except Exception as e:
                fail(
                    "REVERSE MARKET BOB SELECTION",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("reverse-market", "all"):
            try:
                test_reverse_market_alice_import()
            except Exception as e:
                fail(
                    "REVERSE MARKET ALICE IMPORT",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("role-guard", "all"):
            try:
                test_wrong_role_economic_actions()
            except Exception as e:
                fail(
                    "WRONG ROLE ECONOMIC ACTIONS",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("reorg-btc-claim", "all"):
            try:
                test_reorg_btc_claim()
            except Exception as e:
                fail(
                    "BTC CLAIM REORG SAFETY",
                    str(e),
                )
                if args.suite != "all":
                    raise

        if args.suite in ("reorg-btc-refund", "all"):
            try:
                test_reorg_btc_refund()
            except Exception as e:
                fail(
                    "BTC REFUND REORG SAFETY",
                    str(e),
                )
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

def test_pending_blake_claim() -> None:
    banner("PENDING BLAKE CLAIM CONFIRMATION")

    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)

    # Complete the Bitcoin claim normally so Bob can independently
    # learn S from a confirmed Bitcoin transaction.
    swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
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

    assert_state(
        swap_id,
        "bob",
        "BTC_CLAIMED",
    )

    production_env = {
        "BLAKE_SWAP_AUTO_MINE": "0",
    }

    # First broadcast must persist identity without advancing state.
    swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    pending = load_state(
        swap_id,
        "bob",
    )

    first_txid = pending.get(
        "blake_claim_txid"
    )

    require(
        pending.get("state") == "BTC_CLAIMED",
        "unconfirmed BLAKE claim advanced protocol state",
    )
    require(
        first_txid,
        "pending BLAKE claim did not persist txid",
    )
    require(
        pending.get("blake_claim_blockhash") is None,
        "pending BLAKE claim unexpectedly has blockhash",
    )

    report = json.loads(
        swap_cmd(
            "reconcile",
            swap_id,
            "--json",
            role="bob",
            extra_env=production_env,
        ).stdout
    )

    require(
        report["blake"]["status"] == "pending-spend",
        f"pending BLAKE claim reconcile status: {report['blake']}",
    )
    require(
        report["blake"].get("branch") == "claim",
        f"pending BLAKE claim reconcile branch: {report['blake']}",
    )
    require(
        report["next_action"] == "wait",
        f"pending BLAKE claim reconcile action: {report['next_action']}",
    )

    # Retry while the exact claim remains pending. Canonical settlement
    # commands treat recognition of the already-persisted transaction as
    # successful idempotence: return normally without constructing or
    # broadcasting another spend.
    proc = swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    output = proc.stdout + proc.stderr

    require(
        "BLAKE CLAIM PENDING" in output,
        "pending BLAKE claim retry did not recognize the existing tx:\n"
        + output[-2000:],
    )

    still_pending = load_state(
        swap_id,
        "bob",
    )

    require(
        still_pending.get("state") == "BTC_CLAIMED",
        "pending BLAKE claim retry changed protocol state",
    )
    require(
        still_pending.get("blake_claim_txid") == first_txid,
        "pending BLAKE claim retry changed txid",
    )
    require(
        still_pending.get("blake_claim_blockhash") is None,
        "pending BLAKE claim retry unexpectedly recorded blockhash",
    )

    # External confirmation simulates normal production mining.
    mine_blake(1)

    swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    confirmed = load_state(
        swap_id,
        "bob",
    )

    require(
        confirmed.get("state") == "BLAKE_CLAIMED",
        "confirmed BLAKE claim did not advance state",
    )
    require(
        confirmed.get("blake_claim_txid") == first_txid,
        "confirmed BLAKE claim txid changed",
    )
    require(
        confirmed.get("blake_claim_blockhash"),
        "confirmed BLAKE claim did not record blockhash",
    )

    ok(
        "production BLAKE claim waits for active-chain confirmation"
    )
def test_pending_btc_claim() -> None:
    banner("PENDING BTC CLAIM CONFIRMATION")

    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)

    production_env = {
        "BLAKE_SWAP_AUTO_MINE": "0",
    }

    # First claim broadcast: S enters the mempool, but durable
    # protocol state must remain BTC_FUNDED until confirmation.
    swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    pending = load_state(
        swap_id,
        "alice",
    )

    first_txid = pending.get(
        "btc_claim_txid"
    )

    require(
        pending.get("state") == "BTC_FUNDED",
        "unconfirmed BTC claim advanced protocol state",
    )
    require(
        first_txid,
        "pending BTC claim did not persist txid",
    )
    require(
        pending.get("btc_claim_blockhash") is None,
        "pending BTC claim unexpectedly has blockhash",
    )

    report = json.loads(
        swap_cmd(
            "reconcile",
            swap_id,
            "--json",
            role="alice",
            extra_env=production_env,
        ).stdout
    )

    require(
        report["btc"]["status"] == "pending-spend",
        f"pending BTC claim reconcile status: {report['btc']}",
    )
    require(
        report["btc"].get("branch") == "claim",
        f"pending BTC claim reconcile branch: {report['btc']}",
    )
    require(
        report["next_action"] == "wait",
        f"pending BTC claim reconcile action: {report['next_action']}",
    )

    # Bob must not be able to consume Alice's pending claim as
    # authoritative settlement data.
    before_bob = load_state(
        swap_id,
        "bob",
    )

    require(
        before_bob.get("state") == "BTC_FUNDED",
        "Bob was not BTC_FUNDED before pending-claim check",
    )
    require(
        before_bob.get("btc_claim_txid") is None,
        "Bob unexpectedly recorded Alice's pending BTC claim",
    )

    # Retry before confirmation. Recognition of the exact persisted
    # pending claim is a successful idempotent operation; no new spend
    # may be constructed or broadcast.
    proc = swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    output = proc.stdout + proc.stderr

    require(
        "BTC CLAIM PENDING" in output,
        "pending BTC claim retry did not recognize the existing tx:\n"
        + output[-2000:],
    )

    still_pending = load_state(
        swap_id,
        "alice",
    )

    require(
        still_pending.get("state") == "BTC_FUNDED",
        "pending BTC claim retry changed protocol state",
    )
    require(
        still_pending.get("btc_claim_txid") == first_txid,
        "pending BTC claim retry changed txid",
    )
    require(
        still_pending.get("btc_claim_blockhash") is None,
        "pending BTC claim retry unexpectedly recorded blockhash",
    )

    # Confirmation comes from the external test harness, not swap_v2.py.
    mine_btc(1)

    swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    confirmed = load_state(
        swap_id,
        "alice",
    )

    require(
        confirmed.get("state") == "BTC_CLAIMED",
        "confirmed BTC claim did not advance state",
    )
    require(
        confirmed.get("btc_claim_txid") == first_txid,
        "confirmed BTC claim txid changed",
    )
    require(
        confirmed.get("btc_claim_blockhash"),
        "confirmed BTC claim did not record blockhash",
    )

    ok(
        "production BTC claim waits for active-chain confirmation"
    )
def test_pending_btc_refund() -> None:
    banner("PENDING BTC REFUND CONFIRMATION")

    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)

    bob = load_state(
        swap_id,
        "bob",
    )

    current_btc = int(
        btc_cli("getblockcount")
    )

    to_timeout = (
        int(bob["btc_timeout"])
        - current_btc
    )

    if to_timeout > 0:
        mine_btc(to_timeout)

    production_env = {
        "BLAKE_SWAP_AUTO_MINE": "0",
    }

    # First mature refund broadcast: persist identity, but do not
    # treat the refund as authoritative until Bitcoin confirms it.
    swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    pending = load_state(
        swap_id,
        "bob",
    )

    first_txid = pending.get(
        "btc_refund_txid"
    )

    require(
        pending.get("state") == "BTC_FUNDED",
        "unconfirmed BTC refund advanced protocol state",
    )
    require(
        first_txid,
        "pending BTC refund did not persist txid",
    )
    require(
        pending.get("btc_refund_blockhash") is None,
        "pending BTC refund unexpectedly has blockhash",
    )

    report = json.loads(
        swap_cmd(
            "reconcile",
            swap_id,
            "--json",
            role="bob",
            extra_env=production_env,
        ).stdout
    )

    require(
        report["btc"]["status"] == "pending-spend",
        f"pending BTC refund reconcile status: {report['btc']}",
    )
    require(
        report["btc"].get("branch") == "refund",
        f"pending BTC refund reconcile branch: {report['btc']}",
    )
    require(
        report["next_action"] == "wait",
        f"pending BTC refund reconcile action: {report['next_action']}",
    )

    # Retry while the exact refund remains pending. Recognition of the
    # persisted transaction is successful idempotence; no replacement
    # spend is constructed or broadcast.
    proc = swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    output = proc.stdout + proc.stderr

    require(
        "BTC REFUND PENDING" in output,
        "pending BTC refund retry did not recognize the existing tx:\n"
        + output[-2000:],
    )

    still_pending = load_state(
        swap_id,
        "bob",
    )

    require(
        still_pending.get("state") == "BTC_FUNDED",
        "pending BTC refund retry changed protocol state",
    )
    require(
        still_pending.get("btc_refund_txid") == first_txid,
        "pending BTC refund retry changed txid",
    )
    require(
        still_pending.get("btc_refund_blockhash") is None,
        "pending BTC refund retry unexpectedly recorded blockhash",
    )

    # Simulate normal production confirmation externally.
    mine_btc(1)

    swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    confirmed = load_state(
        swap_id,
        "bob",
    )

    require(
        confirmed.get("state") == "BTC_REFUNDED",
        "confirmed BTC refund did not advance state",
    )
    require(
        confirmed.get("btc_refund_txid") == first_txid,
        "confirmed BTC refund txid changed",
    )
    require(
        confirmed.get("btc_refund_blockhash"),
        "confirmed BTC refund did not record blockhash",
    )

    ok(
        "production BTC refund waits for active-chain confirmation"
    )
def test_pending_blake_refund() -> None:
    banner("PENDING BLAKE REFUND CONFIRMATION")

    swap_id = make_fresh_swap()
    print("swap:", swap_id)

    handshake(swap_id)
    fund_blake_and_sync(swap_id)
    fund_btc_and_sync(swap_id)

    # First complete Bob's Bitcoin refund normally.
    bob = load_state(
        swap_id,
        "bob",
    )

    current_btc = int(
        btc_cli("getblockcount")
    )

    to_btc_timeout = (
        int(bob["btc_timeout"])
        - current_btc
    )

    if to_btc_timeout > 0:
        mine_btc(to_btc_timeout)

    swap_cmd(
        "refund-btc",
        swap_id,
        role="bob",
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

    assert_state(
        swap_id,
        "alice",
        "BTC_REFUNDED",
    )

    alice = load_state(
        swap_id,
        "alice",
    )

    current_blake = int(
        blake_cli("getblockcount")
    )

    to_blake_timeout = (
        int(alice["blake_timeout"])
        - current_blake
    )

    if to_blake_timeout > 0:
        mine_blake(to_blake_timeout)

    production_env = {
        "BLAKE_SWAP_AUTO_MINE": "0",
    }

    # First mature refund broadcast must persist its txid without
    # advancing authoritative protocol state.
    swap_cmd(
        "refund-blake",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    pending = load_state(
        swap_id,
        "alice",
    )

    first_txid = pending.get(
        "blake_refund_txid"
    )

    require(
        pending.get("state") == "BTC_REFUNDED",
        "unconfirmed BLAKE refund advanced protocol state",
    )
    require(
        first_txid,
        "pending BLAKE refund did not persist txid",
    )
    require(
        pending.get("blake_refund_blockhash") is None,
        "pending BLAKE refund unexpectedly has blockhash",
    )

    report = json.loads(
        swap_cmd(
            "reconcile",
            swap_id,
            "--json",
            role="alice",
            extra_env=production_env,
        ).stdout
    )

    require(
        report["blake"]["status"] == "pending-spend",
        f"pending BLAKE refund reconcile status: {report['blake']}",
    )
    require(
        report["blake"].get("branch") == "refund",
        f"pending BLAKE refund reconcile branch: {report['blake']}",
    )
    require(
        report["next_action"] == "wait",
        f"pending BLAKE refund reconcile action: {report['next_action']}",
    )

    # Retry while the exact refund remains pending. Recognition of the
    # persisted transaction is successful idempotence; no replacement
    # spend is constructed or broadcast.
    proc = swap_cmd(
        "refund-blake",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    output = proc.stdout + proc.stderr

    require(
        "BLAKE REFUND PENDING" in output,
        "pending BLAKE refund retry did not recognize the existing tx:\n"
        + output[-2000:],
    )

    still_pending = load_state(
        swap_id,
        "alice",
    )

    require(
        still_pending.get("state") == "BTC_REFUNDED",
        "pending BLAKE refund retry changed protocol state",
    )
    require(
        still_pending.get("blake_refund_txid") == first_txid,
        "pending BLAKE refund retry changed txid",
    )
    require(
        still_pending.get("blake_refund_blockhash") is None,
        "pending BLAKE refund retry unexpectedly recorded blockhash",
    )

    # Simulate production confirmation externally.
    mine_blake(1)

    swap_cmd(
        "refund-blake",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    confirmed = load_state(
        swap_id,
        "alice",
    )

    require(
        confirmed.get("state") == "BLAKE_REFUNDED",
        "confirmed BLAKE refund did not advance state",
    )
    require(
        confirmed.get("blake_refund_txid") == first_txid,
        "confirmed BLAKE refund txid changed",
    )
    require(
        confirmed.get("blake_refund_blockhash"),
        "confirmed BLAKE refund did not record blockhash",
    )

    ok(
        "production BLAKE refund waits for active-chain confirmation"
    )
def test_reverse_market_bob_selection() -> None:
    banner("REVERSE MARKET BOB SELECTION V1")

    import importlib.util
    import tempfile
    from types import SimpleNamespace

    src_dir = str(SWAP_V2.parent)

    if src_dir not in sys.path:
        sys.path.insert(0, src_dir)

    spec = importlib.util.spec_from_file_location(
        "swap_v2_reverse_bob_test",
        SWAP_V2,
    )

    require(
        spec is not None
        and spec.loader is not None,
        "could not load swap_v2 module",
    )

    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    maker_pub = "aa" * 32
    taker_pub = "bb" * 32
    offer_id = "11" * 32
    take_id = "22" * 32
    match_id = "33" * 32
    reserved_swap_id = "abcdef123456"

    original = {
        "id": offer_id,
        "pubkey": maker_pub,
    }

    offer_message = {
        "type": "market-offer",
        "version": 2,
        "direction": mod.MARKET_DIRECTION_BTC_FOR_BLAKE,
        "blake_amount": "0.01",
        "btc_sats": 2000,
    }

    selected = {
        "event_id": take_id,
        "offer_id": offer_id,
        "maker": maker_pub,
        "taker": taker_pub,
        "direction": mod.MARKET_DIRECTION_BTC_FOR_BLAKE,
        "blake_amount": "0.01",
        "btc_sats": 2000,
    }

    take_event = {
        "id": take_id,
        "pubkey": taker_pub,
    }

    with tempfile.TemporaryDirectory(
        prefix="blake-reverse-bob-"
    ) as td:
        root = Path(td)

        originals = {
            "BASE_SWAP_DIR": mod.BASE_SWAP_DIR,
            "current_nostr_pubkey": mod.current_nostr_pubkey,
            "make_swap_id": mod.make_swap_id,
            "build_market_match_event": mod.build_market_match_event,
            "verify_nostr_event": mod.verify_nostr_event,
            "nostr_publish_event": mod.nostr_publish_event,
        }

        built = {}

        try:
            mod.BASE_SWAP_DIR = root

            mod.current_nostr_pubkey = (
                lambda: maker_pub
            )

            mod.make_swap_id = (
                lambda: reserved_swap_id
            )

            def fake_build_market_match_event(
                original_arg,
                take_event_arg,
                public_swap,
            ):
                built["swap_id"] = public_swap.swap_id
                built["blake_amount"] = blake_amount_wire(
                    {
                        "blake_amount_base_units":
                            public_swap.blake_amount_base_units,
                    }
                )
                built["btc_sats"] = public_swap.btc_sats

                event = {
                    "id": match_id,
                    "pubkey": maker_pub,
                }

                message = {
                    "type": "market-match",
                    "swap_id": public_swap.swap_id,
                }

                return event, message

            mod.build_market_match_event = (
                fake_build_market_match_event
            )

            mod.verify_nostr_event = (
                lambda event: True
            )

            async def fake_publish_event(event):
                require(
                    event["id"] == match_id,
                    "wrong market-match event published",
                )
                return match_id

            mod.nostr_publish_event = (
                fake_publish_event
            )

            result = mod.accept_reverse_market_take(
                SimpleNamespace(
                    take_id=take_id,
                ),
                original,
                offer_message,
                selected,
                take_event,
            )

            require(
                result["maker_role"] == "bob",
                f"maker role was not Bob: {result!r}",
            )

            require(
                result["taker_role"] == "alice",
                f"taker role was not Alice: {result!r}",
            )

            require(
                result["nostr_bob_pub"] == maker_pub,
                "Bob identity not durably bound",
            )

            require(
                result["nostr_alice_pub"] == taker_pub,
                "Alice identity not durably bound",
            )

            require(
                result["swap_id"] == reserved_swap_id,
                "reserved swap ID changed",
            )

            require(
                result["market_match_id"] == match_id,
                "published market match ID not persisted",
            )

            require(
                built["swap_id"] == reserved_swap_id,
                "public market match used wrong swap ID",
            )

            #
            # Critical invariant: Bob selection may create coordination
            # metadata, but must NOT create atomic-swap state.
            #
            require(
                not (root / f"{reserved_swap_id}.json").exists(),
                "protocol Bob created atomic-swap state during reverse selection",
            )

            selection = (
                root
                / "market-selections"
                / f"{take_id}.json"
            )

            require(
                selection.exists(),
                "reverse market selection was not persisted",
            )

            data = json.loads(
                selection.read_text()
            )

            require(
                "secret_hex" not in data,
                "reverse Bob selection persisted secret_hex",
            )

            require(
                "secret_hash" not in data,
                "reverse Bob selection persisted secret_hash",
            )

            ok(
                "btc-for-blake maker bound as protocol Bob"
            )
            ok(
                "btc-for-blake taker bound as protocol Alice"
            )
            ok(
                "Bob reserved public swap ID without creating swap state"
            )
            ok(
                "Bob created no secret or hashlock"
            )
            ok(
                "reverse market match publication preserved swap binding"
            )

        finally:
            for name, value in originals.items():
                setattr(
                    mod,
                    name,
                    value,
                )
def test_reverse_market_alice_import() -> None:
    banner("REVERSE MARKET ALICE IMPORT V1")

    import importlib.util
    import tempfile
    from types import SimpleNamespace

    src_dir = str(SWAP_V2.parent)

    if src_dir not in sys.path:
        sys.path.insert(0, src_dir)

    spec = importlib.util.spec_from_file_location(
        "swap_v2_reverse_alice_test",
        SWAP_V2,
    )

    require(
        spec is not None
        and spec.loader is not None,
        "could not load swap_v2 module",
    )

    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    maker_pub = "aa" * 32
    taker_pub = "bb" * 32
    offer_id = "11" * 32
    take_id = "22" * 32
    match_id = "33" * 32
    swap_id = "abcdef123456"

    match_event = {
        "id": match_id,
        "pubkey": maker_pub,
        "content": "{}",
    }

    validated = {
        "event_id": match_id,
        "offer_id": offer_id,
        "take_id": take_id,
        "swap_id": swap_id,
        "maker": maker_pub,
        "taker": taker_pub,
        "direction": mod.MARKET_DIRECTION_BTC_FOR_BLAKE,
        "maker_role": "bob",
        "taker_role": "alice",
        "blake_amount": 0.01,
        "btc_sats": 2000,
    }

    with tempfile.TemporaryDirectory(
        prefix="blake-reverse-alice-"
    ) as td:
        root = Path(td)

        originals = {
            "BASE_SWAP_DIR": mod.BASE_SWAP_DIR,
            "current_nostr_pubkey": mod.current_nostr_pubkey,
            "nostr_fetch_event_by_id": mod.nostr_fetch_event_by_id,
            "validate_market_match_event": mod.validate_market_match_event,
            "btc_cli": mod.btc_cli,
            "blake_cli": mod.blake_cli,
        }

        try:
            mod.BASE_SWAP_DIR = root

            mod.current_nostr_pubkey = (
                lambda: taker_pub
            )

            async def fake_fetch_event_by_id(event_id):
                require(
                    event_id == match_id,
                    "wrong market match requested",
                )
                return match_event

            mod.nostr_fetch_event_by_id = (
                fake_fetch_event_by_id
            )

            async def fake_validate_market_match_event(
                event,
                expected_taker,
            ):
                require(
                    event is match_event,
                    "wrong match event validated",
                )

                require(
                    expected_taker == taker_pub,
                    "reverse match validator received wrong taker",
                )

                return dict(validated)

            mod.validate_market_match_event = (
                fake_validate_market_match_event
            )

            def fake_btc_cli(method, *args, **kwargs):
                require(
                    method == "getblockcount",
                    f"unexpected BTC RPC during reverse import: {method}",
                )
                return 1000

            def fake_blake_cli(method, *args, **kwargs):
                require(
                    method == "getblockcount",
                    f"unexpected BLAKE RPC during reverse import: {method}",
                )
                return 2000

            mod.btc_cli = fake_btc_cli
            mod.blake_cli = fake_blake_cli

            #
            # First import: Alice must create the atomic swap and S.
            #
            mod.command_accept_reverse_market_match(
                SimpleNamespace(
                    match_id=match_id,
                )
            )

            state_path = root / f"{swap_id}.json"

            require(
                state_path.exists(),
                "protocol Alice did not create atomic-swap state",
            )

            first = json.loads(
                state_path.read_text()
            )

            require(
                first["state"] == "OFFERED",
                f"reverse Alice state was not OFFERED: {first['state']!r}",
            )

            require(
                first["nostr_alice_pub"] == taker_pub,
                "reverse taker was not pinned as protocol Alice",
            )

            require(
                first["nostr_bob_pub"] == maker_pub,
                "reverse maker was not pinned as protocol Bob",
            )

            require(
                isinstance(first.get("secret_hex"), str)
                and len(first["secret_hex"]) == 64,
                "protocol Alice did not create a 32-byte secret S",
            )

            require(
                isinstance(first.get("secret_hash"), str)
                and len(first["secret_hash"]) == 64,
                "protocol Alice did not create hashlock H",
            )

            tx_fields = (
                "blake_funding_txid",
                "btc_funding_txid",
                "btc_claim_txid",
                "blake_claim_txid",
                "btc_refund_txid",
                "blake_refund_txid",
            )

            require(
                all(first.get(name) is None for name in tx_fields),
                "reverse match import unexpectedly created settlement activity",
            )

            binding_path = (
                root
                / "market-match-bindings"
                / f"{swap_id}.json"
            )

            require(
                binding_path.exists(),
                "reverse market binding was not persisted",
            )

            binding = json.loads(
                binding_path.read_text()
            )

            require(
                binding["maker_role"] == "bob"
                and binding["taker_role"] == "alice",
                f"reverse role binding incorrect: {binding!r}",
            )

            require(
                "secret_hex" not in binding,
                "public reverse-market binding leaked secret S",
            )

            original_secret = first["secret_hex"]
            original_hash = first["secret_hash"]

            #
            # Retry: exact same negotiated swap must reuse Alice state.
            # A second S for the same swap_id would be catastrophic.
            #
            mod.command_accept_reverse_market_match(
                SimpleNamespace(
                    match_id=match_id,
                )
            )

            second = json.loads(
                state_path.read_text()
            )

            require(
                second["secret_hex"] == original_secret,
                "retry generated a second secret S",
            )

            require(
                second["secret_hash"] == original_hash,
                "retry changed hashlock H",
            )

            ok(
                "reverse market taker became protocol Alice"
            )
            ok(
                "protocol Alice created secret S and hashlock H"
            )
            ok(
                "reverse market maker pinned as protocol Bob"
            )
            ok(
                "Alice import remained OFFERED with no settlement activity"
            )
            ok(
                "public market binding contains no secret"
            )
            ok(
                "reverse match retry reused identical secret S"
            )

        finally:
            for name, value in originals.items():
                setattr(
                    mod,
                    name,
                    value,
                )
def test_wrong_role_economic_actions() -> None:
    banner("WRONG ROLE ECONOMIC ACTIONS V1")

    swap_id = make_fresh_swap()
    handshake(swap_id)

    alice_before = load_state(
        swap_id,
        "alice",
    )

    bob_before = load_state(
        swap_id,
        "bob",
    )

    require(
        alice_before["state"] == "OFFERED",
        "Alice was not OFFERED before wrong-role test",
    )

    require(
        bob_before["state"] == "OFFERED",
        "Bob was not OFFERED before wrong-role test",
    )

    blake_mempool_before = set(
        blake_cli("getrawmempool")
    )

    btc_mempool_before = set(
        btc_cli("getrawmempool")
    )

    #
    # Bob must NEVER be able to perform protocol Alice's
    # BLAKE funding action, even if an outer wrapper invokes
    # the wrong command.
    #
    proc = swap_cmd(
        "fund-blake",
        swap_id,
        role="bob",
        expect_ok=False,
    )

    output = (
        (proc.stdout or "")
        + (proc.stderr or "")
    )

    require(
        proc.returncode != 0,
        "protocol Bob unexpectedly succeeded at BLAKE funding",
    )

    require(
        "protocol requires alice"
        in output.lower(),
        (
            "Bob fund-blake failed for the wrong reason:\n"
            + output[-2000:]
        ),
    )

    ok(
        "protocol Bob rejected before BLAKE funding"
    )

    bob_after = load_state(
        swap_id,
        "bob",
    )

    require(
        bob_after == bob_before,
        "wrong-role BLAKE funding changed Bob swap state",
    )

    ok(
        "wrong-role BLAKE funding left Bob state unchanged"
    )

    blake_mempool_after = set(
        blake_cli("getrawmempool")
    )

    require(
        blake_mempool_after
        == blake_mempool_before,
        (
            "wrong-role BLAKE funding changed mempool: "
            f"before={sorted(blake_mempool_before)} "
            f"after={sorted(blake_mempool_after)}"
        ),
    )

    ok(
        "wrong-role BLAKE funding broadcast no transaction"
    )

    #
    # Symmetric invariant:
    # Alice must NEVER be able to perform protocol Bob's
    # BTC funding action.
    #
    proc = swap_cmd(
        "fund-btc",
        swap_id,
        role="alice",
        expect_ok=False,
    )

    output = (
        (proc.stdout or "")
        + (proc.stderr or "")
    )

    require(
        proc.returncode != 0,
        "protocol Alice unexpectedly succeeded at BTC funding",
    )

    require(
        "protocol requires bob"
        in output.lower(),
        (
            "Alice fund-btc failed for the wrong reason:\n"
            + output[-2000:]
        ),
    )

    ok(
        "protocol Alice rejected before BTC funding"
    )

    alice_after = load_state(
        swap_id,
        "alice",
    )

    require(
        alice_after == alice_before,
        "wrong-role BTC funding changed Alice swap state",
    )

    ok(
        "wrong-role BTC funding left Alice state unchanged"
    )

    btc_mempool_after = set(
        btc_cli("getrawmempool")
    )

    require(
        btc_mempool_after
        == btc_mempool_before,
        (
            "wrong-role BTC funding changed mempool: "
            f"before={sorted(btc_mempool_before)} "
            f"after={sorted(btc_mempool_after)}"
        ),
    )

    ok(
        "wrong-role BTC funding broadcast no transaction"
    )

    #
    # Coordination-side invariant:
    # protocol Alice must never import a BLAKE-funded
    # announcement intended for protocol Bob.
    #
    proc = swap_cmd(
        "receive-blake-funded",
        swap_id,
        role="alice",
        expect_ok=False,
    )

    output = (
        (proc.stdout or "")
        + (proc.stderr or "")
    )

    require(
        proc.returncode != 0,
        "protocol Alice unexpectedly received BLAKE funding",
    )

    require(
        "protocol requires bob"
        in output.lower(),
        (
            "Alice receive-blake-funded failed for the wrong reason:\n"
            + output[-2000:]
        ),
    )

    alice_after_receive = load_state(
        swap_id,
        "alice",
    )

    require(
        alice_after_receive == alice_before,
        "wrong-role BLAKE funding receipt changed Alice swap state",
    )

    ok(
        "protocol Alice rejected before BLAKE funding receipt"
    )
    ok(
        "wrong-role BLAKE funding receipt left Alice state unchanged"
    )

    #
    # Symmetric coordination-side invariant:
    # protocol Bob must never import a BTC-funded
    # announcement intended for protocol Alice.
    #
    proc = swap_cmd(
        "receive-btc-funded",
        swap_id,
        role="bob",
        expect_ok=False,
    )

    output = (
        (proc.stdout or "")
        + (proc.stderr or "")
    )

    require(
        proc.returncode != 0,
        "protocol Bob unexpectedly received BTC funding",
    )

    require(
        "protocol requires alice"
        in output.lower(),
        (
            "Bob receive-btc-funded failed for the wrong reason:\n"
            + output[-2000:]
        ),
    )

    bob_after_receive = load_state(
        swap_id,
        "bob",
    )

    require(
        bob_after_receive == bob_before,
        "wrong-role BTC funding receipt changed Bob swap state",
    )

    ok(
        "protocol Bob rejected before BTC funding receipt"
    )
    ok(
        "wrong-role BTC funding receipt left Bob state unchanged"
    )

    print(
        "wrong-role swap:",
        swap_id,
    )
def test_market_role_inversion() -> None:
    banner("MARKET ROLE INVERSION V1")

    import importlib.util

    src_dir = str(SWAP_V2.parent)

    if src_dir not in sys.path:
        sys.path.insert(0, src_dir)

    spec = importlib.util.spec_from_file_location(
        "swap_v2_market_role_test",
        SWAP_V2,
    )

    require(
        spec is not None
        and spec.loader is not None,
        "could not load swap_v2 module",
    )

    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    #
    # Maker gives BLAKE.
    #
    forward = mod.market_protocol_roles(
        mod.MARKET_DIRECTION_BLAKE_FOR_BTC
    )

    require(
        forward == {
            "maker_role": "alice",
            "taker_role": "bob",
        },
        (
            "blake-for-btc role mapping incorrect: "
            f"{forward!r}"
        ),
    )

    ok(
        "blake-for-btc maker is protocol Alice"
    )
    ok(
        "blake-for-btc taker is protocol Bob"
    )

    #
    # Maker gives BTC. Protocol roles MUST invert.
    #
    reverse = mod.market_protocol_roles(
        mod.MARKET_DIRECTION_BTC_FOR_BLAKE
    )

    require(
        reverse == {
            "maker_role": "bob",
            "taker_role": "alice",
        },
        (
            "btc-for-blake role mapping incorrect: "
            f"{reverse!r}"
        ),
    )

    ok(
        "btc-for-blake maker is protocol Bob"
    )
    ok(
        "btc-for-blake taker is protocol Alice"
    )

    #
    # Human-facing give/want semantics must agree with the
    # protocol-role mapping above.
    #
    forward_terms = mod.market_give_want(
        mod.MARKET_DIRECTION_BLAKE_FOR_BTC,
        0.01,
        2000,
    )

    reverse_terms = mod.market_give_want(
        mod.MARKET_DIRECTION_BTC_FOR_BLAKE,
        0.01,
        2000,
    )

    require(
        forward_terms["give"] == "0.01 BLAKE",
        f"forward maker does not give BLAKE: {forward_terms!r}",
    )

    require(
        reverse_terms["give"] == "2,000 sats",
        f"reverse maker does not give BTC: {reverse_terms!r}",
    )

    ok(
        "market give/want semantics agree with protocol roles"
    )

    #
    # Unknown direction must fail closed rather than silently
    # choosing a protocol role.
    #
    try:
        mod.market_protocol_roles(
            "definitely-not-a-market-direction"
        )
    except ValueError:
        pass
    else:
        raise TestFailure(
            "unknown market direction silently received protocol roles"
        )

    ok(
        "unknown market direction rejected"
    )


if __name__ == "__main__":
    raise SystemExit(main())
