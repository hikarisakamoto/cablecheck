package peer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// TestWorkerCancelOp pins the op == "cancel" special case of test_request
// handling on the worker: with no active op the cancel is still answered
// status ok; with an active op the cancel gets its own ok reply first, the
// cancelled op then emits its failed test_result for the ORIGINAL request,
// and the single-op slot is free again for the next request.
func TestWorkerCancelOp(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg2.NonInteractive = true
	states := newStateRecorder(&cfg2)

	opStarted := make(chan struct{})
	handler := opFunc(func(ctx context.Context, op string, params json.RawMessage, progress func(protocol.TestProgress)) (any, string, error) {
		if op == "instant" {
			return map[string]int{"n": 1}, "ok", nil
		}
		close(opStarted)
		<-ctx.Done()
		return nil, "failed", ctx.Err()
	})

	ctx := t.Context()
	resCh := startSession(ctx, cfg2, nil, handler)
	h := startCoordinatorHarness(t, ctx, tr, cfg1)

	h.await(protocol.TypeReady)
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{StartInMs: 0})
	states.wait(t, StateTesting)

	// Cancel with no active op: an immediate ok, nothing else.
	idle := h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "cancel"})
	idleRes := awaitTestResult(t, h, idle)
	if idleRes.Status != "ok" {
		t.Errorf("idle cancel status = %q, want ok", idleRes.Status)
	}

	// Cancel with an active op: the ok reply for the cancel request comes
	// first, then the cancelled op fails its own original request.
	first := h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "block", TimeoutMs: 3_600_000})
	testutil.WaitFor(t, opStarted, "blocking op never started")
	cancelID := h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "cancel"})
	if okRes := awaitTestResult(t, h, cancelID); okRes.Status != "ok" {
		t.Errorf("cancel status = %q, want ok", okRes.Status)
	}
	failRes := awaitTestResult(t, h, first)
	if failRes.Status != "failed" || !strings.Contains(failRes.Error, context.Canceled.Error()) {
		t.Errorf("cancelled op result = %+v, want status failed with a cancellation error", failRes)
	}

	// The op slot is released: a fresh request executes normally.
	next := h.send(protocol.TypeTestRequest, "", protocol.TestRequest{Op: "instant", TimeoutMs: 1000})
	nextRes := awaitTestResult(t, h, next)
	if nextRes.Status != "ok" || string(nextRes.Result) != `{"n":1}` {
		t.Errorf("post-cancel op result = %+v, want ok/{\"n\":1}", nextRes)
	}

	h.send(protocol.TypeAbort, "", protocol.Abort{Reason: "user_interrupt", Stage: "testing", Initiator: "pc1"})
	out := awaitResult(t, resCh)
	if !errors.Is(out.err, ErrPeerAborted) {
		t.Errorf("Run error = %v, want ErrPeerAborted", out.err)
	}
	if out.out.FinalState != StateAborted {
		t.Errorf("FinalState = %s, want aborted", out.out.FinalState)
	}
}

// awaitTestResult awaits the next test_result frame, requires it to answer
// wantReplyTo, and returns its decoded payload.
func awaitTestResult(t *testing.T, h *harness, wantReplyTo string) *protocol.TestResult {
	t.Helper()
	env := h.await(protocol.TypeTestResult)
	if env.InReplyTo != wantReplyTo {
		t.Errorf("test_result InReplyTo = %q, want %q", env.InReplyTo, wantReplyTo)
	}
	res, err := protocol.DecodePayload[protocol.TestResult](env)
	if err != nil {
		t.Fatalf("decode test_result: %v", err)
	}
	return res
}
