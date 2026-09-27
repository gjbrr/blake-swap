#!/usr/bin/env python3
"""
Node configuration resolution suite.

Every node-dependent suite decides whether it can run at all from
``tests/node_config.py``. When that resolution is wrong the failure is
silent in the worst possible way: either a suite claims to have run
while actually talking to nothing, or a perfectly good regtest rig sits
unused because discovery did not find it.

This suite pins the resolution rules, without needing nodes or relays:

  * an explicit environment variable always wins;
  * a running scripts/test-nodes rig is discovered automatically;
  * a stopped rig produces a SKIP reason instead of falling through to
    an unrelated datadir;
  * a checkout that has never started a rig is not told that its rig is
    stopped;
  * ``node_env()`` never renders an unresolved value as ``"None"``.

Each case runs in a throwaway interpreter with a scrubbed environment,
because node_config resolves its constants at import time.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
TESTS = ROOT / "tests"

PYTHON = sys.executable

PROBE = (
    "import json, sys;"
    f"sys.path.insert(0, {str(TESTS)!r});"
    "import node_config as nc;"
    "print(json.dumps({"
    "'rig_running': nc.RIG_RUNNING,"
    "'rig_file': str(nc.RIG_ENV_FILE),"
    "'btc_cli': str(nc.BTC_CLI) if nc.BTC_CLI else None,"
    "'btc_cli_source': nc.BTC_CLI_SOURCE,"
    "'blake_cli': str(nc.BLAKE_CLI) if nc.BLAKE_CLI else None,"
    "'blake_cli_source': nc.BLAKE_CLI_SOURCE,"
    "'btc_datadir': str(nc.BTC_DATADIR) if nc.BTC_DATADIR else None,"
    "'btc_datadir_source': nc.BTC_DATADIR_SOURCE,"
    "'blake_datadir': str(nc.BLAKE_DATADIR) if nc.BLAKE_DATADIR else None,"
    "'blake_datadir_source': nc.BLAKE_DATADIR_SOURCE,"
    "'env': nc.node_env(),"
    "'problems': nc.node_config_problems(),"
    "'summary': nc.node_config_summary(),"
    "}))"
)

#
# The message node_config emits when it can see a rig state file that
# says "stopped". Kept as a fragment so a rewording does not need a
# rewrite of the whole assertion.
#
STOPPED_MARKER = "rig is stopped"

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


def check(name, condition, detail):
    if condition:
        ok(name)
    else:
        fail(name, detail)


def probe(root, extra_env=None):
    """
    Resolve node_config in a child interpreter.

    ``root`` becomes BLAKE_SWAP_TEST_ROOT; every other BLAKE_SWAP_*
    variable is removed so the child starts from the unconfigured case
    a fresh checkout sees.
    """

    env = {
        key: value
        for key, value in os.environ.items()
        if not key.startswith("BLAKE_SWAP_")
    }

    env["BLAKE_SWAP_TEST_ROOT"] = str(root)
    env.update(extra_env or {})

    completed = subprocess.run(
        [PYTHON, "-c", PROBE],
        cwd=str(ROOT),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=120,
    )

    if completed.returncode != 0:
        raise AssertionError(
            "probe failed:\n" + completed.stdout + completed.stderr
        )

    return json.loads(completed.stdout.strip().splitlines()[-1])


def write_state(root, running, **values):
    root.mkdir(parents=True, exist_ok=True)

    state = {"running": running, "root": str(root)}
    state.update(values)

    (root / "rig-env.json").write_text(json.dumps(state), encoding="utf-8")


def make_rig(root):
    """A fake rig: two datadirs that exist, plus a state file."""

    btc = root / "btc"
    blake = root / "blake"
    btc.mkdir(parents=True, exist_ok=True)
    blake.mkdir(parents=True, exist_ok=True)

    return btc, blake


def check_running_rig_is_discovered(tmp):
    root = tmp / "rig"
    btc, blake = make_rig(root)

    write_state(
        root,
        True,
        btc_datadir=str(btc),
        blake_datadir=str(blake),
        btc_cli=PYTHON,
        blake_cli=PYTHON,
    )

    resolved = probe(root)

    check(
        "a running rig is discovered without any environment variables",
        resolved["btc_datadir"] == str(btc)
        and resolved["blake_datadir"] == str(blake),
        f"resolved {resolved['btc_datadir']} / {resolved['blake_datadir']}",
    )

    check(
        "a running rig resolves both chain CLIs",
        resolved["btc_cli"] == PYTHON and resolved["blake_cli"] == PYTHON,
        f"resolved {resolved['btc_cli']} / {resolved['blake_cli']}",
    )

    check(
        "a running rig reports no configuration problems",
        resolved["problems"] == [],
        f"problems: {resolved['problems']}",
    )

    #
    # The rig datadirs must be exported to children, otherwise the
    # harness would run against whatever the engine defaults to.
    #
    exported = resolved["env"]

    check(
        "a running rig is exported to child processes",
        exported.get("BLAKE_SWAP_BTC_DATADIR") == str(btc)
        and exported.get("BLAKE_SWAP_BLAKE_DATADIR") == str(blake),
        f"exported: {exported}",
    )


def check_stopped_rig_skips(tmp):
    root = tmp / "stopped"
    btc, blake = make_rig(root)

    write_state(
        root,
        False,
        btc_datadir=str(btc),
        blake_datadir=str(blake),
        btc_cli=PYTHON,
        blake_cli=PYTHON,
    )

    resolved = probe(root)

    check(
        "a stopped rig is not treated as a usable target",
        resolved["btc_datadir"] != str(btc)
        and resolved["blake_datadir"] != str(blake),
        f"resolved {resolved['btc_datadir']} / {resolved['blake_datadir']}",
    )

    check(
        "a stopped rig explains itself instead of failing to connect",
        any(STOPPED_MARKER in problem for problem in resolved["problems"]),
        f"problems: {resolved['problems']}",
    )


def check_no_rig_file(tmp):
    root = tmp / "fresh"

    resolved = probe(root)

    check(
        "a checkout with no rig state file is not told its rig is stopped",
        not any(STOPPED_MARKER in problem for problem in resolved["problems"]),
        f"problems: {resolved['problems']}",
    )

    check(
        "a checkout with no rig state file and no CLI to find skips",
        resolved["btc_cli"] is not None
        or any("Bitcoin CLI unresolved" in p for p in resolved["problems"]),
        f"problems: {resolved['problems']}",
    )


def check_env_wins(tmp):
    root = tmp / "override"
    btc, blake = make_rig(root)

    write_state(
        root,
        True,
        btc_datadir=str(btc),
        blake_datadir=str(blake),
        btc_cli=PYTHON,
        blake_cli=PYTHON,
    )

    override = tmp / "explicit-datadir"
    override.mkdir(parents=True, exist_ok=True)

    resolved = probe(
        root,
        {
            "BLAKE_SWAP_BTC_DATADIR": str(override),
            "BLAKE_SWAP_BTC_CLI": PYTHON,
        },
    )

    check(
        "an explicit datadir overrides the rig",
        resolved["btc_datadir"] == str(override)
        and resolved["btc_datadir_source"] == "BLAKE_SWAP_BTC_DATADIR",
        f"resolved {resolved['btc_datadir']} "
        f"[{resolved['btc_datadir_source']}]",
    )

    check(
        "the unoverridden chain still comes from the rig",
        resolved["blake_datadir"] == str(blake),
        f"resolved {resolved['blake_datadir']}",
    )


def check_env_never_renders_none(tmp):
    root = tmp / "empty-env"

    resolved = probe(root)

    #
    # A literal "None" would be passed to the engine as a path, which
    # makes every node-backed suite fail with a confusing RPC error
    # instead of reporting the real problem.
    #
    check(
        "node_env() never exports the literal string None",
        not any(
            value == "None" or "None" in value
            for value in resolved["env"].values()
        ),
        f"exported: {resolved['env']}",
    )

    check(
        "node_env() only exports variables it actually resolved",
        set(resolved["env"]).issubset(
            {
                "BLAKE_SWAP_BTC_CLI",
                "BLAKE_SWAP_BTC_DATADIR",
                "BLAKE_SWAP_BTC_RPC_ARGS",
                "BLAKE_SWAP_BLAKE_CLI",
                "BLAKE_SWAP_BLAKE_DATADIR",
                "BLAKE_SWAP_BLAKE_RPC_ARGS",
            }
        ),
        f"exported: {resolved['env']}",
    )


def check_malformed_state_file(tmp):
    root = tmp / "malformed"
    root.mkdir(parents=True, exist_ok=True)

    (root / "rig-env.json").write_text("{not json", encoding="utf-8")

    resolved = probe(root)

    check(
        "a corrupt rig state file degrades to unconfigured, not a crash",
        resolved["rig_running"] is False
        and not any(STOPPED_MARKER in problem for problem in resolved["problems"]),
        f"problems: {resolved['problems']}",
    )


def main():
    with tempfile.TemporaryDirectory(prefix="blake-swap-node-config-") as tmpdir:
        tmp = Path(tmpdir)

        check_running_rig_is_discovered(tmp)
        check_stopped_rig_skips(tmp)
        check_no_rig_file(tmp)
        check_env_wins(tmp)
        check_env_never_renders_none(tmp)
        check_malformed_state_file(tmp)

    print()
    print("=" * 72)
    print("NODE CONFIG SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")

    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
