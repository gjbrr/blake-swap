# Go port: what has been verified

This is the evidence record for the Go/TinyGo port, kept separate from the generated
[`GO_PARITY_MANIFEST.md`](GO_PARITY_MANIFEST.md): the manifest says *what replaces what*, and
this file says *what was actually run, and what it proved*.

Everything below was run in this checkout against the reference engine in the same checkout.
Commands are given so any claim can be re-checked rather than believed.

---

## 1. The Go implementation passes its own suite

```bash
go test ./pkg/... ./cmd/...
```

All packages pass, including the ones that need no rig (amount, wire, market, state, model,
timeout, fees, reconcile, htlc, psbt, keys, nip44, event, vault, rpc, chain, fund, settle,
observe, coord, nostr, store, swap, agent, marketflow, client, worker, platform, adapter,
adversary, relaystub, contract, proto) and the CLI package.

Two of those suites are integration suites that drive the real two-chain regtest rig with the
Python engine as the counterparty (`pkg/swap`): the handshake and settlement in both
directions, the refund path in both directions, a stage-by-stage walk of every checkpoint, and
a record written by Go being loaded by Python.

## 2. The browser client builds and works

```bash
make wasm                              # both modules, gated in CI
make wasmtest                          # every WASM-testable package under TinyGo
python3 scripts/verify-browser.py      # headless Chromium, real clicks
```

The browser check drives the page the way a person does and asserts document markers rather
than scraping text. Its steps, all passing:

| Step | What it proves |
|---|---|
| hello round trip | page → worker → page over the link |
| boot gate | the boot screen clears only when the modules are ready |
| encrypted self-test | an encrypted round trip through the vault's own store |
| vault panel | create, with the passphrase typed into the field |
| identity panel | the signing identity is sealed and read back |
| market relays / publish / list / take | a complete market round trip over a **local relay** the check starts |
| proxy panel | the app server's proxy address is stored |
| engine panel | both chain heights read through the proxy |

The market step is the browser's own `WebSocket`: no CLI, no proxy, no node.

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

Three divergences are deliberate and are recorded in
[`GO_PORT_MAP.md`](GO_PORT_MAP.md) §5: JSON output is JSON only (the reference prints a human
line into its own `--json` mode), a refusal is one `ERROR:` line carrying the reference's
message rather than a traceback around it, and `recovery-plan` refuses a record the reference
renders as a descriptor containing `pk(None)`.

## 4. The two implementations agree across the protocol

| Check | Where | Result |
|---|---|---|
| offer → accept → BLAKE funded → Bitcoin funded → Bitcoin claimed → BLAKE claimed → complete | `pkg/swap` `TestCrossImplStageByStage`, both directions | pass |
| refund both legs | `pkg/swap` `TestCrossImplRefundBothLegs`, both directions | pass |
| a Go record loads in Python | `pkg/swap` `TestCrossImplRecordIsReadableByPython` | pass |
| market handshake, Go maker and Python taker, over a local relay | `cmd/blake-swap-relay` + both CLIs, recorded in [`REGTEST_TWO_HOST.md`](REGTEST_TWO_HOST.md) §15 | pass |
| the protocol report agrees | `pkg/swap` `assertReconcileAgrees` | pass |

The market handshake is the one worth pointing at: the Python engine took a Go offer, the Go
engine accepted that Python-signed take, published the match, and the Python engine imported
it and reported "Bob independently validated v1.5 timeout safety". Neither side learns the
other's secret, and no line of code is shared.

## 5. The seeded corpus is reproducible from this implementation

The corpus's scheduler is `SHA-256("<version>:<seed>:<path>:<stage>")` read as an unsigned
big-endian integer modulo the option count. `pkg/adversary` reproduces it, and its test pins
the reference's eight default schedules (seeds 4, 17, 184467, 20261004 × claim/refund) exactly.

## 6. What is not done

Recorded here rather than left to be inferred from the manifest:

- **22 of the 60 reference suites are not mapped to a Go replacement.** Most are rig-driven
  integration suites — the settlement crash, fault, race and recovery matrices, the isolated
  full-chain swap, the reorg and peer-recovery suites, the banned/adversarial schedulers — that
  drive a full two-chain swap with injected faults. Their Go counterparts would be integration
  tests of the same size, and they are not written.
- **`next` is a different command.** The reference's `next` takes no argument: it discovers the
  single unambiguous non-economic coordination step across the whole state tree (an offer to
  take, an accept to import, a match to import) and performs it. This port's `next SWAP_ID`
  prints the recommended action for one swap. Both are useful; they are not the same command,
  and the CLI is therefore not a drop-in replacement for that one name.
- **The browser has no coordination transport.** The market, the vault, the encrypted store and
  the read-only engine are in the browser; sending a swap's coordination messages to a relay is
  not. The worker's `TODO` is still a `TODO`.
- **Three suites have no Go counterpart by nature**: `packaging_tests.py`,
  `installed_wheel_tests.py` and `docs_alignment_tests.py` test the Python package, the wheel
  and the Python documentation set.

## 7. Reproducing the rig-dependent parts

The integration suites skip unless a two-chain regtest rig and the Python virtualenv are
present:

```bash
scripts/test-nodes start          # writes ~/.blake-swap-test/rig-env.json
make server                       # build the CLI and the app server
go test ./pkg/swap/ -run CrossImpl
python3 scripts/verify-browser.py
```

Without the rig, those suites skip rather than pass, and the browser check reports the market
and vault steps and leaves the engine step until a rig is available — a skip is never counted
as a pass in this repository.
