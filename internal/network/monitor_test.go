package network

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
)

// linkState is a scripted set of sysfs link attribute file contents for one
// interface. A field left at its zero value means "do not write that file"
// (absent), except the numeric fields which use a nil pointer via linkFile.
type linkState struct {
	operstate      string
	carrier        string // "" = absent
	speed          string // "" = absent
	duplex         string // "" = absent
	carrierChanges string // "" = absent
	carrierUp      string // "" = absent
	carrierDown    string // "" = absent
}

// writeLinkState (re)writes the sysfs attribute files for name under root to
// match st. Files whose scripted value is "" are removed so the read path
// exercises the absent-file branch. It is safe to call repeatedly between
// ticks to script a sequence of link states.
func writeLinkState(t *testing.T, root, name string, st linkState) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for file, value := range map[string]string{
		"operstate":          st.operstate,
		"carrier":            st.carrier,
		"speed":              st.speed,
		"duplex":             st.duplex,
		"carrier_changes":    st.carrierChanges,
		"carrier_up_count":   st.carrierUp,
		"carrier_down_count": st.carrierDown,
	} {
		path := filepath.Join(dir, file)
		if value == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove %s: %v", path, err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// TestReadLinkSnapshotEINVALTolerant asserts every field of ReadLinkSnapshot
// degrades to its unknown sentinel independently when the underlying sysfs
// file is absent or holds the "-1" no-carrier value — never a spurious real
// reading and never a panic.
func TestReadLinkSnapshotEINVALTolerant(t *testing.T) {
	root := t.TempDir()
	// operstate present; carrier absent; speed "-1"; duplex absent;
	// carrier_changes present; up/down counts absent.
	writeLinkState(t, root, "eth0", linkState{
		operstate:      "down",
		carrier:        "",
		speed:          "-1",
		duplex:         "",
		carrierChanges: "7",
	})

	var snap LinkSnapshot
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ReadLinkSnapshot panicked: %v", r)
			}
		}()
		snap = ReadLinkSnapshot(root, "eth0")
	}()

	if snap.Operstate != "down" {
		t.Errorf("Operstate = %q, want %q", snap.Operstate, "down")
	}
	if snap.Carrier != -1 {
		t.Errorf("Carrier = %d, want -1 (absent -> unknown)", snap.Carrier)
	}
	if snap.SpeedMbps != -1 {
		t.Errorf("SpeedMbps = %d, want -1 (\"-1\" -> unknown)", snap.SpeedMbps)
	}
	if snap.Duplex != "unknown" {
		t.Errorf("Duplex = %q, want %q (absent -> unknown)", snap.Duplex, "unknown")
	}
	if snap.CarrierChanges != 7 {
		t.Errorf("CarrierChanges = %d, want 7", snap.CarrierChanges)
	}
	if snap.CarrierUp != 0 || snap.CarrierDown != 0 {
		t.Errorf("CarrierUp/Down = %d/%d, want 0/0 (absent)", snap.CarrierUp, snap.CarrierDown)
	}

	// A fully empty interface directory must also be safe.
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "eth1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := ReadLinkSnapshot(empty, "eth1")
	if got.Carrier != -1 || got.SpeedMbps != -1 {
		t.Errorf("empty dir: Carrier=%d SpeedMbps=%d, want -1/-1", got.Carrier, got.SpeedMbps)
	}
	if got.Operstate != "unknown" || got.Duplex != "unknown" {
		t.Errorf("empty dir: Operstate=%q Duplex=%q, want unknown/unknown", got.Operstate, got.Duplex)
	}
}

// drainEvents collects up to want events from the monitor's channel, failing
// if they do not all arrive before the fake clock deadline is exhausted.
func drainEvents(t *testing.T, ch <-chan LinkEvent, want int) []LinkEvent {
	t.Helper()
	var got []LinkEvent
	timeout := time.After(2 * time.Second) // real wall clock: safety net only
	for len(got) < want {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for events: got %d of %d", len(got), want)
		}
	}
	return got
}

// TestMonitorEvents drives a scripted up -> down -> up@100M -> up@1000M
// sequence across ticks on a FakeClock and asserts the monitor emits
// CarrierLost, then CarrierRestored, then SpeedChanged, in that order, with
// monotonic FakeClock timestamps. The restore tick raises the link at 100M;
// because a down link reports speed -1, the speed change to a known value is
// only detectable on the following carrier-up tick (transitions to/from the
// -1 unknown sentinel are deliberately not "speed changed to -1").
func TestMonitorEvents(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	fc := clocktest.New(start)
	const interval = time.Second

	// Baseline (captured at construction): link up at 1000M full.
	writeLinkState(t, root, "eth0", linkState{
		operstate: "up", carrier: "1", speed: "1000", duplex: "full",
		carrierChanges: "4",
	})

	m := NewMonitor("eth0", interval, fc, WithSysfsRoot(root))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// tick rewrites sysfs, advances the fake clock one interval, then waits for
	// the poll to record its events in History — the mutex-guarded,
	// overflow-proof record. Waiting on History (not the channel) avoids
	// conflating this poll's events with any left buffered from a prior tick.
	tick := func(st linkState) {
		before := len(m.History())
		writeLinkState(t, root, "eth0", st)
		fc.BlockUntilWaiters(1)
		fc.Advance(interval)
		waitHistoryGrows(t, m, before)
	}

	// Tick 1: carrier goes down (speed reads -1 on a down link).
	tick(linkState{operstate: "down", carrier: "0", speed: "-1", duplex: "", carrierChanges: "5"})
	// Tick 2: carrier restored at 100M full.
	tick(linkState{operstate: "up", carrier: "1", speed: "100", duplex: "full", carrierChanges: "6"})
	// Tick 3: in-place speed change 100 -> 1000 while the carrier stays up.
	tick(linkState{operstate: "up", carrier: "1", speed: "1000", duplex: "full", carrierChanges: "6"})

	// The channel must have carried events too (buffered, no overflow here).
	select {
	case <-m.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered on the Events channel")
	}

	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("Run returned %v", err)
	}

	// History is the authoritative complete record; assert the three headline
	// events appear in order there (the channel merely mirrors it, lossily
	// under overflow).
	hist := m.History()
	assertOrder(t, typesOf(hist), CarrierLost, CarrierRestored, SpeedChanged)

	// Timestamps across the whole history must be non-decreasing and drawn
	// from the fake clock (never the wall clock).
	if len(hist) < 3 {
		t.Fatalf("History has %d events, want >=3", len(hist))
	}
	var last time.Time
	for i, e := range hist {
		if e.At.Before(last) {
			t.Errorf("event %d timestamp %v precedes previous %v", i, e.At, last)
		}
		if e.At.Before(start) {
			t.Errorf("event %d timestamp %v predates clock start %v", i, e.At, start)
		}
		last = e.At
	}
}

