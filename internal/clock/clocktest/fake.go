// Package clocktest provides FakeClock, a deterministic clock.Clock for
// tests. Time only moves when the test calls Advance, and BlockUntilWaiters
// lets the test park until goroutines under test have registered timers, which
// eliminates lost-wakeup races (Advance running before the goroutine calls
// After).
package clocktest

import (
	"sync"
	"time"

	"cablecheck/internal/clock"
)

// FakeClock is a manually advanced clock.Clock. All methods are safe for
// concurrent use. The canonical pattern for waking a goroutine that waits on
// After or a ticker is:
//
//	fc.BlockUntilWaiters(1) // park until the goroutine has registered
//	fc.Advance(d)           // fire everything due at or before now+d
type FakeClock struct {
	mu      sync.Mutex
	cond    *sync.Cond // broadcast whenever the waiter count increases
	now     time.Time
	timers  []*fakeTimer
	tickers []*fakeTicker
}

var _ clock.Clock = (*FakeClock)(nil)

// fakeTimer is a pending one-shot After registration.
type fakeTimer struct {
	target time.Time
	ch     chan time.Time // buffered 1; written exactly once
}

// fakeTicker implements clock.Ticker against a FakeClock.
type fakeTicker struct {
	c        *FakeClock
	interval time.Duration
	next     time.Time      // scheduled time of the next tick
	ch       chan time.Time // buffered 1; late ticks are dropped like time.Ticker
	stopped  bool
}

// New returns a FakeClock whose Now reports start until Advance is called.
func New(start time.Time) *FakeClock {
	c := &FakeClock{now: start}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Now returns the fake current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that receives the scheduled fire time once Advance
// has moved the clock to or past now+d. If d <= 0 the channel fires
// immediately. The registration counts as a waiter for BlockUntilWaiters.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, &fakeTimer{target: c.now.Add(d), ch: ch})
	c.cond.Broadcast()
	return ch
}

// NewTicker returns a clock.Ticker that fires once per interval d advanced.
// It panics if d <= 0, mirroring time.NewTicker. The ticker counts as one
// waiter for BlockUntilWaiters until it is stopped. Ticks that pile up while
// nobody receives are coalesced into one, like time.Ticker.
func (c *FakeClock) NewTicker(d time.Duration) clock.Ticker {
	if d <= 0 {
		panic("clocktest: non-positive interval for NewTicker")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tk := &fakeTicker{
		c:        c,
		interval: d,
		next:     c.now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	c.tickers = append(c.tickers, tk)
	c.cond.Broadcast()
	return tk
}

// Advance moves the clock forward by d and fires every due waiter: each
// one-shot After whose deadline has been reached receives its scheduled fire
// time, and each ticker delivers one tick per elapsed interval (coalescing
// into the one-element buffer if the receiver is behind). Advance never
// blocks. It panics if d < 0.
func (c *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("clocktest: negative duration for Advance")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)

	kept := c.timers[:0]
	for _, tm := range c.timers {
		if tm.target.After(c.now) {
			kept = append(kept, tm)
			continue
		}
		tm.ch <- tm.target // buffered 1, written once: never blocks
	}
	// Zero dropped entries so fired timers are not retained by the backing array.
	for i := len(kept); i < len(c.timers); i++ {
		c.timers[i] = nil
	}
	c.timers = kept

	for _, tk := range c.tickers {
		for !tk.next.After(c.now) {
			select {
			case tk.ch <- tk.next:
			default: // receiver behind: drop, like time.Ticker
			}
			tk.next = tk.next.Add(tk.interval)
		}
	}
}

// BlockUntilWaiters parks the calling goroutine until at least n waiters are
// registered on the clock: pending After registrations plus live (unstopped)
// tickers. Call it before every Advance that must wake another goroutine.
func (c *FakeClock) BlockUntilWaiters(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.timers)+len(c.tickers) < n {
		c.cond.Wait()
	}
}

// C returns the tick delivery channel.
func (tk *fakeTicker) C() <-chan time.Time { return tk.ch }

// Stop deregisters the ticker; no further ticks are delivered and it no
// longer counts as a waiter. Stop is idempotent.
func (tk *fakeTicker) Stop() {
	tk.c.mu.Lock()
	defer tk.c.mu.Unlock()
	if tk.stopped {
		return
	}
	tk.stopped = true
	for i, other := range tk.c.tickers {
		if other == tk {
			tk.c.tickers = append(tk.c.tickers[:i], tk.c.tickers[i+1:]...)
			break
		}
	}
}
