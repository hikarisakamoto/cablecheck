package testsuite

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// fakeCaller is a scripted peer.RemoteCaller: it records op order and params
// and answers each op from a FIFO of canned payloads.
type fakeCaller struct {
	t          *testing.T
	sessionCtx context.Context
	mu         sync.Mutex
	ops        []string
	params     map[string][]json.RawMessage
	replies    map[string][]any
}

func newFakeCaller(t *testing.T) *fakeCaller {
	return newFakeCallerWithSession(t, context.Background())
}

// newFakeCallerWithSession models the lifetime that remoteCaller observes in
// addition to each Call's context. WithoutCancel can keep a cleanup call's
// context alive, but it cannot revive an already-cancelled peer session.
func newFakeCallerWithSession(t *testing.T, sessionCtx context.Context) *fakeCaller {
	return &fakeCaller{
		t:          t,
		sessionCtx: sessionCtx,
		params:     map[string][]json.RawMessage{},
		replies:    map[string][]any{},
	}
}

// reply queues one canned ok result payload for op.
func (f *fakeCaller) reply(op string, payload any) { f.replies[op] = append(f.replies[op], payload) }

// scriptedResult is a fully specified canned reply — status, error text and
// optional payload — for exercising non-ok result paths.
type scriptedResult struct {
	status  string
	errText string
	payload any
}

// replyStatus queues one canned reply for op with an explicit status.
func (f *fakeCaller) replyStatus(op, status, errText string, payload any) {
	f.replies[op] = append(f.replies[op], scriptedResult{status: status, errText: errText, payload: payload})
}

func (f *fakeCaller) Call(ctx context.Context, op string, params any, timeout time.Duration,
	onProgress func(protocol.TestProgress)) (*protocol.TestResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.sessionCtx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	raw, err := json.Marshal(params)
	if err != nil {
		f.t.Errorf("marshal params for %s: %v", op, err)
		return nil, err
	}
	f.params[op] = append(f.params[op], raw)
	queue := f.replies[op]
	if len(queue) == 0 {
		err := fmt.Errorf("fakeCaller: no scripted reply for op %s", op)
		f.t.Errorf("%v", err)
		return nil, err
	}
	payload := queue[0]
	f.replies[op] = queue[1:]
	status, errText := StatusOK, ""
	if sr, ok := payload.(scriptedResult); ok {
		status, errText, payload = sr.status, sr.errText, sr.payload
	}
	var body json.RawMessage
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			f.t.Errorf("marshal reply for %s: %v", op, err)
			return nil, err
		}
	}
	return &protocol.TestResult{Status: status, Result: body, Error: errText}, nil
}

func (f *fakeCaller) Warn(code, text string) {}

func (f *fakeCaller) SetIdleTimeout(time.Duration) {}

