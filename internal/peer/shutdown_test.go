package peer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// TestShutdownOrder cancels a live session and pins the ordered teardown:
// the active operation's context is cancelled, the farewell abort is written
// (bounded by the 2s farewell deadline) BEFORE the connection closes, the
// reader unblocks, the WaitGroup drains, and no goroutine leaks.
func TestShutdownOrder(t *testing.T) {
	t.Run("coordinator-mid-call", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		cfg1.NonInteractive = true
		fc := clockAsFake(t, cfg1.Clock)

		callErr := make(chan error, 1)
		plan := func(ctx context.Context, rc RemoteCaller) error {
			_, err := rc.Call(ctx, "long_op", nil, time.Hour, nil)
			callErr <- err
			return err
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		resCh := startSession(ctx, cfg1, plan, nil)
		h := startWorkerHarness(t, ctx, tr, cfg2)

		h.await(protocol.TypeReady)
		h.send(protocol.TypeReady, "", protocol.Ready{})
		h.await(protocol.TypeStartConfirm)
		advanceThroughCountdown(t, fc)
		h.await(protocol.TypeTestRequest) // the Call is now in flight

		cancel()

		// Farewell abort arrives before the connection closes: await sees
		// the abort frame, then the drain channel closes with nothing else.
		ab := h.await(protocol.TypeAbort)
		abort, err := protocol.DecodePayload[protocol.Abort](ab)
		if err != nil {
			t.Fatalf("decode abort: %v", err)
		}
		if abort.Reason != "user_interrupt" || abort.Initiator != "pc1" {
			t.Errorf("abort = %+v, want reason user_interrupt initiator pc1", abort)
		}
		if rest := h.remainingTypes(); len(rest) != 0 {
			t.Errorf("frames after the farewell abort: %v, want the conn closed", rest)
		}

		select {
		case err := <-callErr:
			if !errors.Is(err, ErrSessionClosed) && !errors.Is(err, context.Canceled) {
				t.Errorf("mid-flight Call error = %v, want cancellation", err)
			}
		case <-time.After(testutil.TestTimeout(t)):
			t.Fatal("mid-flight Call never unblocked")
		}
		out := awaitResult(t, resCh)
		if !errors.Is(out.err, ErrLocalAbort) || !errors.Is(out.err, context.Canceled) {
			t.Errorf("Run error = %v, want ErrLocalAbort wrapping context.Canceled", out.err)
		}
		if out.out.FinalState != StateAborted || out.out.AbortReason != "user_interrupt" {
			t.Errorf("outcome = %s/%q, want aborted/user_interrupt", out.out.FinalState, out.out.AbortReason)
		}
	})

	t.Run("worker-active-op", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		cfg2.NonInteractive = true
		states := newStateRecorder(&cfg2)

		opStarted := make(chan struct{})
		opCtxErr := make(chan error, 1)
		handler := opFunc(func(ctx context.Context, op string, params json.RawMessage, progress func(protocol.TestProgress)) (any, string, error) {
			close(opStarted)
			<-ctx.Done()
			opCtxErr <- ctx.Err()
			return nil, "failed", ctx.Err()
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		resCh := startSession(ctx, cfg2, nil, handler)
		h := startCoordinatorHarness(t, ctx, tr, cfg1)

		h.await(protocol.TypeReady)
		h.send(protocol.TypeReady, "", protocol.Ready{})
		h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{StartInMs: 0})
		states.wait(t, StateTesting)
		h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "block", TimeoutMs: 3_600_000})
		testutil.WaitFor(t, opStarted, "op never started")

		cancel()

		select {
		case err := <-opCtxErr:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("op ctx error = %v, want context.Canceled", err)
			}
		case <-time.After(testutil.TestTimeout(t)):
			t.Fatal("active op ctx never cancelled")
		}
		ab := h.await(protocol.TypeAbort)
		abort, err := protocol.DecodePayload[protocol.Abort](ab)
		if err != nil {
			t.Fatalf("decode abort: %v", err)
		}
		if abort.Reason != "user_interrupt" || abort.Stage != "block" || abort.Initiator != "pc2" {
			t.Errorf("abort = %+v, want user_interrupt at stage block from pc2", abort)
		}
		out := awaitResult(t, resCh)
		if !errors.Is(out.err, ErrLocalAbort) {
			t.Errorf("Run error = %v, want ErrLocalAbort", out.err)
		}
		if out.out.FinalState != StateAborted {
			t.Errorf("FinalState = %s, want aborted", out.out.FinalState)
		}
	})
}

