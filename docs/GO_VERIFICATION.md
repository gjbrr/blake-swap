# Go port: what has been verified

This is the evidence record for the Go/TinyGo port, kept separate from the generated
[`GO_PARITY_MANIFEST.md`](GO_PARITY_MANIFEST.md): the manifest says *what replaces what*,
and this file says *what was actually run, and what it proved*.

Everything below was run in this checkout against the reference engine in the same
checkout. Commands are given so any claim can be re-checked rather than believed.

---

## 1. The Go implementation passes its own suite

```bash
go test ./pkg/... ./cmd/...
```

Every package passes. Two of those suites drive the real two-chain regtest rig with the
Python engine as the counterparty (`pkg/swap`): the handshake and settlement in both
directions, the refund path in both directions, a stage-by-stage walk of every checkpoint,
and a record written by Go being loaded by Python. The rest of the rig-driven suites run
this implementation on both sides.

The `internal` packages are part of the gate as well (`make test` runs
`./pkg/... ./internal/... ./cmd/server`), because the packaging, documentation,
conformance and clean-install checks live there.

## 2. The 60 reference suites have a Go disposition

`./scripts/test.sh --list` classifies the reference suites as UNIT, REGTEST and NOSTR. The
manifest's suite table carries the disposition of all 60, and the counts are the ones that
table prints:

| Class | Suites | Needs |
|---|---|---|
| unit | 7 | no nodes: `chain_cli_transport`, `cli_wiring`, `docs_alignment`, `installed_wheel`, `node_config`, `packaging`, `security_conformance` |
| regtest | 11 | the two-chain rig: `htlc_key_recovery`, `pre_btc_refund`, `swap_chain_autonomy`, `swap_participant_isolation`, `swap_peer_recovery`, `swap_protocol`, `swap_reorg_safety`, `swap_settlement_crash_matrix`, `swap_settlement_fault_matrix`, `swap_settlement_race`, `swap_settlement_recovery` |
| nostr | 2 | the rig plus a relay: `swap_isolated_nostr_handshake`, `swap_isolated_full_chain_swap` |

So **13** suites need chain nodes, not 18, and **7** need no nodes at all. An earlier
version of this document said 18 and 2, which was wrong: it counted every unmapped suite as
integration and only `chain_cli_transport` and `cli_wiring` as unit.

The 7 unit suites are not one kind, and their Go replacements differ accordingly:

- `chain_cli_transport_tests.py` and `cli_wiring_tests.py` test Python *mechanics* that have
  no Go counterpart — a subprocess argv/stdin boundary and argparse attribute wiring. Their
  portable halves are ported (`pkg/rpc` proves the large body, the exact value types, the
  retry allowlist and the refusal vocabulary; the CLI proves dispatch completeness over all
  58 command names). The argv boundary and the argparse bug class are waived inside those Go
  tests with a reason apiece.
- `docs_alignment_tests.py`, `installed_wheel_tests.py` and `packaging_tests.py` test the
  Python *distribution*. Each has the Go-artefact analogue of every invariant
  (`internal/docsalign`, `internal/installcheck`, `internal/packaging`) and records the
  Python-only assertions as waived sub-checks.
- `node_config_tests.py` and `security_conformance_tests.py` are language-independent and
  ported directly (`pkg/nodeconfig`, `internal/conformance`).

A suite is claimed as **ported** only when a Go test names it with a
`// Parity: tests/<suite>.py` marker and that test exists in the tree. A similar file name
is not evidence, and the manifest no longer infers one.

## 3. The CLI is byte-identical to the reference where it claims to be

Run on a live swap, in the same state tree, one implementation after the other:

| Command | Result |
|---|---|
| `show SWAP` | identical |
| `list` | identical |
| `reconcile SWAP` and `--json` | identical |
| `dashboard` and `--json` | identical |
| `resume` and `resume PREFIX` | identical |
| `run SWAP` | identical |
| `status` | identical |
| `market`, `market-state`, `market-state --json` | identical on live relay evidence |
| `new`, `verify-blake`, `verify-btc`, `fund-blake`, `fund-btc`, `claim-btc`, `claim-blake`, `refund-btc`, `refund-blake` | identical, proved by a scripted walk that runs both implementations |
| `publish-offer`, `accept-offer`, `receive-accept`, `publish-*`/`receive-*` funding, claim and refund, `complete`, `extract-secret`, `resume-all`, `publish-market-offer`, `publish-market-match` | identical, same walk |
| the whole market negotiation: `publish-market-offer`, `market`, `market-state`, `take-market-offer`, `inspect-market-take`, `market-takes`, `market-takes --json`, `accept-market-take`, `accept-market-match`, `market-matches`, `cancel-market-offer` | identical, relay chatter and publish confirmations included |

