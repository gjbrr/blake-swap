#!/usr/bin/env bash
#
# Coverage wrapper around scripts/test.sh.
#
# The suites drive the engine as a subprocess, so coverage must be
# collected from child processes as well as the test process itself:
#
#   * COVERAGE_PROCESS_START points every child interpreter at
#     .coveragerc, and the startup hook installed in the venv makes each
#     child record its own parallel data file;
#   * `coverage combine` merges them into one report.
#
# Usage:
#   scripts/coverage.sh                       # everything runnable
#   scripts/coverage.sh --unit-only           # node-free suites only
#   scripts/coverage.sh --with-nostr          # include public-relay suites
#   scripts/coverage.sh --fail-under 80       # non-zero exit below 80%
#   scripts/coverage.sh --html                # also write htmlcov/
#   scripts/coverage.sh --xml                 # also write coverage.xml
#
# Any other argument is passed through to scripts/test.sh, so
# --list, --strict and --verbose all work.
#
# Exit status mirrors scripts/test.sh, or 2 if coverage itself could not
# run, or 1 if --fail-under was not met.

set -uo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

VENV="$PROJECT_DIR/.venv"
PYTHON_BIN="$VENV/bin/python"
COVERAGE="$VENV/bin/coverage"
CONFIG="$PROJECT_DIR/.coveragerc"

if [[ ! -x "$PYTHON_BIN" || ! -x "$COVERAGE" ]]; then
    cat >&2 <<EOF
ERROR: the coverage virtual environment is missing.

Create it with:

  uv venv --python 3.12 "$VENV"
  uv pip install --python "$PYTHON_BIN" -r requirements.txt coverage

Then re-run this script.
EOF
    exit 2
fi

if [[ ! -f "$CONFIG" ]]; then
    echo "ERROR: coverage config not found: $CONFIG" >&2
    exit 2
fi

FAIL_UNDER=0
WANT_HTML=0
WANT_XML=0
TEST_ARGS=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --fail-under)
            FAIL_UNDER="${2:-}"
            if [[ -z "$FAIL_UNDER" || ! "$FAIL_UNDER" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
                echo "ERROR: --fail-under needs a number" >&2
                exit 2
            fi
            shift 2
            ;;
        --html) WANT_HTML=1; shift ;;
        --xml) WANT_XML=1; shift ;;
        --help|-h)
            sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            TEST_ARGS+=("$1")
            shift
            ;;
    esac
done

# Every child interpreter must start recording coverage.
export COVERAGE_PROCESS_START="$CONFIG"
export COVERAGE_FILE="$PROJECT_DIR/.coverage"

cd "$PROJECT_DIR"

echo "============================================================"
echo "BLAKE SWAP COVERAGE"
echo "============================================================"
echo "python:   $PYTHON_BIN"
echo "config:   $CONFIG"
echo

"$COVERAGE" erase -q

echo "== running suites under coverage =="
echo

"$PROJECT_DIR/scripts/test.sh" "${TEST_ARGS[@]}"
TEST_STATUS=$?

echo
echo "== combining coverage data =="

COMBINED=0
for _ in 1 2 3; do
    if "$COVERAGE" combine -q; then
        COMBINED=1
        break
    fi
    sleep 1
done

if [[ "$COMBINED" -ne 1 ]]; then
    echo "ERROR: coverage combine failed" >&2
    exit 2
fi

if [[ ! -f "$PROJECT_DIR/.coverage" ]]; then
    echo "ERROR: no coverage data was collected." >&2
    echo "The suites may have all been skipped (no nodes configured)." >&2
    exit 2
fi

echo
echo "============================================================"
echo "COVERAGE REPORT"
echo "============================================================"
echo

REPORT_STATUS=0
if [[ "$FAIL_UNDER" != "0" ]]; then
    "$COVERAGE" report --fail-under="$FAIL_UNDER" || REPORT_STATUS=$?
else
    "$COVERAGE" report
fi

if [[ "$WANT_HTML" -eq 1 ]]; then
    "$COVERAGE" html -q
    echo
    echo "HTML report: $PROJECT_DIR/htmlcov/index.html"
fi

if [[ "$WANT_XML" -eq 1 ]]; then
    "$COVERAGE" xml -q
    echo "XML report:  $PROJECT_DIR/coverage.xml"
fi

echo
echo "============================================================"
echo "SUMMARY"
echo "============================================================"
echo "suite status:    $TEST_STATUS (0 = all selected suites passed)"
echo "coverage status: $REPORT_STATUS (0 = threshold met)"

if [[ "$TEST_STATUS" -ne 0 ]]; then
    echo
    echo "NOTE: some suites failed or skipped; coverage reflects only what ran."
    exit "$TEST_STATUS"
fi

if [[ "$REPORT_STATUS" -ne 0 ]]; then
    echo
    echo "COVERAGE THRESHOLD NOT MET (--fail-under $FAIL_UNDER)"
    exit 1
fi

exit 0
