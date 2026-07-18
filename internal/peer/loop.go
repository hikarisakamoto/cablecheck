package peer

import (
	"context"
	"fmt"
	"io"

	"cablecheck/internal/protocol"
)

// loop is the single-threaded session event loop; it owns every piece of
// loop-owned state and performs all conn writes except the heartbeater's.
// It runs until a handler sets the finish spec, then executes the ordered
// shutdown.
func (s *session) loop() (Outcome, error) {
	for s.fin == nil {
		select {
		case <-s.parent.Done():
			s.finishAbort(reasonUserInterrupt, s.abortStage(),
				fmt.Errorf("%w: %w", ErrLocalAbort, context.Cause(s.parent)))
		case <-s.countdownC:
			s.onCountdownTick()
		case ev := <-s.events:
			s.handleEvent(ev)
			if h := s.cfg.hooks.afterEvent; h != nil {
				h(ev)
			}
		}
	}
	return s.shutdown(*s.fin)
}

// finishAbort arranges an aborting shutdown with a farewell frame.
func (s *session) finishAbort(reason, stage string, err error) {
	s.fin = &finishSpec{
		state:          StateAborted,
		reason:         reason,
		farewell:       true,
		farewellReason: reason,
		farewellStage:  stage,
		err:            err,
	}
}

// abortStage is the Abort.Stage for locally initiated aborts: the active op
// when one runs, else the current state.
func (s *session) abortStage() string {
	if op := s.activeOpName(); op != "" {
		return op
	}
	return string(s.sm.Current())
}

// handleEvent dispatches one event.
func (s *session) handleEvent(ev event) {
	switch ev := ev.(type) {
	case evFrame:
		s.handleFrame(ev.env)
	case evConnErr:
		s.handleConnErr(ev.err)
	case evStdin:
		s.handleStdinLine(ev.line)
	case evStdinEOF:
		s.log.Info("stdin closed; treating as quit")
		s.finishAbort(reasonUserInterrupt, s.abortStage(),
			fmt.Errorf("%w: stdin closed", ErrLocalAbort))
	case evOpDone:
		s.handleOpDone(ev)
	case evOpProgress:
		s.handleOpProgress(ev)
	case evPlanDone:
		s.handlePlanDone(ev.err)
	case evTransferDone:
		s.handleTransferDone(ev.err)
	case evCallExpired:
		s.log.Error("test_request expired without a test_result", "op", ev.op, "messageId", ev.msgID)
		s.fin = &finishSpec{
			state:          StateFailed,
			reason:         reasonRequestTimeout,
			farewell:       true,
			farewellReason: reasonRequestTimeout,
			farewellStage:  ev.op,
			err:            fmt.Errorf("%w: op %q", ErrRequestTimeout, ev.op),
		}
	}
}

// handleConnErr treats any connection loss before completion as a peer-lost
// abort: partials are preserved by the caller, no farewell is written to a
// peer that is already gone.
func (s *session) handleConnErr(err error) {
	s.log.Warn("connection lost", "err", err, "state", string(s.sm.Current()))
	s.fin = &finishSpec{
		state:  StateAborted,
		reason: reasonPeerLost,
		err:    fmt.Errorf("%w: connection lost: %w", ErrPeerAborted, err),
	}
}