How that is proved, and what it covers: `TestCLIParityUnknownSwap` runs every command
that takes a swap id against the same durable state with both implementations and
compares the refusal sentence — the half that can be compared exactly, because
neither side changes anything. `TestCLIParityProtocolWalk`, `TestCLIParityRefundWalk`
and `TestCLIParityMarketWalk` run the same scripted walk twice, once with each
implementation driving, and compare every step's output, normalising only the
identifiers that legitimately differ: transaction and event ids, keys, addresses,
heights, fees, the output index the HTLC landed in, a market take's random nonce, and
a remaining-lifetime figure that can fall either side of a minute boundary.

The market walk is the negotiation rather than the read commands alone, because the
relay chatter and the publish confirmations are most of what these commands print: the
market scan line (`Nostr market read: ...`), the fetch line (`Nostr event found on:
...`), the outbound-cache block, the per-relay result block, `MARKET MATCH PUBLISHED`
inside `accept-market-take`, and the `accept message:` line and `OFFER ACCEPTED` block
inside `accept-market-match` are all compared. An event is only comparable if the lines
that say where it went are compared with it.

```bash
go test ./cmd/blake-swap-cli/ -run TestCLIParity
```

Three divergences are deliberate and are recorded in [`GO_PORT_MAP.md`](GO_PORT_MAP.md) §5:
a refusal is one `ERROR:` line rather than a traceback, `recovery-plan` reports per-leg
solvability and does not print the reference's step list, and `next SWAP_ID` is an
extension beside the reference's argument-less `next`.

## 4. The two implementations agree across the protocol

| Check | Where | Result |
|---|---|---|
| offer → accept → BLAKE funded → Bitcoin funded → Bitcoin claimed → BLAKE claimed → complete | `pkg/swap` `TestCrossImplStageByStage`, both directions | pass |
| refund both legs | `pkg/swap` `TestCrossImplRefundBothLegs`, both directions | pass |
| a Go record loads in Python | `pkg/swap` `TestCrossImplRecordIsReadableByPython` | pass |
| market handshake, Go maker and Python taker, over a local relay | `cmd/blake-swap-relay` + both CLIs, in [`REGTEST_TWO_HOST.md`](REGTEST_TWO_HOST.md) §15 | pass |
| market handshake, **both sides Go**, over a local relay | `cmd/blake-swap-cli` `TestSwapMarketFlow` | pass |
| the protocol report agrees | `pkg/swap` `assertReconcileAgrees` | pass |

## 5. The settlement suite is a real durability drill

The reference's settlement matrices are reproduced with the reference's own
fault-injection contract: twelve failpoint names (`after_{btc,blake}_{claim,refund}`
× `{candidate_save_before_broadcast, broadcast, confirmation_before_state_save}`) and four
post-broadcast crash points, all gated by `BLAKE_SWAP_TEST_MODE=1` and ending the process
with the reference's exit codes 97 and 86.

| Suite | What it proves | Command |
|---|---|---|
| crash matrix | a death at any of the twelve boundaries leaves the durable state where it was, leaves the signed candidate with its txid and hex and no block hash, and the restart adopts that transaction instead of signing a second one | `go test ./pkg/swap/ -run TestSettlementCrashMatrix` |
| fault matrix | the same twelve, with the stricter claim: never more than one spender of the HTLC at any observation, and exactly one transaction ever spent it in its block | `go test ./pkg/swap/ -run TestSettlementFaultMatrix` |
| recovery | the actor dies after the broadcast and the node restarts; the record's txid survives, its signed hex decodes to the transaction in the mempool, and the confirming block advances the state | `go test ./pkg/swap/ -run TestSettlementRecovery` |
| race | two invocations released at the same instant: at least one succeeds, the record names one transaction, and exactly one transaction spent the HTLC | `go test ./pkg/swap/ -run TestSettlementRace` |

