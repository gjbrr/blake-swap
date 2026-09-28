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
import os
import shutil
import subprocess
import sys
import tempfile
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
    """
    Stand-in for the ``subprocess`` module inside swap_v2.

    ``results`` allows a different outcome per attempt, which is how the
    retry path is reached: a first call that times out, a second that
    answers. Each entry is either an exception to raise or a result.
    """

    TimeoutExpired = subprocess.TimeoutExpired

    def __init__(self, *, result=None, raises=None, results=None):
        self.result = result
        self.raises = raises
        self.results = list(results) if results is not None else None
        self.calls = []

    def run(self, cmd, **kwargs):
        self.calls.append((list(cmd), kwargs))

        if self.results is not None:
            outcome = self.results.pop(0)

            if isinstance(outcome, BaseException):
                raise outcome

            return outcome

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

        def fake(label, cmd, stdin_text=None, method=None):
            self.calls.append((label, list(cmd), stdin_text, method))
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

    label, cmd, stdin_text, _method = cap.one()

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

    _, cmd, stdin_text, _method = cap.one()

    if stdin_text is not None:
        raise AssertionError("a short call used stdin transport")

    if cmd[-2:] != ["true", "false"]:
        raise AssertionError(f"booleans rendered wrongly: {cmd}")


def test_structured_arguments_are_json_encoded():
    """
    `importdescriptors` takes a JSON array and the node rejects anything
    else with "Error parsing JSON". A list or dict must therefore be
    encoded, not repr'd: `str([{"desc": ...}])` uses single quotes and
    the node cannot read it.
    """
    assert swap_v2._rpc_argument([{"desc": "wsh(x)#abcd1234"}]) == (
        '[{"desc": "wsh(x)#abcd1234"}]'
    )
    assert swap_v2._rpc_argument({"a": 1, "b": [True, None]}) == (
        '{"a": 1, "b": [true, null]}'
    )

    # The scalar cases are unchanged.
    assert swap_v2._rpc_argument(True) == "true"
    assert swap_v2._rpc_argument(False) == "false"
    assert swap_v2._rpc_argument(7) == "7"
    assert swap_v2._rpc_argument("text") == "text"


def test_wallet_selection_is_preserved():
    with captured() as cap:
        swap_v2.btc_cli("getbalance", wallet="alice")

    _, cmd, _, _method = cap.one()

    if "-rpcwallet=alice" not in cmd:
        raise AssertionError(f"wallet selection lost: {cmd}")


def test_oversized_argument_uses_stdin():
    with captured() as cap:
        swap_v2.blake_cli("decoderawtransaction", OVERSIZED)

    _, cmd, stdin_text, _method = cap.one()

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

    _, cmd, stdin_text, _method = cap.one()

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

    _, cmd, stdin_text, _method = cap.one()

    if stdin_text is not None or "-stdin" in cmd:
        raise AssertionError("an argument at the limit left argv")

    with captured() as cap:
        swap_v2.btc_cli("decoderawtransaction", "a" * (limit + 1))

    _, cmd, stdin_text, _method = cap.one()

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


def test_a_warning_before_the_json_does_not_hide_it():
    """
    The reply is JSON that happens to be preceded by a warning.

    Treating the whole stream as text would hand a caller that expects an
    object a string, and the caller would then fail somewhere unrelated to
    the cause.
    """
    original = swap_v2.subprocess

    for stdout, expected in (
        ('Warning: unknown setting\n{"blocks": 7}\n', {"blocks": 7}),
        ('Warning: one\nWarning: two\n[1, 2, 3]\n', [1, 2, 3]),
        ('{"blocks": 7}\ntrailing text\n', {"blocks": 7}),
    ):
        swap_v2.subprocess = FakeSubprocess(
            result=SimpleNamespace(
                returncode=0,
                stdout=stdout,
                stderr="",
            )
        )

        try:
            decoded = swap_v2.run_chain_cli("BLAKE", ["bitcoin-cli"])
        finally:
            swap_v2.subprocess = original

        if decoded != expected:
            raise AssertionError(
                f"warning-prefixed reply decoded as {decoded!r}, "
                f"expected {expected!r}"
            )

    #
    # And genuine text still passes through: a reply with no JSON document
    # in it is not made into one.
    #
    swap_v2.subprocess = FakeSubprocess(
        result=SimpleNamespace(
            returncode=0,
            stdout="error: no JSON here\n",
            stderr="",
        )
    )

    try:
        decoded = swap_v2.run_chain_cli("BLAKE", ["bitcoin-cli"])
    finally:
        swap_v2.subprocess = original

    if decoded != "error: no JSON here":
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



