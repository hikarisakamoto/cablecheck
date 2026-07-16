// Package runnertest provides a scriptable runner.Runner double for hermetic
// tests: FakeRunner matches invocations against ordered Scripts and returns
// canned results (often loaded from testdata fixtures), records every call,
// and hands out FakeProcess handles with observable lifecycle channels.
//
// FakeRunner is called from application goroutines, so failures are reported
// with t.Errorf plus a returned error — never t.Fatalf, which is undefined
// behavior off the test goroutine.
package runnertest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cablecheck/internal/runner"
)

// ArgMatcher decides whether a Script applies to a recorded argument vector.
// Every matcher carries its specificity rank explicitly, recorded by the
// constructor that built it, so script selection never probes or guesses how
// broad a matcher is: a MatchFunc predicate ranks as custom even when it
// delegates to built-in matchers internally. The zero value matches every
// argument vector and ranks lowest, like AnyArgs.
type ArgMatcher struct {
	fn   func(args []string) bool
	rank int
}

// Matches reports whether the matcher accepts args. The zero-value
// ArgMatcher matches everything.
func (m ArgMatcher) Matches(args []string) bool {
	if m.fn == nil {
		return true
	}
	return m.fn(args)
}

// Matcher specificity ranks (higher wins script selection).
const (
	specAny = iota
	specCustom
	specContain
	specPrefix
	specExact
)

// ArgsExact matches only the exact argument vector, in order.
func ArgsExact(args ...string) ArgMatcher {
	want := slices.Clone(args)
	return ArgMatcher{rank: specExact, fn: func(got []string) bool {
		return slices.Equal(got, want)
	}}
}

// ArgsPrefix matches argument vectors that start with the given elements.
func ArgsPrefix(prefix ...string) ArgMatcher {
	want := slices.Clone(prefix)
	return ArgMatcher{rank: specPrefix, fn: func(got []string) bool {
		return len(got) >= len(want) && slices.Equal(got[:len(want)], want)
	}}
}

// ArgsContain matches argument vectors that contain every token, in any
// order and at any position.
func ArgsContain(tokens ...string) ArgMatcher {
	want := slices.Clone(tokens)
	return ArgMatcher{rank: specContain, fn: func(got []string) bool {
		for _, tok := range want {
			if !slices.Contains(got, tok) {
				return false
			}
		}
		return true
	}}
}

// AnyArgs matches every argument vector. It is equivalent to the zero-value
// ArgMatcher (and to leaving Script.Match unset), spelled out for readability.
func AnyArgs() ArgMatcher { return ArgMatcher{} }

// MatchFunc wraps a custom predicate. Custom matchers rank above AnyArgs but
// below the shape-specific built-ins, even when the predicate delegates to
// them internally — the rank travels with the value, it is never inferred
// from behavior.
func MatchFunc(fn func(args []string) bool) ArgMatcher {
	return ArgMatcher{rank: specCustom, fn: fn}
}

// Script is one canned behavior. Selection among matching scripts: the most
// specific matcher wins (Exact > Prefix > Contain > custom MatchFunc > Any);
// at equal specificity, Times-limited scripts are consumed first, oldest
// first (FIFO — the before/after snapshot pattern), otherwise the most
// recently added script wins (override pattern).
type Script struct {
	// Name is the executable name the script applies to; empty matches
	// any name.
	Name string
	// Match filters by argument vector; the zero value matches any args.
	Match ArgMatcher
	// Result is the canned CommandResult template. A TimedOut result also
	// makes the fake return the contract timeout error automatically.
	Result runner.CommandResult
	// StdoutFile, when non-empty, loads Result.Stdout from this path on
	// every use (the fixture convention).
	StdoutFile string
	// Err, when non-nil, is returned instead of a result (simulates
	// infrastructure failures such as a failed Start).
	Err error
	// Delay, when non-nil, blocks Run — or keeps a Started process
	// alive — until the channel is closed (or ctx is done). Combined
	// with Started this is the no-sleep cancellation-test mechanism.
	Delay <-chan struct{}
	// Started, when non-nil, is closed as soon as the first matching
	// call begins: a synchronization point for tests.
	Started chan<- struct{}
	// Times limits how many calls the script serves; 0 means unlimited.
	Times int
}

