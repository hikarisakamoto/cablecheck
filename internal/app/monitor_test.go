package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/config"
	"cablecheck/internal/evaluate"
	"cablecheck/internal/model"
	"cablecheck/internal/network"
	"cablecheck/internal/testsuite"
)

// writeSysfsAttr writes a single sysfs attribute file for the interface under
// root, creating the interface directory if needed.
func writeSysfsAttr(t *testing.T, root, iface, name, value string) {
	t.Helper()
	dir := filepath.Join(root, iface)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestAssembleReportSurfacesRenegotiation drives the sysfs monitor through a
// scripted mid-test renegotiation (carrier_changes advancing by 2 with the
// link otherwise stable), then asserts the event reaches the assembled report
// as a "renegotiation" MonitoringEvent and that evaluate.Facts counts it — the
// exact string the evaluator's renegotiations(r) already looks for.
func TestAssembleReportSurfacesRenegotiation(t *testing.T) {
	root := t.TempDir()
	const iface = "eth0"
	// Baseline: link up, 1000M full, carrier_changes even.
	writeSysfsAttr(t, root, iface, "operstate", "up")
	writeSysfsAttr(t, root, iface, "carrier", "1")
	writeSysfsAttr(t, root, iface, "speed", "1000")
	writeSysfsAttr(t, root, iface, "duplex", "full")
	writeSysfsAttr(t, root, iface, "carrier_changes", "8")

	fc := clocktest.New(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	const interval = time.Second
	m := network.NewMonitor(iface, interval, fc, network.WithSysfsRoot(root))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// A sub-interval flap: identical carrier/speed/duplex, carrier_changes +2.
	writeSysfsAttr(t, root, iface, "carrier_changes", "10")
	fc.BlockUntilWaiters(1)
	fc.Advance(interval)
	// Wait for the poll to record the event, then stop the monitor.
	deadline := time.Now().Add(2 * time.Second)
	for len(m.History()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("monitor produced no event for the scripted renegotiation")
		}
	}
	cancel()
	<-done

	cfg := &config.RunConfig{Role: config.RolePC1}
	a, err := New(cfg, Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.monitor = m

	when := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	rep := a.assembleReport(&preflightInfo{}, &testsuite.SessionResults{}, when, when, nil, nil)

	var got *model.MonitoringEvent
	for i := range rep.MonitoringEvents {
		if rep.MonitoringEvents[i].Type == string(network.Renegotiation) {
			got = &rep.MonitoringEvents[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("report MonitoringEvents %+v missing a %q event", rep.MonitoringEvents, network.Renegotiation)
	}
	if got.Type != "renegotiation" {
		t.Errorf("event Type = %q, want %q (must match evaluate.renegotiations)", got.Type, "renegotiation")
	}
	if got.At != when.UTC() && got.At.IsZero() {
		t.Errorf("event At is zero; want the monitor's fake-clock timestamp")
	}

	facts := evaluate.FactsFromReport(rep)
	if facts.Renegotiations != 1 {
		t.Fatalf("Facts.Renegotiations = %d, want 1", facts.Renegotiations)
	}
}

// TestStartLinkMonitorNoLeak asserts the run's link-watch helper starts a
// polling goroutine and that stopLinkMonitor cancels and joins it cleanly (no
// leaked goroutine), even when no events are produced, and is idempotent.
func TestStartLinkMonitorNoLeak(t *testing.T) {
	root := t.TempDir()
	const iface = "eth0"
	writeSysfsAttr(t, root, iface, "operstate", "up")
	writeSysfsAttr(t, root, iface, "carrier", "1")
	writeSysfsAttr(t, root, iface, "speed", "1000")
	writeSysfsAttr(t, root, iface, "duplex", "full")
	writeSysfsAttr(t, root, iface, "carrier_changes", "2")

	fc := clocktest.New(time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC))
	cfg := &config.RunConfig{Role: config.RolePC1, MonitorInterval: time.Second}
	a, err := New(cfg, Deps{Clock: fc, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sysfsRoot = root

	a.startLinkMonitor(context.Background(), iface)
	if a.monitor == nil {
		t.Fatal("startLinkMonitor did not attach a monitor to the App")
	}
	// A second start is a no-op: the same monitor stays attached.
	first := a.monitor
	a.startLinkMonitor(context.Background(), iface)
	if a.monitor != first {
		t.Fatal("second startLinkMonitor replaced the running monitor")
	}
	// Advance a couple of intervals to prove the loop is live.
	for i := 0; i < 2; i++ {
		fc.BlockUntilWaiters(1)
		fc.Advance(time.Second)
	}
	// stop must return promptly (cancel + join); a hang here means a leak.
	joined := make(chan struct{})
	go func() { a.stopLinkMonitor(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("stopLinkMonitor did not return: goroutine leaked or blocked")
	}
	// Idempotent second stop must be a no-op, not a panic.
	a.stopLinkMonitor()
}
