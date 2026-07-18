package testsuite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/model"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner/runnertest"
)

// newTestOps wires an Ops handler over one FakeRunner with a sysfs root.
func newTestOps(t *testing.T, fr *runnertest.FakeRunner) *Ops {
	t.Helper()
	root := t.TempDir()
	writeSysfs(t, root, "eth0", "carrier_changes", "2\n")
	clk := clocktest.New(time.Unix(1_700_000_000, 0))
	m := &IperfManager{R: fr, Clock: clk, TestID: "ct-test", Identify: fakeIdentify}
	return &Ops{
		Counters: &CounterCollector{R: fr, Clock: clk, IfName: "eth0", SysfsRoot: root},
		Link:     &LinkInspector{R: fr, IfName: "eth0"},
		Ping:     &PingTester{R: fr},
		Iperf:    m,
	}
}

// TestOpsDispatch exercises HandleOp end to end for the quick-mode ops,
// including the status mapping for unavailable tools and unknown ops.
func TestOpsDispatch(t *testing.T) {
	ctx := context.Background()

	t.Run("CountersSnapshot", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
			Result: fixture(t, "ethtool", "stats_e1000e_clean")})
		fr.Script(runnertest.Script{Name: "ip", Result: fixture(t, "ip", "linkstats_clean")})
		ops := newTestOps(t, fr)
		var progressed []protocol.TestProgress
		res, status, err := ops.HandleOp(ctx, OpCountersSnapshot, nil,
			func(p protocol.TestProgress) { progressed = append(progressed, p) })
		if err != nil || status != "ok" {
			t.Fatalf("HandleOp counters = status %q err %v", status, err)
		}
		snap, ok := res.(*model.CounterSnapshot)
		if !ok || len(snap.Standard) == 0 {
			t.Errorf("counters result = %T %+v, want *model.CounterSnapshot with data", res, res)
		}
		if len(progressed) == 0 || progressed[0].Stage != OpCountersSnapshot {
			t.Errorf("op emitted no TestProgress naming its stage: %+v", progressed)
		}
	})

	t.Run("LinkSettings", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
			Result: fixture(t, "ethtool", "settings_r8152_100m_half")})
		ops := newTestOps(t, fr)
		res, status, err := ops.HandleOp(ctx, OpLinkSettings, nil, discardProgress)
		if err != nil || status != "ok" {
			t.Fatalf("HandleOp link = status %q err %v", status, err)
		}
		lr, ok := res.(*LinkSettingsResult)
		if !ok || lr.Settings.SpeedMbps != 100 || len(lr.Observations) == 0 {
			t.Errorf("link result = %T %+v, want settings 100Mb/s with observations", res, res)
		}
	})

	t.Run("PingUnavailable", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Missing("ping")
		ops := newTestOps(t, fr)
		params, _ := json.Marshal(PingRunParams{PeerIP: "10.0.0.2", Count: 100})
		_, status, err := ops.HandleOp(ctx, OpPingRun, params, discardProgress)
		if status != "unavailable" || err == nil {
			t.Errorf("ping with tool missing = status %q err %v, want unavailable + error", status, err)
		}
	})

	t.Run("ServerStartStop", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
			StdoutFile: fixturePath("iperf", "server_listening.txt")})
		ops := newTestOps(t, fr)
		params, _ := json.Marshal(IperfServerStartParams{BindIP: "10.0.0.2", Port: 5201})
		res, status, err := ops.HandleOp(ctx, OpIperfServerStart, params, discardProgress)
		if err != nil || status != "ok" {
			t.Fatalf("server start = status %q err %v", status, err)
		}
		if sr, ok := res.(*ServerStartResult); !ok || sr.Port != 5201 {
			t.Errorf("server start result = %T %+v, want *ServerStartResult port 5201", res, res)
		}
		stopParams, _ := json.Marshal(IperfServerStopParams{Port: 5201})
		_, status, err = ops.HandleOp(ctx, OpIperfServerStop, stopParams, discardProgress)
		if err != nil || status != "ok" {
			t.Fatalf("server stop = status %q err %v", status, err)
		}
		if !fr.Processes()[0].Terminated() && !fr.Processes()[0].Killed() {
			t.Errorf("server process survived the stop op")
		}
	})

	t.Run("UnknownOpRejected", func(t *testing.T) {
		ops := newTestOps(t, runnertest.New(t))
		_, status, err := ops.HandleOp(ctx, "warp_drive", nil, discardProgress)
		if status != "rejected" || err == nil {
			t.Errorf("unknown op = status %q err %v, want rejected + error", status, err)
		}
	})
}

