package network

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cablecheck/internal/clock"
)

// eventChanCapacity is the buffer depth of the Events channel. A full buffer
// causes the poller to drop-and-count rather than block; History() retains the
// complete record regardless (docs/design/tools.md §8).
const eventChanCapacity = 64

// LinkSnapshot is one poll of an interface's sysfs link state. Every numeric
// field carries an explicit unknown sentinel so an EINVAL / absent / "-1"
// read is never mistaken for a real value: Carrier and SpeedMbps are -1 when
// unknown, and Operstate/Duplex are "unknown".
type LinkSnapshot struct {
	// At is the fake- or wall-clock time the snapshot was read.
	At time.Time
	// Operstate is the sysfs operstate, lowercase ("up","down","unknown",
	// "lowerlayerdown","dormant"); "unknown" when the file is unreadable.
	Operstate string
	// Carrier is 1 (up) or 0 (down); -1 when unknown (EINVAL on admin-down,
	// or the file is absent).
	Carrier int
	// SpeedMbps is the link speed in Mb/s; -1 when unknown (EINVAL or the
	// literal "-1" seen on a NO-CARRIER port).
	SpeedMbps int
	// Duplex is "full", "half" or "unknown" ("unknown" on EINVAL/absent).
	Duplex string
	// CarrierChanges is the combined up+down transition count.
	CarrierChanges uint64
	// CarrierUp is the carrier_up_count value.
	CarrierUp uint64
	// CarrierDown is the carrier_down_count value.
	CarrierDown uint64
}

// ReadLinkSnapshot reads the sysfs link attributes for ifName under sysfsRoot
// (empty means DefaultSysfsRoot). Every field is read independently and any
// error — missing file, EINVAL, or the "-1" no-carrier sentinel — maps to
// that field's unknown value, so a partial or admin-down interface never
// yields a spurious reading. It never panics and never reports time itself:
// the caller stamps At.
func ReadLinkSnapshot(sysfsRoot, ifName string) LinkSnapshot {
	root := sysfsRoot
	if root == "" {
		root = DefaultSysfsRoot
	}
	base := filepath.Join(root, ifName)
	return LinkSnapshot{
		Operstate:      readStringAttr(base, "operstate", "unknown"),
		Carrier:        readIntAttr(base, "carrier"),
		SpeedMbps:      readIntAttr(base, "speed"),
		Duplex:         readDuplexAttr(base),
		CarrierChanges: readUintAttr(base, "carrier_changes"),
		CarrierUp:      readUintAttr(base, "carrier_up_count"),
		CarrierDown:    readUintAttr(base, "carrier_down_count"),
	}
}

// readStringAttr reads a trimmed sysfs string attribute, returning fallback on
// any error or an empty file.
func readStringAttr(base, name, fallback string) string {
	data, err := os.ReadFile(filepath.Join(base, name))
	if err != nil {
		return fallback
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return fallback
	}
	return s
}

// readDuplexAttr reads the duplex attribute and normalizes it to
// "full"/"half"/"unknown"; any error, EINVAL, or unrecognized value yields
// "unknown".
func readDuplexAttr(base string) string {
	data, err := os.ReadFile(filepath.Join(base, "duplex"))
	if err != nil {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(string(data))) {
	case "full":
		return "full"
	case "half":
		return "half"
	default:
		return "unknown"
	}
}

