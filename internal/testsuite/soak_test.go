package testsuite

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/config"
	"cablecheck/internal/evaluate"
	"cablecheck/internal/model"
	"cablecheck/internal/runner/runnertest"
	"cablecheck/internal/testutil"
)

// newSoakPlan builds a SoakPlan over ops with a soak topology: 60s TCP per
// cycle, 20s UDP per cycle, one TCP repeat per cycle, a 1500ms per-cycle gap
// for the periodic load profile, and the given soak duration/profile.
func newSoakPlan(ops *Ops, results *SessionResults, clk clock.Clock,
	soakDuration time.Duration, load config.SoakLoad) *SoakPlan {
	return &SoakPlan{
		Ops:            ops,
		Clock:          clk,
		LocalIP:        netip.MustParseAddr("10.0.0.1"),
		PeerIP:         netip.MustParseAddr("10.0.0.2"),
		IperfPort:      5201,
		TCPDuration:    60 * time.Second,
		UDPDuration:    20 * time.Second,
		TCPRepeats:     1,
		MTU:            1500,
		Streams:        4,
		PingCount:      500,
		LocalIperfCaps: model.Iperf3Caps{Version: "3.16", JSON: true, Reverse: true, UDP: true, Bidir: true, OneOff: true},
		SoakDuration:   soakDuration,
		SoakLoad:       load,
		CycleGap:       1500 * time.Millisecond,
		Results:        results,
	}
}

// scriptSoakCycle scripts one runner + caller for a single soak cycle: local
// ping, forward TCP client, reverse TCP server, forward+reverse UDP, and a
// per-cycle counter snapshot. Unlimited (Times:0) so it serves every cycle.
func scriptSoakCycle(fr *runnertest.FakeRunner) {
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		StdoutFile: fixturePath("ethtool", "settings_e1000e_1g.txt")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		StdoutFile: fixturePath("ethtool", "stats_e1000e_clean.txt")})
	fr.Script(runnertest.Script{Name: "ip", StdoutFile: fixturePath("ip", "linkstats_clean.json")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("ping", "quick_clean_100.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	// -c is scripted first so the later, more-specific -u script wins for UDP
	// client calls (which carry both -c and -u); TCP client calls carry only -c.
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-u"),
		StdoutFile: fixturePath("iperf", "udp_316.json")})
}

// replySoakCycle queues one cycle's worth of remote replies on rc (unlimited
// reuse is not possible on the FIFO caller, so replies are queued per cycle).
func replySoakCycle(rc *fakeCaller) {
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}})
	rc.reply(OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 500, IntervalUsedSec: 0.02}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	rc.reply(OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{SenderBitsPerSecond: 5e8, ReceiverBitsPerSecond: 4.9e8}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	rc.reply(OpIperfUDPRun, &UDPRunResult{UDP: model.UDPResult{TargetBps: 800000000, ActualSenderBps: 7.9e8, JitterMs: 0.02}})
}

// TestSoakPeriodicCycles drives a periodic soak with FakeClock: the soak
// duration spans exactly three cycles, and the test asserts three cycles run,
// each with its own counter snapshot, and that the periodic idle gap is
// honored between cycles (a clock waiter appears after each cycle).
func TestSoakPeriodicCycles(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	scriptSoakCycle(fr)
	rc := newFakeCaller(t)
	// Three cycles require eight counter snapshots: the whole-run initial and
	// final boundaries plus two snapshots for each completed cycle. Four calls
	// to replySoakCycle provide exactly those eight counter replies, along with
	// enough non-counter replies for the three cycles that fit in the budget.
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	for range 4 {
		replySoakCycle(rc)
	}

	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	results := &SessionResults{}
	// Two full 1500ms gaps and a final gap bounded to the remaining 1s fit
	// exactly inside the 4s budget.
	plan := newSoakPlan(newTestOps(t, fr), results, clk, 4*time.Second, config.SoakLoadPeriodic)

	done := make(chan error, 1)
	go func() { done <- plan.Run(context.Background(), rc) }()

	for _, advance := range []time.Duration{1500 * time.Millisecond, 1500 * time.Millisecond, time.Second} {
		// One waiter is the whole-run budget and the other is the current gap.
		clk.BlockUntilWaiters(2)
		clk.Advance(advance)
	}

	if err := <-done; err != nil {
		t.Fatalf("soak Run: %v", err)
	}
	if results.CyclesCompleted != 3 {
		t.Fatalf("CyclesCompleted = %d, want 3", results.CyclesCompleted)
	}
	if len(results.CycleCounters) != 3 {
		t.Errorf("per-cycle counter snapshots = %d, want one per cycle (3)", len(results.CycleCounters))
	}
	for i, cc := range results.CycleCounters {
		if cc.PC1 == nil {
			t.Errorf("cycle %d counter snapshot missing the local side", i)
		}
	}
	// The three cycles' TCP results accumulated (one repeat per direction).
	if len(results.TCP) != 6 {
		t.Errorf("TCP results = %d, want 2 per cycle × 3 cycles", len(results.TCP))
	}
}