// TestStopAllServers covers the session-teardown sweep: every iperf3 server
// started via HandleOp is force-stopped so an aborted run leaves nothing
// behind, the registry sees one unregister per server, the swept ports are no
// longer tracked, and a second sweep is a no-op.
func TestStopAllServers(t *testing.T) {
	ctx := context.Background()
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	ops := newTestOps(t, fr)
	reg := &fakeRegistry{}
	ops.Iperf.Reg = reg

	for _, port := range []uint16{5201, 5202} {
		params, _ := json.Marshal(IperfServerStartParams{BindIP: "10.0.0.2", Port: port})
		if _, status, err := ops.HandleOp(ctx, OpIperfServerStart, params, discardProgress); err != nil || status != StatusOK {
			t.Fatalf("start server on port %d = status %q err %v", port, status, err)
		}
	}
	if registered, _ := reg.snapshot(); len(registered) != 2 {
		t.Fatalf("registry registered %d servers, want 2", len(registered))
	}

	ops.StopAllServers(ctx)

	procs := fr.Processes()
	if len(procs) != 2 {
		t.Fatalf("got %d fake processes, want the 2 servers", len(procs))
	}
	for i, p := range procs {
		if !p.Terminated() && !p.Killed() {
			t.Errorf("server process %d survived StopAllServers", i)
		}
	}
	if _, unregistered := reg.snapshot(); unregistered != 2 {
		t.Errorf("registry unregistered %d times after the sweep, want 2", unregistered)
	}

	// The swept servers are no longer tracked: a stop op for them is rejected.
	stopParams, _ := json.Marshal(IperfServerStopParams{Port: 5201})
	if _, status, err := ops.HandleOp(ctx, OpIperfServerStop, stopParams, discardProgress); status != StatusRejected || err == nil {
		t.Errorf("stop of swept port = status %q err %v, want rejected + error", status, err)
	}

	// A second sweep is a no-op: no double unregister, no new signals.
	ops.StopAllServers(ctx)
	if _, unregistered := reg.snapshot(); unregistered != 2 {
		t.Errorf("second StopAllServers unregistered again (%d total, want 2)", unregistered)
	}
}

// TestDecodeOpResult pins the fixed op→decoder table round trip.
func TestDecodeOpResult(t *testing.T) {
	retr := uint64(12)
	cases := []struct {
		op      string
		payload any
	}{
		{OpCountersSnapshot, &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 3}}},
		{OpLinkSettings, &LinkSettingsResult{Settings: model.LinkSettings{SpeedMbps: 1000}}},
		{OpPingRun, &PingRunResult{Ping: model.PingResult{Received: 100}}},
		{OpIperfServerStart, &ServerStartResult{Port: 5201}},
		{OpIperfServerStop, &ServerStopResult{Stopped: true}},
		{OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{Retransmissions: &retr}}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.op, err)
		}
		got, err := DecodeOpResult(tc.op, raw)
		if err != nil {
			t.Errorf("%s: decode: %v", tc.op, err)
			continue
		}
		reencoded, err := json.Marshal(got)
		if err != nil {
			t.Errorf("%s: re-marshal: %v", tc.op, err)
			continue
		}
		if string(reencoded) != string(raw) {
			t.Errorf("%s: round trip mismatch:\n got %s\nwant %s", tc.op, reencoded, raw)
		}
	}
	if _, err := DecodeOpResult("warp_drive", nil); err == nil {
		t.Errorf("DecodeOpResult accepted an unknown op")
	}
}

// discardProgress is a no-op progress sink for tests.
func discardProgress(protocol.TestProgress) {}
