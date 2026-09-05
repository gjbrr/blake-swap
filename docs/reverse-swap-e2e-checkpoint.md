# Reverse Swap E2E Checkpoint

Successful swap:

- swap_id: 736fdecc2086
- direction: btc-for-blake
- amount: 10 BLAKE
- amount: 25,000 sats
- market maker: physical laptop participant
- market maker protocol role: Bob
- market taker: physical Start9 participant
- market taker protocol role: Alice

Verified end-to-end:

- reverse market match imported
- protocol Alice generated S
- protocol Bob never received S through Nostr
- ordinary protocol offer/accept completed
- Alice funded BLAKE
- Bob independently verified BLAKE HTLC
- Bob funded Bitcoin
- Alice independently verified Bitcoin HTLC
- Alice claimed Bitcoin and revealed S on-chain
- Bob extracted S from the Bitcoin witness
- Bob claimed BLAKE
- both settlement directions work with dynamically assigned roles

Infrastructure used:

- independent Bitcoin nodes
- independent BLAKE nodes
- Nostr coordination
- TheForest + nos.lol relays
- separate participant state directories and Nostr identities

Notable cleanup discovered:

- distinguish physical participant identity from protocol Alice/Bob role
- remove remaining default-role assumptions from CLI commands
- make Start9 PYTHONPATH/vendor setup persistent
- make Start9 environment configuration persistent
- provide first-class timeout refresh before funding