**One bound is recorded rather than worked around.** A restarted node re-submits its loaded
wallets' unconfirmed transactions even with wallet broadcast disabled, so a node cannot be
made to forget a wallet transaction; "rebroadcast after mempool loss" is therefore proved
where it is reachable — at the candidate boundary, where nothing was broadcast and the
transaction can only appear in the mempool if the restart sent the recorded bytes.

## 6. Reorg safety is proved with the node, not by editing a record

`go test ./pkg/swap/ -run TestReorgSafetySuite` disconnects and reconnects real blocks with
`invalidateblock` and `reconsiderblock`: a peer refuses to fund its own leg while the
counterparty commitment is no longer confirmed, an authentic announcement whose settlement
is not in the active chain is refused and leaves the receiver's record byte-identical, a
rolled-back final settlement stops being reported as confirmed or settled, and the same
transaction is recovered when the chain returns.

## 7. The browser client builds and works

```bash
make wasm                              # both modules, gated in CI
make wasmtest                          # every WASM-testable package under TinyGo
python3 scripts/verify-browser.py      # headless Chromium, real clicks
```

The browser check drives the page the way a person does and asserts document markers rather
than scraping text. Its market step and its coordination steps use the browser's own
`WebSocket`: no CLI-side socket, no proxy, no node.

### The browser can carry a settlement message

The worker's coordination domain publishes and receives all seven settlement message types
over a relay, with the body built in Go from the swap record rather than accepted from the
page. Sender pinning is enforced by the transport and restated at the use site, and
decryption is fail-closed: a plaintext reply under `nip44_v2`, a payload that looks
encrypted but does not decrypt, and an unreadable payload are each refused.

Six steps in `scripts/verify-browser.py` prove it end to end, against the loopback relay the
check starts:

