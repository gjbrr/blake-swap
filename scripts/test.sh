#!/usr/bin/env bash
#
# BLAKE Swap test runner.
#
# Covers every suite in tests/*_tests.py. Each suite declares its
# prerequisites by class:
#
#   unit     no chain nodes, no relays
#   regtest  needs reachable Bitcoin + BLAKE regtest nodes
#   nostr    needs regtest nodes, real relays, and two participant keys
#
# A suite whose prerequisites are absent exits 77 and is reported as
# SKIP with its reason. A suite that runs and fails is reported as FAIL.
# The runner exits non-zero only if something FAILED (or if --strict is
# given and something was skipped).
#
# Usage:
#   scripts/test.sh                 run everything, skipping unavailable
#   scripts/test.sh --unit-only     run only the node-free suites
#   scripts/test.sh --with-nostr    also run the public-relay suites
#   scripts/test.sh --strict        treat SKIP as failure
#   scripts/test.sh --verbose       stream all suite output
#   scripts/test.sh --list          list suites and prerequisites
#   scripts/test.sh --help
#
# Node selection: set BLAKE_SWAP_BTC_CLI / BLAKE_SWAP_BTC_DATADIR and
# BLAKE_SWAP_BLAKE_CLI / BLAKE_SWAP_BLAKE_DATADIR (plus optional
# BLAKE_SWAP_*_RPC_ARGS). See tests/README.md. These are deliberately
# NOT unset here: the integration harness resolves its nodes from them.

set -uo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

#
# Only participant/session state is cleared. Node configuration is
# preserved, because the integration harness resolves its nodes from it.
#
unset BLAKE_SWAP_ROLE || true
unset BLAKE_SWAP_STATE_DIR || true
unset BLAKE_SWAP_MESSAGE_DIR || true
unset BLAKE_SWAP_CHAIN_WALLET || true
unset BLAKE_SWAP_PARTICIPANT_KEY_FILE || true
unset BLAKE_SWAP_NOSTR_KEY_FILE || true
unset BLAKE_SWAP_NOSTR_KEY_DIR || true
unset BLAKE_SWAP_TRANSPORT || true

export SWAP_V2="$PROJECT_DIR/src/swap_v2.py"
export SWAP_AGENT="$PROJECT_DIR/src/swap_agent.py"
export PYTHONPATH="$PROJECT_DIR/src:$PROJECT_DIR/tests${PYTHONPATH:+:$PYTHONPATH}"

#
# --- suite manifest -------------------------------------------------
#
# Every file matching tests/*_tests.py must appear exactly once below.
# verify_manifest enforces that, so a new suite cannot be silently
# omitted from the runner.
#
UNIT_SUITES=(
    tests/fee_policy_tests.py
    tests/swap_timeout_policy_tests.py
    tests/swap_model_timeout_invariant_tests.py
    tests/dynamic_settlement_builder_tests.py
    tests/btc_dynamic_settlement_tests.py
    tests/swap_dynamic_fee_integration_tests.py
    tests/swap_wire_tests.py
    tests/market_state_tests.py
    tests/docs_alignment_tests.py
)

REGTEST_SUITES=(
    tests/swap_protocol_tests.py
    tests/swap_timeout_negotiation_tests.py
    tests/swap_settlement_recovery_tests.py
    tests/swap_peer_recovery_tests.py
    tests/swap_chain_autonomy_tests.py
    tests/swap_bounded_scan_tests.py
    tests/swap_reorg_safety_tests.py
    tests/swap_autonomous_agents_tests.py
    tests/pre_btc_refund_tests.py
    tests/production_funding_pending_tests.py
    tests/production_settlement_pending_tests.py
    tests/swap_wire_compat_tests.py
    tests/swap_cross_impl_offer_tests.py
    tests/swap_cross_impl_accept_tests.py
    tests/swap_cross_impl_blake_funded_tests.py
    tests/swap_cross_impl_btc_funded_tests.py
    tests/swap_cross_impl_btc_claimed_tests.py
    tests/swap_cross_impl_blake_claimed_tests.py
    tests/swap_cross_impl_full_happy_tests.py
    tests/swap_participant_isolation_tests.py
)

NOSTR_SUITES=(
    tests/swap_isolated_nostr_handshake_tests.py
    tests/swap_isolated_full_chain_swap_tests.py
)

ALL_SUITES=(
    "${UNIT_SUITES[@]}"
    "${REGTEST_SUITES[@]}"
    "${NOSTR_SUITES[@]}"
)

