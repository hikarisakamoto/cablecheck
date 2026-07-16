package runnertest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
	"cablecheck/internal/testutil"
)

// recordTB wraps the real testing.TB but records Errorf/Fatalf calls so tests
// can assert that FakeRunner reports unmatched calls via t.Errorf and never
// t.Fatalf (Fatalf off the test goroutine is undefined behavior).
type recordTB struct {
	testing.TB
	mu     sync.Mutex
	errors []string
	fatals []string
}

func (r *recordTB) Helper() {}

func (r *recordTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, format)
}

func (r *recordTB) Fatalf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fatals = append(r.fatals, format)
}

func (r *recordTB) counts() (errs, fatals int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errors), len(r.fatals)
}

func runSpec(name string, args ...string) runner.CommandSpec {
	return runner.CommandSpec{Name: name, Args: args}
}

func TestArgMatchers(t *testing.T) {
	tests := []struct {
		name string
		m    runnertest.ArgMatcher
		args []string
		want bool
	}{
		{"exact match", runnertest.ArgsExact("-S", "eth0"), []string{"-S", "eth0"}, true},
		{"exact extra arg", runnertest.ArgsExact("-S", "eth0"), []string{"-S", "eth0", "-v"}, false},
		{"exact order", runnertest.ArgsExact("-S", "eth0"), []string{"eth0", "-S"}, false},
		{"prefix match", runnertest.ArgsPrefix("-S"), []string{"-S", "eth0"}, true},
		{"prefix mismatch", runnertest.ArgsPrefix("-S"), []string{"eth0", "-S"}, false},
		{"prefix longer than args", runnertest.ArgsPrefix("-S", "eth0"), []string{"-S"}, false},
		{"contain out of order", runnertest.ArgsContain("eth0", "-S"), []string{"-S", "eth0", "-v"}, true},
		{"contain missing token", runnertest.ArgsContain("-S", "eth1"), []string{"-S", "eth0"}, false},
		{"any empty", runnertest.AnyArgs(), nil, true},
		{"any full", runnertest.AnyArgs(), []string{"x"}, true},
		{"zero value matches all", runnertest.ArgMatcher{}, []string{"x", "y"}, true},
		{"custom match", runnertest.MatchFunc(func(got []string) bool { return len(got) == 1 }), []string{"x"}, true},
		{"custom mismatch", runnertest.MatchFunc(func(got []string) bool { return len(got) == 1 }), []string{"x", "y"}, false},
	}
	for _, tc := range tests {
		if got := tc.m.Matches(tc.args); got != tc.want {
			t.Errorf("%s: matcher(%q) = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestCustomMatcherRanksBelowBuiltins(t *testing.T) {
	f := runnertest.New(t)
	f.Script(runnertest.Script{Name: "tool", Match: runnertest.ArgsExact("run", "now"),
		Result: runner.CommandResult{Stdout: []byte("exact")}})
	// A custom matcher that DELEGATES to built-ins must still rank as
	// custom: it is added later, so if it were misranked Exact it would
	// beat the real ArgsExact script above by recency.
	f.Script(runnertest.Script{Name: "tool", Match: runnertest.MatchFunc(func(got []string) bool {
		return runnertest.ArgsExact("run", "now").Matches(got) ||
			runnertest.ArgsExact("run", "later").Matches(got)
	}), Result: runner.CommandResult{Stdout: []byte("custom")}})
	// Newest and least specific: loses to the custom matcher when both match.
	f.Script(runnertest.Script{Name: "tool", Match: runnertest.AnyArgs(),
		Result: runner.CommandResult{Stdout: []byte("any")}})

	run := func(args ...string) string {
		t.Helper()
		res, err := f.Run(t.Context(), runSpec("tool", args...))
		if err != nil {
			t.Fatalf("Run(%q): %v", args, err)
		}
		return string(res.Stdout)
	}
	if got := run("run", "now"); got != "exact" {
		t.Errorf("delegating custom matcher stole the exact script: got %q, want \"exact\"", got)
	}
	if got := run("run", "later"); got != "custom" {
		t.Errorf("custom matcher lost to AnyArgs: got %q, want \"custom\"", got)
	}
	if got := run("other"); got != "any" {
		t.Errorf("generic args got %q, want \"any\"", got)
	}
}

func TestScriptMatchingSpecificity(t *testing.T) {
	f := runnertest.New(t)
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.AnyArgs(),
		Result: runner.CommandResult{Stdout: []byte("any")}})
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsContain("-S"),
		Result: runner.CommandResult{Stdout: []byte("contain")}})
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		Result: runner.CommandResult{Stdout: []byte("prefix")}})
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"),
		Result: runner.CommandResult{Stdout: []byte("exact")}})

	run := func(args ...string) string {
		t.Helper()
		res, err := f.Run(t.Context(), runSpec("ethtool", args...))
		if err != nil {
			t.Fatalf("Run(%q): %v", args, err)
		}
		return string(res.Stdout)
	}

	if got := run("-S", "eth0"); got != "exact" {
		t.Errorf("exact args got script %q, want \"exact\"", got)
	}
	if got := run("-S", "eth1"); got != "prefix" {
		t.Errorf("prefix args got script %q, want \"prefix\"", got)
	}
	if got := run("-v", "-S"); got != "contain" {
		t.Errorf("contain args got script %q, want \"contain\"", got)
	}
	if got := run("settings"); got != "any" {
		t.Errorf("generic args got script %q, want \"any\"", got)
	}

	// Recency: a later script of EQUAL specificity wins ...
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.AnyArgs(),
		Result: runner.CommandResult{Stdout: []byte("any2")}})
	if got := run("settings"); got != "any2" {
		t.Errorf("after override, generic args got %q, want \"any2\"", got)
	}
	// ... but a more specific earlier script still beats a recent generic one.
	if got := run("-S", "eth0"); got != "exact" {
		t.Errorf("exact args after generic override got %q, want \"exact\"", got)
	}
}

