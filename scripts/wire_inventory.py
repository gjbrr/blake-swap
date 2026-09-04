#!/usr/bin/env python3

from __future__ import annotations

import ast
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "src" / "swap_v2.py"


def string_constant(node):
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    return None


def dict_type(node):
    if not isinstance(node, ast.Dict):
        return None

    for key, value in zip(node.keys, node.values):
        if key is None:
            continue

        if string_constant(key) == "type":
            return string_constant(value)

    return None


def dict_fields(node):
    fields = []

    for key in node.keys:
        if key is None:
            fields.append("**expanded-fields")
            continue

        name = string_constant(key)

        if name is not None:
            fields.append(name)
        else:
            fields.append("<dynamic-key>")

    return fields


def expected_role_from_call(node):
    if not isinstance(node, ast.Call):
        return None

    func = node.func

    if not (
        isinstance(func, ast.Name)
        and func.id == "send_message"
    ):
        return None

    for kw in node.keywords:
        if kw.arg == "expected_role":
            return string_constant(kw.value)

    return None


def enclosing_function(parents, node):
    current = node

    while current in parents:
        current = parents[current]

        if isinstance(
            current,
            (ast.FunctionDef, ast.AsyncFunctionDef),
        ):
            return current.name

    return "<module>"


def main():
    text = SOURCE.read_text()
    tree = ast.parse(text, filename=str(SOURCE))

    parents = {}

    for parent in ast.walk(tree):
        for child in ast.iter_child_nodes(parent):
            parents[child] = parent

    messages = []

    for node in ast.walk(tree):
        msg_type = dict_type(node)

        if msg_type is None:
            continue

        messages.append(
            {
                "type": msg_type,
                "fields": dict_fields(node),
                "function": enclosing_function(
                    parents,
                    node,
                ),
                "line": node.lineno,
            }
        )

    # Find signing-role requirements associated with message types.
    signing_roles = {}

    for node in ast.walk(tree):
        role = expected_role_from_call(node)

        if role is None:
            continue

        if len(node.args) < 2:
            continue

        message_type = string_constant(node.args[1])

        if message_type:
            signing_roles.setdefault(
                message_type,
                set(),
            ).add(role)

    print("WIRE MESSAGE INVENTORY")
    print("======================")
    print()
    print("source:", SOURCE)
    print()

    for msg in sorted(
        messages,
        key=lambda x: (x["type"], x["line"]),
    ):
        print(msg["type"])
        print("-" * len(msg["type"]))
        print("function:", msg["function"])
        print("line:    ", msg["line"])

        roles = signing_roles.get(msg["type"])

        if roles:
            print(
                "signer:  ",
                ", ".join(sorted(roles)),
            )
        else:
            print("signer:   not found via send_message")

        print("fields:")

        for field in msg["fields"]:
            print("  -", field)

        print()


if __name__ == "__main__":
    main()
