package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// drivePlanFailure runs a coordinator session whose plan immediately returns
// planErr, drives the far end through readiness + the synchronized-start
// countdown, and returns the decoded farewell abort frame plus the Run result.
func drivePlanFailure(t *testing.T, planErr error) (*protocol.Abort, sessionResult) {
	t.Helper()
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport, cfg2.Transport = tr, tr
	cfg1.NonInteractive = true

	plan := func(ctx context.Context, rc RemoteCaller) error { return planErr }

	ctx := t.Context()
	resCh := startSession(ctx, cfg1, plan, nil)
	h := startWorkerHarness(t, ctx, tr, cfg2)

	h.await(protocol.TypeReady)
	h.send(protocol.TypeReady, "", protocol.Ready{})
	h.await(protocol.TypeStartConfirm)
	advanceThroughCountdown(t, clockAsFake(t, cfg1.Clock))

	ab := h.await(protocol.TypeAbort)
	abort, err := protocol.DecodePayload[protocol.Abort](ab)
	if err != nil {
		t.Fatalf("decode abort: %v", err)
	}
	return abort, awaitResult(t, resCh)
}

// TestPlanFailureAbortCarriesDetail pins requirement (1): when the coordinator
// plan fails, the internal_error abort sent to the peer carries the failing
// error message in Detail (previously it went out empty), so PC2 can see WHY.
func TestPlanFailureAbortCarriesDetail(t *testing.T) {
	const marker = "iperf3 client could not reach the server at 192.0.2.9"
	abort, res := drivePlanFailure(t, errors.New(marker))

	if abort.Reason != reasonInternalError {
		t.Errorf("abort.Reason = %q, want %q", abort.Reason, reasonInternalError)
	}
	if abort.Initiator != string(RolePC1) {
		t.Errorf("abort.Initiator = %q, want pc1", abort.Initiator)
	}
	if !strings.Contains(abort.Detail, marker) {
		t.Errorf("abort.Detail = %q, want it to contain the plan error %q", abort.Detail, marker)
	}
	if res.err == nil {
		t.Error("Run returned nil error for a failed plan")
	}
	if res.out.FinalState != StateFailed {
		t.Errorf("FinalState = %s, want failed", res.out.FinalState)
	}
	if res.out.AbortReason != reasonInternalError {
		t.Errorf("AbortReason = %q, want %q", res.out.AbortReason, reasonInternalError)
	}
}

// TestPlanFailureAbortRedactsToken is the load-bearing security test: a plan
// error whose text embeds the session token must never leak the token onto the
// wire (or into logs). Detail must be redacted before it reaches Abort.
func TestPlanFailureAbortRedactsToken(t *testing.T) {
	planErr := fmt.Errorf("handshake dial with token %s to peer failed", testToken)
	abort, _ := drivePlanFailure(t, planErr)

	if abort.Reason != reasonInternalError {
		t.Errorf("abort.Reason = %q, want %q", abort.Reason, reasonInternalError)
	}
	if strings.Contains(abort.Detail, testToken) {
		t.Errorf("abort.Detail leaked the session token: %q", abort.Detail)
	}
	if !strings.Contains(abort.Detail, redactedMarker) {
		t.Errorf("abort.Detail = %q, want it to contain %q", abort.Detail, redactedMarker)
	}
}
