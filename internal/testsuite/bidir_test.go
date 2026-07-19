package testsuite

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"cablecheck/internal/model"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// TestBidirNative: when the effective capabilities (local AND peer) include
// --bidir, the bidirectional step runs one native --bidir client on PC1
// against a single one-off server on PC2, and the per-direction totals are
// derived from the stream rows.
func TestBidirNative(t *testing.T) {
	ctx := context.Background()
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("--bidir"),
		StdoutFile: fixturePath("iperf", "bidir_314.json")})

	rc := newFakeCaller(t)
	rc.reply(OpIperfCaps, &model.Iperf3Caps{Version: "3.14", JSON: true, UDP: true, Bidir: true})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.stepBidir(ctx, rc); err != nil {
		t.Fatalf("stepBidir: %v", err)
	}

	// Exactly one local iperf3 invocation: the --bidir client. The single
	// server lives on PC2 (remote op), never locally.
	calls := fr.CallsFor("iperf3")
	if len(calls) != 1 {
		t.Fatalf("got %d local iperf3 calls %v, want just the --bidir client", len(calls), calls)
	}
	args := calls[0].Args
	for _, tok := range []string{"-c", "10.0.0.2", "--bidir", "-P", "4", "-t", "30"} {
		if !slices.Contains(args, tok) {
			t.Errorf("client args %q lack %q", args, tok)
		}
	}
	if slices.Contains(args, "-s") {
		t.Errorf("client args %q contain -s; the server belongs on PC2", args)
	}

	wantOps := []string{OpIperfCaps, OpIperfServerStart, OpIperfServerStop}
	if !slices.Equal(rc.ops, wantOps) {
		t.Errorf("remote ops = %q, want %q (single server on PC2)", rc.ops, wantOps)
	}
	var start IperfServerStartParams
	if err := json.Unmarshal(rc.params[OpIperfServerStart][0], &start); err != nil {
		t.Fatalf("unmarshal server start params: %v", err)
	}
	if start.BindIP != "10.0.0.2" || start.Port != 5201 {
		t.Errorf("server start params = %+v, want PC2's 10.0.0.2:5201", start)
	}

	b := results.Bidir
	if b == nil {
		t.Fatal("results.Bidir = nil, want the native bidir result")
	}
	if b.TwoPhaseFallback {
		t.Errorf("TwoPhaseFallback = true, want a native --bidir run")
	}
	if b.PC1ToPC2.SenderBitsPerSecond != 941e6 {
		t.Errorf("PC1ToPC2.SenderBitsPerSecond = %v, want 941e6 from the fixture streams", b.PC1ToPC2.SenderBitsPerSecond)
	}
	if b.PC2ToPC1.SenderBitsPerSecond != 887e6 {
		t.Errorf("PC2ToPC1.SenderBitsPerSecond = %v, want 887e6 from the fixture streams", b.PC2ToPC1.SenderBitsPerSecond)
	}
	if b.PC1ToPC2.ReceiverBitsPerSecond != 940.8e6 {
		t.Errorf("PC1ToPC2.ReceiverBitsPerSecond = %v, want 940.8e6 from the fixture receiver row", b.PC1ToPC2.ReceiverBitsPerSecond)
	}
	if b.PC2ToPC1.ReceiverBitsPerSecond != 886.4e6 {
		t.Errorf("PC2ToPC1.ReceiverBitsPerSecond = %v, want 886.4e6 from the fixture receiver row", b.PC2ToPC1.ReceiverBitsPerSecond)
	}
	if b.PC1ToPC2.Retransmissions == nil || *b.PC1ToPC2.Retransmissions != 17 {
		t.Errorf("PC1ToPC2.Retransmissions = %v, want 17", b.PC1ToPC2.Retransmissions)
	}
	if b.PC2ToPC1.Retransmissions == nil || *b.PC2ToPC1.Retransmissions != 4 {
		t.Errorf("PC2ToPC1.Retransmissions = %v, want 4", b.PC2ToPC1.Retransmissions)
	}
	if results.Incomplete {
		t.Errorf("clean native bidir run marked incomplete")
	}
}

