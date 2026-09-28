# Recovering an HTLC after a wallet key loss

Signing a claim or a refund depends on the participant's wallet holding the
private keys behind the HTLC's addresses. A wallet restore, rename,
re-creation gone wrong, or a move to a new machine can lose them — and
nothing in the swap record says so. When that happens:

- the **refund** branch becomes unspendable, so an abandoned HTLC's funds are
  stuck; and
- the **claim** branch becomes unspendable, so a completed swap cannot be
  collected.

The funds are not lost. The HTLC's script is fully determined by the swap
record (the two participant keys, the hashlock and the timeout), and the
private keys are recoverable from your own backup. This document is the
procedure, and `tests/htlc_key_recovery_tests.py` performs it end to end on
regtest: it funds an HTLC to a key the wallet has **never held**, imports the
descriptor into a fresh wallet, and refunds from that wallet.

---

## 1. See what you have

```bash
scripts/blake engine recovery-plan <swap-id>
```

`scripts/blake` exposes a curated operator surface and passes everything else
through to the engine with `engine <command>`; the settlement and recovery
commands are deliberately not part of the curated list.

Read-only. It does not import, sign, fund or broadcast anything. For each
chain it reports:

- the HTLC **descriptor** and its checksum;
- the **address** the funds are at, and the recorded funding outpoint;
- which **branch** belongs to you (`claim` or `refund`) and which of your
  keys it uses;
- the **timeout height** the refund branch becomes spendable at;
- whether the current wallet can already spend it (`solvable` from
  `getaddressinfo`); and
- if it cannot, the exact import and spend steps.

If the plan cannot derive the local protocol role, the record is missing the
identities needed to say which branch is yours. That is worth fixing before
anything else.

## 2. Understand the two traps

Both are easy to get wrong and both fail in a way that looks like something
else.

**`getdescriptorinfo` returns the *public* form.** Call it on a descriptor
containing a private key and it reports `hasprivatekeys: true`, but the
`descriptor` field it returns has the key replaced by its public key. Turn
that around and import it, and Core refuses with *"Cannot import descriptor
without private keys to a wallet with private keys enabled"*. The
`checksum` field, however, **is** the checksum of the string you passed, so:

```bash
# checksum belongs to your private form
bitcoin-cli -rpcwallet=<wallet> getdescriptorinfo "$PRIVATE_DESCRIPTOR"

# import YOUR string with THAT checksum
bitcoin-cli -rpcwallet=<wallet> importdescriptors \
  "[{\"desc\":\"$PRIVATE_DESCRIPTOR#<checksum>\",\"timestamp\":\"now\"}]"
```

**`timestamp` is required, and it decides how much chain is scanned.**
`"now"` means no historical scan, which is what you want for an HTLC you are
about to spend. A number triggers a rescan from that time minus two hours,
and a single `"timestamp": 0` anywhere in a batch forces a **full** rescan of
the whole batch. `importdescriptors` is synchronous: it returns when its
rescan finishes, and a concurrent attempt fails with *"Wallet is currently
rescanning"*.

Two smaller ones:

- `deriveaddresses` (and therefore the recovery plan's address) needs a
  **checksummed** descriptor.
- In this engine `BLAKE_SWAP_BLAKE_WALLET` / `BLAKE_SWAP_BTC_WALLET` outrank
  `BLAKE_SWAP_CHAIN_WALLET`. If you are pointing a command at a recovered
  wallet, set the chain-specific variable (and the general one, for clarity),
  or the original wallet will be used and the spend will simply not
  finalize.

## 3. Rebuild the descriptor with your key

Take the descriptor from step 1 and replace **your own public key** with your
WIF (or extended private key). Leave the counterparty's public key alone. On
the BLAKE chain, for example, Alice's refund branch is the second clause:

```
wsh(or_i(and_v(v:pk(<BOB_PUB>),sha256(<H>)),
         and_v(v:after(<T>),pk(<ALICE_PUB>))))
                                       ^^^^^^^^^^^ replace this
```

Substituting a key changes the string, and therefore its checksum: always
re-run `getdescriptorinfo` on the substituted string.

## 4. Import and verify

```bash
bitcoin-cli -rpcwallet=<wallet> importdescriptors \
  "[{\"desc\":\"<private-descriptor>#<checksum>\",\"timestamp\":\"now\"}]"

bitcoin-cli -rpcwallet=<wallet> getaddressinfo <htlc-address>
# expect: "solvable": true, "ismine": true
```

A `warnings` entry of *"Not all private keys provided"* is expected: you hold
one branch's key, not both.

## 5. Spend

**Refund** — spendable at or after the timeout height. With the descriptor
imported, the wallet can sign it (again, `engine` reaches the command that is
not part of the curated wrapper surface):

```bash
scripts/blake engine refund-blake <swap-id>
scripts/blake engine refund-btc   <swap-id>
```

**Claim** — a P2WSH claim **cannot** be signed from a raw transaction over
RPC. The signing path needs the SHA256 preimage, and the node accepts it only
through a PSBT input field (`PSBT_IN_SHA256`, BIP174 type `0x0a`); there is no
RPC that injects a preimage into a PSBT. The engine already builds claims that
way, so use it rather than assembling one by hand:

```bash
scripts/blake engine claim-btc   <swap-id>
scripts/blake engine claim-blake <swap-id>
```

Both require `S`. If you do not have `S`, the claim branch cannot be spent by
anyone — but the refund branch still can, once the timeout is mature.

## 6. After a recovery

A recovered wallet holds the HTLC descriptor but not the rest of the swap's
addresses. Once the swap is settled or refunded, archive it
(`scripts/blake dismiss <swap-id>`) and return to your normal wallet. If you
recovered into a throwaway wallet, do not leave funds there.

---

## What is not yet automated

Importing each HTLC descriptor automatically at swap creation would make this
drill unnecessary: the wallet would never be in a position where it cannot
spend its own swap. It is planned, not implemented, and the constraints above
are why it needs care — the private form needs its own checksum, the
`timestamp` must not trigger a rescan, and Core v30 will not let one wallet mix
watch-only and private-key descriptors. See `docs/ROADMAP.md` §2.