// TestSoakContinuousRunsBackToBackAndHonorsBudget gates cycle two on its TCP
// client without advancing the clock first. That proves there is no idle
// timer between cycles; advancing the one whole-run budget timer then cancels
// and discards the in-flight cycle at the exact deadline.
func TestSoakContinuousRunsBackToBackAndHonorsBudget(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	scriptSoakCycle(fr)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"), Times: 1,
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})
	started := make(chan struct{})
	hang := make(chan struct{})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json"), Delay: hang, Started: started})
	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	for range 3 {
		replySoakCycle(rc)
	}

	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	results := &SessionResults{}
	budget := time.Second
	plan := newSoakPlan(newTestOps(t, fr), results, clk, budget, config.SoakLoadContinuous)
	plan.UDPDuration = 0

	done := make(chan error, 1)
	go func() { done <- plan.Run(context.Background(), rc) }()

	testutil.WaitFor(t, started, "second continuous cycle did not start back-to-back")
	clk.BlockUntilWaiters(1)
	clk.Advance(budget)

	if err := <-done; err != nil {
		t.Fatalf("soak Run: %v", err)
	}
	if results.CyclesCompleted != 1 {
		t.Fatalf("continuous CyclesCompleted = %d, want only the committed first cycle", results.CyclesCompleted)
	}
	if elapsed := clk.Now().Sub(time.Unix(1_700_000_000, 0)); elapsed != budget {
		t.Errorf("continuous elapsed = %v, want exact budget %v", elapsed, budget)
	}
}

