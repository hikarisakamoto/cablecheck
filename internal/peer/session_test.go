package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

func TestSessionCallbacks(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg1.Stdin = strings.NewReader("")

	var handshakeID string
	type transition struct{ from, to State }
	var transitions []transition
	cfg1.OnHandshake = func(testID string) { handshakeID = testID }
	cfg1.OnState = func(from, to State) {
		transitions = append(transitions, transition{from: from, to: to})
	}

	ctx := t.Context()
	resCh := startSession(ctx, cfg1, func(context.Context, RemoteCaller) error { return nil }, nil)
	h := startWorkerHarness(t, ctx, tr, cfg2)
	h.await(protocol.TypeAbort)
	out := awaitResult(t, resCh)
	if !errors.Is(out.err, ErrLocalAbort) {
		t.Errorf("Run error = %v, want ErrLocalAbort", out.err)
	}
	if handshakeID != h.hs.TestID || handshakeID != out.out.TestID {
		t.Errorf("OnHandshake testID = %q, harness/outcome = %q/%q", handshakeID, h.hs.TestID, out.out.TestID)
	}
	want := []transition{
		{StateInitializing, StatePreflight},
		{StatePreflight, StateListening},
		{StateListening, StateHandshake},
		{StateHandshake, StateWaitingForLocalStart},
		{StateWaitingForLocalStart, StateAborted},
	}
	if len(transitions) != len(want) {
		t.Fatalf("OnState transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Errorf("OnState transition %d = %+v, want %+v", i, transitions[i], want[i])
		}
	}
}

// TestStdinPromptCommands walks the interactive prompt: "start" sends ready
// and transitions, a duplicate "start" is idempotent with an already-ready
// message, "status" prints the local state plus the peer's heartbeat state,
// garbage gets one help line, "quit" runs the abort flow, and EOF means quit.
func TestStdinPromptCommands(t *testing.T) {
	t.Run("command-walk", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		stdout := &syncBuffer{}
		cfg1.Stdout = stdout
		cfg1.Stdin = testutil.ScriptStdin(t, "start", "START ", "blorp", "status", "quit")

		ctx := t.Context()
		resCh := startSession(ctx, cfg1, func(ctx context.Context, rc RemoteCaller) error { return nil }, nil)
		h := startWorkerHarness(t, ctx, tr, cfg2)

		// "start" → exactly one ready frame; the duplicate is answered
		// locally, never re-sent.
		h.await(protocol.TypeReady)
		// "quit" → farewell abort, then the conn closes.
		ab := h.await(protocol.TypeAbort)
		abort, err := protocol.DecodePayload[protocol.Abort](ab)
		if err != nil {
			t.Fatalf("decode abort: %v", err)
		}
		if abort.Reason != "user_interrupt" || abort.Initiator != "pc1" {
			t.Errorf("abort = %+v, want reason user_interrupt initiator pc1", abort)
		}
		for _, typ := range h.remainingTypes() {
			if typ == protocol.TypeReady {
				t.Error("duplicate start sent a second ready frame; want idempotence")
			}
		}

		out := awaitResult(t, resCh)
		if !errors.Is(out.err, ErrLocalAbort) {
			t.Errorf("Run error = %v, want ErrLocalAbort", out.err)
		}
		if out.out.FinalState != StateAborted || out.out.AbortReason != "user_interrupt" {
			t.Errorf("outcome = %s/%q, want aborted/user_interrupt", out.out.FinalState, out.out.AbortReason)
		}

		got := stdout.String()
		containsAll(t, got,
			"already ready",
			"commands:",
			"state: "+string(StateWaitingForPeerStart),
			"peer:",
		)
		// The prompt answers arrive in command order.
		iReady := strings.Index(got, "already ready")
		iHelp := strings.Index(got, "commands:")
		iStatus := strings.Index(got, "state: ")
		if !(iReady < iHelp && iHelp < iStatus) {
			t.Errorf("prompt output out of order (already=%d help=%d status=%d):\n%s", iReady, iHelp, iStatus, got)
		}
	})

	t.Run("eof-means-quit", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		cfg1.Stdin = strings.NewReader("") // immediate EOF, e.g. `< /dev/null`

		ctx := t.Context()
		resCh := startSession(ctx, cfg1, func(ctx context.Context, rc RemoteCaller) error { return nil }, nil)
		h := startWorkerHarness(t, ctx, tr, cfg2)

		h.await(protocol.TypeAbort)
		out := awaitResult(t, resCh)
		if !errors.Is(out.err, ErrLocalAbort) {
			t.Errorf("Run error = %v, want ErrLocalAbort", out.err)
		}
		if out.out.FinalState != StateAborted {
			t.Errorf("FinalState = %s, want aborted", out.out.FinalState)
		}
	})

	t.Run("status-format", func(t *testing.T) {
		got := formatStatus(StateTesting, &protocol.Heartbeat{Seq: 7, State: "testing", ActiveOp: "iperf3_client_run"}, 2*time.Second)
		containsAll(t, got, "state: testing", "peer: testing", "#7", "iperf3_client_run", "2s")
		if none := formatStatus(StateReady, nil, 0); !strings.Contains(none, "no heartbeat") {
			t.Errorf("formatStatus without heartbeat = %q, want a no-heartbeat note", none)
		}
	})
}

