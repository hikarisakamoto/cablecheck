package testsuite

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner"
)

const (
	cableCommandTimeout = 90 * time.Second
	cableWindowTimeout  = 4 * time.Minute
)

// CableTester runs the root-only ethtool pair and optional TDR diagnostics
// through the injected command runner.
type CableTester struct {
	// R executes ethtool (or sudo -n -- ethtool) without a shell.
	R runner.Runner
	// IfName is the interface under test.
	IfName string
	// RawDir receives complete stdout/stderr artifacts when non-empty.
	RawDir string
	// SudoOK is the preflight result for passwordless `sudo -n`.
	SudoOK bool
}

// Run executes `ethtool --cable-test` and, only when tdr is true, the
// separately opt-in `--cable-test-tdr`. Privilege unavailability is returned
// as unavailable data, never as an error or fault.
func (t *CableTester) Run(ctx context.Context, tdr bool) (model.CableTestResult, error) {
	base, runnable, err := t.run(ctx, "--cable-test", "cable-test")
	if !runnable || err != nil || !base.Available || !tdr {
		return base, err
	}
	tdrResult, _, err := t.run(ctx, "--cable-test-tdr", "cable-test-tdr")
	if err != nil {
		return base, err
	}
	if tdrResult.Available {
		base.Samples = tdrResult.Samples
		base.UnparsedLines = tdrResult.UnparsedLines
	}
	return base, nil
}

func (t *CableTester) run(ctx context.Context, flag, label string) (model.CableTestResult, bool, error) {
	spec := runner.CommandSpec{
		Name: "ethtool", Args: []string{flag, t.IfName},
		Timeout: cableCommandTimeout, Label: label,
	}
	if t.RawDir != "" {
		spec.TeeStdoutPath = filepath.Join(t.RawDir, label+".stdout")
		spec.TeeStderrPath = filepath.Join(t.RawDir, label+".stderr")
	}
	privileged, ok := Privileged(spec, t.SudoOK)
	if !ok {
		return model.CableTestResult{
			Available: false, UnavailableReason: "requires root; passwordless sudo unavailable",
		}, false, nil
	}
	result, err := t.R.Run(ctx, privileged)
	if err != nil {
		return model.CableTestResult{}, true, fmt.Errorf("testsuite: %s: %w", label, err)
	}
	if flag == "--cable-test-tdr" {
		return parser.ParseCableTestTDR(result.Stdout, result.Stderr, result.ExitCode), true, nil
	}
	return parser.ParseCableTest(result.Stdout, result.Stderr, result.ExitCode), true, nil
}

// CablePlan appends one opt-in disruptive diagnostic to any mode plan. The
// base plan completes first, leaving its counter snapshots outside the
// self-inflicted link-loss interval.
type CablePlan struct {
	// Base is the selected quick, standard, or soak plan.
	Base peer.PlanFunc
	// Tester runs the local coordinator's ethtool commands.
	Tester *CableTester
	// TDR enables the separately opt-in TDR command.
	TDR bool
	// Results receives the locally captured result immediately after the
	// command returns, before best-effort peer recovery.
	Results *SessionResults
	// NormalIdleTimeout is restored after the window; zero selects the
	// protocol default.
	NormalIdleTimeout time.Duration
	// BeginWindow marks local monitor events self-inflicted.
	BeginWindow func()
	// EndWindow clears the local monitor annotation.
	EndWindow func()
	// OnStep announces the appended cable diagnostic step when non-nil.
	OnStep func(step, total int, name string)
	// Step is the appended step's 1-based index.
	Step int
	// Total is the complete displayed plan length.
	Total int
}

// Run executes the base plan, coordinates the widened link-loss window,
// captures the local diagnostic, and treats post-test peer recovery as best
// effort so a dropped control connection cannot discard the result.
func (p *CablePlan) Run(ctx context.Context, rc peer.RemoteCaller) error {
	if p.Base != nil {
		if err := p.Base(ctx, rc); err != nil {
			return err
		}
	}
	if p.OnStep != nil {
		p.OnStep(p.Step, p.Total, "cable diagnostics")
	}

	rc.Warn("cable_test_window", "cable diagnostics are starting; the tested link may temporarily go down")
	rc.SetIdleTimeout(cableWindowTimeout)
	normalTimeout := p.NormalIdleTimeout
	if normalTimeout <= 0 {
		normalTimeout = protocol.DefaultIdleTimeout
	}
	windowStarted := false
	defer func() {
		if windowStarted && p.EndWindow != nil {
			p.EndWindow()
		}
		rc.SetIdleTimeout(normalTimeout)
	}()

	start, err := rc.Call(ctx, OpCableTestWindowStart,
		protocol.CableTestWindowParams{IdleTimeoutMs: cableWindowTimeout.Milliseconds()},
		30*time.Second, nil)
	if err != nil || start.Status != StatusOK {
		reason := "peer coordination failed before cable-test window"
		if err != nil {
			reason += ": " + err.Error()
		} else if start.Error != "" {
			reason += ": " + start.Error
		}
		p.Results.CableTest = &model.CableTestResult{Available: false, UnavailableReason: reason}
		p.Results.Notes = append(p.Results.Notes, reason)
		return nil
	}
	windowStarted = true
	if p.BeginWindow != nil {
		p.BeginWindow()
	}

	result, runErr := p.Tester.Run(ctx, p.TDR)
	p.Results.CableTest = &result
	if runErr != nil {
		return runErr
	}
	end, endErr := rc.Call(ctx, OpCableTestWindowEnd, nil, 30*time.Second, nil)
	if endErr != nil || end.Status != StatusOK {
		note := "cable-test peer recovery was best-effort; the local diagnostic result was retained"
		if endErr != nil {
			note += ": " + endErr.Error()
		} else if end.Error != "" {
			note += ": " + end.Error
		}
		p.Results.Notes = append(p.Results.Notes, note)
	}
	return nil
}