// TestSoakCancelDiscardsPartialCycle cancels after cycle two's ping, while its
// first TCP pass is starting. The completed first cycle remains intact and no
// measurement from the uncommitted second cycle leaks into the session.
func TestSoakCancelDiscardsPartialCycle(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		StdoutFile: fixturePath("ethtool", "settings_e1000e_1g.txt")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		StdoutFile: fixturePath("ethtool", "stats_e1000e_clean.txt")})
	fr.Script(runnertest.Script{Name: "ip", StdoutFile: fixturePath("ip", "linkstats_clean.json")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("ping", "quick_clean_100.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	// The first cycle's forward TCP client completes normally; the second
	// cycle's forward TCP client hangs until the context is cancelled.
	started := make(chan struct{})
	hang := make(chan struct{}) // never closed: hangs until cancel
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"), Times: 1,
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json"), Delay: hang, Started: started})

	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	for range 3 {
		replySoakCycle(rc)
	}

	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	results := &SessionResults{}
	plan := newSoakPlan(newTestOps(t, fr), results, clk, time.Hour, config.SoakLoadContinuous)
	// No per-cycle UDP here: the cancellation gate is the second cycle's
	// forward TCP client, so each cycle is counters+ping+TCP only, keeping the
	// hang precisely on TCP.
	plan.UDPDuration = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- plan.Run(ctx, rc) }()

	// Continuous mode begins cycle two immediately after committing cycle one.
	testutil.WaitFor(t, started, "second cycle TCP client never started")
	cancel()

	err := <-done
	if err == nil {
		t.Fatalf("cancelled soak Run returned nil, want a context error")
	}
	// The first, fully completed cycle must be retained.
	if results.CyclesCompleted != 1 {
		t.Errorf("CyclesCompleted = %d, want the 1 fully completed cycle retained", results.CyclesCompleted)
	}
	if len(results.CycleCounters) != 1 {
		t.Errorf("retained per-cycle counters = %d, want 1", len(results.CycleCounters))
	}
	if !results.Incomplete {
		t.Errorf("cancelled soak not marked Incomplete")
	}
	if len(results.Ping) != 2 {
		t.Errorf("Ping results = %d, want only cycle one's two directions", len(results.Ping))
	}
	if len(results.TCP) != 2 {
		t.Errorf("TCP results = %d, want only cycle one's two directions", len(results.TCP))
	}
}

// TestSoakCancelFinalCountersRetainLocalAndLeaveRemoteUnavailable models the
// peer session being torn down with the caller context. The deferred final
// boundary uses WithoutCancel, so PC1 can still capture locally, but the
// RemoteCaller must reject PC2's snapshot once its session context is done.
func TestSoakCancelFinalCountersRetainLocalAndLeaveRemoteUnavailable(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		StdoutFile: fixturePath("ethtool", "settings_e1000e_1g.txt")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		StdoutFile: fixturePath("ethtool", "stats_e1000e_clean.txt")})
	fr.Script(runnertest.Script{Name: "ip", StdoutFile: fixturePath("ip", "linkstats_clean.json")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("ping", "quick_clean_100.txt")})
	started := make(chan struct{})
	hang := make(chan struct{})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json"), Delay: hang, Started: started})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := newFakeCallerWithSession(t, ctx)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 7}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 8}})
	rc.reply(OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 500, IntervalUsedSec: 0.02}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})

	results := &SessionResults{}
	plan := newSoakPlan(newTestOps(t, fr), results,
		clocktest.New(time.Unix(1_700_000_000, 0)), time.Hour, config.SoakLoadContinuous)
	plan.UDPDuration = 0

	done := make(chan error, 1)
	go func() { done <- plan.Run(ctx, rc) }()
	testutil.WaitFor(t, started, "first soak TCP client never started")
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled soak Run error = %v, want context cancellation", err)
	}
	if results.FinalCounters.PC1 == nil {
		t.Fatal("final PC1 counter snapshot missing after cancellation")
	}
	if results.FinalCounters.PC2 != nil {
		t.Fatalf("final PC2 counter snapshot = %+v, want absent after session cancellation", results.FinalCounters.PC2)
	}
	deltas, deltaOK := evaluate.DeltaSet(results.InitialCounters.PC2, results.FinalCounters.PC2)
	if deltaOK || len(deltas) != 0 {
		t.Errorf("PC2 deltas = (%v, %v), want unavailable empty set", deltas, deltaOK)
	}
	report := &model.Report{
		InitialCounters: results.InitialCounters,
		FinalCounters:   results.FinalCounters,
		Partial:         results.Incomplete,
	}
	facts := evaluate.FactsFromReport(report)
	if facts.PC2.CountersAvailable || facts.PC2.DeltaOK {
		t.Errorf("PC2 facts = %+v, want counters unavailable with DeltaOK=false", facts.PC2)
	}
	if !results.Incomplete || !facts.Partial {
		t.Errorf("cancelled run markers: Incomplete=%v Partial=%v, want both true", results.Incomplete, facts.Partial)
	}
}

func TestSoakContinuousHasNoIdleGap(t *testing.T) {
	continuous := newSoakPlan(nil, &SessionResults{}, clocktest.New(time.Unix(1_700_000_000, 0)), time.Hour, config.SoakLoadContinuous)
	if got := continuous.gap(); got != 0 {
		t.Errorf("continuous gap = %v, want no idle gap", got)
	}

	periodic := newSoakPlan(nil, &SessionResults{}, clocktest.New(time.Unix(1_700_000_000, 0)), time.Hour, config.SoakLoadPeriodic)
	if got := periodic.gap(); got != periodic.CycleGap {
		t.Errorf("periodic gap = %v, want configured %v", got, periodic.CycleGap)
	}
}

