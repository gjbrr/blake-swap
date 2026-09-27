#!/usr/bin/env python3

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

import node_config


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"

SWAP_V2 = SRC / "swap_v2.py"

#
# Source directory for the two participant Nostr identities this suite
# uses. Override with BLAKE_SWAP_TEST_KEY_DIR (or BLAKE_SWAP_NOSTR_KEY_DIR).
#
# These keys sign real events published to real relays, so this suite
# deliberately does NOT fabricate fallback keys: that would publish
# events under a publicly-known private key.
#
OLD_KEYS = Path(
    os.environ.get(
        "BLAKE_SWAP_TEST_KEY_DIR",
        os.environ.get(
            "BLAKE_SWAP_NOSTR_KEY_DIR",
            Path.home() / ".blake-swaps-v2" / "nostr-keys",
        ),
    )
).expanduser()

SKIP_EXIT_CODE = 77

PASS = 0
FAIL = 0


def skip(reason):
    """
    Abort with the conventional "prerequisites absent" exit code.

    SystemExit is not an Exception, so this passes through the
    `except Exception` guards in main() unchanged.
    """
    print()
    print("=" * 72)
    print(f"SKIP: {reason}")
    print("=" * 72)
    print("This suite was NOT run. It negotiates over real Nostr relays")
    print("and needs two existing participant keys.")
    print()
    print(f"  key directory: {OLD_KEYS}")
    print()
    print("Set BLAKE_SWAP_TEST_KEY_DIR to a directory containing")
    print("alice.key and bob.key, then re-run. See tests/README.md.")
    print()
    raise SystemExit(SKIP_EXIT_CODE)


def ok(name):
    global PASS
    PASS += 1
    print("PASS ", name)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run_swap(
    *args,
    role,
    state_dir,
    expect_ok=True,
):
    env = os.environ.copy()

    env.update({
        "BLAKE_SWAP_ROLE": role,
        "BLAKE_SWAP_STATE_DIR": str(state_dir),
        "BLAKE_SWAP_TRANSPORT": "nostr",
        "PYTHONPATH": str(SRC),
    })

    #
    # Point the engine at the resolved chain nodes rather than its
    # built-in defaults, which only exist on the author's host.
    #
    env.update(node_config.node_env())

    p = subprocess.run(
        [
            sys.executable,
            str(SWAP_V2),
            *map(str, args),
        ],
        text=True,
        capture_output=True,
        env=env,
    )

    if expect_ok and p.returncode != 0:
        raise RuntimeError(
            "command failed:\n"
            + (p.stderr or p.stdout)
        )

    return p


def install_key(root, role):
    source = OLD_KEYS / f"{role}.key"

    if not source.exists():
        skip(f"missing source Nostr key: {source}")

    key_dir = root / "nostr-keys"
    key_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    target = key_dir / f"{role}.key"

    shutil.copy2(
        source,
        target,
    )

    return target


def state_path(root, role, swap_id):
    return (
        root
        / role
        / f"{swap_id}.json"
    )


def load_state(root, role, swap_id):
    path = state_path(
        root,
        role,
        swap_id,
    )

    require(
        path.exists(),
        f"missing {role} state: {path}",
    )

    return json.loads(
        path.read_text()
    )


def assert_no_peer_state(
    alice_root,
    bob_root,
    swap_id,
):
    require(
        not state_path(
            alice_root,
            "bob",
            swap_id,
        ).exists(),
        "Alice root contains Bob state",
    )

    require(
        not state_path(
            bob_root,
            "alice",
            swap_id,
        ).exists(),
        "Bob root contains Alice state",
    )


def assert_file_transport_unused(root):
    message_dir = root / "messages"

    if message_dir.exists():
        require(
            not any(message_dir.iterdir()),
            f"file transport used under {root}",
        )


