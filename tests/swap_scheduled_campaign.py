#!/usr/bin/env python3

"""
Coverage campaign for scheduled adversarial profiles.

Supports:
    scheduled-v1
    scheduled-v2

Cheap phase:
    Scan integer seeds without touching either chain and find the first
    seed covering every effective schedule shape.

Execution phase:
    Replay that minimal coverage corpus through the seeded adversarial
    runner.

Differential phase:
    Replay the exact same corpus through direct Python and the JSON
    subprocess boundary and require identical portable fingerprints.
"""

import argparse
import itertools
import random
import re
import subprocess
import sys
import time

import swap_seeded_adversarial_tests as seeded


REPLAY_RE = re.compile(
    r"^(\d+)\s+(claim|refund)\s+([0-9a-f]{16})$",
    re.MULTILINE,
)


EXPECTED_COUNTS = {
    "scheduled-v1": 22,
    "scheduled-v2": 94,
    "scheduled-v3": 52,
}


def path_for_seed(seed):
    return (
        "claim"
        if random.Random(seed).randrange(2) == 0
        else "refund"
    )


# ============================================================
# V1
# ============================================================

def normalize_v1(
    path,
    primary,
    reorg_probe,
    secondary,
):
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


def expected_shapes_v1():
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
            normalize_v1(
                path,
                primary,
                reorg_probe,
                secondary,
            )
        )

    return shapes


def schedule_key_v1(seed):
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


# ============================================================
# V2
# ============================================================

def normalize_v2(
    path,
    primary,
    reorg_probe,
    secondary,
    funding_reconcile,
    settlement_reconcile,
):
    #
    # scheduled-v2 refuses to produce a completely disturbance-free
    # sequence.
    #
    if (
        primary == "normal"
        and not reorg_probe
        and secondary == "normal"
        and funding_reconcile == "none"
        and settlement_reconcile == "none"
    ):
        settlement_reconcile = "both"

    return (
        path,
        primary,
        bool(reorg_probe),
        secondary,
        funding_reconcile,
        settlement_reconcile,
    )


def expected_shapes_v2():
    """
    Nominal space:

        2 paths
        × 3 primary
        × 2 reorg
        × 2 secondary
        × 2 funding reconcile
        × 2 settlement reconcile
        = 96

    One completely disturbance-free cell per path is normalized into an
    already-existing settlement-reconcile cell, leaving 94 effective
    shapes.
    """

    shapes = set()

    for (
        path,
        primary,
        reorg_probe,
        secondary,
        funding_reconcile,
        settlement_reconcile,
    ) in itertools.product(
        ("claim", "refund"),
        ("normal", "crash", "race"),
        (False, True),
        ("normal", "race"),
        ("none", "both"),
        ("none", "both"),
    ):
        shapes.add(
            normalize_v2(
                path,
                primary,
                reorg_probe,
                secondary,
                funding_reconcile,
                settlement_reconcile,
            )
        )

    return shapes


def schedule_key_v2(seed):
    path = path_for_seed(seed)

    schedule = seeded.build_fault_schedule_v2(
        seed,
        path,
    )

    return (
        path,
        schedule["primary_settlement"],
        bool(schedule["reorg_probe"]),
        schedule["secondary_settlement"],
        schedule["funding_reconcile"],
        schedule["settlement_reconcile"],
    )


# ============================================================
# V3
# ============================================================

def normalize_v3(
    path,
    blake_funding_delivery,
    btc_funding_delivery,
    primary_delivery,
):
    #
    # scheduled-v3 never emits a completely undisturbed transport
    # sequence. The all-normal cell is normalized to primary replay.
    #
    if (
        blake_funding_delivery == "normal"
        and btc_funding_delivery == "normal"
        and primary_delivery == "normal"
    ):
        primary_delivery = "replay"

    return (
        path,
        blake_funding_delivery,
        btc_funding_delivery,
        primary_delivery,
    )


def expected_shapes_v3():
    """
    Nominal space:

        2 paths
        × 3 BLAKE funding delivery modes
        × 3 BTC funding delivery modes
        × 3 primary settlement delivery modes
        = 54

    One all-normal cell per path is normalized into an existing replay
    cell, leaving 52 effective shapes.
    """

    shapes = set()

    for (
        path,
        blake_funding_delivery,
        btc_funding_delivery,
        primary_delivery,
    ) in itertools.product(
        ("claim", "refund"),
        ("normal", "replay", "omit-sync"),
        ("normal", "replay", "omit-sync"),
        ("normal", "replay", "omit-sync"),
    ):
        shapes.add(
            normalize_v3(
                path,
                blake_funding_delivery,
                btc_funding_delivery,
                primary_delivery,
            )
        )

    return shapes


def schedule_key_v3(seed):
    path = path_for_seed(seed)

    schedule = seeded.build_fault_schedule_v3(
        seed,
        path,
    )

    return (
        path,
        schedule["blake_funding_delivery"],
        schedule["btc_funding_delivery"],
        schedule["primary_delivery"],
    )