def test_a_stalled_node_is_retried_against_the_real_cli():
    """
    A node that takes longer than the timeout, and then answers.

    The stall is a wrapper around the *real* bitcoin-cli that sleeps on
    its first invocation only. That matters: the timeout under test is a
    genuinely slow process being killed, not an injected exception, and
    the retry then has something real to recover.

    The second half disables retries and requires the same stall to fail,
    which is what shows the first half passed *because* of the retry
    rather than because the stall never happened.
    """
    problems = node_config.node_config_problems()

    if problems:
        print("SKIP  stalled-node retry (no chain nodes configured)")
        return

    real_cli = swap_v2.BLAKE_CLI
    wrapper_dir = Path(tempfile.mkdtemp(prefix="blake-stall-"))
    marker = wrapper_dir / "stalled"
    wrapper = wrapper_dir / "slow-bitcoin-cli"

    wrapper.write_text(
        "#!/bin/sh\n"
        f"if [ ! -f {marker} ]; then\n"
        f"  : > {marker}\n"
        "  sleep 3\n"
        "fi\n"
        f'exec "{real_cli}" "$@"\n'
    )
    wrapper.chmod(0o755)

    names = (
        "BLAKE_SWAP_RPC_TIMEOUT_SECONDS",
        "BLAKE_SWAP_RPC_MAX_ATTEMPTS",
        "BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS",
    )
    saved = {name: os.environ.get(name) for name in names}

    try:
        swap_v2.BLAKE_CLI = str(wrapper)

        os.environ["BLAKE_SWAP_RPC_TIMEOUT_SECONDS"] = "1"
        os.environ["BLAKE_SWAP_RPC_MAX_ATTEMPTS"] = "3"
        os.environ["BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS"] = "4"

        height = swap_v2.blake_cli("getblockcount")

        if not isinstance(height, int) or height < 0:
            raise AssertionError(f"the retry returned {height!r}")

        if not marker.exists():
            raise AssertionError(
                "the wrapper never stalled, so this proved nothing"
            )

        marker.unlink()
        os.environ["BLAKE_SWAP_RPC_MAX_ATTEMPTS"] = "1"

        try:
            swap_v2.blake_cli("getblockcount")
        except RuntimeError as e:
            if "timed out" not in str(e):
                raise AssertionError(
                    f"the stall failed for the wrong reason: {e}"
                )
        else:
            raise AssertionError(
                "a stalled node answered with retries disabled, so the "
                "stall was not real"
            )
    finally:
        swap_v2.BLAKE_CLI = real_cli

        for name, value in saved.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value

        shutil.rmtree(wrapper_dir, ignore_errors=True)

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



def with_env(**values):
    """Set environment variables for the duration of a block."""
    import contextlib

    @contextlib.contextmanager
    def manager():
        saved = {name: os.environ.get(name) for name in values}

        os.environ.update({name: str(v) for name, v in values.items()})

        try:
            yield
        finally:
            for name, value in saved.items():
                if value is None:
                    os.environ.pop(name, None)
                else:
                    os.environ[name] = value

    return manager()


ANSWER = SimpleNamespace(returncode=0, stdout='{"blocks": 7}\n', stderr="")


