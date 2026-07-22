package peer

import (
	"context"
	"fmt"
	"time"

	"cablecheck/internal/protocol"
)

// sendLocalReady announces local readiness once: it writes the ready frame
// and transitions out of waiting_for_local_start — straight to ready when
// the peer's ready already arrived. Invoked by the start command or
// automatically under --non-interactive.
func (s *session) sendLocalReady() {
	if s.localReady {
		return
	}
	if _, err := s.send(protocol.TypeReady, "", protocol.Ready{
		NonInteractive: s.cfg.NonInteractive,
	}); err != nil {
		s.log.Warn("send ready failed", "err", err)
		return
	}
	s.localReady = true
	next := StateWaitingForPeerStart
	if s.peerReady {
		next = StateReady
	}
	if terr := s.sm.Transition(next); terr != nil {
		s.log.Warn("readiness transition rejected", "err", terr)
		return
	}
	if next == StateReady {
		s.onBothReady()
	} else {
		fmt.Fprintln(s.stdout(), "ready — waiting for peer")
	}
}

// handlePeerReady processes the peer's ready frame. While we are still in
// waiting_for_local_start it only sets the flag — the transition table stays
// honest (docs/design/proto.md §4).
func (s *session) handlePeerReady(env *protocol.Envelope) {
	if s.peerReady {
		s.log.Warn("duplicate ready from peer ignored", "messageId", env.MessageID)
		return
	}
	switch s.sm.Current() {
	case StateWaitingForLocalStart:
		s.peerReady = true
	case StateWaitingForPeerStart:
		s.peerReady = true
		if err := s.sm.Transition(StateReady); err != nil {
			s.log.Warn("ready transition rejected", "err", err)
			return
		}
		s.onBothReady()
	default:
		s.warnInvalidState(env)
	}
}

// onBothReady runs when the session enters the ready state: the coordinator
// schedules the synchronized start; the worker waits for start_confirmation.
func (s *session) onBothReady() {
	fmt.Fprintln(s.stdout(), "both sides ready")
	if s.cfg.Role != RolePC1 {
		return
	}
	startIn := startCountdownMs * time.Millisecond
	if _, err := s.send(protocol.TypeStartConfirm, "", protocol.StartConfirmation{
		StartAt:   s.clk.Now().Add(startIn).UTC(),
		StartInMs: startCountdownMs,
		Mode:      s.cfg.Mode,
		Steps:     s.cfg.Steps,
	}); err != nil {
		s.log.Warn("send start_confirmation failed", "err", err)
		return
	}
	s.startCountdown(startIn)
}

// handleStartConfirmation processes the coordinator's synchronized start on
// the worker. The countdown anchor is the frame's ARRIVAL time plus
// startInMs; the absolute startAt is recorded for the report only — wall
// clocks on two air-gapped PCs are not trusted (proto.md pitfall 5).
func (s *session) handleStartConfirmation(env *protocol.Envelope) {
	if s.cfg.Role != RolePC2 {
		s.warnInvalidState(env)
		return
	}
	if err := s.sm.Require(StateReady); err != nil {
		s.warnInvalidState(env)
		return
	}
	sc, err := protocol.DecodePayload[protocol.StartConfirmation](env)
	if err != nil {
		s.log.Warn("malformed start_confirmation dropped", "err", err)
		return
	}
	s.mode = sc.Mode
	s.steps = append(s.steps[:0], sc.Steps...)
	s.log.Info("synchronized start scheduled", "startAt", sc.StartAt, "startInMs", sc.StartInMs, "mode", sc.Mode)
	s.startCountdown(time.Duration(sc.StartInMs) * time.Millisecond)
}

// startCountdown arms the 3-2-1-GO ticks: one second apart, anchored at the
// current instant plus total. Ticks that would land in the past (a short
// total) are skipped; GO always fires.
func (s *session) startCountdown(total time.Duration) {
	anchor := s.clk.Now()
	s.countdown = s.countdown[:0]
	for n := 3; n >= 1; n-- {
		at := total - time.Duration(n)*time.Second
		if at < 0 {
			continue
		}
		s.countdown = append(s.countdown, countdownStep{at: anchor.Add(at), label: fmt.Sprintf("%d", n)})
	}
	s.countdown = append(s.countdown, countdownStep{at: anchor.Add(total), label: ""})
	s.armCountdown()
}

// armCountdown schedules the next pending countdown step off Clock.After.
func (s *session) armCountdown() {
	if len(s.countdown) == 0 {
		s.countdownC = nil
		return
	}
	s.countdownC = s.clk.After(s.countdown[0].at.Sub(s.clk.Now()))
}

// onCountdownTick fires one countdown step: print the number, or GO —
// which transitions ready → testing and, on the coordinator, launches the
// plan driver.
func (s *session) onCountdownTick() {
	if len(s.countdown) == 0 {
		s.countdownC = nil
		return
	}
	step := s.countdown[0]
	s.countdown = s.countdown[1:]
	if step.label != "" {
		fmt.Fprintf(s.stdout(), "%s… ", step.label)
		s.armCountdown()
		return
	}
	s.countdownC = nil
	fmt.Fprintln(s.stdout(), "GO")
	if err := s.sm.Transition(StateTesting); err != nil {
		s.log.Warn("testing transition rejected", "err", err)
		return
	}
	if s.cfg.Role == RolePC1 {
		s.startPlan()
	}
}

// startPlan launches the coordinator's plan driver goroutine.
func (s *session) startPlan() {
	plan := s.plan
	if plan == nil {
		plan = func(ctx context.Context, rc RemoteCaller) error { return nil }
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := plan(s.ctx, &remoteCaller{s: s})
		s.sendEvent(evPlanDone{err: err})
	}()
}

// handleStdinLine executes one operator command.
func (s *session) handleStdinLine(line string) {
	switch line {
	case "":
		// Ignore blank lines: no help spam on stray Enter.
	case "start":
		if s.localReady {
			fmt.Fprintln(s.stdout(), "already ready")
			return
		}
		if err := s.sm.Require(StateWaitingForLocalStart); err != nil {
			fmt.Fprintf(s.stdout(), "cannot start now (state: %s)\n", s.sm.Current())
			return
		}
		s.sendLocalReady()
	case "status":
		age := time.Duration(0)
		if !s.peerHBAt.IsZero() {
			age = s.clk.Now().Sub(s.peerHBAt)
		}
		line := formatStatus(s.sm.Current(), s.peerHB, age)
		if !s.lastRecvAt.IsZero() {
			line += fmt.Sprintf(" | last frame %s ago", s.clk.Now().Sub(s.lastRecvAt).Round(time.Second))
		}
		fmt.Fprintln(s.stdout(), line)
	case "quit":
		s.finishAbort(reasonUserInterrupt, s.abortStage(),
			fmt.Errorf("%w: operator quit", ErrLocalAbort))
	default:
		fmt.Fprintln(s.stdout(), "commands: start | status | quit")
	}
}

// formatStatus renders the status command's line: own state plus the peer's
// last-heartbeat state.
func formatStatus(cur State, peer *protocol.Heartbeat, peerAge time.Duration) string {
	if peer == nil {
		return fmt.Sprintf("state: %s | peer: no heartbeat yet", cur)
	}
	line := fmt.Sprintf("state: %s | peer: %s (heartbeat #%d, %s ago", cur, peer.State, peer.Seq, peerAge.Round(time.Second))
	if peer.ActiveOp != "" {
		line += ", op " + peer.ActiveOp
	}
	return line + ")"
}
