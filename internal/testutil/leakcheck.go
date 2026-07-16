// Package testutil provides shared hermetic test helpers: goroutine leak
// checking, partial-read dribbling, deadline-derived wait budgets, and
// scripted stdin. It is test infrastructure: the bounded real-time polling in
// LeakCheck and the deadline math in TestTimeout are the sanctioned uses of
// the time package outside internal/clock.
package testutil

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// modulePathMarker identifies goroutines running cablecheck code: any
// goroutine whose stack mentions it is attributed to this module, which
// filters out testing-framework and os/signal background goroutines.
const modulePathMarker = "cablecheck/internal"

const (
	leakPollStep   = 10 * time.Millisecond
	leakPollBudget = 2 * time.Second
)

// LeakCheck guards a test against leaked cablecheck goroutines. Call it at
// the top of the test: it snapshots the number of goroutines whose stacks
// contain the module path, then registers a t.Cleanup that polls up to 2s
// (in 10ms steps) for the count to return to the snapshot and reports the
// offending stacks via t.Errorf if it never does. The poll is a bounded
// convergence wait for shutdown, not scheduling synchronization.
func LeakCheck(t testing.TB) {
	t.Helper()
	before := len(moduleGoroutines())
	t.Cleanup(func() {
		deadline := time.Now().Add(leakPollBudget)
		var stacks []string
		for {
			stacks = moduleGoroutines()
			if len(stacks) <= before {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(leakPollStep)
		}
		t.Errorf("testutil.LeakCheck: %d cablecheck goroutine(s) at test start, %d still running after cleanup; offending stacks:\n\n%s",
			before, len(stacks), strings.Join(stacks, "\n\n"))
	})
}

// moduleGoroutines returns one stack stanza per live goroutine whose stack
// contains modulePathMarker, captured via runtime.Stack(buf, true).
func moduleGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for _, stanza := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stanza, modulePathMarker) {
			out = append(out, stanza)
		}
	}
	return out
}