// TestQuickPlanDrivesSteps runs the whole TCP-only quick plan against a
// scripted local runner and a scripted remote caller, checking step
// announcements, remote op order/params and the accumulated SessionResults.
func TestQuickPlanDrivesSteps(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		Result: fixture(t, "ethtool", "settings_e1000e_1g")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		Result: fixture(t, "ethtool", "stats_e1000e_clean")})
	fr.Script(runnertest.Script{Name: "ip", Result: fixture(t, "ip", "linkstats_clean")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-i", "0.02"),
		Result: fixture(t, "ping", "quick_clean_100")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		StdoutFile: fixturePath("ping", "fullsize_ok.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("--bidir"),
		StdoutFile: fixturePath("iperf", "bidir_314.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-u"),
		StdoutFile: fixturePath("iperf", "udp_316.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})

	ops := newTestOps(t, fr)
	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 1}})
	rc.reply(OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 100, IntervalUsedSec: 0.02}})
	rc.reply(OpPingFullSize, &PingRunResult{Ping: model.PingResult{Transmitted: 100, Received: 100}})
	rc.reply(OpIperfCaps, &model.Iperf3Caps{Version: "3.16", JSON: true, UDP: true, Bidir: true})
	// One one-off server per remote-hosted phase: TCP forward, bidir, UDP forward.
	for range 3 {
		rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
		rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	}
	rc.reply(OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{SenderBitsPerSecond: 5e8, ReceiverBitsPerSecond: 4.9e8}})
	rc.reply(OpIperfUDPRun, &UDPRunResult{UDP: model.UDPResult{
		TargetBps: 800000000, ActualSenderBps: 799958000, JitterMs: 0.02}})

	var steps []string
	results := &SessionResults{}
	plan := newQuickPlan(ops, results)
	plan.OnStep = func(step, total int, name string) {
		steps = append(steps, fmt.Sprintf("[%d/%d] %s", step, total, name))
	}
	if err := plan.Run(context.Background(), rc); err != nil {
		t.Fatalf("plan.Run: %v", err)
	}

	names := QuickPlanSteps()
	if len(steps) != len(names) {
		t.Fatalf("announced %d steps %q, want %d", len(steps), steps, len(names))
	}
	for i, name := range names {
		want := fmt.Sprintf("[%d/%d] %s", i+1, len(names), name)
		if steps[i] != want {
			t.Errorf("step %d announced %q, want %q", i, steps[i], want)
		}
	}

	wantOps := []string{
		OpLinkSettings,                        // [1] link settings
		OpCountersSnapshot,                    // [2] initial counters
		OpPingRun,                             // [3] ping stability
		OpPingFullSize,                        // [4] full-size ping
		OpIperfServerStart, OpIperfServerStop, // [5] TCP fwd (server on PC2)
		OpIperfClientRun,                                   // [6] TCP rev (client on PC2)
		OpIperfCaps, OpIperfServerStart, OpIperfServerStop, // [7] native bidir
		OpIperfServerStart, OpIperfServerStop, OpIperfUDPRun, // [8] UDP both directions
		OpCountersSnapshot, // [9] final counters
	}
	if !slices.Equal(rc.ops, wantOps) {
		t.Errorf("remote op order = %q, want %q", rc.ops, wantOps)
	}

	var startParams IperfServerStartParams
	if err := json.Unmarshal(rc.params[OpIperfServerStart][0], &startParams); err != nil {
		t.Fatalf("unmarshal server start params: %v", err)
	}
	if startParams.BindIP != "10.0.0.2" || startParams.Port != 5201 {
		t.Errorf("remote server start params = %+v, want bind 10.0.0.2:5201 (the worker's own IP)", startParams)
	}
	var clientParams IperfClientRunParams
	if err := json.Unmarshal(rc.params[OpIperfClientRun][0], &clientParams); err != nil {
		t.Fatalf("unmarshal client run params: %v", err)
	}
	if clientParams.LocalIP != "10.0.0.2" || clientParams.PeerIP != "10.0.0.1" ||
		clientParams.Port != 5201 || clientParams.DurationSec != 30 || clientParams.Streams != 4 {
		t.Errorf("remote client params = %+v, want the reverse direction toward 10.0.0.1", clientParams)
	}
	var pingParams PingRunParams
	if err := json.Unmarshal(rc.params[OpPingRun][0], &pingParams); err != nil {
		t.Fatalf("unmarshal ping params: %v", err)
	}
	if pingParams.PeerIP != "10.0.0.1" || pingParams.Count != 100 {
		t.Errorf("remote ping params = %+v, want the worker pinging 10.0.0.1", pingParams)
	}

	assertQuickResults(t, results)
}

