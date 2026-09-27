#!/usr/bin/env python3
"""
Chain CLI transport suite.

Every chain RPC in the engine goes through ``blake_cli`` / ``btc_cli``,
which built a ``bitcoin-cli`` argv and ran it. That is fine until an
argument is large: Linux caps a single argv element at ``MAX_ARG_STRLEN``
(128 KiB), so a funding transaction whose hex exceeds that makes the
kernel refuse to exec at all:

    OSError: [Errno 7] Argument list too long

That is a real failure, not a theoretical one: a wallet consolidating a
few hundred inputs produces a transaction whose hex alone is over the
cap, and the failure lands in the middle of `fund-blake`.

``bitcoin-cli -stdin`` reads the same command and arguments from
standard input, one per line, so the value never touches argv. This
suite pins the choice:

  * ordinary arguments stay in argv, unchanged and in order;
  * Python booleans still render as the JSON literals ``true`` / ``false``
    in both transports;
  * an argument above the safe length switches the whole call to
    ``-stdin``, with the command and every argument, in order, on
    standard input;
  * the boundary itself is exact: at the limit argv is used, one byte
    more and stdin is used;
  * a failing exec becomes a named RuntimeError rather than a raw
    OSError from inside ``subprocess``.

When chain nodes are reachable it also performs a real
``decoderawtransaction`` through the stdin transport, against a real
transaction, so the transport is verified end to end and not merely
modelled.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import errno
import json
import subprocess
import sys
from types import SimpleNamespace
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"

sys.path.insert(0, str(SRC))
sys.path.insert(0, str(ROOT / "tests"))

import node_config  # noqa: E402

node_config.require_coincurve()

#
# The engine resolves its own chain paths from the environment when it
# is imported, so publish the resolved rig first: otherwise the live
# check below would talk to the engine's built-in author-host defaults.
#
node_config.install_into_environ()

import swap_v2  # noqa: E402

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


def check(name, fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
    else:
        ok(name)


class FakeSubprocess:
    """Stand-in for the ``subprocess`` module inside swap_v2."""

    TimeoutExpired = subprocess.TimeoutExpired

    def __init__(self, *, result=None, raises=None):
        self.result = result
        self.raises = raises
        self.calls = []

    def run(self, cmd, **kwargs):
        self.calls.append((list(cmd), kwargs))

        if self.raises is not None:
            raise self.raises

        return self.result


class captured:
    """
    Capture the ``(label, cmd, stdin_text)`` of every CLI call.

    ``blake_cli`` looks ``run_chain_cli`` up on the module at call time,
    so patching the module attribute is enough and no call site has to
    know this suite exists.
    """

    def __init__(self):
        self.calls = []

    def __enter__(self):
        self._original = swap_v2.run_chain_cli

        def fake(label, cmd, stdin_text=None):
            self.calls.append((label, list(cmd), stdin_text))
            return {}

        swap_v2.run_chain_cli = fake
        return self

    def __exit__(self, *exc):
        swap_v2.run_chain_cli = self._original
        return False

    def one(self):
        if len(self.calls) != 1:
            raise AssertionError(f"expected one CLI call, got {self.calls}")

        return self.calls[0]


OVERSIZED = "ab" * 60_000


def test_ordinary_arguments_stay_in_argv():
    with captured() as cap:
        swap_v2.blake_cli("getblockcount")

    label, cmd, stdin_text = cap.one()

    if label != "BLAKE":
        raise AssertionError(f"wrong label: {label}")

    if "-regtest" not in cmd:
        raise AssertionError(f"BLAKE CLI lost -regtest: {cmd}")

    if cmd[-1] != "getblockcount":
        raise AssertionError(f"command is not last in argv: {cmd}")

    if stdin_text is not None:
        raise AssertionError("a short call used stdin transport")

    if "-stdin" in cmd:
        raise AssertionError("a short call used -stdin")


def test_booleans_render_as_json_literals():
    with captured() as cap:
        swap_v2.btc_cli("sendtoaddress", "addr", 0.1, True, False)

    _, cmd, stdin_text = cap.one()

    if stdin_text is not None:
        raise AssertionError("a short call used stdin transport")

    if cmd[-2:] != ["true", "false"]:
        raise AssertionError(f"booleans rendered wrongly: {cmd}")


def test_wallet_selection_is_preserved():
    with captured() as cap:
        swap_v2.btc_cli("getbalance", wallet="alice")

    _, cmd, _ = cap.one()

    if "-rpcwallet=alice" not in cmd:
        raise AssertionError(f"wallet selection lost: {cmd}")


def test_oversized_argument_uses_stdin():
    with captured() as cap:
        swap_v2.blake_cli("decoderawtransaction", OVERSIZED)

    _, cmd, stdin_text = cap.one()

    if OVERSIZED in cmd:
        raise AssertionError("the oversized value is still in argv")

    if cmd[-1] != "-stdin":
        raise AssertionError(f"-stdin was not requested: {cmd}")

    if stdin_text != f"decoderawtransaction\n{OVERSIZED}\n":
        raise AssertionError(
            "stdin payload is not the command and value, in order"
        )


def test_oversized_call_keeps_argument_order_and_types():
    with captured() as cap:
        swap_v2.btc_cli("decoderawtransaction", OVERSIZED, False)

    _, cmd, stdin_text = cap.one()

    if cmd[-1] != "-stdin":
        raise AssertionError(f"-stdin was not requested: {cmd}")

    if stdin_text != f"decoderawtransaction\n{OVERSIZED}\nfalse\n":
        raise AssertionError(
            f"stdin payload lost order or types: {stdin_text[:80]!r}"
        )


def test_the_boundary_is_exact():
    limit = swap_v2.ARGV_SAFE_LENGTH

    with captured() as cap:
        swap_v2.btc_cli("decoderawtransaction", "a" * limit)

    _, cmd, stdin_text = cap.one()

    if stdin_text is not None or "-stdin" in cmd:
        raise AssertionError("an argument at the limit left argv")

    with captured() as cap:
        swap_v2.btc_cli("decoderawtransaction", "a" * (limit + 1))

    _, cmd, stdin_text = cap.one()

    if stdin_text is None or cmd[-1] != "-stdin":
        raise AssertionError("an argument over the limit stayed in argv")


def test_exec_failure_becomes_a_named_error():
    fake = FakeSubprocess(
        raises=OSError(errno.E2BIG, "Argument list too long")
    )

    original = swap_v2.subprocess
    swap_v2.subprocess = fake

    try:
        swap_v2.run_chain_cli("BLAKE", ["bitcoin-cli", "getblockcount"])
    except RuntimeError as e:
        message = str(e)
    except Exception as e:  # noqa: BLE001
        raise AssertionError(
            f"an exec failure surfaced as {type(e).__name__}: {e}"
        ) from e
    else:
        raise AssertionError("a failing exec did not raise")
    finally:
        swap_v2.subprocess = original

    if "BLAKE" not in message:
        raise AssertionError(f"the error does not name the chain: {message}")

    if "Argument list too long" not in message:
        raise AssertionError(f"the error lost the cause: {message}")


def test_timeout_names_the_override():
    fake = FakeSubprocess(
        raises=subprocess.TimeoutExpired(cmd="bitcoin-cli", timeout=1)
    )

    original = swap_v2.subprocess
    swap_v2.subprocess = fake

    try:
        swap_v2.run_chain_cli("Bitcoin", ["bitcoin-cli", "getblockcount"])
    except RuntimeError as e:
        message = str(e)
    else:
        raise AssertionError("a timed-out RPC did not raise")
    finally:
        swap_v2.subprocess = original

    if "BLAKE_SWAP_RPC_TIMEOUT_SECONDS" not in message:
        raise AssertionError(
            f"the timeout error does not name the knob: {message}"
        )


def test_results_are_decoded_and_text_passes_through():
    fake = FakeSubprocess(
        result=SimpleNamespace(
            returncode=0,
            stdout=json.dumps({"blocks": 7}),
            stderr="",
        )
    )

    original = swap_v2.subprocess
    swap_v2.subprocess = fake

    try:
        decoded = swap_v2.run_chain_cli("BLAKE", ["bitcoin-cli"])
    finally:
        swap_v2.subprocess = original

    if decoded != {"blocks": 7}:
        raise AssertionError(f"JSON result was not decoded: {decoded!r}")

    fake = FakeSubprocess(
        result=SimpleNamespace(returncode=0, stdout="not json\n", stderr="")
    )

    swap_v2.subprocess = fake

    try:
        decoded = swap_v2.run_chain_cli("BLAKE", ["bitcoin-cli"])
    finally:
        swap_v2.subprocess = original

    if decoded != "not json":
        raise AssertionError(f"plain text was not passed through: {decoded!r}")


def test_nonzero_exit_raises_the_node_error():
    fake = FakeSubprocess(
        result=SimpleNamespace(
            returncode=1,
            stdout="",
            stderr="error code: -8\nerror message:\nInvalid parameter\n",
        )
    )

    original = swap_v2.subprocess
    swap_v2.subprocess = fake

    try:
        swap_v2.run_chain_cli("BLAKE", ["bitcoin-cli"])
    except RuntimeError as e:
        if "Invalid parameter" not in str(e):
            raise AssertionError(f"the node error was lost: {e}") from e
    else:
        raise AssertionError("a failed RPC did not raise")
    finally:
        swap_v2.subprocess = original


def test_real_node_accepts_the_stdin_transport():
    """
    Force the stdin path against the real node.

    The threshold is lowered for one call rather than synthesising a
    128 KiB transaction: the point is to prove that the node accepts the
    command through stdin, not to reproduce the original oversize.
    """

    problems = node_config.node_config_problems()

    if problems:
        print("SKIP  real-node stdin transport (no chain nodes configured)")
        return

    original_limit = swap_v2.ARGV_SAFE_LENGTH

    try:
        swap_v2.ARGV_SAFE_LENGTH = 1

        block_hash = swap_v2.blake_cli("getblockhash", 1)
        block = swap_v2.blake_cli("getblock", block_hash, 2)
        tx_hex = block["tx"][0]["hex"]

        decoded = swap_v2.blake_cli("decoderawtransaction", tx_hex)
    finally:
        swap_v2.ARGV_SAFE_LENGTH = original_limit

    if decoded.get("txid") != block["tx"][0]["txid"]:
        raise AssertionError(
            "the stdin transport decoded a different transaction: "
            f"{decoded.get('txid')} != {block['tx'][0]['txid']}"
        )

    if int(decoded.get("vsize", 0)) < 1:
        raise AssertionError(f"decoded transaction has no vsize: {decoded}")


def main():
    print()
    print("=" * 72)
    print("CHAIN CLI TRANSPORT")
    print("=" * 72)
    print()
    print(f"argv-safe length: {swap_v2.ARGV_SAFE_LENGTH}")
    print()

    check("ordinary arguments stay in argv", test_ordinary_arguments_stay_in_argv)
    check("booleans render as JSON literals", test_booleans_render_as_json_literals)
    check("wallet selection is preserved", test_wallet_selection_is_preserved)
    check("an oversized argument switches to stdin", test_oversized_argument_uses_stdin)
    check("the stdin payload keeps order and types", test_oversized_call_keeps_argument_order_and_types)
    check("the argv/stdin boundary is exact", test_the_boundary_is_exact)
    check("a failed exec becomes a named error", test_exec_failure_becomes_a_named_error)
    check("a timeout names the override", test_timeout_names_the_override)
    check("results are decoded, text passes through", test_results_are_decoded_and_text_passes_through)
    check("a non-zero exit raises the node error", test_nonzero_exit_raises_the_node_error)
    check("the real node accepts the stdin transport", test_real_node_accepts_the_stdin_transport)

    print()
    print("=" * 72)
    print("CHAIN CLI TRANSPORT SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
