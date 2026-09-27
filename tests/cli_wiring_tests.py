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


def collect_parser_attributes(main_node):
    """
    Return {command_function_name: {allowed attribute names}}.

    Walks main() in source order, tracking the parser currently being
    configured and binding its accumulated arguments when
    ``p.set_defaults(func=...)`` is reached.
    """
    allowed = {}
    state = {"current": None}

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

                continue

            if not isinstance(stmt, ast.Expr):
                continue

            call = stmt.value

            if not isinstance(call, ast.Call):
                continue

            func = call.func

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

    walk(main_node.body)

    return allowed


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

    allowed = collect_parser_attributes(main_node)

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
