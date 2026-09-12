#!/usr/bin/env python3
"""
Regression test for abort-before-BTC-funding recovery.

Invariant:

    Alice funds BLAKE.
    Bob never funds Bitcoin.

    Before BTC timeout:
        Alice waits.
        Bob may fund BTC.

    After BTC timeout, before BLAKE timeout:
        Alice waits for BLAKE refund maturity.
        Bob must never fund BTC.

    After BLAKE timeout:
        Alice may refund BLAKE directly.

    After the refund confirms:
        both participants independently converge on
        BLAKE_REFUNDED / settled / no action.

This uses FILE transport and local regtest through the existing
swap_protocol_tests harness. It does not use public Nostr relays.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]

# Make the harness test the repository implementation, not some
# unrelated ~/swap_v2.py default.
os.environ["SWAP_V2"] = str(
    ROOT / "src" / "swap_v2.py"
)

sys.path.insert(
    0,
    str(ROOT / "tests"),
)

import swap_protocol_tests as h


#
# The operator may have scripts/participant-env loaded in the shell.
# The protocol harness predates that neutral participant profile and
# inherits os.environ, so force every test subprocess back onto the
# harness's isolated Alice/Bob state directories.
#
def reconcile_json(
    swap_id: str,
    role: str,
) -> dict:
    p = h.swap_cmd(
        "reconcile",
        swap_id,
        "--json",
        role=role,
    )

    try:
        return json.loads(p.stdout)
    except json.JSONDecodeError as e:
        raise h.TestFailure(
            "reconcile did not return JSON:\n"
            + p.stdout
        ) from e


def require_no_btc_funding(
    swap_id: str,
) -> None:
    for role in ("alice", "bob"):
        state = h.load_state(
            swap_id,
            role,
        )
        h.require(
            state.get("btc_funding_txid") is None,
            f"{role} unexpectedly recorded BTC funding",
        )


def main() -> int:
    h.banner(
        "PRE-BTC BLAKE REFUND REGRESSION"
    )

    h.preflight()
    h.ok("preflight")

    #
    # Use deliberately wide timeout spacing for this regression.
    # The ordinary protocol harness uses very short regtest
    # windows; this test needs enough room to observe three
    # distinct phases deterministically.
    #
    btc_start = int(
        h.btc_cli("getblockcount")
    )
    blake_start = int(
        h.blake_cli("getblockcount")
    )

    btc_timeout = btc_start + 40
    blake_timeout = blake_start + 80

    p = h.swap_cmd(
        "new",
        "--blake", "0.011",
        "--sats", "2198",
        "--btc-timeout", str(btc_timeout),
        "--blake-timeout", str(blake_timeout),
        role="alice",
    )

    swap_id = h.parse_swap_id(
        p.stdout
    )

    print("swap:", swap_id)
    print(
        "starting BTC height:",
        btc_start,
        "timeout:",
        btc_timeout,
    )
    print(
        "starting BLAKE height:",
        blake_start,
        "timeout:",
        blake_timeout,
    )

    h.handshake(swap_id)
    h.ok("handshake")

    #
    # Alice funds BLAKE. Bob deliberately never funds BTC.
    #
    h.fund_blake_and_sync(swap_id)

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_FUNDED",
    )
    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_FUNDED",
    )
    require_no_btc_funding(swap_id)

    h.ok(
        "BLAKE funded while Bitcoin remains absent"
    )

    #
    # Identity diagnostics.
    #
    from coincurve import PrivateKey

    def pub_from_keyfile(path):
        secret = bytes.fromhex(
            Path(path).read_text().strip()
        )
        compressed = (
            PrivateKey(secret)
            .public_key
            .format(compressed=True)
        )
        return compressed[1:].hex()

    expected_alice_pub = pub_from_keyfile(
        h.ALICE_PARTICIPANT_KEY
    )
    expected_bob_pub = pub_from_keyfile(
        h.BOB_PARTICIPANT_KEY
    )

    alice_state = h.load_state(
        swap_id,
        "alice",
    )
    bob_state = h.load_state(
        swap_id,
        "bob",
    )

    print()
    print("IDENTITY DIAGNOSTICS")
    print("--------------------")
    print(
        "expected Alice:",
        expected_alice_pub,
    )
    print(
        "expected Bob:  ",
        expected_bob_pub,
    )
    print(
        "Alice state nostr_alice_pub:",
        alice_state.get("nostr_alice_pub"),
    )
    print(
        "Alice state nostr_bob_pub:  ",
        alice_state.get("nostr_bob_pub"),
    )
    print(
        "Bob state nostr_alice_pub:  ",
        bob_state.get("nostr_alice_pub"),
    )
    print(
        "Bob state nostr_bob_pub:    ",
        bob_state.get("nostr_bob_pub"),
    )
    print()

    #
    # --------------------------------------------------------
    # Phase 1: both funding windows are still live.
    # --------------------------------------------------------
    #
    alice = reconcile_json(
        swap_id,
        "alice",
    )
    bob = reconcile_json(
        swap_id,
        "bob",
    )

    print(
        "phase 1 Alice:",
        alice["next_action"],
        "-",
        alice["reason"],
    )
    print(
        "phase 1 Bob:  ",
        bob["next_action"],
        "-",
        bob["reason"],
    )
    print(
        "phase 1 timeouts:",
        alice.get("timeouts"),
    )

    h.require(
        alice["next_action"] == "wait",
        "Alice did not wait while BTC funding window was live",
    )

    h.require(
        bob["next_action"] == "fund-btc",
        "Bob was not offered BTC funding while window was live",
    )

    require_no_btc_funding(swap_id)

    h.ok(
        "live window: Alice waits and Bob may fund BTC"
    )

    #
    # --------------------------------------------------------
    # Phase 2: expire Bitcoin only.
    #
    # The chains are independent. Mining Bitcoin does not
    # advance the BLAKE refund clock.
    # --------------------------------------------------------
    #
    bob_state = h.load_state(
        swap_id,
        "bob",
    )

    btc_height = int(
        h.btc_cli("getblockcount")
    )
    btc_timeout = int(
        bob_state["btc_timeout"]
    )

    to_btc_timeout = (
        btc_timeout - btc_height
    )

    if to_btc_timeout > 0:
        h.mine_btc(to_btc_timeout)

    alice = reconcile_json(
        swap_id,
        "alice",
    )
    bob = reconcile_json(
        swap_id,
        "bob",
    )

    h.require(
        alice["next_action"] == "wait",
        "Alice did not wait between BTC and BLAKE timeouts",
    )

    h.require(
        "funding window has closed"
        in alice["reason"],
        "Alice did not identify the closed BTC funding window",
    )

    h.require(
        bob["next_action"]
        == "manual-attention",
        "Bob was not blocked after BTC timeout",
    )

    h.require(
        "timeout"
        in bob["reason"].lower(),
        "Bob's refusal was not timeout-derived",
    )

    require_no_btc_funding(swap_id)

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_FUNDED",
    )
    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_FUNDED",
    )

    h.ok(
        "BTC expired: Bob cannot fund; Alice waits for BLAKE"
    )

    #
    # --------------------------------------------------------
    # Phase 3: mature the BLAKE refund.
    # --------------------------------------------------------
    #
    alice_state = h.load_state(
        swap_id,
        "alice",
    )

    blake_height = int(
        h.blake_cli("getblockcount")
    )
    blake_timeout = int(
        alice_state["blake_timeout"]
    )

    to_blake_timeout = (
        blake_timeout - blake_height
    )

    if to_blake_timeout > 0:
        h.mine_blake(
            to_blake_timeout
        )

    alice = reconcile_json(
        swap_id,
        "alice",
    )

    h.require(
        alice["blake"]["status"]
        == "funded",
        "expected BLAKE HTLC is not funded/unspent before refund",
    )

    h.require(
        alice["btc"]["status"]
        == "absent",
        "Bitcoin unexpectedly exists before BLAKE refund",
    )

    h.require(
        alice["next_action"]
        == "refund-blake",
        "mature abandoned BLAKE HTLC was not refundable",
    )

    h.require(
        alice["decision_class"]
        != "manual-attention",
        "safe mature BLAKE refund was classified as manual attention",
    )

    require_no_btc_funding(swap_id)

    h.ok(
        "mature BLAKE refund is recommended"
    )

    #
    # --------------------------------------------------------
    # Phase 4: exercise the real refund command.
    #
    # This specifically tests the new BLAKE_FUNDED authorization
    # path: BTC must be independently absent immediately before
    # Alice is permitted to spend the BLAKE HTLC.
    # --------------------------------------------------------
    #
    h.swap_cmd(
        "refund-blake",
        swap_id,
        role="alice",
    )

    h.assert_state(
        swap_id,
        "alice",
        "BLAKE_REFUNDED",
    )

    require_no_btc_funding(swap_id)

    h.ok(
        "Alice directly refunded abandoned BLAKE HTLC"
    )

    #
    # Bob has stale BLAKE_FUNDED local state at this point.
    # He must recover the terminal refund from chain truth,
    # without a settlement message from Alice.
    #
    bob_before = reconcile_json(
        swap_id,
        "bob",
    )

    h.require(
        bob_before["blake"]["status"]
        == "spent",
        "Bob did not observe confirmed BLAKE spend",
    )

    h.require(
        bob_before["blake"].get("branch")
        == "refund",
        "Bob did not identify BLAKE refund branch",
    )

    h.require(
        bob_before["next_action"]
        == "sync-settlement",
        "Bob was not offered chain-derived settlement sync",
    )

    h.ok(
        "Bob independently discovered Alice's refund"
    )

    #
    # Autonomous recovery must import confirmed chain truth.
    #
    h.swap_cmd(
        "resume",
        swap_id,
        role="bob",
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_REFUNDED",
    )

    #
    # --------------------------------------------------------
    # Phase 5: prove terminal convergence on both participants.
    # --------------------------------------------------------
    #
    for role in ("alice", "bob"):
        report = reconcile_json(
            swap_id,
            role,
        )

        h.require(
            report["checkpoint_state"]
            == "BLAKE_REFUNDED",
            f"{role} did not converge to BLAKE_REFUNDED",
        )

        h.require(
            report["blake"]["status"]
            == "spent",
            f"{role} does not see BLAKE spent",
        )

        h.require(
            report["blake"].get("branch")
            == "refund",
            f"{role} does not see refund branch",
        )

        h.require(
            report["btc"]["status"]
            == "absent",
            f"{role} unexpectedly sees Bitcoin funding",
        )

        h.require(
            report["decision_class"]
            == "settled",
            f"{role} does not classify refund as settled",
        )

        h.require(
            report["next_action"]
            == "none",
            f"{role} still has an action after terminal refund",
        )

    require_no_btc_funding(swap_id)

    h.ok(
        "both participants converged to terminal BLAKE_REFUNDED"
    )

    print()
    print(
        "PASS: aborted-before-BTC recovery regression"
    )
    print(
        "PASS: Bitcoin was never funded"
    )
    print(
        "PASS: no Nostr settlement message was required"
    )
    print(
        "PASS: final authority came from confirmed chain truth"
    )

    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as e:
        print()
        print("FAIL: PRE-BTC BLAKE REFUND REGRESSION")
        print(e)
        raise
