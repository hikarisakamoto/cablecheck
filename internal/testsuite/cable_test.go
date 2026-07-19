package testsuite

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner/runnertest"
)

type cableCoordCaller struct {
	events  []string
	dropEnd bool
}

func (c *cableCoordCaller) Call(_ context.Context, op string, _ any, _ time.Duration,
	_ func(protocol.TestProgress)) (*protocol.TestResult, error) {
	c.events = append(c.events, "call:"+op)
	if op == OpCableTestWindowEnd && c.dropEnd {
		return nil, errors.New("control connection dropped")
	}
	result := json.RawMessage(`{}`)
	if op == OpCableTestWindowEnd {
		result = json.RawMessage(`{"selfInflictedCarrierEvents":3}`)
	}
	return &protocol.TestResult{Status: StatusOK, Result: result}, nil
}

func (c *cableCoordCaller) Warn(code, _ string) {
	c.events = append(c.events, "warn:"+code)
}

func (c *cableCoordCaller) SetIdleTimeout(d time.Duration) {
	c.events = append(c.events, "idle:"+d.String())
}

// TestCableTestCoordination verifies the ordered warning/timeout/window
// barrier, privileged command arguments, local result retention, and
// best-effort recovery after a control-channel drop.
func TestCableTestCoordination(t *testing.T) {
	restoreEUID := geteuid
	geteuid = func() int { return 0 }
	defer func() { geteuid = restoreEUID }()

	for _, tc := range []struct {
		name    string
		tdr     bool
		dropEnd bool
	}{
		{name: "cable test recovers"},
		{name: "TDR is explicitly requested", tdr: true},
		{name: "connection drop keeps local result", dropEnd: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := runnertest.New(t)
			fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("--cable-test", "eth0"),
				Result: fixture(t, "ethtool", "cabletest_ok")})
			if tc.tdr {
				fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("--cable-test-tdr", "eth0"),
					Result: fixture(t, "ethtool", "cabletest_tdr")})
			}

			caller := &cableCoordCaller{dropEnd: tc.dropEnd}
			results := &SessionResults{}
			plan := &CablePlan{
				Base: func(context.Context, peer.RemoteCaller) error {
					caller.events = append(caller.events, "base")
					return nil
				},
				Tester: &CableTester{R: fr, IfName: "eth0", SudoOK: true},
				TDR:    tc.tdr, Results: results,
				NormalIdleTimeout: 20 * time.Second,
				BeginWindow:       func() { caller.events = append(caller.events, "monitor:begin") },
				EndWindow: func() uint64 {
					caller.events = append(caller.events, "monitor:end")
					return 2
				},
			}
			if err := plan.Run(context.Background(), caller); err != nil {
				t.Errorf("CablePlan.Run: %v", err)
			}
			if results.CableTest == nil || !results.CableTest.Available || len(results.CableTest.Pairs) != 4 {
				t.Errorf("CableTest result = %+v, want retained four-pair result", results.CableTest)
			}
			if tc.tdr && len(results.CableTest.Samples) != 5 {
				t.Errorf("TDR samples = %+v, want five", results.CableTest.Samples)
			}
			wantCounts := model.PeerCarrierEvents{PC1: 2, PC2: 3}
			if tc.dropEnd {
				wantCounts.PC2 = 0
			}
			if results.CableTest.SelfInflictedCarrierEvents != wantCounts {
				t.Errorf("carrier counts = %+v, want %+v", results.CableTest.SelfInflictedCarrierEvents, wantCounts)
			}

			wantPrefix := []string{
				"base",
				"warn:cable_test_window",
				"idle:4m0s",
				"call:" + OpCableTestWindowStart,
				"monitor:begin",
				"call:" + OpCableTestWindowEnd,
				"monitor:end",
				"idle:20s",
			}
			if !slices.Equal(caller.events, wantPrefix) {
				t.Errorf("coordination events = %q, want %q", caller.events, wantPrefix)
			}
			if tc.dropEnd {
				if len(results.Notes) == 0 || !slices.ContainsFunc(results.Notes, func(note string) bool {
					return strings.Contains(note, "best-effort")
				}) {
					t.Errorf("drop notes = %q, want best-effort coordination note", results.Notes)
				}
			}
		})
	}
}

// TestCableTestRequiresRootWithoutSudo verifies a denied privilege path is
// unavailable data and never invokes ethtool.
func TestCableTestRequiresRootWithoutSudo(t *testing.T) {
	restoreEUID := geteuid
	geteuid = func() int { return 1000 }
	defer func() { geteuid = restoreEUID }()

	fr := runnertest.New(t)
	result, err := (&CableTester{R: fr, IfName: "eth0", SudoOK: false}).Run(context.Background(), false)
	if err != nil {
		t.Errorf("CableTester.Run: %v", err)
	}
	want := "requires root; passwordless sudo unavailable"
	if result.Available || result.UnavailableReason != want || len(result.Pairs) != 0 {
		t.Errorf("result = %+v, want unavailable %q without faults", result, want)
	}
	if calls := fr.Calls(); len(calls) != 0 {
		t.Errorf("runner calls = %+v, want none", calls)
	}
}

// TestCableTesterUsesPrivilegedWrapper verifies the runnable non-root path
// invokes passwordless sudo with an argv vector, never a shell string.
func TestCableTesterUsesPrivilegedWrapper(t *testing.T) {
	restoreEUID := geteuid
	geteuid = func() int { return 1000 }
	defer func() { geteuid = restoreEUID }()

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "sudo", Match: runnertest.ArgsExact(
		"-n", "--", "ethtool", "--cable-test", "eth0"),
		Result: fixture(t, "ethtool", "cabletest_ok")})
	result, err := (&CableTester{R: fr, IfName: "eth0", SudoOK: true}).Run(context.Background(), false)
	if err != nil {
		t.Errorf("CableTester.Run: %v", err)
	}
	if !result.Available || len(result.Pairs) != 4 {
		t.Errorf("result = %+v, want available pair result", result)
	}
}

func TestCableTestRequestedTDRUnavailabilityIsRecorded(t *testing.T) {
	restoreEUID := geteuid
	geteuid = func() int { return 0 }
	defer func() { geteuid = restoreEUID }()

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("--cable-test", "eth0"),
		Result: fixture(t, "ethtool", "cabletest_ok")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("--cable-test-tdr", "eth0"),
		Result: fixture(t, "ethtool", "cabletest_unsupported")})

	result, err := (&CableTester{R: fr, IfName: "eth0", SudoOK: true}).Run(context.Background(), true)
	if err != nil {
		t.Errorf("CableTester.Run: %v", err)
	}
	if !result.Available {
		t.Errorf("base cable test Available = false, want true")
	}
	if result.TDRUnavailableReason != "driver does not support cable test" {
		t.Errorf("TDRUnavailableReason = %q, want unsupported-driver reason", result.TDRUnavailableReason)
	}
}
