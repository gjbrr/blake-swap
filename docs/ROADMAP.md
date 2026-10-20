# Roadmap and deferred work

This document records work that is **known, scoped, and not yet done**, so
that it is visible to operators and reviewers instead of living in
someone's head. It is not a promise of dates.

The current state is otherwise described by `README.md`, `docs/PROTOCOL.md`,
`docs/WIRE_PROTOCOL.md`, `docs/PERFORMANCE_AND_RECOVERY.md`, and
`tests/README.md`. `tests/docs_alignment_tests.py` fails if this set of
documents drifts from the implementation.

---

## 1. Coordination confidentiality (optional NIP-44)

**Status: implemented for the reference engine; not yet in the independent
implementations.**

The default transport is plaintext Nostr kind-1 events, and that is
documented rather than assumed: the protocol does not depend on transport
confidentiality, because the secret `S` is never transported and every
claim taken from a message is re-verified against a chain.

`BLAKE_SWAP_NOSTR_ENCRYPTION=nip44_v2`, set on both participants, encrypts
settlement content as well:

- `src/nip44.py` implements NIP-44 v2 (secp256k1 ECDH → HKDF-Extract with
  the fixed `nip44-v2` salt → HKDF-Expand with the nonce → ChaCha20 →
  HMAC-SHA256), verified against the official vectors rather than against
  its own output: `tests/nip44_tests.py`.
- Settlement messages other than `offer` carry a signed
  `["encryption", "nip44_v2"]` tag and a base64 payload as `content`. The
  tag is inside the signature, so a relay cannot strip it.
- The `offer` stays plaintext in both directions: it is first contact, the
  maker does not yet know the taker, and NIP-44 is pairwise. It is also
  public coordination data by nature.
- Refusals run in both directions: an encrypted participant refuses a
  plaintext message of an encryptable type (a downgrade), and a plaintext
  participant refuses content it cannot read. A failed decryption never
  falls back to parsing the content as plaintext.

What remains before it can be described as generally deployable:

1. The cross-implementation half now exists for the codec and the event:
   `tools/minimal_nip44.py` is a second, standalone NIP-44 v2
   implementation that matches the official vectors on its own, and
   `tests/nip44_tests.py` checks that it decrypts what the engine
   encrypts, that the engine decrypts what the tool encrypts, and that the
   engine verifies, decrypts and parses a coordination event the tool
   signed. The remaining gap is narrower than it was: the `tools/minimal_*`
   message producers still write *files*, and the file transport has no
   encryption layer, so encrypted end-to-end evidence through `tools/`
   would mean giving those producers a Nostr path rather than a codec.
2. A `nip44`-native peer will encrypt *every* message it can address,
   including an `offer` it has already seen a pubkey for. This engine
   decrypts an encrypted offer when it can derive the conversation key, but
   the interop matrix with such a peer has not been exercised.
3. The payload carries no protocol domain separation beyond the
   conversation key: NIP-44 authenticates the nonce and ciphertext only.
   Binding the payload to the swap (`type`, `swap_id`, a monotonic step) is
   done by the message content itself, which is validated after
   decryption; a shared key used in another protocol would still admit
   cross-protocol replay, so the same identity keys should not be reused
   for unrelated NIP-44 traffic.
4. Encryption protects `content` only. Publisher identity, timing, message
   existence, frequency and approximate size remain visible to relays, and
   there is no forward secrecy: the conversation key is a long-lived shared
   secret. Hiding participants or timing needs gift-wrap with a fresh key
   per message and authenticated relays, which this project does not do.

## 2. Watch-only HTLC descriptor import

**Status: implemented for the public (watch-only) form; the private-key
import remains a manual procedure by design.**

Each chain's HTLC is imported into a dedicated watch-only wallet before that
chain is funded (`report_watch_only_import()`, and explicitly via
`scripts/blake engine watch-htlc <swap-id>`), and `dashboard` reports a live
swap whose HTLC no loaded wallet can solve. The wallet is created with
`disable_private_keys` and `blank` set, so it holds no key of its own and
cannot be confused with the signing wallet; the descriptor is the node's
canonical public form, which already carries the checksum `importdescriptors`
requires.

Decided: **a wallet of its own per chain**, not the participant's, because
Core refuses to mix a watch-only descriptor into a wallet with private keys
enabled, and because the participant wallet's keys are exactly what a key loss
destroys.

