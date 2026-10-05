#!/usr/bin/env python3

"""
Run the complete blake-swap Python test matrix.

Goals:

- discover every tests/*_tests.py suite;
- continue after failures;
- preserve complete stdout/stderr per suite;
- distinguish PASS / SKIP / FAIL / TIMEOUT;
- print a concise release-baseline summary;
- return nonzero if anything failed or timed out.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import time
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
TESTS = ROOT / "tests"
LOG_ROOT = ROOT / ".test-matrix-logs"


#
# The matrix runner itself is not a test suite.
#
EXCLUDE = {
    "run_full_matrix.py",
}


def discover():
    return sorted(
        path
        for path in TESTS.glob("*_tests.py")
        if path.name not in EXCLUDE
    )


def resolve_skip_code():
    #
    # Use the repository's own node-config value when available rather
    # than baking an exit code into the runner.
    #
    sys.path.insert(
        0,
        str(TESTS),
    )

    try:
        import node_config
    except Exception:
        return 77

    return int(
        getattr(
            node_config,
            "SKIP_EXIT_CODE",
            77,
        )
    )


def format_elapsed(seconds):
    if seconds < 60:
        return f"{seconds:.1f}s"

    minutes = int(seconds // 60)
    remaining = seconds - (minutes * 60)

    return f"{minutes}m {remaining:.1f}s"


def write_log(
    path,
    *,
    command,
    returncode,
    elapsed,
    stdout,
    stderr,
):
    text = [
        f"command: {' '.join(command)}",
        f"returncode: {returncode}",
        f"elapsed: {elapsed:.3f}s",
        "",
        "===== STDOUT =====",
        stdout or "",
        "",
        "===== STDERR =====",
        stderr or "",
        "",
    ]

    path.write_text(
        "\n".join(text)
    )


def main():
    parser = argparse.ArgumentParser()

    parser.add_argument(
        "--timeout",
        type=int,
        default=900,
        help=(
            "Maximum seconds allowed for one suite "
            "(default: 900)."
        ),
    )

    parser.add_argument(
        "--match",
        default=None,
        help=(
            "Run only suites whose filename contains "
            "this substring."
        ),
    )

    args = parser.parse_args()

    suites = discover()

    if args.match:
        suites = [
            path
            for path in suites
            if args.match in path.name
        ]

    if not suites:
        print("No test suites discovered.")
        return 1

    skip_code = resolve_skip_code()

    stamp = time.strftime(
        "%Y%m%d-%H%M%S"
    )

    log_dir = (
        LOG_ROOT / stamp
    )

    log_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    env = os.environ.copy()

    #
    # A number of unit/model suites import swap_v2 directly rather than
    # launching src/swap_v2.py as a subprocess.  When this matrix runner
    # executes from the repository root, src/ is not otherwise on
    # sys.path.  Export the repository's test import surface explicitly.
    #
    pythonpath = [
        str(ROOT / "src"),
        str(ROOT / "tests"),
    ]

    inherited_pythonpath = env.get("PYTHONPATH")

    if inherited_pythonpath:
        pythonpath.append(inherited_pythonpath)

    env["PYTHONPATH"] = os.pathsep.join(pythonpath)

    #
    # Never allow an operator's current participant session to leak into
    # the release matrix. Individual suites establish their own role,
    # wallet, state, transport and node settings as required.
    #
    for key in (
        "BLAKE_SWAP_STATE_DIR",
        "BLAKE_SWAP_MESSAGE_DIR",
        "BLAKE_SWAP_ROLE",
        "BLAKE_SWAP_CHAIN_WALLET",
        "BLAKE_SWAP_BLAKE_WALLET",
        "BLAKE_SWAP_BTC_WALLET",
        "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
        "BLAKE_SWAP_NOSTR_KEY_FILE",
        "BLAKE_SWAP_NOSTR_KEY_DIR",
        "BLAKE_SWAP_FAILPOINT",
        "BLAKE_SWAP_TEST_CRASH_POINT",
    ):
        env.pop(
            key,
            None,
        )

    results = []

    started = time.time()

    print()
    print("BLAKE-SWAP FULL TEST MATRIX")
    print("=" * 78)
    print("root:      ", ROOT)
    print("python:    ", sys.executable)
    print("suites:    ", len(suites))
    print("skip code: ", skip_code)
    print("logs:      ", log_dir)
    print()

    for number, suite in enumerate(
        suites,
        start=1,
    ):
        command = [
            sys.executable,
            str(suite),
        ]

        print(
            f"[{number:02}/{len(suites):02}] "
            f"{suite.name} ... ",
            end="",
            flush=True,
        )

        suite_started = time.time()

        try:
            p = subprocess.run(
                command,
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=args.timeout,
            )

            elapsed = (
                time.time()
                - suite_started
            )

            if p.returncode == 0:
                status = "PASS"
            elif p.returncode == skip_code:
                status = "SKIP"
            else:
                status = "FAIL"

            log_path = (
                log_dir
                / f"{suite.stem}.log"
            )

            write_log(
                log_path,
                command=command,
                returncode=p.returncode,
                elapsed=elapsed,
                stdout=p.stdout,
                stderr=p.stderr,
            )

            results.append({
                "suite": suite.name,
                "status": status,
                "returncode": p.returncode,
                "elapsed": elapsed,
                "log": log_path,
            })

            print(
                f"{status:4}  "
                f"{format_elapsed(elapsed)}"
            )

            if status == "FAIL":
                text = (
                    (p.stdout or "")
                    + "\n"
                    + (p.stderr or "")
                )

                tail = "\n".join(
                    text.rstrip().splitlines()[
                        -18:
                    ]
                )

                if tail:
                    print()
                    print(tail)
                    print()

        except subprocess.TimeoutExpired as e:
            elapsed = (
                time.time()
                - suite_started
            )

            stdout = (
                e.stdout.decode()
                if isinstance(
                    e.stdout,
                    bytes,
                )
                else (
                    e.stdout
                    or ""
                )
            )

            stderr = (
                e.stderr.decode()
                if isinstance(
                    e.stderr,
                    bytes,
                )
                else (
                    e.stderr
                    or ""
                )
            )

            log_path = (
                log_dir
                / f"{suite.stem}.log"
            )

            write_log(
                log_path,
                command=command,
                returncode="TIMEOUT",
                elapsed=elapsed,
                stdout=stdout,
                stderr=stderr,
            )

            results.append({
                "suite": suite.name,
                "status": "TIMEOUT",
                "returncode": None,
                "elapsed": elapsed,
                "log": log_path,
            })

            print(
                "TIMEOUT  "
                f"{format_elapsed(elapsed)}"
            )

    total_elapsed = (
        time.time()
        - started
    )

    counts = {
        status: sum(
            1
            for result in results
            if result["status"] == status
        )
        for status in (
            "PASS",
            "SKIP",
            "FAIL",
            "TIMEOUT",
        )
    }

    print()
    print("=" * 78)
    print("FULL MATRIX SUMMARY")
    print("=" * 78)

    print(
        f"PASS:     {counts['PASS']}"
    )

    print(
        f"SKIP:     {counts['SKIP']}"
    )

    print(
        f"FAIL:     {counts['FAIL']}"
    )

    print(
        f"TIMEOUT:  {counts['TIMEOUT']}"
    )

    print(
        f"TOTAL:    {len(results)}"
    )

    print(
        "ELAPSED:  "
        + format_elapsed(
            total_elapsed
        )
    )

    print(
        "LOGS:     "
        + str(log_dir)
    )

    bad = [
        result
        for result in results
        if result["status"]
        in {
            "FAIL",
            "TIMEOUT",
        }
    ]

    skipped = [
        result
        for result in results
        if result["status"]
        == "SKIP"
    ]

    if skipped:
        print()
        print("SKIPPED SUITES")
        print("-" * 78)

        for result in skipped:
            print(
                result["suite"]
            )

    if bad:
        print()
        print("FAILED / TIMED OUT")
        print("-" * 78)

        for result in bad:
            print(
                f"{result['status']:7} "
                f"{result['suite']}"
            )

            print(
                "        "
                f"log: {result['log']}"
            )

    else:
        print()
        print(
            "PASS: no suite failed or timed out."
        )

    return 1 if bad else 0


if __name__ == "__main__":
    raise SystemExit(main())
