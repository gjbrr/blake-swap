#!/usr/bin/env python3
"""Generate and check the Go parity manifest.

The manifest is the port's ledger: for every Python module, function group,
CLI command and test suite, what replaces it in Go and whether that replacement
exists yet. Its value depends on it being true, so nothing here is typed by
hand that can be read from the tree:

  * the Python anchors are read from the sources,
  * the Go packages and symbols are read from the sources,
  * the mapping between them is the only curated part, and a mapping whose
    Python anchor or Go package has vanished is an error.

Usage:
    scripts/go_parity.py --write     # regenerate docs/GO_PARITY_MANIFEST.md
    scripts/go_parity.py --check     # verify the manifest is current (CI)
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "src"
PKG = ROOT / "pkg"
TESTS = ROOT / "tests"
MANIFEST = ROOT / "docs" / "GO_PARITY_MANIFEST.md"


# --------------------------------------------------------------------------
# Python side
# --------------------------------------------------------------------------


def python_defs(path: Path) -> set[str]:
    """Top-level function and class names in one Python module."""

    names: set[str] = set()

    for line in path.read_text().splitlines():
        match = re.match(
            r"^(?:async\s+)?(?:def|class)\s+([A-Za-z_][A-Za-z0-9_]*)", line,
        )

        if match:
            names.add(match.group(1))

    return names


def python_commands(path: Path) -> list[str]:
    """The CLI subcommands, in the order the parser declares them."""

    text = path.read_text()
    commands: list[str] = []

    for match in re.finditer(
        r"sub\.add_parser\(\s*\n?\s*\"([a-z0-9-]+)\"",
        text,
    ):
        name = match.group(1)

        if name not in commands:
            commands.append(name)

    return commands


# --------------------------------------------------------------------------
# Go side
# --------------------------------------------------------------------------


COMMAND_CASE = re.compile(r'case ((?:"[a-z0-9\-]+"(?:\s*,\s*)?)+):')


def go_cli_commands(path: Path) -> set[str]:
    """The command names the Go CLI dispatches, read from its switch.

    Reading the dispatch rather than a maintained list means the manifest cannot
    claim a command is missing after it was wired, or present because somebody added
    a line to a document.
    """
    if not path.is_file():
        return set()

    names: set[str] = set()

    for group in COMMAND_CASE.findall(path.read_text()):
        names |= set(re.findall(r'"([a-z0-9\-]+)"', group))

    return names


def go_packages() -> dict[str, Path]:
    """Every Go package under pkg/, keyed by import path suffix."""

    found: dict[str, Path] = {}

    for directory in sorted(PKG.iterdir()):
        if not directory.is_dir():
            continue

        if not any(directory.glob("*.go")):
            continue

        found[directory.name] = directory

    return found


def go_symbols(directory: Path) -> set[str]:
    """Exported top-level declarations in a Go package."""

    names: set[str] = set()

    for path in directory.glob("*.go"):
        if path.name.endswith("_test.go"):
            continue

        in_block = False

        for line in path.read_text().splitlines():
            if re.match(r"^(?:const|var)\s*\($", line):
                in_block = True

                continue

            if in_block:
                if line.startswith(")"):
                    in_block = False

                    continue

                match = re.match(r"^\s+([A-Z][A-Za-z0-9_]*)", line)

                if match:
                    names.add(match.group(1))

                continue

            for pattern in (
                r"^func\s+([A-Z][A-Za-z0-9_]*)",
                r"^func\s+\([^)]*\)\s+([A-Z][A-Za-z0-9_]*)",
                r"^type\s+([A-Z][A-Za-z0-9_]*)",
                r"^(?:const|var)\s+([A-Z][A-Za-z0-9_]*)",
            ):
                match = re.match(pattern, line)

                if match:
                    names.add(match.group(1))

    return names


def go_test_files() -> dict[str, set[str]]:
    """Go test functions per package directory under pkg/."""

    tests: dict[str, set[str]] = {}

    for directory in sorted(PKG.iterdir()):
        names: set[str] = set()

        for path in directory.glob("*_test.go"):
            for line in path.read_text().splitlines():
                match = re.match(r"^func\s+(Test[A-Za-z0-9_]*)", line)

                if match:
                    names.add(match.group(1))

        if names:
            tests[directory.name] = names

    return tests


# --------------------------------------------------------------------------
# The curated mapping
# --------------------------------------------------------------------------


@dataclass
class Module:
    """One Python module and what replaces it."""

    python: str
    go: str
    status: str  # ported | partial | pending
    note: str = ""
    symbols: list[str] = field(default_factory=list)
    anchors: list[str] = field(default_factory=list)


# The value layer, the crypto layer and the io layer as they stand. A row is
# added when a package lands, not when work on it starts.
MODULES: list[Module] = [
    Module(
        "src/swap_v2.py (amounts)",
        "pkg/amount",
        "ported",
        "BLAKE base units, sats, exact parsing, and Coin for the node's two JSON shapes",
        ["ParseBlake", "ParseSats", "Coin", "Sats", "Blake"],
        ["blake_base_units", "btc_sats_from_coin", "format_blake_amount"],
    ),
    Module(
        "src/swap_v2.py (decimal money)",
        "pkg/decimal",
        "ported",
        "exact decimal arithmetic: every amount and feerate crosses it without a float",
        ["Dec", "Parse", "MustParse"],
        ["blake_amount_decimal"],
    ),
    Module(
        "src/swap_wire.py",
        "pkg/wire",
        "ported",
        "the swap messages as structs, the shared envelope, Validate/Encode/Decode",
        ["Kind", "Validate", "Encode", "Decode"],
        ["encode_message", "decode_message", "validate_transport_message"],
    ),
    Module(
        "src/swap_v2.py (market vocabulary)",
        "pkg/market",
        "ported",
        "offer/take/match/cancel v1 and v2, direction to roles, binding rules",
        ["Offer", "Take", "Match", "Cancel", "GiveWant", "ProtocolRoles"],
        ["market_give_want", "market_protocol_roles", "market_role_labels"],
    ),
    Module(
        "src/swap_v2.py (State)",
        "pkg/state",
        "ported",
        "the enum, the forward and reorg edges, the two transition functions",
        ["State", "CanTransitionTo", "Transition", "ApplyReorg"],
        ["apply_reorg_transition"],
    ),
    Module(
        "src/swap_v2.py (Swap)",
        "pkg/model",
        "ported",
        "the 42-field record, creation defaults, invariants, legacy migration",
        ["Swap", "New", "Decode", "Migrate"],
        ["make_swap", "migrate_swap_record", "load_swap", "save_swap"],
    ),
    Module(
        "src/swap_timeout_policy.py",
        "pkg/timeout",
        "ported",
        "the timeout policy, the plan builder, validation and description",
        ["Policy", "BuildPlan", "ValidatePlan", "DescribePlan"],
        ["build_timeout_plan", "validate_timeout_plan"],
    ),
    Module(
        "src/fee_policy.py",
        "pkg/fees",
        "ported",
        "rate choice, fee for vsize, caps, minimum viable input",
        ["ChooseRate", "FeeForVsize", "EnforceCap", "MinimumInputForRate"],
        ["choose_fee_rate_sat_vb", "fee_for_vsize"],
    ),
    Module(
        "src/swap_v2.py (_reconcile_*)",
        "pkg/reconcile",
        "ported",
        "the 12-action decision table, readiness, negotiation status, the report",
        ["Decide", "ClassOf", "Build", "EvaluateReadiness"],
        ["_reconcile_next_action", "_reconcile_execution_readiness"],
    ),
    Module(
        "src/swap_v2.py (HTLC descriptor)",
        "pkg/htlc",
        "ported",
        "the wsh(or_i(...)) body and the witness-shape classifier",
        ["Bond", "ClassifySpend", "Descriptor"],
        ["htlc_descriptor_body", "classify_htlc_spend"],
    ),
    Module(
        "src/swap_v2.py (PSBT)",
        "pkg/psbt",
        "ported",
        "minimal PSBT parse/serialize and 0x0b preimage injection",
        ["AddSHA256Preimage", "ReadCompactSize", "FindMapEnd"],
        ["add_sha256_preimage", "read_compact_size", "find_map_end"],
    ),
    Module(
        "src/swap_v2.py (keys)",
        "pkg/keys",
        "ported",
        "key handling, Schnorr signing, raw-x ECDH, and an owned BIP-340 verifier",
        ["Key", "VerifySchnorr", "FromSecret"],
        ["nostr_private_key_hex", "current_nostr_pubkey"],
    ),
    Module(
        "src/nip44.py",
        "pkg/nip44",
        "ported",
        "NIP-44 v2 payloads plus the checks Python makes before trusting one",
        ["Encrypt", "Decrypt", "LooksEncrypted"],
        ["encrypt", "decrypt", "looks_encrypted"],
    ),
    Module(
        "src/swap_v2.py (events)",
        "pkg/event",
        "ported",
        "kind-1 events, author pinning, the event id over canonical JSON",
        ["Event", "ComputeID", "Sign", "Verify"],
        ["build_nostr_event", "nostr_event_id", "verify_nostr_event"],
    ),
    Module(
        "src/swap_v2.py (BLAKE/BTC node access)",
        "pkg/rpc",
        "ported",
        "JSON-RPC transport, the retry allowlist, never retrying a state change",
        ["Client", "Call", "HTTPTransport"],
        ["run_chain_cli", "rpc_method_is_retryable"],
    ),
    Module(
        "src/swap_v2.py (chain helpers)",
        "pkg/chain",
        "ported",
        "the typed node surface, wallet binding by URL path, exact amounts",
        ["Node", "Wallet", "Tx", "TxOut"],
        ["btc_cli", "blake_cli", "chain_dispatch"],
    ),
    Module(
        "src/swap_v2.py (HTLC contract)",
        "pkg/contract",
        "ported",
        "the node-compiled descriptor, its derived address and expected amount",
        ["Derive", "VerifyFunding", "CheckOutput"],
        ["htlc_contract", "get_blake_contract_descriptor", "derive_btc_contract_address"],
    ),
    Module(
        "src/swap_v2.py (_reconcile_observe_htlc)",
        "pkg/observe",
        "ported",
        "observation statuses, the funded scan and the hardened spender search",
        ["Observe", "ScanFunding", "FindSpend"],
        ["_reconcile_observe_htlc", "scan_chain_for_expected_funding", "find_confirmed_htlc_spend"],
    ),
    Module(
        "src/swap_v2.py (funding)",
        "pkg/fund",
        "ported",
        "funding one leg, the crash-window intent, watch-only import and solvability",
        ["Fund", "ResolveIntent", "ImportWatchOnly", "VerifyAnchor"],
        ["command_fund_blake", "write_funding_intent", "reconcile_funding_intent"],
    ),
    Module(
        "src/swap_v2.py (settlement)", "pkg/settle", "ported",
        "the PSBT pipeline, fee/vsize convergence, broadcast and rebroadcast",
        ["Build", "Claim", "Refund", "Broadcast", "SpendFee"],
        ["build_dynamic_settlement_spend", "_decoded_spend_fee_sats", "_decoded_tx_vsize"],
    ),
    Module(
        "src/swap_v2.py (coordination)", "pkg/coord", "ported",
        "one Transport interface: the local spool, or Nostr; validation and sender pinning on both",
        ["FileTransport", "NostrTransport", "Validate", "Transport"],
        ["file_send_message", "send_message", "receive_message", "validate_transport_message"],
    ),
    Module(
        "src/swap_v2.py (relays)", "pkg/nostr", "ported",
        "relay sessions over a platform connection, publish report, REQ/EOSE with topic filters and several filters per subscription, event cache, the encryption policy",
        ["Client", "BuildCoordinationEvent", "DecodeContent", "Cache", "Filter", "FetchMany", "FetchReport"],
        ["nostr_send_message", "nostr_receive_message", "nostr_publish_event", "decode_coordination_content"],
    ),
    Module(
        "src/swap_v2.py (durable state)", "pkg/store", "ported",
        "workflow store: atomic swap records, the message spool, the archive",
        ["File", "Memory", "SupersededPlaceholders", "ArchiveSuperseded"],
        ["atomic_write_json", "load_local_swap", "local_swap_paths", "file_send_message", "file_receive_message"],
    ),
    Module(
        "src/swap_v2.py (commands)", "pkg/swap", "partial",
        "the 58 commands as a Go API: the full settlement lifecycle, refunds, the offer/accept handshake, and the reporting/recovery/resume commands are in; a Go participant completes real swaps and refunds against the Python reference in both directions, and both implementations agree about the protocol report; market commands remain",
        ["Engine", "Status", "Show", "List", "Advance", "FundBlake", "FundBTC", "VerifyBlake", "VerifyBTC",
         "ClaimBTC", "ClaimBlake", "RefundBTC", "RefundBlake", "Complete", "ExtractSecret",
         "AnnounceBlakeFunding", "ReceiveBlakeFunding", "AnnounceBTCFunding", "ReceiveBTCFunding",
         "AnnounceBTCCLaimed", "ReceiveBTCCLaimed", "AnnounceBlakeClaimed", "ReceiveBlakeClaimed",
         "AnnounceBTCRefunded", "ReceiveBTCRefunded", "New", "PublishOffer", "AcceptOffer", "ReceiveAccept",
         "Reconcile", "Next", "Resume", "WatchHTLC", "RecoveryPlan", "Dashboard", "ResumeAll",
         "SyncFunding", "SyncSettlement", "RefreshTimeouts"],
        ["command_status", "command_show", "command_advance", "command_list", "command_fund_blake", "command_fund_btc",
         "command_claim_btc", "command_claim_blake", "command_refund_btc", "command_refund_blake", "command_complete",
         "command_publish_blake_funded", "command_receive_blake_funded", "command_publish_btc_funded", "command_receive_btc_funded",
         "command_publish_btc_claimed", "command_receive_btc_claimed", "command_publish_blake_claimed", "command_receive_blake_claimed",
         "command_publish_btc_refunded", "command_receive_btc_refunded", "command_extract_secret",
         "command_new", "command_publish_offer", "command_accept_offer", "command_receive_accept",
         "command_reconcile", "command_next", "command_resume", "command_watch_htlc",
         "command_recovery_plan", "command_dashboard", "command_resume_all",
         "command_sync_funding", "command_sync_settlement", "command_refresh_timeouts"],
    ),
    Module(
        "src/swap_agent.py", "pkg/agent", "pending",
        "the autonomous driver's step policy and budget",
        [], ["AgentPolicy"],
    ),
    Module(
        "src/swap_v2.py (market flow)", "pkg/marketflow", "partial",
        "market events: offer, take, cancel and match are built, parsed and validated by the reference; state is derived from evidence the same way, for one offer and for a whole book of them; the market commands remain",
        ["BuildOffer", "ParseOffer", "BuildTake", "ParseTake", "BuildCancel", "ParseCancel", "BuildMatch",
         "ParseMatch", "DeriveState", "CancellationApplies", "Book", "OpenOffers", "Entry", "MarketFilters"],
        ["build_market_offer_v2_event", "nostr_parse_market_offer_event", "build_market_take_event",
         "nostr_validate_market_take_event", "build_market_cancel_event", "build_market_match_event",
         "validate_market_match_evidence", "nostr_derive_market_offer_state", "nostr_market_cancellation_applies"],
    ),
    Module(
        "src/swap_v2.py (CLI)", "cmd/blake-swap-cli", "partial",
        "the drop-in CLI: the implemented commands with the reference's arguments and environment; `market`, `market-state` and `market-state --json` are byte-identical to the reference on live relay evidence, the rest of the output text is not yet",
        [], ["main"],
    ),
    Module(
        "(new)", "pkg/vault", "ported",
        "passphrase KDF and record encryption with the NIP-44 primitives",
        ["Seal", "Open", "DeriveKey"],
        [],
    ),
    Module(
        "(new)", "pkg/proxy", "ported",
        "the node RPC proxy: credentials stay server-side, the method allowlist and a per-address rate limit are the boundary",
        ["Proxy", "Config", "Endpoint", "DefaultAllowlist"],
        ["run_chain_cli", "chain_dispatch"],
    ),
    Module(
        "(new)", "pkg/worker", "partial",
        "the browser worker's domains: the event loop, the encrypted-state domain, the read-only engine domain it drives through the app server's proxy, and the command dispatcher",
        ["NewVaultDomain", "Domain", "Chans"],
        [],
    ),
    Module(
        "(new)", "pkg/client", "partial",
        "the browser client's encrypted state: a store.Store whose records are sealed with a passphrase-derived key before they reach IndexedDB, with a pluggable entropy source",
        ["Encrypted", "Create", "Open", "KV", "Memory"],
        ["load_swap", "save_swap", "atomic_write_json"],
    ),
    Module(
        "(new)", "pkg/platform", "ported",
        "the platform seams: clock, console, durable KV, a relay WebSocket and a JSON-RPC transport, each with a browser implementation (IndexedDB, WebSocket, fetch) and a host one (memory or files, TCP socket, HTTP POST)",
        ["KV", "Clock", "Console", "HTTP", "IndexedDB", "WebSocket", "Fetch", "MemoryKV", "DialWebSocket",
         "NewTransport"],
        ["chain_dispatch", "nostr_publish_event_to_relay"],
    ),
]


@dataclass
class Suite:
    """One Python suite and its Go replacement, when it has one."""

    python: str
    go: str = ""
    status: str = "pending"


def suite_rows() -> list[Suite]:
    """Every registered Python suite, with the Go tests that cover it."""

    go_tests = go_test_files()
    rows: list[Suite] = []

    for path in sorted(TESTS.glob("*_tests.py")):
        name = path.name
        row = Suite(python=name)

        # A suite is only claimed as ported when a Go test file names it
        # explicitly, so the manifest cannot drift into optimism.
        for package, tests in go_tests.items():
            if any(name.removesuffix("_tests.py").replace("_", "") in t.lower().replace("_", "")
                   for t in tests):
                row.go = f"pkg/{package}"
                row.status = "partial"
                break

        rows.append(row)

    return rows


# --------------------------------------------------------------------------
# Rendering and checking
# --------------------------------------------------------------------------


def commandPath(go: str) -> str:
    """The directory a mapping points at, for a package or a command."""
    target = go.split(" ")[0]

    if target.startswith("cmd/"):
        return target

    return "pkg/" + target.removeprefix("pkg/")


def moduleExists(go: str) -> bool:
    """Whether a mapping's Go target exists in the tree."""
    target = go.split(" ")[0].removeprefix("pkg/")
    path = ROOT / (go.split(" ")[0] if go.startswith("cmd/") else "pkg/" + target)

    if go.startswith("cmd/"):
        return path.is_dir()

    return target in go_packages()