// TestSynchronizedStartCountdown pins the countdown anchor: frame ARRIVAL
// time + startInMs — never the absolute startAt (which is deliberately bogus
// here) — with the 3-2-1 ticks scheduled off Clock.After one second apart.
func TestSynchronizedStartCountdown(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg2.NonInteractive = true
	stdout := &syncBuffer{}
	cfg2.Stdout = stdout
	fc := clockAsFake(t, cfg2.Clock)
	states := newStateRecorder(&cfg2)

	ctx := t.Context()
	resCh := startSession(ctx, cfg2, nil, opFunc(nopOp))
	h := startCoordinatorHarness(t, ctx, tr, cfg1)

	h.await(protocol.TypeReady)
	h.send(protocol.TypeReady, "", protocol.Ready{})
	// startAt lies one hour out; only startInMs may drive the countdown.
	h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{
		StartAt:   fc.Now().Add(time.Hour),
		StartInMs: 3500,
		Mode:      "standard",
	})

	fc.BlockUntilWaiters(2) // heartbeat ticker + armed first tick
	if s := stdout.String(); strings.Contains(s, "3") {
		t.Errorf("countdown output before any advance: %q", s)
	}
	steps := []struct {
		advance time.Duration
		want    string
		notYet  string
	}{
		{500 * time.Millisecond, "3", "2"},
		{time.Second, "2", "1"},
		{time.Second, "1", "GO"},
	}
	for _, st := range steps {
		fc.Advance(st.advance)
		fc.BlockUntilWaiters(2) // next tick armed ⇒ this tick's print flushed
		got := stdout.String()
		if !strings.Contains(got, st.want) {
			t.Errorf("after +%v: output %q missing %q", st.advance, got, st.want)
		}
		if strings.Contains(got, st.notYet) {
			t.Errorf("after +%v: output %q already contains %q", st.advance, got, st.notYet)
		}
	}
	fc.Advance(time.Second) // GO at arrival + 3.5s of fake time
	states.wait(t, StateTesting)
	containsAll(t, stdout.String(), "GO")

	h.send(protocol.TypeAbort, "", protocol.Abort{Reason: "user_interrupt", Stage: "testing", Initiator: "pc1"})
	out := awaitResult(t, resCh)
	if !errors.Is(out.err, ErrPeerAborted) {
		t.Errorf("Run error = %v, want ErrPeerAborted", out.err)
	}
	if out.out.FinalState != StateAborted || out.out.AbortReason != "user_interrupt" {
		t.Errorf("outcome = %s/%q, want aborted/user_interrupt", out.out.FinalState, out.out.AbortReason)
	}
}

