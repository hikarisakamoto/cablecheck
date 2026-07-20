package testsuite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner/runnertest"
)

// hasUnreachableSkip reports whether a throughput test named name was recorded
// as skipped due to peer-unreachability (the marker the evaluator keys LIM-05
// off).
func hasUnreachableSkip(r *SessionResults, name string) bool {
	for _, s := range r.SkippedTests {
		if s.Name == name && strings.HasPrefix(s.Reason, model.SkipReasonUnreachable) {
			return true
		}
	}
	return false
}

// TestStatusForUnreachable locks the narrowing: a connect/reachability failure
// maps to unavailable (non-fatal), while any other iperf3 client error stays
// failed.
func TestStatusForUnreachable(t *testing.T) {
	unreachable := fmt.Errorf("testsuite: iperf3 client: %w", parser.ErrIperfUnreachable)
	if got := statusFor(unreachable); got != StatusUnavailable {
		t.Errorf("statusFor(unreachable) = %q, want %q", got, StatusUnavailable)
	}
	plain := fmt.Errorf("testsuite: iperf3 client: %w", parser.ErrIperfClient)
	if got := statusFor(plain); got != StatusFailed {
		t.Errorf("statusFor(plain client error) = %q, want %q", got, StatusFailed)
	}
}

// TestUnreachableTCPForwardIsNonFatal covers the local-client path
// (withRemoteServer): a firewalled data port yields a recorded skip and a nil
// step error, so the run finalizes instead of aborting.
func TestUnreachableTCPForwardIsNonFatal(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "client_error_refused.stdout")})
	rc := newFakeCaller(t)
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.stepTCPForward(context.Background(), rc); err != nil {
		t.Fatalf("stepTCPForward is fatal for an unreachable data port: %v", err)
	}
	if !hasUnreachableSkip(results, "tcp") {
		t.Errorf("SkippedTests = %+v, want a %q skip prefixed %q", results.SkippedTests, "tcp", model.SkipReasonUnreachable)
	}
	if results.Incomplete {
		t.Error("Results.Incomplete = true; a firewall skip must not mark the run partial")
	}
	if len(results.TCP) != 0 {
		t.Errorf("results.TCP = %+v, want no empty partial recorded for an unreachable connect", results.TCP)
	}
}

// TestUnreachableBidirFallbackIsNonFatal covers the two-phase bidir fallback's
// local client leg (the one path the first pass missed): an unreachable data
// port must be a recorded skip, not a fatal abort, even when the remote leg
// succeeds.
func TestUnreachableBidirFallbackIsNonFatal(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "client_error_refused.stdout")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	rc := newFakeCaller(t)
	rc.reply(OpIperfServerStart, &ServerStartResult{Port: 5201})
	rc.reply(OpIperfClientRun, &TCPRunResult{TCP: model.TCPResult{
		Duration: model.Duration(30 * time.Second), ParallelStreams: 4}})
	rc.reply(OpIperfServerStop, &ServerStopResult{Stopped: true})

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.bidirFallback(context.Background(), rc, "fallback note"); err != nil {
		t.Fatalf("bidirFallback is fatal for an unreachable local client: %v", err)
	}
	if !hasUnreachableSkip(results, "bidirectional") {
		t.Errorf("SkippedTests = %+v, want a bidirectional unreachable skip", results.SkippedTests)
	}
	if results.Incomplete {
		t.Error("Results.Incomplete = true; a firewall skip must not mark the run partial")
	}
}

// TestUnreachableTCPReverseIsNonFatal covers the remote-client path
// (callRemoteForTest): the worker reports StatusUnavailable with iperf3's
// connect-failure message, which the coordinator reclassifies to the same
// non-fatal unreachable skip even though the typed sentinel did not cross the
// RPC boundary.
func TestUnreachableTCPReverseIsNonFatal(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	rc := newFakeCaller(t)
	rc.replyStatus(OpIperfClientRun, StatusUnavailable, "unable to connect to server: Connection refused", nil)

	results := &SessionResults{}
	plan := newQuickPlan(newTestOps(t, fr), results)
	if err := plan.stepTCPReverse(context.Background(), rc); err != nil {
		t.Fatalf("stepTCPReverse is fatal for an unreachable data port: %v", err)
	}
	if !hasUnreachableSkip(results, "tcp") {
		t.Errorf("SkippedTests = %+v, want a %q unreachable skip", results.SkippedTests, "tcp")
	}
}