func TestSoakPeriodicGapBoundedByRemainingBudget(t *testing.T) {
	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	plan := newSoakPlan(nil, &SessionResults{}, clk, time.Second, config.SoakLoadPeriodic)
	done := make(chan error, 1)
	go func() { done <- plan.waitGap(context.Background(), time.Second) }()
	clk.BlockUntilWaiters(1)
	clk.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("waitGap: %v", err)
	}
	if elapsed := clk.Now().Sub(time.Unix(1_700_000_000, 0)); elapsed != time.Second {
		t.Errorf("elapsed = %v, want tight soak budget %v", elapsed, time.Second)
	}
}

type recordingSoakClock struct {
	*clocktest.FakeClock
	afterCalls chan time.Time
}

func (c *recordingSoakClock) After(d time.Duration) <-chan time.Time {
	c.afterCalls <- c.Now()
	return c.FakeClock.After(d)
}

// TestSoakDurationStartsBeforeInitialSetup gates the initial link inspection
// while fake wall-clock time advances. The whole-run budget timer must have
// been registered at plan entry, not after link and counter setup finish.
func TestSoakDurationStartsBeforeInitialSetup(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	scriptSoakCycle(fr)
	linkStarted := make(chan struct{})
	linkDelay := make(chan struct{})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"), Times: 1,
		StdoutFile: fixturePath("ethtool", "settings_e1000e_1g.txt"), Started: linkStarted, Delay: linkDelay})

	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	for range 3 {
		rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}})
	}

	start := time.Unix(1_700_000_000, 0)
	baseClock := clocktest.New(start)
	clk := &recordingSoakClock{FakeClock: baseClock, afterCalls: make(chan time.Time, 1)}
	results := &SessionResults{}
	plan := newSoakPlan(newTestOps(t, fr), results, clk, 10*time.Second, config.SoakLoadContinuous)
	plan.UDPDuration = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- plan.Run(ctx, rc) }()
	testutil.WaitFor(t, linkStarted, "initial soak link inspection")

	// The test-owned waiter lets setup consume deterministic fake wall time
	// even in the buggy implementation where the plan has no timer yet.
	baseClock.After(3 * time.Second)
	baseClock.BlockUntilWaiters(1)
	baseClock.Advance(3 * time.Second)
	close(linkDelay)

	registeredAt := <-clk.afterCalls
	cancel()
	_ = <-done
	if !registeredAt.Equal(start) {
		t.Errorf("soak budget timer registered at %v, want plan start %v", registeredAt, start)
	}
}

func TestSoakFinalCountersIncludeLastCycle(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	scriptSoakCycle(fr)
	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	// Counter snapshots are consumed in this order: whole-run initial,
	// cycle-local pre-load, cycle-local post-load, whole-run final. Keep the
	// intermediate values distinct so the asserted 10 -> 11 boundary delta
	// proves the deferred final capture consumed the last reply.
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 10}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 20}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 21}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 11}})
	rc.reply(OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 500, IntervalUsedSec: 0.02}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	rc.reply(OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{SenderBitsPerSecond: 5e8, ReceiverBitsPerSecond: 4.9e8}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	rc.reply(OpIperfUDPRun, &UDPRunResult{UDP: model.UDPResult{TargetBps: 800000000, ActualSenderBps: 7.9e8}})

	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	results := &SessionResults{}
	plan := newSoakPlan(newTestOps(t, fr), results, clk, 1500*time.Millisecond, config.SoakLoadPeriodic)
	done := make(chan error, 1)
	go func() { done <- plan.Run(context.Background(), rc) }()
	// The persistent budget timer is waiter one. Waiter two is registered only
	// after cycle one has consumed both cycle-local snapshots and entered the
	// periodic gap, making this the deterministic point to expire the budget.
	clk.BlockUntilWaiters(2)
	clk.Advance(1500 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("soak Run: %v", err)
	}

	delta, ok := counterDeltaForTest(results.InitialCounters.PC2, results.FinalCounters.PC2, "rx_crc")
	if !ok || delta != 1 {
		t.Errorf("whole-run PC2 rx_crc delta = (%d, %v), want (1, true)", delta, ok)
	}
}

