# Re-pointing the swap at the local wallet

The CLI and the rig run against a node that is also its own wallet. The browser has no such
node: the keys are in tinywallet, and every `callWithWallet` path refuses in that build. This is
the map of what the three flows actually call, what answers it now, and what still has no local
equivalent.

`pkg/localwallet` is the second implementation of the engine's node surface. Chain reads go to
the node (the light client is the chain read); wallet operations go to the local wallet over its
channel; the rest refuse by name through `pkg/walletdep`.

## The map

Wallet-scoped on `chain.Wallet` (must be local or refused):

| method | used by | now |
| --- | --- | --- |
| `SendToAddress` | fund | local: `domain.Send` (build, sign, broadcast) |
| `Balances` | fund preflight | local: sum of the chain's own coins |
| `ListTransactions` | fund recovery | local: the wallet's receive entries (a send journal is still needed for full recovery) |
| `Tx` | fund locate/receipt | local: the wallet's raw hex for what it sent |
| `Decode` | fund settle | node: `decoderawtransaction` is not a wallet call |
| `AddressInfo` | engine key naming | local: the next address and its pubkey |
| `NewAddress` | fund | local: the next receive address |
| `ImportDescriptor` | fund watch-only | refused: the local watch set replaces it |
| `ProcessPSBT` | settle | local: `domain.SignPSBT` (P2WPKH; P2WSH HTLC inputs next) |

Node-wide on the three `Node` interfaces (stay on the node):

`Height` (wallet tip for the engine), `BlockHash`, `BlockTxs`, `RawTx`, `TxOut`, `DecodeRaw`,
`EstimateSmartFee`, `MempoolInfo`, `CreateRawTransaction`, `CreateRawTransactionAt`,
`ConvertToPSBT`, `UtxoUpdatePSBT`, `FinalizePSBT`, `SendRaw`, `InMempool`, `MineToAddress`
(rig only).

Refused by name (no local equivalent):

`ListWallets`, `EnsureWallet`, `GetWalletInfo`, `ChainInfo`, `DescriptorInfo`,
`DeriveAddresses`, `ImportDescriptor`, `WalletTxIn`.

The last two of the local derivations replace their node counterparts: a contract is derived
from its terms through `domain.DeriveHTLC` (pinned to the node's compiled vector), and a
contract is watched through `domain.WatchHTLC`, so neither `getdescriptorinfo` nor
`importdescriptors` is needed.

## The fork rule the BLAKE leg depends on

At the fork there are no new coins. Every UTXO that existed then exists on both chains and is
Bitcoin; only the coinbases mined after it, and what spends them, are BLAKE. A BLAKE swap funded
from a pre-fork coin would exchange Bitcoin for Bitcoin. `wallet.Profile.ForkHeight` records the
boundary, `domain.SpendableOn` lists only a chain's own money, and `domain.Send` refuses a
pre-fork coin with `ErrNotNative`. The height is a deployment parameter: zero is right for the
regtest rig, whose chains diverge at genesis, and must be set for mainnet BLAKE.

## What remains

1. A channel client in this repo (or tinywallet importing the flows) that implements
   `localwallet.Wallet` over the wallet worker, so `WalletTx` can be answered from the wallet's
   own send journal.
2. P2WSH signing for HTLC claim and refund. The engine's own `AddressInfo` doc decides the key:
   the contract names the wallet's address pubkey, so the wallet signs with a key it already
   derives — no key is passed over the channel.
3. The two-chain rig proof: fund and settle an HTLC with this adapter, no wallet RPC.