| Step | What it proves |
|---|---|
| a | the browser publishes a signed coordination message |
| b | a second client (the CLI's `coord-read`) reads and validates it |
| c | that client replies |
| d | the browser receives and validates the reply |
| e | a reply signed by the wrong key is refused, and the refusal names both authors |
| f | one encrypted round trip under `nip44_v2`, and a plaintext reply refused as a downgrade |

Exit status 0 on the run recorded at the bottom of this file.

### The crypto path is the TinyGo one, and it is tested as such

The secp256k1 surface comes from `git.smesh.lol/orly`, whose signer and BIP-340 code have
`tinygo`/`wasm` build-tagged variants: a host build uses the amd64 assembly and a browser
build uses the pure-Go field, group and Schnorr implementation. That switch is not cosmetic —
an assembly-only dependency compiles on the host and fails, or silently misbehaves, in wasm —
so the key package's own tests run under `tinygo test -target=wasm`:

```bash
tinygo test -target=wasm ./pkg/keys ./pkg/event ./pkg/nostr ./pkg/nip44 ./pkg/coord
```

They sign and verify the BIP-340 reference vectors (deterministic signing reproduces each
vector, which is what cross-checks the pure-Go signer against the vectors rather than against
itself), and they exercise the raw-x ECDH that NIP-44 needs. CI runs `make wasm` and the same
tests on every push.

## 8. `next` is the reference's discovery command

`next` with no argument scans the whole local state tree and the configured relays, finds the
one unambiguous non-economic coordination step, and performs it: take an open offer,
import an authenticated acceptance, import a fresh validated match, import the reverse
direction's offer, or accept a valid take. Freshness is
`BLAKE_SWAP_NEXT_FRESH_SECONDS` (default 3600, refused below 60); durable
`market-selections/offer-<id>.json` records and a prior-take check stop it publishing a
second take or accepting the same take twice. Exactly one action is performed and reported
with the reference's banners, more than one is listed and refused, and none falls through to
the settlement and funding report — which is printed and never executed.

`next SWAP_ID` remains as an extension: it reports one swap's recommendation.

```bash
go test ./cmd/blake-swap-cli/ -run TestNext
```

## 9. The seeded corpus is reproducible from this implementation

The corpus's scheduler is `SHA-256("<version>:<seed>:<path>:<stage>")` read as an unsigned
big-endian integer modulo the option count. `pkg/adversary` reproduces it, and its test pins
the reference's eight default schedules (seeds 4, 17, 184467, 20261004 × claim/refund) exactly.

## 10. Reproducing the rig-dependent parts

```bash
scripts/test-nodes start          # writes ~/.blake-swap-test/rig-env.json
make server                       # build the CLI and the app server
go test ./pkg/swap/               # every protocol, settlement, reorg and recovery suite
python3 scripts/verify-browser.py
```

Without the rig, those suites skip and say so, naming the reason. A skip is never counted as
a pass in this repository, and a stale `rig-env.json` whose nodes are gone is a skip with a
reason rather than a failure.

## 11. What is not done

Recorded here rather than left to be inferred from the manifest:

Nothing in the suite table is pending. What is left is verification breadth rather than
port work:

- **`./scripts/test.sh` has not been run end to end in one pass** in this session. Each Go
  suite above was run on its own against the same rig, and the reference suites quoted in §2
  were run individually; the runner has not swept all 60 in one go since the last of them
  landed.
- **Three guarantees are bounded, not absolute.** A restarted node re-submits its wallets'
  unconfirmed transactions, so "the node forgot the mempool" is not reproducible for a
  wallet transaction (§5). A refund submitted exactly at its CLTV height can be final for
  the next block rather than this one, which the suites handle by adopting the recorded
  transaction rather than by mining differently. And the key-recovery drill proves that a
  wallet created after the fact **owns and can solve** an HTLC whose key it holds, but not
  that it can *spend* it: the node will not complete a PSBT signature for an
  announced-descriptor wallet unless the descriptor is active, and activating it would
  change the script the drill is about. The refund spend itself is proven end to end by the
  settlement and pre-Bitcoin refund suites, on wallets that existed when the HTLC was
  funded.
- **Rig-driven packages must not run concurrently.** The suites in
  `internal/rigsuites` and the CLI's parity walk both drive the one regtest rig: mining
  blocks and funding HTLCs from two packages at once makes each of them describe a
  different world, and the failures look like protocol errors. `make test` and CI pass
  `-p 1`, which runs one package at a time.
- **The go tool's package timeout is shorter than this repository's rig suites.** A single
  `go test ./pkg/... ./cmd/...` on a machine without the rig skips them and finishes in a
  minute; with the rig up, `internal/rigsuites` alone runs for longer than the ten minutes
  `go test` allows a package, so `make test` and CI pass `-timeout 45m` (and `30m`). The
  budget is a real deadline, not a disabled one: a hung suite still fails and still names
  the test it was in.

## 12. Recorded command results

| Command | Result |
|---|---|
| `go test ./pkg/... ./cmd/...` | exit 0: 36 packages ok; the rig-driven suites skip with a reason when no rig is present, which is what CI runs |
| `go test -timeout 45m ./pkg/... ./internal/... ./cmd/...` | exit 0 with the rig up: 41 packages ok, including `internal/rigsuites` |
| `./scripts/test.sh` | `passed: 58  skipped: 0  failed: 0` — every suite that does not need `--with-nostr`; no public relay contacted |
| `make wasmtest` | exit 0: every WASM-testable package under TinyGo |
| `tinygo test -target=wasm ./pkg/keys ./pkg/event ./pkg/nostr ./pkg/nip44 ./pkg/coord` | pass, all five packages |
| `make wasm` | pass, both modules |
| `python3 scripts/verify-browser.py` | exit 0: hello, boot gate, encrypted self-test, vault, identity, market relays/publish/list/take, coordination a–f, settlement transport round trip, proxy, engine heights |
| `python3 scripts/go_parity.py --check` | `parity manifest: current` |
| `.venv/bin/python tests/docs_alignment_tests.py` | 11 checks, 0 failures |
| `.venv/bin/python tests/cli_wiring_tests.py` | 58 subcommands, 0 failures |
| `python3 tests/security_conformance_tests.py`, `tests/packaging_tests.py` | pass |
| `PATH=$PWD/.venv/bin:$PATH python3 tests/installed_wheel_tests.py` | 4 checks, 0 failures |
