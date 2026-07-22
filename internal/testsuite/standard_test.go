package testsuite

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// newStandardPlan builds a StandardPlan over ops with the canonical standard
// topology: 60s TCP repeated twice per direction, 30s UDP, 4 streams, a
// 1500-packet ping, MTU 1500, bidir-capable local iperf3.
func newStandardPlan(ops *Ops, results *SessionResults) *StandardPlan {
	return &StandardPlan{
		Ops:            ops,
		LocalIP:        netip.MustParseAddr("10.0.0.1"),
		PeerIP:         netip.MustParseAddr("10.0.0.2"),
		IperfPort:      5201,
		TCPDuration:    60 * time.Second,
		UDPDuration:    30 * time.Second,
		TCPRepeats:     2,
		MTU:            1500,
		Streams:        4,
		PingCount:      1500,
		LocalIperfCaps: model.Iperf3Caps{Version: "3.16", JSON: true, Reverse: true, UDP: true, Bidir: true, OneOff: true},
		Results:        results,
	}
}

// TestStandardPlanSequence runs the full standard plan against a scripted
// runner and caller and asserts the extra depth the mode adds over quick:
// 60s TCP twice per direction, the extra half-rate UDP run, monitoring-active
// step list, and the accumulated per-direction results.
func TestStandardPlanSequence(t *testing.T) {
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
	rc.reply(OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 1500, IntervalUsedSec: 0.02}})
	rc.reply(OpPingFullSize, &PingRunResult{Ping: model.PingResult{Transmitted: 100, Received: 100}})
	rc.reply(OpIperfCaps, &model.Iperf3Caps{Version: "3.16", JSON: true, UDP: true, Bidir: true})
	// Two forward TCP repeats + reverse TCP repeats + bidir server + two UDP
	// runs (default rate + extra half rate), each remote-hosted phase getting
	// one one-off server start/stop; reverse TCP + reverse UDP host locally.
	for range 6 {
		rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
		rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	}
	for range 2 {
		rc.reply(OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{SenderBitsPerSecond: 5e8, ReceiverBitsPerSecond: 4.9e8}})
	}
	for range 2 {
		rc.reply(OpIperfUDPRun, &UDPRunResult{UDP: model.UDPResult{
			TargetBps: 800000000, ActualSenderBps: 799958000, JitterMs: 0.02}})
	}

	var steps []string
	results := &SessionResults{}
	plan := newStandardPlan(ops, results)
	plan.OnStep = func(step, total int, name string) {
		steps = append(steps, name)
	}
	if err := plan.Run(context.Background(), rc); err != nil {
		t.Fatalf("plan.Run: %v", err)
	}

	names := StandardPlanSteps()
	if !slices.Equal(steps, names) {
		t.Fatalf("announced steps = %q, want the standard step list %q", steps, names)
	}
	if len(rc.steps) != len(names) {
		t.Fatalf("attached %d remote steps %+v, want %d", len(rc.steps), rc.steps, len(names))
	}
	for i, name := range names {
		if got := rc.steps[i]; got != (fakeStep{step: i + 1, total: len(names), name: name}) {
			t.Errorf("remote standard step %d = %+v, want label matching local announcement", i, got)
		}
	}

	// The step list must reflect the repeated TCP phases and the extra
	// half-rate UDP run so the operator sees the real workload.
	joined := strings.Join(names, "\n")
	for _, want := range []string{"repeat 1", "repeat 2", "reduced-rate UDP"} {
		if !strings.Contains(joined, want) {
			t.Errorf("standard step list %q missing %q", names, want)
		}
	}

	// Each one-way TCP direction ran TCPRepeats(=2) times at 60s.
	tcpDirs := directions(t, results.TCP, func(r model.TCPResult) string { return r.Direction })
	wantDirs := []string{
		model.DirectionPC1ToPC2, model.DirectionPC1ToPC2,
		model.DirectionPC2ToPC1, model.DirectionPC2ToPC1,
	}
	if !slices.Equal(tcpDirs, wantDirs) {
		t.Errorf("TCP directions = %q, want each direction twice: %q", tcpDirs, wantDirs)
	}

	// The forward TCP client ran twice at -t 60 (excluding the UDP and the
	// native --bidir client runs, which also carry -c).
	var tcpClientRuns int
	for _, call := range fr.CallsFor("iperf3") {
		if slices.Contains(call.Args, "-c") && !slices.Contains(call.Args, "-u") && !slices.Contains(call.Args, "--bidir") {
			tcpClientRuns++
			assertArgValue(t, call.Args, "-t", "60")
		}
	}
	if tcpClientRuns != 2 {
		t.Errorf("local forward TCP client ran %d times, want 2 (TCPRepeats)", tcpClientRuns)
	}

	// The remote reverse TCP client ran twice too, at 60s.
	if got := len(rc.params[OpIperfClientRun]); got != 2 {
		t.Fatalf("remote TCP client runs = %d, want 2 (TCPRepeats)", got)
	}
	for i := range rc.params[OpIperfClientRun] {
		var p IperfClientRunParams
		if err := json.Unmarshal(rc.params[OpIperfClientRun][i], &p); err != nil {
			t.Fatalf("unmarshal reverse TCP params: %v", err)
		}
		if p.DurationSec != 60 {
			t.Errorf("reverse TCP run %d DurationSec = %d, want 60", i, p.DurationSec)
		}
	}

	// Two UDP runs per direction: one at the derived default rate and one
	// extra at half that rate.
	udpDirs := directions(t, results.UDP, func(u model.UDPResult) string { return u.Direction })
	if len(udpDirs) != 4 {
		t.Fatalf("UDP results = %d, want 4 (two directions × default + reduced rate)", len(udpDirs))
	}

	// The two local UDP client runs used the derived 800M and its half (400M).
	var udpRates []string
	for _, call := range fr.CallsFor("iperf3") {
		if slices.Contains(call.Args, "-u") {
			i := slices.Index(call.Args, "-b")
			if i >= 0 && i+1 < len(call.Args) {
				udpRates = append(udpRates, call.Args[i+1])
			}
		}
	}
	if !slices.Contains(udpRates, "800000000") {
		t.Errorf("UDP rates %q missing the derived 800000000 default", udpRates)
	}
	if !slices.Contains(udpRates, strconv.Itoa(800000000/2)) {
		t.Errorf("UDP rates %q missing the extra half-rate %d run", udpRates, 800000000/2)
	}

	if results.Incomplete {
		t.Errorf("clean standard run marked incomplete")
	}
	if results.InitialCounters.PC1 == nil || results.FinalCounters.PC1 == nil {
		t.Errorf("standard run missing bracketing counters")
	}
	if results.Bidir == nil {
		t.Errorf("standard run missing bidirectional stress result")
	}
}

