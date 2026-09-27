#!/usr/bin/env python3
"""
Shared chain-node configuration for the blake-swap test suites.

The test suites must not assume any particular host layout. Every value
is resolved from the environment first, then from a list of conventional
locations. This module is deliberately dependency-free and has no
import-time side effects beyond resolving paths, so that isolated suites
can use it without pulling in the full integration harness.

Environment variables (the same names the engine reads):

    BLAKE_SWAP_BTC_CLI
    BLAKE_SWAP_BTC_DATADIR
    BLAKE_SWAP_BTC_RPC_ARGS
    BLAKE_SWAP_BLAKE_CLI
    BLAKE_SWAP_BLAKE_DATADIR
    BLAKE_SWAP_BLAKE_RPC_ARGS

For a remote node, set the matching ``*_RPC_ARGS`` variable (for example
``-regtest -rpcconnect=host -rpcport=18443``) and leave the datadir
unset.
"""

from __future__ import annotations

import json
import os
import shlex
import shutil
import sys
from pathlib import Path


HOME = Path.home()

PROJECT_DIR = Path(__file__).resolve().parent.parent

#
# The local two-chain rig created by scripts/test-nodes. When it is
# running, its nodes are the preferred target: they are disposable,
# regtest-only, and already funded, so a suite can never accidentally be
# pointed at a mainnet datadir by a bare `bitcoin-cli` on PATH.
#
RIG_ROOT = Path(
    os.environ.get("BLAKE_SWAP_TEST_ROOT", HOME / ".blake-swap-test")
).expanduser()

RIG_ENV_FILE = RIG_ROOT / "rig-env.json"

SWAP_V2 = Path(
    os.environ.get(
        "SWAP_V2",
        PROJECT_DIR / "src" / "swap_v2.py",
    )
)

#
# Exit code used for "prerequisites absent, nothing was tested".
#
# 77 is the conventional automake/autotools skip code and is understood
# by scripts/test.sh, which reports such suites as SKIP rather than
# silently omitting them or miscounting them as failures.
#
SKIP_EXIT_CODE = 77


def skip(reason, *, details=None, hint=None):
    """
    Abort the current suite with the "prerequisites absent" exit code.

    Derives from SystemExit rather than Exception on purpose: every test
    module wraps its body in ``except Exception``, and a missing node or
    key is not a test failure.
    """
    print()
    print("=" * 72)
    print(f"SKIP: {reason}")
    print("=" * 72)
    print("This suite was NOT run. Its prerequisites are not satisfied.")
    print()

    for line in details or []:
        print(f"  {line}")

    if details:
        print()

    if hint:
        print(hint)
        print()

    raise SystemExit(SKIP_EXIT_CODE)


def _existing_executable(value):
    if not value:
        return None

    path = Path(str(value)).expanduser()

    if path.exists() and os.access(path, os.X_OK):
        return path

    found = shutil.which(str(value))

    return Path(found) if found else None


def _resolve_executable(env_var, candidates):
    """
    Resolve a chain CLI executable.

    Returns ``(path_or_None, human_readable_source_of_truth)``.

    A bare name (for example ``bitcoin-cli``) is resolved through PATH.
    An explicitly set environment variable wins even if it does not
    resolve, so a typo produces a clear diagnostic instead of silently
    falling back to a different node.
    """
    explicit = os.environ.get(env_var)

    if explicit:
        found = _existing_executable(explicit)

        if found is None:
            return None, f"{env_var}={explicit} (not executable)"

        return found, env_var

    for candidate in candidates:
        found = _existing_executable(candidate)

        if found is not None:
            return found, str(candidate)

    return None, None


def _resolve_datadir(env_var, candidates):
    """
    Resolve a chain data directory.

    Returns ``(path_or_None, human_readable_source_of_truth)``.
    """
    explicit = os.environ.get(env_var)

    if explicit:
        path = Path(explicit).expanduser()

        if path.is_dir():
            return path, env_var

        return None, f"{env_var}={explicit} (not a directory)"

    for candidate in candidates:
        path = Path(str(candidate)).expanduser()

        if path.is_dir():
            return path, str(path)

    return None, None


def _rig_env():
    """
    Read the state file that scripts/test-nodes writes when it starts.

    Returns ``{}`` when no rig has been started from this checkout. The
    file, rather than the mere existence of a datadir, is what makes the
    rig authoritative: a stopped rig must produce a SKIP, not a suite
    that connects to nothing and fails.
    """
    try:
        with RIG_ENV_FILE.open("r", encoding="utf-8") as handle:
            state = json.load(handle)
    except (OSError, ValueError):
        return {}

    return state if isinstance(state, dict) else {}


