package swap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// fakeNode implements only the calls a test needs.
//
// It embeds the engine's own interface, so an unimplemented call is a nil method
// value rather than a compile error — which is what keeps a partial fake possible
// for a surface this wide. A test that reaches an unstubbed call panics, which is
// the right failure for "this command needed something the test did not expect".
type fakeNode struct {
	Node

	height int64
	err    error
}

func (f *fakeNode) Height(context.Context) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}

	return f.height, nil
}

// newRecord builds a valid swap record.
func newRecord(t *testing.T, id string) *model.Swap {
	t.Helper()

	record, err := model.New(strings.NewReader(strings.Repeat("ab", 64)), model.NewParams{
		SwapID:       id,
		BlakeAmount:  amount.Blake(1_000_000),
		BTCSats:      25_000,
		BlakeTimeout: 500,
		BTCTimeout:   400,
	})

	if err != nil {
		t.Fatal(err)
	}

	return record
}

func newEngine(t *testing.T) (*Engine, *store.Memory) {
	t.Helper()

	st := store.NewMemory()

	engine := NewEngine(st, fund.NewMemStore(), &fakeNode{height: 700}, &fakeNode{height: 600})

	return engine, st
}

func TestStatusReportsBothChains(t *testing.T) {
	engine, _ := newEngine(t)

	status, err := engine.Status(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if status.Blake != 700 || status.BTC != 600 {
		t.Fatalf("status = %+v", status)
	}

	// A node that cannot be reached is an error, not a zero height: reporting
	// zero would look like a chain reset.
	engine.BTC = &fakeNode{err: fmt.Errorf("connection refused")}

	if _, err := engine.Status(context.Background()); err == nil {
		t.Fatal("an unreachable node was reported as a height")
	}
}

func TestShowAndList(t *testing.T) {
	ctx := context.Background()
	engine, st := newEngine(t)

	first := newRecord(t, "aaaa00000001")
	second := newRecord(t, "aaaa00000002")

	for _, record := range []*model.Swap{second, first} {
		if err := st.Save(ctx, store.Alice, record); err != nil {
			t.Fatal(err)
		}
	}

	shown, err := engine.Show(ctx, first.SwapID)

	if err != nil || shown.SwapID != first.SwapID {
		t.Fatalf("shown = %+v, err = %v", shown, err)
	}

	if _, err := engine.Show(ctx, "ffff00000000"); err == nil {
		t.Fatal("an unknown swap was shown")
	}

	listed, err := engine.List(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(listed) != 2 || listed[0].SwapID != first.SwapID || listed[1].SwapID != second.SwapID {
		t.Fatalf("listed = %+v", listed)
	}
}

// badStore is a store with one unreadable record.
type badStore struct {
	store.Store
}

func (b badStore) List(context.Context) ([]string, error) {
	return []string{"aaaa00000003"}, nil
}

func (b badStore) Load(context.Context, string) (*model.Swap, error) {
	return nil, fmt.Errorf("record is corrupt")
}

// TestListReportsAnUnreadableRecord: a listing that silently skipped it would
// hide the one record an operator most needs to see.
func TestListReportsAnUnreadableRecord(t *testing.T) {
	engine := NewEngine(badStore{store.NewMemory()}, fund.NewMemStore(), nil, nil)

	_, err := engine.List(context.Background())

	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdvanceIsGatedAndForwardOnly(t *testing.T) {
	ctx := context.Background()
	engine, st := newEngine(t)

	record := newRecord(t, "aaaa00000004")

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	// Off by default: the command exists for tests and demos.
	if _, _, err := engine.Advance(ctx, record.SwapID, store.Alice, string(state.BlakeFunded)); err == nil {
		t.Fatal("advance ran outside test mode")
	}

	engine.TestMode = true

	from, to, err := engine.Advance(ctx, record.SwapID, store.Alice, string(state.BlakeFunded))

	if err != nil {
		t.Fatalf("a legal transition was refused: %v", err)
	}

	if from != state.Offered || to != state.BlakeFunded {
		t.Fatalf("%s -> %s", from, to)
	}

	persisted, err := st.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if persisted.State != state.BlakeFunded {
		t.Fatalf("the record still says %s", persisted.State)
	}

	// A state the machine does not reach in one step is refused, and the record
	// is left exactly as it was.
	if _, _, err := engine.Advance(ctx, record.SwapID, store.Alice, string(state.Complete)); err == nil {
		t.Fatal("an illegal transition was accepted")
	}

	persisted, err = st.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if persisted.State != state.BlakeFunded {
		t.Fatalf("a refused advance changed the record to %s", persisted.State)
	}
}

// TestAdvanceUsesTheRequestedRole: the record lives in one participant-local
// tree, so the role is a protocol assertion and it has to be applied to the save
// as well as the load.
func TestAdvanceUsesTheRequestedRole(t *testing.T) {
	ctx := context.Background()
	engine, st := newEngine(t)
	engine.TestMode = true

	record := newRecord(t, "aaaa00000005")

	if err := st.Save(ctx, store.Bob, record); err != nil {
		t.Fatal(err)
	}

	if _, _, err := engine.Advance(ctx, record.SwapID, "mallory", string(state.BlakeFunded)); err == nil {
		t.Fatal("an unknown role was accepted")
	}

	// The engine's own role is a default, not a limit: Bob's own process drives
	// Bob's record.
	if _, _, err := engine.Advance(ctx, record.SwapID, store.Bob, string(state.BlakeFunded)); err != nil {
		t.Fatalf("bob could not advance his own record: %v", err)
	}
}

func TestEngineDefaults(t *testing.T) {
	engine := NewEngine(store.NewMemory(), fund.NewMemStore(), nil, nil)

	if engine.Role != store.Alice {
		t.Fatalf("role = %q", engine.Role)
	}

	if engine.BlakeWallet == "" || engine.BTCWallet == "" {
		t.Fatal("no signing wallets were defaulted")
	}
}
