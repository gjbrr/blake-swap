# BTC ↔ BLAKE Atomic Swap

Experimental peer-to-peer atomic swaps between Bitcoin and the BLAKE
Bitcoin fork using Bitcoin-style HTLCs and Nostr coordination.

**Status:** two-host regtest reference implementation, reproduced
successfully in both physical directions.

**Do not use with valuable funds.**

---

## What has been demonstrated

The reference implementation has completed BTC ↔ BLAKE atomic swaps
across two independently configured Linux hosts with:

- independent Bitcoin and BLAKE nodes,
- separate wallets and participant state,
- separate Nostr identities,
- authenticated market coordination,
- explicit initial BLAKE funding,
- independent HTLC verification,
- chain-based restart and recovery,
- Bitcoin revelation of the hashlock secret,
- Bob recovering that secret directly from Bitcoin,
- settlement without requiring Nostr economic messages,
- dynamic Alice/Bob role assignment in both physical directions.

The practical clean-room reproduction baseline is:

```text
3ac62777eb3e47a5ed99f7000f4f4015a0a6c318
```

The documentation in this tree is aligned with:

```text
f61eeb285914216ec130fe78d1b8c635d3e28315
```

Use the same commit on both hosts.

---

## How it works, in one paragraph

Alice sells BLAKE and buys BTC; Bob sells BTC and buys BLAKE. Alice
generates a 32-byte secret `S` and commits to `H = SHA256(S)`. Both
chains receive an HTLC committing to the same `H`: on BLAKE, Bob can
claim with the preimage and Alice can refund after a timelock; on
Bitcoin the parties are swapped. Alice funds BLAKE first, Bob funds
Bitcoin second, and Alice then claims Bitcoin — which reveals `S` in
the Bitcoin witness. Bob reads `S` from the confirmed Bitcoin
transaction and uses it to claim BLAKE. Nostr carries the negotiation
and the settlement announcements; the chains carry the truth.

---

## Requirements

| Requirement | Notes |
|---|---|
| Python 3.10+ | the engine uses `X \| None` type syntax |
| `coincurve` | BIP-340 Schnorr signing and verification |
| `websockets` | Nostr transport |
| Bitcoin Core | the BTC chain (regtest) |
| BLAKE (Bitcoin fork) | the BLAKE chain (regtest) |
| Nostr relays | coordination; defaults in `docs/REGTEST_TWO_HOST.md` |

Pinned versions are in `requirements.txt`:

```bash
python3 -m venv "$HOME/blake-swap-venv"
"$HOME/blake-swap-venv/bin/pip" install -r requirements.txt
```

The helper scripts prefer `$HOME/blake-swap-venv/bin/python3` and fall
back to `python3`. `scripts/test.sh` additionally accepts a
project-local `.venv`.

---

## Quick start

The full two-host procedure is in `docs/REGTEST_TWO_HOST.md`. In short,
on each host:

```bash
# 1. Start Bitcoin and BLAKE regtest nodes, and peer them to the
#    canonical hub host on each chain.

# 2. Create this participant's wallets, keys, and profile.
./scripts/setup-participant alice        # or: bob

# 3. Load the generated profile (do this in every new shell).
source scripts/participant-env

# 4. Verify local prerequisites and cross-host synchronization.
./scripts/blake-health scripts/participant-env
./scripts/blake-peer-health scripts/participant-env

# 5. Trade.
./scripts/blake market
./scripts/blake offer blake-for-btc --blake 1 --sats 370000 --minutes 30
./scripts/blake next
./scripts/blake fund
./scripts/blake resume
```

`scripts/blake` is the operator interface. There is a smaller
market-only wrapper at `scripts/blake-swap`, and the underlying engine
is `src/swap_v2.py` with 56 subcommands.

### Safety model

- `next` performs **only** unambiguous non-economic coordination, and
  never moves funds.
- Initial BLAKE funding is **always** an explicit `fund` commitment.
- `resume` / `resume-all` handle only post-funding settlement and
  recovery.
- Ambiguous IDs or multiple live choices fail closed.
- `dismiss` is local archival and refuses any swap that may hold funds.

---

## Repository layout

```
src/
  swap_v2.py               the engine: protocol, Nostr, market, reconcile, CLI
  swap_wire.py             dependency-free wire-format validator
  swap_agent.py            headless per-role polling daemon
  swap_timeout_policy.py   CLTV / timeout budget math
  fee_policy.py            dynamic fee selection with a fail-closed cap

docs/
  PROTOCOL.md              protocol specification
  WIRE_PROTOCOL.md         message formats
  REGTEST_TWO_HOST.md      two-host reproduction guide
  PERFORMANCE_AND_RECOVERY.md  design rationale and performance notes
  reverse-swap-e2e-checkpoint.md   historical evidence
  two_host_Regtest.md             historical (earlier topology)
  V1_9_TWO_HOST_INDEPENDENT_NODES.md  historical (earlier checkpoint)

scripts/
  blake                    primary operator interface
  blake-swap               market-only wrapper
  swap                     strict participant-profile guard -> engine
  blake-service            start/stop/status/logs for the runner
  blake-health             local prerequisite check
  blake-peer-health        cross-host sync and checkpoint check
  setup-participant        create wallets, keys, and profile
  market-view              print open offers
  test.sh                  test runner
  stability-check          static safety-invariant checks
  wire_inventory.py        regenerate the wire message inventory

tests/                     51 suites: unit tests and regtest integration
tools/minimal_*.py         independent wire-message producers
tools/minimal_nip44.py     standalone NIP-44 v2 implementation (cross-checks src/nip44.py)
checkpoints/v1.5/          frozen earlier snapshot with SHA256SUMS
```