// RecordedCall is one observed Run or Start invocation.
type RecordedCall struct {
	// Name is the invoked executable name.
	Name string
	// Args is a copy of the argument vector.
	Args []string
	// Spec is the full CommandSpec as received.
	Spec runner.CommandSpec
	// Kind is "run" or "start".
	Kind string
}

// scriptState wraps a Script with its bookkeeping.
type scriptState struct {
	Script
	used      int
	startOnce sync.Once
}

// FakeRunner is a mutex-guarded, scriptable runner.Runner. Create it with
// New; it registers a t.Cleanup that unblocks anything still waiting on it.
type FakeRunner struct {
	t testing.TB

	mu      sync.Mutex
	scripts []*scriptState
	missing map[string]bool
	calls   []RecordedCall
	procs   []*FakeProcess
	nextPID int

	aborted   chan struct{} // closed at cleanup: unblocks Delay waits and live processes
	abortOnce sync.Once
}

var _ runner.Runner = (*FakeRunner)(nil)

// New returns an empty FakeRunner bound to t.
func New(t testing.TB) *FakeRunner {
	f := &FakeRunner{
		t:       t,
		missing: make(map[string]bool),
		nextPID: 42000,
		aborted: make(chan struct{}),
	}
	t.Cleanup(f.shutdown)
	return f
}

// shutdown unblocks every outstanding Delay wait and undead FakeProcess so
// tests cannot leak goroutines through the fake.
func (f *FakeRunner) shutdown() {
	f.abortOnce.Do(func() { close(f.aborted) })
}

// Script adds s; chainable.
func (f *FakeRunner) Script(s Script) *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, &scriptState{Script: s})
	return f
}

// Missing marks executable names as absent: LookPath, Run and Start on them
// return an error satisfying errors.Is(err, exec.ErrNotFound).
func (f *FakeRunner) Missing(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		f.missing[n] = true
	}
}

// Calls returns a copy of every recorded Run/Start invocation, in order.
func (f *FakeRunner) Calls() []RecordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsFor returns the recorded invocations of one executable name.
func (f *FakeRunner) CallsFor(name string) []RecordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RecordedCall
	for _, c := range f.calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// Processes returns every FakeProcess handed out by Start, in Start order.
func (f *FakeRunner) Processes() []*FakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.procs)
}

// LookPath resolves name to itself, or to exec.ErrNotFound if marked Missing.
func (f *FakeRunner) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing[name] {
		return "", notFoundError(name)
	}
	return name, nil
}

// Run executes a scripted command to completion, honoring the real Runner
// error contract (non-zero exit is data; TimedOut results carry the timeout
// error; a pre-canceled ctx fails outright like the real cmd.Start; ctx
// cancellation during Delay wraps ctx.Err()).
func (f *FakeRunner) Run(ctx context.Context, spec runner.CommandSpec) (*runner.CommandResult, error) {
	s, err := f.admit(spec, "run")
	if err != nil {
		return nil, err
	}
	signalStarted(s)
	// A pre-canceled ctx fails Run outright, like the real Exec.Run whose
	// cmd.Start fails before the child ever launches: nil result, error
	// wrapping ctx.Err(). Mirrors the identical check in Start.
	if err := ctx.Err(); err != nil {
		return nil, canceledError(spec.Name, err)
	}
	if s.Delay != nil {
		select {
		case <-s.Delay:
		case <-ctx.Done():
			res, _ := f.buildResult(s, spec)
			if res != nil {
				res.ExitCode = -1
				// The real runner's parent-cancel path delivers SIGTERM
				// first; mirror it so consumer assertions on Signal hold
				// against production behavior.
				res.Signal = "SIGTERM"
			}
			return res, canceledError(spec.Name, ctx.Err())
		case <-f.aborted:
			return nil, fmt.Errorf("runnertest: FakeRunner shut down during run of %s", spec.Name)
		}
	}
	if s.Err != nil {
		return nil, s.Err
	}
	res, err := f.buildResult(s, spec)
	if err != nil {
		return nil, err
	}
	if res.TimedOut {
		return res, timeoutError(spec.Name)
	}
	return res, nil
}

