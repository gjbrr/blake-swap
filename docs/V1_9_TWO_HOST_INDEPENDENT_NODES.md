# HISTORICAL CHECKPOINT — NOT THE CURRENT SETUP GUIDE

This file records an earlier v1.9 development checkpoint. For the current independently reproduced two-host procedure, see `docs/REGTEST_TWO_HOST.md`.

---

cd ~/blake-swap

git status

git add \
  src/swap_v2.py \
  src/swap_agent.py \
  scripts/test.sh \
  tests/swap_bounded_scan_tests.py \
  tests/swap_participant_isolation_tests.py \
  tests/swap_isolated_nostr_handshake_tests.py \
  tests/swap_isolated_full_chain_swap_tests.py

git commit -m "Complete independent-node two-host atomic swap"

git tag -a v1.9-independent-nodes \
  -m "Laptop Alice and Start9 Bob complete atomic swap with independent Bitcoin and BLAKE nodes"

git status
git log --oneline --decorate -8
