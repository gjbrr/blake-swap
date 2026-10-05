#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
TOOLS = ROOT / "tools"
TESTS = ROOT / "tests"

sys.path.insert(0, str(SRC))
sys.path.insert(0, str(TESTS))

import swap_protocol_tests as h


PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print("PASS ", name)


def fail(name, detail):
    global FAIL
    FAIL += 1
    print("FAIL ", name)
    print("      ", detail)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def remove_message(
    swap_id,
    message_type,
):
    path = h.message_file(
        swap_id,
        message_type,
    )

    if path.exists():
        path.unlink()


def wire_write(
    swap_id,
    message_type,
    fields,
):
    remove_message(
        swap_id,
        message_type,
    )

    cmd = [
        sys.executable,
        str(
            TOOLS
            / "minimal_wire_write.py"
        ),
        "--type",
        message_type,
        "--swap-id",
        swap_id,
        "--json",
        json.dumps(
            fields,
            separators=(",", ":"),
            sort_keys=True,
        ),
    ]

    #
    # The writer addresses the FILE transport itself, so it must be
    # told the same message directory the harness uses, and the same
    # chain nodes for any value it has to verify.
    #
    env = h.tool_env()
    env["PYTHONPATH"] = str(SRC)

    p = subprocess.run(
        cmd,
        text=True,
        capture_output=True,
        env=env,
    )

    if p.returncode != 0:
        raise RuntimeError(
            p.stderr
            or p.stdout
        )

    path = h.message_file(
        swap_id,
        message_type,
    )

    require(
        path.exists(),
        (
            f"independent writer produced no "
            f"{message_type} message"
        ),
    )


