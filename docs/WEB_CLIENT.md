# The web client

The client is a browser participant: a person creates or unlocks a vault, sets an identity
and a node proxy, and settles a BLAKE/BTC swap against another participant without a
terminal. It is the same engine the CLI drives — the same protocol decisions, the same
records — with the document as its output instead of stdout.

This page is the operator's manual. What is implemented, what is not, and what to do when
something is wrong.

## Running it

```bash
make serve                 # build both wasm modules and the server, then serve
```

`make serve` delegates to `scripts/serve-dev.sh`, which reads the running rig's own
description and passes the server the four paths it needs:

```bash
scripts/test-nodes start           # starts both regtest nodes, writes ~/.blake-swap-test/rig-env.json
make serve                         # serves the app at http://127.0.0.1:8787/
```

With no rig running the app still serves; it reports both chains as unreachable and every
chain read refuses with the node's own message. That is a perfectly good way to work on the
page.

Options:

```bash
scripts/serve-dev.sh --addr 127.0.0.1:9000    # another port
scripts/serve-dev.sh --open                   # open a browser
scripts/serve-dev.sh --no-build               # serve what is already built
make serve ARGS="--open"                      # pass options through make
```

The edit loop is `make wasm` and a reload: the server sends `Cache-Control: no-store`, so a
rebuilt module is never served from cache. Only a change to `cmd/server` needs a restart.

The server binds to `127.0.0.1` by default and proxies only the JSON-RPC methods the client
needs. **Do not expose it on a public interface.** It holds the node credentials and has no
authentication, because it is meant to be the half of the app that runs on the operator's own
machine. A remote deployment needs a reverse proxy that authenticates the person and speaks
TLS, and the proxy should be given its own allowlist and rate limit.

## The flow, once

1. **Setup.** Create a vault with a passphrase. Everything the worker keeps — swap records,
   the Nostr key, the wallet names, the last opened swap — is sealed with a key derived from
   it before it reaches IndexedDB. Losing the passphrase loses the records; there is no
   recovery, and the page says so.
2. **Identity.** Either import a 64-hex Nostr secret, or generate one. A generated secret is
   shown **once**, in its own box, and never again: write it down then. After that the client
   shows only the public key.
3. **Proxy.** Leave the field empty and the client uses the origin it was served from, which
   is what makes `make serve` work with no setup. Set it explicitly only when the app is
   served somewhere other than the machine running the node proxy.
4. **Wallets.** One signing wallet and one watch-only wallet per chain, named as the node
   names them. The defaults are `blake`, `btc`, `blake-watch` and `btc-watch`. The client
   shows whether the node can see each one; a wallet the node does not have is the most
   common reason a funding refuses.
5. **Relays.** Where the market and coordination are read from. One public relay is the
   default; anything `ws://` or `wss://` is accepted.

## Making a swap

The client does not ask whether you are Alice or Bob. It derives the role from the record:
whoever signed the offer is Alice, whoever accepted it is Bob. The same vault and the same
wallets can be Alice in one swap and Bob in another, which is how two participants on one
node are actually configured.

**As Alice:**

1. Swaps → *Create a swap*: the two amounts. The deadlines are computed from both chain tips.
2. Open the swap, press **Publish offer**, then tell Bob the swap id.
3. **Receive acceptance** once Bob has accepted.
4. **Fund BLAKE**, then **Sync funding** after the transaction confirms.
5. **Receive BTC funding** once Bob has funded.
6. **Claim BTC**, then **Sync settlement** after it confirms.
7. **Receive BLAKE claimed**, then **Complete**.

**As Bob:**

1. Swaps → *Accept an offer somebody sent you*: Alice's swap id and her public key. Pinning
   her key is what makes this an acceptance of *her* offer. (For a market trade, the match
   row's *Import match* does the same thing with the terms already pinned.)
2. **Fund BTC**, then **Sync funding** after it confirms.
3. **Receive BTC claimed**, then **Extract secret** (Bob has no secret until Alice claims),
   then **Claim BLAKE**.
4. **Complete**.

Every action that moves money asks first, in a dialog that names the action, the swap, the
amounts, the counterparty and what will happen. Escape cancels it. Nothing is sent twice: a
second click while a command is running is ignored, and the progress line names the command
that is running.

Which buttons are enabled comes from the latest reconciliation, not from the state string.
The engine still refuses anything it would refuse — the page is not the security boundary —
but a stale view must not invite an action the engine is about to reject.

## After a reload

The vault starts locked, always. Unlocking restores the dashboard, the market snapshot and
the swap you were working on, from the sealed store. Nothing is kept in `localStorage`,
`sessionStorage` or a plaintext file to make that quicker.

If the same swap is being worked on by a second browser tab, one of them will hold a stale
picture. The *Refresh* button re-reads; the engine refuses anything already done.

## When something is wrong

- **"no proxy address"** — the proxy field is empty and the page was not served by the app
  server. Set it, or use `make serve`.
- **"Requested wallet does not exist or is not loaded"** — a wallet name the node does not
  have. Setup → *Wallet status* says which.
- **"SECURITY FAILURE: current Nostr identity is not pinned to this swap"** — the record was
  negotiated by a different key than the one in the vault. The vault holds one identity; a
  second identity means a second vault.
- **A relay error in the market** — the relay did not answer. The scan line says how many
  relays answered; the error is the relay's own.
- **The page stops answering** — a wasm instance that panicked is dead. Reload; the records
  are sealed and survive. `data-last-command` and `data-command-log` on the document say what
  was running.

## Recovery

The swap view has **Recovery plan**, which prints the compiled descriptor, the address, the
amount, the timelock and this participant's own keys for each leg, and whether a loaded
wallet can spend the output. If nothing on the machine can spend it, the dashboard says so
above every other row: *HTLCs NOT SOLVABLE BY A LOADED WALLET*. `Watch HTLC` imports the
descriptor into the watch-only wallet, and the CLI's `recovery-plan` prints the same facts
for a host-side operator.

## What is not implemented

**The reverse market direction.** The browser completes a market trade in the
`blake-for-btc` direction: take an offer, accept the take as the maker, import a match as the
taker, and withdraw an offer. In the reverse direction (`btc-for-blake`, where the maker is
protocol Bob) the taker's half is available too: importing a reverse match creates the secret
as protocol Alice. The maker's two steps — accepting a reverse take, which publishes the
reverse match and reserves the swap id, and importing Alice's protocol offer afterwards —
are still CLI-only, so a browser participant is a complete reverse *taker* and needs the CLI
to be the reverse *maker*.

`market.ignore-match` is also CLI-only.

Also not implemented: exporting the sealed vault (a stretch goal — only the sealed blob would
leave the browser), wallet creation from the page, and any activity log.
