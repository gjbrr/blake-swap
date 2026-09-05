#!/usr/bin/env bash

unset BLAKE_SWAP_ROLE || true
unset BLAKE_SWAP_STATE_DIR || true
unset BLAKE_SWAP_NOSTR_KEY_DIR || true
unset BLAKE_SWAP_TRANSPORT || true

unset BLAKE_SWAP_BTC_CLI || true
unset BLAKE_SWAP_BTC_DATADIR || true
unset BLAKE_SWAP_BTC_RPC_ARGS || true

unset BLAKE_SWAP_BLAKE_CLI || true
unset BLAKE_SWAP_BLAKE_DATADIR || true
unset BLAKE_SWAP_BLAKE_RPC_ARGS || true
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export SWAP_V2="$PROJECT_DIR/src/swap_v2.py"
export SWAP_AGENT="$PROJECT_DIR/src/swap_agent.py"
export PYTHONPATH="$PROJECT_DIR/src:$PROJECT_DIR/tests${PYTHONPATH:+:$PYTHONPATH}"

cd "$PROJECT_DIR"

echo "============================================================"
echo "BLAKE SWAP TEST SUITE"
echo "============================================================"
echo "project: $PROJECT_DIR"
echo "engine:  $SWAP_V2"
echo

echo "== Syntax =="
python3 -m py_compile \
    src/swap_v2.py \
    src/swap_agent.py \
    src/swap_timeout_policy.py

echo
echo "== Timeout policy =="
python3 tests/swap_timeout_policy_tests.py

echo
echo "== Timeout negotiation / Time Mallory =="
python3 tests/swap_timeout_negotiation_tests.py

echo
echo "== Protocol regression =="
python3 tests/swap_protocol_tests.py all

echo
echo "== Settlement recovery =="
python3 tests/swap_settlement_recovery_tests.py

echo
echo "== Peer recovery =="
python3 tests/swap_peer_recovery_tests.py

echo
echo "== Chain autonomy =="
python3 tests/swap_chain_autonomy_tests.py

echo
echo "== Bounded funding scan =="
python3 tests/swap_bounded_scan_tests.py

echo
echo
echo "== Reorg safety =="
PYTHONPATH="$PROJECT_DIR/tests:$PROJECT_DIR/src" python3 tests/swap_reorg_safety_tests.py


echo "== Autonomous agents: kill / restart / recovery =="
python3 tests/swap_autonomous_agents_tests.py

echo
echo "============================================================"
echo "ALL V1.5 TEST SUITES PASSED"
echo "============================================================"
