#!/usr/bin/env python3
"""
CLI argument wiring suite.

Every command function reads its inputs from an argparse Namespace. If a
function reads ``args.something`` but the parser for that subcommand
never defines ``something``, the command raises AttributeError at run
time — on the operator's first use, and only for that one subcommand.

That is a bug class which is easy to introduce (a new flag on one
command copied to a sibling) and invisible to tests that construct
their own Namespace. This suite closes it generically: it derives each
subcommand's available attributes from the parser construction in
``main()`` and checks every direct ``args.<attr>`` read in the matching
command function.

``getattr(args, "x", default)`` is intentionally allowed: that is the
defensive form and cannot fail.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import ast
import re
import subprocess
import sys
from pathlib import Path

import node_config


ROOT = Path(__file__).resolve().parents[1]
SWAP_V2 = node_config.SWAP_V2

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
    for line in str(detail).splitlines():
        print(f"      {line}")


def dest_from_flag(flag):
    """argparse destination for a flag or positional string."""
    return flag.lstrip("-").replace("-", "_")


def collect_argument_helpers(tree):
    """
    Return ``{function name: {attribute, ...}}`` for module-level helpers
    that add arguments to a parser they are given.

    Six subcommands can spend an HTLC, and each needs the same
    ``--fee-rate`` option, so the engine adds it with one helper rather
    than six copies. That is the right shape for the engine and an
    awkward one for this suite: a call to the helper is not an
    ``add_argument`` call, so the walk below would see those parsers as
    defining nothing but ``swap_id`` and report a wiring failure that is
    not real.

    The fix is to resolve the helper rather than to weaken the check: the
    attributes come from the helper's own ``add_argument`` calls, so
    renaming the option in the helper keeps this suite honest instead of
    letting it drift.
    """
    helpers = {}

    for node in tree.body:
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue

        dests = set()

        for call in ast.walk(node):
            if (
                isinstance(call, ast.Call)
                and isinstance(call.func, ast.Attribute)
                and call.func.attr == "add_argument"
            ):
                for arg in call.args:
                    if isinstance(arg, ast.Constant) and isinstance(arg.value, str):
                        dests.add(dest_from_flag(arg.value))
                        break

        if dests:
            helpers[node.name] = dests

    return helpers


def collect_parser_attributes(main_node, helpers=None):
    """
    Return ``(allowed, subcommands)``.

    ``allowed`` maps command function name -> allowed attribute names.
    ``subcommands`` maps command function name -> the subcommand string
    the parser advertises.

    Walks main() in source order, tracking the parser currently being
    configured and binding its accumulated arguments when
    ``p.set_defaults(func=...)`` is reached.

    ``helpers`` maps a helper function name to the attributes it adds, so
    a shared option block counts as defined for every subparser that
    calls it.
    """
    allowed = {}
    subcommands = {}
    state = {"current": None, "current_name": None}

    helpers = helpers or {}

    def walk(statements):
        for stmt in statements:
            if isinstance(stmt, ast.If):
                walk(stmt.body)
                walk(stmt.orelse)
                continue

            if isinstance(stmt, ast.Assign):
                value = stmt.value

                if (
                    isinstance(value, ast.Call)
                    and isinstance(value.func, ast.Attribute)
                    and value.func.attr == "add_parser"
                ):
                    # A new subparser starts a fresh argument set.
                    state["current"] = set()
                    state["current_name"] = (
                        value.args[0].value
                        if value.args
                        and isinstance(value.args[0], ast.Constant)
                        and isinstance(value.args[0].value, str)
                        else None
                    )

                continue

            if not isinstance(stmt, ast.Expr):
                continue

            call = stmt.value

            if not isinstance(call, ast.Call):
                continue

            func = call.func

            if isinstance(func, ast.Name):
                #
                # A shared option block: `add_fee_rate_argument(p)`.
                #
                if state["current"] is not None:
                    state["current"].update(helpers.get(func.id, ()))

                continue

            if not isinstance(func, ast.Attribute):
                continue

            if func.attr == "add_argument":
                for arg in call.args:
                    if isinstance(arg, ast.Constant) and isinstance(arg.value, str):
                        if state["current"] is not None:
                            state["current"].add(dest_from_flag(arg.value))
                        break

            elif func.attr == "set_defaults":
                target = None

                for kw in call.keywords:
                    if kw.arg == "func" and isinstance(kw.value, ast.Name):
                        target = kw.value.id

                if target is not None:
                    allowed.setdefault(target, set()).update(
                        state["current"] or set()
                    )

                    if state["current_name"] is not None:
                        subcommands[target] = state["current_name"]

    walk(main_node.body)

    return allowed, subcommands


def direct_args_attributes(function_node):
    """
    Direct ``args.<attr>`` reads.

    ``getattr(args, "attr", default)`` produces no Attribute node, so the
    defensive form is never flagged.
    """
    used = set()

    for node in ast.walk(function_node):
        if (
            isinstance(node, ast.Attribute)
            and isinstance(node.value, ast.Name)
            and node.value.id == "args"
        ):
            used.add(node.attr)

    return used


def live_surface(subcommands):
    """
    Compare the statically extracted surface with the real parser.

    The static walk above and the parser itself can drift: a subcommand
    registered through a helper, or an ``add_parser`` reached in a way
    the walk does not model, would be invisible to the analysis. Asking
    the engine to build its parser catches that, and it is the only
    check in the test tree that exercises ``main()``.
    """

    node_config.require_coincurve()

    completed = subprocess.run(
        [sys.executable, str(SWAP_V2), "--help"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=120,
        cwd=str(ROOT),
        env={
            **node_config.node_env(),
            "PYTHONPATH": str(ROOT / "src"),
            "BLAKE_SWAP_TEST_MODE": "1",
        },
    )

    if completed.returncode != 0:
        fail(
            "the engine builds its argument parser",
            "`swap_v2.py --help` exited "
            f"{completed.returncode}:\n{completed.stderr}",
        )
        return

    ok("the engine builds its argument parser")

    match = re.search(r"^\s*\{([^}]*)\}\s*$", completed.stdout, re.MULTILINE)

    if match is None:
        fail(
            "the help output advertises its subcommands",
            "could not find a subcommand list in `swap_v2.py --help`",
        )
        return

    advertised = {name.strip() for name in match.group(1).split(",") if name.strip()}
    declared = set(subcommands.values())

    missing = sorted(declared - advertised)
    extra = sorted(advertised - declared)

    problems = []

    if missing:
        problems.append(
            "the static analysis found subcommands the parser does not "
            f"advertise: {missing}"
        )

    if extra:
        problems.append(
            "the parser advertises subcommands the static analysis missed: "
            f"{extra}"
        )

    check(
        f"all {len(advertised)} advertised subcommands are accounted for",
        problems,
    )


def check(name, problems):
    if problems:
        fail(name, problems[0] if len(problems) == 1 else "\n".join(problems))
    else:
        ok(name)


def main():
    if not SWAP_V2.exists():
        fail("engine source is present", f"not found: {SWAP_V2}")
        return 1

    source = SWAP_V2.read_text()
    tree = ast.parse(source, filename=str(SWAP_V2))

    print()
    print("=" * 72)
    print("CLI ARGUMENT WIRING")
    print("=" * 72)
    print()

    functions = {
        node.name: node
        for node in ast.walk(tree)
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
    }

    main_node = functions.get("main")

    if main_node is None:
        fail("main() is present", "could not find def main() in the engine")
        return 1

    allowed, subcommands = collect_parser_attributes(
        main_node,
        collect_argument_helpers(tree),
    )

    if not allowed:
        fail(
            "parser construction is parseable",
            "extracted no subcommands from main(); the suite needs updating",
        )
        return 1

    ok(f"parsed {len(allowed)} subcommand argument sets")

    missing_functions = sorted(
        name for name in allowed if name not in functions
    )

    if missing_functions:
        fail(
            "every registered command function exists",
            "\n".join(missing_functions),
        )
    else:
        ok("every registered command function exists")

    unwired = []

    for name, attrs in sorted(allowed.items()):
        node = functions.get(name)

        if node is None:
            continue

        used = direct_args_attributes(node)
        unbound = sorted(used - attrs)

        if unbound:
            unwired.append((name, unbound, sorted(attrs)))

    if unwired:
        detail_lines = []

        for name, unbound, attrs in unwired:
            detail_lines.append(
                f"{name} reads {unbound} but its parser defines {attrs}"
            )

        fail(
            "every args attribute read by a command is defined by its parser",
            "\n".join(detail_lines),
        )
    else:
        ok(
            "every args attribute read by a command is defined by its parser"
        )

    live_surface(subcommands)

    print()
    print("=" * 72)
    print("CLI ARGUMENT WIRING SUMMARY")
    print("=" * 72)
    print(f"subcommands: {len(allowed)}")
    print(f"passes:      {PASS}")
    print(f"failures:    {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