The manual procedure remains for the case it exists for: a wallet that has
*already* lost the keys needs the private form, with its own checksum, and
`docs/KEY_RECOVERY.md` is that drill. `tests/htlc_key_recovery_tests.py`
performs both halves end to end on regtest.

Three constraints confirmed against the node, and the reason the automatic
half imports the public form only:

- `getdescriptorinfo` returns the **public** canonical form even when the input
  carried a private key, while its `checksum` belongs to the input string. A
  private-key import must therefore checksum its own string; importing the
  returned `descriptor` fails with "Cannot import descriptor without private
  keys to a wallet with private keys enabled", and appending the checksum to an
  already-checksummed descriptor fails with "Multiple '#' symbols".
- `deriveaddresses` requires a checksummed descriptor.
- `BLAKE_SWAP_BLAKE_WALLET` / `BLAKE_SWAP_BTC_WALLET` outrank
  `BLAKE_SWAP_CHAIN_WALLET`, so pointing a command at a recovered wallet means
  setting the chain-specific variable.

Not done, and deliberately: importing at the moment of *funding discovery*
rather than before funding (a swap funded by the counterparty before this
build existed is reported by `dashboard` and can be recorded with the explicit
`watch-htlc` command), and rescanning the chain for an HTLC funded before its
descriptor was imported. `"timestamp": "now"` is correct for the normal order;
a historical rescan is a different, much heavier operation.

Already established: the engine asks the node to compile the policy
(`wsh(or_i(and_v(v:pk(A),sha256(H)),and_v(v:after(T),pk(B))))`), so the funded
script is the node's Miniscript compilation and the descriptor matches the
output by construction — which is why the watch-only copy can be built from the
swap record alone, with no chain lookup. An HTLC **claim** also cannot be
signed from a raw transaction over RPC at all, because Core accepts the SHA256
preimage only through a PSBT input field (`PSBT_IN_SHA256`); the engine already
injects it into a PSBT, so spending a recovered HTLC uses an existing path.

## 3. Public-network observation and scanning

**Status: the observation path is implemented; validation against a live
public network is not.**

Funding confirmation is derived from the funding transaction and its block
hash, so recovering an older record no longer depends on the HTLC still being
unspent in the current UTXO set (`gettxout` remains a fast path), and a missing
scan anchor is a hard error rather than a silent scan from genesis, with
`BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN=1` as the explicit legacy opt-in.

Known-outpoint observation is now the path `sync-funding` takes: the HTLC
descriptor imported before funding (see §2) means one `listunspent` for the
HTLC's own address finds the output, and the watch-only wallet's
`gettransaction` supplies the block hash. No block scan and no transaction
index — the wallet holds the script, so a pruned or non-indexed node is enough.
`observe_funding_from_watch_wallet()` is that path, and it sits between the
recorded-outpoint fast path and the bounded scan. It was verified on the rig by
capping the fallback scan at one block, so a run that could not observe would
have to refuse rather than quietly scan.

The bounded verbosity-2 scan stays as the fallback for a swap funded before the
descriptor was imported, and for an output already spent before this
participant looked — a spent HTLC is not in `listunspent`. Both are bounded by
the recorded anchor.

The fee policy has been reviewed against real fee-market history rather than
against a live node: mempool.space publishes per-block feerate percentiles, and
`docs/PERFORMANCE_AND_RECOVERY.md` now carries the measured settlement vsizes,
the cap arithmetic, and the minimum viable swap size for each historical fee
environment. The finding is that the percentage cap, not the estimator, decides
whether a small swap can settle — a documented constraint now, not an open
question.

Still open, and needing a *failing* node rather than a public one: the RPC
timeout and retry posture against a node that is merely slow rather than down.
That wants a node that can be made to stall, which the regtest rig can do.

**That is now implemented and verified against a node that stalls rather than
fails.** A read-only call that times out, or that fails with a connection errno
that means "not reachable right now" (`ECONNREFUSED`, `ECONNRESET`,
`ETIMEDOUT`, `EPIPE`, …), is retried up to `BLAKE_SWAP_RPC_MAX_ATTEMPTS`
(default 3) with an exponential backoff from
`BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS` (default 1 s, doubling, capped by
`BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS` at 30 s). A state-changing call is
never retried: a call that timed out may have succeeded, so repeating
`sendtoaddress` could fund a swap twice, and the timeout message says so rather
than leaving the operator to guess which side of the ambiguity the engine chose.
The retry set is therefore an allowlist of reads and pure local transformations
(`RETRYABLE_RPC_METHODS`); a newly added mutating call gets no retry until
someone classifies it. `tests/chain_cli_transport_tests.py` holds the evidence,
including a run against a wrapper around the *real* `bitcoin-cli` that sleeps
past the timeout on its first call only: the retry recovers, and the same stall
with retries disabled must fail — so the first half cannot pass because the
stall never happened.