RIG = _rig_env()

RIG_RUNNING = bool(RIG.get("running"))


def _rig_candidates(key, default):
    """Rig paths, but only while the rig reports itself as running."""

    if not RIG_RUNNING:
        return []

    value = RIG.get(key) or default

    return [Path(str(value)).expanduser()]


BTC_RPC_ARGS = tuple(
    shlex.split(os.environ.get("BLAKE_SWAP_BTC_RPC_ARGS", ""))
)

BLAKE_RPC_ARGS = tuple(
    shlex.split(os.environ.get("BLAKE_SWAP_BLAKE_RPC_ARGS", ""))
)


BTC_CLI, BTC_CLI_SOURCE = _resolve_executable(
    "BLAKE_SWAP_BTC_CLI",
    [
        # A running scripts/test-nodes rig, then the conventional
        # clone-and-symlink layout it is installed from.
        *_rig_candidates("btc_cli", HOME / "s" / "bitcoin-core" / "bin" / "bitcoin-cli"),
        HOME / "s" / "bitcoin-core" / "bin" / "bitcoin-cli",
        HOME / "s" / "bitcoin" / "bin" / "bitcoin-cli",
        HOME / "bitcoin-core" / "build" / "bin" / "bitcoin-cli",
        HOME / "bitcoin" / "build" / "bin" / "bitcoin-cli",
        "/usr/local/bin/bitcoin-cli",
        "/usr/bin/bitcoin-cli",
        "bitcoin-cli",
        # Historical author-host layout, kept as a last resort so the
        # original clean-room environment still works unconfigured.
        "/home/gary/bitcoin-core/build/bin/bitcoin-cli",
    ],
)

#
# The BLAKE CLI is deliberately NOT resolved through PATH: a generic
# `bitcoin-cli` on PATH is almost certainly Bitcoin Core, and using it
# for the BLAKE chain would silently test the wrong node.
#
BLAKE_CLI, BLAKE_CLI_SOURCE = _resolve_executable(
    "BLAKE_SWAP_BLAKE_CLI",
    [
        *_rig_candidates("blake_cli", HOME / "s" / "bitcoin-knots" / "bin" / "bitcoin-cli"),
        HOME / "s" / "bitcoin-knots" / "bin" / "bitcoin-cli",
        HOME / "bitcoin-blake-rc2" / "build-rc2" / "bin" / "bitcoin-cli",
        HOME / "bitcoin-blake-rc2" / "build" / "bin" / "bitcoin-cli",
        HOME / "bitcoin-blake" / "build" / "bin" / "bitcoin-cli",
        "/home/gary/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli",
    ],
)

#
# Order matters: the disposable rig first, then explicit regtest
# datadirs, and a general Bitcoin datadir last. A regtest run out of
# ``~/.bitcoin`` is legitimate, but it must never outrank a dedicated
# test datadir.
#
BTC_DATADIR, BTC_DATADIR_SOURCE = _resolve_datadir(
    "BLAKE_SWAP_BTC_DATADIR",
    [
        *_rig_candidates("btc_datadir", RIG_ROOT / "btc"),
        HOME / "bitcoin-swap-regtest",
        HOME / ".bitcoin",
        "/home/gary/bitcoin-swap-regtest",
    ],
)

BLAKE_DATADIR, BLAKE_DATADIR_SOURCE = _resolve_datadir(
    "BLAKE_SWAP_BLAKE_DATADIR",
    [
        *_rig_candidates("blake_datadir", RIG_ROOT / "blake"),
        HOME / "blake-swap-regtest",
        HOME / ".blake-swap-regtest",
        "/home/gary/blake-swap-regtest",
    ],
)


def node_config_summary():
    """Human-readable resolution of the chain node configuration."""

    def shown(path, source):
        if path is not None:
            return f"{path}  [{source}]"
        return f"<unresolved>  [{source or 'no candidate found'}]"

    return [
        f"Bitcoin CLI:      {shown(BTC_CLI, BTC_CLI_SOURCE)}",
        f"Bitcoin datadir:  {shown(BTC_DATADIR, BTC_DATADIR_SOURCE)}",
        f"Bitcoin RPC args: {list(BTC_RPC_ARGS)}",
        f"BLAKE CLI:        {shown(BLAKE_CLI, BLAKE_CLI_SOURCE)}",
        f"BLAKE datadir:    {shown(BLAKE_DATADIR, BLAKE_DATADIR_SOURCE)}",
        f"BLAKE RPC args:   {list(BLAKE_RPC_ARGS)}",
    ]


