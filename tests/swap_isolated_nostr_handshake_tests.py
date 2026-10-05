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


def amount_base_units(record):
    """
    Return the canonical BLAKE amount in integer base units.

    Current swap state stores blake_amount_base_units.  Older records
    used blake_amount as a coin-denominated JSON number.
    """
    if record.get("blake_amount_base_units") is not None:
        return int(record["blake_amount_base_units"])

    from decimal import Decimal

    return int(
        Decimal(str(amount_base_units(record)))
        * Decimal(100_000_000)
    )


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

    #
    # Preserve any relay configuration already supplied
    # in the calling shell. Otherwise swap_v2 uses its
    # normal configured/default relay behavior.
    #

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


def install_key(
    host_root,
    role,
):
    source = (
        OLD_KEYS
        / f"{role}.key"
    )

    if not source.exists():
        skip(
            f"missing source Nostr key: {source}"
        )

    key_dir = (
        host_root
        / "nostr-keys"
    )

    key_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    target = (
        key_dir
        / f"{role}.key"
    )

    shutil.copy2(
        source,
        target,
    )

    return target


def load_state(
    root,
    role,
    swap_id,
):
    #
    # BLAKE_SWAP_STATE_DIR is already the participant-local root, so
    # the protocol role is not a directory level.
    #
    path = root / f"{swap_id}.json"

    require(
        path.exists(),
        f"missing {role} state: {path}",
    )

    return json.loads(
        path.read_text()
    )


def main():
    node_config.require_node_config()

    base = Path(
        tempfile.mkdtemp(
            prefix="blake-isolated-nostr-"
        )
    )

    alice_root = (
        base
        / "alice-host"
    )

    bob_root = (
        base
        / "bob-host"
    )

    alice_root.mkdir()
    bob_root.mkdir()

    try:
        #
        # Each host receives ONLY its own Nostr secret key.
        #
        alice_key = install_key(
            alice_root,
            "alice",
        )

        bob_key = install_key(
            bob_root,
            "bob",
        )

        require(
            alice_key.exists(),
            "Alice key missing",
        )

        require(
            bob_key.exists(),
            "Bob key missing",
        )

        require(
            not (
                alice_root
                / "nostr-keys"
                / "bob.key"
            ).exists(),
            "Alice host contains Bob's private Nostr key",
        )

        require(
            not (
                bob_root
                / "nostr-keys"
                / "alice.key"
            ).exists(),
            "Bob host contains Alice's private Nostr key",
        )

        ok(
            "each participant possesses only its own Nostr private key"
        )

        #
        # Alice creates local swap state.
        #
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

        ok(
            "Alice created swap in isolated state root"
        )

        #
        # Alice publishes through Nostr.
        #
        run_swap(
            "publish-offer",
            swap_id,
            role="alice",
            state_dir=alice_root,
        )

        ok(
            "Alice published offer through Nostr"
        )

        #
        # Bob has no local Alice state or shared message.
        #
        require(
            not (
                bob_root
                / "alice"
                / f"{swap_id}.json"
            ).exists(),
            "Bob host contains Alice state before receipt",
        )

        require(
            not (
                bob_root
                / "messages"
                / f"{swap_id}.offer.json"
            ).exists(),
            "Bob received offer through file transport",
        )

        ok(
            "Bob had no filesystem copy of Alice state or offer"
        )

        #
        # Bob discovers Alice's Nostr offer and accepts.
        #
        # This suite exercises the direct (non-market) handshake, where
        # there is no maker-signed market match to authenticate the
        # author. The expected author is established out of band by the
        # test itself installing Alice's key, so the unpinned path is
        # chosen explicitly here.
        #
        run_swap(
            "accept-offer",
            swap_id,
            "--allow-unpinned-author",
            role="bob",
            state_dir=bob_root,
        )

        bob = load_state(
            bob_root,
            "bob",
            swap_id,
        )

        require(
            bob.get(
                "nostr_alice_pub"
            ),
            "Bob did not pin Alice Nostr identity",
        )

        require(
            bob.get(
                "secret_hex"
            ) is None,
            "Bob learned Alice's secret",
        )

        ok(
            "Bob imported offer and pinned Alice identity from Nostr"
        )

        #
        # Alice now receives Bob's signed accept.
        #
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

        require(
            alice.get(
                "nostr_bob_pub"
            ),
            "Alice did not pin Bob Nostr identity",
        )

        require(
            alice.get(
                "blake_bob_pub"
            ),
            "Alice did not receive Bob BLAKE settlement key",
        )

        require(
            alice.get(
                "btc_bob_pub"
            ),
            "Alice did not receive Bob BTC settlement key",
        )

        ok(
            "Alice imported Bob acceptance and pinned Bob identity"
        )

        #
        # Economic/hashlock agreement.
        #
        require(
            alice["secret_hash"]
            == bob["secret_hash"],
            "participants disagree on secret hash",
        )

        require(
            float(
                amount_base_units(alice)
            )
            == float(
                amount_base_units(bob)
            ),
            "participants disagree on BLAKE amount",
        )

        require(
            int(
                alice["btc_sats"]
            )
            == int(
                bob["btc_sats"]
            ),
            "participants disagree on BTC amount",
        )

        require(
            int(
                alice["btc_timeout"]
            )
            == int(
                bob["btc_timeout"]
            ),
            "participants disagree on BTC timeout",
        )

        require(
            int(
                alice["blake_timeout"]
            )
            == int(
                bob["blake_timeout"]
            ),
            "participants disagree on BLAKE timeout",
        )

        ok(
            "isolated participants agree on economic and hashlock terms"
        )

        #
        # Strong filesystem-isolation assertions.
        #
        require(
            not (
                alice_root
                / "bob"
                / f"{swap_id}.json"
            ).exists(),
            "Alice filesystem contains Bob swap state",
        )

        require(
            not (
                bob_root
                / "alice"
                / f"{swap_id}.json"
            ).exists(),
            "Bob filesystem contains Alice swap state",
        )

        for root in (
            alice_root,
            bob_root,
        ):
            message_dir = (
                root
                / "messages"
            )

            if message_dir.exists():
                require(
                    not any(
                        message_dir.iterdir()
                    ),
                    (
                        "file transport unexpectedly "
                        f"used under {root}"
                    ),
                )

        ok(
            "no participant can see peer state and file transport remained unused"
        )

        print()
        print("=" * 72)
        print("ISOLATED NOSTR HANDSHAKE SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)
        print("swap:    ", swap_id)
        print("alice:   ", alice_root)
        print("bob:     ", bob_root)

        print()
        print(
            "PASS: negotiation crossed Nostr "
            "without a shared participant filesystem."
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

        print(
            "FAIL ",
            exc,
        )

        print()
        print("=" * 72)
        print("ISOLATED NOSTR HANDSHAKE SUMMARY")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)

        raise