func TestScriptTimesConsumedFIFO(t *testing.T) {
	rt := &recordTB{TB: t}
	f := runnertest.New(rt)
	// Unlimited fallback added FIRST; Times-limited scripts must still be
	// consumed before it, oldest first (before/after snapshot pattern).
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"),
		Result: runner.CommandResult{Stdout: []byte("steady")}})
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"), Times: 1,
		Result: runner.CommandResult{Stdout: []byte("before")}})
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"), Times: 1,
		Result: runner.CommandResult{Stdout: []byte("after")}})

	for i, want := range []string{"before", "after", "steady", "steady"} {
		res, err := f.Run(t.Context(), runSpec("ethtool", "-S", "eth0"))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got := string(res.Stdout); got != want {
			t.Errorf("call %d got %q, want %q", i, got, want)
		}
	}
	if errs, fatals := rt.counts(); errs != 0 || fatals != 0 {
		t.Errorf("recorded %d errors, %d fatals; want none", errs, fatals)
	}
}

func TestUnmatchedCallErrorfNeverFatalf(t *testing.T) {
	rt := &recordTB{TB: t}
	f := runnertest.New(rt)
	res, err := f.Run(t.Context(), runSpec("ping", "-c", "1"))
	if err == nil {
		t.Error("unmatched Run returned nil error, want error")
	}
	if res != nil {
		t.Errorf("unmatched Run returned result %+v, want nil", res)
	}
	if _, err := f.Start(t.Context(), runSpec("iperf3", "-s")); err == nil {
		t.Error("unmatched Start returned nil error, want error")
	}
	errs, fatals := rt.counts()
	if errs != 2 {
		t.Errorf("recorded %d Errorf calls, want 2", errs)
	}
	if fatals != 0 {
		t.Errorf("recorded %d Fatalf calls, want 0 — Fatalf is forbidden off the test goroutine", fatals)
	}
	// Unmatched calls are still recorded.
	if got := len(f.Calls()); got != 2 {
		t.Errorf("Calls() = %d entries, want 2", got)
	}
}

