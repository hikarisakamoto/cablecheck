package testsuite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/evaluate"
	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
	"cablecheck/internal/testutil"
)

// fakeCaller is a scripted peer.RemoteCaller: it records op order and params
// and answers each op from a FIFO of canned payloads.
type fakeCaller struct {
	t          *testing.T
	sessionCtx context.Context
	mu         sync.Mutex
	ops        []string
	calls      []fakeCall
	params     map[string][]json.RawMessage
	replies    map[string][]any
	step       fakeStep
	steps      []fakeStep
}

type fakeStep struct {
	step  int
	total int
	name  string
}

type fakeCall struct {
	op       string
	timeout  time.Duration
	detached bool
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

// scriptedCallError makes a fake RPC wait at a test-owned synchronization
// point before returning err. A nil wait returns immediately.
type scriptedCallError struct {
	err     error
	started chan<- struct{}
	wait    <-chan struct{}
}

// replyStatus queues one canned reply for op with an explicit status.
func (f *fakeCaller) replyStatus(op, status, errText string, payload any) {
	f.replies[op] = append(f.replies[op], scriptedResult{status: status, errText: errText, payload: payload})
}

func (f *fakeCaller) replyError(op string, err error) {
	f.replies[op] = append(f.replies[op], scriptedCallError{err: err})
}

func (f *fakeCaller) replyHang(op string, started chan<- struct{}, until <-chan struct{}, err error) {
	f.replies[op] = append(f.replies[op], scriptedCallError{err: err, started: started, wait: until})
}

func (f *fakeCaller) Call(ctx context.Context, op string, params any, timeout time.Duration,
	onProgress func(protocol.TestProgress)) (*protocol.TestResult, error) {
	return f.call(ctx, op, params, timeout, onProgress, false)
}

func (f *fakeCaller) CallDetached(ctx context.Context, op string, params any,
	timeout time.Duration) (*protocol.TestResult, error) {
	return f.call(ctx, op, params, timeout, nil, true)
}

func (f *fakeCaller) call(ctx context.Context, op string, params any, timeout time.Duration,
	onProgress func(protocol.TestProgress), detached bool) (*protocol.TestResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{op: op, timeout: timeout, detached: detached})
	f.mu.Unlock()
	if err := f.sessionCtx.Err(); err != nil {
		return nil, peer.ErrSessionClosed
	}
	if onProgress != nil {
		onProgress(protocol.TestProgress{Stage: op, Percent: -1, Text: "running " + op})
	}
	f.mu.Lock()
	f.ops = append(f.ops, op)
	raw, err := json.Marshal(params)
	if err != nil {
		f.mu.Unlock()
		f.t.Errorf("marshal params for %s: %v", op, err)
		return nil, err
	}
	f.params[op] = append(f.params[op], raw)
	queue := f.replies[op]
	if len(queue) == 0 {
		f.mu.Unlock()
		err := fmt.Errorf("fakeCaller: no scripted reply for op %s", op)
		f.t.Errorf("%v", err)
		return nil, err
	}
	payload := queue[0]
	f.replies[op] = queue[1:]
	f.mu.Unlock()
	if scripted, ok := payload.(scriptedCallError); ok {
		if scripted.started != nil {
			close(scripted.started)
		}
		if scripted.wait != nil {
			select {
			case <-scripted.wait:
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-f.sessionCtx.Done():
				return nil, peer.ErrSessionClosed
			}
		}
		return nil, scripted.err
	}
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

func TestPlanProgressObserverPropagation(t *testing.T) {
	var got []protocol.TestProgress
	observe := func(p protocol.TestProgress) { got = append(got, p) }

	rc := newFakeCaller(t)
	rc.reply(OpLinkSettings, &LinkSettingsResult{})
	quick := &QuickPlan{Results: &SessionResults{}, OnProgress: observe}
	if _, err := quick.callRemote(context.Background(), rc, OpLinkSettings, nil, time.Second); err != nil {
		t.Fatalf("quick callRemote: %v", err)
	}

	standard := (&StandardPlan{OnProgress: observe}).engine()
	standard.OnProgress(protocol.TestProgress{Stage: "standard"})
	soak := (&SoakPlan{OnProgress: observe}).engine(&SessionResults{})
	soak.OnProgress(protocol.TestProgress{Stage: "soak"})

	want := []string{OpLinkSettings, "standard", "soak"}
	if len(got) != len(want) {
		t.Fatalf("progress updates = %+v, want stages %q", got, want)
	}
	for i, stage := range want {
		if got[i].Stage != stage {
			t.Errorf("progress update %d stage = %q, want %q", i, got[i].Stage, stage)
		}
	}
}

func (f *fakeCaller) Warn(code, text string) {}

func (f *fakeCaller) SetIdleTimeout(time.Duration) {}

func (f *fakeCaller) SetStep(step, total int, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step = fakeStep{step: step, total: total, name: name}
	f.steps = append(f.steps, f.step)
}

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
	if len(rc.steps) != len(names) {
		t.Fatalf("attached %d remote steps %+v, want %d", len(rc.steps), rc.steps, len(names))
	}
	for i, name := range names {
		want := fmt.Sprintf("[%d/%d] %s", i+1, len(names), name)
		if steps[i] != want {
			t.Errorf("step %d announced %q, want %q", i, steps[i], want)
		}
		if got := rc.steps[i]; got != (fakeStep{step: i + 1, total: len(names), name: name}) {
			t.Errorf("remote step %d = %+v, want metadata matching local announcement", i, got)
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
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 1}})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.Run(context.Background(), rc); err == nil {
		t.Fatalf("plan.Run succeeded despite the client timing out")
	}
	if !results.Incomplete {
		t.Errorf("aborted run not marked Incomplete")
	}
	if results.FinalCounters.PC1 == nil {
		t.Errorf("final counters = %+v, want the local side salvaged after the abort", results.FinalCounters)
	}
	if results.FinalCounters.PC2 == nil {
		t.Errorf("final counters = %+v, want the responsive peer salvaged after the abort", results.FinalCounters)
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

// TestQuickPlanAbortsOnMissingTCPSummary covers the clean-exit schema-drift
// path: the semantic parse error must remain fatal, while the decoded result is
// recorded as incomplete and the remote one-off server is still stopped.
func TestQuickPlanAbortsOnMissingTCPSummary(t *testing.T) {
	fr := runnertest.New(t)
	rc := newFakeCaller(t)
	scriptPreTCPSteps(t, fr, rc)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_no_summary.json")})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 1}})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	err := plan.Run(context.Background(), rc)
	var parseErr *parser.ParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("plan.Run error = %T %v, want wrapped *parser.ParseError", err, err)
	}
	if results.FinalCounters.PC1 == nil {
		t.Errorf("final counters = %+v, want the local side salvaged after the abort", results.FinalCounters)
	}
	if results.FinalCounters.PC2 == nil {
		t.Errorf("final counters = %+v, want the responsive peer salvaged after the abort", results.FinalCounters)
	}
	if !results.Incomplete {
		t.Error("Results.Incomplete = false, want true")
	}
	if len(results.TCP) != 1 {
		t.Fatalf("results.TCP has %d entries %+v, want one incomplete forward result", len(results.TCP), results.TCP)
	}
	got := results.TCP[0]
	if !got.Incomplete || got.Direction != model.DirectionPC1ToPC2 {
		t.Errorf("TCP result = %+v, want incomplete PC1 to PC2 result", got)
	}
	if len(got.IntervalResults) != 2 {
		t.Errorf("TCP result lost decoded interval diagnostics: %+v", got)
	}
	if !slices.Contains(rc.ops, OpIperfServerStop) {
		t.Errorf("remote ops %q lack %s: the one-off server leaked after parse failure", rc.ops, OpIperfServerStop)
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
	rc.reply(OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 1}})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.Run(context.Background(), rc); err == nil {
		t.Fatalf("plan.Run succeeded despite the remote client failure")
	}
	if !results.Incomplete {
		t.Errorf("aborted run not marked Incomplete")
	}
	if results.FinalCounters.PC1 == nil {
		t.Errorf("final counters = %+v, want the local side salvaged after the abort", results.FinalCounters)
	}
	if results.FinalCounters.PC2 == nil {
		t.Errorf("final counters = %+v, want the responsive peer salvaged after the abort", results.FinalCounters)
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

// TestQuickPlanAbortSalvagesLocalFinalCounters models the peer session being
// torn down mid-TCP (the shape of a real peer-lost abort). The deferred final
// capture uses WithoutCancel, so PC1's counters still land and yield deltas
// against the baseline; the detached remote attempt sees the dead session and
// is dropped without contaminating the cancellation error.
func TestQuickPlanAbortSalvagesLocalFinalCounters(t *testing.T) {
	testutil.LeakCheck(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr := runnertest.New(t)
	rc := newFakeCallerWithSession(t, ctx)
	scriptPreTCPSteps(t, fr, rc)
	started := make(chan struct{})
	hang := make(chan struct{})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json"), Delay: hang, Started: started})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	done := make(chan error, 1)
	go func() { done <- plan.Run(ctx, rc) }()
	testutil.WaitFor(t, started, "forward TCP client never started")
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled quick Run error = %v, want context cancellation", err)
	}
	if !results.Incomplete {
		t.Errorf("aborted run not marked Incomplete")
	}
	if results.FinalCounters.PC1 == nil {
		t.Fatal("final PC1 counter snapshot missing after abort")
	}
	if results.FinalCounters.PC2 != nil {
		t.Fatalf("final PC2 counter snapshot = %+v, want absent: the salvage must not call the peer", results.FinalCounters.PC2)
	}
	if n := opCount(rc.ops, OpCountersSnapshot); n != 1 {
		t.Errorf("remote saw %d %s ops %q, want only the initial snapshot", n, OpCountersSnapshot, rc.ops)
	}
	if n := detachedCallCount(rc.calls, OpCountersSnapshot); n != 1 {
		t.Errorf("detached counter attempts = %d, want one bounded salvage attempt", n)
	}
	if _, ok := evaluate.DeltaSet(results.InitialCounters.PC1, results.FinalCounters.PC1); !ok {
		t.Errorf("PC1 baseline/final pair does not yield deltas")
	}
}

func TestAbortSalvageDropsWedgedPeerTimeout(t *testing.T) {
	testutil.LeakCheck(t)
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		Result: fixture(t, "ethtool", "stats_e1000e_clean")})
	fr.Script(runnertest.Script{Name: "ip", Result: fixture(t, "ip", "linkstats_clean")})
	rc := newFakeCaller(t)
	started := make(chan struct{})
	expire := make(chan struct{})
	rc.replyHang(OpCountersSnapshot, started, expire, peer.ErrRequestTimeout)
	original := errors.New("local parse failure")
	baseline := func() *model.CounterSnapshot {
		return &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	}
	results := &SessionResults{InitialCounters: model.PeerCounters{PC1: baseline(), PC2: baseline()}}
	q := &QuickPlan{Ops: newTestOps(t, fr), Results: results}
	done := make(chan error, 1)
	go func() { done <- q.salvageFinalCounters(context.Background(), rc, original) }()
	testutil.WaitFor(t, started, "detached counter salvage to start")
	select {
	case err := <-done:
		t.Fatalf("salvage returned before detached timeout: %v", err)
	default:
	}
	if got := rc.calls[len(rc.calls)-1]; !got.detached || got.timeout != detachedCounterTimeout {
		t.Errorf("detached call = %+v, want timeout %s", got, detachedCounterTimeout)
	}
	close(expire)
	err := <-done
	if !errors.Is(err, original) || errors.Is(err, peer.ErrRequestTimeout) {
		t.Errorf("salvage error = %v, want only original failure", err)
	}
	if results.FinalCounters.PC1 == nil || results.FinalCounters.PC2 != nil {
		t.Errorf("final counters = %+v, want local-only after wedged peer", results.FinalCounters)
	}
}

