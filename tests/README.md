# BLAKE Swap Test Suites

31 suites covering the atomic-swap engine, the wire format, the market
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
| `unit` | nothing (some need the declared Python deps) | 13 |
| `regtest` | reachable Bitcoin + BLAKE regtest nodes | 20 |
| `nostr` | regtest nodes, real relays, two participant keys | 2 |

The two `nostr` suites publish to real relays and are therefore
**opt-in** via `--with-nostr`.

A `unit` suite needs no nodes and no relays. A few of them exercise the
engine's signing path and therefore need `coincurve`, which is a declared
dependency; those skip cleanly when it is unavailable.

---

## Node configuration

The integration suites resolve their nodes at run time; nothing is
hardcoded to a particular host. Values are taken from the environment
first and then from conventional locations.

| Variable | Purpose |
|---|---|
| `BLAKE_SWAP_BTC_CLI` | Bitcoin `bitcoin-cli` path |
| `BLAKE_SWAP_BTC_DATADIR` | Bitcoin data directory |
| `BLAKE_SWAP_BTC_RPC_ARGS` | extra `bitcoin-cli` arguments (remote nodes) |
| `BLAKE_SWAP_BLAKE_CLI` | BLAKE `bitcoin-cli` path |
| `BLAKE_SWAP_BLAKE_DATADIR` | BLAKE data directory |
| `BLAKE_SWAP_BLAKE_RPC_ARGS` | extra `bitcoin-cli` arguments (remote nodes) |

These are the same variables the engine reads, so the harness and the
engine always agree on which nodes to use. `scripts/test.sh`
deliberately does **not** unset them.

### Resolution order

1. the environment variable, if set;
2. for the Bitcoin CLI: `$HOME/bitcoin-core/build/bin/bitcoin-cli`,
   `$HOME/bitcoin/build/bin/bitcoin-cli`, `/usr/local/bin/bitcoin-cli`,
   `/usr/bin/bitcoin-cli`, then `bitcoin-cli` on `PATH`;
3. for the BLAKE CLI: `$HOME/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli`
   and similar build locations, then the historical author-host path.

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

## Suite inventory

### Unit suites — no prerequisites (13)

| Suite | Covers |
|---|---|
| `fee_policy_tests.py` | fee-rate selection, ceiling rounding, and the fail-closed fee cap |
| `swap_timeout_policy_tests.py` | timeout budget math, integer block rounding, the minimum recovery floor, and the refusal to order cross-chain heights |
| `swap_model_timeout_invariant_tests.py` | `Swap.validate` must not compare BTC and BLAKE heights as one number line |
| `dynamic_settlement_builder_tests.py` | settlement spend construction, including explicit fee overrides |
| `btc_dynamic_settlement_tests.py` | dynamic settlement against a stubbed node (estimatesmartfee, mempool, decoderawtransaction) |
| `swap_dynamic_fee_integration_tests.py` | dynamic fee path with `swap_v2.btc_cli` monkeypatched |
| `swap_wire_tests.py` | the standalone wire validator: schemas, canonical encoding, duplicate-key and `secret_hex` rejection |
| `market_state_tests.py` | OFFER/TAKE/MATCH/CANCEL parsing and OPEN/MATCHED/CANCELLED/EXPIRED derivation, including terminal-event tie-breaks |
| `docs_alignment_tests.py` | the documentation still matches the implementation: message types, states, transitions, HTLC clauses, timeout numbers, wrapper subcommands, and referenced paths |
| `cli_wiring_tests.py` | every `args.<attr>` a command reads is defined by that subcommand's parser, so a flag copied to a sibling command cannot leave a latent `AttributeError` |
| `swap_dashboard_json_tests.py` | `dashboard --json` emits the reconciled swap set and preserves the human-readable output |
| `swap_market_takes_json_tests.py` | `market-takes` resolves offer-ID prefixes and emits validated JSON (needs `coincurve`) |
| `swap_resume_step_budget_tests.py` | `resume --max-steps` forwards an explicit step budget to the driver |

### Regtest suites — need chain nodes (20)

**Core protocol**

| Suite | Covers |
|---|---|
| `swap_protocol_tests.py` | the black-box harness. See below. |
| `swap_timeout_negotiation_tests.py` | Bob's independent v1.5 timeout validation ("Time Mallory"): unknown policy, versionless offers, missing recovery, expired windows, short windows, dishonest metadata |
| `pre_btc_refund_tests.py` | Alice must be able to abort and refund BLAKE before Bob funds Bitcoin |
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
