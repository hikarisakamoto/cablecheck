// Package app orchestrates a cablecheck run: preflight, report directory and
// debug logging, the peer session with the quick test plan, report assembly,
// evaluation and rendering, and the mapping of every outcome onto the 0-7
// exit-code contract.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/config"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner"
)

// BuildInfo carries the ldflags-injected build identity from main through
// cli into the app and the report.
type BuildInfo struct {
	// Version is the cablecheck version, e.g. "1.0.0" ("dev" for local builds).
	Version string
	// Commit is the short git commit hash.
	Commit string
	// Date is the UTC build timestamp.
	Date string
}

// Deps bundles everything an App needs from the outside world; every field
// is injectable so the whole run is testable in-process. Zero fields get
// production defaults in New.
type Deps struct {
	// Runner executes external tools; nil means the real exec runner.
	Runner runner.Runner
	// Clock supplies time; nil means the real clock.
	Clock clock.Clock
	// Stdin feeds the interactive start prompt; nil disables it.
	Stdin io.Reader
	// Stdout receives operator-facing output (token banner, countdown,
	// progress, summary); nil discards.
	Stdout io.Writer
	// Stderr receives logging; nil discards.
	Stderr io.Writer
	// StateDir overrides the pidfile state tree used for the stale-process
	// scan; empty means the production default (and enables the real
	// pidfile registry).
	StateDir string
	// Build identifies this build; flows into the report and the handshake.
	Build BuildInfo
	// OnStep, when set, observes every plan step as (step, total, name);
	// the cli wires progress rendering here.
	OnStep func(step, total int, name string)

	// hooks are test-only fault-injection and observation points; the zero
	// value installs none (docs/design/testing.md §3).
	hooks testHooks
}

// testHooks is the unexported integration-test seam.
type testHooks struct {
	// onState observes every peer session state transition.
	onState func(peer.State)
	// mangleReportChunk corrupts outbound report chunks (wired when the
	// report transfer lands; stored now so the seam is stable).
	mangleReportChunk func([]byte) []byte
}

// App is one prepared cablecheck run. Construct with New, then Start (which
// on PC1 binds the control listener before returning), then Wait.
type App struct {
	cfg  *config.RunConfig
	deps Deps

	// ln is PC1's pre-bound control listener (nil on PC2 or before Start).
	ln net.Listener
	// controlPort is the effective control port: the actually bound port
	// on PC1 (supports --control-port 0), the configured one on PC2.
	controlPort uint16

	// sysfsRoot overrides /sys/class/net for interface classification in
	// tests; empty means the real tree.
	sysfsRoot string
	// heartbeatInterval/idleTimeout override the session's protocol timers
	// in tests; zero means the protocol defaults.
	heartbeatInterval, idleTimeout time.Duration

	started bool
	done    chan struct{}
	code    ExitCode
	err     error

	// watch captures data the peer session exposes through its logger.
	watch sessionWatch
}

// New validates cfg and assembles an App with production defaults for every
// unset dependency. It performs no I/O.
func New(cfg *config.RunConfig, deps Deps) (*App, error) {
	if cfg == nil {
		return nil, errors.New("app: nil config")
	}
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.Runner == nil {
		deps.Runner = runner.New(deps.Clock)
	}
	if deps.Stdout == nil {
		deps.Stdout = io.Discard
	}
	if deps.Stderr == nil {
		deps.Stderr = io.Discard
	}
	return &App{
		cfg:         cfg,
		deps:        deps,
		controlPort: cfg.ControlPort,
		done:        make(chan struct{}),
	}, nil
}

// Start begins the run. On PC1 it binds the control listener — including
// --control-port 0, whose real port ControlAddr then reports — before
// returning; every later phase (preflight, session, reporting) runs in a
// background goroutine joined by Wait.
func (a *App) Start(ctx context.Context) error {
	if a.started {
		return errors.New("app: Start called twice")
	}
	a.started = true
	if a.cfg.Role == config.RolePC1 {
		addr := net.JoinHostPort(a.cfg.LocalIP.Unmap().String(), strconv.Itoa(int(a.cfg.ControlPort)))
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			// Record the failure and release Wait: a caller that waits
			// after a failed Start gets the same outcome back instead of
			// blocking on a channel nothing will ever close.
			ee := &ExitError{Code: ExitConfig, Err: fmt.Errorf("app: bind control port %s: %w", addr, err)}
			a.code, a.err = ExitConfig, ee
			close(a.done)
			return ee
		}
		a.ln = ln
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			a.controlPort = uint16(ta.Port)
		}
	}
	go func() {
		defer close(a.done)
		a.code, a.err = a.run(ctx)
	}()
	return nil
}

