package peer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// TestReportTransferSeam pins the report-transfer message routing that this
// package wires now (the actual transfer logic arrives later): a worker
// without a ReceiveReports hook declines the manifest, and a coordinator's
// SendReports callback can send report frames and receive the routed acks.
func TestReportTransferSeam(t *testing.T) {
	t.Run("worker-declines-without-receiver", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		cfg2.NonInteractive = true
		states := newStateRecorder(&cfg2)

		ctx := t.Context()
		resCh := startSession(ctx, cfg2, nil, opFunc(nopOp))
		h := startCoordinatorHarness(t, ctx, tr, cfg1)

		h.await(protocol.TypeReady)
		h.send(protocol.TypeReady, "", protocol.Ready{})
		h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{StartInMs: 0})
		states.wait(t, StateTesting)

		h.send(protocol.TypeReport, "", protocol.ReportManifest{
			Files:     []protocol.ReportFile{{Name: "report.json", Size: 3, SHA256: "abc"}},
			TotalSize: 3,
		})
		ackEnv := h.await(protocol.TypeReportAck)
		ack, err := protocol.DecodePayload[protocol.ReportAck](ackEnv)
		if err != nil {
			t.Fatalf("decode report_ack: %v", err)
		}
		if !ack.Declined || ack.OK {
			t.Errorf("report_ack = %+v, want a manifest-level decline", ack)
		}

		h.send(protocol.TypeComplete, "", protocol.Complete{Classification: "GOOD"})
		h.await(protocol.TypeComplete)
		out := awaitResult(t, resCh)
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.out.FinalState != StateCompleted {
			t.Errorf("FinalState = %s, want completed", out.out.FinalState)
		}
	})

	t.Run("coordinator-send-seam", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		cfg1.NonInteractive = true
		cfg2.Caps.AcceptReportTransfer = true
		fc := clockAsFake(t, cfg1.Clock)

		gotAck := make(chan protocol.ReportAck, 1)
		cfg1.SendReports = func(ctx context.Context, rt *ReportChannel) error {
			if rt.TestID() == "" {
				t.Error("ReportChannel.TestID is empty")
			}
			if err := rt.Send(protocol.TypeReport, "", protocol.ReportManifest{TotalSize: 0}); err != nil {
				return err
			}
			env, err := rt.Receive(ctx)
			if err != nil {
				return err
			}
			ack, err := protocol.DecodePayload[protocol.ReportAck](env)
			if err != nil {
				return err
			}
			gotAck <- *ack
			return nil
		}

		ctx := t.Context()
		resCh := startSession(ctx, cfg1, func(ctx context.Context, rc RemoteCaller) error { return nil }, nil)
		h := startWorkerHarness(t, ctx, tr, cfg2)

		h.await(protocol.TypeReady)
		h.send(protocol.TypeReady, "", protocol.Ready{})
		h.await(protocol.TypeStartConfirm)
		advanceThroughCountdown(t, fc)

		h.await(protocol.TypeReport)
		h.send(protocol.TypeReportAck, "", protocol.ReportAck{Declined: true, Error: "not yet implemented"})
		comp := h.await(protocol.TypeComplete)
		h.send(protocol.TypeComplete, comp.MessageID, protocol.Complete{Classification: "GOOD"})

		out := awaitResult(t, resCh)
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.out.FinalState != StateCompleted {
			t.Errorf("FinalState = %s, want completed", out.out.FinalState)
		}
		select {
		case ack := <-gotAck:
			if !ack.Declined {
				t.Errorf("routed ack = %+v, want the declined ack", ack)
			}
		default:
			t.Error("SendReports never received the routed report_ack")
		}
	})
}

