package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cablecheck/internal/protocol"
)

// handleTestRequest processes one coordinator RPC on the worker: at most one
// op runs at a time — an overlapping request (except op "cancel") is
// answered immediately with status "rejected".
func (s *session) handleTestRequest(env *protocol.Envelope) {
	if s.cfg.Role != RolePC2 {
		s.warnInvalidState(env)
		return
	}
	if err := s.sm.Require(StateTesting); err != nil {
		s.warnInvalidState(env)
		return
	}
	req, err := protocol.DecodePayload[protocol.TestRequest](env)
	if err != nil {
		s.log.Warn("malformed test_request dropped", "err", err)
		return
	}
	s.opMu.Lock()
	running := s.activeOp
	s.opMu.Unlock()
	if req.Op == "cancel" {
		if running != nil {
			s.log.Info("cancelling active op on request", "op", running.op)
			running.cancel()
		}
		s.writeTestResult(env.MessageID, protocol.TestResult{
			Status: "ok", StartedAt: s.clk.Now().UTC(), FinishedAt: s.clk.Now().UTC(),
		})
		return
	}
	if running != nil {
		s.log.Warn("overlapping test_request rejected", "op", req.Op, "active", running.op)
		s.writeTestResult(env.MessageID, protocol.TestResult{
			Status:     "rejected",
			Error:      fmt.Sprintf("op %q already active", running.op),
			StartedAt:  s.clk.Now().UTC(),
			FinishedAt: s.clk.Now().UTC(),
		})
		return
	}
	s.startOp(env.MessageID, req)
}

// startOp launches the single executor goroutine for one accepted request.
// The executor never touches the connection: progress and the result travel
// through the event loop as evOpProgress/evOpDone.
func (s *session) startOp(reqID string, req *protocol.TestRequest) {
	var opCtx context.Context
	var cancel context.CancelFunc
	if req.TimeoutMs > 0 {
		opCtx, cancel = context.WithTimeout(s.ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	} else {
		opCtx, cancel = context.WithCancel(s.ctx)
	}
	op := &activeOp{reqID: reqID, op: req.Op, cancel: cancel, done: make(chan struct{})}
	s.opMu.Lock()
	s.activeOp = op
	s.opMu.Unlock()

	ops := s.ops
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(op.done)
		defer cancel()
		started := s.clk.Now().UTC()

		var lastProgress time.Time
		progress := func(p protocol.TestProgress) {
			now := s.clk.Now()
			if !lastProgress.IsZero() && now.Sub(lastProgress) < progressMinInterval {
				return // throttle to at most one update per second
			}
			lastProgress = now
			s.sendEvent(evOpProgress{reqID: reqID, p: p})
		}

		var payload any
		var status string
		var err error
		if ops == nil {
			status, err = "unavailable", fmt.Errorf("peer: no op handler configured")
		} else {
			payload, status, err = ops.HandleOp(opCtx, req.Op, req.Params, progress)
		}
		res := protocol.TestResult{
			Status:     status,
			StartedAt:  started,
			FinishedAt: s.clk.Now().UTC(),
		}
		if err != nil {
			res.Error = err.Error()
			if res.Status == "" || res.Status == "ok" {
				res.Status = "failed"
			}
		} else if res.Status == "" {
			res.Status = "ok"
		}
		s.sendEvent(evOpDone{reqID: reqID, res: res, resultPayload: payload})
	}()
}

// handleOpDone routes the executor's finished op back to the coordinator and
// releases the single-op slot.
func (s *session) handleOpDone(ev evOpDone) {
	res := ev.res
	if ev.resultPayload != nil {
		raw, err := json.Marshal(ev.resultPayload)
		if err != nil {
			s.log.Warn("marshal op result failed", "op", ev.reqID, "err", err)
			res.Status = "failed"
			res.Error = fmt.Sprintf("marshal result: %v", err)
		} else {
			res.Result = raw
		}
	}
	s.writeTestResult(ev.reqID, res)
	s.opMu.Lock()
	if s.activeOp != nil && s.activeOp.reqID == ev.reqID {
		s.activeOp = nil
	}
	s.opMu.Unlock()
}