def render() -> str:
    """Build the manifest markdown."""

    commands = python_commands(SRC / "swap_v2.py")
    packages = go_packages()
    symbols = {name: go_symbols(path) for name, path in packages.items()}
    suites = suite_rows()

    lines: list[str] = []

    lines.append("# Go parity manifest")
    lines.append("")
    lines.append(
        "This file is generated by `scripts/go_parity.py`; do not edit it by hand."
    )
    lines.append("")
    lines.append(
        "It records what replaces each part of the Python engine in Go, and what "
        "has not been replaced yet. Nothing in it is asserted: the Python anchors "
        "and the Go symbols are read from the tree, and the script fails if a "
        "mapping points at something that no longer exists."
    )
    lines.append("")
    lines.append("Statuses: **ported** (behaviour and tests), **partial** (in "
                 "progress), **pending** (not started).")
    lines.append("")

    ported = sum(1 for module in MODULES if module.status == "ported")
    lines.append(f"## Modules ({ported}/{len(MODULES)} ported)")
    lines.append("")
    lines.append("| Python | Go | Status | Contents |")
    lines.append("|---|---|---|---|")

    for module in MODULES:
        go = module.go
        package = go.split(" ")[0].removeprefix("pkg/")

        if module.status != "pending" and package not in packages:
            go += " (missing)"

        lines.append(f"| `{module.python}` | `{go}` | {module.status} | {module.note} |")

    lines.append("")
    lines.append("### Symbols")
    lines.append("")
    lines.append("| Python anchors | Go symbols | Package |")
    lines.append("|---|---|---|")

    for module in MODULES:
        package = module.go.split(" ")[0].removeprefix("pkg/")
        anchors = ", ".join(f"`{anchor}`" for anchor in module.anchors) or "—"
        symbols = ", ".join(f"`{symbol}`" for symbol in module.symbols) or "—"

        lines.append(f"| {anchors} | {symbols} | `pkg/{package}` |")

    lines.append("")
    lines.append(f"## CLI commands ({len(commands)})")
    implemented = go_cli_commands(ROOT / "cmd" / "blake-swap-cli" / "main.go")
    done = sum(1 for command in commands if command in implemented)

    lines.append("")
    lines.append(
        f"The Go CLI dispatches {done} of these {len(commands)}. Implemented means the "
        "command exists with the reference's arguments and environment; it does not by "
        "itself mean the output text is identical, which is tracked per command in the "
        "module table above."
    )
    lines.append("")
    lines.append("| Command | Go | Status |")
    lines.append("|---|---|---|")

    for command in commands:
        if command in implemented:
            lines.append(f"| `{command}` | `run` | implemented |")
        else:
            lines.append(f"| `{command}` | — | pending |")

    lines.append("")
    lines.append(f"## Test suites ({len(suites)})")
    lines.append("")
    lines.append(
        "Every Python suite, and the Go package whose tests cover the same "
        "ground where one does. A suite is only marked partial when a Go test "
        "names it."
    )
    lines.append("")
    lines.append("| Python suite | Go | Status |")
    lines.append("|---|---|---|")

    for suite in suites:
        go = f"`{suite.go}`" if suite.go else "—"
        lines.append(f"| `{suite.python}` | {go} | {suite.status} |")

    lines.append("")

    return "\n".join(lines)


