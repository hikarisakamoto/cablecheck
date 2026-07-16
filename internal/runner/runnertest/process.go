package runnertest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	"cablecheck/internal/runner"
)

// FakeProcess is the runner.Process double handed out by FakeRunner.Start.
// Its Stdout streams the scripted output while the process is "running"
// (then blocks until exit, then EOF), and its lifecycle is fully observable:
// TermCh/KillCh close on the first Terminate/Kill, Done closes on exit.
//
// Default behavior: Wait blocks until the test calls Exit, until Terminate
// or Kill synthesize an ExitCode -1 signal-death result, until the script's
// Delay channel closes (auto-exit with the scripted result), until the Start
// ctx is canceled (synthesized SIGTERM death whose Wait error wraps
// ctx.Err(), mirroring the real runner's parent-cancel path), or until the
// Wait ctx is done.
type FakeProcess struct {
	pid     int
	spec    runner.CommandSpec
	stdout  io.Reader
	content []byte // scripted stdout bytes, echoed into synthesized results

	exited   chan struct{}
	exitOnce sync.Once
	termCh   chan struct{}
	termOnce sync.Once
	killCh   chan struct{}
	killOnce sync.Once
	aborted  <-chan struct{}

	mu         sync.Mutex
	result     *runner.CommandResult
	waitErr    error
	terminated bool
	killed     bool
}

var _ runner.Process = (*FakeProcess)(nil)

// newFakeProcess builds a process whose live stdout is scripted.Stdout.
func newFakeProcess(pid int, spec runner.CommandSpec, scripted runner.CommandResult, aborted <-chan struct{}) *FakeProcess {
	p := &FakeProcess{
		pid:     pid,
		spec:    spec,
		content: slices.Clone(scripted.Stdout),
		exited:  make(chan struct{}),
		termCh:  make(chan struct{}),
		killCh:  make(chan struct{}),
		aborted: aborted,
	}
	p.stdout = io.MultiReader(
		bytes.NewReader(p.content),
		&tailBlockReader{exited: p.exited, aborted: aborted},
	)
	return p
}

// PID returns the scripted process id.
func (p *FakeProcess) PID() int { return p.pid }

// Stdout returns the live scripted stream: the scripted bytes are readable
// immediately (readiness scanning), then reads block until the process
// exits, then io.EOF. All callers share one reader.
func (p *FakeProcess) Stdout() io.Reader { return p.stdout }

// Wait blocks until the process exits or ctx is done. It is idempotent and
// safe from multiple goroutines.
func (p *FakeProcess) Wait(ctx context.Context) (*runner.CommandResult, error) {
	select {
	case <-p.exited:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.result, p.waitErr
	case <-ctx.Done():
		return nil, fmt.Errorf("runnertest: wait for %s: %w", p.spec.Name, ctx.Err())
	case <-p.aborted:
		return nil, fmt.Errorf("runnertest: FakeRunner shut down before %s exited", p.spec.Name)
	}
}

// Exit makes Wait return res (with the spec filled in). Only the first exit
// — scripted, test-driven, or signal-synthesized — takes effect.
func (p *FakeProcess) Exit(res runner.CommandResult) {
	p.exit(res, nil)
}

// exit is the single exit point: it installs res and waitErr exactly once.
// A TimedOut result with a nil waitErr gets the contract timeout error.
func (p *FakeProcess) exit(res runner.CommandResult, waitErr error) {
	p.exitOnce.Do(func() {
		res.Spec = p.spec
		p.mu.Lock()
		p.result = &res
		p.waitErr = waitErr
		if res.TimedOut && waitErr == nil {
			p.waitErr = timeoutError(p.spec.Name)
		}
		p.mu.Unlock()
		close(p.exited)
	})
}

// dieBySignal synthesizes a signal death (ExitCode -1, carrying the scripted
// stdout) with waitErr as the Wait error: nil for consumer-initiated
// Terminate/Kill (signal death is data), a ctx-wrapping error for the
// Start-ctx cancellation path.
func (p *FakeProcess) dieBySignal(sig string, waitErr error) {
	p.exit(runner.CommandResult{
		Stdout:   slices.Clone(p.content),
		ExitCode: -1,
		Signal:   sig,
	}, waitErr)
}

// Terminate observably closes TermCh and, if the process is still running,
// synthesizes a SIGTERM death (ExitCode -1) carrying the scripted stdout.
func (p *FakeProcess) Terminate() error {
	p.mu.Lock()
	p.terminated = true
	p.mu.Unlock()
	p.termOnce.Do(func() { close(p.termCh) })
	p.dieBySignal("SIGTERM", nil)
	return nil
}

// Kill observably closes KillCh and, if the process is still running,
// synthesizes a SIGKILL death (ExitCode -1) carrying the scripted stdout.
func (p *FakeProcess) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.killOnce.Do(func() { close(p.killCh) })
	p.dieBySignal("SIGKILL", nil)
	return nil
}

// Done is closed once the process has exited.
func (p *FakeProcess) Done() <-chan struct{} { return p.exited }

// Terminated reports whether Terminate has been called.
func (p *FakeProcess) Terminated() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminated
}

// Killed reports whether Kill has been called.
func (p *FakeProcess) Killed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// TermCh is closed on the first Terminate call — a no-sleep sync point for
// tests asserting graceful-shutdown behavior.
func (p *FakeProcess) TermCh() <-chan struct{} { return p.termCh }

// KillCh is closed on the first Kill call.
func (p *FakeProcess) KillCh() <-chan struct{} { return p.killCh }

// tailBlockReader keeps a live stdout stream open past the scripted content:
// reads block until the process exits (or the FakeRunner shuts down), then
// return io.EOF.
type tailBlockReader struct {
	exited  <-chan struct{}
	aborted <-chan struct{}
}

func (r *tailBlockReader) Read([]byte) (int, error) {
	select {
	case <-r.exited:
	case <-r.aborted:
	}
	return 0, io.EOF
}