// TestDeclinedManifestChunksDoNotAbort pins T1c: when a worker declines a
// report manifest (no ReceiveReports hook / NoReportTransfer), the sender still
// streams the whole first file before reading the decline ack, so several
// report_chunk frames arrive AFTER the decline. Those late frames must take the
// late-frame drop path — not the invalid-state strike counter — or the third
// one would abort the whole session with protocol_error (PC1 would exit 5
// instead of its health code). The decline branch must therefore mark the
// transfer as having run. Here the harness delivers more chunks than the strike
// budget tolerates; the session must still complete normally.
func TestDeclinedManifestChunksDoNotAbort(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg2.NonInteractive = true
	// No ReceiveReports hook: the worker declines the manifest.
	logger, logs := newTestLogger()
	cfg2.Logger = logger
	states := newStateRecorder(&cfg2)

	ctx := t.Context()
	resCh := startSession(ctx, cfg2, nil, opFunc(nopOp))
	h := startCoordinatorHarness(t, ctx, tr, cfg1)

	h.await(protocol.TypeReady)
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{StartInMs: 0})
	states.wait(t, StateTesting)

	// Manifest is declined (no receiver configured).
	h.send(protocol.TypeReport, "", protocol.ReportManifest{
		Files:     []protocol.ReportFile{{Name: "report.json", Size: 3, SHA256: "abc"}},
		TotalSize: 3,
	})
	ackEnv := h.await(protocol.TypeReportAck)
	ack, err := protocol.DecodePayload[protocol.ReportAck](ackEnv)
	if err != nil {
		t.Fatalf("decode report_ack: %v", err)
	}
	if !ack.Declined {
		t.Fatalf("report_ack = %+v, want a manifest-level decline", ack)
	}

	// The sender streamed the whole first file before reading the decline:
	// more straggler chunks than the strike budget tolerates now arrive.
	for seq := range maxInvalidState + 1 {
		h.send(protocol.TypeReportChunk, "", protocol.ReportChunk{Name: "report.json", Seq: seq})
	}

	// The session must still be alive and finish the complete exchange.
	h.send(protocol.TypeComplete, "", protocol.Complete{Classification: "GOOD"})
	h.await(protocol.TypeComplete)
	out := awaitResult(t, resCh)
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.out.FinalState != StateCompleted {
		t.Errorf("FinalState = %s, want completed", out.out.FinalState)
	}
	for _, typ := range h.remainingTypes() {
		if typ == protocol.TypeAbort {
			t.Errorf("declined-manifest chunks drew an abort; want them dropped quietly")
		}
	}
	containsAll(t, logs.String(), "late report-transfer frame dropped")
	if got := logs.String(); strings.Contains(got, "invalid_state") {
		t.Errorf("declined-manifest chunks counted as invalid-state strikes; logs:\n%s", got)
	}
}

// TestTransferRouteFullBufferDoesNotBlock pins the guard against a wedged
// event loop: when the active transfer callback stops draining (e.g. it
// failed mid-file while the peer keeps streaming chunks), routing one more
// frame into the full buffer must return promptly — the frame is dropped with
// a warning — so the loop stays able to process the callback's
// evTransferDone. A blocking send here would deadlock both sides until
// operator interrupt (heartbeats keep either idle timeout from firing).
func TestTransferRouteFullBufferDoesNotBlock(t *testing.T) {
	testutil.LeakCheck(t)
	log, logBuf := newTestLogger()
	s := newSession(Config{Role: RolePC2, Logger: log}, nil, nil)
	s.ids = protocol.NewMessageIDMaker("pc1")
	s.testID = "ct-transfer-route-test"
	s.sm = NewStateMachine(StateGeneratingReport, nil)
	ctx, cancel := context.WithCancel(t.Context())
	s.ctx, s.cancel = ctx, cancel
	defer cancel()

	s.transferOn = true
	s.transfer = make(chan *protocol.Envelope, transferBufferSize)
	chunk := func(seq int) *protocol.Envelope {
		env, err := protocol.NewEnvelope(protocol.TypeReportChunk, s.testID, s.ids.Next(), protocol.ReportChunk{
			Name: "report.json", Seq: seq,
		})
		if err != nil {
			t.Fatalf("NewEnvelope: %v", err)
		}
		return env
	}
	for i := range transferBufferSize {
		s.routeTransferFrame(chunk(i))
	}
	if got := len(s.transfer); got != transferBufferSize {
		t.Fatalf("routed frames buffered = %d, want %d (buffer full)", got, transferBufferSize)
	}

	// Buffer full, nobody draining: one more routed frame must not block the
	// caller (which in production is the event loop itself).
	overflow := chunk(transferBufferSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.routeTransferFrame(overflow)
	}()
	select {
	case <-done:
	case <-time.After(testutil.TestTimeout(t)):
		t.Fatal("routeTransferFrame blocked on a full transfer buffer")
	}
	if got := len(s.transfer); got != transferBufferSize {
		t.Errorf("routed frames buffered = %d, want still %d (overflow dropped)", got, transferBufferSize)
	}
	containsAll(t, logBuf.String(), "report-transfer buffer full")
}

