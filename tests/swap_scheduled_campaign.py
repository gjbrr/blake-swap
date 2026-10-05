#!/usr/bin/env python3

"""
Coverage campaign for scheduled-v1 adversarial schedules.

Cheap phase:
    Scan integer seeds without touching either chain and find the first
    seed covering every effective scheduled-v1 schedule shape.

Execution phase:
    Replay that minimal coverage corpus through the seeded adversarial
    runner.

Differential phase:
    Replay the exact same corpus through direct Python and the JSON
    subprocess boundary and require identical portable fingerprints.
"""

import argparse
import itertools
import re
import subprocess
import sys
import time

import swap_seeded_adversarial_tests as seeded


REPLAY_RE = re.compile(
    r"^(\d+)\s+(claim|refund)\s+([0-9a-f]{16})$",
    re.MULTILINE,
)


def path_for_seed(seed):
    #
    # Preserve the established path-selection oracle exactly.
    #
    import random

    return (
        "claim"
        if random.Random(seed).randrange(2) == 0
        else "refund"
    )


def normalize_schedule(
    path,
    primary,
    reorg_probe,
    secondary,
):
    #
    # scheduled-v1 refuses to generate a completely fault-free plan.
    #
    if (
        primary == "normal"
        and not reorg_probe
        and secondary == "normal"
    ):
        secondary = "race"

    return (
        path,
        primary,
        bool(reorg_probe),
        secondary,
    )


def schedule_key(seed):
    path = path_for_seed(seed)

    schedule = seeded.build_fault_schedule(
        seed,
        path,
    )

    return (
        path,
        schedule["primary_settlement"],
        bool(schedule["reorg_probe"]),
        schedule["secondary_settlement"],
    )


def expected_shapes():
    """
    Enumerate the effective scheduled-v1 state space independently of
    the SHA-256 seed chooser.

    Nominally:
        2 paths × 3 primary × 2 reorg × 2 secondary = 24

    But each path maps:
        normal / no-reorg / normal
    to:
        normal / no-reorg / race

    Therefore the effective space is 22 unique schedule shapes.
    """

    shapes = set()

    for (
        path,
        primary,
        reorg_probe,
        secondary,
    ) in itertools.product(
        ("claim", "refund"),
        ("normal", "crash", "race"),
        (False, True),
        ("normal", "race"),
    ):
        shapes.add(
            normalize_schedule(
                path,
                primary,
                reorg_probe,
                secondary,
            )
        )

    return shapes


def discover_coverage(scan_limit):
    target = expected_shapes()
    found = {}

    for seed in range(scan_limit):
        key = schedule_key(seed)

        if key in target:
            found.setdefault(
                key,
                seed,
            )

        if len(found) == len(target):
            break

    missing = target - set(found)

    return target, found, missing


def format_shape(shape):
    (
        path,
        primary,
        reorg_probe,
        secondary,
    ) = shape

    return (
        f"{path:<6}  "
        f"primary={primary:<6}  "
        f"reorg={'yes' if reorg_probe else 'no ':<3}  "
        f"secondary={secondary:<6}"
    )


def print_coverage(
    target,
    found,
    missing,
):
    print()
    print("SCHEDULED-V1 COVERAGE")
    print("=====================")
    print(
        f"effective shapes: {len(target)}"
    )
    print(
        f"covered shapes:   {len(found)}"
    )
    print(
        f"missing shapes:   {len(missing)}"
    )
    print()

    ordered = sorted(
        found.items(),
        key=lambda item: (
            item[0][0],
            item[0][1],
            item[0][2],
            item[0][3],
        ),
    )

    for shape, seed in ordered:
        print(
            f"seed {seed:<8}  "
            + format_shape(shape)
        )

    if missing:
        print()
        print("MISSING")
        print("=======")

        for shape in sorted(missing):
            print(format_shape(shape))


def coverage_seeds(found):
    #
    # Sort numerically so execution order itself is deterministic.
    #
    return sorted(
        set(found.values())
    )


def parse_replay_corpus(output):
    result = {}

    for (
        seed_text,
        path,
        digest,
    ) in REPLAY_RE.findall(output):
        seed = int(seed_text)

        result[seed] = (
            path,
            digest,
        )

    return result