// TestUnknownMessageTolerated sends a bogus message type and a duplicate
// message ID mid-session: both are logged at warn and dropped, and the
// session keeps operating (it still reaches testing afterwards).
func TestUnknownMessageTolerated(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg2.NonInteractive = true
	logger, logs := newTestLogger()
	cfg2.Logger = logger
	states := newStateRecorder(&cfg2)

	ctx := t.Context()
	resCh := startSession(ctx, cfg2, nil, opFunc(nopOp))
	h := startCoordinatorHarness(t, ctx, tr, cfg1)

	h.await(protocol.TypeReady)
	// Bogus type, then a frame reusing an already-seen message ID.
	bogusID := h.send(protocol.MessageType("gizmo_upgrade"), "", nil)
	dup := mustEnvelope(t, protocol.TypeHeartbeat, h.hs.TestID, bogusID, protocol.Heartbeat{Seq: 99})
	if err := h.conn.WriteEnvelope(dup); err != nil {
		t.Fatalf("send duplicate-ID frame: %v", err)
	}

	// The session must still work: readiness completes and testing begins.
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{StartInMs: 0})
	states.wait(t, StateTesting)

	h.send(protocol.TypeAbort, "", protocol.Abort{Reason: "user_interrupt", Stage: "testing", Initiator: "pc1"})
	out := awaitResult(t, resCh)
	if !errors.Is(out.err, ErrPeerAborted) {
		t.Errorf("Run error = %v, want ErrPeerAborted", out.err)
	}
	containsAll(t, logs.String(), "unknown message type", "gizmo_upgrade", "duplicate")
}

// TestInvalidStateStrikes pins the invalid_state warning policy of
// docs/design/proto.md §4: every state-inappropriate frame is answered with
// exactly one warning{code:"invalid_state"} and the session keeps working,
// until the third strike aborts it with protocol_error.
func TestInvalidStateStrikes(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg1.NonInteractive = true
	logger, logs := newTestLogger()
	cfg1.Logger = logger

	// The plan idles until teardown so the coordinator stays in testing.
	plan := func(ctx context.Context, rc RemoteCaller) error {
		<-ctx.Done()
		return ctx.Err()
	}

	ctx := t.Context()
	resCh := startSession(ctx, cfg1, plan, nil)
	h := startWorkerHarness(t, ctx, tr, cfg2)
	h.await(protocol.TypeReady)

	// Strike 1: a post-handshake hello is invalid in every session state.
	h.send(protocol.TypeHello, "", protocol.Hello{Role: "pc2", Token: testToken})
	w1 := decodeWarning(t, h.await(protocol.TypeWarning))
	if w1.Code != "invalid_state" || w1.Stage != string(StateWaitingForPeerStart) {
		t.Errorf("strike-1 warning = %+v, want code invalid_state in waiting_for_peer_start", w1)
	}

	// The session survives strike 1: the readiness exchange still completes
	// (a duplicate warning would surface in the start_confirmation await).
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.await(protocol.TypeStartConfirm)
	advanceThroughCountdown(t, clockAsFake(t, cfg1.Clock))

	// Strike 2: a test_request sent TO the coordinator (wrong direction).
	h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "sneaky", TimeoutMs: 1000})
	w2 := decodeWarning(t, h.await(protocol.TypeWarning))
	if w2.Code != "invalid_state" || w2.Stage != string(StateTesting) {
		t.Errorf("strike-2 warning = %+v, want code invalid_state in testing", w2)
	}

	// Strike 3: one more invalid frame draws the warning and then the abort.
	h.send(protocol.TypeHello, "", protocol.Hello{Role: "pc2", Token: testToken})
	if w3 := decodeWarning(t, h.await(protocol.TypeWarning)); w3.Code != "invalid_state" {
		t.Errorf("strike-3 warning = %+v, want code invalid_state", w3)
	}
	ab := h.await(protocol.TypeAbort)
	abort, err := protocol.DecodePayload[protocol.Abort](ab)
	if err != nil {
		t.Fatalf("decode abort: %v", err)
	}
	if abort.Reason != "protocol_error" || abort.Stage != string(StateTesting) || abort.Initiator != "pc1" {
		t.Errorf("abort = %+v, want protocol_error at testing from pc1", abort)
	}
	if rest := h.remainingTypes(); len(rest) != 0 {
		t.Errorf("frames after the strike-3 abort: %v, want the conn closed", rest)
	}

	out := awaitResult(t, resCh)
	if !errors.Is(out.err, ErrPeerAborted) {
		t.Errorf("Run error = %v, want ErrPeerAborted", out.err)
	}
	if out.out.FinalState != StateAborted || out.out.AbortReason != "protocol_error" {
		t.Errorf("outcome = %s/%q, want aborted/protocol_error", out.out.FinalState, out.out.AbortReason)
	}
	containsAll(t, logs.String(), "frame invalid in current state")
}

