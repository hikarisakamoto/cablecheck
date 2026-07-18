package testsuite

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/runner/runnertest"
)

// TestUDPClientArgs pins the exact UDP client invocation from the tools.md §2
// matrix — base argv plus `-u -b <plain integer bits/s> -t <dur> -l <mtu-28>`
// — and the mapping of the parsed server-observed stats onto model.UDPResult.
func TestUDPClientArgs(t *testing.T) {
	ctx := context.Background()
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-u"),
		StdoutFile: fixturePath("iperf", "udp_39_loss.json")})
	m, _ := newTestManager(t, fr)

	res, err := m.RunUDPClient(ctx, netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"),
		5201, 20*time.Second, 800000000, 1472)
	if err != nil {
		t.Fatalf("RunUDPClient: %v", err)
	}

	call := fr.CallsFor("iperf3")[0]
	want := []string{"-c", "10.0.0.2", "-B", "10.0.0.1", "-p", "5201",
		"-J", "--connect-timeout", "3000",
		"-u", "-b", "800000000", "-t", "20", "-l", "1472"}
	if !slices.Equal(call.Args, want) {
		t.Errorf("client args = %q, want %q", call.Args, want)
	}
	if wantTimeout := 20*time.Second + clientTimeoutSlack; call.Spec.Timeout != wantTimeout {
		t.Errorf("Spec.Timeout = %v, want %v (test duration + client slack)", call.Spec.Timeout, wantTimeout)
	}

	if res.Incomplete {
		t.Errorf("clean run marked incomplete")
	}
	u := res.UDP
	if u.TargetBps != 800000000 {
		t.Errorf("TargetBps = %d, want the requested 800000000 (the parser cannot know it)", u.TargetBps)
	}
	if u.JitterMs != 0.041 || u.LostPackets != 412 || u.TotalPackets != 170000 || u.LossPercent != 0.24 {
		t.Errorf("server-observed stats = jitter %v lost %d/%d (%v%%), want 0.041 / 412/170000 (0.24%%)",
			u.JitterMs, u.LostPackets, u.TotalPackets, u.LossPercent)
	}
	if u.OutOfOrder == nil || *u.OutOfOrder != 3 {
		t.Errorf("OutOfOrder = %v, want 3", u.OutOfOrder)
	}
	if u.ActualSenderBps != 100096000.0 {
		t.Errorf("ActualSenderBps = %v, want 100096000", u.ActualSenderBps)
	}
	if u.CPU.HostTotal != 5.104061 {
		t.Errorf("CPU.HostTotal = %v, want 5.104061", u.CPU.HostTotal)
	}
}

// scriptUDPStep scripts one local runner + fake caller for a full stepUDP
// round: local UDP client (forward), local one-off server (reverse), and the
// three remote ops (server start/stop for the forward phase, the reverse UDP
// client run on the worker).
func scriptUDPStep(t *testing.T, fr *runnertest.FakeRunner, rc *fakeCaller) {
	t.Helper()
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-u"),
		StdoutFile: fixturePath("iperf", "udp_316.json")})
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})
	rc.reply(OpIperfUDPRun, &UDPRunResult{UDP: model.UDPResult{
		TargetBps: 800000000, ActualSenderBps: 799958000, LossPercent: 0.01, JitterMs: 0.02}})
}