def check() -> int:
    """Verify the manifest's references resolve, and that it is current."""

    problems: list[str] = []
    packages = go_packages()
    symbols = {name: go_symbols(path) for name, path in packages.items()}
    python = {
        path.name: python_defs(path) for path in sorted(SRC.glob("*.py"))
    }

    for module in MODULES:
        package = module.go.split(" ")[0].removeprefix("pkg/")

        if module.status == "pending":
            continue

        if not moduleExists(module.go):
            problems.append(
                f"{module.python}: Go target {commandPath(module.go)} does not exist"
            )

            continue

        for symbol in module.symbols:
            if symbol == "" or module.go.startswith("cmd/"):
                continue

            if symbol not in symbols[package]:
                problems.append(
                    f"{module.python}: pkg/{package} has no exported {symbol}"
                )

        for anchor in module.anchors:
            found = any(anchor in names for names in python.values())

            if not found:
                problems.append(
                    f"{module.python}: the Python source no longer defines {anchor}"
                )

    if not MANIFEST.exists():
        problems.append(f"{MANIFEST.relative_to(ROOT)} is missing")

    elif MANIFEST.read_text() != render():
        problems.append(
            f"{MANIFEST.relative_to(ROOT)} is stale; run scripts/go_parity.py --write"
        )

    if problems:
        print("PARITY MANIFEST PROBLEMS", file=sys.stderr)

        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)

        return 1

    print("parity manifest: current")

    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--stdout", action="store_true")

    args = parser.parse_args()

    if args.stdout:
        print(render())

        return 0

    if args.write:
        MANIFEST.write_text(render())
        print(f"wrote {MANIFEST.relative_to(ROOT)}")

        return 0

    return check()


if __name__ == "__main__":
    raise SystemExit(main())
