#!/usr/bin/env python3

import re
import sys
import tempfile
import time
from pathlib import Path

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


def discovery_work(output: str, chain: str):
    """
    How the engine found the funding: ("observed", 0) or ("scan", blocks).

    The watch-only wallet answers this without reading the chain's history
    at all, which is the point: recovery work must not grow with the age of
    the chain. The bounded scan is the fallback for when that observation is
    unavailable, and it must stay bounded too.
    """
    #
    # The observation path prints one line either way: the outpoint when it
    # worked, or "wallet unavailable" when it could not be used. Only the
    # first is an observation.
    #
    observed = [
        line
        for line in output.splitlines()
        if line.startswith("FUNDING OBSERVATION:")
        and "unavailable" not in line
    ]

    if observed:
        if "FUNDING SCAN:" in output:
            raise h.TestFailure(
                "the engine both observed and scanned for funding:\n"
                f"{output}"
            )

        return "observed", 0

    return "scan", scan_blocks(output, chain)


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

    kind, bob_blake_scan = discovery_work(
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or ""),
        "BLAKE",
    )

    h.require(
        kind == "observed",
        (
            "Bob's BLAKE funding recovery used a block scan "
            f"({bob_blake_scan} blocks) with a watch-only wallet available"
        ),
    )

    h.assert_state(
        swap_id,
        "bob",
        "BLAKE_FUNDED",
    )

    h.ok(
        "Bob observed BLAKE funding from the watch-only wallet with no "
        "block scan"
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

    #
    # Force the fallback for this half without depending on Bitcoin Core's
    # wallet-name validation rules.  A temporary bitcoin-cli wrapper rejects
    # only operations involving this test's watch wallet and forwards every
    # other RPC unchanged to the real node.
    #
    with tempfile.TemporaryDirectory(
        prefix="blake-bounded-scan-",
    ) as temporary:
        wrapper = Path(temporary) / "bitcoin-cli"

        wrapper.write_text(
            f"""#!{sys.executable}
import os
import sys

REAL_CLI = {str(h.BTC_CLI)!r}
SPECIAL = "bounded-scan-watch"
WATCH_COMMANDS = {{
    "loadwallet",
    "createwallet",
    "importdescriptors",
    "listunspent",
    "gettransaction",
}}

args = sys.argv[1:]

uses_special_wallet = (
    SPECIAL in args
    or ("-rpcwallet=" + SPECIAL) in args
)

if uses_special_wallet and any(
    arg in WATCH_COMMANDS
    for arg in args
):
    print(
        "forced watch-wallet failure for bounded-scan test",
        file=sys.stderr,
    )
    raise SystemExit(1)

os.execv(
    REAL_CLI,
    [REAL_CLI, *args],
)
""",
            encoding="utf-8",
        )

        wrapper.chmod(0o755)

        proc = h.swap_cmd(
            "sync-funding",
            swap_id,
            "--role",
            "alice",
            role="alice",
            extra_env={
                "BLAKE_SWAP_BTC_WATCH_WALLET": "bounded-scan-watch",
                "BLAKE_SWAP_BTC_CLI": str(wrapper),
            },
        )

    kind, alice_btc_scan = discovery_work(
        (proc.stdout or "")
        + "\n"
        + (proc.stderr or ""),
        "BTC",
    )

    h.require(
        kind == "scan",
        "Alice's Bitcoin recovery did not fall back to the bounded scan",
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
        "Alice discovered BTC funding with the bounded scan fallback "
        f"({alice_btc_scan} blocks)"
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