def main():
    node_config.require_node_config()

    base = Path(
        tempfile.mkdtemp(
            prefix="blake-full-isolated-"
        )
    )

    alice_root = base / "alice-host"
    bob_root = base / "bob-host"

    alice_root.mkdir()
    bob_root.mkdir()

    try:
        #
        # Each participant gets only its own Nostr identity.
        #
        install_key(
            alice_root,
            "alice",
        )

        install_key(
            bob_root,
            "bob",
        )

        require(
            not (
                alice_root
                / "nostr-keys"
                / "bob.key"
            ).exists(),
            "Alice has Bob Nostr private key",
        )

        require(
            not (
                bob_root
                / "nostr-keys"
                / "alice.key"
            ).exists(),
            "Bob has Alice Nostr private key",
        )

        ok("participants have isolated Nostr private keys")

        # ========================================================
        # NEGOTIATION — NOSTR ONLY
        # ========================================================

        p = run_swap(
            "new",
            "--blake",
            "10",
            "--sats",
            "25000",
            role="alice",
            state_dir=alice_root,
        )

        match = re.search(
            r"swap id:\s*([0-9a-f]{12})",
            p.stdout,
        )

        require(
            match is not None,
            "could not parse swap id",
        )

        swap_id = match.group(1)

        run_swap(
            "publish-offer",
            swap_id,
            role="alice",
            state_dir=alice_root,
        )

        run_swap(
            "accept-offer",
            swap_id,
            role="bob",
            state_dir=bob_root,
        )

        run_swap(
            "receive-accept",
            swap_id,
            role="alice",
            state_dir=alice_root,
        )

        alice = load_state(
            alice_root,
            "alice",
            swap_id,
        )

        bob = load_state(
            bob_root,
            "bob",
            swap_id,
        )

        require(
            alice["secret_hash"]
            == bob["secret_hash"],
            "hashlock mismatch after Nostr negotiation",
        )

        require(
            alice.get("nostr_bob_pub"),
            "Alice did not pin Bob Nostr identity",
        )

        require(
            bob.get("nostr_alice_pub"),
            "Bob did not pin Alice Nostr identity",
        )

        assert_no_peer_state(
            alice_root,
            bob_root,
            swap_id,
        )

        assert_file_transport_unused(
            alice_root
        )

        assert_file_transport_unused(
            bob_root
        )

        ok(
            "isolated Nostr negotiation completed with no shared filesystem"
        )

        # ========================================================
        # ALICE FUNDS BLAKE — NO FUNDING ANNOUNCEMENT
        # ========================================================

        run_swap(
            "fund-blake",
            swap_id,
            role="alice",
            state_dir=alice_root,
        )

        alice = load_state(
            alice_root,
            "alice",
            swap_id,
        )

        require(
            alice["state"]
            == "BLAKE_FUNDED",
            "Alice did not reach BLAKE_FUNDED",
        )

        ok(
            "Alice funded BLAKE without settlement announcement"
        )

        #
        # Bob reconstructs BLAKE funding from chain only.
        #
        run_swap(
            "sync-funding",
            swap_id,
            "--role",
            "bob",
            role="bob",
            state_dir=bob_root,
        )

        bob = load_state(
            bob_root,
            "bob",
            swap_id,
        )

        require(
            bob["state"]
            == "BLAKE_FUNDED",
            "Bob did not discover BLAKE funding",
        )

        require(
            bob.get("blake_funding_txid"),
            "Bob did not recover BLAKE funding txid",
        )

        ok(
            "Bob reconstructed BLAKE funding from chain only"
        )

        # ========================================================
        # BOB FUNDS BTC — NO FUNDING ANNOUNCEMENT
        # ========================================================

        run_swap(
            "fund-btc",
            swap_id,
            role="bob",
            state_dir=bob_root,
        )

        bob = load_state(
            bob_root,
            "bob",
            swap_id,
        )

        require(
            bob["state"]
            == "BTC_FUNDED",
            "Bob did not reach BTC_FUNDED",
        )

        ok(
            "Bob funded Bitcoin without settlement announcement"
        )

        #
        # Alice reconstructs BTC funding from chain only.
        #
        run_swap(
            "sync-funding",
            swap_id,
            "--role",
            "alice",
            role="alice",
            state_dir=alice_root,
        )

        alice = load_state(
            alice_root,
            "alice",
            swap_id,
        )

        require(
            alice["state"]
            == "BTC_FUNDED",
            "Alice did not discover BTC funding",
        )

        require(
            alice.get("btc_funding_txid"),
            "Alice did not recover BTC funding txid",
        )

        ok(
            "Alice reconstructed Bitcoin funding from chain only"
        )

        # ========================================================
        # ALICE CLAIMS BTC — NO CLAIM ANNOUNCEMENT
        # ========================================================

        run_swap(
            "claim-btc",
            swap_id,
            role="alice",
            state_dir=alice_root,
        )

        alice = load_state(
            alice_root,
            "alice",
            swap_id,
        )

        require(
            alice["state"]
            == "BTC_CLAIMED",
            "Alice did not reach BTC_CLAIMED",
        )

        ok(
            "Alice claimed BTC and revealed S on Bitcoin"
        )

        #
        # Bob discovers settlement and S from Bitcoin only.
        #
        run_swap(
            "sync-settlement",
            swap_id,
            "--role",
            "bob",
            role="bob",
            state_dir=bob_root,
        )

        bob = load_state(
            bob_root,
            "bob",
            swap_id,
        )

        require(
            bob["state"]
            == "BTC_CLAIMED",
            "Bob did not discover BTC claim",
        )

        require(
            bob.get(
                "revealed_secret_hex"
            ),
            "Bob did not recover S from Bitcoin",
        )

        require(
            bob.get("secret_hex")
            is None,
            "Bob obtained Alice original secret field",
        )

        ok(
            "Bob discovered BTC claim and S from Bitcoin only"
        )

        # ========================================================
        # BOB CLAIMS BLAKE — NO CLAIM ANNOUNCEMENT
        # ========================================================

        run_swap(
            "claim-blake",
            swap_id,
            role="bob",
            state_dir=bob_root,
        )

        bob = load_state(
            bob_root,
            "bob",
            swap_id,
        )

        require(
            bob["state"]
            == "BLAKE_CLAIMED",
            "Bob did not reach BLAKE_CLAIMED",
        )

        ok(
            "Bob claimed BLAKE with Bitcoin-recovered S"
        )

        #
        # Alice discovers Bob's claim from BLAKE only.
        #
        run_swap(
            "sync-settlement",
            swap_id,
            "--role",
            "alice",
            role="alice",
            state_dir=alice_root,
        )

        alice = load_state(
            alice_root,
            "alice",
            swap_id,
        )

        require(
            alice["state"]
            == "BLAKE_CLAIMED",
            "Alice did not discover BLAKE claim",
        )

        ok(
            "Alice reconstructed BLAKE settlement from chain only"
        )

        # ========================================================
        # COMPLETE
        # ========================================================

        run_swap(
            "complete",
            swap_id,
            "--role",
            "alice",
            role="alice",
            state_dir=alice_root,
        )

        run_swap(
            "complete",
            swap_id,
            "--role",
            "bob",
            role="bob",
            state_dir=bob_root,
        )

        alice = load_state(
            alice_root,
            "alice",
            swap_id,
        )

        bob = load_state(
            bob_root,
            "bob",
            swap_id,
        )

        require(
            alice["state"] == "COMPLETE",
            "Alice did not reach COMPLETE",
        )

        require(
            bob["state"] == "COMPLETE",
            "Bob did not reach COMPLETE",
        )

        ok(
            "isolated participants both reached COMPLETE"
        )

        # ========================================================
        # FINAL ISOLATION AUDIT
        # ========================================================

        assert_no_peer_state(
            alice_root,
            bob_root,
            swap_id,
        )

        assert_file_transport_unused(
            alice_root
        )

        assert_file_transport_unused(
            bob_root
        )

        require(
            not (
                alice_root
                / "nostr-keys"
                / "bob.key"
            ).exists(),
            "Alice ended with Bob private key",
        )

        require(
            not (
                bob_root
                / "nostr-keys"
                / "alice.key"
            ).exists(),
            "Bob ended with Alice private key",
        )

        ok(
            "full swap preserved filesystem and key isolation"
        )

        print()
        print("=" * 72)
        print("FULL ISOLATED CHAIN SWAP SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)
        print("swap:    ", swap_id)
        print()
        print("Alice received BTC.")
        print("Bob received BLAKE.")
        print(
            "Nostr negotiated the swap; "
            "the chains carried settlement."
        )

    finally:
        shutil.rmtree(
            base,
            ignore_errors=True,
        )


if __name__ == "__main__":
    try:
        main()

    except Exception as exc:
        FAIL += 1

        print("FAIL ", exc)

        print()
        print("=" * 72)
        print("FULL ISOLATED CHAIN SWAP SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)

        raise