// TestUDPDefaultRateFromLink pins the UDP rate derivation inside the plan:
// a 1 Gb/s negotiated link yields `-b 800000000` (80%, plain integer), an
// explicit --udp-rate is honored but flags near-saturation above 95% of the
// negotiated speed, and an unknown speed falls back to 100M with a warning
// note plus the rate-assumed flag for the evaluator.
func TestUDPDefaultRateFromLink(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, results *SessionResults, mutate func(*QuickPlan)) (*runnertest.FakeRunner, *fakeCaller) {
		t.Helper()
		fr := runnertest.New(t)
		rc := newFakeCaller(t)
		scriptUDPStep(t, fr, rc)
		plan := newQuickPlan(newTestOps(t, fr), results)
		if mutate != nil {
			mutate(plan)
		}
		if err := plan.stepUDP(ctx, rc); err != nil {
			t.Fatalf("stepUDP: %v", err)
		}
		return fr, rc
	}

	linked := func(speedMbps int) *SessionResults {
		return &SessionResults{Link: map[string]*LinkSettingsResult{
			RolePC1Key: {Settings: model.LinkSettings{SpeedMbps: speedMbps, Duplex: "full", LinkDetected: true}},
			RolePC2Key: {Settings: model.LinkSettings{SpeedMbps: speedMbps, Duplex: "full", LinkDetected: true}},
		}}
	}

	bArg := func(t *testing.T, args []string) string {
		t.Helper()
		i := slices.Index(args, "-b")
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("args %q lack -b", args)
		}
		return args[i+1]
	}

	t.Run("1G_negotiated_gives_800M", func(t *testing.T) {
		results := linked(1000)
		fr, rc := run(t, results, nil)
		client := fr.CallsFor("iperf3")[0]
		if got := bArg(t, client.Args); got != "800000000" {
			t.Errorf("-b = %q, want the plain integer 800000000 (80%% of 1G)", got)
		}
		// The same derived rate is sent to the worker for the reverse run.
		var p UDPRunParams
		if err := json.Unmarshal(rc.params[OpIperfUDPRun][0], &p); err != nil {
			t.Fatalf("unmarshal UDP run params: %v", err)
		}
		if p.RateBps != 800000000 {
			t.Errorf("remote RateBps = %d, want 800000000", p.RateBps)
		}
		if results.UDPNearSaturation || results.UDPRateAssumed {
			t.Errorf("flags = nearSaturation %v rateAssumed %v, want false/false on a clean derivation",
				results.UDPNearSaturation, results.UDPRateAssumed)
		}
		if len(results.UDP) != 2 {
			t.Fatalf("results.UDP has %d entries, want both directions", len(results.UDP))
		}
		if results.UDP[0].Direction != model.DirectionPC1ToPC2 || results.UDP[1].Direction != model.DirectionPC2ToPC1 {
			t.Errorf("UDP directions = %q/%q, want pc1_to_pc2 then pc2_to_pc1",
				results.UDP[0].Direction, results.UDP[1].Direction)
		}
	})

	t.Run("explicit_rate_honored_near_saturation_flagged", func(t *testing.T) {
		results := linked(1000)
		fr, _ := run(t, results, func(q *QuickPlan) { q.UDPRate = 990000000 })
		if got := bArg(t, fr.CallsFor("iperf3")[0].Args); got != "990000000" {
			t.Errorf("-b = %q, want the explicit 990000000 honored", got)
		}
		if !results.UDPNearSaturation {
			t.Errorf("UDPNearSaturation = false, want true for 99%% of the negotiated speed")
		}
		if !notesContain(results.Notes, "saturation") {
			t.Errorf("notes %q lack a near-saturation warning", results.Notes)
		}
	})

	t.Run("unknown_speed_falls_back_to_100M", func(t *testing.T) {
		results := &SessionResults{} // no link settings captured at all
		fr, _ := run(t, results, nil)
		if got := bArg(t, fr.CallsFor("iperf3")[0].Args); got != "100000000" {
			t.Errorf("-b = %q, want the 100000000 fallback for an unknown link speed", got)
		}
		if !results.UDPRateAssumed {
			t.Errorf("UDPRateAssumed = false, want true when the speed is unknown")
		}
		if !notesContain(results.Notes, "unknown") {
			t.Errorf("notes %q lack the unknown-speed warning", results.Notes)
		}
	})
}

// notesContain reports whether any note contains substr (case-insensitive).
func notesContain(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(strings.ToLower(n), strings.ToLower(substr)) {
			return true
		}
	}
	return false
}