// TestTransferStragglersAfterCallbackDoNotAbort pins the warnings-only
// contract for report transfer (events.go evTransferDone): frames still in
// flight when the transfer callback ends must be dropped quietly, NOT fed to
// the invalid-state strike counter — acking is per-file, so a receiver that
// stops mid-file easily has more than maxInvalidState chunks in flight, and
// striking them would escalate a warning-grade transfer failure into a
// protocol_error abort on both sides. After a completed transfer the peer
// streams four straggler chunks; the session must still complete normally.
func TestTransferStragglersAfterCallbackDoNotAbort(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg2.NonInteractive = true
	logger, logs := newTestLogger()
	cfg2.Logger = logger
	states := newStateRecorder(&cfg2)

	// The receiver consumes the manifest, acks it and returns: the transfer
	// is over from the callback's point of view.
	cfg2.ReceiveReports = func(ctx context.Context, rt *ReportChannel) error {
		env, err := rt.Receive(ctx)
		if err != nil {
			return err
		}
		return rt.Send(protocol.TypeReportAck, env.MessageID, protocol.ReportAck{OK: true})
	}
	// The stragglers must arrive after the loop processed evTransferDone,
	// or they would still be routed to the (already gone) callback.
	transferDone := make(chan struct{})
	cfg2.hooks.afterEvent = func(ev event) {
		if _, ok := ev.(evTransferDone); ok {
			close(transferDone)
		}
	}

	ctx := t.Context()
	resCh := startSession(ctx, cfg2, nil, opFunc(nopOp))
	h := startCoordinatorHarness(t, ctx, tr, cfg1)

	h.await(protocol.TypeReady)
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{StartInMs: 0})
	states.wait(t, StateTesting)

	h.send(protocol.TypeReport, "", protocol.ReportManifest{
		Files:     []protocol.ReportFile{{Name: "report.json", Size: 3, SHA256: "abc"}},
		TotalSize: 3,
	})
	h.await(protocol.TypeReportAck)
	testutil.WaitFor(t, transferDone, "transfer callback outcome never reached the loop")

	// More straggler chunks than the strike budget tolerates.
	for seq := range maxInvalidState + 1 {
		h.send(protocol.TypeReportChunk, "", protocol.ReportChunk{Name: "report.json", Seq: seq})
	}

	// The session must still be alive and finish the complete exchange.
	h.send(protocol.TypeComplete, "", protocol.Complete{Classification: "GOOD"})
	h.await(protocol.TypeComplete)
	out := awaitResult(t, resCh)
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.out.FinalState != StateCompleted {
		t.Errorf("FinalState = %s, want completed", out.out.FinalState)
	}
	for _, typ := range h.remainingTypes() {
		if typ == protocol.TypeWarning || typ == protocol.TypeAbort {
			t.Errorf("straggler chunks drew a %s frame; want them dropped quietly", typ)
		}
	}
	containsAll(t, logs.String(), "late report-transfer frame dropped")
	if got := logs.String(); strings.Contains(got, "invalid_state") {
		t.Errorf("straggler chunks counted as invalid-state strikes; logs:\n%s", got)
	}
}

// TestCoordinatorAuthThreeStrikes lets a wrong-token worker try three times:
// the coordinator keeps listening after the first two rejections and gives up
// as a session failure on the third.
func TestCoordinatorAuthThreeStrikes(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, _ := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport = tr
	cfg1.NonInteractive = true

	ctx := t.Context()
	resCh := startSession(ctx, cfg1, func(ctx context.Context, rc RemoteCaller) error { return nil }, nil)

	for i := range 3 {
		nc, err := tr.Dial(ctx, "192.168.1.1:8443")
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn := protocol.NewConn(nc)
		hello := mustEnvelope(t, protocol.TypeHello, "", "pc2-00000001", protocol.Hello{
			Token: "wrong-token", Role: "pc2", LocalIP: "192.168.1.2", PeerIP: "192.168.1.1",
		})
		if err := conn.WriteEnvelope(hello); err != nil {
			t.Fatalf("attempt %d: write hello: %v", i, err)
		}
		reply, err := conn.ReadEnvelope()
		if err != nil {
			t.Fatalf("attempt %d: read reply: %v", i, err)
		}
		if reply.Type != protocol.TypeAbort {
			t.Fatalf("attempt %d: reply = %s, want abort", i, reply.Type)
		}
		ab, err := protocol.DecodePayload[protocol.Abort](reply)
		if err != nil {
			t.Fatalf("attempt %d: decode abort: %v", i, err)
		}
		if ab.Reason != abortAuthFailed || ab.Detail != "" {
			t.Errorf("attempt %d: abort = %+v, want bare auth_failed", i, ab)
		}
		conn.Close()
	}

	out := awaitResult(t, resCh)
	if !errors.Is(out.err, ErrHandshakeFailed) || !errors.Is(out.err, errAuthFailed) {
		t.Errorf("Run error = %v, want ErrHandshakeFailed wrapping errAuthFailed", out.err)
	}
	if out.out.FinalState != StateFailed {
		t.Errorf("FinalState = %s, want failed", out.out.FinalState)
	}
}
