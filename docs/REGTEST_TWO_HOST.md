# Two-Host Regtest Reproduction Guide

This is the current operator guide for reproducing the BTC ↔ BLAKE
atomic-swap reference implementation on two independent Linux hosts.

The procedure was clean-room tested in September 2026 with:

- an HP EliteBook participant,
- a Lenovo IdeaPad participant,
- independent Bitcoin regtest nodes,
- independent BLAKE regtest nodes,
- Nostr coordination,
- separate participant wallets, state directories, and signing
  identities.

Both market directions completed successfully.

This remains experimental software.

**Do not use valuable funds.**

---

## 1. Reference checkpoint

The clean-room reproduction was performed from:

```text
3ac62777eb3e47a5ed99f7000f4f4015a0a6c318
```

The current documented implementation state is:

```text
f61eeb285914216ec130fe78d1b8c635d3e28315
```

Use the same commit on **both** hosts. A protocol mismatch between hosts
is the most common cause of a swap that negotiates but never settles.

---

## 2. Topology

Two participants, two hosts. Each host runs its own Bitcoin and BLAKE
regtest nodes. The two nodes on each chain are **peers on the same
regtest chain** — they are independent processes with independent
wallets, but they must observe one shared chain.

One node per chain acts as the **canonical hub**: the node the other
side peers with. A checkpoint block hash is agreed in advance so both
sides can prove they are on the same chain.

```text
Host A (participant "alice")          Host B (participant "bob")
  Bitcoin regtest node  <---P2P--->   Bitcoin regtest node
  BLAKE   regtest node  <---P2P--->   BLAKE   regtest node
  wallet: blakeswap                   wallet: blakeswap
  state:  ~/.blake-swap               state:  ~/.blake-swap
  Nostr identity A                    Nostr identity B
                 \                     /
                  \__ Nostr relays ___/
```

The participants must **not** share a state directory, a wallet, or a
Nostr key. On two separate hosts the defaults already satisfy this. If
you run both participants on one host for convenience, see §13.

---

## 3. Prerequisites

On **both** hosts:

| Requirement | Purpose |
|---|---|
| Python 3 | runs the engine |
| `coincurve` (pinned in `requirements.txt`) | Schnorr signing and verification |
| `websockets` (pinned in `requirements.txt`) | Nostr transport |
| Bitcoin Core built or installed | the BTC chain |
| BLAKE (Bitcoin fork) built or installed | the BLAKE chain |
| Outbound WSS access to the chosen relays | coordination |

Install the Python dependencies. The helper scripts look for a virtual
environment at `$HOME/blake-swap-venv`:

```bash
python3 -m venv "$HOME/blake-swap-venv"
"$HOME/blake-swap-venv/bin/pip" install -r requirements.txt
```

The test runner additionally accepts a project-local `.venv`; see §14.

Clone the same commit on both hosts:

```bash
git clone git@github.com:gjbrr/blake-swap.git
cd blake-swap
git checkout f61eeb285914216ec130fe78d1b8c635d3e28315
```

---

## 4. Step 1 — Start the chain nodes

Each host runs both nodes in regtest. Paths below match the defaults
used by `scripts/setup-participant`; substitute your own.

```bash
# Bitcoin
bitcoind -regtest -datadir="$HOME/bitcoin-swap-regtest" -daemon

# BLAKE
bitcoind -regtest -datadir="$HOME/blake-swap-regtest" -daemon
```

Confirm both answer RPC:

```bash
bitcoin-cli   -regtest -datadir="$HOME/bitcoin-swap-regtest" getblockchaininfo
bitcoin-cli   -regtest -datadir="$HOME/blake-swap-regtest" getblockchaininfo   # BLAKE cli
```

The BLAKE CLI is a different binary from Bitcoin Core. Point the BLAKE
commands at the BLAKE build, for example
`$HOME/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli`.

---

## 5. Step 2 — Peer the two hosts and agree a checkpoint

Choose one host as the canonical hub for each chain. On the other host,
connect to it:

```bash
bitcoin-cli -regtest -datadir="$HOME/bitcoin-swap-regtest" \
  addnode "<hub-host>:19444" onetry

bitcoin-cli -regtest -datadir="$HOME/blake-swap-regtest" \
  addnode "<hub-host>:18444" onetry
```

Mine a few blocks on the hub so both sides share a chain, then record a
checkpoint block hash from the hub:

```bash
bitcoin-cli -regtest -datadir="$HOME/bitcoin-swap-regtest" -rpcwallet=blakeswap \
  generatetoaddress 101 "$(bitcoin-cli -regtest -datadir="$HOME/bitcoin-swap-regtest" \
  -rpcwallet=blakeswap getnewaddress)"

bitcoin-cli -regtest -datadir="$HOME/bitcoin-swap-regtest" getbestblockhash
```