func TestAbortSalvageJoinsSubstantivePeerFailure(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		Result: fixture(t, "ethtool", "stats_e1000e_clean")})
	fr.Script(runnertest.Script{Name: "ip", Result: fixture(t, "ip", "linkstats_clean")})
	rc := newFakeCaller(t)
	rc.replyStatus(OpCountersSnapshot, StatusFailed, "counter parser failed", nil)
	original := errors.New("local parse failure")
	baseline := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	results := &SessionResults{InitialCounters: model.PeerCounters{PC1: baseline, PC2: baseline}}
	q := &QuickPlan{Ops: newTestOps(t, fr), Results: results}

	err := q.salvageFinalCounters(context.Background(), rc, original)
	if !errors.Is(err, original) || !strings.Contains(err.Error(), "counter parser failed") {
		t.Errorf("salvage error = %v, want original joined with substantive peer failure", err)
	}
	if results.FinalCounters.PC1 == nil || results.FinalCounters.PC2 != nil {
		t.Errorf("final counters = %+v, want local snapshot retained", results.FinalCounters)
	}
}

// TestQuickPlanAbortWithoutBaselineSkipsSalvage aborts before initial counters
// were ever taken: a lone final snapshot can produce no delta, so the salvage
// must not capture anything.
func TestQuickPlanAbortWithoutBaselineSkipsSalvage(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		Result: fixture(t, "ethtool", "settings_e1000e_1g")})
	rc := newFakeCaller(t)
	rc.replyStatus(OpLinkSettings, StatusFailed, "peer gone", nil)

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.Run(context.Background(), rc); err == nil {
		t.Fatalf("plan.Run succeeded despite the link step failure")
	}
	if n := opCount(rc.ops, OpCountersSnapshot); n != 0 {
		t.Errorf("remote saw %d %s ops, want none without a baseline", n, OpCountersSnapshot)
	}
	if results.FinalCounters.PC1 != nil || results.FinalCounters.PC2 != nil {
		t.Errorf("final counters = %+v, want empty without a baseline", results.FinalCounters)
	}
}

