#!/usr/bin/env python3

import re
import time

import swap_protocol_tests as h


def scan_blocks(output: str, chain: str) -> int:
    """
    Parse:
        FUNDING SCAN: BLAKE start=X tip=Y blocks=Z
    or:
        FUNDING SCAN: BITCOIN start=X tip=Y blocks=Z
    """
    pattern = (
        rf"FUNDING SCAN:\s*{re.escape(chain)}\s+"
        rf"start=(\d+)\s+tip=(\d+)\s+blocks=(\d+)"
    )

    m = re.search(
        pattern,
        output,
        re.IGNORECASE,
    )

    if not m:
        raise h.TestFailure(
            f"could not find {chain} funding-scan diagnostic in:\n"
            f"{output}"
        )

    return int(
        m.group(3)
    )


def main():
    started = time.time()

    h.preflight()
    h.ok("preflight")

    h.banner(
        "BOUNDED FUNDING SCAN: BUILD OLD HISTORY"
    )

    #
    # Deliberately make both chains much longer BEFORE
    # creating the swap. None of this history is relevant
    # to the new swap.
    #
    h.mine_btc(250)
    h.mine_blake(250)

    btc_before = int(
        h.btc_cli("getblockcount")
    )

    blake_before = int(
        h.blake_cli("getblockcount")
    )

    h.ok(
        "created 250 irrelevant historical blocks on each chain"
    )

    #
    # Fresh swap must anchor recovery at approximately these
    # current tips, not at genesis.
    #
    swap_id = h.make_fresh_swap()

    alice = h.load_state(
        swap_id,
        "alice",
    )

    h.require(
        alice.get("btc_scan_start_height") is not None,
        "Alice missing Bitcoin scan anchor",
    )

    h.require(
        alice.get("blake_scan_start_height") is not None,
        "Alice missing BLAKE scan anchor",
    )

    h.require(
        int(alice["btc_scan_start_height"])
        >= btc_before,
        "Alice Bitcoin scan anchor predates fresh-swap creation",
    )

    h.require(
        int(alice["blake_scan_start_height"])
        >= blake_before,
        "Alice BLAKE scan anchor predates fresh-swap creation",
    )

    h.ok(
        "fresh Alice state persisted independent scan anchors"
    )

    #
    # Negotiation gives Bob his own independent anchors.
    #
    h.handshake(
        swap_id
    )

    bob = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        bob.get("btc_scan_start_height") is not None,
        "Bob missing Bitcoin scan anchor",
    )

    h.require(
        bob.get("blake_scan_start_height") is not None,
        "Bob missing BLAKE scan anchor",
    )

    h.ok(
        "Bob persisted independent local scan anchors"
    )

    # ---------------------------------------------------------
    # Alice funds BLAKE.
    # Bob gets NO funding announcement.
    # Bob must discover from chain, starting near acceptance.
    # ---------------------------------------------------------

    h.swap_cmd(
        "fund-blake",
        swap_id,
        role="alice",
    )

    proc = h.swap_cmd(
        "sync-funding",
        swap_id,
        "--role",
        "bob",
        role="bob",
    )

    bob_blake_scan = scan_blocks(
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or ""),
        "BLAKE",
    )

    h.require(
        bob_blake_scan <= 5,
        (
            "Bob's BLAKE funding recovery scanned "
            f"{bob_blake_scan} blocks; expected <= 5"
        ),
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_FUNDED",
    )

    h.ok(
        f"Bob discovered BLAKE funding with bounded scan ({bob_blake_scan} blocks)"
    )

    # ---------------------------------------------------------
    # Bob funds Bitcoin.
    # Alice gets NO funding announcement.
    # Alice must discover from chain, starting near creation.
    # ---------------------------------------------------------

    h.swap_cmd(
        "fund-btc",
        swap_id,
        role="bob",
    )

    proc = h.swap_cmd(
        "sync-funding",
        swap_id,
        "--role",
        "alice",
        role="alice",
    )

    alice_btc_scan = scan_blocks(
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or ""),
        "BITCOIN",
    )

    h.require(
        alice_btc_scan <= 5,
        (
            "Alice's Bitcoin funding recovery scanned "
            f"{alice_btc_scan} blocks; expected <= 5"
        ),
    )

    h.assert_state(
        swap_id,
        "alice",
        "BTC_FUNDED",
    )

    h.ok(
        f"Alice discovered BTC funding with bounded scan ({alice_btc_scan} blocks)"
    )

    h.banner(
        "BOUNDED FUNDING SCAN SUMMARY"
    )

    print(
        "passes:  ",
        h.PASS_COUNT,
    )

    print(
        "failures:",
        h.FAIL_COUNT,
    )

    print(
        "elapsed: ",
        f"{time.time() - started:.1f}s",
    )

    print(
        "swap:    ",
        swap_id,
    )

    print()
    print(
        "PASS: old chain history did not increase "
        "fresh-swap funding recovery work."
    )

    return 1 if h.FAIL_COUNT else 0


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