def main():
    #
    # All implementations in this interoperability test
    # communicate through one explicit shared file-transport
    # directory. Independent writers inherit this environment.
    #
    os.environ["BLAKE_SWAP_MESSAGE_DIR"] = str(
        h.MESSAGE_DIR
    )

    h.preflight()
    ok("preflight")

    # ============================================================
    # Alice engine creates local swap state only.
    # ============================================================

    p = h.swap_cmd(
        "new",
        "--blake",
        "0.001",
        "--sats",
        "25000",
        role="alice",
    )

    swap_id = h.parse_swap_id(
        p.stdout
    )

    #
    # We still call publish-offer ONCE solely so Alice's engine
    # creates her settlement keys. Then delete its message.
    #
    h.swap_cmd(
        "publish-offer",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    wire_write(
        swap_id,
        "offer",
        {
            "blake_amount":
                h.blake_amount_wire(alice),

            "btc_sats":
                int(alice["btc_sats"]),

            "secret_hash":
                alice["secret_hash"],

            "btc_timeout":
                int(alice["btc_timeout"]),

            "blake_timeout":
                int(alice["blake_timeout"]),

            "created_at":
                int(alice["created_at"]),

            "blake_alice_pub":
                alice["blake_alice_pub"],

            "btc_alice_pub":
                alice["btc_alice_pub"],

            "timeout_policy_version":
                alice[
                    "timeout_policy_version"
                ],

            "btc_timeout_window_seconds":
                int(
                    alice[
                        "btc_timeout_window_seconds"
                    ]
                ),

            "blake_timeout_window_seconds":
                int(
                    alice[
                        "blake_timeout_window_seconds"
                    ]
                ),

            "bob_recovery_seconds":
                int(
                    alice[
                        "bob_recovery_seconds"
                    ]
                ),
        },
    )

    ok("independent implementation produced offer")

    # ============================================================
    # Bob accepts offer, but replace Bob's reference accept.
    # ============================================================

    h.swap_cmd(
        "accept-offer",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    wire_write(
        swap_id,
        "accept",
        {
            "secret_hash":
                bob["secret_hash"],

            "blake_bob_pub":
                bob["blake_bob_pub"],

            "btc_bob_pub":
                bob["btc_bob_pub"],
        },
    )

    h.swap_cmd(
        "receive-accept",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice["blake_bob_pub"]
        == bob["blake_bob_pub"],
        "Alice imported wrong Bob BLAKE key",
    )

    require(
        alice["btc_bob_pub"]
        == bob["btc_bob_pub"],
        "Alice imported wrong Bob BTC key",
    )

    ok("independent implementation produced accept")

    # ============================================================
    # Alice funds BLAKE. Independent implementation announces it.
    # ============================================================

    h.swap_cmd(
        "fund-blake",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    wire_write(
        swap_id,
        "blake-funded",
        {
            "secret_hash":
                alice["secret_hash"],

            "blake_amount":
                h.blake_amount_wire(alice),

            "blake_timeout":
                int(alice["blake_timeout"]),

            "blake_funding_txid":
                alice[
                    "blake_funding_txid"
                ],

            "blake_funding_vout":
                int(
                    alice[
                        "blake_funding_vout"
                    ]
                ),
        },
    )

    h.swap_cmd(
        "receive-blake-funded",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"] == "BLAKE_FUNDED",
        "Bob failed BLAKE funding import",
    )

    ok(
        "independent blake-funded advanced Bob after chain verification"
    )

    # ============================================================
    # Bob funds BTC. Independent implementation announces it.
    # ============================================================

    h.swap_cmd(
        "fund-btc",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    wire_write(
        swap_id,
        "btc-funded",
        {
            "secret_hash":
                bob["secret_hash"],

            "btc_sats":
                int(bob["btc_sats"]),

            "btc_timeout":
                int(bob["btc_timeout"]),

            "btc_funding_txid":
                bob[
                    "btc_funding_txid"
                ],

            "btc_funding_vout":
                int(
                    bob[
                        "btc_funding_vout"
                    ]
                ),
        },
    )

    h.swap_cmd(
        "receive-btc-funded",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice["state"] == "BTC_FUNDED",
        "Alice failed BTC funding import",
    )

    ok(
        "independent btc-funded advanced Alice after chain verification"
    )

    # ============================================================
    # Alice claims BTC. Independent implementation announces it.
    # ============================================================

    h.swap_cmd(
        "claim-btc",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    wire_write(
        swap_id,
        "btc-claimed",
        {
            "secret_hash":
                alice["secret_hash"],

            "btc_claim_txid":
                alice["btc_claim_txid"],

            "btc_claim_blockhash":
                alice[
                    "btc_claim_blockhash"
                ],
        },
    )

    #
    # Inspect independent wire message before Bob touches it.
    #
    claim_message = json.loads(
        h.message_file(
            swap_id,
            "btc-claimed",
        ).read_text()
    )

    require(
        "secret_hex"
        not in claim_message,
        "independent btc-claimed leaked secret_hex",
    )

    bob_before = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob_before.get("secret_hex")
        is None,
        "Bob knew Alice original secret",
    )

    require(
        bob_before.get(
            "revealed_secret_hex"
        )
        is None,
        "Bob knew S before Bitcoin extraction",
    )

    h.swap_cmd(
        "receive-btc-claimed",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob.get(
            "revealed_secret_hex"
        )
        is None,
        "claim message transported S",
    )

    ok(
        "independent btc-claimed transported no secret"
    )

    # ============================================================
    # Bob learns S ONLY from Bitcoin.
    # ============================================================

    h.swap_cmd(
        "extract-secret",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    recovered = bob.get(
        "revealed_secret_hex"
    )

    require(
        recovered,
        "Bob failed to extract S",
    )

    require(
        bob.get("secret_hex")
        is None,
        "Bob original secret field became populated",
    )

    require(
        hashlib.sha256(
            bytes.fromhex(
                recovered
            )
        ).hexdigest()
        == bob["secret_hash"],
        "Bitcoin-recovered S does not match H",
    )

    ok(
        "Bob recovered S only from Bitcoin"
    )

    # ============================================================
    # Bob claims BLAKE. Independent implementation announces it.
    # ============================================================

    h.swap_cmd(
        "claim-blake",
        swap_id,
        role="bob",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        bob["state"]
        == "BLAKE_CLAIMED",
        "Bob failed to claim BLAKE",
    )

    wire_write(
        swap_id,
        "blake-claimed",
        {
            "secret_hash":
                bob["secret_hash"],

            "blake_claim_txid":
                bob[
                    "blake_claim_txid"
                ],

            "blake_claim_blockhash":
                bob[
                    "blake_claim_blockhash"
                ],
        },
    )

    h.swap_cmd(
        "receive-blake-claimed",
        swap_id,
        role="alice",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    require(
        alice["state"]
        == "BLAKE_CLAIMED",
        "Alice failed BLAKE claim import",
    )

    ok(
        "independent blake-claimed advanced Alice after chain verification"
    )

    # ============================================================
    # Both complete.
    # ============================================================

    h.swap_cmd(
        "complete",
        swap_id,
        "--role",
        "alice",
        role="alice",
    )

    h.swap_cmd(
        "complete",
        swap_id,
        "--role",
        "bob",
        role="bob",
    )

    alice = h.load_state(
        swap_id,
        "alice",
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    require(
        alice["state"]
        == "COMPLETE",
        "Alice did not reach COMPLETE",
    )

    require(
        bob["state"]
        == "COMPLETE",
        "Bob did not reach COMPLETE",
    )

    ok(
        "full independent-wire happy path reached COMPLETE"
    )

    # ============================================================
    # Final transport-secret audit.
    # ============================================================

    for path in h.MESSAGE_DIR.glob(
        f"{swap_id}.*.json"
    ):
        data = json.loads(
            path.read_text()
        )

        require(
            "secret_hex"
            not in data,
            (
                "secret_hex leaked through "
                f"{path.name}"
            ),
        )

    ok(
        "full independent-wire path never transported Alice's secret"
    )

    print()
    print("=" * 72)
    print("FULL CROSS-IMPLEMENTATION HAPPY PATH")
    print("=" * 72)
    print("passes:  ", PASS)
    print("failures:", FAIL)
    print("swap:    ", swap_id)

    print()
    print(
        "Alice received BTC."
    )
    print(
        "Bob received BLAKE."
    )
    print(
        "S crossed chains through Bitcoin, not messaging."
    )

    if FAIL:
        raise SystemExit(1)


if __name__ == "__main__":
    try:
        main()

    except Exception as exc:
        fail(
            "full cross-implementation happy path",
            str(exc),
        )

        print()
        print("=" * 72)
        print("FULL CROSS-IMPLEMENTATION HAPPY PATH")
        print("=" * 72)
        print("passes:  ", PASS)
        print("failures:", FAIL)

        raise
