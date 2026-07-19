package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// newHeartbeatSession builds the minimal session a bare heartbeatLoop needs,
// plus the far end of its pipe and a synchronization channel fed by the
// afterHeartbeatTick test hook (one value per evaluated tick, true when a
// heartbeat frame was written).
func newHeartbeatSession(t *testing.T, fc *clocktest.FakeClock) (*session, *protocol.Conn, chan bool) {
	t.Helper()
	a, b := net.Pipe()
	ticks := make(chan bool, 16)
	cfg := Config{Role: RolePC1, Clock: fc}
	cfg.hooks.afterHeartbeatTick = func(sent bool) { ticks <- sent }
	s := newSession(cfg, nil, nil)
	s.conn = protocol.NewConn(a)
	s.ids = protocol.NewMessageIDMaker("pc1")
	s.testID = "ct-heartbeat-test"
	s.sm = NewStateMachine(StateTesting, nil)
	ctx, cancel := context.WithCancel(t.Context())
	s.ctx, s.cancel = ctx, cancel
	far := protocol.NewConn(b)
	t.Cleanup(func() {
		cancel()
		s.wg.Wait()
		s.conn.Close()
		far.Close()
	})
	return s, far, ticks
}

// awaitTick receives one afterHeartbeatTick observation.
func awaitTick(t *testing.T, ticks chan bool) bool {
	t.Helper()
	select {
	case sent := <-ticks:
		return sent
	case <-time.After(testutil.TestTimeout(t)):
		t.Fatalf("timed out waiting for a heartbeat tick evaluation")
		return false
	}
}

// TestHeartbeatScheduling drives the heartbeater with a fake ticker and pins
// the send condition: a heartbeat is written only when the session's last
// send is at least 4s (80% of the 5s interval) old; recent traffic suppresses
// the tick's send.
func TestHeartbeatScheduling(t *testing.T) {
	testutil.LeakCheck(t)
	fc := clocktest.New(time.Date(2026, 7, 18, 1, 0, 0, 0, time.UTC))
	s, far, ticks := newHeartbeatSession(t, fc)

	// A frame is written now: the session's last-send timestamp is t0.
	warn, err := protocol.NewEnvelope(protocol.TypeWarning, s.testID, s.ids.Next(), protocol.Warning{Code: "traffic"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- s.write(warn) }()
	if env, rerr := far.ReadEnvelope(); rerr != nil || env.Type != protocol.TypeWarning {
		t.Fatalf("far read = (%v, %v), want the traffic frame", env, rerr)
	}
	if werr := <-writeDone; werr != nil {
		t.Fatalf("session write: %v", werr)
	}

	s.wg.Add(1)
	go s.heartbeatLoop()

	// Tick at t0+5s: last send 5s old >= 4s → heartbeat sent.
	fc.BlockUntilWaiters(1)
	readDone := make(chan *protocol.Envelope, 1)
	go func() {
		env, rerr := far.ReadEnvelope()
		if rerr != nil {
			readDone <- nil
			return
		}
		readDone <- env
	}()
	fc.Advance(5 * time.Second)
	if sent := awaitTick(t, ticks); !sent {
		t.Fatal("tick at t0+5s: heartbeat not sent, want sent (last send 5s old)")
	}
	env := <-readDone
	if env == nil || env.Type != protocol.TypeHeartbeat {
		t.Fatalf("far read = %v, want a heartbeat frame", env)
	}
	hb, err := protocol.DecodePayload[protocol.Heartbeat](env)
	if err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if hb.Seq != 1 || hb.State != string(StateTesting) {
		t.Errorf("heartbeat = %+v, want seq 1 state testing", hb)
	}

	// Fresh traffic at t0+8s, then tick at t0+10s: last send only 2s old →
	// the tick must NOT write a heartbeat.
	fc.Advance(3 * time.Second)
	traffic2 := warn2(t, s) // built here: warn2 may Fatalf, never off the test goroutine
	go func() { writeDone <- s.write(traffic2) }()
	if env, rerr := far.ReadEnvelope(); rerr != nil || env.Type != protocol.TypeWarning {
		t.Fatalf("far read = (%v, %v), want the second traffic frame", env, rerr)
	}
	if werr := <-writeDone; werr != nil {
		t.Fatalf("session write: %v", werr)
	}
	fc.Advance(2 * time.Second)
	if sent := awaitTick(t, ticks); sent {
		t.Fatal("tick at t0+10s: heartbeat sent, want suppressed (last send 2s old)")
	}

	// Tick at t0+15s: last send 7s old → heartbeat seq 2.
	go func() {
		env, rerr := far.ReadEnvelope()
		if rerr != nil {
			readDone <- nil
			return
		}
		readDone <- env
	}()
	fc.Advance(5 * time.Second)
	if sent := awaitTick(t, ticks); !sent {
		t.Fatal("tick at t0+15s: heartbeat not sent, want sent (last send 7s old)")
	}
	env = <-readDone
	if env == nil || env.Type != protocol.TypeHeartbeat {
		t.Fatalf("far read = %v, want the second heartbeat", env)
	}
	hb, err = protocol.DecodePayload[protocol.Heartbeat](env)
	if err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if hb.Seq != 2 {
		t.Errorf("heartbeat seq = %d, want 2 (exactly two heartbeats total)", hb.Seq)
	}
}

// warn2 builds a second traffic frame with a fresh message ID.
func warn2(t *testing.T, s *session) *protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(protocol.TypeWarning, s.testID, s.ids.Next(), protocol.Warning{Code: "traffic"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	return env
}

// TestHeartbeatLossDetection runs a full worker session whose peer goes
// silent after the handshake: the per-frame idle deadline expires, the reader
// reports exactly one connection-loss event, and the session aborts once with
// reason peer_lost and exit-5 semantics — without sending any farewell abort
// to the dead peer.
func TestHeartbeatLossDetection(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg2.NonInteractive = true
	cfg2.IdleTimeout = 50 * time.Millisecond // real-time read deadline
	var connErrs atomic.Int32
	cfg2.hooks.afterEvent = func(ev event) {
		if _, ok := ev.(evConnErr); ok {
			connErrs.Add(1)
		}
	}

	ctx := t.Context()
	resCh := startSession(ctx, cfg2, nil, opFunc(func(context.Context, string, json.RawMessage, func(protocol.TestProgress)) (any, string, error) {
		return nil, "ok", nil
	}))
	h := startCoordinatorHarness(t, ctx, tr, cfg1)

	h.await(protocol.TypeReady) // worker auto-ready; harness then goes silent

	out := awaitResult(t, resCh)
	if !errors.Is(out.err, ErrPeerAborted) {
		t.Errorf("Run error = %v, want ErrPeerAborted", out.err)
	}
	if out.out.FinalState != StateAborted {
		t.Errorf("FinalState = %s, want aborted", out.out.FinalState)
	}
	if out.out.AbortReason != "peer_lost" {
		t.Errorf("AbortReason = %q, want peer_lost", out.out.AbortReason)
	}
	if got := connErrs.Load(); got != 1 {
		t.Errorf("connection-loss events handled = %d, want exactly 1", got)
	}
	// No farewell abort is sent to a peer that is already gone.
	for _, typ := range h.remainingTypes() {
		if typ == protocol.TypeAbort {
			t.Error("session sent a farewell abort on peer loss; want none")
		}
	}
}
