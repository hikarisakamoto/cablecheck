// Package runner executes external tools (ethtool, ip, ping, iperf3) with
// process-group lifecycle management, bounded output capture, and
// deterministic timeout attribution.
//
// Contract highlights:
//   - Commands never go through a shell; Name is resolved via exec.LookPath.
//   - The child environment is os.Environ() plus LC_ALL=C and LANG=C, always
//     (NLS translations break every text parser), then Spec.Env.
//   - Every child runs in its own process group (Setpgid), and all kill paths
//     signal the negative pgid so grandchildren (sudo wrappers, sh stubs) die
//     with the child.
//   - err != nil is reserved for infrastructure failures. A non-zero exit or
//     a signal death is data (ping exits 1 on any loss): err == nil and the
//     caller inspects ExitCode/Signal.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"cablecheck/internal/clock"
)

const (
	// DefaultGracePeriod is the SIGTERM-to-SIGKILL escalation delay used
	// when CommandSpec.GracePeriod is zero.
	DefaultGracePeriod = 3 * time.Second
	// DefaultMaxOutputBytes is the per-stream in-memory capture cap used
	// when CommandSpec.MaxOutputBytes is zero.
	DefaultMaxOutputBytes int64 = 4 << 20
	// waitDelaySlack is added to the grace period to form cmd.WaitDelay,
	// the pipe-unblock backstop: it un-hangs Wait when a grandchild
	// inherited the stdout pipe and survived (WaitDelay's own kill only
	// reaches the direct child, so the group-kill escalation is still the
	// primary mechanism — both are required).
	waitDelaySlack = 2 * time.Second
)

// ErrTimeout marks results of commands that exceeded CommandSpec.Timeout.
// Errors returned for timeouts satisfy both errors.Is(err, ErrTimeout) and
// errors.Is(err, context.DeadlineExceeded).
var ErrTimeout = errors.New("runner: command timed out")

// CommandSpec describes one external command invocation.
type CommandSpec struct {
	// Name is the bare executable name (or an absolute path), resolved
	// via LookPath. There is never any shell interpretation.
	Name string
	// Args are the argument vector passed verbatim to the executable.
	Args []string
	// Env entries are appended to the base environment, which is
	// os.Environ() plus LC_ALL=C and LANG=C (always injected).
	Env []string
	// Stdin, when non-nil, is fed to the child's standard input.
	Stdin io.Reader
	// Timeout bounds the command's runtime; 0 means bounded only by the
	// ctx passed to Run/Start.
	Timeout time.Duration
	// GracePeriod is the SIGTERM-to-SIGKILL escalation delay on kill
	// paths; 0 means DefaultGracePeriod.
	GracePeriod time.Duration
	// MaxOutputBytes caps the per-stream in-memory capture; 0 means
	// DefaultMaxOutputBytes. Tee files always receive the full stream.
	MaxOutputBytes int64
	// TeeStdoutPath, when non-empty, names a file that receives the
	// COMPLETE stdout stream (the report raw/ dir), regardless of the
	// in-memory cap.
	TeeStdoutPath string
	// TeeStderrPath is the stderr equivalent of TeeStdoutPath.
	TeeStderrPath string
	// Label is a short slug used for logging and raw file naming, for
	// example "iperf3-tcp-fwd".
	Label string
}

// CommandResult is the outcome of one command invocation. It is populated
// even when Run returns a timeout or cancellation error, so partial output
// is never lost.
type CommandResult struct {
	// Spec is the spec that produced this result.
	Spec CommandSpec
	// Stdout holds the captured (possibly capped) standard output.
	Stdout []byte
	// Stderr holds the captured (possibly capped) standard error.
	Stderr []byte
	// StdoutTruncated reports that the in-memory cap was hit for stdout;
	// the tee file, if configured, is still complete.
	StdoutTruncated bool
	// StderrTruncated is the stderr equivalent of StdoutTruncated.
	StderrTruncated bool
	// ExitCode is the process exit status, or -1 if it died by signal.
	ExitCode int
	// Signal names the terminating signal ("SIGKILL", "SIGTERM", ...) or
	// is empty for a normal exit.
	Signal string
	// Started is the launch timestamp (from the runner's Clock).
	Started time.Time
	// Duration is the wall time from launch to reap.
	Duration time.Duration
	// TimedOut reports that CommandSpec.Timeout expired. It is set from
	// the escalation path's attribution flag, never inferred from
	// ctx.Err() alone (a parent cancellation is not a timeout).
	TimedOut bool
}

// Failed reports whether the command exited unsuccessfully (non-zero exit
// or signal death). This is data, not an error condition.
func (r *CommandResult) Failed() bool { return r.ExitCode != 0 }

