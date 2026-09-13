# BTC ↔ BLAKE Atomic Swap

Experimental peer-to-peer atomic swaps between Bitcoin and the BLAKE Bitcoin
fork using Bitcoin-style HTLCs and Nostr coordination.

**Status:** two-host regtest reference implementation, reproduced successfully
in both physical directions.

**Do not use with valuable funds.**

## What has been demonstrated

The reference implementation has completed BTC ↔ BLAKE atomic swaps across two
independently configured Linux hosts with:

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