// Start launches a scripted long-lived process and returns its FakeProcess.
// If the script has a Delay channel, the process exits with the scripted
// result when the channel is closed; otherwise it lives until Exit,
// Terminate or Kill. Mirroring the real Runner.Start contract ("Cancelling
// ctx kills the process group"): a pre-canceled ctx fails Start outright
// (like the real cmd.Start), and canceling ctx later synthesizes a SIGTERM
// death whose Wait error wraps ctx.Err().
func (f *FakeRunner) Start(ctx context.Context, spec runner.CommandSpec) (runner.Process, error) {
	s, err := f.admit(spec, "start")
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		signalStarted(s)
		return nil, fmt.Errorf("runnertest: start %s: %w", spec.Name, err)
	}
	if s.Err != nil {
		signalStarted(s)
		return nil, s.Err
	}
	res, err := f.buildResult(s, spec)
	if err != nil {
		signalStarted(s)
		return nil, err
	}

	f.mu.Lock()
	f.nextPID++
	p := newFakeProcess(f.nextPID, spec, *res, f.aborted)
	f.procs = append(f.procs, p)
	f.mu.Unlock()

	// Signalled only after the process is observable via Processes().
	signalStarted(s)
	// Lifecycle watcher: auto-exit with the scripted result when Delay
	// closes (a nil Delay channel blocks forever), and mirror the real
	// runner's parent-cancel kill by synthesizing a SIGTERM death when ctx
	// is canceled. TermCh/KillCh stay untouched: ctx cancellation is not a
	// consumer-initiated Terminate/Kill.
	delay := s.Delay
	scripted := *res
	go func() {
		select {
		case <-delay:
			p.Exit(scripted)
		case <-ctx.Done():
			p.dieBySignal("SIGTERM", canceledError(spec.Name, ctx.Err()))
		case <-p.Done():
		case <-f.aborted:
		}
	}()
	return p, nil
}

// admit records the call, resolves Missing names, and selects the script.
func (f *FakeRunner) admit(spec runner.CommandSpec, kind string) (*scriptState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, RecordedCall{
		Name: spec.Name,
		Args: slices.Clone(spec.Args),
		Spec: spec,
		Kind: kind,
	})
	if f.missing[spec.Name] {
		return nil, notFoundError(spec.Name)
	}
	s := f.match(spec.Name, spec.Args)
	if s == nil {
		err := fmt.Errorf("runnertest: no script matches %s of %s %q", kind, spec.Name, spec.Args)
		f.t.Errorf("%v", err) // never Fatalf: we may be off the test goroutine
		return nil, err
	}
	s.used++
	return s, nil
}

// match selects the best non-exhausted script for (name, args) under the
// rules documented on Script. Caller holds f.mu.
func (f *FakeRunner) match(name string, args []string) *scriptState {
	var best *scriptState
	for _, s := range f.scripts {
		if s.Name != "" && s.Name != name {
			continue
		}
		if s.Times > 0 && s.used >= s.Times {
			continue
		}
		if !s.Match.Matches(args) {
			continue
		}
		if best == nil || s.Match.rank > best.Match.rank {
			best = s
			continue
		}
		if s.Match.rank < best.Match.rank {
			continue
		}
		// Equal specificity: Times-limited beats unlimited; among
		// Times-limited keep the oldest (FIFO); among unlimited the
		// most recent wins.
		bestLimited, sLimited := best.Times > 0, s.Times > 0
		switch {
		case sLimited && !bestLimited:
			best = s
		case !sLimited && bestLimited:
			// keep best
		case sLimited && bestLimited:
			// keep best (older, FIFO)
		default:
			best = s // most recent unlimited
		}
	}
	return best
}

