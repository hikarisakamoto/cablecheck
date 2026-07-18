package peer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// TestRPCCorrelation drives the coordinator RPC path end to end: a Call turns
// into a test_request, progress frames are routed to onProgress in order, the
// test_result matching InReplyTo resolves the Call, a late result after the
// call finished is logged and dropped without derailing the session, and a
// call that never completes expires into abort(request_timeout) + session
// failure.
func TestRPCCorrelation(t *testing.T) {
	t.Run("progress-and-result-then-late-duplicate", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		cfg1.NonInteractive = true
		logger, logs := newTestLogger()
		cfg1.Logger = logger

		progCh := make(chan protocol.TestProgress, 8)
		callDone := make(chan struct{})
		gate := make(chan struct{})
		var res *protocol.TestResult
		var callErr error
		plan := func(ctx context.Context, rc RemoteCaller) error {
			res, callErr = rc.Call(ctx, "ping_run", map[string]int{"count": 3}, 2*time.Second,
				func(p protocol.TestProgress) {
					select {
					case progCh <- p:
					default:
					}
				})
			close(callDone)
			<-gate
			return nil
		}

		ctx := t.Context()
		resCh := startSession(ctx, cfg1, plan, nil)
		h := startWorkerHarness(t, ctx, tr, cfg2)

		h.await(protocol.TypeReady)
		h.send(protocol.TypeReady, "", protocol.Ready{})
		h.await(protocol.TypeStartConfirm)
		advanceThroughCountdown(t, clockAsFake(t, cfg1.Clock))

		req := h.await(protocol.TypeTestRequest)
		treq, err := protocol.DecodePayload[protocol.TestRequest](req)
		if err != nil {
			t.Fatalf("decode test_request: %v", err)
		}
		if treq.Op != "ping_run" || treq.TimeoutMs != 2000 {
			t.Errorf("test_request = op %q timeoutMs %d, want ping_run/2000", treq.Op, treq.TimeoutMs)
		}
		var params map[string]int
		if err := json.Unmarshal(treq.Params, &params); err != nil || params["count"] != 3 {
			t.Errorf("test_request params = %s (%v), want {\"count\":3}", treq.Params, err)
		}

		h.send(protocol.TypeTestProgress, req.MessageID, protocol.TestProgress{Stage: "warmup", Percent: 10})
		h.send(protocol.TypeTestProgress, req.MessageID, protocol.TestProgress{Stage: "running", Percent: 50})
		h.send(protocol.TypeTestResult, req.MessageID, protocol.TestResult{
			Status: "ok", Result: json.RawMessage(`{"x":1}`),
		})
		testutil.WaitFor(t, callDone, "Call never resolved")
		if callErr != nil {
			t.Fatalf("Call: %v", callErr)
		}
		if res == nil || res.Status != "ok" || string(res.Result) != `{"x":1}` {
			t.Errorf("Call result = %+v, want status ok result {\"x\":1}", res)
		}
		for i, want := range []string{"warmup", "running"} {
			select {
			case p := <-progCh:
				if p.Stage != want {
					t.Errorf("progress[%d].Stage = %q, want %q", i, p.Stage, want)
				}
			default:
				t.Errorf("progress[%d] (%s) never routed to onProgress", i, want)
			}
		}

		// A late/duplicate result for the already-resolved call must be
		// dropped without panic; the session keeps working afterwards.
		h.send(protocol.TypeTestResult, req.MessageID, protocol.TestResult{Status: "ok"})
		close(gate) // plan finishes → complete exchange
		comp := h.await(protocol.TypeComplete)
		h.send(protocol.TypeComplete, comp.MessageID, protocol.Complete{Classification: "GOOD", ExitCode: 0})

		out := awaitResult(t, resCh)
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.out.FinalState != StateCompleted {
			t.Errorf("FinalState = %s, want completed", out.out.FinalState)
		}
		if out.out.PeerComplete == nil || out.out.PeerComplete.Classification != "GOOD" {
			t.Errorf("PeerComplete = %+v, want the peer's GOOD verdict", out.out.PeerComplete)
		}
		if out.out.TestID != h.hs.TestID {
			t.Errorf("TestID = %q, want %q", out.out.TestID, h.hs.TestID)
		}
		containsAll(t, logs.String(), "test_result")
	})

	t.Run("timeout-expiry-aborts-session", func(t *testing.T) {
		testutil.LeakCheck(t)
		cfg1, cfg2 := testConfigs(t)
		tr := newPipeTransport()
		cfg1.Transport, cfg2.Transport = tr, tr
		cfg1.NonInteractive = true
		cfg1.CallGrace = time.Second
		fc := clockAsFake(t, cfg1.Clock)

		callErr := make(chan error, 1)
		plan := func(ctx context.Context, rc RemoteCaller) error {
			_, err := rc.Call(ctx, "ping_run", nil, 2*time.Second, nil)
			callErr <- err
			return err
		}

		ctx := t.Context()
		resCh := startSession(ctx, cfg1, plan, nil)
		h := startWorkerHarness(t, ctx, tr, cfg2)

		h.await(protocol.TypeReady)
		h.send(protocol.TypeReady, "", protocol.Ready{})
		h.await(protocol.TypeStartConfirm)
		advanceThroughCountdown(t, fc)

		h.await(protocol.TypeTestRequest) // never answered
		fc.BlockUntilWaiters(2)           // heartbeat ticker + the Call expiry timer
		fc.Advance(3 * time.Second)       // timeout 2s + grace 1s

		ab := h.await(protocol.TypeAbort)
		abort, err := protocol.DecodePayload[protocol.Abort](ab)
		if err != nil {
			t.Fatalf("decode abort: %v", err)
		}
		if abort.Reason != "request_timeout" || abort.Stage != "ping_run" || abort.Initiator != "pc1" {
			t.Errorf("abort = %+v, want reason request_timeout, stage ping_run, initiator pc1", abort)
		}

		select {
		case err := <-callErr:
			if !errors.Is(err, ErrRequestTimeout) {
				t.Errorf("Call error = %v, want ErrRequestTimeout", err)
			}
		case <-time.After(testutil.TestTimeout(t)):
			t.Fatal("Call never returned after expiry")
		}
		out := awaitResult(t, resCh)
		if !errors.Is(out.err, ErrRequestTimeout) {
			t.Errorf("Run error = %v, want ErrRequestTimeout", out.err)
		}
		if out.out.FinalState != StateFailed {
			t.Errorf("FinalState = %s, want failed", out.out.FinalState)
		}
		if out.out.AbortReason != "request_timeout" {
			t.Errorf("AbortReason = %q, want request_timeout", out.out.AbortReason)
		}
	})
}