// TestWorkerRejectsOverlappingOps sends a second test_request while one op is
// still executing: the worker answers it immediately with
// test_result{status:"rejected"} and the original op completes untouched.
// It also pins the 1/s progress throttle: two back-to-back progress calls
// yield a single test_progress frame.
func TestWorkerRejectsOverlappingOps(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg2.NonInteractive = true
	cfg2.Complete = func() protocol.Complete { return protocol.Complete{Classification: "GOOD", ExitCode: 0} }
	states := newStateRecorder(&cfg2)

	opStarted := make(chan struct{})
	release := make(chan struct{})
	handler := opFunc(func(ctx context.Context, op string, params json.RawMessage, progress func(protocol.TestProgress)) (any, string, error) {
		progress(protocol.TestProgress{Stage: "p1", Percent: 10})
		progress(protocol.TestProgress{Stage: "p2", Percent: 20}) // throttled away (<1s apart)
		close(opStarted)
		select {
		case <-release:
			return map[string]int{"ok": 1}, "ok", nil
		case <-ctx.Done():
			return nil, "failed", ctx.Err()
		}
	})

	ctx := t.Context()
	resCh := startSession(ctx, cfg2, nil, handler)
	h := startCoordinatorHarness(t, ctx, tr, cfg1)

	h.await(protocol.TypeReady)
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{StartInMs: 0})
	states.wait(t, StateTesting)

	first := h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "block", TimeoutMs: 3_600_000})
	testutil.WaitFor(t, opStarted, "first op never started")
	prog := h.await(protocol.TypeTestProgress)
	if prog.InReplyTo != first {
		t.Errorf("test_progress InReplyTo = %q, want %q", prog.InReplyTo, first)
	}

	second := h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "sneaky", TimeoutMs: 1000})
	rej := h.await(protocol.TypeTestResult)
	if rej.InReplyTo != second {
		t.Errorf("rejection InReplyTo = %q, want the second request %q", rej.InReplyTo, second)
	}
	rejRes, err := protocol.DecodePayload[protocol.TestResult](rej)
	if err != nil {
		t.Fatalf("decode rejection: %v", err)
	}
	if rejRes.Status != "rejected" {
		t.Errorf("second request status = %q, want rejected", rejRes.Status)
	}

	close(release)
	done := h.await(protocol.TypeTestResult)
	if done.InReplyTo != first {
		t.Errorf("result InReplyTo = %q, want the first request %q", done.InReplyTo, first)
	}
	doneRes, err := protocol.DecodePayload[protocol.TestResult](done)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if doneRes.Status != "ok" || string(doneRes.Result) != `{"ok":1}` {
		t.Errorf("first op result = %+v, want ok/{\"ok\":1}", doneRes)
	}

	h.send(protocol.TypeComplete, "", protocol.Complete{Classification: "WARNING", ExitCode: 1})
	comp := h.await(protocol.TypeComplete)
	reply, err := protocol.DecodePayload[protocol.Complete](comp)
	if err != nil {
		t.Fatalf("decode complete reply: %v", err)
	}
	if reply.Classification != "GOOD" {
		t.Errorf("complete reply = %+v, want the Config.Complete verdict", reply)
	}

	out := awaitResult(t, resCh)
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.out.FinalState != StateCompleted {
		t.Errorf("FinalState = %s, want completed", out.out.FinalState)
	}
	if out.out.PeerComplete == nil || out.out.PeerComplete.Classification != "WARNING" {
		t.Errorf("PeerComplete = %+v, want the coordinator's WARNING verdict", out.out.PeerComplete)
	}
}