// assertQuickResults checks the SessionResults accumulator after a clean run.
func assertQuickResults(t *testing.T, results *SessionResults) {
	t.Helper()
	if results.Incomplete {
		t.Errorf("clean run marked incomplete")
	}
	if results.Link[RolePC1Key] == nil || results.Link[RolePC2Key] == nil {
		t.Errorf("link settings missing a side: %+v", results.Link)
	}
	if results.InitialCounters.PC1 == nil || results.InitialCounters.PC2 == nil {
		t.Errorf("initial counters missing a side")
	}
	if results.FinalCounters.PC1 == nil || results.FinalCounters.PC2 == nil {
		t.Errorf("final counters missing a side")
	}
	if got := directions(t, results.Ping, func(p model.PingResult) string { return p.Direction }); !slices.Equal(got,
		[]string{model.DirectionPC1ToPC2, model.DirectionPC2ToPC1}) {
		t.Errorf("ping directions = %q, want both directions in order", got)
	}
	if got := directions(t, results.TCP, func(r model.TCPResult) string { return r.Direction }); !slices.Equal(got,
		[]string{model.DirectionPC1ToPC2, model.DirectionPC2ToPC1}) {
		t.Errorf("tcp directions = %q, want both directions in order", got)
	}
	if got := directions(t, results.FullSizePing, func(p model.PingResult) string { return p.Direction }); !slices.Equal(got,
		[]string{model.DirectionPC1ToPC2, model.DirectionPC2ToPC1}) {
		t.Errorf("full-size ping directions = %q, want both directions in order", got)
	}
	if got := directions(t, results.UDP, func(u model.UDPResult) string { return u.Direction }); !slices.Equal(got,
		[]string{model.DirectionPC1ToPC2, model.DirectionPC2ToPC1}) {
		t.Errorf("udp directions = %q, want both directions in order", got)
	}
	if results.Bidir == nil {
		t.Errorf("results.Bidir = nil, want the bidirectional stress result")
	} else if results.Bidir.TwoPhaseFallback {
		t.Errorf("Bidir.TwoPhaseFallback = true, want native --bidir when both peers support it")
	}
}

// directions maps a result slice to its direction labels.
func directions[T any](t *testing.T, items []T, key func(T) string) []string {
	t.Helper()
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, key(it))
	}
	return out
}

// newQuickPlan builds a QuickPlan over ops with the canonical test topology
// (PC1 10.0.0.1, PC2 10.0.0.2, 30s TCP, 20s UDP, 4 streams, 100-packet ping,
// MTU 1500, bidir-capable local iperf3).
func newQuickPlan(ops *Ops, results *SessionResults) *QuickPlan {
	return &QuickPlan{
		Ops:            ops,
		LocalIP:        netip.MustParseAddr("10.0.0.1"),
		PeerIP:         netip.MustParseAddr("10.0.0.2"),
		IperfPort:      5201,
		TCPDuration:    30 * time.Second,
		UDPDuration:    20 * time.Second,
		MTU:            1500,
		Streams:        4,
		PingCount:      100,
		LocalIperfCaps: model.Iperf3Caps{Version: "3.16", JSON: true, Reverse: true, UDP: true, Bidir: true, OneOff: true},
		Results:        results,
	}
}

// scriptPreTCPSteps scripts the local runner and the fake caller for the
// steps preceding the forward TCP phase (link, initial counters, ping).
func scriptPreTCPSteps(t *testing.T, fr *runnertest.FakeRunner, rc *fakeCaller) {
	t.Helper()
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		Result: fixture(t, "ethtool", "settings_e1000e_1g")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		Result: fixture(t, "ethtool", "stats_e1000e_clean")})
	fr.Script(runnertest.Script{Name: "ip", Result: fixture(t, "ip", "linkstats_clean")})
	fr.Script(runnertest.Script{Name: "ping", Result: fixture(t, "ping", "quick_clean_100")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		StdoutFile: fixturePath("ping", "fullsize_ok.txt")})
	rc.reply(OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000, Duplex: "full"}})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}})
	rc.reply(OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 100, IntervalUsedSec: 0.02}})
	rc.reply(OpPingFullSize, &PingRunResult{Ping: model.PingResult{Transmitted: 100, Received: 100}})
}

