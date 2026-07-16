package testutil

import (
	"testing"
	"time"
)

const (
	// testTimeoutGrace is subtracted from the test binary deadline so a
	// wait that overruns still leaves time to report and clean up before
	// the test framework panics.
	testTimeoutGrace = 2 * time.Second
	// testTimeoutFloor is the minimum budget TestTimeout ever returns.
	testTimeoutFloor = 10 * time.Second
	// testTimeoutFallback is returned when no deadline is available
	// (-timeout=0, or a testing.TB without a Deadline method); it matches
	// the go test default timeout.
	testTimeoutFallback = 10 * time.Minute
)

// TestTimeout returns the budget a single bounded wait may consume: the time
// remaining until the test binary's deadline minus a 2s grace period, with a
// 10s floor so waits stay generous under -race slowdowns and tight -timeout
// overrides. Without a deadline it returns a generous fallback.
func TestTimeout(t testing.TB) time.Duration {
	td, ok := t.(interface{ Deadline() (time.Time, bool) })
	if !ok {
		return testTimeoutFallback
	}
	deadline, ok := td.Deadline()
	if !ok {
		return testTimeoutFallback
	}
	budget := time.Until(deadline) - testTimeoutGrace
	if budget < testTimeoutFloor {
		return testTimeoutFloor
	}
	return budget
}

// WaitFor blocks until ch receives or is closed, or the TestTimeout budget
// expires, in which case it reports msg via t.Errorf. It never calls
// t.Fatalf, so it is safe in helpers reachable from non-test goroutines.
func WaitFor(t testing.TB, ch <-chan struct{}, msg string) {
	t.Helper()
	timer := time.NewTimer(TestTimeout(t))
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Errorf("testutil.WaitFor: timed out waiting for %s", msg)
	}
}