// readIntAttr reads a signed sysfs integer (carrier, speed). A missing file,
// EINVAL, an unparseable value, or the literal "-1" all map to the -1 unknown
// sentinel — never a spurious real reading.
func readIntAttr(base, name string) int {
	data, err := os.ReadFile(filepath.Join(base, name))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// readUintAttr reads an unsigned sysfs counter; any error or unparseable value
// yields 0 (these counters are monotonic from boot, so 0 is the safe floor).
func readUintAttr(base, name string) uint64 {
	data, err := os.ReadFile(filepath.Join(base, name))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// LinkEventType classifies a detected link-state change.
type LinkEventType string

// The link event types emitted by the monitor.
const (
	// CarrierLost fires when carrier transitions 1 -> 0.
	CarrierLost LinkEventType = "carrier_lost"
	// CarrierRestored fires when carrier transitions 0 -> 1.
	CarrierRestored LinkEventType = "carrier_restored"
	// SpeedChanged fires when SpeedMbps changes between two known values
	// (transitions to/from the -1 unknown sentinel are ignored).
	SpeedChanged LinkEventType = "speed_changed"
	// DuplexChanged fires when Duplex changes between two known values.
	DuplexChanged LinkEventType = "duplex_changed"
	// OperstateChanged fires when the operstate string changes.
	OperstateChanged LinkEventType = "operstate_changed"
	// Renegotiation fires when carrier_changes advanced by >=2 within one
	// interval (a full down/up flap the per-field comparison would miss), or
	// when speed/duplex changed while carrier stayed up.
	Renegotiation LinkEventType = "renegotiation"
)

// LinkEvent is one detected link-state change with the snapshots that bracket
// it. The Type string values match model.MonitoringEvent.Type so mapping to
// the report is a direct copy.
type LinkEvent struct {
	// At is the time the change was observed (the After snapshot's time).
	At time.Time
	// Type is the kind of change.
	Type LinkEventType
	// Detail is a human-readable description of the change.
	Detail string
	// SelfInflicted marks events observed during the coordinated cable-test
	// window; evaluators exclude them from instability findings.
	SelfInflicted bool
	// Before is the previous snapshot.
	Before LinkSnapshot
	// After is the snapshot in which the change was detected.
	After LinkSnapshot
}

// Monitor polls an interface's sysfs link state on a fixed interval and emits
// LinkEvents for carrier/speed/duplex/operstate changes and renegotiations.
// It is safe for concurrent use once Run is started; every accessor is
// mutex- or atomic-guarded.
type Monitor struct {
	ifName    string
	interval  time.Duration
	clk       clock.Clock
	sysfsRoot string

	ch              chan LinkEvent
	dropped         atomic.Uint64
	cableTestWindow atomic.Bool

	mu      sync.Mutex
	last    *LinkSnapshot
	current LinkSnapshot
	history []LinkEvent
}

// MonitorOption customizes a Monitor at construction.
type MonitorOption func(*Monitor)

// WithSysfsRoot overrides /sys/class/net for the monitor's sysfs reads; tests
// pass a t.TempDir tree. An empty root keeps the production default.
func WithSysfsRoot(root string) MonitorOption {
	return func(m *Monitor) { m.sysfsRoot = root }
}

// NewMonitor constructs a Monitor for ifName that polls every interval using
// clk. Pass WithSysfsRoot to inject a test sysfs tree. The monitor does not
// begin polling until Run is called.
func NewMonitor(ifName string, interval time.Duration, clk clock.Clock, opts ...MonitorOption) *Monitor {
	m := &Monitor{
		ifName:   ifName,
		interval: interval,
		clk:      clk,
		ch:       make(chan LinkEvent, eventChanCapacity),
	}
	for _, opt := range opts {
		opt(m)
	}
	// Capture the baseline synchronously on the constructing goroutine so the
	// first real change is measured against the link state as it was when the
	// monitor was created — before Run's goroutine and, in tests, before any
	// scripted sysfs rewrite. This makes the first detected event
	// deterministic under FakeClock.
	m.poll()
	return m
}

// Run polls the interface every interval until ctx is cancelled, emitting
// events for every detected change against the previous poll. It always Stops
// the ticker and returns ctx.Err() (context.Canceled/DeadlineExceeded) on
// exit. The baseline is taken by NewMonitor, so the first tick is already
// compared against a known state. Run blocks; start it in a goroutine.
func (m *Monitor) Run(ctx context.Context) error {
	ticker := m.clk.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C():
			m.poll()
		}
	}
}

// poll reads a fresh snapshot, records it as current, and emits any events the
// change from the previous snapshot implies.
func (m *Monitor) poll() {
	snap := ReadLinkSnapshot(m.sysfsRoot, m.ifName)
	snap.At = m.clk.Now()

	m.mu.Lock()
	prev := m.last
	m.current = snap
	stored := snap
	m.last = &stored
	var events []LinkEvent
	if prev != nil {
		events = detectEvents(*prev, snap)
		if m.cableTestWindow.Load() {
			for i := range events {
				events[i].SelfInflicted = true
				events[i].Detail += " (self-inflicted cable-test window)"
			}
		}
		m.history = append(m.history, events...)
	}
	m.mu.Unlock()

	for _, ev := range events {
		m.emit(ev)
	}
}