// Runner executes external commands. Product code depends on this interface
// so tests can substitute runnertest.FakeRunner.
type Runner interface {
	// Run executes the command to completion. err != nil only for
	// infrastructure failures: executable not found
	// (errors.Is(err, exec.ErrNotFound)), timeout (result non-nil,
	// TimedOut=true, errors.Is(err, ErrTimeout)), or context
	// cancellation (err wraps ctx.Err(), partial output preserved).
	Run(ctx context.Context, spec CommandSpec) (*CommandResult, error)
	// Start launches a long-lived command (iperf3 -s) and returns a
	// handle for live output scanning and lifecycle control. Cancelling
	// ctx kills the process group.
	Start(ctx context.Context, spec CommandSpec) (Process, error)
	// LookPath resolves an executable name; the returned error satisfies
	// errors.Is(err, exec.ErrNotFound) when the tool is absent.
	LookPath(name string) (string, error)
}

// Process is a handle on a command launched with Start.
type Process interface {
	// PID returns the child's process id (== its process group id).
	PID() int
	// Stdout returns the live stdout stream for readiness scanning. The
	// capped copy still lands in the eventual CommandResult. The stream
	// reaches EOF once the process has been reaped.
	Stdout() io.Reader
	// Wait blocks until the process has been reaped or ctx is done. It
	// is idempotent and safe from multiple goroutines; ctx bounds only
	// the wait, never the process itself.
	Wait(ctx context.Context) (*CommandResult, error)
	// Terminate sends SIGTERM to the process group. ESRCH is ignored.
	Terminate() error
	// Kill sends SIGKILL to the process group. ESRCH is ignored.
	Kill() error
	// Done is closed once the process has been reaped and its
	// CommandResult is available.
	Done() <-chan struct{}
}

// Exec is the production Runner backed by os/exec.
type Exec struct {
	clk clock.Clock
}

var _ Runner = (*Exec)(nil)

// New returns a Runner that executes real commands, using clk for the
// Started/Duration timestamps and the kill-escalation timer.
func New(clk clock.Clock) *Exec { return &Exec{clk: clk} }

// LookPath resolves name via exec.LookPath, preserving the exec.ErrNotFound
// chain for errors.Is.
func (e *Exec) LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("runner: %w", err)
	}
	return path, nil
}

// Run executes spec to completion. See Runner.Run for the error contract.
func (e *Exec) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	p, err := e.start(ctx, spec, false)
	if err != nil {
		return nil, err
	}
	// The reap is guaranteed to finish: cancellation/timeout SIGTERMs the
	// group, the escalation timer SIGKILLs it, and WaitDelay unblocks the
	// pipes — so this wait is bounded and must not be cut short by ctx,
	// or partial output would be lost.
	<-p.done
	return p.result, p.finalErr
}

// Start launches spec and returns a live Process handle. See Runner.Start.
func (e *Exec) Start(ctx context.Context, spec CommandSpec) (Process, error) {
	return e.start(ctx, spec, true)
}

// cappedWriter stores up to max bytes in memory and discards the rest,
// recording that truncation happened. It never returns an error, so an
// io.MultiWriter sibling (the tee file) always receives the full stream.
type cappedWriter struct {
	max       int64
	buf       bytes.Buffer
	truncated bool
}

func newCappedWriter(max int64) *cappedWriter { return &cappedWriter{max: max} }

// Write implements io.Writer; it always reports len(p) consumed.
func (w *cappedWriter) Write(p []byte) (int, error) {
	remain := w.max - int64(w.buf.Len())
	switch {
	case remain >= int64(len(p)):
		w.buf.Write(p)
	case remain > 0:
		w.buf.Write(p[:remain])
		w.truncated = true
	default:
		if len(p) > 0 {
			w.truncated = true
		}
	}
	return len(p), nil
}

// finalize returns the captured bytes, appending a truncation marker line
// when the cap was hit. teePath, if non-empty, names the file holding the
// complete stream and is referenced in the marker.
func (w *cappedWriter) finalize(teePath string) []byte {
	if !w.truncated {
		return w.buf.Bytes()
	}
	where := "no tee file was configured"
	if teePath != "" {
		where = "full stream in " + teePath
	}
	fmt.Fprintf(&w.buf, "\n[cablecheck: output truncated at %d bytes; %s]\n", w.max, where)
	return w.buf.Bytes()
}

// signalName returns the conventional "SIGXXX" name for sig.
func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGALRM:
		return "SIGALRM"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGUSR1:
		return "SIGUSR1"
	case syscall.SIGUSR2:
		return "SIGUSR2"
	default:
		return fmt.Sprintf("SIG#%d", int(sig))
	}
}