// buildResult instantiates the script's result template for one call,
// loading StdoutFile if configured. Load failures are test failures.
func (f *FakeRunner) buildResult(s *scriptState, spec runner.CommandSpec) (*runner.CommandResult, error) {
	res := s.Result
	res.Spec = spec
	res.Stdout = slices.Clone(res.Stdout)
	res.Stderr = slices.Clone(res.Stderr)
	if s.StdoutFile != "" {
		data, err := os.ReadFile(s.StdoutFile)
		if err != nil {
			err = fmt.Errorf("runnertest: load StdoutFile for %s: %w", spec.Name, err)
			f.t.Errorf("%v", err)
			return nil, err
		}
		res.Stdout = data
	}
	return &res, nil
}

// signalStarted closes the script's Started channel exactly once.
func signalStarted(s *scriptState) {
	if s.Started == nil {
		return
	}
	s.startOnce.Do(func() { close(s.Started) })
}

// notFoundError mirrors the real LookPath error chain.
func notFoundError(name string) error {
	return fmt.Errorf("runnertest: %s: %w", name, exec.ErrNotFound)
}

// timeoutError mirrors the real runner's timeout error contract:
// errors.Is on both runner.ErrTimeout and context.DeadlineExceeded.
func timeoutError(name string) error {
	return fmt.Errorf("runnertest: %s timed out: %w",
		name, errors.Join(runner.ErrTimeout, context.DeadlineExceeded))
}

// canceledError mirrors the real runner's parent-cancel error contract: the
// returned error wraps cause (the ctx.Err() observed at cancellation), so
// errors.Is(err, context.Canceled) holds for a canceled parent ctx.
func canceledError(name string, cause error) error {
	return fmt.Errorf("runnertest: %s canceled: %w", name, cause)
}

// FromFixture loads a canned CommandResult from testdata: either a plain
// stdout-only fixture dir/name.txt, or the triplet convention
// dir/name.stdout, dir/name.stderr, dir/name.exit (each file optional, but
// at least one must exist; missing stderr/exit mean empty output and exit 0).
func FromFixture(dir, name string) (runner.CommandResult, error) {
	var res runner.CommandResult

	txt, err := os.ReadFile(filepath.Join(dir, name+".txt"))
	switch {
	case err == nil:
		res.Stdout = txt
		return res, nil
	case !errors.Is(err, fs.ErrNotExist):
		return res, fmt.Errorf("runnertest: fixture %s/%s: %w", dir, name, err)
	}

	found := false
	read := func(ext string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(dir, name+ext))
		switch {
		case err == nil:
			found = true
			return data, nil
		case errors.Is(err, fs.ErrNotExist):
			return nil, nil
		default:
			return nil, fmt.Errorf("runnertest: fixture %s/%s%s: %w", dir, name, ext, err)
		}
	}
	if res.Stdout, err = read(".stdout"); err != nil {
		return res, err
	}
	if res.Stderr, err = read(".stderr"); err != nil {
		return res, err
	}
	exitData, err := read(".exit")
	if err != nil {
		return res, err
	}
	if exitData != nil {
		code, err := strconv.Atoi(strings.TrimSpace(string(exitData)))
		if err != nil {
			return res, fmt.Errorf("runnertest: fixture %s/%s.exit: %w", dir, name, err)
		}
		res.ExitCode = code
	}
	if !found {
		return res, fmt.Errorf("runnertest: no fixture named %s in %s (want %s.txt or %s.{stdout,stderr,exit})",
			name, dir, name, name)
	}
	return res, nil
}