// TestRemoteCallerWarn pins the other half of the RemoteCaller surface: a
// plan's Warn emits one non-fatal warning frame carrying the code, text and
// the current state as stage, and the session still completes normally.
func TestRemoteCallerWarn(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg1.NonInteractive = true

	plan := func(ctx context.Context, rc RemoteCaller) error {
		rc.Warn("mtu_mismatch", "peer MTU 9000 exceeds local 1500")
		return nil
	}

	ctx := t.Context()
	resCh := startSession(ctx, cfg1, plan, nil)
	h := startWorkerHarness(t, ctx, tr, cfg2)

	h.await(protocol.TypeReady)
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.await(protocol.TypeStartConfirm)
	advanceThroughCountdown(t, clockAsFake(t, cfg1.Clock))

	w := h.await(protocol.TypeWarning)
	warn, err := protocol.DecodePayload[protocol.Warning](w)
	if err != nil {
		t.Fatalf("decode warning: %v", err)
	}
	if warn.Code != "mtu_mismatch" || warn.Text != "peer MTU 9000 exceeds local 1500" || warn.Stage != string(StateTesting) {
		t.Errorf("warning = %+v, want mtu_mismatch text at stage testing", warn)
	}

	// The warning is non-fatal: the plan finished and the session completes.
	comp := h.await(protocol.TypeComplete)
	h.send(protocol.TypeComplete, comp.MessageID, protocol.Complete{Classification: "GOOD", ExitCode: 0})
	out := awaitResult(t, resCh)
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.out.FinalState != StateCompleted {
		t.Errorf("FinalState = %s, want completed", out.out.FinalState)
	}
}

// clockAsFake asserts cfg carries the clocktest fake and returns it.
func clockAsFake(t *testing.T, c clock.Clock) *clocktest.FakeClock {
	t.Helper()
	fc, ok := c.(*clocktest.FakeClock)
	if !ok {
		t.Fatalf("test config clock is %T, want *clocktest.FakeClock", c)
	}
	return fc
}