// TestSoakDeadlineDuringFirstCycleStillBoundsWholeRunCounters advances the
// soak budget while cycle one's first TCP client is in flight. The cycle's
// traffic stays transactional, but independent before/after counter captures
// must still bound counter movement caused by that discarded load.
func TestSoakDeadlineDuringFirstCycleStillBoundsWholeRunCounters(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		StdoutFile: fixturePath("ethtool", "settings_e1000e_1g.txt")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		StdoutFile: fixturePath("ethtool", "stats_e1000e_clean.txt")})
	fr.Script(runnertest.Script{Name: "ip", StdoutFile: fixturePath("ip", "linkstats_clean.json")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("ping", "quick_clean_100.txt")})
	started := make(chan struct{})
	hang := make(chan struct{})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json"), Delay: hang, Started: started})

	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	// Independent initial boundary, cycle-local pre-load snapshot, then the
	// independent final boundary captured after the deadline cancels traffic.
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 10}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 10}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 13}})
	rc.reply(OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 500, IntervalUsedSec: 0.02}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})

	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	results := &SessionResults{}
	budget := time.Second
	plan := newSoakPlan(newTestOps(t, fr), results, clk, budget, config.SoakLoadContinuous)
	plan.UDPDuration = 0

	done := make(chan error, 1)
	go func() { done <- plan.Run(context.Background(), rc) }()
	testutil.WaitFor(t, started, "first soak cycle TCP client never started")
	clk.BlockUntilWaiters(1)
	clk.Advance(budget)

	if err := <-done; err != nil {
		t.Fatalf("soak Run: %v", err)
	}
	if results.CyclesCompleted != 0 || len(results.TCP) != 0 {
		t.Fatalf("discarded cycle leaked results: cycles=%d TCP=%d", results.CyclesCompleted, len(results.TCP))
	}
	delta, ok := counterDeltaForTest(results.InitialCounters.PC2, results.FinalCounters.PC2, "rx_crc")
	if !ok || delta != 3 {
		t.Errorf("whole-run PC2 rx_crc delta = (%d, %v), want (3, true)", delta, ok)
	}
}

func counterDeltaForTest(before, after *model.CounterSnapshot, name string) (uint64, bool) {
	if before == nil || after == nil {
		return 0, false
	}
	b, bok := before.Standard[name]
	a, aok := after.Standard[name]
	if !bok || !aok || a < b {
		return 0, false
	}
	return a - b, true
}

// TestSoakEventLog scripts a link renegotiation during a soak run and asserts
// the plan records the monitor's events in the session results timeline so the
// report reflects mid-soak link disturbances.
func TestSoakEventLog(t *testing.T) {
	defer testutil.LeakCheck(t)

	fr := runnertest.New(t)
	scriptSoakCycle(fr)
	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	for range 2 {
		replySoakCycle(rc)
	}

	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	results := &SessionResults{}
	// Budget below one CycleGap: the single cycle runs, then its inter-cycle
	// gap pushes the clock past the budget so the loop stops at one cycle.
	plan := newSoakPlan(newTestOps(t, fr), results, clk, time.Second, config.SoakLoadPeriodic)

	// A scripted event source stands in for the link monitor: it hands the
	// plan a disconnect and a renegotiation event, which the plan must fold
	// into results.MonitoringEvents.
	events := []model.MonitoringEvent{
		{At: clk.Now(), Type: "carrier_lost", Detail: "carrier dropped mid-soak"},
		{At: clk.Now(), Type: "renegotiation", Detail: "link renegotiated 1000->100 Mb/s"},
	}
	var mu sync.Mutex
	plan.EventSource = func() []model.MonitoringEvent {
		mu.Lock()
		defer mu.Unlock()
		return events
	}

	done := make(chan error, 1)
	go func() { done <- plan.Run(context.Background(), rc) }()
	clk.BlockUntilWaiters(2)
	clk.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("soak Run: %v", err)
	}

	if len(results.MonitoringEvents) != 2 {
		t.Fatalf("MonitoringEvents = %+v, want the scripted disconnect + renegotiation", results.MonitoringEvents)
	}
	if results.MonitoringEvents[0].Type != "carrier_lost" || results.MonitoringEvents[1].Type != "renegotiation" {
		t.Errorf("event timeline = %+v, want carrier_lost then renegotiation", results.MonitoringEvents)
	}
}
