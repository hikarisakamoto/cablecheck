package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
)

// proc is the concrete Process implementation for real children.
type proc struct {
	pid  int
	spec CommandSpec
	live *streamBuffer // nil when created by Run

	// done is closed by the reaper after result/finalErr are set.
	done     chan struct{}
	result   *CommandResult
	finalErr error
}

var _ Process = (*proc)(nil)

// start prepares and launches spec. When live is true, a third stdout leg (a
// non-blocking in-memory pipe) is attached for readiness scanning via
// Process.Stdout.
func (e *Exec) start(ctx context.Context, spec CommandSpec, live bool) (*proc, error) {
	if spec.GracePeriod <= 0 {
		spec.GracePeriod = DefaultGracePeriod
	}
	if spec.MaxOutputBytes <= 0 {
		spec.MaxOutputBytes = DefaultMaxOutputBytes
	}

	path, err := e.LookPath(spec.Name)
	if err != nil {
		return nil, err
	}

	runCtx, cancelTimeout := ctx, context.CancelFunc(func() {})
	if spec.Timeout > 0 {
		runCtx, cancelTimeout = context.WithTimeout(ctx, spec.Timeout)
	}
	// On success the reaper goroutine owns cancelTimeout; every error
	// return before that must release the timeout context.
	launched := false
	defer func() {
		if !launched {
			cancelTimeout()
		}
	}()

	cmd := exec.CommandContext(runCtx, path, spec.Args...)
	// Base env: os.Environ() + LC_ALL=C + LANG=C (always; NLS breaks every
	// text parser), then per-spec entries. os/exec keeps the last
	// occurrence of duplicate keys.
	cmd.Env = append(append(os.Environ(), "LC_ALL=C", "LANG=C"), spec.Env...)
	cmd.Stdin = spec.Stdin
	// Own process group: child pgid == child pid, inherited by
	// grandchildren, so group signals reach the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Pipe-unblock backstop only; the group-kill escalation below is the
	// real kill mechanism (WaitDelay's kill hits only the direct child).
	cmd.WaitDelay = spec.GracePeriod + waitDelaySlack

	outCap := newCappedWriter(spec.MaxOutputBytes)
	errCap := newCappedWriter(spec.MaxOutputBytes)
	var tees []*os.File
	closeTees := func() {
		for _, f := range tees {
			f.Close()
		}
	}
	openTee := func(path string) (*os.File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			closeTees()
			return nil, fmt.Errorf("runner: create tee file: %w", err)
		}
		tees = append(tees, f)
		return f, nil
	}

	stdoutW := []io.Writer{outCap}
	if spec.TeeStdoutPath != "" {
		f, err := openTee(spec.TeeStdoutPath)
		if err != nil {
			return nil, err
		}
		// Tee first: cappedWriter never errors, and tee files must see
		// the complete stream.
		stdoutW = append([]io.Writer{f}, stdoutW...)
	}
	var liveBuf *streamBuffer
	if live {
		liveBuf = newStreamBuffer(spec.MaxOutputBytes)
		stdoutW = append(stdoutW, liveBuf)
	}
	cmd.Stdout = io.MultiWriter(stdoutW...)

	stderrW := []io.Writer{errCap}
	if spec.TeeStderrPath != "" {
		f, err := openTee(spec.TeeStderrPath)
		if err != nil {
			return nil, err
		}
		stderrW = append([]io.Writer{f}, stderrW...)
	}
	cmd.Stderr = io.MultiWriter(stderrW...)

	// Timeout/cancel attribution and escalation. cmd.Cancel runs when
	// runCtx fires before the process exits: it attributes the
	// cancellation (spec timeout vs parent ctx — never inferred from
	// ctx.Err() alone after the fact), SIGTERMs the group, and arms the
	// SIGKILL escalation timer.
	var timedOut, canceled atomic.Bool
	waitDone := make(chan struct{}) // closed once cmd.Wait has returned
	cmd.Cancel = func() error {
		if spec.Timeout > 0 && ctx.Err() == nil {
			timedOut.Store(true)
		} else {
			canceled.Store(true)
		}
		pgid := cmd.Process.Pid
		if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		grace := spec.GracePeriod
		afterGrace := e.clk.After(grace)
		go func() {
			select {
			case <-waitDone:
			case <-afterGrace:
				// ESRCH deliberately ignored: the group may have
				// exited between the check and the signal.
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}()
		return nil
	}

	started := e.clk.Now()
	if err := cmd.Start(); err != nil {
		closeTees()
		if liveBuf != nil {
			liveBuf.Close()
		}
		return nil, fmt.Errorf("runner: start %s: %w", spec.Name, err)
	}
	launched = true

	p := &proc{
		pid:  cmd.Process.Pid,
		spec: spec,
		live: liveBuf,
		done: make(chan struct{}),
	}

	// Reaper: waits for the child, finalizes captured output, builds the
	// CommandResult and the contract error, then closes done.
	go func() {
		waitErr := cmd.Wait()
		close(waitDone)
		cancelTimeout()
		closeTees()
		if liveBuf != nil {
			defer liveBuf.Close()
		}

		res := &CommandResult{
			Spec:            spec,
			Stdout:          outCap.finalize(spec.TeeStdoutPath),
			Stderr:          errCap.finalize(spec.TeeStderrPath),
			StdoutTruncated: outCap.truncated,
			StderrTruncated: errCap.truncated,
			ExitCode:        -1,
			Started:         started,
			Duration:        e.clk.Now().Sub(started),
			TimedOut:        timedOut.Load(),
		}
		if ps := cmd.ProcessState; ps != nil {
			res.ExitCode = ps.ExitCode()
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.Signal = signalName(ws.Signal())
			}
		}

		var exitErr *exec.ExitError
		switch {
		case timedOut.Load():
			p.finalErr = fmt.Errorf("runner: %s timed out after %s: %w",
				spec.Name, spec.Timeout, errors.Join(ErrTimeout, context.DeadlineExceeded))
		case canceled.Load():
			p.finalErr = fmt.Errorf("runner: %s canceled: %w", spec.Name, ctx.Err())
		case waitErr == nil, errors.As(waitErr, &exitErr), errors.Is(waitErr, exec.ErrWaitDelay):
			// Normal exit, non-zero exit, or signal death: data, not
			// error. ErrWaitDelay means the process exited but a
			// grandchild held a pipe past the backstop; the exit
			// status is still valid data.
			p.finalErr = nil
		default:
			p.finalErr = fmt.Errorf("runner: %s: %w", spec.Name, waitErr)
		}
		p.result = res
		close(p.done)
	}()
	return p, nil
}