def node_config_problems():
    """
    Return a list of unresolved node-configuration problems.

    A datadir is only required when no explicit RPC args are supplied;
    a remote node needs ``-regtest`` and connection arguments instead.
    """
    problems = []

    #
    # A checkout that has a rig state file saying "stopped" must skip
    # rather than quietly fall through to some other datadir: the rig is
    # the intended target, and a generic `~/.bitcoin` is not a
    # substitute for a regtest chain the suites can spend on.
    #
    if RIG and not RIG_RUNNING:
        problems.append(
            "The scripts/test-nodes rig is stopped. Start it with "
            "`scripts/test-nodes start`."
        )

    if BTC_CLI is None:
        problems.append(
            "Bitcoin CLI unresolved. Set BLAKE_SWAP_BTC_CLI to your "
            "bitcoin-cli path."
        )

    if BLAKE_CLI is None:
        problems.append(
            "BLAKE CLI unresolved. Set BLAKE_SWAP_BLAKE_CLI to the BLAKE "
            "build's bitcoin-cli (not Bitcoin Core)."
        )

    if BTC_CLI is not None and BTC_DATADIR is None and not BTC_RPC_ARGS:
        problems.append(
            "Bitcoin datadir unresolved. Set BLAKE_SWAP_BTC_DATADIR, or "
            "set BLAKE_SWAP_BTC_RPC_ARGS for a remote node."
        )

    if BLAKE_CLI is not None and BLAKE_DATADIR is None and not BLAKE_RPC_ARGS:
        problems.append(
            "BLAKE datadir unresolved. Set BLAKE_SWAP_BLAKE_DATADIR, or "
            "set BLAKE_SWAP_BLAKE_RPC_ARGS for a remote node."
        )

    return problems


def node_env():
    """
    Environment variables that point a child process at the resolved
    chain nodes.

    Only resolved values are included, so an unresolved value can never
    be passed on as the literal string ``"None"`` and shadow the
    engine's own defaults.
    """
    env = {}

    for variable, value in (
        ("BLAKE_SWAP_BTC_CLI", BTC_CLI),
        ("BLAKE_SWAP_BLAKE_CLI", BLAKE_CLI),
        ("BLAKE_SWAP_BTC_DATADIR", BTC_DATADIR),
        ("BLAKE_SWAP_BLAKE_DATADIR", BLAKE_DATADIR),
    ):
        if value is not None:
            env[variable] = str(value)

    if BTC_RPC_ARGS:
        env["BLAKE_SWAP_BTC_RPC_ARGS"] = " ".join(BTC_RPC_ARGS)

    if BLAKE_RPC_ARGS:
        env["BLAKE_SWAP_BLAKE_RPC_ARGS"] = " ".join(BLAKE_RPC_ARGS)

    return env


def install_into_environ():
    """
    Publish the resolved node configuration into ``os.environ``.

    A suite that imports the engine *in process* must do this before the
    import: the engine resolves its own chain paths from the environment
    at import time, and would otherwise fall back to its built-in
    author-host defaults even though this module resolved a rig.

    Subprocess-driving suites do not need it -- the harness injects
    :func:`node_env` into every child -- but it is harmless there too.
    """
    os.environ.update(node_env())


def require_node_config():
    """
    Abort with a SKIP if the node configuration cannot be resolved.

    Raises nothing when the configuration is complete; reachability is
    the caller's responsibility.
    """
    problems = node_config_problems()

    if problems:
        skip(
            "chain node prerequisites are not satisfied",
            details=node_config_summary() + [""] + problems,
            hint=(
                "See tests/README.md for how to point the suites at "
                "your nodes."
            ),
        )


def require_coincurve():
    """
    Abort with a SKIP if coincurve is not importable.

    The engine needs coincurve to sign Nostr events, so a suite that
    exercises the signing path cannot run without it. This is a
    dependency prerequisite rather than a node prerequisite.
    """
    try:
        import coincurve  # noqa: F401
    except ImportError:
        skip(
            "coincurve is not importable by this interpreter",
            details=[f"interpreter: {sys.executable}"],
            hint=(
                "Install the declared dependencies with:\n"
                f'  "{sys.executable}" -m pip install -r requirements.txt'
            ),
        )


if __name__ == "__main__":
    print("CHAIN NODE CONFIGURATION")
    print("========================")
    print()

    for line in node_config_summary():
        print(line)

    print()
    problems = node_config_problems()

    if problems:
        print("PROBLEMS")
        print("--------")

        for problem in problems:
            print(f"  - {problem}")

        print()
        sys.exit(SKIP_EXIT_CODE)

    print("READY: node configuration is complete.")
