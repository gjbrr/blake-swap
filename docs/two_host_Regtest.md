Laptop / Alice
  ├─ Alice state
  ├─ Alice Nostr key
  ├─ Bitcoin regtest node
  └─ BLAKE regtest node

Start9 / Bob
  ├─ Bob state
  ├─ Bob Nostr key
  ├─ swap_v2.py
  ├─ vendored coincurve/websockets
  └─ SSH-backed bitcoin-cli wrappers
         ↓
       laptop nodes

Nostr:
  offer / accept

Chains:
  BLAKE funding
  BTC funding
  BTC claim + S revelation
  BLAKE claim