// TestSalvageFinalCountersGuards pins the salvage no-op conditions directly:
// a clean run, an already-started final capture, and a missing or empty local
// baseline must all leave FinalCounters untouched and return the original
// error unchanged. Ops is nil, so any capture attempt panics loudly.
func TestSalvageFinalCountersGuards(t *testing.T) {
	abort := errors.New("boom")
	withCounters := func() *model.CounterSnapshot {
		return &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 1}}
	}
	cases := []struct {
		name   string
		mutate func(*SessionResults)
		runErr error
	}{
		{"clean run", func(r *SessionResults) { r.InitialCounters.PC1 = withCounters() }, nil},
		{"final capture already started", func(r *SessionResults) {
			r.InitialCounters.PC1 = withCounters()
			r.FinalCounters.PC2 = withCounters()
		}, abort},
		{"no local baseline", func(r *SessionResults) { r.InitialCounters.PC2 = withCounters() }, abort},
		{"empty local baseline", func(r *SessionResults) {
			r.InitialCounters.PC1 = &model.CounterSnapshot{Standard: map[string]uint64{}}
		}, abort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := &SessionResults{}
			tc.mutate(results)
			pc2Before := results.FinalCounters.PC2
			q := &QuickPlan{Results: results}
			if err := q.salvageFinalCounters(context.Background(), newFakeCaller(t), tc.runErr); !errors.Is(err, tc.runErr) {
				t.Errorf("salvage error = %v, want the original %v unchanged", err, tc.runErr)
			}
			if results.FinalCounters.PC1 != nil || results.FinalCounters.PC2 != pc2Before {
				t.Errorf("final counters = %+v, want untouched", results.FinalCounters)
			}
		})
	}
}

