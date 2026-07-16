// Package clock abstracts time for cablecheck. Product code never calls
// time.Now, time.After or time.NewTicker directly; it takes a Clock so tests
// can substitute the deterministic fake in clock/clocktest. This package (and
// its Real implementation) is the single sanctioned home of wall-clock reads.
package clock

import "time"

// Clock provides the time operations used throughout cablecheck.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel on which the current time is delivered once,
	// after d has elapsed.
	After(d time.Duration) <-chan time.Time
	// NewTicker returns a Ticker delivering ticks every d. It panics if
	// d <= 0, mirroring time.NewTicker. Callers must Stop the Ticker when
	// done with it.
	NewTicker(d time.Duration) Ticker
}

// Ticker delivers periodic ticks on the channel returned by C until stopped.
type Ticker interface {
	// C returns the channel on which ticks are delivered. Like
	// time.Ticker.C it has a one-element buffer and drops ticks when the
	// receiver falls behind.
	C() <-chan time.Time
	// Stop stops tick delivery. It does not close the channel. Stop is
	// idempotent.
	Stop()
}