// ControlAddr returns PC1's bound control listener address (real port even
// for --control-port 0); nil on PC2 or before Start.
func (a *App) ControlAddr() net.Addr {
	if a.ln == nil {
		return nil
	}
	return a.ln.Addr()
}

// Wait blocks until the run finished and returns its exit code and error,
// mirroring os/exec.Cmd.Wait: after a failed Start it returns the Start
// failure instead of deadlocking, and calling it before Start is an
// ExitInternal error. Like Start, it must not be called concurrently with
// Start itself.
func (a *App) Wait() (ExitCode, error) {
	if !a.started {
		return ExitInternal, errors.New("app: Wait called before Start")
	}
	<-a.done
	return a.code, a.err
}

// preboundListener hands the pre-bound PC1 listener to the peer session's
// Transport seam so the session accepts on the very socket Start bound.
type preboundListener struct {
	mu sync.Mutex
	ln net.Listener
}

// Listen implements peer.Transport for the coordinator: it returns the
// pre-bound listener (once), wrapped so accepted connections get keepalive.
func (t *preboundListener) Listen(context.Context, string) (net.Listener, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln == nil {
		return nil, errors.New("app: control listener already consumed")
	}
	ln := t.ln
	t.ln = nil
	return keepAliveListener{ln}, nil
}

// Dial implements peer.Transport; the coordinator never dials.
func (t *preboundListener) Dial(context.Context, string) (net.Conn, error) {
	return nil, errors.New("app: coordinator transport cannot dial")
}

// keepAliveListener applies the protocol keepalive policy to accepted
// connections, mirroring the peer package's own TCP transport.
type keepAliveListener struct{ net.Listener }

// Accept configures keepalive on each accepted connection; a connection that
// cannot be configured is closed and the error returned (listener survives).
func (l keepAliveListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if kerr := protocol.ConfigureKeepAlive(nc); kerr != nil {
		nc.Close()
		return nil, fmt.Errorf("app: configure keepalive: %w", kerr)
	}
	return nc, nil
}

// sessionWatch collects facts the peer session publishes through structured
// log records — the session's logger is its sanctioned observation seam for
// the app layer (peer/session.go logs "state" on every transition and
// handshake.go logs "handshake complete" with the assigned testId; those
// record shapes are load-bearing here). TestRunQuickHappyPath pins both
// captures end-to-end: the testId feeding partial reports and the worker
// summary, and the state stream feeding the hooks.onState sync primitive
// (docs/design/testing.md §3).
type sessionWatch struct {
	mu      sync.Mutex
	testID  string
	onState func(peer.State)
}

// TestID returns the captured session test ID, "" before the handshake.
func (w *sessionWatch) TestID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.testID
}

// setTestID stores the captured test ID.
func (w *sessionWatch) setTestID(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.testID = id
}

// watchHandler wraps the session logger's handler and feeds sessionWatch.
type watchHandler struct {
	slog.Handler
	w *sessionWatch
}

// Handle inspects the record before delegating: "state" records drive the
// onState test hook, "handshake complete" records carry the testId.
func (h watchHandler) Handle(ctx context.Context, r slog.Record) error {
	switch r.Message {
	case "state":
		if h.w.onState != nil {
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "to" {
					h.w.onState(peer.State(a.Value.String()))
					return false
				}
				return true
			})
		}
	case "handshake complete":
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "testId" {
				h.w.setTestID(a.Value.String())
				return false
			}
			return true
		})
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs keeps the watcher wrapped around the derived handler.
func (h watchHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return watchHandler{Handler: h.Handler.WithAttrs(attrs), w: h.w}
}

// WithGroup keeps the watcher wrapped around the derived handler.
func (h watchHandler) WithGroup(name string) slog.Handler {
	return watchHandler{Handler: h.Handler.WithGroup(name), w: h.w}
}
