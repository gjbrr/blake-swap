#!/usr/bin/env python3
"""
Documentation alignment suite.

The documentation in docs/ and README.md is a specification of the
existing implementation. This suite fails if the two drift apart, so a
protocol or wire change cannot silently leave the specification wrong.

It checks, without needing chain nodes or relays:

  * every fenced code block is balanced;
  * every repository path referenced by the docs exists;
  * every documented BLAKE_SWAP_* variable is read by src/ or scripts/;
  * every settlement message type is documented;
  * every State member and every allowed transition is documented;
  * the documented HTLC descriptor clauses match the engine's templates;
  * the documented default timeout plan matches the computed plan;
  * every scripts/blake subcommand named in the docs exists;
  * every doc referenced from README.md exists;
  * the suite counts the READMEs state match scripts/test.sh;
  * every registered suite has an inventory row in tests/README.md.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

import node_config


ROOT = Path(__file__).resolve().parents[1]
DOCS_DIR = ROOT / "docs"
SRC = ROOT / "src"

DOC_PATHS = [ROOT / "README.md"] + sorted(DOCS_DIR.glob("*.md"))

#
# Paths the docs reference that are generated at run time and
# deliberately not committed.
#
GENERATED_PATHS = {
    "scripts/participant-env",
}

PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print(f"PASS  {name}")


def fail(name, detail):
    global FAIL
    FAIL += 1
    print(f"FAIL  {name}")
    print(f"      {detail}")


def check(name, problems):
    if problems:
        fail(name, problems[0] if len(problems) == 1 else "\n      ".join([""] + problems))
    else:
        ok(name)


def read_docs():
    return {path: path.read_text() for path in DOC_PATHS if path.exists()}


def engine_source():
    return "\n".join(
        path.read_text() for path in sorted(SRC.glob("*.py"))
    )


def script_source():
    return "\n".join(
        path.read_text()
        for path in sorted(ROOT.joinpath("scripts").iterdir())
        if path.is_file()
    )


# ---------------------------------------------------------------- checks


def test_fences_balanced(docs):
    problems = []

    for path, text in docs.items():
        fences = text.count("\n```")

        if fences % 2:
            problems.append(
                f"{path.relative_to(ROOT)}: {fences} fence markers (odd)"
            )

    check("every documentation code fence is balanced", problems)


def test_referenced_paths_exist(docs):
    problems = []

    pattern = re.compile(
        r"`((?:src|scripts|tests|tools|docs|checkpoints)/"
        r"[A-Za-z0-9_./*-]+)`"
    )

    for path, text in docs.items():
        for match in sorted(set(pattern.findall(text))):
            if "*" in match or match in GENERATED_PATHS:
                continue

            if not (ROOT / match).exists():
                problems.append(
                    f"{path.relative_to(ROOT)}: missing path {match}"
                )

    check("every repository path referenced by the docs exists", problems)


def test_documented_env_vars_are_read(docs):
    haystack = engine_source() + script_source()
    problems = []

    for path, text in docs.items():
        for variable in sorted(set(re.findall(r"BLAKE_SWAP_[A-Z0-9_]+", text))):
            if variable not in haystack:
                problems.append(
                    f"{path.relative_to(ROOT)}: {variable} is read nowhere "
                    "in src/ or scripts/"
                )

    check("every documented environment variable is read by the code", problems)


def test_settlement_types_documented(docs):
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "swap_wire",
        SRC / "swap_wire.py",
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    wire_doc = (DOCS_DIR / "WIRE_PROTOCOL.md").read_text()
    problems = [
        f"WIRE_PROTOCOL.md does not document {t}"
        for t in sorted(module.SETTLEMENT_TYPES)
        if t not in wire_doc
    ]

    check("every settlement message type is documented", problems)


def test_states_and_transitions_documented(docs):
    source = (SRC / "swap_v2.py").read_text()
    protocol = (DOCS_DIR / "PROTOCOL.md").read_text()

    state_block = source[
        source.index("class State(str, Enum):"):
        source.index("ALLOWED_TRANSITIONS")
    ]

    states = re.findall(r'^\s+([A-Z_]+) = "', state_block, re.M)

    problems = [
        f"PROTOCOL.md does not document State.{s}"
        for s in states
        if s not in protocol
    ]

    transition_block = source[
        source.index("ALLOWED_TRANSITIONS = {"):
        source.index("@dataclass")
    ]

    edges = []

    for frm, body in re.findall(
        r"State\.([A-Z_]+):\s*\{([^}]*)\}",
        transition_block,
    ):
        for target in re.findall(r"State\.([A-Z_]+)", body):
            edges.append((frm, target))

            if frm not in protocol or target not in protocol:
                problems.append(
                    f"PROTOCOL.md does not document transition "
                    f"{frm} -> {target}"
                )

    if not states:
        problems.append("could not parse any State members from the engine")

    if not edges:
        problems.append(
            "could not parse any transitions from ALLOWED_TRANSITIONS"
        )

    check(
        f"all {len(states)} states and {len(edges)} transitions are documented",
        problems,
    )


def test_descriptor_templates_documented(docs):
    """
    The docs describe the HTLC clauses. They must match the engine's
    actual descriptor templates, clause for clause.
    """
    source = (SRC / "swap_v2.py").read_text()
    protocol = (DOCS_DIR / "PROTOCOL.md").read_text()

    def normalise(text):
        return re.sub(r"\s+", "", text)

    required_fragments = [
        # BLAKE claim branch
        "and_v(v:pk(<blake_bob_pub>),sha256(<H>))",
        # BLAKE refund branch
        "and_v(v:after(<blake_timeout>),pk(<blake_alice_pub>))",
        # BTC claim branch
        "and_v(v:pk(<btc_alice_pub>),sha256(<H>))",
        # BTC refund branch
        "and_v(v:after(<btc_timeout>),pk(<btc_bob_pub>))",
    ]

    problems = []

    normalised_doc = normalise(protocol)

    for fragment in required_fragments:
        if normalise(fragment) not in normalised_doc:
            problems.append(
                f"PROTOCOL.md is missing descriptor clause {fragment}"
            )

    # The engine must actually use each of those clauses.
    for needle in (
        "v:pk({swap.blake_bob_pub})",
        "sha256({swap.secret_hash})",
        "v:after({swap.blake_timeout})",
        "pk({swap.blake_alice_pub})",
        "v:pk({swap.btc_alice_pub})",
        "v:after({swap.btc_timeout})",
        "pk({swap.btc_bob_pub})",
    ):
        if needle not in source:
            problems.append(
                f"engine no longer contains expected template {needle}"
            )

    check("documented HTLC clauses match the engine templates", problems)


def test_default_timeout_plan_documented(docs):
    """
    PROTOCOL.md states the concrete default plan. Recompute it and make
    sure the documented numbers are still true.
    """
    sys.path.insert(0, str(SRC))

    from swap_timeout_policy import (
        TimeoutPolicy,
        bob_recovery_budget_seconds,
        build_timeout_plan,
    )

    policy = TimeoutPolicy()
    plan = build_timeout_plan(btc_height=0, blake_height=0, policy=policy)

    protocol = (DOCS_DIR / "PROTOCOL.md").read_text()

    problems = []

    expected = {
        "Bitcoin block count": (str(plan.btc_blocks), "21"),
        "BLAKE block count": (str(plan.blake_blocks), "46"),
    }

    for label, (computed, documented) in expected.items():
        if computed != documented:
            problems.append(
                f"{label} changed to {computed}; PROTOCOL.md documents "
                f"{documented}"
            )

    if f"{plan.btc_window_seconds / 3600:.2f}" not in protocol:
        problems.append(
            "PROTOCOL.md does not state the computed BTC window "
            f"{plan.btc_window_seconds / 3600:.2f} h"
        )

    if f"{plan.blake_window_seconds / 3600:.2f}" not in protocol:
        problems.append(
            "PROTOCOL.md does not state the computed BLAKE window "
            f"{plan.blake_window_seconds / 3600:.2f} h"
        )

    if f"{plan.bob_recovery_seconds / 3600:.2f}" not in protocol:
        problems.append(
            "PROTOCOL.md does not state the computed recovery budget "
            f"{plan.bob_recovery_seconds / 3600:.2f} h"
        )

    return problems, plan, bob_recovery_budget_seconds(policy)


def test_wrapper_subcommands_documented(docs):
    """
    Every `scripts/blake <command>` the docs mention must be handled by
    the wrapper's case statement, and every wrapper command should be
    reachable from the documented vocabulary.
    """
    wrapper = (ROOT / "scripts" / "blake").read_text()

    handled = set(
        re.findall(r"^\s{2}([a-z][a-z-]*)\|?[a-z-]*\)", wrapper, re.M)
    )

    documented = set()

    for text in docs.values():
        documented.update(
            re.findall(r"scripts/blake ([a-z][a-z-]*)", text)
        )
        documented.update(
            re.findall(r"\./scripts/blake ([a-z][a-z-]*)", text)
        )

    problems = [
        f"docs reference `scripts/blake {cmd}` but the wrapper has no case"
        for cmd in sorted(documented)
        if cmd not in handled
    ]

    check(
        f"all {len(documented)} documented wrapper subcommands exist",
        problems,
    )


def test_readme_doc_links_exist(docs):
    readme = (ROOT / "README.md").read_text()
    problems = []

    for match in sorted(set(re.findall(r"`(docs/[A-Za-z0-9_.-]+\.md)`", readme))):
        if not (ROOT / match).exists():
            problems.append(f"README.md links missing document {match}")

    for doc in sorted(DOCS_DIR.glob("*.md")):
        if doc.name not in readme:
            problems.append(
                f"README.md does not mention docs/{doc.name}"
            )

    check("README documentation index is complete", problems)


#
# Suite counts are asserted in prose in both READMEs. They went stale
# three times during the test work, each time because a suite was added
# without the numbers following it. Derive them from the runner instead.
#
SUITE_CLASSES = ("UNIT", "REGTEST", "NOSTR")

COUNT_PATTERNS = (
    # README.md: "tests/                     41 suites: unit tests ..."
    (ROOT / "README.md", r"tests/\s+(\d+) suites:"),
    # tests/README.md: "41 suites covering the atomic-swap engine"
    (ROOT / "tests" / "README.md", r"^(\d+) suites covering"),
    # tests/README.md: "| `unit` | ... | 17 |"
    (ROOT / "tests" / "README.md", r"\|\s*`unit`\s*\|[^|]*\|\s*(\d+)\s*\|"),
    (
        ROOT / "tests" / "README.md",
        r"\|\s*`regtest`\s*\|[^|]*\|\s*(\d+)\s*\|",
    ),
    (
        ROOT / "tests" / "README.md",
        r"\|\s*`nostr`\s*\|[^|]*\|\s*(\d+)\s*\|",
    ),
    # tests/README.md section headings
    (
        ROOT / "tests" / "README.md",
        r"### Unit suites — no prerequisites \((\d+)\)",
    ),
    (
        ROOT / "tests" / "README.md",
        r"### Regtest suites — need chain nodes \((\d+)\)",
    ),
)


def manifest_counts():
    """
    Parse scripts/test.sh for how many suites each class declares.

    Read as text rather than sourced, so a malformed runner is reported
    as a mismatch rather than executed.
    """

    text = (ROOT / "scripts" / "test.sh").read_text()
    counts = {}

    for name in SUITE_CLASSES:
        match = re.search(
            rf"^{name}_SUITES=\((.*?)^\)",
            text,
            re.MULTILINE | re.DOTALL,
        )

        if match is None:
            counts[name] = None
            continue

        counts[name] = len(
            re.findall(r"tests/[A-Za-z0-9_]+\.py", match.group(1))
        )

    return counts


def test_documented_suite_counts_match_manifest(docs):
    counts = manifest_counts()

    total = sum(value for value in counts.values() if value is not None)

    expected = {
        "unit": counts.get("UNIT"),
        "regtest": counts.get("REGTEST"),
        "nostr": counts.get("NOSTR"),
        "total": total,
    }

    problems = [
        f"scripts/test.sh declares no {name}_SUITES list"
        for name in SUITE_CLASSES
        if counts.get(name) is None
    ]

    for path, pattern in COUNT_PATTERNS:
        text = path.read_text()

        for match in re.finditer(pattern, text, re.MULTILINE):
            documented = int(match.group(1))

            if documented in expected.values():
                continue

            problems.append(
                f"{path.relative_to(ROOT)}: documents {documented} suites, "
                f"but the runner declares "
                f"{expected['unit']} unit / {expected['regtest']} regtest / "
                f"{expected['nostr']} nostr ({total} total)"
            )

    check(
        f"documented suite counts match the runner "
        f"({total} suites)",
        problems,
    )


def manifest_suite_files():
    """Every tests/*_tests.py the runner declares."""

    text = (ROOT / "scripts" / "test.sh").read_text()
    files = []

    for name in SUITE_CLASSES:
        match = re.search(
            rf"^{name}_SUITES=\((.*?)^\)",
            text,
            re.MULTILINE | re.DOTALL,
        )

        if match is None:
            continue

        files.extend(
            re.findall(r"tests/([A-Za-z0-9_]+\.py)", match.group(1))
        )

    return sorted(set(files))


def test_suite_inventory_is_complete():
    """
    Every registered suite must have a row in the tests/README.md
    inventory, so a new suite cannot appear in the runner while being
    invisible to the person reading about it.
    """

    inventory = (ROOT / "tests" / "README.md").read_text()

    documented = set(
        re.findall(r"\| `([a-z0-9_]+_tests\.py)` \|", inventory)
    )

    problems = []

    for suite in manifest_suite_files():
        if suite not in documented:
            problems.append(
                f"tests/README.md has no inventory row for {suite}"
            )

    on_disk = sorted(
        path.name for path in (ROOT / "tests").glob("*_tests.py")
    )

    registered = set(manifest_suite_files())

    for suite in on_disk:
        if suite not in registered:
            problems.append(
                f"{suite} exists on disk but is not registered in "
                "scripts/test.sh"
            )

    check(
        f"all {len(registered)} registered suites are documented",
        problems,
    )


def main():
    missing = [p for p in DOC_PATHS if not p.exists()]

    if missing:
        print("FAIL  documentation files are missing")
        for path in missing:
            print(f"      {path}")
        return 1

    docs = read_docs()

    print()
    print("=" * 72)
    print("DOCUMENTATION ALIGNMENT")
    print("=" * 72)
    print()
    print(f"documents: {len(docs)}")

    test_fences_balanced(docs)
    test_referenced_paths_exist(docs)
    test_documented_env_vars_are_read(docs)
    test_settlement_types_documented(docs)
    test_states_and_transitions_documented(docs)
    test_descriptor_templates_documented(docs)

    try:
        problems, plan, recovery = test_default_timeout_plan_documented(docs)
    except Exception as e:  # noqa: BLE001
        fail("default timeout plan is reproducible", str(e))
    else:
        check(
            "documented default timeout plan matches the computed plan",
            problems,
        )
        print(
            f"      BTC {plan.btc_blocks} blocks / "
            f"BLAKE {plan.blake_blocks} blocks / "
            f"recovery {recovery}s"
        )

    test_wrapper_subcommands_documented(docs)
    test_readme_doc_links_exist(docs)
    test_documented_suite_counts_match_manifest(docs)
    test_suite_inventory_is_complete()

    print()
    print("=" * 72)
    print("DOCUMENTATION ALIGNMENT SUMMARY")
    print("=" * 72)
    print(f"passes:  {PASS}")
    print(f"failures:{FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
