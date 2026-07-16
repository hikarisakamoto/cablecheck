package testutil_test

import (
	"testing"
	"time"

	"cablecheck/internal/testutil"
)

// fakeDeadlineTB overrides Deadline so TestTimeout's derivation is testable.
type fakeDeadlineTB struct {
	testing.TB
	deadline time.Time
	ok       bool
}

func (f fakeDeadlineTB) Deadline() (time.Time, bool) { return f.deadline, f.ok }

func TestTestTimeoutFloor(t *testing.T) {
	near := fakeDeadlineTB{TB: t, deadline: time.Now().Add(3 * time.Second), ok: true}
	if got := testutil.TestTimeout(near); got != 10*time.Second {
		t.Errorf("TestTimeout(near deadline) = %v, want 10s floor", got)
	}
}

func TestTestTimeoutSubtractsGrace(t *testing.T) {
	far := fakeDeadlineTB{TB: t, deadline: time.Now().Add(10 * time.Minute), ok: true}
	got := testutil.TestTimeout(far)
	if got < 9*time.Minute || got > 10*time.Minute-2*time.Second {
		t.Errorf("TestTimeout(deadline in 10m) = %v, want ~9m58s (deadline minus 2s grace)", got)
	}
}

func TestTestTimeoutNoDeadline(t *testing.T) {
	// Has a Deadline method reporting "no deadline set".
	none := fakeDeadlineTB{TB: t, ok: false}
	if got := testutil.TestTimeout(none); got < 10*time.Second {
		t.Errorf("TestTimeout(no deadline) = %v, want a generous value >= 10s", got)
	}
	// Has no Deadline method at all (e.g. *testing.B).
	bare := struct{ testing.TB }{t}
	if got := testutil.TestTimeout(bare); got < 10*time.Second {
		t.Errorf("TestTimeout(no Deadline method) = %v, want a generous value >= 10s", got)
	}
}

func TestTestTimeoutRealT(t *testing.T) {
	if got := testutil.TestTimeout(t); got < 10*time.Second {
		t.Errorf("TestTimeout(t) = %v, want >= 10s", got)
	}
}

func TestWaitForClosedChannel(t *testing.T) {
	ch := make(chan struct{})
	close(ch)
	testutil.WaitFor(t, ch, "already-closed channel") // must return without error
}
