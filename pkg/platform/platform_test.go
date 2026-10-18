package platform

import (
	"testing"
	"time"
)

// TestFixedClock: a test can move time without sleeping.
func TestFixedClock(t *testing.T) {
	clock := &FixedClock{Current: time.Unix(1_700_000_000, 0)}

	if clock.Now().Unix() != 1_700_000_000 {
		t.Fatalf("now = %d", clock.Now().Unix())
	}

	clock.Advance(time.Hour)

	if clock.Now().Unix() != 1_700_003_600 {
		t.Fatalf("now = %d", clock.Now().Unix())
	}
}

// TestRealClockIsUsableAsAValue pins that the zero value works, so a caller does not
// have to remember how to construct it.
func TestRealClockIsUsableAsAValue(t *testing.T) {
	var clock RealClock

	if clock.Now().IsZero() {
		t.Fatal("the system clock returned zero")
	}
}