## 4. Wire compatibility and legacy messages

**Status: maintained until retired.**

The engine still parses and can construct the v1 market messages
(versionless offers and the v1 `market-take` shape). This is preserved for
compatibility with the earlier checkpoint. Retirement criteria, in order:

1. No v1 offer has been observed on the configured relays for a full offer
   lifetime.
2. The independent implementations no longer emit v1.
3. The compatibility suites can be reduced to a parse-only path.

Until then, the v1 construction path stays covered by tests rather than
deleted.

## 5. Known cosmetic and hygiene items

| Item | Detail |
|---|---|
| Dashboard status labels | `WAITING FOR ALICE` / `WAITING FOR BOB` are protocol-role labels and are correct, but they read as names rather than roles. Renaming them would change an operator-visible string, so it is deferred rather than done quietly. |
| Unreachable defensive raises | Three `raise` statements cannot currently be reached because an earlier equivalent check fires first (the wire integer type guards, one settlement state dispatch, and the runner's allowlist mismatch). They are kept deliberately as restatements of safety invariants, and are annotated where they live. |
| ~~Engine internals duplication~~ | **Done.** The recorded-settlement block, the glob/load/report block, the atomic-write pattern and the chain dispatch tables each have one implementation now. Six further state writers that had no crash protection at all were found and fixed at the same time, because the original inventory had counted only the writers that already used a temp file. |
| ~~`run_chain_cli` response parsing~~ | **Done.** A reply that does not parse is re-decoded from its first `{`/`[`, so a warning prefixed to the JSON no longer hides it. Genuinely non-JSON text still passes through. |

## 6. Independent implementations

`tools/minimal_*.py` are separate programs that speak the wire protocol.
They exist so that the compatibility suites test a second implementation
rather than the engine against itself. Any wire change must be reflected
there in the same commit, or the evidence they provide stops meaning
anything.

## The market accept path

Implemented, for the direction a browser participant needs first. The state machine now
lives in `pkg/marketops`, which both front ends call:

- `AcceptTake` turns a signed take into a swap, writes the two selection records durably
  *before* announcing anything, publishes the protocol offer and the market match, and
  treats a repeat as "already accepted" rather than as a second swap;
- `AcceptMatch` is the taker's side: validate the match against the offer and take it names,
  create the swap from the offer with the market's terms pinned, and bind the negotiation to
  it — repairing a missing binding rather than refusing to work;
- `PublishMatch`, `ReadTakeSet`, `FetchEvent` and the market evidence read are shared too.

The CLI keeps its output byte-identical by rendering from the same operations: the
`TestCLIParityMarketWalk` comparison against the Python reference still passes, and the
selection/binding tests moved into `pkg/marketops` with their assertions intact.

The worker exposes `market.takes`, `market.matches`, `market.accept-take`,
`market.accept-match` and `market.cancel`, so the Market screen now lists the takes on an
offer, accepts one, imports a match, and withdraws an offer. `scripts/verify-browser.py`
proves the handshake against a CLI taker: publish, take, list, accept, publish the match, and
import it on the taker's side.

In the **reverse direction** (`btc-for-blake`, where the maker is protocol Bob) both sides
are in the browser. `AcceptTake` dispatches to the reverse branch after the same canonical
validation, so `market.accept-take` reserves the swap id and publishes the match without
creating a swap or a secret; `market.accept-reverse-match` is the taker's import, where the
secret is created; and `market.accept-reverse-offer` is the maker's second step, importing
the taker's protocol offer and sending the accept, including the crash window where the keys
are durable and the message never left. `market.ignore-match` tombstones a match locally in
the sealed vault, and the browser's match list honours it.

All of those are `pkg/marketops` operations shared with the CLI, whose output stays
byte-identical: `TestCLIParityMarketWalk` covers the reverse negotiation and the tombstone,
and `scripts/verify-browser.py` runs the reverse direction end to end with the browser as the
maker.
