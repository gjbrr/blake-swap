# BLAKE Swap Test Suites

60 suites covering the atomic-swap engine, the wire format, the market
state machine, crash recovery, reorganization safety,
independent-implementation compatibility, and documentation alignment.

All suites are plain Python scripts. There is no third-party test
framework: each suite prints `PASS`/`FAIL` lines and exits with a
meaningful status code.

---

## Quick start

Node-free suites, no setup required:

```bash
./scripts/test.sh --unit-only
```

Everything runnable on this host:

```bash
./scripts/test.sh
```

List suites and their prerequisites:

```bash
./scripts/test.sh --list
```

---

## Prerequisite classes

| Class | Needs | Count |
|---|---|---|
| `unit` | nothing (some need the declared Python deps) | 29 |
| `regtest` | reachable Bitcoin + BLAKE regtest nodes | 29 |
| `nostr` | regtest nodes, real relays, two participant keys | 2 |

The two `nostr` suites publish to real relays and are therefore
**opt-in** via `--with-nostr`.

A `unit` suite needs no nodes and no relays. A few of them exercise the
engine's signing path and therefore need `coincurve`, which is a declared
dependency; those skip cleanly when it is unavailable.

`nip44_tests.py` is the one suite with an **optional** dependency. NIP-44's
key derivation is pure Python, but its cipher and per-message keys need
`cryptography`, which is the `crypto` extra rather than a runtime
dependency. The eight cipher-dependent checks are skipped, not failed, when
it is absent, and the suite exits 77 so the runner reports `SKIP` instead of
claiming a pass for checks that never ran. Install the extra to run them:

```bash
pip install -e ".[dev,crypto]"   # everything the suites can use
scripts/test.sh --unit-only --strict
```

`--strict` makes that skip a failure, which is why CI installs the extra:
a unit gate that silently stopped covering the encrypted coordination path
is the outcome the flag exists to prevent.

---

## Node configuration

The integration suites resolve their nodes at run time; nothing is
hardcoded to a particular host. Values are taken from the environment
first, then from a running local rig, then from conventional locations.

### The local two-chain rig (recommended)

`scripts/test-nodes` builds a disposable Bitcoin Core + Bitcoin Knots
regtest rig, funds it, and publishes its location so the suites find it
with no environment variables at all:

```bash
scripts/test-nodes start        # start both nodes, mine, publish state
scripts/test.sh                 # every suite, including the regtest ones
scripts/test-nodes stop         # stop; the node-backed suites then SKIP
scripts/test-nodes reset        # stop and delete both datadirs
```