# ============================================================
# Generic profile dispatch
# ============================================================

def expected_shapes(profile):
    if profile == "scheduled-v1":
        return expected_shapes_v1()

    if profile == "scheduled-v2":
        return expected_shapes_v2()

    if profile == "scheduled-v3":
        return expected_shapes_v3()

    raise ValueError(
        f"unsupported scheduled profile: {profile}"
    )


def schedule_key(profile, seed):
    if profile == "scheduled-v1":
        return schedule_key_v1(seed)

    if profile == "scheduled-v2":
        return schedule_key_v2(seed)

    if profile == "scheduled-v3":
        return schedule_key_v3(seed)

    raise ValueError(
        f"unsupported scheduled profile: {profile}"
    )


def discover_coverage(
    profile,
    scan_limit,
):
    target = expected_shapes(profile)
    found = {}

    for seed in range(scan_limit):
        key = schedule_key(
            profile,
            seed,
        )

        if key in target:
            found.setdefault(
                key,
                seed,
            )

        if len(found) == len(target):
            break

    missing = target - set(found)

    return target, found, missing


def format_shape(
    profile,
    shape,
):
    if profile == "scheduled-v1":
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

    if profile == "scheduled-v2":
        (
            path,
            primary,
            reorg_probe,
            secondary,
            funding_reconcile,
            settlement_reconcile,
        ) = shape

        return (
            f"{path:<6}  "
            f"primary={primary:<6}  "
            f"reorg={'yes' if reorg_probe else 'no ':<3}  "
            f"secondary={secondary:<6}  "
            f"funding-sync={funding_reconcile:<4}  "
            f"settlement-sync={settlement_reconcile:<4}"
        )

    (
        path,
        blake_funding_delivery,
        btc_funding_delivery,
        primary_delivery,
    ) = shape

    return (
        f"{path:<6}  "
        f"blake-funding={blake_funding_delivery:<9}  "
        f"btc-funding={btc_funding_delivery:<9}  "
        f"primary={primary_delivery:<9}"
    )


def print_coverage(
    profile,
    target,
    found,
    missing,
):
    title = (
        profile.upper()
        + " COVERAGE"
    )

    print()
    print(title)
    print("=" * len(title))

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
        key=lambda item: item[0],
    )

    for shape, seed in ordered:
        print(
            f"seed {seed:<8}  "
            + format_shape(
                profile,
                shape,
            )
        )

    if missing:
        print()
        print("MISSING")
        print("=======")

        for shape in sorted(missing):
            print(
                format_shape(
                    profile,
                    shape,
                )
            )


def coverage_seeds(found):
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
    profile,
    seeds,
    adapter,
):
    command = [
        sys.executable,
        "tests/swap_seeded_adversarial_tests.py",
        "--adapter",
        adapter,
        "--fault-profile",
        profile,
        *[
            str(seed)
            for seed in seeds
        ],
    ]

    print()
    print("EXECUTION")
    print("=========")
    print(
        "profile:",
        profile,
    )
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
            f"profile: {profile}"
        )
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
        f"{len(corpus)} {profile} seeds "
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
    profile,
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
            f"{profile} mismatch(es)"
        )

    print()
    print(
        f"PASS  all {profile} coverage seeds "
        "match across adapter boundary"
    )


def main():
    parser = argparse.ArgumentParser()

    parser.add_argument(
        "--profile",
        choices=(
            "scheduled-v1",
            "scheduled-v2",
            "scheduled-v3",
        ),
        default="scheduled-v1",
        help=(
            "Scheduled adversarial profile whose state space "
            "should be covered."
        ),
    )

    parser.add_argument(
        "--scan-limit",
        type=int,
        default=1_000_000,
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
        args.profile,
        args.scan_limit,
    )

    print_coverage(
        args.profile,
        target,
        found,
        missing,
    )

    expected_count = EXPECTED_COUNTS[
        args.profile
    ]

    if len(target) != expected_count:
        raise RuntimeError(
            f"{args.profile} effective state-space changed: "
            f"expected {expected_count} shapes, "
            f"observed {len(target)}"
        )

    if missing:
        raise RuntimeError(
            f"could not cover every {args.profile} shape "
            f"within seeds 0..{args.scan_limit - 1}"
        )

    seeds = coverage_seeds(
        found
    )

    print()
    print(
        f"PASS  full {args.profile} shape coverage "
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
            args.profile,
            seeds,
            "direct",
        )

        subprocess_corpus = run_campaign(
            args.profile,
            seeds,
            "json-subprocess",
        )

        compare_corpora(
            args.profile,
            direct,
            subprocess_corpus,
        )

    elif args.execute:
        run_campaign(
            args.profile,
            seeds,
            args.adapter,
        )

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