func TestMissingReturnsErrNotFound(t *testing.T) {
	rt := &recordTB{TB: t}
	f := runnertest.New(rt)
	f.Missing("iperf3")

	if _, err := f.LookPath("iperf3"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("LookPath err = %v, want errors.Is(err, exec.ErrNotFound)", err)
	}
	if got, err := f.LookPath("ping"); err != nil || got != "ping" {
		t.Errorf("LookPath(ping) = (%q, %v), want (\"ping\", nil)", got, err)
	}
	if _, err := f.Run(t.Context(), runSpec("iperf3", "--version")); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Run err = %v, want errors.Is(err, exec.ErrNotFound)", err)
	}
	if _, err := f.Start(t.Context(), runSpec("iperf3", "-s")); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Start err = %v, want errors.Is(err, exec.ErrNotFound)", err)
	}
	if errs, fatals := rt.counts(); errs != 0 || fatals != 0 {
		t.Errorf("Missing-name calls recorded %d errors, %d fatals; want none (not an unmatched call)", errs, fatals)
	}
}

func TestDelayStartedChannels(t *testing.T) {
	testutil.LeakCheck(t)
	f := runnertest.New(t)

	t.Run("delay released", func(t *testing.T) {
		started := make(chan struct{})
		delay := make(chan struct{})
		f.Script(runnertest.Script{Name: "ping", Match: runnertest.AnyArgs(), Times: 1,
			Started: started, Delay: delay,
			Result: runner.CommandResult{ExitCode: 1}})
		type outcome struct {
			res *runner.CommandResult
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := f.Run(t.Context(), runSpec("ping", "-c", "5"))
			done <- outcome{res, err}
		}()
		testutil.WaitFor(t, started, "scripted call to start")
		select {
		case <-done:
			t.Fatal("Run returned while Delay channel still open")
		default:
		}
		close(delay)
		out := <-done
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.res.ExitCode != 1 {
			t.Errorf("ExitCode = %d, want 1", out.res.ExitCode)
		}
	})

	t.Run("ctx cancel during delay", func(t *testing.T) {
		started := make(chan struct{})
		delay := make(chan struct{}) // never closed
		f.Script(runnertest.Script{Name: "ping", Match: runnertest.AnyArgs(), Times: 1,
			Started: started, Delay: delay})
		ctx, cancel := context.WithCancel(t.Context())
		type outcome struct {
			res *runner.CommandResult
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := f.Run(ctx, runSpec("ping", "-c", "5"))
			done <- outcome{res, err}
		}()
		testutil.WaitFor(t, started, "scripted call to start")
		cancel()
		out := <-done
		if !errors.Is(out.err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(err, context.Canceled)", out.err)
		}
		if out.res == nil {
			t.Fatal("result = nil, want partial result on cancel")
		}
		// The real runner's parent-cancel path delivers SIGTERM first (see
		// TestContextCancelKillsChild); the fake must mirror that so
		// consumer assertions on Signal hold against production behavior.
		if out.res.Signal != "SIGTERM" {
			t.Errorf("Signal = %q, want SIGTERM (contract fidelity with the real runner's cancel path)", out.res.Signal)
		}
		if out.res.ExitCode != -1 {
			t.Errorf("ExitCode = %d, want -1", out.res.ExitCode)
		}
	})
}

