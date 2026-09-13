# Two-Host Regtest Reproduction Guide

This is the current operator guide for reproducing the BTC ↔ BLAKE atomic-swap
reference implementation on two independent Linux hosts.

The procedure was clean-room tested in September 2026 with:

- an HP EliteBook participant,
- a Lenovo IdeaPad participant,
- independent Bitcoin regtest nodes,
- independent BLAKE regtest nodes,
- Nostr coordination,
- separate participant wallets, state directories, and signing identities.

Both market directions completed successfully.

This remains experimental software.

**Do not use valuable funds.**

---

## 1. Reference checkpoint

The clean-room reproduction was performed from:

```text
3ac62777eb3e47a5ed99f7000f4f4015a0a6c318