// handleFrame validates one inbound envelope and dispatches by type.
func (s *session) handleFrame(env *protocol.Envelope) {
	s.lastRecvAt = s.clk.Now()
	if ok, reason := s.dedup.Observe(env.MessageID); !ok {
		s.log.Warn("duplicate or out-of-order message dropped", "messageId", env.MessageID, "reason", reason)
		return
	}
	if err := protocol.CheckVersion(env); err != nil {
		s.log.Warn("frame with wrong protocol version dropped", "messageId", env.MessageID, "err", err)
		return
	}
	if err := checkAssignedTestID(env, s.testID); err != nil {
		s.log.Warn("frame with foreign testId dropped", "messageId", env.MessageID, "err", err)
		return
	}

	switch env.Type {
	case protocol.TypeHeartbeat:
		if hb, err := protocol.DecodePayload[protocol.Heartbeat](env); err == nil {
			s.peerHB = hb
			s.peerHBAt = s.clk.Now()
		} else {
			s.log.Warn("malformed heartbeat dropped", "err", err)
		}
	case protocol.TypeReady:
		s.handlePeerReady(env)
	case protocol.TypeStartConfirm:
		s.handleStartConfirmation(env)
	case protocol.TypeTestRequest:
		s.handleTestRequest(env)
	case protocol.TypeTestProgress:
		if pc := s.peekCall(env.InReplyTo); pc != nil {
			if pc.onProgress != nil {
				if p, err := protocol.DecodePayload[protocol.TestProgress](env); err == nil {
					pc.onProgress(*p)
				} else {
					s.log.Warn("malformed test_progress dropped", "err", err)
				}
			}
		} else {
			s.log.Warn("test_progress with no pending call dropped", "inReplyTo", env.InReplyTo)
		}
	case protocol.TypeTestResult:
		s.handleTestResult(env)
	case protocol.TypeWarning:
		if w, err := protocol.DecodePayload[protocol.Warning](env); err == nil {
			s.log.Warn("peer warning", "code", w.Code, "text", w.Text, "stage", w.Stage)
		} else {
			s.log.Warn("malformed warning dropped", "err", err)
		}
	case protocol.TypeAbort:
		s.handlePeerAbort(env)
	case protocol.TypeReport:
		s.handleReportManifest(env)
	case protocol.TypeReportChunk, protocol.TypeReportAck:
		s.routeTransferFrame(env)
	case protocol.TypeComplete:
		s.handleComplete(env)
	case protocol.TypeHello, protocol.TypeHelloAck, protocol.TypeCapabilities:
		s.warnInvalidState(env)
	default:
		s.log.Warn("unknown message type ignored", "type", string(env.Type), "messageId", env.MessageID)
	}
}

// warnInvalidState answers a state-inappropriate frame with a warning and
// counts strikes; three strikes abort the session with protocol_error
// (docs/design/proto.md §4).
func (s *session) warnInvalidState(env *protocol.Envelope) {
	s.log.Warn("frame invalid in current state", "type", string(env.Type),
		"messageId", env.MessageID, "state", string(s.sm.Current()))
	if _, err := s.send(protocol.TypeWarning, "", protocol.Warning{
		Code:  "invalid_state",
		Text:  fmt.Sprintf("%s not valid in state %s", env.Type, s.sm.Current()),
		Stage: string(s.sm.Current()),
	}); err != nil {
		s.log.Debug("invalid_state warning send failed", "err", err)
	}
	s.invalidState++
	if s.invalidState >= maxInvalidState {
		s.finishAbort(abortProtocolError, s.abortStage(),
			fmt.Errorf("%w: %d invalid-state frames", ErrPeerAborted, s.invalidState))
	}
}

// handlePeerAbort processes the peer's abort frame: cancel local work,
// preserve partials, terminal state aborted with exit-5 semantics.
func (s *session) handlePeerAbort(env *protocol.Envelope) {
	ab, err := protocol.DecodePayload[protocol.Abort](env)
	if err != nil {
		ab = &protocol.Abort{Reason: "unknown"}
	}
	fmt.Fprintf(s.stdout(), "peer aborted: %s at %s\n", ab.Reason, ab.Stage)
	s.log.Warn("peer aborted", "reason", ab.Reason, "stage", ab.Stage, "detail", ab.Detail)
	s.fin = &finishSpec{
		state:  StateAborted,
		reason: ab.Reason,
		err:    fmt.Errorf("%w: %s at %s", ErrPeerAborted, ab.Reason, ab.Stage),
	}
}

// stdout is the operator-facing writer.
func (s *session) stdout() io.Writer { return s.cfg.stdout() }
