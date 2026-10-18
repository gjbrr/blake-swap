// Package platform is the browser's side of the world, behind interfaces.
//
// Every capability the client needs that a browser provides differently from a
// host is here and nowhere else: the clock, the log, durable storage, an HTTP
// transport, and a WebSocket to a relay. The engine and the market layer depend on
// the interfaces, so the same code runs on a host (where the implementations are
// ordinary Go) and in a wasm worker (where they are JavaScript bridges).
//
// The interfaces are small on purpose. A seam that offers more than the code above it
// uses is a seam that will grow one convenience at a time until the two platforms
// disagree about something that mattered.
package platform

import (
	"context"
	"time"
)

// Clock is the time source.
//
// It is an interface because two clocks that disagree by minutes make two
// participants disagree about whether an offer expired, and a test needs to move
// time without sleeping.
type Clock interface {
	Now() time.Time
}

// Console is the log sink.
type Console interface {
	Log(message string)
}

// KV is durable key-value storage.
//
// The browser implementation is IndexedDB, which is asynchronous, which is why every
// method takes a context: a synchronous API here would be a lie that the browser
// cannot keep.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	// Keys returns the keys with a prefix, sorted, so iteration is deterministic
	// on both platforms.
	Keys(ctx context.Context, prefix string) ([]string, error)
}

// RealClock is the system clock.
//
// It is a distinct type rather than a function so it can be embedded and so a zero
// RealClock is usable.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// FixedClock is a clock that does not move unless it is told to.
type FixedClock struct {
	Current time.Time
}

// Now implements Clock.
func (c *FixedClock) Now() time.Time { return c.Current }

// Advance moves a fixed clock forward.
func (c *FixedClock) Advance(d time.Duration) { c.Current = c.Current.Add(d) }

// PrintConsole writes to the platform's standard logger.
type PrintConsole struct{}

// Log implements Console.
func (PrintConsole) Log(message string) { println(message) }

// sortStrings sorts in place.
//
// Both platforms must hand keys back in the same order, or a scan over records would
// depend on which platform it ran on.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