// PID returns the child's pid (== its pgid, thanks to Setpgid).
func (p *proc) PID() int { return p.pid }

// Stdout returns the live stdout stream. For processes created by Run (which
// has no live leg) it returns an empty, already-closed stream. The live
// stream is bounded: it delivers at most CommandSpec.MaxOutputBytes bytes —
// past the cap, reads block until the process is reaped and then return
// io.EOF. It exists for readiness scanning, not full-stream consumption; use
// a tee file for the complete stream.
func (p *proc) Stdout() io.Reader {
	if p.live == nil {
		return eofReader{}
	}
	return p.live
}

// Wait blocks until the process is reaped or ctx is done; ctx bounds only
// the wait. It is idempotent and safe for concurrent use.
func (p *proc) Wait(ctx context.Context) (*CommandResult, error) {
	select {
	case <-p.done:
		return p.result, p.finalErr
	case <-ctx.Done():
		return nil, fmt.Errorf("runner: wait for %s: %w", p.spec.Name, ctx.Err())
	}
}

// Terminate sends SIGTERM to the process group; no-op after reap.
func (p *proc) Terminate() error { return p.signalGroup(syscall.SIGTERM) }

// Kill sends SIGKILL to the process group; no-op after reap.
func (p *proc) Kill() error { return p.signalGroup(syscall.SIGKILL) }

// Done is closed once the process has been reaped.
func (p *proc) Done() <-chan struct{} { return p.done }

// signalGroup delivers sig to the negative pgid, ignoring ESRCH and
// suppressing the signal entirely once the process has been reaped (the pgid
// could have been reused by then).
func (p *proc) signalGroup(sig syscall.Signal) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := syscall.Kill(-p.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("runner: signal %s to pgid %d: %w", signalName(sig), p.pid, err)
	}
	return nil
}

// eofReader is an always-empty stream.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// streamBuffer is a non-blocking, bounded in-memory pipe: writes always
// succeed immediately (so the exec copy goroutine can never stall on a slow
// or absent reader), reads block until data arrives or the buffer is closed.
//
// Only the first max bytes of the stream are retained; everything after the
// cap is silently dropped. The live leg exists for readiness scanning — once
// the scanner stops reading, an unbounded buffer would accumulate the entire
// output of a long-lived child until reap, which is exactly what the
// per-stream cap prevents. The capped CommandResult copy and the (complete)
// tee file remain the durable records.
type streamBuffer struct {
	max     int64 // total-stream cap; bytes past it are dropped
	written int64 // bytes accepted so far (read or not)

	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newStreamBuffer(max int64) *streamBuffer {
	s := &streamBuffer{max: max}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Write appends p up to the total-stream cap; it never blocks and never
// fails, and always reports len(p) consumed so io.MultiWriter siblings keep
// receiving the full stream. Writes after Close are silently discarded.
func (s *streamBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		if keep := min(s.max-s.written, int64(len(p))); keep > 0 {
			s.buf = append(s.buf, p[:keep]...)
			s.written += keep
			s.cond.Broadcast()
		}
	}
	return len(p), nil
}

// Read blocks until data is available or the buffer is closed; after Close
// it drains the remaining data and then returns io.EOF.
func (s *streamBuffer) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) == 0 && !s.closed {
		s.cond.Wait()
	}
	if len(s.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// Close marks the stream ended and wakes blocked readers. It is idempotent.
func (s *streamBuffer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.cond.Broadcast()
}