// TestBidirFallbackTwoPorts: without --bidir on both peers the step runs two
// coordinated one-way phases — PC1→PC2 toward the worker's server on
// iperf-port, PC2→PC1 toward a local server on iperf-port+1 — records the
// limitation as a note (never a failure), and assembles both directions into
// one BidirResult flagged TwoPhaseFallback.
func TestBidirFallbackTwoPorts(t *testing.T) {
	ctx := context.Background()
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})

	rc := newFakeCaller(t)
	rc.reply(OpIperfCaps, &model.Iperf3Caps{Version: "3.9", JSON: true, UDP: true, Bidir: false})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	retr := uint64(3)
	rc.reply(OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{
		SenderBitsPerSecond: 5e8, ReceiverBitsPerSecond: 4.9e8, Retransmissions: &retr}})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.stepBidir(ctx, rc); err != nil {
		t.Fatalf("stepBidir: %v (the fallback is a limitation, never a failure)", err)
	}

	// Local side: one one-off server on iperf-port+1 (the reverse phase's
	// receiving side) and one plain client toward the worker on iperf-port.
	var serverArgs, clientArgs []string
	for _, c := range fr.CallsFor("iperf3") {
		if slices.Contains(c.Args, "-s") {
			serverArgs = c.Args
		}
		if slices.Contains(c.Args, "-c") {
			clientArgs = c.Args
		}
	}
	if serverArgs == nil || !slices.Contains(serverArgs, strconv.Itoa(5202)) {
		t.Errorf("local server args = %q, want a server on port 5202 (iperf-port+1)", serverArgs)
	}
	if clientArgs == nil || !slices.Contains(clientArgs, "5201") {
		t.Errorf("local client args = %q, want the forward client on port 5201", clientArgs)
	}
	if slices.Contains(clientArgs, "--bidir") {
		t.Errorf("local client args %q use --bidir despite the missing capability", clientArgs)
	}

	// Remote side: the worker's client runs toward PC1 on iperf-port+1.
	var p IperfClientRunParams
	if err := json.Unmarshal(rc.params[OpIperfClientRun][0], &p); err != nil {
		t.Fatalf("unmarshal remote client params: %v", err)
	}
	if p.Port != 5202 || p.PeerIP != "10.0.0.1" || p.LocalIP != "10.0.0.2" {
		t.Errorf("remote client params = %+v, want the reverse phase toward 10.0.0.1:5202", p)
	}
	if p.Bidir {
		t.Errorf("remote client params request --bidir despite the missing capability")
	}

	b := results.Bidir
	if b == nil {
		t.Fatal("results.Bidir = nil, want the two-phase fallback result")
	}
	if !b.TwoPhaseFallback {
		t.Errorf("TwoPhaseFallback = false, want true")
	}
	if b.PC1ToPC2.SenderBitsPerSecond != 944e6 {
		t.Errorf("PC1ToPC2.SenderBitsPerSecond = %v, want 944e6 from the local phase", b.PC1ToPC2.SenderBitsPerSecond)
	}
	if b.PC2ToPC1.SenderBitsPerSecond != 5e8 {
		t.Errorf("PC2ToPC1.SenderBitsPerSecond = %v, want 5e8 from the remote phase", b.PC2ToPC1.SenderBitsPerSecond)
	}
	if b.PC2ToPC1.Retransmissions == nil || *b.PC2ToPC1.Retransmissions != 3 {
		t.Errorf("PC2ToPC1.Retransmissions = %v, want 3", b.PC2ToPC1.Retransmissions)
	}
	if !notesContain(results.Notes, "bidir") {
		t.Errorf("notes %q lack the two-phase limitation note", results.Notes)
	}
	if results.Incomplete {
		t.Errorf("fallback run marked incomplete; the fallback is a limitation, not a failure")
	}
}

// TestBidirFallbackStopsRemoteServerWhenLocalReadyFails pins cleanup of the
// forward one-off server on PC2 when the reverse server on PC1 exits before
// readiness. No client ever reaches the remote server on this path, so it
// must be stopped explicitly instead of waiting for session teardown.
func TestBidirFallbackStopsRemoteServerWhenLocalReadyFails(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{
		Name:  "iperf3",
		Match: runnertest.ArgsContain("-s", "5202"),
		Result: runner.CommandResult{
			ExitCode: 1,
			Stderr:   []byte("unable to start listener for connections: Address already in use\n"),
		},
		Delay: closedChan(),
	})

	rc := newFakeCaller(t)
	rc.reply(OpIperfCaps, &model.Iperf3Caps{Version: "3.9", JSON: true, Bidir: false})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})

	plan := newQuickPlan(newTestOps(t, fr), &SessionResults{})
	if err := plan.stepBidir(context.Background(), rc); err == nil {
		t.Errorf("stepBidir succeeded despite the local reverse server readiness failure")
	}
	wantOps := []string{OpIperfCaps, OpIperfServerStart, OpIperfServerStop}
	if !slices.Equal(rc.ops, wantOps) {
		t.Errorf("remote ops = %q, want %q; PC2's port 5201 server must be stopped on the early return", rc.ops, wantOps)
	}
	if len(rc.params[OpIperfServerStop]) == 1 {
		var stop IperfServerStopParams
		if err := json.Unmarshal(rc.params[OpIperfServerStop][0], &stop); err != nil {
			t.Errorf("unmarshal server stop params: %v", err)
		} else if stop.Port != 5201 {
			t.Errorf("server stop port = %d, want the remote forward server on 5201", stop.Port)
		}
	}
}