// TestMintOrderMatchesWireOrder pins the write-path invariant behind the
// message-ID ordering fix: the receive path drops any frame whose per-role
// sequence is non-increasing, so an ID minted first MUST reach the wire
// first. Two goroutines race through the session's send helper with an
// injected stall between mint and write on the first frame; without one lock
// held across mint+write the second send overtakes the first on the wire —
// the peer then silently drops the overtaken frame (e.g. a test_request
// beaten by a heartbeat tick, expiring the Call into request_timeout).
func TestMintOrderMatchesWireOrder(t *testing.T) {
	testutil.LeakCheck(t)
	client, server := net.Pipe()
	defer server.Close()

	s := newSession(Config{Role: RolePC1}, nil, nil)
	s.ids = protocol.NewMessageIDMaker("pc1")
	s.testID = "ct-mint-order"
	s.sm = NewStateMachine(StateTesting, nil)
	s.conn = protocol.NewConn(client)
	defer s.conn.Close()
	ctx, cancel := context.WithCancel(t.Context())
	s.ctx, s.cancel = ctx, cancel
	defer cancel()

	// The far end drains frames in wire order (net.Pipe is synchronous, so
	// wire order is exactly read order).
	type recv struct {
		env *protocol.Envelope
		err error
	}
	rconn := protocol.NewConn(server)
	got := make(chan recv, 4)
	go func() {
		for {
			env, err := rconn.ReadEnvelope()
			got <- recv{env: env, err: err}
			if err != nil {
				return
			}
		}
	}()

	// The first sender stalls between mint and write until the racing send
	// finished — or until the fallback timeout, which is the only exit once
	// the write lock keeps the racer from even minting during the stall.
	entered := make(chan struct{})
	raced := make(chan struct{})
	var writes atomic.Int32
	s.cfg.hooks.beforeWrite = func(*protocol.Envelope) {
		if writes.Add(1) != 1 {
			return
		}
		close(entered)
		select {
		case <-raced:
		case <-time.After(200 * time.Millisecond):
		}
	}

	errs := make(chan error, 2)
	go func() {
		_, err := s.send(protocol.TypeHeartbeat, "", protocol.Heartbeat{Seq: 1})
		errs <- err
	}()
	testutil.WaitFor(t, entered, "first send never reached the write path")
	go func() {
		_, err := s.send(protocol.TypeHeartbeat, "", protocol.Heartbeat{Seq: 2})
		errs <- err
		close(raced)
	}()

	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("send: %v", err)
			}
		case <-time.After(testutil.TestTimeout(t)):
			t.Fatal("timed out waiting for the racing sends")
		}
	}

	var seqs []uint64
	for range 2 {
		select {
		case r := <-got:
			if r.err != nil {
				t.Fatalf("read frame: %v", r.err)
			}
			seqs = append(seqs, messageSeq(t, r.env.MessageID))
		case <-time.After(testutil.TestTimeout(t)):
			t.Fatal("timed out waiting for the frames")
		}
	}
	if seqs[0] >= seqs[1] {
		t.Errorf("wire order = seq %d before seq %d; a receiver drops the overtaken frame as non-increasing", seqs[0], seqs[1])
	}
}

// messageSeq extracts the numeric sequence from a "<role>-<seq>" message ID.
func messageSeq(t *testing.T, id string) uint64 {
	t.Helper()
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		t.Fatalf("message ID %q has no sequence", id)
	}
	n, err := strconv.ParseUint(id[i+1:], 10, 64)
	if err != nil {
		t.Fatalf("message ID %q: parse sequence: %v", id, err)
	}
	return n
}

// decodeWarning decodes a warning frame's payload.
func decodeWarning(t *testing.T, env *protocol.Envelope) *protocol.Warning {
	t.Helper()
	w, err := protocol.DecodePayload[protocol.Warning](env)
	if err != nil {
		t.Fatalf("decode warning: %v", err)
	}
	return w
}

// nopOp is an OpHandler body that succeeds immediately.
func nopOp(ctx context.Context, op string, params json.RawMessage, progress func(protocol.TestProgress)) (any, string, error) {
	return nil, "ok", nil
}