The BLAKE side is [Bitcoin Knots](https://github.com/bitcoinknots/bitcoin)
with the BLAKE2b proof-of-work hardfork, which is Knots **v29.4.1 or
later**; the rig prefers `$HOME/s/bitcoin-blake2b/bin/` when that build is
installed and falls back to a stock Knots otherwise. Either build is
enough for the swap machinery — no suite depends on how a block header is
hashed — so the rig does not schedule the hardfork or pass its flags, and
a suite that restarts a node replays that daemon's own argv rather than a
hardcoded copy. Build the chain's node with:

```bash
git clone --depth 1 --branch v29.4.2.knots20260508 \
    https://github.com/bitcoinknots/bitcoin.git ~/s/bitcoinknots/bitcoin
cmake -B ~/s/bitcoinknots/bitcoin/build -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_GUI=OFF -DBUILD_TESTS=OFF -DBUILD_BENCH=OFF
cmake --build ~/s/bitcoinknots/bitcoin/build -j"$(nproc)"
mkdir -p ~/s/bitcoin-blake2b/bin
for b in bitcoind bitcoin-cli bitcoin-tx bitcoin-util; do
    ln -sfn ~/s/bitcoinknots/bitcoin/build/bin/$b ~/s/bitcoin-blake2b/bin/$b
done
```

A wallet created by a newer node cannot be reopened by an older one, so
switching the rig between node versions needs a `scripts/test-nodes reset`
(fresh chain, fresh wallets). The suites bootstrap their own capital: the
BTC side mines, and the BLAKE side takes the rig wallet's initial maturity
run once and then recycles it between the two participant wallets.

`start` writes `$BLAKE_SWAP_TEST_ROOT/rig-env.json`; `tests/node_config.py`
reads it. That is what makes a running rig authoritative: a **stopped**
rig produces a `SKIP` naming the rig, rather than a suite that connects
to nothing and fails, or one that quietly runs against an unrelated
datadir. To override it, set the environment variables below in the
usual way.

| Variable | Purpose |
|---|---|
| `BLAKE_SWAP_BTC_CLI` | Bitcoin `bitcoin-cli` path |
| `BLAKE_SWAP_BTC_DATADIR` | Bitcoin data directory |
| `BLAKE_SWAP_BTC_RPC_ARGS` | extra `bitcoin-cli` arguments (remote nodes) |
| `BLAKE_SWAP_BLAKE_CLI` | BLAKE `bitcoin-cli` path |
| `BLAKE_SWAP_BLAKE_DATADIR` | BLAKE data directory |
| `BLAKE_SWAP_BLAKE_RPC_ARGS` | extra `bitcoin-cli` arguments (remote nodes) |
| `BLAKE_SWAP_RPC_TIMEOUT_SECONDS` | per-RPC timeout; a hung node fails closed instead of blocking (default 120) |
| `BLAKE_SWAP_RPC_MAX_ATTEMPTS` | attempts for a **read-only** RPC that fails transiently (default 3). State-changing calls are never retried, because a call that timed out may have succeeded |
| `BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS` | first retry delay, doubled per attempt (default 1.0, i.e. 1 s then 2 s) |
| `BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS` | ceiling on that doubling (default 30) |
| `BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS` | lifetime of an `ignore-match` tombstone; 0 never expires (default 0) |
| `BLAKE_SWAP_TEST_ROOT` | location of the `scripts/test-nodes` rig (default `$HOME/.blake-swap-test`) |
| `BLAKE_SWAP_TEST_BTC_CLI` | `bitcoin-cli` the rig starts for Bitcoin Core |
| `BLAKE_SWAP_TEST_BLAKE_CLI` | `bitcoin-cli` the rig starts for Bitcoin Knots |

These are the same variables the engine reads, so the harness and the
engine always agree on which nodes to use. `scripts/test.sh`
deliberately does **not** unset them.

### Resolution order

1. the environment variable, if set;
2. a running `scripts/test-nodes` rig, from `rig-env.json`;
3. for the Bitcoin CLI: `$HOME/s/bitcoin-core/bin/bitcoin-cli`,
   `$HOME/bitcoin-core/build/bin/bitcoin-cli`,
   `$HOME/bitcoin/build/bin/bitcoin-cli`, `/usr/local/bin/bitcoin-cli`,
   `/usr/bin/bitcoin-cli`, then `bitcoin-cli` on `PATH`;
4. for the BLAKE CLI: `$HOME/s/bitcoin-knots/bin/bitcoin-cli`, then
   `$HOME/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli` and similar build
   locations.
5. for a datadir: the rig, then `$HOME/bitcoin-swap-regtest`, then
   `$HOME/.bitcoin` (Bitcoin); the rig, then `$HOME/blake-swap-regtest`
   and `$HOME/.blake-swap-regtest` (BLAKE).

The BLAKE CLI is deliberately **not** resolved through `PATH`: a generic
`bitcoin-cli` is almost certainly Bitcoin Core, and using it for the
BLAKE chain would silently test the wrong node.

If an explicit variable is set but does not resolve, the suites report
that verbatim rather than falling back to a different node.

### Nodes with a non-default layout

```bash
export BLAKE_SWAP_BTC_CLI=/opt/bitcoin/bin/bitcoin-cli
export BLAKE_SWAP_BTC_DATADIR=/srv/btc-regtest
export BLAKE_SWAP_BLAKE_CLI=/opt/blake/bin/bitcoin-cli
export BLAKE_SWAP_BLAKE_DATADIR=/srv/blake-regtest

./scripts/test.sh
```

### Remote nodes

Leave the datadir unset and pass the connection arguments instead. Note
that the harness mirrors the engine, so the Bitcoin CLI does **not**
implicitly add `-regtest` — include it in the RPC args:

```bash
export BLAKE_SWAP_BTC_CLI=/usr/bin/bitcoin-cli
export BLAKE_SWAP_BTC_RPC_ARGS="-regtest -rpcconnect=10.0.0.5 -rpcport=18443 -rpcuser=... -rpcpassword=..."
export BLAKE_SWAP_BLAKE_CLI=/usr/bin/bitcoin-cli
export BLAKE_SWAP_BLAKE_RPC_ARGS="-regtest -rpcconnect=10.0.0.5 -rpcport=19443 -rpcuser=... -rpcpassword=..."
```

Check the resolution without running anything:

```bash
python3 tests/node_config.py
```

---

## The skip protocol

A suite that cannot run reports and exits with status **77** (the
conventional automake skip code) and prints a line beginning `SKIP:`.

```
========================================================================
SKIP: chain node prerequisites are not satisfied
========================================================================
This suite was NOT run. Its prerequisites are not satisfied.

  Bitcoin CLI:      <unresolved>  [no candidate found]
  ...

  Bitcoin CLI unresolved. Set BLAKE_SWAP_BTC_CLI to your bitcoin-cli path.

See tests/README.md for how to point the suites at your nodes.
```

`SKIP` is not `FAIL`. The runner:

- reports every suite it selected, so nothing is silently omitted;
- exits `0` when everything that ran passed, even if some suites skipped;
- exits `1` when anything failed, or when `--strict` was given and
  anything skipped;
- exits `2` when the runner or its suite manifest is misconfigured.

The runner also verifies its own manifest against `tests/*_tests.py` and
refuses to run if the two disagree, so a newly added suite cannot be
forgotten.

---

## Runner reference

```
scripts/test.sh                 run unit + regtest suites
scripts/test.sh --unit-only     run only the node-free suites
scripts/test.sh --with-nostr    also run the public-relay suites
scripts/test.sh --strict        treat SKIP as failure
scripts/test.sh --verbose       stream all suite output
scripts/test.sh --list          list suites and prerequisites
scripts/test.sh --help
```

---

## Coverage

`scripts/coverage.sh` runs the same suites under coverage and reports
line **and branch** coverage for `src/`:

```
scripts/coverage.sh                     report only
scripts/coverage.sh --fail-under 80     enforce a threshold (exit 1 below it)
scripts/coverage.sh --html              also write htmlcov/
scripts/coverage.sh --xml               also write coverage.xml
scripts/coverage.sh --unit-only         node-free suites only
```

The suites drive the engine as a **subprocess**, so coverage is collected
from child processes as well: `.coveragerc` sets `parallel = True`,
`coverage.sh` exports `COVERAGE_PROCESS_START`, and the startup hook
installed in `.venv` makes each child record its own data file, which
`coverage combine` then merges. `exit 2` means coverage itself could not
run (missing `.venv`, nothing collected); `exit 1` means the threshold
was not met.

`--fail-under 80` is the project's gate, and the suite set currently
meets it. CI runs it on every push and pull request, on a runner that
provisions both node binaries and verifies the rig came up before running
anything; `tests/packaging_tests.py` checks that the workflow, `pyproject.toml`
and `requirements.txt` agree.

`coverage.sh` installs the subprocess startup hook
(`site-packages/a1_coverage.pth`) itself if it is missing, and refuses
to run without it: the engine runs as a subprocess, so a checkout that
never had the hook would produce a report that silently omits `src/`
and still appear to pass. It also refuses to report on stale parallel
data, so each run measures that run. A suite that cannot run in the current environment inflates
nothing: it reports `SKIP` and contributes no coverage, which is the
honest reading — run `scripts/test-nodes start` first.

---

## Suite inventory

### Unit suites — no prerequisites (29)

| Suite | Covers |
|---|---|
| `fee_policy_tests.py` | fee-rate selection, ceiling rounding, and the fail-closed fee cap |
| `swap_timeout_policy_tests.py` | timeout budget math, integer block rounding, the minimum recovery floor, and the refusal to order cross-chain heights |
| `swap_model_timeout_invariant_tests.py` | `Swap.validate` must not compare BTC and BLAKE heights as one number line |
| `dynamic_settlement_builder_tests.py` | settlement spend construction, including explicit fee overrides |
| `btc_dynamic_settlement_tests.py` | dynamic settlement against a stubbed node (estimatesmartfee, mempool, decoderawtransaction) |
| `swap_dynamic_fee_integration_tests.py` | dynamic fee path with `swap_v2.btc_cli` monkeypatched |
| `swap_wire_tests.py` | the standalone wire validator: schemas, canonical encoding, duplicate-key and `secret_hex` rejection |
| `command_surface_tests.py` | `resume-all` ownership/stem filtering, `dismiss` ownership refusal, `run` step-budget validation and failure containment, ignored-match tombstone expiry, and env-knob validation |
| `market_state_tests.py` | OFFER/TAKE/MATCH/CANCEL parsing and OPEN/MATCHED/CANCELLED/EXPIRED derivation, including terminal-event tie-breaks |
| `docs_alignment_tests.py` | the documentation still matches the implementation: message types, states, transitions, HTLC clauses, timeout numbers, wrapper subcommands, and referenced paths |
| `cli_wiring_tests.py` | every `args.<attr>` a command reads is defined by that subcommand's parser, so a flag copied to a sibling command cannot leave a latent `AttributeError` |
| `nostr_market_tests.py` | the coordination layer against an in-process relay stub: message round-trip, author pinning, event cache, prefix resolution, and the full offer → take → match → cancel market lifecycle. Needs no nodes and no network. |
| `swap_dashboard_json_tests.py` | `dashboard --json` emits the reconciled swap set and preserves the human-readable output |
| `swap_market_takes_json_tests.py` | `market-takes` resolves offer-ID prefixes and emits validated JSON (needs `coincurve`) |
| `swap_resume_step_budget_tests.py` | `resume --max-steps` forwards an explicit step budget to the driver |
| `schema_equality_tests.py` | the engine's acceptance boundary *is* the canonical schema, for all seven settlement types: a schema-valid message is accepted field for field, every required field is required, unknown fields and a nested `secret_hex` are refused by both, internal annotations are tolerated on receipt and refused on send, and a static check that every message type the engine hands to the transport has a schema (and vice versa) |
| `packaging_tests.py` | `pyproject.toml` against `requirements.txt` and `src/` (runtime pins, the optional extra, the declared modules, the licence and the Python range), and the CI workflow itself: it parses, pins the runner and Python, provisions **both** node binaries with matching version/checksum pairs, verifies the rig came up, gates the full suite at the documented threshold, and never makes a merge depend on public relays |
| `installed_wheel_tests.py` | builds the distributable wheel, installs it into a fresh isolated virtual environment, runs the installed console entry points and a node-free engine command outside the checkout, and proves core imports resolve from the installed environment rather than `src/` or inherited `PYTHONPATH` |
| `nip44_tests.py` | the NIP-44 v2 codec against the **official** test vectors rather than its own output: conversation keys, message keys, all published padding lengths, plaintext/payload pairs (ASCII, multi-plane Unicode, emoji, right-to-left), long messages by hash, the 2026 extended length-prefix boundary, and every published invalid class. Eleven cipher-dependent checks need the `crypto` extra and skip without it, and three of them check the standalone implementation in `tools/minimal_nip44.py` against the engine in both directions |
| `blake_amount_tests.py` | BLAKE amounts as integer base units: exact parse/render round-trips, canonical wire strings, refusal of anything finer than one base unit, the legacy float record migration (including the rounded-and-reported case), and the one-base-unit difference that the old `1e-8` tolerance accepted |
| `agent_policy_tests.py` | the autonomous agent's (state → command) table for both roles, the child-process environment and output forwarding, escalation of a child's `SECURITY FAILURE`, the give-up bound on consecutive failures, and clean SIGTERM unwinding |
| `driver_policy_tests.py` | the autonomous driver's decision table, driven directly through its only input (the reconciliation report) with every action executor replaced by a recorder: `claim-btc`, `fund-btc`, `refund-btc` and `refund-blake` are attempted exactly when protocol role, checkpoint, chain status, secret provenance and timeout margin all permit, and never otherwise; a violated precondition is a contained, reported step failure |
| `chain_cli_transport_tests.py` | the chain-RPC transport: ordinary arguments stay in argv, an argument above `MAX_ARG_STRLEN` switches the whole call to `bitcoin-cli -stdin`, booleans still render as `true`/`false`, and a failed exec becomes a named error. Also performs a real `decoderawtransaction` through the stdin transport when chain nodes are reachable |
| `node_config_tests.py` | node resolution: environment first, then a running rig, then conventional paths; a stopped or corrupt rig state file degrades to a skip instead of a wrong target |
| `fail_closed_tests.py` | refusal behaviour when preconditions fail: corrupted state files, unknown swap IDs, mismatched chain identities, missing preimages, and unconfirmed settlements all stop the command instead of acting on a guess, and no on-disk state is mutated |

| `implementation_adapter_tests.py` | structural contract for independently replaceable Alice/Bob implementation adapters |
| `moxie_adapter_smoke_tests.py` | validates the language-neutral adapter fixture and the first Python/Moxie seed-4 target |
| `security_conformance_tests.py` | machine-readable C1-C47 security-conformance manifest completeness and proving-suite mappings |
| `swap_model_settlement_identity_tests.py` | durable settlement identity invariants at the swap-model boundary |

### Regtest suites — need chain nodes (29)

**Core protocol**

| Suite | Covers |
|---|---|
| `swap_protocol_tests.py` | the black-box harness. See below. |
| `swap_timeout_negotiation_tests.py` | Bob's independent v1.5 timeout validation ("Time Mallory"): unknown policy, versionless offers, missing recovery, expired windows, short windows, dishonest metadata |
| `pre_btc_refund_tests.py` | Alice must be able to abort and refund BLAKE before Bob funds Bitcoin |
| `swap_reverse_market_flow_tests.py` | the **reverse** (`btc-for-blake`) market path, where the maker is protocol Bob and the **taker** creates the secret: proves the maker reserves the swap and signs the match without ever holding S |
| `swap_encrypted_coordination_tests.py` | the opt-in encrypted coordination mode over the relay stub: a real encrypted offer/accept handshake, the offer staying plaintext because it is first contact, ciphertext that leaks no message field, and refusals in both directions (an encrypted participant refuses a plaintext downgrade; a plaintext participant refuses content it cannot read) |
| `htlc_key_recovery_tests.py` | the key-recovery drill on regtest: an HTLC funded to a refund key the wallet has **never held**, the `recovery-plan` command's report, importing the private descriptor into a fresh wallet (with its own checksum and `timestamp: "now"`), and that wallet then performing the actual refund |
| `swap_driver_surface_tests.py` | the operator/driver surface on a live swap: `next` converging one safe step at a time, refusing to fund, and refusing to choose between two negotiations or two funding-ready trades, `dashboard` and `dashboard --json`, read-only `reconcile` (JSON and human), `show`/`list`, `ignore-match` tombstones, `dismiss` archiving only provably unfunded swaps, `sync-funding` discovering funding from the chain, and `resume` driving settlement to COMPLETE |
| `swap_market_flow_tests.py` | the **marketplace** path end to end on regtest over the local relay stub: publish-market-offer → take → accept-market-take → accept-market-match → settle to COMPLETE, plus a cancel flow and read-only reconciliation |
| `swap_participant_isolation_tests.py` | distinct participant state roots and no cross-participant state |

**Recovery and safety**

| Suite | Covers |
|---|---|
| `swap_settlement_recovery_tests.py` | crash-point recovery for all four settlement legs, a missing transaction index, and an unknown spender |
| `swap_peer_recovery_tests.py` | an actor disappearing after broadcast: the peer must recover from chain state alone, with no secret leaked on the refund path |
| `swap_reorg_safety_tests.py` | funding, claim, and refund reorganizations, plus final BLAKE claim/refund pending-after-reorg handling |
| `production_funding_pending_tests.py` | an unconfirmed funding broadcast must not advance state, must persist the txid, and must not double-send on retry |
| `production_settlement_pending_tests.py` | the same pending-confirmation semantics for claim and refund legs, with auto-mining disabled |

**Autonomy**

| Suite | Covers |
|---|---|
| `swap_chain_autonomy_tests.py` | idle funding polling, no funding announcements, and full post-negotiation settlement driven only from chain state |
| `swap_bounded_scan_tests.py` | bounded funding-scan behaviour and its diagnostics |
| `swap_autonomous_agents_tests.py` | `src/swap_agent.py`: start, kill, restart, and SIGTERM lifecycle |

**Independent-implementation wire compatibility**

Each of these runs the real engine on one side and then produces the
peer's message using only `tools/minimal_*.py` and `src/swap_wire.py`,
proving the engine accepts messages built solely from the published
wire rules.

| Suite | Independence point |
|---|---|
| `swap_wire_compat_tests.py` | round-trips every engine-produced message through `swap_wire` |
| `swap_cross_impl_offer_tests.py` | the `offer` |
| `swap_cross_impl_accept_tests.py` | the `accept` |
| `swap_cross_impl_blake_funded_tests.py` | the `blake-funded` announcement |
| `swap_cross_impl_btc_funded_tests.py` | the `btc-funded` announcement |
| `swap_cross_impl_btc_claimed_tests.py` | the `btc-claimed` announcement |
| `swap_cross_impl_blake_claimed_tests.py` | the `blake-claimed` announcement |
| `swap_cross_impl_full_happy_tests.py` | an end-to-end swap with independently produced messages |

| `swap_seeded_adversarial_tests.py` | replayable seeded adversarial sequences and portable behavioral fingerprints for differential implementations |
| `swap_settlement_crash_matrix_tests.py` | all four settlement branches crashed at candidate-save, post-broadcast, and confirmation-before-save boundaries |
| `swap_settlement_fault_matrix_tests.py` | deterministic settlement fault injection and exact-transaction recovery behavior |
| `swap_settlement_race_tests.py` | concurrent claim/refund attempts converge on one canonical economic transaction identity |

### Nostr suites — need relays and keys, opt-in (2)

| Suite | Covers |
|---|---|
| `swap_isolated_nostr_handshake_tests.py` | the offer/accept handshake over real relays with isolated state and keys |
| `swap_isolated_full_chain_swap_tests.py` | a full swap negotiated over real relays, asserting filesystem and key isolation throughout |

Both require an existing participant key pair and publish real events,
so they are deliberately **not** run by default and deliberately do
**not** fabricate fallback keys (that would publish events under a
publicly-known private key). Point them at a key directory with:

```bash
export BLAKE_SWAP_TEST_KEY_DIR="$HOME/.blake-swap/nostr-keys"
./scripts/test.sh --with-nostr
```

Relay selection uses `BLAKE_SWAP_NOSTR_RELAYS` (or the engine default).
Prefer a private or local relay when running these.

---

## The integration harness

`tests/swap_protocol_tests.py` (4,100+ lines) is the shared black-box
harness. It:

- drives the **public CLI as a subprocess**, never importing engine
  internals, so it tests the same surface an operator uses;
- creates a fresh temporary state root per run and separate Alice/Bob
  state directories;
- scrubs inherited participant/session configuration so an operator's
  shell state cannot leak into a test;
- forces **FILE transport only** and points
  `BLAKE_SWAP_NOSTR_RELAYS` at an unreachable local address, so it never
  contacts a public relay;
- mines regtest blocks on demand;
- creates the `alice` and `bob` signing wallets on both chains if they
  do not already exist.

It is both a suite and a library. 18 suites do
`import swap_protocol_tests as h` and reuse `h.preflight`, `h.run`,
`h.swap_cmd`, `h.btc_cli`, `h.blake_cli`, `h.load_state`, `h.handshake`,
`h.fund_blake_and_sync`, and the assertion helpers.

Its own suites can be selected individually:

```bash
python3 tests/swap_protocol_tests.py happy
python3 tests/swap_protocol_tests.py refund
python3 tests/swap_protocol_tests.py mallory
python3 tests/swap_protocol_tests.py recovery
python3 tests/swap_protocol_tests.py race
python3 tests/swap_protocol_tests.py market-retry
python3 tests/swap_protocol_tests.py market-roles
python3 tests/swap_protocol_tests.py reverse-market
python3 tests/swap_protocol_tests.py role-guard
python3 tests/swap_protocol_tests.py pending-btc-claim
python3 tests/swap_protocol_tests.py pending-blake-claim
python3 tests/swap_protocol_tests.py pending-btc-refund
python3 tests/swap_protocol_tests.py pending-blake-refund
python3 tests/swap_protocol_tests.py all
```

The two `test_reorg_btc_claim`/`test_reorg_btc_refund` scenarios run only
under `all`.

---

## The local relay stub

`tests/nostr_relay_stub.py` is a minimal in-memory NIP-01 relay that runs on its
own event loop inside the test process. Pointing `BLAKE_SWAP_NOSTR_RELAYS` at it
lets the transport, marketplace, and event-cache paths be exercised **without
contacting any public relay**, which is faster and avoids publishing test traffic.

It is a stub, not a relay: it has no auth, no persistence, and accepts whatever it
is given, so the *client's* signature and schema verification is what gets tested.
A `limit` returns the **newest** events, matching real relay behaviour.

`tests/nostr_market_tests.py` drives the full market lifecycle against it.

## The shared node-configuration helper

`tests/node_config.py` holds the resolution logic, the `skip()` helper,
and `node_env()`. The harness re-exports those names so existing suites
can keep using `h.BTC_CLI` and friends.

Use `node_env()` — never `str(node_config.BTC_CLI)` — when a suite needs
to build an explicit environment for a child process. `node_env()`
includes only resolved values, so an unresolved path can never be passed
on as the literal string `"None"` and shadow the engine's own defaults.

```python
import node_config

env = os.environ.copy()
env.update(node_config.node_env())
```

---

## What the regtest suites do to your node

Run them against a **dedicated regtest datadir**, not a node you care
about. The integration suites:

- create and use `alice` and `bob` wallets;
- mine many regtest blocks (coinbase maturity alone requires 100);
- create and broadcast transactions;
- write only inside a temporary state directory that is removed on exit.

They never touch mainnet, testnet, or signet.

---

## Adding a suite

1. Name the file `tests/<name>_tests.py`.
2. Exit `0` on success, non-zero on failure, and use
   `node_config.skip(...)` (status 77) when a prerequisite is missing.
3. If the suite needs chain nodes, call `preflight()` — either
   `h.preflight()` via the harness, or
   `node_config.require_node_config()` for a standalone suite.
4. Add the file to the matching array in `scripts/test.sh`. The runner
   verifies its manifest against `tests/*_tests.py` and will refuse to
   run until you do.

---

## See also

- `docs/PROTOCOL.md` — protocol specification
- `docs/WIRE_PROTOCOL.md` — message formats
- `docs/REGTEST_TWO_HOST.md` — two-host operator guide
- `scripts/wire_inventory.py` — regenerate the wire message inventory
  from the engine source