// assertOrder fails unless want appears as an ordered (not necessarily
// contiguous) subsequence of seen.
func assertOrder(t *testing.T, seen []LinkEventType, want ...LinkEventType) {
	t.Helper()
	i := 0
	for _, tp := range seen {
		if i < len(want) && tp == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("events %v do not contain ordered subsequence %v (matched %d)", seen, want, i)
	}
}

// TestRenegotiationByCarrierChanges asserts that two consecutive polls with
// identical carrier/speed/duplex but carrier_changes advanced by >=2 yield
// exactly one Renegotiation event (a full down/up flap within one interval).
func TestRenegotiationByCarrierChanges(t *testing.T) {
	root := t.TempDir()
	fc := clocktest.New(time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC))
	const interval = time.Second

	writeLinkState(t, root, "eth0", linkState{
		operstate: "up", carrier: "1", speed: "1000", duplex: "full",
		carrierChanges: "10",
	})
	m := NewMonitor("eth0", interval, fc, WithSysfsRoot(root))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	ev := m.Events()

	// Identical carrier/speed/duplex, but carrier_changes jumped by 2:
	// a down/up flap happened entirely within the interval.
	writeLinkState(t, root, "eth0", linkState{
		operstate: "up", carrier: "1", speed: "1000", duplex: "full",
		carrierChanges: "12",
	})
	fc.BlockUntilWaiters(1)
	fc.Advance(interval)

	got := drainEvents(t, ev, 1)
	if got[0].Type != Renegotiation {
		t.Fatalf("event = %q, want Renegotiation", got[0].Type)
	}

	cancel()
	<-done

	hist := m.History()
	n := 0
	for _, e := range hist {
		if e.Type == Renegotiation {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Renegotiation count = %d, want exactly 1 (history %v)", n, typesOf(hist))
	}
}

// TestMonitorChannelNeverBlocks fills the 64-event buffer with no consumer,
// keeps polling, and asserts the poller never blocks: Run still returns
// promptly on ctx cancel and Dropped() counts the overflow.
func TestMonitorChannelNeverBlocks(t *testing.T) {
	root := t.TempDir()
	fc := clocktest.New(time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC))
	const interval = time.Second

	// Baseline up.
	changes := 100
	writeLinkState(t, root, "eth0", linkState{
		operstate: "up", carrier: "1", speed: "1000", duplex: "full",
		carrierChanges: strconv.Itoa(changes),
	})
	m := NewMonitor("eth0", interval, fc, WithSysfsRoot(root))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Never consume Events(). Flap carrier every tick so an event is produced
	// each poll; drive well past the 64-buffer capacity.
	up := true
	for i := 0; i < 200; i++ {
		up = !up
		changes++
		carrier := "0"
		operstate := "down"
		speed := "-1"
		if up {
			carrier = "1"
			operstate = "up"
			speed = "1000"
		}
		writeLinkState(t, root, "eth0", linkState{
			operstate: operstate, carrier: carrier, speed: speed, duplex: "full",
			carrierChanges: strconv.Itoa(changes),
		})
		fc.BlockUntilWaiters(1)
		fc.Advance(interval)
	}

	// The poller must still be alive and responsive to cancellation.
	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel: poller blocked on a full channel")
	}

	if m.Dropped() == 0 {
		t.Fatalf("Dropped() = 0, want > 0 after overflowing the 64-event buffer")
	}
	// History records every event regardless of channel overflow.
	if len(m.History()) <= 64 {
		t.Fatalf("History() has %d events, want > 64 (full record)", len(m.History()))
	}
}

// typesOf extracts the event types of a history slice for diagnostics.
func typesOf(hist []LinkEvent) []LinkEventType {
	out := make([]LinkEventType, len(hist))
	for i, e := range hist {
		out[i] = e.Type
	}
	return out
}

// waitHistoryGrows blocks until the monitor's History grows beyond before,
// which the polling goroutine records synchronously (under lock) as it
// processes each tick. It spins against a real-time deadline that only trips
// on a genuine hang, keeping the test deterministic without a time.Sleep for
// synchronization.
func waitHistoryGrows(t *testing.T, m *Monitor, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(m.History()) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("History did not grow past %d events; poll appears stuck", before)
		}
		runtime.Gosched()
	}
}