func TestCallRecording(t *testing.T) {
	f := runnertest.New(t)
	f.Script(runnertest.Script{Name: "ethtool", Match: runnertest.AnyArgs()})
	f.Script(runnertest.Script{Name: "iperf3", Match: runnertest.AnyArgs()})

	spec := runSpec("ethtool", "-S", "eth0")
	spec.Label = "stats-before"
	if _, err := f.Run(t.Context(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := f.Start(t.Context(), runSpec("iperf3", "-s", "-1")); err != nil {
		t.Fatalf("Start: %v", err)
	}

	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls() = %d entries, want 2", len(calls))
	}
	if calls[0].Kind != "run" || calls[0].Name != "ethtool" {
		t.Errorf("call 0 = %+v, want run ethtool", calls[0])
	}
	if calls[0].Spec.Label != "stats-before" {
		t.Errorf("call 0 spec label = %q, want stats-before", calls[0].Spec.Label)
	}
	if calls[1].Kind != "start" || calls[1].Name != "iperf3" {
		t.Errorf("call 1 = %+v, want start iperf3", calls[1])
	}
	if got := f.CallsFor("ethtool"); len(got) != 1 || got[0].Args[1] != "eth0" {
		t.Errorf("CallsFor(ethtool) = %+v, want the single -S eth0 call", got)
	}
	if got := f.Processes(); len(got) != 1 {
		t.Errorf("Processes() = %d entries, want 1", len(got))
	}
}

func TestScriptedTimeoutSynthesizesError(t *testing.T) {
	f := runnertest.New(t)
	f.Script(runnertest.Script{Name: "ping", Match: runnertest.AnyArgs(),
		Result: runner.CommandResult{TimedOut: true, ExitCode: -1, Signal: "SIGTERM"}})
	res, err := f.Run(t.Context(), runSpec("ping", "-c", "500"))
	if res == nil || !res.TimedOut {
		t.Fatalf("result = %+v, want non-nil with TimedOut=true", res)
	}
	if !errors.Is(err, runner.ErrTimeout) {
		t.Errorf("err = %v, want errors.Is(err, runner.ErrTimeout) — fake must honor the real error contract", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
}

func TestScriptErrReturned(t *testing.T) {
	f := runnertest.New(t)
	boom := errors.New("iperf3 refused to start")
	f.Script(runnertest.Script{Name: "iperf3", Match: runnertest.AnyArgs(), Err: boom})
	if _, err := f.Start(t.Context(), runSpec("iperf3", "-s")); !errors.Is(err, boom) {
		t.Errorf("Start err = %v, want scripted error", err)
	}
	if got := len(f.Processes()); got != 0 {
		t.Errorf("Processes() = %d entries after failed Start, want 0", got)
	}
}

func TestFromFixture(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	write("simple.txt", "plain stdout fixture\n")
	write("trio.stdout", "out\n")
	write("trio.stderr", "err\n")
	write("trio.exit", "2\n")
	write("erronly.stderr", "boom\n")
	write("erronly.exit", "1")
	write("badexit.exit", "not-a-number")

	res, err := runnertest.FromFixture(dir, "simple")
	if err != nil {
		t.Fatalf("FromFixture(simple): %v", err)
	}
	if string(res.Stdout) != "plain stdout fixture\n" || res.ExitCode != 0 {
		t.Errorf("simple = %+v, want stdout-only, exit 0", res)
	}

	res, err = runnertest.FromFixture(dir, "trio")
	if err != nil {
		t.Fatalf("FromFixture(trio): %v", err)
	}
	if string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" || res.ExitCode != 2 {
		t.Errorf("trio = %+v, want out/err/exit-2 triplet", res)
	}

	res, err = runnertest.FromFixture(dir, "erronly")
	if err != nil {
		t.Fatalf("FromFixture(erronly): %v", err)
	}
	if len(res.Stdout) != 0 || string(res.Stderr) != "boom\n" || res.ExitCode != 1 {
		t.Errorf("erronly = %+v, want empty stdout, stderr, exit 1", res)
	}

	if _, err := runnertest.FromFixture(dir, "nope"); err == nil {
		t.Error("FromFixture(missing) = nil error, want error")
	}
	if _, err := runnertest.FromFixture(dir, "badexit"); err == nil {
		t.Error("FromFixture(bad exit file) = nil error, want error")
	}
}