// TestQuickPlanFinalStepPartialFailureDoesNotResnapshot fails stepFinalCounters
// after its local capture (remote reports failure): the salvage must not
// overwrite the already-taken PC1 snapshot with a later one or re-issue the
// RPC — exactly two snapshot ops total, initial and final.
func TestQuickPlanFinalStepPartialFailureDoesNotResnapshot(t *testing.T) {
	fr := runnertest.New(t)
	rc := newFakeCaller(t)
	scriptPreTCPSteps(t, fr, rc)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("--bidir"),
		StdoutFile: fixturePath("iperf", "bidir_314.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-u"),
		StdoutFile: fixturePath("iperf", "udp_316.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	rc.reply(OpIperfCaps, &model.Iperf3Caps{Version: "3.16", JSON: true, UDP: true, Bidir: true})
	for range 3 {
		rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
		rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	}
	rc.reply(OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{SenderBitsPerSecond: 5e8, ReceiverBitsPerSecond: 4.9e8}})
	rc.reply(OpIperfUDPRun, &UDPRunResult{UDP: model.UDPResult{
		TargetBps: 800000000, ActualSenderBps: 799958000, JitterMs: 0.02}})
	rc.replyStatus(OpCountersSnapshot, StatusFailed, "worker died during final capture", nil)

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.Run(context.Background(), rc); err == nil {
		t.Fatalf("plan.Run succeeded despite the final counters failure")
	}
	if n := opCount(rc.ops, OpCountersSnapshot); n != 2 {
		t.Errorf("remote saw %d %s ops %q, want exactly initial + final", n, OpCountersSnapshot, rc.ops)
	}
	if results.FinalCounters.PC1 == nil {
		t.Error("final PC1 counter snapshot missing, want the capture taken before the remote failure")
	}
	if results.FinalCounters.PC2 != nil {
		t.Errorf("final PC2 counter snapshot = %+v, want absent after the remote failure", results.FinalCounters.PC2)
	}
}

// opCount counts occurrences of op in the recorded remote op order.
func opCount(ops []string, op string) int {
	n := 0
	for _, o := range ops {
		if o == op {
			n++
		}
	}
	return n
}

func detachedCallCount(calls []fakeCall, op string) int {
	n := 0
	for _, call := range calls {
		if call.detached && call.op == op {
			n++
		}
	}
	return n
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
