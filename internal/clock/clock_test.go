package clock_test

import (
	"testing"
	"time"

	"cablecheck/internal/clock"
)

// Real must satisfy the canonical clock.Clock contract.
var _ clock.Clock = clock.Real{}

func TestRealNowMonotonic(t *testing.T) {
	c := clock.Real{}
	a := c.Now()
	b := c.Now()
	if a.IsZero() {
		t.Fatal("Real.Now returned the zero time")
	}
	if b.Before(a) {
		t.Fatalf("Real.Now went backwards: %v then %v", a, b)
	}
}

func TestRealAfterAndTicker(t *testing.T) {
	c := clock.Real{}
	if c.After(time.Hour) == nil {
		t.Fatal("Real.After returned a nil channel")
	}
	tk := c.NewTicker(time.Hour)
	if tk.C() == nil {
		t.Fatal("Real ticker C() returned a nil channel")
	}
	tk.Stop()
	tk.Stop() // Stop must be idempotent, mirroring time.Ticker
}
