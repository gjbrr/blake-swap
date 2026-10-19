//go:build !js

package rigsuites

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// Parity: tests/swap_protocol_tests.py
//
// The reference's protocol suite is the base harness the settlement, reorg and
// recovery suites build on: it walks the happy path, the abortive path, the
// in-flight states, the replayed and tampered messages, and the actions a
// participant must not be able to perform in the wrong role.
//
// The Go translation keeps each scenario and its decisive invariant. Where the
// reference also prints and re-asserts intermediate lines, the assertions kept
// here are the ones that would catch a protocol error: the durable state, the
// message that was or was not produced, the transaction that did or did not move,
// and the refusal vocabulary. Nothing is skipped when the rig is present.
func TestProtocolSuite(t *testing.T) {
	r := rigtest.Load(t)

	t.Run("happy path", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		// Nothing about the secret may cross before the Bitcoin claim: that
		// ordering is the whole safety argument of the protocol.
		rigtest.AssertFieldPresent(t, alice, id, "secret_hex")
		rigtest.AssertFieldAbsent(t, bob, id, "secret_hex")
		rigtest.AssertFieldAbsent(t, bob, id, "revealed_secret_hex")

		r.FundBothLegs(t, alice, bob, messages, id)

		r.Run(t, alice, messages, "claim-btc", id)
		r.Run(t, alice, messages, "publish-btc-claimed", id)
		r.Run(t, bob, messages, "receive-btc-claimed", id)

		// Bob's only route to the secret is the Bitcoin chain.
		r.Run(t, bob, messages, "extract-secret", id)

		rigtest.AssertFieldPresent(t, bob, id, "revealed_secret_hex")

		secret, _ := rigtest.RecordField(t, alice, id, "secret_hex")
		revealed, _ := rigtest.RecordField(t, bob, id, "revealed_secret_hex")

		if fmt.Sprint(secret) != fmt.Sprint(revealed) {
			t.Fatalf("Bob's revealed secret %v is not Alice's %v", revealed, secret)
		}

		r.Run(t, bob, messages, "claim-blake", id)
		r.Run(t, bob, messages, "publish-blake-claimed", id)
		r.Run(t, alice, messages, "receive-blake-claimed", id)
		r.Run(t, alice, messages, "complete", id)

		r.AssertState(t, alice, messages, id, "COMPLETE")
		r.AssertState(t, bob, messages, id, "BLAKE_CLAIMED")

		// Both HTLCs are spent by the claim branch, which is the only evidence
		// that would survive a stolen record.
		for _, chain := range []string{"blake", "btc"} {
			txid, vout := fundingPoint(t, r, alice, id, chain)

			if utxo := r.Node(t, chain, "gettxout", txid, fmt.Sprint(vout)); strings.TrimSpace(utxo) != "" {
				t.Fatalf("the %s HTLC is still unspent after a completed swap", chain)
			}
		}
	})

	t.Run("refund both legs", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		// Bob refunds Bitcoin once its CLTV is reached, which the node enforces:
		// a non-final transaction is refused rather than reported as a protocol
		// error, so the deadline is mined the way an operator would.
		r.MineTo(t, "btc", bob.Role, r.FieldInt(t, alice, messages, id, "btc_timeout"))

		r.Run(t, bob, messages, "refund-btc", id)
		r.Run(t, bob, messages, "publish-btc-refunded", id)
		r.Run(t, alice, messages, "receive-btc-refunded", id)

		r.AssertState(t, bob, messages, id, "BTC_REFUNDED")

		r.MineTo(t, "blake", alice.Role, r.FieldInt(t, alice, messages, id, "blake_timeout"))

		r.Run(t, alice, messages, "refund-blake", id)

		r.AssertState(t, alice, messages, id, "BLAKE_REFUNDED")

		// The secret never moved: neither record holds a revealed secret.
		rigtest.AssertFieldAbsent(t, bob, id, "revealed_secret_hex")
		rigtest.AssertFieldAbsent(t, alice, id, "revealed_secret_hex")
	})

	t.Run("timeout refresh", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		r.FundBothLegs(t, alice, bob, messages, id)

		beforeBTC := r.FieldInt(t, alice, messages, id, "btc_timeout")
		beforeBlake := r.FieldInt(t, alice, messages, id, "blake_timeout")

		// Time passes on both chains. A refresh must keep the ordering invariant:
		// Bitcoin's deadline has to leave a budget for Bob's BLAKE claim after it,
		// and BLAKE's must stay after it.
		r.Mine(t, "btc", alice.Role, 40)
		r.Mine(t, "blake", alice.Role, 40)

		r.Run(t, alice, messages, "refresh-timeouts", id)

		afterBTC := r.FieldInt(t, alice, messages, id, "btc_timeout")
		afterBlake := r.FieldInt(t, alice, messages, id, "blake_timeout")

		if afterBTC < beforeBTC {
			t.Fatalf("a refresh moved Bitcoin's deadline backwards: %d -> %d",
				beforeBTC, afterBTC)
		}

		if afterBlake < beforeBlake {
			t.Fatalf("a refresh moved BLAKE's deadline backwards: %d -> %d",
				beforeBlake, afterBlake)
		}

		if afterBlake-afterBTC != beforeBlake-beforeBTC {
			t.Fatalf("the BLAKE-minus-Bitcoin window changed across a refresh: "+
				"%d -> %d", beforeBlake-beforeBTC, afterBlake-afterBTC)
		}
	})

	t.Run("recovery from chain evidence", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		// Alice funds BLAKE and the announcement is lost: Bob's record still says
		// OFFERED, and only the chain can move it.
		r.Run(t, alice, messages, "fund-blake", id)

		r.AssertState(t, bob, messages, id, "OFFERED")

		r.Run(t, bob, messages, "sync-funding", id)

		r.AssertState(t, bob, messages, id, "BLAKE_FUNDED")

		// The same for Bitcoin, from Alice's side.
		r.Run(t, bob, messages, "fund-btc", id)
		r.Run(t, alice, messages, "sync-funding", id)

		r.AssertState(t, alice, messages, id, "BTC_FUNDED")
	})

	t.Run("wrong role economic actions", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		bobBefore := r.Snapshot(t, bob, id)
		aliceBefore := r.Snapshot(t, alice, id)

		blakeBefore := r.Mempool(t, "blake")
		btcBefore := r.Mempool(t, "btc")

		// Bob is protocol Bob: BLAKE funding is Alice's commitment, and the refusal
		// must say so rather than fail on a missing input.
		r.Expect(t, bob, messages, "protocol requires alice", "fund-blake", id)

		r.AssertSnapshotUnchanged(t, bob, id, bobBefore, "Bob's refused BLAKE funding")
		r.AssertMempoolUnchanged(t, "blake", blakeBefore, "Bob's refused BLAKE funding")

		// And the mirror: Bitcoin funding is Bob's job.
		r.Expect(t, alice, messages, "protocol requires bob", "fund-btc", id)

		r.AssertSnapshotUnchanged(t, alice, id, aliceBefore, "Alice's refused Bitcoin funding")
		r.AssertMempoolUnchanged(t, "btc", btcBefore, "Alice's refused Bitcoin funding")

		// A BLAKE-funded announcement for Bob is not Alice's to import.
		r.Run(t, alice, messages, "fund-blake", id)
		r.Run(t, alice, messages, "publish-blake-funded", id)

		r.Expect(t, alice, messages, "", "receive-blake-funded", id)
	})

	t.Run("cross-swap funding replay", func(t *testing.T) {
		messages := t.TempDir()

		alice := rigtest.NewSide(t, "go", "alice")
		bob := rigtest.NewSide(t, "go", "bob")

		other := r.Handshake(t, alice, bob, messages)
		id := r.Handshake(t, alice, bob, messages)

		r.Run(t, alice, messages, "fund-blake", other)
		r.Run(t, alice, messages, "publish-blake-funded", other)

		// The announcement is authentic and signed by Alice; it is simply about a
		// different swap. A receiver that imported it would bind Bob's funding
		// decision to a swap he never agreed to.
		before := r.Snapshot(t, bob, id)

		r.Expect(t, bob, messages, "", "receive-blake-funded", id)

		r.AssertSnapshotUnchanged(t, bob, id, before, "a cross-swap funding replay")
	})

	t.Run("cross-swap claim replay", func(t *testing.T) {
		messages := t.TempDir()

		alice := rigtest.NewSide(t, "go", "alice")
		bob := rigtest.NewSide(t, "go", "bob")

		other := r.Handshake(t, alice, bob, messages)
		id := r.Handshake(t, alice, bob, messages)

		r.FundBothLegs(t, alice, bob, messages, other)

		r.Run(t, alice, messages, "claim-btc", other)
		r.Run(t, alice, messages, "publish-btc-claimed", other)

		before := r.Snapshot(t, bob, id)

		r.Expect(t, bob, messages, "", "receive-btc-claimed", id)

		r.AssertSnapshotUnchanged(t, bob, id, before, "a cross-swap claim replay")
	})

	t.Run("cross-swap refund replay", func(t *testing.T) {
		messages := t.TempDir()

		alice := rigtest.NewSide(t, "go", "alice")
		bob := rigtest.NewSide(t, "go", "bob")

		other := r.Handshake(t, alice, bob, messages)
		id := r.Handshake(t, alice, bob, messages)

		r.FundBothLegs(t, alice, bob, messages, other)
		r.MineTo(t, "btc", bob.Role, r.FieldInt(t, alice, messages, other, "btc_timeout"))

		r.Run(t, bob, messages, "refund-btc", other)
		r.Run(t, bob, messages, "publish-btc-refunded", other)

		before := r.Snapshot(t, alice, id)

		r.Expect(t, alice, messages, "", "receive-btc-refunded", id)

		r.AssertSnapshotUnchanged(t, alice, id, before, "a cross-swap refund replay")
	})

	t.Run("tampered messages are refused", func(t *testing.T) {
		alice, bob, messages, id := r.SetupGoSwap(t)

		// A funding announcement whose amount was edited is still correctly signed
		// by its author but no longer matches the chain; it must be refused.
		r.Run(t, alice, messages, "fund-blake", id)
		r.Run(t, alice, messages, "publish-blake-funded", id)

		path := messagePath(t, messages, id, "blake-funded")

		original, tampered := tamperAmount(t, path)

		before := r.Snapshot(t, bob, id)

		r.Expect(t, bob, messages, "", "receive-blake-funded", id)

		r.AssertSnapshotUnchanged(t, bob, id, before, "a tampered funding announcement")

		// Restoring the message must make it importable again, which proves the
		// refusal was about the tampering and not about the swap.
		writeFile(t, path, original)
		r.Run(t, bob, messages, "receive-blake-funded", id)

		r.AssertState(t, bob, messages, id, "BLAKE_FUNDED")

		if tampered == original {
			t.Fatal("the tamper did not change the message")
		}
	})

	t.Run("pending claims and refunds adopt the durable transaction", func(t *testing.T) {
		// Four operations, each killed immediately after its broadcast. The restart
		// must adopt the transaction the record already names rather than signing a
		// second one: two claims of one HTLC are a race, and the loser's fee is
		// wasted at best.
		//
		// The crash point fires before the auto-mine, so the transaction is in the
		// mempool and the swap is legitimately still pending afterwards. The block
		// that confirms it is mined by the test, which is what an operator waiting
		// for a block would do.
		operations := []struct {
			name    string
			crash   string
			command string
			field   string
			actor   string
			prepare func(t *testing.T, alice, bob *rigtest.Side, messages, id string)
			want    string
		}{
			{
				name:    "bitcoin claim",
				crash:   "btc-claim-after-broadcast",
				command: "claim-btc",
				field:   "btc_claim_txid",
				actor:   "alice",
				want:    "BTC_CLAIMED",
			},
			{
				name:    "blake claim",
				crash:   "blake-claim-after-broadcast",
				command: "claim-blake",
				field:   "blake_claim_txid",
				actor:   "bob",
				prepare: func(t *testing.T, alice, bob *rigtest.Side, messages, id string) {
					r.Run(t, alice, messages, "claim-btc", id)
					r.Run(t, alice, messages, "publish-btc-claimed", id)
					r.Run(t, bob, messages, "receive-btc-claimed", id)
					r.Run(t, bob, messages, "extract-secret", id)
				},
				want: "BLAKE_CLAIMED",
			},
			{
				name:    "bitcoin refund",
				crash:   "btc-refund-after-broadcast",
				command: "refund-btc",
				field:   "btc_refund_txid",
				actor:   "bob",
				prepare: func(t *testing.T, alice, bob *rigtest.Side, messages, id string) {
					r.MineTo(t, "btc", bob.Role, r.FieldInt(t, alice, messages, id, "btc_timeout"))
				},
				want: "BTC_REFUNDED",
			},
		}

		for _, operation := range operations {
			t.Run(operation.name, func(t *testing.T) {
				alice, bob, messages, id := r.SetupGoSwap(t)

				r.FundBothLegs(t, alice, bob, messages, id)

				if operation.prepare != nil {
					operation.prepare(t, alice, bob, messages, id)
				}

				actor := alice
				chain := "btc"

				if operation.actor == "bob" {
					actor = bob
				}

				if strings.HasPrefix(operation.field, "blake_") {
					chain = "blake"
				}

				output, exit, err := r.Exec(t, actor, messages,
					[]string{swap.CrashPointEnv + "=" + operation.crash},
					operation.command, id)

				if err != nil {
					t.Fatalf("running the armed %s: %v", operation.command, err)
				}

				if exit != swap.CrashPointExit {
					t.Fatalf("exit = %d, want %d at %s\n%s",
						exit, swap.CrashPointExit, operation.crash, output)
				}

				txid, _ := rigtest.RecordField(t, actor, id, operation.field)

				if fmt.Sprint(txid) == "" || fmt.Sprint(txid) == "<nil>" {
					t.Fatalf("the killed %s left no %s", operation.command, operation.field)
				}

				// The restart adopts it: same transaction, still in the mempool.
				r.Run(t, actor, messages, operation.command, id)

				after, _ := rigtest.RecordField(t, actor, id, operation.field)

				if after != txid {
					t.Fatalf("the restart signed a second transaction: %v -> %v", txid, after)
				}

				if _, err := r.NodeTry(t, chain, "getmempoolentry", fmt.Sprint(txid)); err != nil {
					t.Fatalf("the adopted transaction is not in the %s mempool: %v", chain, err)
				}

				// Once a block arrives the same transaction confirms and advances the
				// state; nothing new is signed.
				r.Mine(t, chain, actor.Role, 1)
				r.Run(t, actor, messages, operation.command, id)

				r.AssertState(t, actor, messages, id, operation.want)

				confirmed, _ := rigtest.RecordField(t, actor, id, operation.field)

				if confirmed != txid {
					t.Fatalf("confirmation used a different transaction: %v -> %v",
						txid, confirmed)
				}
			})
		}

		// The abortive BLAKE refund is its own shape: Alice refunds once her
		// timelock is reached, with Bitcoin never funded. It is the pre-BTC refund
		// path, and it is the one where a lost record strands Alice's BLAKE.
		t.Run("blake refund", func(t *testing.T) {
			alice, bob, messages, id := r.SetupGoSwap(t)

			r.Run(t, alice, messages, "fund-blake", id)
			r.Run(t, alice, messages, "publish-blake-funded", id)
			r.Run(t, bob, messages, "receive-blake-funded", id)

			r.MineTo(t, "blake", alice.Role, r.FieldInt(t, alice, messages, id, "blake_timeout"))

			output, exit, err := r.Exec(t, alice, messages,
				[]string{swap.CrashPointEnv + "=blake-refund-after-broadcast"},
				"refund-blake", id)

			if err != nil {
				t.Fatalf("running the armed refund: %v", err)
			}

			if exit != swap.CrashPointExit {
				t.Fatalf("exit = %d, want %d\n%s", exit, swap.CrashPointExit, output)
			}

			txid, _ := rigtest.RecordField(t, alice, id, "blake_refund_txid")

			if fmt.Sprint(txid) == "" || fmt.Sprint(txid) == "<nil>" {
				t.Fatal("the killed refund left no blake_refund_txid")
			}

			r.Run(t, alice, messages, "refund-blake", id)

			after, _ := rigtest.RecordField(t, alice, id, "blake_refund_txid")

			if after != txid {
				t.Fatalf("the restart signed a second refund: %v -> %v", txid, after)
			}

			r.Mine(t, "blake", alice.Role, 1)
			r.Run(t, alice, messages, "refund-blake", id)

			r.AssertState(t, alice, messages, id, "BLAKE_REFUNDED")
		})
	})
}