# Only swap_protocol_tests.py takes a suite selector.
suite_args() {
    case "$1" in
        tests/swap_protocol_tests.py) printf 'all' ;;
        *) printf '' ;;
    esac
}

verify_manifest() {
    local discovered
    local listed

    discovered="$(cd "$PROJECT_DIR" && ls tests/*_tests.py 2>/dev/null | sort)"

    listed="$(
        printf '%s\n' "${ALL_SUITES[@]}" \
            | sed 's|^\./||' \
            | sort
    )"

    if [[ "$discovered" != "$listed" ]]; then
        echo "ERROR: the suite manifest in scripts/test.sh is out of date." >&2
        echo >&2
        echo "Suites on disk but not in the manifest:" >&2
        comm -23 <(printf '%s\n' "$discovered") <(printf '%s\n' "$listed") >&2
        echo >&2
        echo "Manifest entries with no file on disk:" >&2
        comm -13 <(printf '%s\n' "$discovered") <(printf '%s\n' "$listed") >&2
        echo >&2
        return 1
    fi

    return 0
}

usage() {
    cat <<'EOF'
BLAKE SWAP TEST RUNNER

Usage:
  scripts/test.sh [options]

Options:
  --unit-only     run only the node-free unit suites
  --with-nostr    also run the public-relay isolated suites
  --strict        treat a skipped suite as a failure
  --verbose       stream all suite output instead of summarising
  --list          list every suite with its prerequisite class
  --help          show this help

Exit status:
  0   everything that ran passed
  1   at least one suite failed
  1   a suite was skipped and --strict was given
  2   the runner or the suite manifest is misconfigured

Prerequisites:
  unit suites need nothing.
  regtest suites need reachable Bitcoin and BLAKE regtest nodes.
  nostr suites need those nodes plus real relays and two participant
  keys, so they are opt-in via --with-nostr.

Node selection (see tests/README.md):
  BLAKE_SWAP_BTC_CLI, BLAKE_SWAP_BTC_DATADIR, BLAKE_SWAP_BTC_RPC_ARGS
  BLAKE_SWAP_BLAKE_CLI, BLAKE_SWAP_BLAKE_DATADIR, BLAKE_SWAP_BLAKE_RPC_ARGS

The runner never silently omits a suite: a missing prerequisite is
reported as SKIP, and the manifest is checked against tests/*_tests.py.
EOF
}

MODE="all"
VERBOSE=0
STRICT=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --unit-only) MODE="unit" ;;
        --with-nostr) MODE="with-nostr" ;;
        --strict) STRICT=1 ;;
        --verbose) VERBOSE=1 ;;
        --list) MODE="list" ;;
        --help|-h) usage; exit 0 ;;
        *)
            echo "ERROR: unknown option: $1" >&2
            echo >&2
            usage >&2
            exit 2
            ;;
    esac
    shift
done

if ! verify_manifest; then
    exit 2
fi

if [[ "$MODE" == "list" ]]; then
    print_class() {
        local label="$1"
        shift

        printf '%s\n' "$label"

        local suite
        for suite in "$@"; do
            printf '  %s\n' "$suite"
        done

        printf '\n'
    }

    echo "BLAKE SWAP TEST SUITES"
    echo "======================"
    echo

    print_class "unit (no prerequisites)" "${UNIT_SUITES[@]}"
    print_class "regtest (needs chain nodes)" "${REGTEST_SUITES[@]}"
    print_class "nostr (needs nodes + relays + keys)" "${NOSTR_SUITES[@]}"

    echo "total: ${#ALL_SUITES[@]} suites"
    exit 0
fi

if [[ -x "$PROJECT_DIR/.venv/bin/python" ]]; then
    PYTHON_BIN="$PROJECT_DIR/.venv/bin/python"
elif [[ -x "$HOME/blake-swap-venv/bin/python3" ]]; then
    PYTHON_BIN="$HOME/blake-swap-venv/bin/python3"
else
    PYTHON_BIN="$(command -v python3)"
fi

if [[ -z "$PYTHON_BIN" ]]; then
    echo "ERROR: no usable python3 interpreter found" >&2
    exit 2
fi

HAVE_COINCURVE=1
if ! "$PYTHON_BIN" - <<'PY_CHECK'
import sys

try:
    import coincurve  # noqa: F401
except ImportError:
    raise SystemExit(1) from None
PY_CHECK
then
    HAVE_COINCURVE=0
fi

if [[ "$HAVE_COINCURVE" -eq 0 ]]; then
    echo "WARNING: $PYTHON_BIN cannot import coincurve."
    echo "         The node-free unit suites do not need it and will run."
    echo "         The engine cannot sign, so any regtest suite will fail."
    echo "         Install the dependencies with:"
    echo "           \"$PYTHON_BIN\" -m pip install -r requirements.txt"
    echo
fi

case "$MODE" in
    unit) SELECTED=("${UNIT_SUITES[@]}") ;;
    with-nostr) SELECTED=("${ALL_SUITES[@]}") ;;
    *) SELECTED=("${UNIT_SUITES[@]}" "${REGTEST_SUITES[@]}") ;;
esac

echo "============================================================"
echo "BLAKE SWAP TEST SUITE"
echo "============================================================"
echo "project: $PROJECT_DIR"
echo "python:  $PYTHON_BIN"
echo "mode:    $MODE"
echo "suites:  ${#SELECTED[@]} of ${#ALL_SUITES[@]}"
echo

echo "== Syntax =="
if ! "$PYTHON_BIN" -m py_compile src/*.py; then
    echo "FAIL: source did not compile" >&2
    exit 2
fi
echo "PASS"
echo

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
FAILED_SUITES=()
SKIPPED_SUITES=()

run_suite() {
    local suite="$1"
    local args
    local status=0
    local log
    local reason
    local -a argv=()

    args="$(suite_args "$suite")"

    if [[ -n "$args" ]]; then
        read -r -a argv <<< "$args"
    fi

    log="$(mktemp)"

    (
        cd "$PROJECT_DIR" &&
        "$PYTHON_BIN" "$suite" "${argv[@]}"
    ) >"$log" 2>&1 || status=$?

    case "$status" in
        0)
            PASS_COUNT=$((PASS_COUNT + 1))
            printf 'PASS  %s\n' "$suite"

            if [[ "$VERBOSE" -eq 1 ]]; then
                sed 's/^/      /' "$log"
            fi

            rm -f "$log"
            ;;
        77)
            SKIP_COUNT=$((SKIP_COUNT + 1))
            SKIPPED_SUITES+=("$suite")

            reason="$(grep -m1 '^SKIP:' "$log" || true)"

            if [[ -z "$reason" ]]; then
                reason="SKIP: prerequisites not satisfied"
            fi

            printf 'SKIP  %s\n' "$suite"
            printf '      %s\n' "$reason"

            if [[ "$VERBOSE" -eq 1 ]]; then
                sed 's/^/      /' "$log"
            fi

            rm -f "$log"
            ;;
        *)
            FAIL_COUNT=$((FAIL_COUNT + 1))
            FAILED_SUITES+=("$suite")

            printf 'FAIL  %s (exit %s)\n' "$suite" "$status"
            printf '      ---- output (tail) ----\n'
            tail -n 40 "$log" | sed 's/^/      /'
            printf '      full output: %s\n' "$log"
            # Keep the log for inspection.
            ;;
    esac
}

echo "== Suites =="
echo

for suite in "${SELECTED[@]}"; do
    run_suite "$suite"
done

echo
echo "============================================================"
echo "SUMMARY"
echo "============================================================"
printf 'passed:  %s\n' "$PASS_COUNT"
printf 'skipped: %s\n' "$SKIP_COUNT"
printf 'failed:  %s\n' "$FAIL_COUNT"
echo

print_list() {
    local label="$1"
    shift

    local suite
    for suite in "$@"; do
        printf '  %s\n' "$suite"
    done
}

if [[ "$SKIP_COUNT" -gt 0 ]]; then
    echo "SKIPPED SUITES (prerequisites absent)"
    echo "-------------------------------------"
    print_list skipped "${SKIPPED_SUITES[@]}"
    echo
    echo "To run the regtest suites, configure reachable nodes and re-run."
    echo "To run the nostr suites, pass --with-nostr."
    echo "See tests/README.md."
    echo
fi

if [[ "$FAIL_COUNT" -gt 0 ]]; then
    echo "FAILED SUITES"
    echo "-------------"
    print_list failed "${FAILED_SUITES[@]}"
    echo
    echo "TEST SUITE FAILED"
    exit 1
fi

if [[ "$STRICT" -eq 1 && "$SKIP_COUNT" -gt 0 ]]; then
    echo "STRICT MODE: $SKIP_COUNT suite(s) were skipped"
    exit 1
fi

echo "============================================================"
echo "ALL SELECTED BLAKE SWAP TEST SUITES PASSED"
echo "============================================================"
echo
echo "No public Nostr relay was contacted unless --with-nostr was given."

exit 0
