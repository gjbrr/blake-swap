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

1. The two independent implementations in `tools/minimal_*.py` must gain
   the same mode, or the cross-implementation evidence lapses for
   encrypted runs. The existing compatibility evidence is plaintext, and
   `tests/swap_encrypted_coordination_tests.py` covers this engine only.
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

**Status: planned, after the recovery procedure.**

The participant wallet currently holds the private keys behind the
addresses used for HTLC claim and refund. A wallet restore that loses those
keys makes both the claim and the refund impossible. Signing depends on
them being present.

The plan is to import each swap's HTLC descriptor as watch-only, with the
private keys where the participant holds them, so a restored wallet can
still spend. Two facts constrain how this has to be done, and both were
verified against Bitcoin Core:

- The engine already asks the node to compile the policy
  (`wsh(or_i(and_v(v:pk(A),sha256(H)),and_v(v:after(T),pk(B))))`), so the
  funded script is the node's Miniscript compilation. The descriptor is
  therefore the source of truth and matches the output by construction.
- `importdescriptors` requires the checksum returned by
  `getdescriptorinfo` **on the exact string being imported**. Substituting
  the participant's WIF for their own pubkey changes the checksum, so the
  private-key form must be checksummed separately.
- Import must use `"timestamp": "now"` for a descriptor that has not been
  funded yet, to avoid a chain rescan. Note that one `"timestamp": 0` item
  in a batch forces a full rescan for the whole batch.
- An HTLC **claim** cannot be signed from a raw transaction over RPC: the
  signing path needs the SHA256 preimage, which Core only accepts through a
  PSBT input field (`PSBT_IN_SHA256`, BIP174 type `0x0a`). The engine
  already constructs claims as PSBTs with the preimage injected, which is
  why this is a documentation and import problem rather than a signing
  rewrite. Refunds sign normally from a raw transaction.

## 3. Public-network observation and scanning

**Status: in progress.**

Recovery currently establishes funding from the wallet and from bounded
block scans. Two changes are being made:

- Funding confirmation is derived from the funding transaction and its
  block hash, so recovery of an older record no longer depends on the HTLC
  still being unspent in the current UTXO set (`gettxout` remains a fast
  path).
- A missing scan anchor is a hard error, matching the posture the spender
  scan already takes, rather than silently degrading to a scan from
  genesis. An explicit opt-in remains for legacy state that predates
  anchors.

Still deferred, for when the BLAKE chain is run as a public network:

- Known-outpoint and transaction-indexed observation instead of
  verbosity-2 block scans. `docs/PERFORMANCE_AND_RECOVERY.md` describes
  this as the intended production design.
- A review of fee policy against live public mempools, and of the RPC
  timeout and retry posture against a node that is merely slow rather than
  down.

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
| Engine internals duplication | Several blocks are duplicated between commands (the recorded-settlement block, the glob/load/report block, the atomic-write pattern, the chain dispatch tables). De-duplicating them is a readability change with regression risk, so it is sequenced last. |
| `run_chain_cli` response parsing | A node that prints anything before its JSON is treated as "not JSON" and the raw text is returned. Parsing structurally, or failing closed, is a small follow-up. |

## 6. Independent implementations

`tools/minimal_*.py` are separate programs that speak the wire protocol.
They exist so that the compatibility suites test a second implementation
rather than the engine against itself. Any wire change must be reflected
there in the same commit, or the evidence they provide stops meaning
anything.
