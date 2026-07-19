package peer

import (
	"context"
	"fmt"

	"cablecheck/internal/protocol"
)

// transferBufferSize is the routed-frame buffer between the event loop and a
// report-transfer callback; per-file acking keeps it small in practice.
const transferBufferSize = 64

// ReportChannel is the narrow seam handed to the report-transfer callbacks
// (Config.SendReports / Config.ReceiveReports): it can send report-transfer
// frames through the session's connection and receive the report-transfer
// frames the event loop routes to it. The chunking, digest and retry logic
// lives with the callbacks; this package only owns the message routing.
type ReportChannel struct {
	s *session
}

// TestID returns the session's assigned test ID.
func (r *ReportChannel) TestID() string { return r.s.testID }

// PeerCaps returns the peer's handshake capabilities (transfer acceptance,
// caps negotiation).
func (r *ReportChannel) PeerCaps() protocol.Capabilities { return r.s.peerCaps }

// Send writes one report-transfer frame (report, report_chunk or
// report_ack) with the given payload, correlated via inReplyTo when set.
func (r *ReportChannel) Send(typ protocol.MessageType, inReplyTo string, payload any) error {
	switch typ {
	case protocol.TypeReport, protocol.TypeReportChunk, protocol.TypeReportAck:
	default:
		return fmt.Errorf("peer: ReportChannel.Send: type %s is not a report-transfer frame", typ)
	}
	_, err := r.s.send(typ, inReplyTo, payload)
	return err
}

// Receive returns the next routed report-transfer frame, or an error when
// ctx or the session ends first.
func (r *ReportChannel) Receive(ctx context.Context) (*protocol.Envelope, error) {
	select {
	case env := <-r.s.transfer:
		return env, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.s.ctx.Done():
		return nil, ErrSessionClosed
	}
}

// startTransfer launches a report-transfer callback goroutine and switches
// the loop into routing report-transfer frames to it.
func (s *session) startTransfer(cb func(context.Context, *ReportChannel) error) {
	if s.transferOn {
		return
	}
	s.transferOn = true
	s.transferRan = true
	s.transfer = make(chan *protocol.Envelope, transferBufferSize)
	rt := &ReportChannel{s: s}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := cb(s.ctx, rt)
		s.sendEvent(evTransferDone{err: err})
	}()
}

// routeTransferFrame hands a report-transfer frame to the active callback.
// Without one the frame is state-invalid — unless a transfer already ran:
// stragglers still in flight when the callback ended are an expected
// consequence of per-file acking (a receiver failing mid-file leaves the
// sender streaming), and transfer failures are warnings by contract, so they
// are dropped with a debug log rather than fed to the invalid-state strike
// counter, which would escalate them into a protocol_error abort. The send
// is non-blocking: routing runs on the event loop itself, and a callback
// that stopped draining must not wedge the loop — its evTransferDone would
// be queued behind the routed frames and could never be processed. On a full
// buffer the frame is dropped with a warning; the callback's own
// ack/verification logic surfaces the loss.
func (s *session) routeTransferFrame(env *protocol.Envelope) {
	if !s.transferOn {
		if s.transferRan {
			s.log.Debug("late report-transfer frame dropped",
				"type", string(env.Type), "messageId", env.MessageID)
			return
		}
		s.warnInvalidState(env)
		return
	}
	select {
	case s.transfer <- env:
	default:
		s.log.Warn("report-transfer buffer full; frame dropped",
			"type", string(env.Type), "messageId", env.MessageID)
	}
}

// handleReportManifest processes the opening report frame on the receiving
// side: with a ReceiveReports hook the session enters generating_report and
// routes the manifest (and all subsequent transfer frames) to the hook;
// without one it declines the manifest so the sender can move on
// (docs/design/proto.md §8 step 1).
func (s *session) handleReportManifest(env *protocol.Envelope) {
	if s.cfg.Role != RolePC2 {
		// The coordinator streams reports; it never receives a manifest.
		s.warnInvalidState(env)
		return
	}
	if err := s.sm.Require(StateTesting, StateGeneratingReport); err != nil {
		s.warnInvalidState(env)
		return
	}
	if s.sm.Current() == StateTesting {
		if terr := s.sm.Transition(StateGeneratingReport); terr != nil {
			s.log.Warn("generating_report transition rejected", "err", terr)
			return
		}
	}
	if s.cfg.ReceiveReports == nil || s.cfg.NoReportTransfer {
		s.log.Info("declining report transfer: no receiver configured")
		// Mark the transfer as having run so the report_chunk frames the sender
		// streams before it reads this decline (it streams a whole file — >= 3
		// chunks for a >= 512 KiB file — before reading any ack) take the
		// late-frame drop path in routeTransferFrame instead of the
		// invalid-state strike counter. Without this the third such chunk would
		// trip maxInvalidState and abort the whole session with protocol_error,
		// forcing PC1 to exit 5 instead of its true health code.
		s.transferRan = true
		if _, err := s.send(protocol.TypeReportAck, env.MessageID, protocol.ReportAck{
			Declined: true,
			Error:    "report transfer not accepted",
		}); err != nil {
			s.log.Warn("send manifest decline failed", "err", err)
		}
		return
	}
	s.startTransfer(s.cfg.ReceiveReports)
	s.routeTransferFrame(env)
}

// handleTransferDone finishes the transfer phase. A transfer error is a
// warning by contract — never a session failure. The coordinator then moves
// on to the complete exchange; the worker keeps waiting for the
// coordinator's complete.
func (s *session) handleTransferDone(err error) {
	// Stop routing: a straggler transfer frame after the callback finished
	// must not pile into a channel nobody drains.
	s.transferOn = false
	if err != nil {
		s.log.Warn("report transfer failed", "err", err)
		if _, serr := s.send(protocol.TypeWarning, "", protocol.Warning{
			Code:  "report_transfer_failed",
			Text:  err.Error(),
			Stage: string(s.sm.Current()),
		}); serr != nil {
			s.log.Debug("send transfer warning failed", "err", serr)
		}
	}
	if s.cfg.Role == RolePC1 && !s.sentComplete {
		s.sendComplete()
	}
}
