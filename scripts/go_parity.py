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
CMD = ROOT / "cmd"
TESTS = ROOT / "tests"
MANIFEST = ROOT / "docs" / "GO_PARITY_MANIFEST.md"


# --------------------------------------------------------------------------
# Python side
# --------------------------------------------------------------------------


def python_defs(path: Path) -> set[str]:
    """Top-level function, class and constant names in one Python module.

    Constants are included because a contract can be a name rather than a function: the
    participant adapter's surface is a tuple, and a manifest that could not anchor on it
    would have to describe the interface without pointing at it.
    """

    names: set[str] = set()

    for line in path.read_text().splitlines():
        match = re.match(
            r"^(?:async\s+)?(?:def|class)\s+([A-Za-z_][A-Za-z0-9_]*)", line,
        )

        if not match:
            match = re.match(r"^([A-Z][A-Z0-9_]*)\s*(?::[^=]+)?=", line)

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
    """Go test functions per package directory, under pkg/ and under cmd/.

    The CLI packages are included because several Python suites are about the command
    surface rather than a library: their Go replacements live beside the dispatcher, and a
    manifest that could not see them would keep reporting them as unported.
    """

    tests: dict[str, set[str]] = {}

    directories = [("pkg", directory) for directory in sorted(PKG.iterdir())]
    directories += [("cmd", directory) for directory in sorted(CMD.iterdir())]
    directories += [
        ("internal", directory) for directory in sorted((ROOT / "internal").iterdir())
    ]

    for prefix, directory in directories:
        if not directory.is_dir():
            continue

        names: set[str] = set()

        for path in directory.glob("*_test.go"):
            for line in path.read_text().splitlines():
                match = re.match(r"^func\s+(Test[A-Za-z0-9_]*)", line)

                if match:
                    names.add(match.group(1))

        if names:
            tests[f"{prefix}/{directory.name}"] = names

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
        "src/swap_v2.py (commands)", "pkg/swap", "ported",
        "the 58 commands as a Go API: the full settlement lifecycle, refunds, the offer/accept handshake, the market commands, and the reporting/recovery/resume paths; a Go participant completes real swaps and refunds against the Python reference in both directions, and both implementations agree about the protocol report. Output-text parity is tracked on the CLI row, not here",
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
        "src/swap_agent.py", "pkg/agent", "ported",
        "the autonomous participant's step table, the conditional steps it re-reads the record for, and the failure budget that stops it; the CLI's `agent` command is the equivalent of the reference's separate script",
        ["Plan", "Run", "Terminal", "DefaultPolicy", "Outcome", "Host"], ["alice_step", "bob_step"],
    ),
    Module(
        "src/swap_v2.py (market flow)", "pkg/marketflow", "ported",
        "market events: offer, take, cancel and match are built, parsed and validated the same way; state is derived from evidence, for one offer and for a whole book of them; the market commands that drive it are in the CLI",
        ["BuildOffer", "ParseOffer", "BuildTake", "ParseTake", "BuildCancel", "ParseCancel", "BuildMatch",
         "ParseMatch", "DeriveState", "CancellationApplies", "Book", "OpenOffers", "Entry", "MarketFilters"],
        ["build_market_offer_v2_event", "nostr_parse_market_offer_event", "build_market_take_event",
         "nostr_validate_market_take_event", "build_market_cancel_event", "build_market_match_event",
         "validate_market_match_evidence", "nostr_derive_market_offer_state", "nostr_market_cancellation_applies"],
    ),
    Module(
        "src/swap_v2.py (CLI)", "cmd/blake-swap-cli", "partial",
        "the drop-in CLI: every command with the reference's arguments and environment, plus `next` with no argument as discovery and `next SWAP_ID` as an extension. Byte-identical to the reference, proved command by command by a scripted walk that runs both implementations: `show`, `list`, `status`, `reconcile`, `dashboard`, `resume`, `run`, `market`, `market-state`, `new`, `publish-offer`, `accept-offer`, `receive-accept`, every funding, claim and refund publish and receive, `verify-blake`, `verify-btc`, `fund-blake`, `fund-btc`, `claim-btc`, `claim-blake`, `refund-btc`, `refund-blake`, `complete`, `extract-secret`, `resume-all`, `publish-market-offer`, `publish-market-match`. The four market commands that fetch by id still print their relay chatter differently; they are correct and tested, and that wording is the remaining gap",
        [], ["main"],
    ),
    Module(
        "(new)", "pkg/vault", "ported",
        "passphrase KDF and record encryption with the NIP-44 primitives",
        ["Seal", "Open", "DeriveKey"],
        [],
    ),
    Module(
        "(new)", "pkg/walletdep", "ported",
        "the refusal the browser gives for every node wallet path: a sentinel error plus the "
        "operation name, so a missing wallet service is reported as a missing wallet service "
        "rather than as a proxy or node failure. The package itself is not build-tagged; what "
        "is gated on `js && wasm` is the code that answers the node-wallet methods with it, so "
        "the CLI and the rig suites keep their node wallet",
        ["Refuse", "ErrNoWalletService"],
        [],
    ),
    Module(
        "(new)", "pkg/proxy", "ported",
        "the node RPC proxy: credentials stay server-side, the method allowlist and a per-address rate limit are the boundary",
        ["Proxy", "Config", "Endpoint", "DefaultAllowlist"],
        ["run_chain_cli", "chain_dispatch"],
    ),
    Module(
        "(new)", "pkg/worker", "ported",
        "the browser worker's domains: the event loop, the encrypted-state domain, the read-only engine domain it drives through the app server's proxy, the market domain that reads, publishes and takes over the browser's own WebSocket, the coordination domain that publishes and receives all seven settlement message types over a relay with sender pinning and fail-closed decryption, and the command dispatcher",
        ["NewVaultDomain", "Domain", "Chans", "SecretRelays", "DefaultRelays"],
        [],
    ),
    Module(
        "(new)", "pkg/client", "ported",
        "the browser client's encrypted state: a store.Store whose records are sealed with a passphrase-derived key before they reach IndexedDB, with a pluggable entropy source",
        ["Encrypted", "Create", "Open", "KV", "Memory"],
        ["load_swap", "save_swap", "atomic_write_json"],
    ),
    Module(
        "src/swap_v2.py (seeded adversarial corpus scheduler)", "pkg/adversary", "partial",
        "the corpus's language-independent scheduler: the documented SHA-256 derivation, the stage vocabulary it chooses at, the fault profiles, and the rule that no seed may degenerate into the happy path. The part that is not here is not a port gap: replaying generated events against a live pair is the reference harness's job, and `tests/swap_seeded_adversarial_tests.py` still does it",
        ["Choice", "Build", "Corpus", "Schedule", "DefaultSeeds", "FaultProfiles", "FaultVocabulary",
         "Paths"],
        ["deterministic_schedule_choice", "build_fault_schedule", "DEFAULT_SEEDS"],
    ),
    Module(
        "(new)", "pkg/adapter", "partial",
        "the portable participant interface `blake-swap-adapter-v1`: capabilities, role-pinned commands and the portable state projection, served one JSON request per process. The part that is not here is not a port gap: pair orchestration and seeded scenario scheduling belong to the reference harness, which is what drives two implementations against each other",
        ["Capabilities", "Handle", "PortableState", "Decode", "Encode", "Protocol", "Operations", "Roles",
         "PortableFields"],
        ["PARTICIPANT_ADAPTER_SURFACE", "PythonParticipantAdapter", "SwapImplementationPair"],
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
    # tests are the Go test functions that name this suite with a `// Parity:`
    # marker. They are read from the tree, so a suite cannot be claimed by a file
    # name that merely looks similar.
    tests: list[str] = field(default_factory=list)
    note: str = ""


# The four dispositions a suite may have.
#
#   ported   a Go test names it with a `// Parity:` marker (or an explicit
#            replacement whose named test exists)
#   partial  a replacement exists but does not yet carry the marker
#   pending  nothing replaces it
#   waived   an explicit, checked reason says the Go artifact cannot carry it
#
# `waived` is not an escape hatch: the reason must be specific, and the check below
# refuses a short one or a placeholder.
SUITE_STATUSES = ("ported", "partial", "pending", "waived")

# Markers are how a suite is claimed. `// Parity: tests/x_tests.py` in a Go test
# file attributes the suite to the test function that follows it (or precedes it,
# when the marker is inside the function's own doc block).
PARITY_MARKER = re.compile(r"//\s*Parity:\s*(.+)")
TEST_FUNCTION = re.compile(r"^func\s+(Test[A-Za-z0-9_]*)\s*\(")
PYTHON_SUITE = re.compile(r"tests/([A-Za-z0-9_]+\.py)")

# Suites whose Go replacement is real but whose assertions belong to a mechanism
# that only exists on this side. Every entry needs a reason that says which
# assertion is waived and what covers the risk instead; the check refuses
# placeholders and anything under a dozen words.
SUITE_WAIVERS: dict[str, tuple[str, str]] = {}


# Suites whose Go replacement exists but cannot carry a `// Parity:` marker yet.
#
# This table is the only remaining hand-written statement about suites, and it is
# deliberately empty: every suite is either named by a marker or listed as pending
# in the generated table. An entry here is a claim without a test behind it.
SUITE_REPLACEMENTS: dict[str, tuple[str, str]] = {}


def parity_markers() -> dict[str, list[tuple[str, str]]]:
    """Every suite named by a `// Parity:` marker, and the Go test that names it.

    A marker applies to the next `func Test...` in the file, which is how the
    markers in this tree are written (above the function they describe). A marker
    inside a function's body applies to that function.
    """

    found: dict[str, list[tuple[str, str]]] = {}

    directories = [
        ("pkg", directory) for directory in sorted(PKG.iterdir())
    ] + [
        ("cmd", directory) for directory in sorted(CMD.iterdir())
    ] + [
        ("internal", directory) for directory in sorted((ROOT / "internal").iterdir())
    ]

    for prefix, directory in directories:
        if not directory.is_dir():
            continue

        for path in sorted(directory.glob("*_test.go")):
            pending: list[str] = []
            current: str | None = None

            for line in path.read_text().splitlines():
                function = TEST_FUNCTION.match(line)

                if function:
                    current = function.group(1)

                    for suite in pending:
                        found.setdefault(suite, []).append(
                            (f"{prefix}/{directory.name}", current)
                        )

                    pending = []

                    continue

                marker = PARITY_MARKER.search(line)

                if marker:
                    suites = PYTHON_SUITE.findall(marker.group(1))

                    if current is not None and not pending:
                        # Inside or after a function: attribute immediately.
                        for suite in suites:
                            found.setdefault(suite, []).append(
                                (f"{prefix}/{directory.name}", current)
                            )
                    else:
                        pending.extend(suites)

    return found


def suite_rows() -> list[Suite]:
    """Every registered Python suite, with the Go tests that cover it."""

    go_tests = go_test_files()
    markers = parity_markers()
    rows: list[Suite] = []

    for path in sorted(TESTS.glob("*_tests.py")):
        name = path.name
        row = Suite(python=name)

        # A marker is the only way a suite is claimed as ported: it names the
        # suite in a Go test that exists, so the manifest cannot drift into
        # optimism on a similar file name.
        if name in markers:
            named = markers[name]
            packages: list[str] = []

            for package, function in named:
                if function in go_tests.get(package, set()):
                    row.tests.append(f"{package}:{function}")

                    if package not in packages:
                        packages.append(package)

            if row.tests:
                row.go = ", ".join(packages)
                row.status = "ported"
                rows.append(row)

                continue

        if name in SUITE_WAIVERS:
            row.go, row.note = SUITE_WAIVERS[name]
            row.status = "waived"
            rows.append(row)

            continue

        if name in SUITE_REPLACEMENTS:
            row.go, row.status = SUITE_REPLACEMENTS[name]
            rows.append(row)

            continue

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
        "What has actually been run — and what it proved — is in "
        "[`GO_VERIFICATION.md`](GO_VERIFICATION.md), including the list of what is "
        "not done."
    )
    lines.append("")
    lines.append(
        "It records what replaces each part of the Python engine in Go, and what "
        "has not been replaced yet. Nothing in it is asserted: the Python anchors "
        "and the Go symbols are read from the tree, and the script fails if a "
        "mapping points at something that no longer exists."
    )
    lines.append("")
    lines.append(
        "Statuses: **ported** (behaviour and tests), **partial** (in progress), "
        "**pending** (not started), **waived** (an explicit, checked reason why "
        "the Go artifact cannot carry the same assertion)."
    )
    lines.append("")

    ported = sum(1 for module in MODULES if module.status == "ported")
    lines.append(f"## Modules ({ported}/{len(MODULES)} ported)")
    lines.append("")
    lines.append("| Python | Go | Status | Contents |")
    lines.append("|---|---|---|---|")

    for module in MODULES:
        go = module.go

        # A command is not a package, so the check has to know both kinds of target.
        if module.status != "pending" and not moduleExists(module.go):
            go += " (missing)"

        lines.append(f"| `{module.python}` | `{go}` | {module.status} | {module.note} |")

    lines.append("")
    lines.append("### Symbols")
    lines.append("")
    lines.append("| Python anchors | Go symbols | Package |")
    lines.append("|---|---|---|")

    for module in MODULES:
        # A command is not a package: rendering one as `pkg/cmd/...` invented a path
        # that does not exist, and the documentation check caught it.
        target = module.go.split(" ")[0]
        anchors = ", ".join(f"`{anchor}`" for anchor in module.anchors) or "—"
        symbols = ", ".join(f"`{symbol}`" for symbol in module.symbols) or "—"

        lines.append(f"| {anchors} | {symbols} | `{target}` |")

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

    counts = {status: 0 for status in SUITE_STATUSES}

    for suite in suites:
        counts[suite.status] = counts.get(suite.status, 0) + 1

    lines.append(
        "Every Python suite and its disposition: "
        + ", ".join(f"{counts[status]} {status}" for status in SUITE_STATUSES)
        + "."
    )
    lines.append("")
    lines.append(
        "A suite is **ported** only when a Go test names it with a "
        "`// Parity:` marker and that test exists in the tree; a similar file "
        "name is not evidence. **Waived** needs a specific reason, and the check "
        "refuses a placeholder."
    )
    lines.append("")
    lines.append("| Python suite | Go | Status | Go test |")
    lines.append("|---|---|---|---|")

    for suite in suites:
        go = f"`{suite.go}`" if suite.go else "—"
        tests = "<br>".join(f"`{test}`" for test in suite.tests) or "—"
        lines.append(f"| `{suite.python}` | {go} | {suite.status} | {tests} |")

    lines.append("")

    waived = [suite for suite in suites if suite.status == "waived"]

    if waived:
        lines.append("### Waivers")
        lines.append("")
        lines.append("| Python suite | Why the Go artifact cannot carry it |")
        lines.append("|---|---|")

        for suite in waived:
            lines.append(f"| `{suite.python}` | {suite.note} |")

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

    #
    # Some contracts live beside the suites rather than in src/: the participant adapter and
    # the seeded corpus scheduler are test-facing boundaries, and both are Python modules the
    # manifest anchors on. They are read here rather than being claimed without a source.
    #
    for extra in sorted(TESTS.glob("*.py")):
        python[extra.name] = python_defs(extra)

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

    for suite in suite_rows():
        if suite.status not in SUITE_STATUSES:
            problems.append(
                f"{suite.python}: unknown status {suite.status!r}"
            )

        if suite.status == "ported" and not suite.tests:
            problems.append(
                f"{suite.python}: ported without a named Go test"
            )

        if suite.status == "waived":
            words = suite.note.split()

            if len(words) < 12:
                problems.append(
                    f"{suite.python}: waiver is too short to be specific"
                )

            if suite.note.strip().lower() in {
                "not applicable", "n/a", "none", "todo", "not ported",
            }:
                problems.append(
                    f"{suite.python}: waiver is a placeholder"
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