// fundingPoint reads one chain's funding outpoint from a record.
func fundingPoint(t *testing.T, r *rigtest.Rig, s *rigtest.Side, id, which string) (string, int64) {
	t.Helper()

	txid, _ := rigtest.RecordField(t, s, id, which+"_funding_txid")
	vout, _ := rigtest.RecordField(t, s, id, which+"_funding_vout")

	number, ok := vout.(float64)

	if !ok {
		t.Fatalf("%s_funding_vout is not a number: %v", which, vout)
	}

	return fmt.Sprint(txid), int64(number)
}

// messagePath finds the spool file for one message type of one swap.
func messagePath(t *testing.T, messages, id, kind string) string {
	t.Helper()

	entries, err := os.ReadDir(messages)

	if err != nil {
		t.Fatalf("reading the message spool: %v", err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		names = append(names, entry.Name())

		if strings.Contains(entry.Name(), id) && strings.Contains(entry.Name(), kind) {
			return filepath.Join(messages, entry.Name())
		}
	}

	t.Fatalf("no %s message for %s in %v", kind, id, names)

	return ""
}

// tamperAmount edits the BLAKE amount in a signed message and returns both the
// original text and the tampered one.
func tamperAmount(t *testing.T, path string) (string, string) {
	t.Helper()

	data, err := os.ReadFile(path)

	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	original := string(data)

	// The amount appears as a decimal string in the message content. Rewriting it
	// leaves the signature valid for the tampered bytes only if the signature were
	// recomputed, so the receiver must refuse it either way: either the signature
	// fails or the amount disagrees with the chain.
	tampered := strings.Replace(original, "0.001", "0.002", 1)

	if tampered == original {
		t.Fatalf("the message does not contain the amount to tamper with:\n%s", original)
	}

	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatalf("tampering with %s: %v", path, err)
	}

	return original, tampered
}

// writeFile writes a spool file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