// TestQuickPlanPreservesPartialTCPForward aborts the local forward TCP client
// (runner-timeout kill mid-test): the partial result RunTCPClient returns
// alongside its error must land in SessionResults.TCP instead of being
// dropped, the run is marked incomplete, and the remote one-off server is
// still stopped.
func TestQuickPlanPreservesPartialTCPForward(t *testing.T) {
	fr := runnertest.New(t)
	rc := newFakeCaller(t)
	scriptPreTCPSteps(t, fr, rc)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		Result: runner.CommandResult{TimedOut: true, ExitCode: -1, Signal: "SIGKILL"}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.Run(context.Background(), rc); err == nil {
		t.Fatalf("plan.Run succeeded despite the client timing out")
	}
	if !results.Incomplete {
		t.Errorf("aborted run not marked Incomplete")
	}
	if len(results.TCP) != 1 {
		t.Fatalf("results.TCP has %d entries %+v, want the 1 partial forward result", len(results.TCP), results.TCP)
	}
	got := results.TCP[0]
	if !got.Incomplete {
		t.Errorf("partial TCP result Incomplete = false, want true")
	}
	if got.Direction != model.DirectionPC1ToPC2 {
		t.Errorf("partial result direction = %q, want %q", got.Direction, model.DirectionPC1ToPC2)
	}
	if got.Duration != model.Duration(30*time.Second) || got.ParallelStreams != 4 {
		t.Errorf("partial result lost its run parameters: %+v, want 30s / 4 streams", got)
	}
	if !slices.Contains(rc.ops, OpIperfServerStop) {
		t.Errorf("remote ops %q lack %s: the one-off server leaked after the abort", rc.ops, OpIperfServerStop)
	}
}

// TestQuickPlanPreservesPartialTCPReverse fails the remote iperf3 client run
// with a partial payload attached to the failed status, as HandleOp produces
// for an aborted run: the coordinator must decode the non-ok payload and
// keep the partial TCP result.
func TestQuickPlanPreservesPartialTCPReverse(t *testing.T) {
	fr := runnertest.New(t)
	rc := newFakeCaller(t)
	scriptPreTCPSteps(t, fr, rc)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	rc.replyStatus(OpIperfClientRun, StatusFailed, "iperf3 client aborted mid-test",
		&TCPRunResult{Incomplete: true, TCP: model.TCPResult{
			Duration: model.Duration(30 * time.Second), ParallelStreams: 4}})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.Run(context.Background(), rc); err == nil {
		t.Fatalf("plan.Run succeeded despite the remote client failure")
	}
	if !results.Incomplete {
		t.Errorf("aborted run not marked Incomplete")
	}
	if len(results.TCP) != 2 {
		t.Fatalf("results.TCP has %d entries %+v, want forward + partial reverse", len(results.TCP), results.TCP)
	}
	partial := results.TCP[1]
	if !partial.Incomplete {
		t.Errorf("partial TCP result Incomplete = false, want true")
	}
	if partial.Direction != model.DirectionPC2ToPC1 {
		t.Errorf("partial result direction = %q, want %q", partial.Direction, model.DirectionPC2ToPC1)
	}
	if partial.Duration != model.Duration(30*time.Second) || partial.ParallelStreams != 4 {
		t.Errorf("partial result lost its run parameters: %+v, want 30s / 4 streams", partial)
	}
}

func TestQuickPlanRecordsUnavailableRemotePing(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ping", Result: fixture(t, "ping", "quick_clean_100")})
	rc := newFakeCaller(t)
	rc.replyStatus(OpPingRun, StatusUnavailable, "ping disappeared after preflight", nil)

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.stepPing(context.Background(), rc); err != nil {
		t.Fatalf("stepPing: %v", err)
	}
	if len(results.Ping) != 1 {
		t.Errorf("results.Ping has %d entries, want the completed local direction only", len(results.Ping))
	}
	want := model.SkippedTest{Name: "ping", Reason: "peer could not run ping_run: ping disappeared after preflight"}
	if !slices.Contains(results.SkippedTests, want) {
		t.Errorf("SkippedTests = %+v, want %+v", results.SkippedTests, want)
	}
}