def run_campaign(
    seeds,
    adapter,
):
    command = [
        sys.executable,
        "tests/swap_seeded_adversarial_tests.py",
        "--adapter",
        adapter,
        "--fault-profile",
        "scheduled-v1",
        *[
            str(seed)
            for seed in seeds
        ],
    ]

    print()
    print("EXECUTION")
    print("=========")
    print(
        "adapter:",
        adapter,
    )
    print(
        "seeds:  ",
        " ".join(
            str(seed)
            for seed in seeds
        ),
    )

    started = time.time()

    proc = subprocess.run(
        command,
        text=True,
        capture_output=True,
    )

    elapsed = time.time() - started

    if proc.returncode != 0:
        print()
        print("CAMPAIGN FAILED")
        print("===============")
        print(
            f"adapter: {adapter}"
        )
        print(
            f"returncode: {proc.returncode}"
        )
        print()
        print(proc.stdout)

        if proc.stderr:
            print()
            print("STDERR")
            print("======")
            print(proc.stderr)

        raise SystemExit(
            proc.returncode
        )

    corpus = parse_replay_corpus(
        proc.stdout
    )

    if len(corpus) != len(seeds):
        print(proc.stdout)

        raise RuntimeError(
            "campaign passed but replay corpus "
            f"contained {len(corpus)} seeds; "
            f"expected {len(seeds)}"
        )

    print(
        f"PASS  {adapter}: "
        f"{len(corpus)} scheduled seeds "
        f"in {elapsed:.1f}s"
    )

    print()
    print("REPLAY CORPUS")
    print("=============")

    for seed in seeds:
        path, digest = corpus[seed]

        print(
            seed,
            path,
            digest,
        )

    return corpus


def compare_corpora(
    direct,
    subprocess_corpus,
):
    seeds = sorted(
        set(direct)
        | set(subprocess_corpus)
    )

    failures = []

    print()
    print("DIFFERENTIAL COMPARISON")
    print("=======================")

    for seed in seeds:
        a = direct.get(seed)
        b = subprocess_corpus.get(seed)

        if a == b:
            path, digest = a

            print(
                f"PASS  {seed:<8} "
                f"{path:<6} {digest}"
            )
        else:
            failures.append(
                (
                    seed,
                    a,
                    b,
                )
            )

            print(
                f"FAIL  {seed:<8} "
                f"direct={a!r} "
                f"json-subprocess={b!r}"
            )

    if failures:
        raise RuntimeError(
            f"{len(failures)} differential "
            "scheduled-v1 mismatch(es)"
        )

    print()
    print(
        "PASS  all scheduled-v1 coverage seeds "
        "match across adapter boundary"
    )


def main():
    parser = argparse.ArgumentParser()

    parser.add_argument(
        "--scan-limit",
        type=int,
        default=100000,
        help=(
            "Maximum integer seed (exclusive) to scan "
            "while looking for full schedule coverage."
        ),
    )

    parser.add_argument(
        "--execute",
        action="store_true",
        help=(
            "Execute the minimal coverage corpus after "
            "discovering it."
        ),
    )

    parser.add_argument(
        "--adapter",
        choices=(
            "direct",
            "json-subprocess",
        ),
        default="direct",
        help=(
            "Adapter used with --execute."
        ),
    )

    parser.add_argument(
        "--compare-adapters",
        action="store_true",
        help=(
            "Execute the coverage corpus through direct and "
            "json-subprocess implementations and require "
            "identical portable fingerprints."
        ),
    )

    args = parser.parse_args()

    target, found, missing = discover_coverage(
        args.scan_limit
    )

    print_coverage(
        target,
        found,
        missing,
    )

    if len(target) != 22:
        raise RuntimeError(
            "scheduled-v1 effective state-space changed: "
            f"expected 22 shapes, observed {len(target)}"
        )

    if missing:
        raise RuntimeError(
            "could not cover every scheduled-v1 shape "
            f"within seeds 0..{args.scan_limit - 1}"
        )

    seeds = coverage_seeds(
        found
    )

    print()
    print(
        "PASS  full scheduled-v1 shape coverage "
        f"with {len(seeds)} seed(s)"
    )

    print(
        "coverage seeds:",
        " ".join(
            str(seed)
            for seed in seeds
        ),
    )

    if args.compare_adapters:
        direct = run_campaign(
            seeds,
            "direct",
        )

        subprocess_corpus = run_campaign(
            seeds,
            "json-subprocess",
        )

        compare_corpora(
            direct,
            subprocess_corpus,
        )

    elif args.execute:
        run_campaign(
            seeds,
            args.adapter,
        )

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