def test_a_slow_read_only_call_is_retried():
    """
    A node that is merely slow must not fail a read.

    The first attempt times out and the second answers, which is exactly
    the "slow rather than down" case the timeout alone used to turn into a
    hard failure.
    """
    original = swap_v2.subprocess
    fake = FakeSubprocess(
        results=[
            subprocess.TimeoutExpired(cmd="bitcoin-cli", timeout=1),
            ANSWER,
        ]
    )
    swap_v2.subprocess = fake

    try:
        with with_env(
            BLAKE_SWAP_RPC_TIMEOUT_SECONDS="1",
            BLAKE_SWAP_RPC_MAX_ATTEMPTS="3",
            BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS="0",
        ):
            decoded = swap_v2.run_chain_cli(
                "BLAKE",
                ["bitcoin-cli", "getblockcount"],
                method="getblockcount",
            )
    finally:
        swap_v2.subprocess = original

    if decoded != {"blocks": 7}:
        raise AssertionError(f"the retry did not recover the answer: {decoded!r}")

    if len(fake.calls) != 2:
        raise AssertionError(f"expected two attempts, got {len(fake.calls)}")


def test_a_slow_state_changing_call_is_not_retried():
    """
    After a timeout on a call that can change state, the engine does not
    know whether the node acted, so it must not guess by sending again.

    A repeated `sendtoaddress` funds a swap twice. The operator is told
    what happened and how re-running resolves it.
    """
    original = swap_v2.subprocess
    fake = FakeSubprocess(
        results=[
            subprocess.TimeoutExpired(cmd="bitcoin-cli", timeout=1),
            ANSWER,
        ]
    )
    swap_v2.subprocess = fake

    try:
        with with_env(
            BLAKE_SWAP_RPC_TIMEOUT_SECONDS="1",
            BLAKE_SWAP_RPC_MAX_ATTEMPTS="3",
            BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS="0",
        ):
            try:
                swap_v2.run_chain_cli(
                    "BTC",
                    ["bitcoin-cli", "sendtoaddress"],
                    method="sendtoaddress",
                )
            except RuntimeError as e:
                message = str(e)
            else:
                raise AssertionError("a timed-out send did not raise")
    finally:
        swap_v2.subprocess = original

    if len(fake.calls) != 1:
        raise AssertionError(
            f"a state-changing call was retried {len(fake.calls)} times"
        )

    for expected in ("NOT", "retried", "may or may not have acted", "re-run"):
        if expected not in message:
            raise AssertionError(
                f"the refusal did not explain the ambiguity ({expected!r}): "
                f"{message}"
            )


def test_an_unknown_method_is_not_retried():
    """
    The retry list is an allowlist on purpose: a method nobody classified
    gets the behaviour the engine had before retries existed.
    """
    if swap_v2.rpc_method_is_retryable("sendtoaddress"):
        raise AssertionError("a send became retryable")

    if swap_v2.rpc_method_is_retryable("somefuturecall"):
        raise AssertionError("an unclassified method became retryable")

    if not swap_v2.rpc_method_is_retryable("getblockcount"):
        raise AssertionError("a chain read is not retryable")


def test_transient_connection_failures_are_retried():
    """
    A refused connection is a node that is not there *yet*; a missing
    binary is not. Only the first kind is worth another attempt.
    """
    original = swap_v2.subprocess

    fake = FakeSubprocess(
        results=[
            ConnectionRefusedError(errno.ECONNREFUSED, "Connection refused"),
            ANSWER,
        ]
    )
    swap_v2.subprocess = fake

    try:
        with with_env(
            BLAKE_SWAP_RPC_MAX_ATTEMPTS="3",
            BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS="0",
        ):
            decoded = swap_v2.run_chain_cli(
                "BLAKE",
                ["bitcoin-cli", "getblockcount"],
                method="getblockcount",
            )
    finally:
        swap_v2.subprocess = original

    if decoded != {"blocks": 7} or len(fake.calls) != 2:
        raise AssertionError(
            f"a refused connection was not retried: {decoded!r}, "
            f"{len(fake.calls)} attempts"
        )

    fake = FakeSubprocess(raises=FileNotFoundError(errno.ENOENT, "no cli"))
    swap_v2.subprocess = fake

    try:
        with with_env(
            BLAKE_SWAP_RPC_MAX_ATTEMPTS="3",
            BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS="0",
        ):
            try:
                swap_v2.run_chain_cli(
                    "BLAKE",
                    ["bitcoin-cli", "getblockcount"],
                    method="getblockcount",
                )
            except RuntimeError as e:
                assert "could not be executed" in str(e), e
            else:
                raise AssertionError("a missing CLI did not raise")
    finally:
        swap_v2.subprocess = original

    if len(fake.calls) != 1:
        raise AssertionError(
            f"a missing binary was retried {len(fake.calls)} times"
        )