Use the resulting hash as `BLAKE_SWAP_CANONICAL_BTC_CHECKPOINT` on both
hosts, and do the same for the BLAKE chain.

`scripts/blake-peer-health` verifies that your node is peered with the
hub and that the checkpoint is on your active chain. It needs these
variables, which you should add to each participant profile:

```bash
export BLAKE_SWAP_CANONICAL_HOST="<hub-host>"
export BLAKE_SWAP_CANONICAL_BTC_PORT=19444
export BLAKE_SWAP_CANONICAL_BLAKE_PORT=18444
export BLAKE_SWAP_CANONICAL_BTC_CHECKPOINT="<btc-checkpoint-blockhash>"
export BLAKE_SWAP_CANONICAL_BLAKE_CHECKPOINT="<blake-checkpoint-blockhash>"
```

---

## 6. Step 3 — Create the participant profile

Run this from a **clean shell** — no other participant profile may be
loaded. `setup-participant` refuses to run otherwise, deliberately, so
that wallet and state configuration cannot leak between participants.

On host A:

```bash
./scripts/setup-participant alice
```

On host B:

```bash
./scripts/setup-participant bob
```

`setup-participant` performs these steps and fails loudly if any fails:

1. checks that the Bitcoin and BLAKE CLIs exist and are executable;
2. checks that both node RPCs are reachable;
3. creates and loads the `blakeswap` wallet on both chains (creating it
   if it does not exist);
4. creates the state directory and the Nostr key directory with mode
   `700`;
5. generates a 32-byte participant Nostr secret at
   `$STATE_DIR/participant.key` with mode `600` (only if absent);
6. writes the participant profile to `scripts/participant-env` with
   mode `600`;
7. runs `scripts/blake-health` against the new profile.

Overridable settings (defaults shown):

| Variable | Default |
|---|---|
| `BLAKE_SWAP_SETUP_WALLET` | `blakeswap` |
| `BLAKE_SWAP_SETUP_STATE_DIR` | `$HOME/.blake-swap` |
| `BLAKE_SWAP_SETUP_PROFILE` | `<repo>/scripts/participant-env` |
| `BLAKE_SWAP_NOSTR_KEY_DIR` | `$STATE_DIR/nostr-keys` |
| `BLAKE_SWAP_NOSTR_RELAYS` | `wss://TheForest.nostr1.com,wss://nos.lol` |
| `BLAKE_SWAP_BTC_CLI` | `$HOME/bitcoin-core/build/bin/bitcoin-cli` |
| `BLAKE_SWAP_BTC_DATADIR` | `$HOME/bitcoin-swap-regtest` |
| `BLAKE_SWAP_BLAKE_CLI` | `$HOME/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli` |
| `BLAKE_SWAP_BLAKE_DATADIR` | `$HOME/blake-swap-regtest` |

If your node paths differ, export the `*_CLI` and `*_DATADIR` variables
**before** running `setup-participant` so the generated profile records
them.

`scripts/participant-env` is git-ignored and is generated per host. It
is not committed; do not copy one host's profile to the other.

---

## 7. Step 4 — Load the profile and verify health

In every new shell, load the profile first:

```bash
source scripts/participant-env
```

This prints `Loaded BLAKE Swap participant environment`. It exports the
role, wallet, state directory, key location, transport, relays, and
both chain CLI/datadir pairs.

Confirm what is loaded:

```bash
./scripts/show-swap-env
```

Check local prerequisites:

```bash
./scripts/blake-health scripts/participant-env
```

Expect `READY: local participant prerequisites are healthy`. This check
verifies the role, state directory, key format, relay configuration, and
RPC plus wallet usability on both chains. It explicitly does **not**
contact a relay.

Check cross-host synchronization:

```bash
./scripts/blake-peer-health scripts/participant-env
```

Expect `READY FOR TWO-HOST SWAPS`. This verifies that your node is
peered with the canonical hub on both chains and that the agreed
checkpoint is on your active chain. **Both hosts must report READY
before attempting a swap.**

Record each host's Nostr public key — it is printed by
`show-swap-env` and by any `scripts/blake` command:

```bash
./scripts/show-swap-env | grep 'nostr pub'
```

You will use these to identify the counterparty in the marketplace.

---

## 8. Step 5 — Fund the regtest wallets

Each host needs spendable regtest coins on **both** chains: BLAKE to
fund the BLAKE HTLC (or receive it), and BTC to fund the Bitcoin HTLC.