Two annotated tags mark the documented checkpoints, so a hash in the
documentation can be checked out and verified rather than taken on trust:

```bash
git tag -l 'v1.*'
git checkout v1.5-regtest-prototype     # the sources in checkpoints/v1.5/
git checkout v1.9-independent-nodes     # docs/V1_9_TWO_HOST_INDEPENDENT_NODES.md
```

`checkpoints/v1.5/SHA256SUMS` lists the frozen files by relative path, so
verify it from inside that directory:

```bash
cd checkpoints/v1.5 && sha256sum -c SHA256SUMS
```

---

## Testing

Node-free unit suites run anywhere:

```bash
./scripts/test.sh --unit-only
```

The full suite covers all 51 test files, including the regtest
integration suites and the independent-implementation wire suites. The
regtest suites need two chain nodes; `scripts/test-nodes` starts a
disposable Bitcoin Core + Bitcoin Knots rig for them:

```bash
./scripts/test-nodes start       # start both regtest nodes and fund them
./scripts/test.sh                # everything runnable on this host
./scripts/test.sh --list         # list suites and prerequisites
./scripts/test.sh --with-nostr   # also run the public-relay suites
./scripts/test-nodes stop        # stop them again
```

A running rig is discovered automatically, so nothing needs exporting.
`scripts/coverage.sh` runs the same suites under coverage and reports
line and branch coverage for `src/`; `--fail-under 80` enforces the
project's coverage gate, which the current suite set meets.

`scripts/test-nodes` and `scripts/coverage.sh` document their own
options with `--help`.

CI (`.github/workflows/ci.yml`) does the same thing on a clean runner: it
installs the project with `pip install -e ".[dev,crypto]"` (the `crypto`
extra is what the NIP-44 suite's cipher checks need, and `--strict` makes
their absence a failure rather than a quietly uncovered feature), downloads
the pinned Bitcoin Core and Bitcoin Knots builds with their checksums, starts
the rig, **verifies the rig actually came up** (a runner exits 0 for a skipped
suite, so this is checked explicitly), runs the unit suites and the
documentation check with `--strict`, and then gates the full suite at
`--fail-under 80`. The public-relay suites never gate a merge.
`tests/packaging_tests.py` keeps the workflow, the packaging metadata and
`requirements.txt` from drifting apart — including that the workflow installs
every extra the suites use.

See `tests/README.md` for the suite inventory, prerequisites, and the
environment variables that override node discovery. The integration
harness fails closed with a clear diagnostic when its node prerequisites
are missing, and `test.sh` reports such suites as `SKIP` rather than
silently omitting them.

---

## Documentation

| Document | Contents |
|---|---|
| `docs/PROTOCOL.md` | roles, HTLC construction, timeout policy, state machine, security properties, recovery |
| `docs/WIRE_PROTOCOL.md` | transport bindings, every settlement and market message, validation rules, verification obligations |
| `docs/REGTEST_TWO_HOST.md` | step-by-step two-host reproduction, health checks, troubleshooting |
| `docs/PERFORMANCE_AND_RECOVERY.md` | why live coordination, historical recovery, and chain settlement are separated |
| `docs/ROADMAP.md` | known, scoped work that is not yet done, and what is deliberately deferred |
| `docs/KEY_RECOVERY.md` | recovering an HTLC after a wallet key loss: the recovery plan, the descriptor-import traps, and how to spend each branch |
| `tests/README.md` | test suite inventory, prerequisites, and node configuration |
| `CONTRIBUTOR_ORIENTATION.md` | orientation for new contributors |

---

## Security notes

This is experimental software and has not been audited. In particular:

- Nostr events are **plaintext** kind-1 events by default. Amounts,
  timeouts, transaction IDs, and identities are publicly readable by
  anyone watching the relays. An optional NIP-44 mode
  (`BLAKE_SWAP_NOSTR_ENCRYPTION=nip44_v2`, enabled on both participants)
  encrypts settlement content; the `offer` necessarily stays plaintext,
  because it is first contact and the maker does not yet know the taker.
  See `docs/WIRE_PROTOCOL.md` §3.1a.
- The secret `S` is never transported. It is revealed only by Alice's
  confirmed Bitcoin claim and recovered by Bob only from that
  transaction's witness. The implementation asserts this at every
  publish and receive path.
- Chain state is always authoritative. A coordination message is an
  instruction to go and verify, never a fact.
- The implementation requires at least one confirmation before treating
  a spend as confirmed, and refuses to rebroadcast when a UTXO has
  disappeared but no confirmed spender can be found.

---

## Licence

MIT. See `LICENSE`.

The coordination transport is plaintext by default, and the default
posture is documented rather than assumed: the protocol does not depend
on transport confidentiality, because the secret `S` is never
transported and every fact taken from a message is re-verified against a
chain.

An optional NIP-44 v2 mode (`BLAKE_SWAP_NOSTR_ENCRYPTION=nip44_v2`)
encrypts settlement content as well, with a signed `encryption` tag and a
refusal in both directions: an encrypted participant refuses a plaintext
downgrade, and a plaintext participant refuses content it cannot read.
The `offer` stays plaintext, because it is first contact and NIP-44 is
pairwise by construction. The codec is verified against the official
NIP-44 test vectors.

**Do not use with valuable funds.**
