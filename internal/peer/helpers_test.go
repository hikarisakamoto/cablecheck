package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// syncBuffer is a mutex-guarded bytes.Buffer: the session event loop writes
// prompt output concurrently with test assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the accumulated output.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestLogger returns a text slog.Logger capturing into the returned
// syncBuffer, for asserting on warn lines after the session finished.
func newTestLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// sessionResult carries a Run outcome across goroutines.
type sessionResult struct {
	out Outcome
	err error
}

// startSession launches Run in its own goroutine and returns the channel the
// result arrives on.
func startSession(ctx context.Context, cfg Config, plan PlanFunc, ops OpHandler) <-chan sessionResult {
	ch := make(chan sessionResult, 1)
	go func() {
		out, err := Run(ctx, cfg, plan, ops)
		ch <- sessionResult{out: out, err: err}
	}()
	return ch
}

// awaitResult receives the session result, bounded by the test deadline.
func awaitResult(t *testing.T, ch <-chan sessionResult) sessionResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(testutil.TestTimeout(t)):
		t.Fatalf("timed out waiting for session Run to return")
		return sessionResult{}
	}
}

// stateRecorder wires a Config test hook that records every entered state.
type stateRecorder struct {
	ch chan State
}

// newStateRecorder returns a recorder and installs it on cfg.
func newStateRecorder(cfg *Config) *stateRecorder {
	r := &stateRecorder{ch: make(chan State, 64)}
	cfg.hooks.onState = func(_, to State) {
		select {
		case r.ch <- to:
		default:
		}
	}
	return r
}

// wait blocks until the session enters want, bounded by the test deadline.
func (r *stateRecorder) wait(t *testing.T, want State) {
	t.Helper()
	deadline := time.After(testutil.TestTimeout(t))
	for {
		select {
		case s := <-r.ch:
			if s == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for state %s", want)
			return
		}
	}
}

// harness drives the far end of a session under test: the real handshake
// half, then raw frame scripting over the protocol connection. A drain
// goroutine keeps reading so session writes never block on the unbuffered
// pipe.
type harness struct {
	t      *testing.T
	conn   *protocol.Conn
	hs     *handshakeResult
	frames chan *protocol.Envelope
}

// startDrain launches the frame drain; the channel closes when the
// connection dies.
func (h *harness) startDrain() {
	h.frames = make(chan *protocol.Envelope, 128)
	go func() {
		for {
			env, err := h.conn.ReadEnvelope()
			if err != nil {
				close(h.frames)
				return
			}
			h.frames <- env
		}
	}()
}

// startWorkerHarness dials through tr and runs the worker handshake half,
// posing as PC2 opposite a coordinator session under test. It blocks until
// the session side completes the handshake.
func startWorkerHarness(t *testing.T, ctx context.Context, tr *pipeTransport, cfg Config) *harness {
	t.Helper()
	nc, err := tr.Dial(ctx, "192.168.1.1:8443")
	if err != nil {
		t.Fatalf("harness dial: %v", err)
	}
	conn := protocol.NewConn(nc)
	hs, err := workerHandshake(conn, cfg)
	if err != nil {
		t.Fatalf("harness worker handshake: %v", err)
	}
	h := &harness{t: t, conn: conn, hs: hs}
	t.Cleanup(func() { conn.Close() })
	h.startDrain()
	return h
}

// startCoordinatorHarness accepts through tr and runs the coordinator
// handshake half, posing as PC1 opposite a worker session under test.
func startCoordinatorHarness(t *testing.T, ctx context.Context, tr *pipeTransport, cfg Config) *harness {
	t.Helper()
	ln, err := tr.Listen(ctx, "192.168.1.1:8443")
	if err != nil {
		t.Fatalf("harness listen: %v", err)
	}
	nc, err := ln.Accept()
	if err != nil {
		t.Fatalf("harness accept: %v", err)
	}
	conn := protocol.NewConn(nc)
	hs, err := coordinatorHandshake(conn, cfg)
	if err != nil {
		t.Fatalf("harness coordinator handshake: %v", err)
	}
	h := &harness{t: t, conn: conn, hs: hs}
	t.Cleanup(func() { conn.Close() })
	h.startDrain()
	return h
}

// send writes one frame from the harness peer, continuing the handshake's
// message-ID sequence, and returns its message ID.
func (h *harness) send(typ protocol.MessageType, inReplyTo string, payload any) string {
	h.t.Helper()
	msgID := h.hs.IDs.Next()
	env, err := protocol.NewEnvelope(typ, h.hs.TestID, msgID, payload)
	if err != nil {
		h.t.Errorf("harness build %s: %v", typ, err)
		return msgID
	}
	env.InReplyTo = inReplyTo
	if err := h.conn.WriteEnvelope(env); err != nil {
		h.t.Errorf("harness send %s: %v", typ, err)
	}
	return msgID
}

// await returns the next non-heartbeat frame, requiring it to be of type
// typ; any other type is a test failure. Bounded by the test deadline.
func (h *harness) await(typ protocol.MessageType) *protocol.Envelope {
	h.t.Helper()
	deadline := time.After(testutil.TestTimeout(h.t))
	for {
		select {
		case env, ok := <-h.frames:
			if !ok {
				h.t.Fatalf("connection closed while awaiting %s", typ)
				return nil
			}
			if env.Type == protocol.TypeHeartbeat {
				continue
			}
			if env.Type != typ {
				h.t.Fatalf("awaiting %s, got %s", typ, env.Type)
				return nil
			}
			return env
		case <-deadline:
			h.t.Fatalf("timed out awaiting %s", typ)
			return nil
		}
	}
}

// remainingTypes waits for the connection to die and returns the types of
// all frames that arrived after the last await.
func (h *harness) remainingTypes() []protocol.MessageType {
	h.t.Helper()
	var types []protocol.MessageType
	deadline := time.After(testutil.TestTimeout(h.t))
	for {
		select {
		case env, ok := <-h.frames:
			if !ok {
				return types
			}
			types = append(types, env.Type)
		case <-deadline:
			h.t.Fatalf("timed out waiting for the harness connection to close")
			return types
		}
	}
}

// opFunc adapts a function to OpHandler.
type opFunc func(ctx context.Context, op string, params json.RawMessage, progress func(protocol.TestProgress)) (any, string, error)

// HandleOp implements OpHandler.
func (f opFunc) HandleOp(ctx context.Context, op string, params json.RawMessage, progress func(protocol.TestProgress)) (any, string, error) {
	return f(ctx, op, params, progress)
}

// advanceThroughCountdown walks a FakeClock-driven session through the full
// 3.5s synchronized-start countdown (ticks at 500ms, then 1s apart). The
// session under test keeps exactly two clock waiters live during the
// countdown — the heartbeat ticker and the armed countdown timer — so each
// BlockUntilWaiters(2) parks until the loop has re-armed the next tick.
func advanceThroughCountdown(t *testing.T, fc *clocktest.FakeClock) {
	t.Helper()
	for _, d := range []time.Duration{500 * time.Millisecond, time.Second, time.Second, time.Second} {
		fc.BlockUntilWaiters(2)
		fc.Advance(d)
	}
}

// containsAll asserts got contains every want substring.
func containsAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q; got:\n%s", w, got)
		}
	}
}