```bash
# Bitcoin
bitcoin-cli -regtest -datadir="$HOME/bitcoin-swap-regtest" -rpcwallet=blakeswap \
  generatetoaddress 101 \
  "$(bitcoin-cli -regtest -datadir="$HOME/bitcoin-swap-regtest" \
     -rpcwallet=blakeswap getnewaddress)"

# BLAKE (same commands, BLAKE cli and BLAKE datadir)
```

Mine at least 100 blocks so coinbase outputs are spendable on both
chains.

> **Note on auto-mining.** `BLAKE_SWAP_AUTO_MINE` defaults to `1`, which
> makes the engine mine one block locally after a funding broadcast. This
> is a regtest convenience. Leave it enabled for regtest reproduction and
> set `BLAKE_SWAP_AUTO_MINE=0` if you want to observe the unconfirmed
> paths explicitly (the pending-confirmation test suites do exactly
> this).

---

## 9. Step 6 — Run a swap

Both participants must have the profile loaded in their shell.

### 9.1 `blake-for-btc` (maker gives BLAKE, wants BTC)

This is the default direction. The maker is protocol **Alice**; the
taker is protocol **Bob**.

**Maker (host A):**

```bash
./scripts/blake market
```

Pick a price, then publish an offer:

```bash
./scripts/blake offer blake-for-btc --blake 1 --sats 370000 --minutes 30
```

This prints a 64-character offer ID. Share it with the taker.

Then drive coordination and settlement:

```bash
./scripts/blake next      # perform the one unambiguous coordination step
./scripts/blake fund      # REQUIRED: the explicit BLAKE funding commitment
./scripts/blake resume    # continue settlement
```

**Taker (host B):**

```bash
./scripts/blake market
./scripts/blake take <offer-id>
./scripts/blake next
./scripts/blake resume
```

### 9.2 `btc-for-blake` (maker gives BTC, wants BLAKE)

The maker is protocol **Bob** and the taker is protocol **Alice**. The
secret is therefore generated by the **taker**.

**Maker (host B):**

```bash
./scripts/blake offer btc-for-blake --sats 25000 --blake 10 --minutes 30
```

**Taker (host A):**

```bash
./scripts/blake take <offer-id>
./scripts/blake next      # imports the match and publishes the protocol offer
./scripts/blake fund
./scripts/blake resume
```

**Maker (host B) then:**

```bash
./scripts/blake next
./scripts/blake resume
```

### 9.3 The command vocabulary

`scripts/blake` is the primary operator interface:

| Command | Meaning |
|---|---|
| `market` | list open offers from Nostr |
| `offer <direction> --blake N --sats N --minutes N` | publish a market offer |
| `take <offer-id>` | take an open offer |
| `accept-take <offer-prefix>` | accept the only valid take on your offer |
| `accept-match [match-prefix]` | import the pending market match |
| `ignore-match <match-prefix>` | permanently ignore a match locally |
| `next` | perform the single unambiguous **non-economic** coordination step |
| `fund` | the explicit initial BLAKE funding commitment |
| `run [seconds]` | foreground supervisor: `next` + `resume-all` in a loop |
| `resume [swap-prefix]` | resume settlement for one live swap |
| `dismiss <swap-prefix>` | locally archive a provably unfunded swap |
| `swaps` | dashboard of active, attention, settled, archived swaps |
| `show <swap-id>` | raw local record |
| `reconcile <swap-id>` | read-only forensic reconciliation |
| `status` | check both chain nodes |
| `engine <args...>` | pass arguments straight to `swap_v2.py` |

### 9.4 Safety model

- `next` performs **only** unambiguous non-economic coordination. It
  ships a source-level assertion that it does not move funds.
- Initial BLAKE funding is **always** an explicit `fund` commitment. It
  is never automatic.
- `resume` and `resume-all` handle only post-funding settlement and
  recovery.
- Ambiguous IDs, multiple live choices, and multiple safe coordination
  steps all **fail closed**.
- `dismiss` is local archival only, and refuses any swap whose chain
  state cannot be proven to hold no funds.

---

## 10. Step 7 — Verify the result

After a successful swap, both sides should show a terminal state:

```bash
./scripts/blake swaps
```

Confirm independent, on-chain evidence rather than trusting the
messages:

- **Alice** should have a confirmed Bitcoin claim spending the Bitcoin
  HTLC, revealing the secret in its witness.
- **Bob** should have a confirmed BLAKE claim spending the BLAKE HTLC.
- Both transactions should be visible in the counterparty's node,
  which is why §5's checkpoint check matters.

Per-swap detail:

```bash
./scripts/blake show <swap-id>
./scripts/blake reconcile <swap-id>
```

`reconcile` is read-only and reports the derived protocol role, both
chain observations, remaining timeout heights, secret provenance, and
the single recommended next action.

---

## 11. Step 8 — Run unattended (optional)

For a long-running participant, run the supervisor under `nohup`:

```bash
./scripts/blake-service start scripts/participant-env 15
./scripts/blake-service status
./scripts/blake-service logs 200
./scripts/blake-service stop
```

`start` requires the profile on first use; `status`, `logs`, and `stop`
then remember it. State lives under `$BLAKE_SWAP_STATE_DIR/service`
(`blake-run.pid`, `blake-run.log`, `profile-path`).

The supervisor loop runs `next` followed by `resume-all`, suppresses
unchanged output, prints an `ACTION REQUIRED` prompt when an unfunded
swap is ready (because funding is never automatic), and pauses
coordination without stopping settlement if `next` errors.

Set `BLAKE_SWAP_RUNNER_UNIT` in the profile if you prefer a systemd
user unit; `blake-health` will then report on it.

---

## 12. Recovery and troubleshooting

| Symptom | Likely cause | Action |
|---|---|---|
| `no participant profile is loaded` | profile not sourced | `source scripts/participant-env` |
| `Bitcoin CLI not found` | wrong CLI path in profile | re-run `setup-participant` with `BLAKE_SWAP_BTC_CLI` exported |
| `RPC unavailable` | node not running or wrong datadir | start the node, check `getblockchaininfo` |
| `wallet is not loaded` | wallet missing in the node | re-run `setup-participant` |
| `NOT READY` from `blake-peer-health` | nodes not peered, or wrong checkpoint | re-check §5 |
| Offer visible but `next` does nothing | match older than the 60-minute auto window | import explicitly with `accept-match <match-prefix>` |
| `READY TO FUND` | the swap needs the explicit commitment | `./scripts/blake fund` |
| `MULTIPLE SAFE COORDINATION STEPS` | more than one unambiguous action | resolve them one at a time; the tool refuses to choose |
| `manual-attention` in reconcile | ambiguous or unsafe chain state | inspect `reconcile` output before acting |

General procedure when something looks wrong:

```bash
./scripts/blake status                 # are both chains reachable
./scripts/blake swaps                  # dashboard, read-only
./scripts/blake reconcile <swap-id>    # forensic, read-only
./scripts/blake resume                 # advance the one live swap
./scripts/blake next                   # only unambiguous coordination
```

`sync-funding` and `sync-settlement` reconstruct state directly from
confirmed chain history and are safe to run when local state is behind
the chain:

```bash
./scripts/blake engine sync-funding  <swap-id> --role alice
./scripts/blake engine sync-settlement <swap-id> --role alice
```

---

## 13. Running both participants on one host

For a single-machine dry run you must give each participant a distinct
state directory, wallet, and key, or they will collide. Create them
from clean shells:

```bash
# shell 1
export BLAKE_SWAP_SETUP_STATE_DIR="$HOME/.blake-swap-alice"
export BLAKE_SWAP_SETUP_WALLET=alice
export BLAKE_SWAP_SETUP_PROFILE="$PWD/scripts/alice-env"
./scripts/setup-participant alice

# shell 2
export BLAKE_SWAP_SETUP_STATE_DIR="$HOME/.blake-swap-bob"
export BLAKE_SWAP_SETUP_WALLET=bob
export BLAKE_SWAP_SETUP_PROFILE="$PWD/scripts/bob-env"
./scripts/setup-participant bob
```

`scripts/alice-env` is committed as a static example profile, but it
hardcodes one machine's `$HOME` layout; prefer generating profiles with
`setup-participant`.

The wrapper `scripts/swap` actively refuses to run when the role and the
state/key directories disagree (for example `role=alice` with a
`.blake-swap-bob` state path), because that exact mix-up occurred during
the original two-host smoke test.

To exercise the protocol without live nodes at all, use the FILE
transport and the integration harness described in §14.

---

## 14. Tests and diagnostics

The node-free unit suites run anywhere:

```bash
./scripts/test.sh --unit-only
```

The full suite requires reachable regtest nodes and an agreed node
layout. See `scripts/test.sh --help` and `tests/README.md` for the
suite list, prerequisite handling, and how to point the harness at your
nodes with `BLAKE_SWAP_BTC_CLI` / `BLAKE_SWAP_BLAKE_CLI` and the
matching datadirs.

Static diagnostics:

```bash
./scripts/stability-check        # syntax + safety-invariant source checks
python3 scripts/wire_inventory.py   # regenerate the wire message inventory
```

---

## 15. Safety summary

- Both hosts must run the **same** commit.
- Both hosts must pass `blake-health` and `blake-peer-health` before
  swapping.
- The participants must never share state, wallets, or keys.
- Initial BLAKE funding is always a deliberate human action.
- Never trust a coordination message in place of on-chain verification.
- This is experimental software on regtest.

**Do not use with valuable funds.**