def test_retries_are_bounded_and_configured():
    """Exhausting the attempts fails, and the knobs are validated."""
    original = swap_v2.subprocess
    fake = FakeSubprocess(
        raises=subprocess.TimeoutExpired(cmd="bitcoin-cli", timeout=1)
    )
    swap_v2.subprocess = fake

    try:
        with with_env(
            BLAKE_SWAP_RPC_TIMEOUT_SECONDS="1",
            BLAKE_SWAP_RPC_MAX_ATTEMPTS="2",
            BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS="0",
        ):
            try:
                swap_v2.run_chain_cli(
                    "BLAKE",
                    ["bitcoin-cli", "getblockcount"],
                    method="getblockcount",
                )
            except RuntimeError as e:
                message = str(e)
            else:
                raise AssertionError("a permanently slow node did not raise")
    finally:
        swap_v2.subprocess = original

    if len(fake.calls) != 2:
        raise AssertionError(f"expected 2 attempts, got {len(fake.calls)}")

    if "attempt 2/2" not in message:
        raise AssertionError(f"the failure did not count attempts: {message}")

    for name, value in (
        ("BLAKE_SWAP_RPC_MAX_ATTEMPTS", "0"),
        ("BLAKE_SWAP_RPC_MAX_ATTEMPTS", "many"),
        ("BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS", "-1"),
        ("BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS", "soon"),
        ("BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS", "-1"),
        ("BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS", "later"),
    ):
        with with_env(**{name: value}):
            try:
                if name == "BLAKE_SWAP_RPC_MAX_ATTEMPTS":
                    swap_v2.rpc_max_attempts()
                else:
                    swap_v2.rpc_retry_backoff_seconds()
            except RuntimeError:
                pass
            else:
                raise AssertionError(f"{name}={value!r} was accepted")

    # The cap bounds the backoff rather than the base.
    with with_env(
        BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS="100",
        BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS="2",
    ):
        assert swap_v2.rpc_retry_backoff_seconds() == 2.0

    if swap_v2.rpc_method_is_retryable(None):
        raise AssertionError("a missing method name was treated as retryable")

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
    check("structured arguments are JSON encoded", test_structured_arguments_are_json_encoded)
    check("wallet selection is preserved", test_wallet_selection_is_preserved)
    check("an oversized argument switches to stdin", test_oversized_argument_uses_stdin)
    check("the stdin payload keeps order and types", test_oversized_call_keeps_argument_order_and_types)
    check("the argv/stdin boundary is exact", test_the_boundary_is_exact)
    check("a failed exec becomes a named error", test_exec_failure_becomes_a_named_error)
    check("a timeout names the override", test_timeout_names_the_override)
    check("a slow read-only call is retried", test_a_slow_read_only_call_is_retried)
    check("a slow state-changing call is not retried", test_a_slow_state_changing_call_is_not_retried)
    check("an unknown method is not retried", test_an_unknown_method_is_not_retried)
    check("transient connection failures are retried", test_transient_connection_failures_are_retried)
    check("retries are bounded and configured", test_retries_are_bounded_and_configured)
    check("a warning before the JSON does not hide it", test_a_warning_before_the_json_does_not_hide_it)
    check("results are decoded, text passes through", test_results_are_decoded_and_text_passes_through)
    check("a non-zero exit raises the node error", test_nonzero_exit_raises_the_node_error)
    check("the real node accepts the stdin transport", test_real_node_accepts_the_stdin_transport)
    check("a stalled node is retried against the real cli", test_a_stalled_node_is_retried_against_the_real_cli)

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
