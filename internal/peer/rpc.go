package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cablecheck/internal/protocol"
)

// RPC-surface errors of the coordinator session.
var (
	// ErrSessionClosed is returned by Call when the session tears down
	// before the result arrives (or a Call is attempted afterwards).
	ErrSessionClosed = errors.New("peer: session closed")
	// ErrRequestTimeout is returned by Call — and wrapped in Run's error —
	// when a test_request got no test_result within TimeoutMs plus the call
	// grace. The coordinator aborts the session: a worker that heartbeats
	// but cannot answer is not trustworthy for the remaining steps
	// (docs/design/proto.md §6).
	ErrRequestTimeout = errors.New("peer: request timed out waiting for test_result")
)

// pendingCall tracks one in-flight test_request on the coordinator.
type pendingCall struct {
	// done receives the correlated test_result (buffered so the loop never
	// blocks); it is closed on session teardown.
	done chan *protocol.TestResult
	// onProgress, when non-nil, is invoked from the event loop for every
	// routed test_progress; it must not block.
	onProgress func(protocol.TestProgress)
	// op names the requested operation, for logs and the timeout abort.
	op string
}

// registerCall adds a pending call under msgID; it reports false once the
// session has started tearing down.
func (s *session) registerCall(msgID string, pc *pendingCall) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pendingClosed {
		return false
	}
	s.pending[msgID] = pc
	return true
}

// unregisterCall removes a pending call, if still present.
func (s *session) unregisterCall(msgID string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	delete(s.pending, msgID)
}

// takeCall removes and returns the pending call correlated to inReplyTo, or
// nil (late result, unknown ID).
func (s *session) takeCall(inReplyTo string) *pendingCall {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	pc := s.pending[inReplyTo]
	delete(s.pending, inReplyTo)
	return pc
}

// peekCall returns the pending call correlated to inReplyTo without removing
// it (progress routing), or nil.
func (s *session) peekCall(inReplyTo string) *pendingCall {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	return s.pending[inReplyTo]
}

// closePendingCalls fails every in-flight Call with ErrSessionClosed (their
// done channels are closed) and refuses new registrations. Part of teardown.
func (s *session) closePendingCalls() {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pendingClosed {
		return
	}
	s.pendingClosed = true
	for id, pc := range s.pending {
		close(pc.done)
		delete(s.pending, id)
	}
}

// remoteCaller is the RemoteCaller handed to the coordinator's PlanFunc.
type remoteCaller struct {
	s *session
}

// Call implements RemoteCaller: it writes one test_request and blocks until
// the correlated test_result, cancellation, or expiry of TimeoutMs plus the
// session's call grace — in which case it signals the loop to abort the
// session with reason request_timeout.
func (rc *remoteCaller) Call(ctx context.Context, op string, params any, timeout time.Duration,
	onProgress func(protocol.TestProgress)) (*protocol.TestResult, error) {
	s := rc.s
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("peer: marshal params for op %q: %w", op, err)
	}
	pc := &pendingCall{done: make(chan *protocol.TestResult, 1), onProgress: onProgress, op: op}
	registered := false
	msgID, err := s.mintAndWrite(func(msgID string) (*protocol.Envelope, error) {
		// Registration must precede the write (a fast result would find no
		// pending call), and mint+write must be atomic (mintAndWrite doc).
		if !s.registerCall(msgID, pc) {
			return nil, ErrSessionClosed
		}
		registered = true
		return protocol.NewEnvelope(protocol.TypeTestRequest, s.testID, msgID, protocol.TestRequest{
			Op:        op,
			Params:    raw,
			TimeoutMs: int(timeout / time.Millisecond),
		})
	})
	if registered {
		defer s.unregisterCall(msgID)
	}
	if err != nil {
		if errors.Is(err, ErrSessionClosed) {
			return nil, ErrSessionClosed
		}
		return nil, fmt.Errorf("peer: send test_request for op %q: %w", op, err)
	}

	budget := timeout + s.cfg.callGrace()
	select {
	case res, ok := <-pc.done:
		if !ok {
			return nil, ErrSessionClosed
		}
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrSessionClosed
	case <-s.clk.After(budget):
		s.sendEvent(evCallExpired{msgID: msgID, op: op})
		return nil, fmt.Errorf("%w: op %q after %s", ErrRequestTimeout, op, budget)
	}
}

// Warn implements RemoteCaller by sending a non-fatal warning frame to the
// peer; a failed send is logged and otherwise ignored.
func (rc *remoteCaller) Warn(code, text string) {
	s := rc.s
	if _, err := s.send(protocol.TypeWarning, "", protocol.Warning{
		Code:  code,
		Text:  text,
		Stage: string(s.sm.Current()),
	}); err != nil {
		s.log.Warn("send warning frame failed", "code", code, "err", err)
	}
}

// SetIdleTimeout implements RemoteCaller by changing the coordinator's
// local framed-connection deadline for subsequent reads.
func (rc *remoteCaller) SetIdleTimeout(d time.Duration) {
	rc.s.conn.SetIdleTimeout(d)
}