// SetCableTestWindow marks whether newly observed link events are expected
// consequences of the coordinated disruptive cable diagnostic.
func (m *Monitor) SetCableTestWindow(active bool) {
	m.cableTestWindow.Store(active)
}

// emit delivers an event on the buffered channel; when the buffer is full it
// drops the event and increments the dropped counter so the poller never
// blocks on a slow or absent consumer.
func (m *Monitor) emit(ev LinkEvent) {
	select {
	case m.ch <- ev:
	default:
		m.dropped.Add(1)
	}
}

// detectEvents compares two consecutive snapshots and returns the events the
// transition implies, in a stable order (carrier, speed, duplex, operstate,
// renegotiation).
func detectEvents(before, after LinkSnapshot) []LinkEvent {
	var events []LinkEvent
	mk := func(t LinkEventType, detail string) LinkEvent {
		return LinkEvent{At: after.At, Type: t, Detail: detail, Before: before, After: after}
	}

	carrierUp := before.Carrier == 1 && after.Carrier == 1

	if before.Carrier == 1 && after.Carrier == 0 {
		events = append(events, mk(CarrierLost, "carrier down (link lost)"))
	} else if before.Carrier == 0 && after.Carrier == 1 {
		events = append(events, mk(CarrierRestored, "carrier up (link restored)"))
	}

	// Speed change only between two known values; a transition to/from the -1
	// unknown sentinel (carrier drop/restore) must not read as "speed changed
	// to -1".
	speedChanged := before.SpeedMbps >= 0 && after.SpeedMbps >= 0 && before.SpeedMbps != after.SpeedMbps
	if speedChanged {
		events = append(events, mk(SpeedChanged,
			"speed "+strconv.Itoa(before.SpeedMbps)+"Mb/s -> "+strconv.Itoa(after.SpeedMbps)+"Mb/s"))
	}

	// Duplex change only between two known values.
	duplexChanged := isKnownDuplex(before.Duplex) && isKnownDuplex(after.Duplex) && before.Duplex != after.Duplex
	if duplexChanged {
		events = append(events, mk(DuplexChanged, "duplex "+before.Duplex+" -> "+after.Duplex))
	}

	if before.Operstate != after.Operstate {
		events = append(events, mk(OperstateChanged, "operstate "+before.Operstate+" -> "+after.Operstate))
	}

	// Renegotiation: a full down/up flap within one interval (carrier_changes
	// advanced by >=2 even if carrier/speed/duplex look identical), or a
	// speed/duplex change while the carrier stayed up (renegotiation without a
	// visible drop).
	flap := after.CarrierChanges >= before.CarrierChanges+2
	renegNoDrop := carrierUp && (speedChanged || duplexChanged)
	if flap || renegNoDrop {
		detail := "renegotiation without carrier drop"
		if flap {
			detail = "sub-interval link flap (carrier_changes +" +
				strconv.FormatUint(after.CarrierChanges-before.CarrierChanges, 10) + ")"
		}
		events = append(events, mk(Renegotiation, detail))
	}

	return events
}

// isKnownDuplex reports whether d is a definite duplex value (not "unknown").
func isKnownDuplex(d string) bool {
	return d == "full" || d == "half"
}

// Events returns the receive end of the buffered event channel. When the
// buffer is full the poller drops events (counted by Dropped) rather than
// blocking, so a consumer that falls behind never stalls monitoring.
func (m *Monitor) Events() <-chan LinkEvent {
	return m.ch
}

// History returns a copy of the full ordered event record — every event ever
// detected, independent of channel overflow — for inclusion in the report.
func (m *Monitor) History() []LinkEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LinkEvent, len(m.history))
	copy(out, m.history)
	return out
}

// Current returns the most recent snapshot; the zero value before the first
// poll.
func (m *Monitor) Current() LinkSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Dropped returns the number of events dropped because the Events buffer was
// full when they were produced.
func (m *Monitor) Dropped() uint64 {
	return m.dropped.Load()
}
