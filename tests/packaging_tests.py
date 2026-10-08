#!/usr/bin/env python3
"""
Packaging and CI suite.

The packaging metadata and the CI workflow are the two places a project
describes how it is built and verified, and both are easy to let rot: a new
dependency lands in `requirements.txt` and nowhere else, a new module is not
declared, or the workflow drifts from the commands it claims to run. A YAML
syntax error in the workflow is worse than either, because nothing local
notices until CI silently stops running the suite.

So this suite checks the two against each other and against the repository:

  * `pyproject.toml` parses and declares name, version, readme, licence and a
    Python range;
  * its runtime dependencies are exactly the non-optional pins in
    `requirements.txt`, and its `crypto` extra is exactly the optional one;
  * the modules it declares are exactly the modules in `src/`;
  * `LICENSE` exists and is an MIT licence;
  * the CI workflow parses as YAML, runs on a pinned runner, provisions BOTH
    node binaries with matching version/checksum pairs, verifies the rig
    actually came up, runs the unit suites and the documentation check, gates
    the full suite at the documented threshold, and never makes a merge depend
    on public relays.

PyYAML is required for the workflow check and comes from the `dev` extra.
That is deliberate: a check that skips without its dependency is the same
vacuous pass this suite exists to prevent.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import re
import sys
import tomllib
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]

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


def read_pyproject():
    with (ROOT / "pyproject.toml").open("rb") as handle:
        return tomllib.load(handle)


def requirement_pins():
    """
    The pins in requirements.txt, split into runtime and optional.

    A pin is optional when the comment block immediately above it says so;
    that is how the file already distinguishes `cryptography`.
    """

    runtime = []
    optional = []
    pending_comments = []

    for raw in (ROOT / "requirements.txt").read_text().splitlines():
        line = raw.strip()

        if not line:
            pending_comments = []
            continue

        if line.startswith("#"):
            pending_comments.append(line.lower())
            continue

        mentions_optional = any(
            "optional" in comment for comment in pending_comments
        )

        (optional if mentions_optional else runtime).append(line)
        pending_comments = []

    return runtime, optional


def workflow_text():
    return (ROOT / ".github" / "workflows" / "ci.yml").read_text()


def load_workflow():
    try:
        import yaml
    except ImportError as e:
        raise AssertionError(
            "PyYAML is needed to check the workflow; install the dev extra "
            "with `pip install -e '.[dev]'`"
        ) from e

    return yaml.safe_load(workflow_text())


# ------------------------------------------------------------ metadata


def test_pyproject_declares_the_project():
    project = read_pyproject()["project"]

    for field in (
        "name",
        "version",
        "description",
        "readme",
        "license",
        "requires-python",
        "dependencies",
    ):
        if field not in project:
            raise AssertionError(f"pyproject.toml has no project.{field}")

    if project["readme"] != "README.md":
        raise AssertionError(f"unexpected readme: {project['readme']}")

    if not (ROOT / "README.md").exists():
        raise AssertionError("README.md does not exist")

    licence = project["license"]

    if not isinstance(licence, dict) or licence.get("file") != "LICENSE":
        raise AssertionError(f"unexpected licence declaration: {licence}")

    if not (ROOT / "LICENSE").exists():
        raise AssertionError("LICENSE does not exist")

    #
    # The Python range is not cosmetic: coincurve 21 does not build on 3.14,
    # so claiming an open range would promise an environment that cannot
    # install.
    #
    specifier = project["requires-python"]

    if "3.14" not in specifier and "<" not in specifier:
        raise AssertionError(
            f"requires-python does not exclude the unsupported versions: "
            f"{specifier!r}"
        )


def test_licence_is_mit():
    text = (ROOT / "LICENSE").read_text()

    if not text.startswith("MIT License"):
        raise AssertionError("LICENSE does not start with 'MIT License'")

    if "WITHOUT WARRANTY OF ANY KIND" not in text:
        raise AssertionError("LICENSE is missing the warranty disclaimer")

    if "Permission is hereby granted" not in text:
        raise AssertionError("LICENSE is missing the grant")


def test_dependencies_match_requirements():
    project = read_pyproject()["project"]
    runtime, optional = requirement_pins()

    if sorted(project["dependencies"]) != sorted(runtime):
        raise AssertionError(
            "pyproject dependencies and requirements.txt runtime pins "
            f"disagree:\n  pyproject:   {sorted(project['dependencies'])}\n"
            f"  requirements: {sorted(runtime)}"
        )

    extra = project.get("optional-dependencies", {}).get("crypto")

    if extra is None:
        raise AssertionError("pyproject has no [project.optional-dependencies] crypto")

    if sorted(extra) != sorted(optional):
        raise AssertionError(
            "the crypto extra and the optional requirements pins disagree: "
            f"{sorted(extra)} vs {sorted(optional)}"
        )

    #
    # And the optional dependency must not also be a hard requirement, or
    # a plaintext-only deployment would be forced to install it.
    #
    overlap = set(extra) & set(project["dependencies"])

    if overlap:
        raise AssertionError(
            f"{sorted(overlap)} are both required and optional"
        )


def test_console_scripts_are_declared():
    project = read_pyproject()["project"]

    scripts = project.get("scripts")

    expected = {
        "blake-swap-engine": "swap_v2:main",
        "blake-swap-agent": "swap_agent:main",
    }

    if scripts != expected:
        raise AssertionError(
            "unexpected installed console scripts:\n"
            f"  expected: {expected}\n"
            f"  observed: {scripts}"
        )


def test_readme_python_range_matches_pyproject():
    readme = (ROOT / "README.md").read_text()

    if "Python 3.12–3.13" not in readme:
        raise AssertionError(
            "README does not advertise the supported Python 3.12–3.13 range"
        )

    if "Python 3.10+" in readme:
        raise AssertionError(
            "README still advertises the obsolete Python 3.10+ range"
        )


def test_declared_modules_match_src():
    declared = set(
        read_pyproject()["tool"]["setuptools"]["py-modules"]
    )

    on_disk = {
        path.stem
        for path in (ROOT / "src").glob("*.py")
        if path.name != "__init__.py"
    }

    missing = sorted(on_disk - declared)
    extra = sorted(declared - on_disk)

    problems = []

    if missing:
        problems.append(f"not declared in pyproject.toml: {missing}")

    if extra:
        problems.append(f"declared but not present in src/: {extra}")

    if problems:
        raise AssertionError("\n".join(problems))


# ------------------------------------------------------------ the workflow


def test_the_workflow_parses():
    workflow = load_workflow()

    if workflow is None:
        raise AssertionError("the workflow is empty")

    #
    # `on` is a YAML 1.1 boolean, so a parser may read the key as True. Accept
    # either: what matters is that there is a trigger and a job.
    #
    if "jobs" not in workflow:
        raise AssertionError("the workflow declares no jobs")

    if "test" not in workflow["jobs"]:
        raise AssertionError(
            f"the workflow has no test job: {sorted(workflow['jobs'])}"
        )


def test_the_workflow_pins_the_runner_and_python():
    job = load_workflow()["jobs"]["test"]

    if job.get("runs-on") != "ubuntu-24.04":
        raise AssertionError(
            "the runner is not pinned to ubuntu-24.04, so a runner-image "
            f"change would alter CI silently: {job.get('runs-on')!r}"
        )

    text = workflow_text()

    if 'python-version: "3.12"' not in text:
        raise AssertionError("the workflow does not pin Python 3.12")

    if ":latest" in text:
        raise AssertionError("the workflow uses a :latest tag")


def test_the_workflow_provisions_both_chains():
    text = workflow_text()

    #
    # Two different binaries. Using one for both chains would drop the
    # cross-implementation evidence the rig exists to provide.
    #
    for expected in (
        "bitcoincore.org",
        "bitcoinknots.org",
        "sha256sum -c -",
        "scripts/test-nodes start",
        "scripts/test-nodes stop",
    ):
        if expected not in text:
            raise AssertionError(f"the workflow never mentions {expected!r}")

    core_version = re.search(
        r'CORE_VERSION: "([^"]+)"', text
    )
    knots_version = re.search(
        r'KNOTS_VERSION: "([^"]+)"', text
    )
    core_sha = re.search(
        r'CORE_SHA256: "([0-9a-f]{64})"', text
    )
    knots_sha = re.search(
        r'KNOTS_SHA256: "([0-9a-f]{64})"', text
    )

    if not all((core_version, knots_version, core_sha, knots_sha)):
        raise AssertionError(
            "the workflow does not pin both versions and their checksums"
        )

    if core_version.group(1) == knots_version.group(1):
        raise AssertionError("both chains are pinned to the same version")

    if core_sha.group(1) == knots_sha.group(1):
        raise AssertionError(
            "both binaries are pinned to the same checksum, so one of them "
            "is not being verified"
        )

    #
    # The pins must be what the download step actually uses. The URLs are
    # built from the variables, so check the variables, not the literals:
    # asserting the literal version appears in a URL would only pass if the
    # pin were duplicated, which is the drift this suite is for.
    #
    for variable in ("CORE_VERSION", "KNOTS_VERSION"):
        if f"${{{variable}}}" not in text:
            raise AssertionError(
                f"{variable} is pinned but no URL or checksum line uses it"
            )

    for variable in ("CORE_SHA256", "KNOTS_SHA256"):
        if f"${{{variable}}}" not in text:
            raise AssertionError(
                f"{variable} is pinned but the checksum line does not use it"
            )


def test_the_workflow_proves_the_rig_came_up():
    """
    A rig that never starts must fail the job. The suites SKIP when their
    nodes are missing and the runner exits 0 for a skip, so without an
    explicit check the job would pass having run almost nothing.
    """

    text = workflow_text()

    for expected in (
        "rig-env.json",
        "getblockcount",
        "-ge 101",
        "ulimit -n",
    ):
        if expected not in text:
            raise AssertionError(
                f"the workflow does not verify the rig ({expected!r})"
            )


def test_the_workflow_gates_at_the_documented_threshold():
    text = workflow_text()
    readme = (ROOT / "README.md").read_text()
    tests_readme = (ROOT / "tests" / "README.md").read_text()

    if "--fail-under 80" not in text:
        raise AssertionError("the workflow does not enforce the coverage gate")

    for document, name in ((readme, "README.md"), (tests_readme, "tests/README.md")):
        if "--fail-under 80" not in document and "fail-under 80" not in document:
            raise AssertionError(
                f"{name} does not state the coverage gate the workflow uses"
            )

    #
    # --strict everywhere it runs the suites, so a SKIP is a failure.
    #
    for line in text.splitlines():
        stripped = line.strip()

        if stripped.startswith("scripts/test.sh") and "--strict" not in stripped:
            raise AssertionError(
                f"the workflow runs the suites without --strict: {stripped}"
            )

        if stripped.startswith("scripts/coverage.sh") and "--strict" not in stripped:
            raise AssertionError(
                f"the workflow gates coverage without --strict: {stripped}"
            )

    if "docs_alignment_tests.py" not in text:
        raise AssertionError(
            "the workflow does not run the documentation check"
        )


def test_the_workflow_installs_every_extra_the_suites_need():
    """
    A unit suite that cannot run in the environment CI builds.

    `tests/nip44_tests.py` exercises the cipher whenever `cryptography` is
    installed and exits 77 when it is not; `--strict` then makes that skip a
    failure. So a workflow that installs only the `dev` extra either fails
    the unit gate outright, or, without `--strict`, reports a unit gate that
    silently stopped covering the encrypted coordination path. The extras it
    installs must therefore include the optional one the suites use.
    """

    text = workflow_text()

    installs = [
        line.strip()
        for line in text.splitlines()
        if "pip install" in line and not line.strip().startswith("#")
    ]

    if not any("crypto" in line for line in installs):
        raise AssertionError(
            "the workflow never installs the crypto extra, so the NIP-44 "
            f"cipher is not covered by the unit gate: {installs}"
        )


def test_the_workflow_never_depends_on_public_relays():
    text = workflow_text()

    #
    # Mentioning the flag in a comment is fine; invoking it is not. Check
    # the commands, not the text.
    #
    for line in text.splitlines():
        stripped = line.strip()

        if stripped.startswith("#"):
            continue

        if "--with-nostr" in stripped:
            raise AssertionError(
                "the workflow runs the public-relay suites, so a merge "
                f"would depend on a third party being up: {stripped}"
            )

    if "htmlcov" not in text:
        raise AssertionError(
            "the workflow does not upload the coverage report"
        )


def main():
    print()
    print("=" * 72)
    print("PACKAGING AND CI")
    print("=" * 72)
    print()

    check("pyproject.toml declares the project", test_pyproject_declares_the_project)
    check("LICENSE is an MIT licence", test_licence_is_mit)
    check("dependencies match requirements.txt", test_dependencies_match_requirements)
    check("installed console scripts are declared", test_console_scripts_are_declared)
    check("README Python range matches pyproject", test_readme_python_range_matches_pyproject)
    check("the declared modules match src/", test_declared_modules_match_src)

    check("the CI workflow parses", test_the_workflow_parses)
    check("the workflow pins the runner and Python", test_the_workflow_pins_the_runner_and_python)
    check("the workflow provisions both chains", test_the_workflow_provisions_both_chains)
    check("the workflow proves the rig came up", test_the_workflow_proves_the_rig_came_up)
    check("the workflow gates at the documented threshold", test_the_workflow_gates_at_the_documented_threshold)
    check("the workflow installs every extra the suites need", test_the_workflow_installs_every_extra_the_suites_need)
    check("the workflow never depends on public relays", test_the_workflow_never_depends_on_public_relays)

    print()
    print("=" * 72)
    print("PACKAGING AND CI SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