func TestStandardReducedUDPRateNeverExceedsPrimary(t *testing.T) {
	plan := &StandardPlan{}
	primary := model.Bitrate(1_000_000)
	reduced := plan.reducedRate(primary)
	if reduced > primary {
		t.Errorf("reduced UDP rate = %s, exceeds primary rate %s", reduced, primary)
	}
	if want := primary / 2; reduced != want {
		t.Errorf("reduced UDP rate = %s, want 50%% of actual primary rate %s", reduced, want)
	}
}

// assertArgValue asserts args contains flag immediately followed by want.
func assertArgValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) || args[i+1] != want {
		t.Errorf("args %q: want %s %s", args, flag, want)
	}
}

// TestStandardPlanPreservesPartialOnError aborts the local forward TCP client
// on its first repeat and asserts the standard plan preserves the partial
// result and marks the session incomplete, exactly like the quick plan.
func TestStandardPlanPreservesPartialOnError(t *testing.T) {
	fr := runnertest.New(t)
	rc := newFakeCaller(t)
	scriptPreTCPSteps(t, fr, rc)
	// Overwrite the quick pre-TCP ping reply count to the standard preset.
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		Result: runner.CommandResult{TimedOut: true, ExitCode: -1, Signal: "SIGKILL"}})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})

	results := &SessionResults{}
	plan := newStandardPlan(newTestOps(t, fr), results)
	if err := plan.Run(context.Background(), rc); err == nil {
		t.Fatalf("plan.Run succeeded despite the client timing out")
	}
	if !results.Incomplete {
		t.Errorf("aborted standard run not marked Incomplete")
	}
	if len(results.TCP) != 1 || !results.TCP[0].Incomplete {
		t.Errorf("results.TCP = %+v, want the 1 partial forward result", results.TCP)
	}
	if results.TCP[0].Duration != model.Duration(60*time.Second) {
		t.Errorf("partial TCP result duration = %v, want the 60s standard duration", results.TCP[0].Duration)
	}
}
