#!/usr/bin/env python3

"""
Clean-wheel installation regression test.

This suite proves that the Python reference implementation is actually
distributable rather than merely runnable from a source checkout.

It:

  * builds a wheel from the repository;
  * creates a completely separate virtual environment;
  * installs that wheel normally, including declared dependencies;
  * runs from outside the repository;
  * removes PYTHONPATH;
  * exercises both installed console entry points;
  * runs a node-free engine command;
  * verifies core modules resolve from the clean venv's site-packages,
    never from the repository checkout.

No Bitcoin/BLAKE nodes or Nostr relays are required.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]


PASS = 0
FAIL = 0


def check(name, fn):
    global PASS, FAIL

    try:
        fn()
    except Exception as exc:
        FAIL += 1
        print(f"FAIL  {name}")
        print(f"      {exc}")
    else:
        PASS += 1
        print(f"PASS  {name}")


def run(
    argv,
    *,
    cwd=None,
    env=None,
    expect=0,
):
    proc = subprocess.run(
        [str(x) for x in argv],
        cwd=cwd,
        env=env,
        text=True,
        capture_output=True,
    )

    if proc.returncode != expect:
        raise AssertionError(
            f"command returned {proc.returncode}, expected {expect}\n"
            f"command: {' '.join(str(x) for x in argv)}\n"
            f"cwd: {cwd}\n"
            f"stdout:\n{proc.stdout[-4000:]}\n"
            f"stderr:\n{proc.stderr[-4000:]}"
        )

    return proc


class CleanInstall:
    def __init__(self):
        self.tmp = None
        self.root = None
        self.dist = None
        self.venv = None
        self.home = None
        self.python = None
        self.engine = None
        self.agent = None
        self.wheel = None

    def __enter__(self):
        self.tmp = tempfile.TemporaryDirectory(
            prefix="blake-swap-wheel-test-"
        )

        self.root = Path(self.tmp.name)
        self.dist = self.root / "dist"
        self.venv = self.root / "venv"
        self.home = self.root / "home"

        self.dist.mkdir()
        self.home.mkdir()

        run(
            (
                sys.executable,
                "-m",
                "pip",
                "wheel",
                "--disable-pip-version-check",
                "--no-cache-dir",
                "--no-deps",
                "--wheel-dir",
                self.dist,
                ROOT,
            ),
            cwd=self.root,
            env=self.env(),
        )

        wheels = list(
            self.dist.glob(
                "blake_swap-*.whl"
            )
        )

        if len(wheels) != 1:
            raise AssertionError(
                "expected exactly one blake-swap wheel; "
                f"observed {wheels}"
            )

        self.wheel = wheels[0]

        run(
            (
                sys.executable,
                "-m",
                "venv",
                self.venv,
            ),
            cwd=self.root,
        )

        self.python = (
            self.venv
            / "bin"
            / "python"
        )

        pip = (
            self.venv
            / "bin"
            / "pip"
        )

        self.engine = (
            self.venv
            / "bin"
            / "blake-swap-engine"
        )

        self.agent = (
            self.venv
            / "bin"
            / "blake-swap-agent"
        )

        run(
            (
                pip,
                "install",
                "--disable-pip-version-check",
                "--no-cache-dir",
                self.wheel,
            ),
            cwd=self.root,
            env=self.env(),
        )

        return self

    def env(self):
        result = os.environ.copy()

        result.pop(
            "PYTHONPATH",
            None,
        )

        #
        # Do not allow the source checkout's node/state settings to make
        # this test accidentally depend on Gary's machine, the regtest rig,
        # or a CI runner's integration-test environment.
        #
        for key in list(result):
            if (
                key.startswith("BLAKE_SWAP_")
                or key == "SWAP_V2"
                or key == "SWAP_AGENT"
            ):
                result.pop(
                    key,
                    None,
                )

        result["HOME"] = str(
            self.home
        )

        result[
            "BLAKE_SWAP_STATE_DIR"
        ] = str(
            self.home
            / "state"
        )

        return result

    def __exit__(
        self,
        exc_type,
        exc,
        tb,
    ):
        self.tmp.cleanup()


def test_wheel_install():
    with CleanInstall() as clean:
        if not clean.wheel.is_file():
            raise AssertionError(
                "wheel does not exist"
            )

        proc = run(
            (
                clean.python,
                "-m",
                "pip",
                "check",
            ),
            cwd=clean.root,
            env=clean.env(),
        )

        if (
            "No broken requirements found."
            not in proc.stdout
        ):
            raise AssertionError(
                "pip check did not report a healthy install:\n"
                + proc.stdout
            )


def test_console_entry_points():
    with CleanInstall() as clean:
        for command in (
            clean.engine,
            clean.agent,
        ):
            if not command.is_file():
                raise AssertionError(
                    f"installed command missing: {command}"
                )

            run(
                (
                    command,
                    "--help",
                ),
                cwd=clean.root,
                env=clean.env(),
            )


def test_node_free_installed_engine():
    with CleanInstall() as clean:
        proc = run(
            (
                clean.engine,
                "list",
            ),
            cwd=clean.root,
            env=clean.env(),
        )

        if "No swaps." not in proc.stdout:
            raise AssertionError(
                "fresh installed engine did not report an empty swap set:\n"
                + proc.stdout
            )

        state = (
            clean.home
            / "state"
        )

        expected_dirs = {
            state,
            state / "messages",
            state / "nostr-event-cache",
        }

        missing = {
            path
            for path in expected_dirs
            if not path.is_dir()
        }

        if missing:
            raise AssertionError(
                "installed engine did not initialize its state tree: "
                f"{sorted(str(x) for x in missing)}"
            )


def test_imports_are_checkout_independent():
    with CleanInstall() as clean:
        probe = r'''
import json
from pathlib import Path
import swap_v2
import swap_agent
import swap_wire
import fee_policy
import swap_timeout_policy

repo = Path(__REPO__).resolve()

modules = (
    swap_v2,
    swap_agent,
    swap_wire,
    fee_policy,
    swap_timeout_policy,
)

paths = {}

for module in modules:
    path = Path(module.__file__).resolve()

    if repo == path or repo in path.parents:
        raise SystemExit(
            f"{module.__name__} imported from source checkout: {path}"
        )

    paths[module.__name__] = str(path)

print(json.dumps(paths, sort_keys=True))
'''.replace(
            "__REPO__",
            repr(str(ROOT)),
        )

        proc = run(
            (
                clean.python,
                "-c",
                probe,
            ),
            cwd=clean.root,
            env=clean.env(),
        )

        paths = json.loads(
            proc.stdout
        )

        for name, raw_path in paths.items():
            path = Path(
                raw_path
            ).resolve()

            #
            # Stronger than merely "not from the checkout": the module
            # must actually live under this disposable venv.
            #
            if (
                clean.venv.resolve()
                not in path.parents
            ):
                raise AssertionError(
                    f"{name} did not resolve from clean venv: {path}"
                )


def main():
    print()
    print("=" * 72)
    print("INSTALLED WHEEL")
    print("=" * 72)
    print()

    check(
        "wheel installs with healthy dependencies",
        test_wheel_install,
    )

    check(
        "installed console entry points execute",
        test_console_entry_points,
    )

    check(
        "installed engine runs node-free outside checkout",
        test_node_free_installed_engine,
    )

    check(
        "installed imports are checkout-independent",
        test_imports_are_checkout_independent,
    )

    print()
    print("=" * 72)
    print("INSTALLED WHEEL SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