// handleOpProgress forwards one throttled executor progress update.
func (s *session) handleOpProgress(ev evOpProgress) {
	if _, err := s.send(protocol.TypeTestProgress, ev.reqID, ev.p); err != nil {
		s.log.Debug("send test_progress failed", "err", err)
	}
}

// writeTestResult sends a test_result correlated to reqID.
func (s *session) writeTestResult(reqID string, res protocol.TestResult) {
	if _, err := s.send(protocol.TypeTestResult, reqID, res); err != nil {
		s.log.Warn("send test_result failed", "err", err)
	}
}

// handleTestResult resolves the coordinator's pending Call, tolerating a
// late result whose call already timed out or finished (log + drop, never
// panic — proto.md pitfall 9).
func (s *session) handleTestResult(env *protocol.Envelope) {
	pc := s.takeCall(env.InReplyTo)
	if pc == nil {
		s.log.Warn("late test_result with no pending call dropped", "inReplyTo", env.InReplyTo, "messageId", env.MessageID)
		return
	}
	res, err := protocol.DecodePayload[protocol.TestResult](env)
	if err != nil {
		s.log.Warn("malformed test_result; failing the call", "err", err)
		res = &protocol.TestResult{Status: "failed", Error: fmt.Sprintf("malformed test_result: %v", err)}
	}
	pc.done <- res // buffered; the loop never blocks here
}

// handlePlanDone reacts to the coordinator plan finishing: success moves the
// session into generating_report and kicks off report transfer (when wired)
// followed by the complete exchange; failure aborts the session.
func (s *session) handlePlanDone(err error) {
	if err != nil {
		if isCancellation(err) {
			// The teardown that cancelled the plan is already in flight;
			// evPlanDone processed here only loses a race with it.
			s.log.Info("test plan ended by cancellation", "err", err)
			return
		}
		s.log.Error("test plan failed", "err", err)
		s.fin = &finishSpec{
			state:          StateFailed,
			reason:         reasonInternalError,
			farewell:       true,
			farewellReason: reasonInternalError,
			farewellStage:  string(s.sm.Current()),
			err:            fmt.Errorf("peer: test plan failed: %w", err),
		}
		return
	}
	if terr := s.sm.Transition(StateGeneratingReport); terr != nil {
		s.log.Warn("generating_report transition rejected", "err", terr)
		return
	}
	if s.cfg.SendReports != nil && !s.cfg.NoReportTransfer && s.peerCaps.AcceptReportTransfer {
		s.startTransfer(s.cfg.SendReports)
		return
	}
	s.sendComplete()
}

// isCancellation reports whether err only reflects session teardown.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, ErrSessionClosed)
}

// sendComplete writes this side's complete frame (final verdict payload from
// Config.Complete) and, on the coordinator, leaves the loop waiting for the
// peer's answering complete.
func (s *session) sendComplete() {
	s.writeComplete("")
	s.sentComplete = true
}

// writeComplete emits the complete frame, optionally correlated.
func (s *session) writeComplete(inReplyTo string) {
	payload := protocol.Complete{}
	if s.cfg.Complete != nil {
		payload = s.cfg.Complete()
	}
	if _, err := s.send(protocol.TypeComplete, inReplyTo, payload); err != nil {
		s.log.Warn("send complete failed", "err", err)
	}
}

// handleComplete processes the peer's complete frame: the worker answers
// with its own view and both sides finish in state completed.
func (s *session) handleComplete(env *protocol.Envelope) {
	comp, err := protocol.DecodePayload[protocol.Complete](env)
	if err != nil {
		s.log.Warn("malformed complete dropped", "err", err)
		return
	}
	switch s.cfg.Role {
	case RolePC1:
		if !s.sentComplete {
			s.warnInvalidState(env)
			return
		}
	case RolePC2:
		if serr := s.sm.Require(StateTesting, StateGeneratingReport); serr != nil {
			s.warnInvalidState(env)
			return
		}
		if s.sm.Current() == StateTesting {
			if terr := s.sm.Transition(StateGeneratingReport); terr != nil {
				s.log.Warn("generating_report transition rejected", "err", terr)
			}
		}
		s.writeComplete(env.MessageID)
	}
	s.peerComplete = comp
	s.fin = &finishSpec{state: StateCompleted}
}
