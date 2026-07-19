package peer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/protocol"
)

// Session-level errors surfaced by Run. The app layer maps them to exit
// codes: ErrPeerAborted to the peer/orchestration failure (5), ErrLocalAbort
// to the local interrupt (6), handshake and token failures to the
// config/dependency failure (4).
var (
	// ErrPeerAborted reports the peer aborting the session or vanishing
	// (Outcome.AbortReason carries the peer's reason, or "peer_lost").
	ErrPeerAborted = errors.New("peer: session aborted by peer")
	// ErrLocalAbort reports a local abort: Ctrl+C (context cancellation),
	// the quit command, or stdin EOF.
	ErrLocalAbort = errors.New("peer: session aborted locally")
	// ErrHandshakeFailed reports that no authenticated session could be
	// established; it wraps the specific handshake failure.
	ErrHandshakeFailed = errors.New("peer: connection or handshake failed")
	// ErrTokenRejected reports that the coordinator refused our token — a
	// local configuration error on the worker, never retried.
	ErrTokenRejected error = errTokenRejected
)

// Abort reasons introduced by the session layer (the handshake ones live in
// handshake.go).
const (
	reasonUserInterrupt  = "user_interrupt"
	reasonPeerLost       = "peer_lost"
	reasonRequestTimeout = "request_timeout"
	reasonInternalError  = "internal_error"
)

// Session tunables.
const (
	// eventBufferSize is the event channel capacity; every producer send
	// additionally selects on the session context.
	eventBufferSize = 64
	// maxAuthStrikes is how many wrong-token handshakes the coordinator
	// tolerates before giving up.
	maxAuthStrikes = 3
	// maxHandshakeRetries bounds retryable non-auth handshake failures on
	// the coordinator (one retry, then exit).
	maxHandshakeRetries = 2
	// maxInvalidState is how many invalid-state frames the peer may send
	// before the session aborts with protocol_error.
	maxInvalidState = 3
	// farewellWriteTimeout bounds the best-effort farewell abort write
	// during teardown; a stalled write never delays shutdown further.
	farewellWriteTimeout = 2 * time.Second
	// startCountdownMs is the synchronized-start lead time the coordinator
	// announces in start_confirmation.
	startCountdownMs = 3500
	// progressMinInterval is the worker-side floor between forwarded
	// progress updates.
	progressMinInterval = time.Second
)

// Outcome and the Run signature are part of the canonical cross-package
// contract; see config.go for Outcome and the callback types.

// Run executes one peer session to completion: it establishes the control
// connection (the coordinator listens, the worker dials), performs the
// authenticated handshake, then runs the single-threaded event loop over
// readiness sync, the synchronized start, RPC orchestration, report-transfer
// routing and the complete exchange. Preflight is the caller's job. The
// returned Outcome is meaningful even on error (partial-session reporting).
func Run(ctx context.Context, cfg Config, plan PlanFunc, ops OpHandler) (Outcome, error) {
	s := newSession(cfg, plan, ops)
	return s.run(ctx)
}

// session owns all state of one Run. The event loop goroutine is the only
// writer of the loop-owned fields; producers communicate exclusively through
// the events channel.
type session struct {
	cfg  Config
	plan PlanFunc
	ops  OpHandler
	clk  clock.Clock
	log  *slog.Logger

	sm       *StateMachine
	conn     *protocol.Conn
	ids      *protocol.MessageIDMaker
	dedup    *protocol.DuplicateDetector
	testID   string
	peerCaps protocol.Capabilities
	mode     string

	parent context.Context // the caller's ctx: cancellation = local abort
	ctx    context.Context // session ctx: cancelled first in teardown
	cancel context.CancelFunc
	events chan event
	wg     sync.WaitGroup

	// writeMu makes minting a message ID and writing the frame one atomic
	// step (mintAndWrite): concurrent writers may otherwise reach the wire
	// in the opposite order of their IDs, and the receiver silently drops
	// non-increasing sequences.
	writeMu sync.Mutex

	// lastSend is the clk-timebase unix-nano timestamp of the last
	// successful frame write through s.write; the heartbeater's skip check.
	lastSend atomic.Int64

	// pending is the coordinator's in-flight Call registry (rpc.go).
	pendingMu     sync.Mutex
	pending       map[string]*pendingCall
	pendingClosed bool

	// activeOp is the worker's single in-flight op; opMu guards it because
	// the heartbeater reads the op name.
	opMu     sync.Mutex
	activeOp *activeOp

	// stdinFile is non-nil when stdin supports read deadlines; teardown
	// unblocks the pump through it. Otherwise the pump is the one
	// sanctioned orphan goroutine (docs/design/proto.md §5).
	stdinFile *os.File

	// Loop-owned state (no locking: single goroutine).
	localReady   bool
	peerReady    bool
	sentComplete bool
	peerComplete *protocol.Complete
	peerHB       *protocol.Heartbeat
	peerHBAt     time.Time
	lastRecvAt   time.Time
	invalidState int
	countdown    []countdownStep
	countdownC   <-chan time.Time
	transfer     chan *protocol.Envelope
	transferOn   bool
	transferRan  bool
	fin          *finishSpec
}

// activeOp is the handle of the worker's in-flight operation.
type activeOp struct {
	reqID  string
	op     string
	cancel context.CancelFunc
	done   chan struct{} // closed when the executor goroutine finished
}

// countdownStep is one scheduled tick of the synchronized-start countdown.
type countdownStep struct {
	at    time.Time
	label string // "3", "2", "1" or "" for GO
}

// finishSpec tells the loop how to shut the session down.
type finishSpec struct {
	state          State  // terminal state to enter
	reason         string // Outcome.AbortReason ("" for completed)
	farewell       bool   // send a best-effort abort frame
	farewellReason string
	farewellStage  string
	err            error // Run's returned error
}

// newSession builds a session from cfg without touching the network.
func newSession(cfg Config, plan PlanFunc, ops OpHandler) *session {
	s := &session{
		cfg:     cfg,
		plan:    plan,
		ops:     ops,
		clk:     cfg.clock(),
		log:     cfg.logger(),
		events:  make(chan event, eventBufferSize),
		pending: make(map[string]*pendingCall),
	}
	if cfg.Role == RolePC1 {
		s.mode = cfg.Mode
	}
	return s
}

// run is Run's body on the constructed session.
func (s *session) run(ctx context.Context) (Outcome, error) {
	s.parent = ctx
	s.sm = NewStateMachine(StateInitializing, func(from, to State) {
		s.log.Info("state", "from", string(from), "to", string(to))
		if h := s.cfg.OnState; h != nil {
			h(from, to)
		}
		if h := s.cfg.hooks.onState; h != nil {
			h(from, to)
		}
	})
	// Preflight ran before Run by contract; walk the table to the
	// connection phase.
	if err := s.sm.Transition(StatePreflight); err != nil {
		return s.failEarly(err)
	}

	hs, conn, err := s.establish(ctx)
	if err != nil {
		if ctx.Err() != nil {
			_ = s.sm.Transition(StateAborted)
			return s.outcome(reasonUserInterrupt), fmt.Errorf("%w: %w", ErrLocalAbort, context.Cause(ctx))
		}
		_ = s.sm.Transition(StateFailed)
		return s.outcome(""), fmt.Errorf("%w: %w", ErrHandshakeFailed, err)
	}
	s.conn = conn
	s.testID = hs.TestID
	s.peerCaps = hs.PeerCaps
	s.ids = hs.IDs
	s.dedup = hs.Dedup
	if h := s.cfg.OnHandshake; h != nil {
		h(s.testID)
	}
	if err := s.sm.Transition(StateWaitingForLocalStart); err != nil {
		conn.Close()
		return s.failEarly(err)
	}

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(2)
	go s.readLoop()
	go s.heartbeatLoop()
	s.startStdin()
	if s.cfg.NonInteractive {
		s.sendLocalReady()
	}
	return s.loop()
}

// failEarly handles impossible internal failures before the loop exists.
func (s *session) failEarly(err error) (Outcome, error) {
	_ = s.sm.Transition(StateFailed)
	return s.outcome(""), fmt.Errorf("peer: internal error: %w", err)
}

// outcome assembles the Outcome from the session's current state.
func (s *session) outcome(reason string) Outcome {
	return Outcome{
		FinalState:   s.sm.Current(),
		PeerComplete: s.peerComplete,
		AbortReason:  reason,
		PeerCaps:     s.peerCaps,
		TestID:       s.testID,
		Mode:         s.mode,
	}
}

// controlAddr is the "ip:port" of the control channel.
func (s *session) controlAddr() string {
	ip := s.cfg.PeerIP
	if s.cfg.Role == RolePC1 {
		ip = s.cfg.LocalIP
	}
	return net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(int(s.cfg.ControlPort)))
}

// establish produces the authenticated control connection for either role.
func (s *session) establish(ctx context.Context) (*handshakeResult, *protocol.Conn, error) {
	if s.cfg.Role == RolePC1 {
		if err := s.sm.Transition(StateListening); err != nil {
			return nil, nil, err
		}
		return s.establishCoordinator(ctx)
	}
	if err := s.sm.Transition(StateConnecting); err != nil {
		return nil, nil, err
	}
	return s.establishWorker(ctx)
}

// establishWorker dials the coordinator and runs the worker handshake; every
// failure is fatal for the process (docs/design/proto.md §3).
func (s *session) establishWorker(ctx context.Context) (*handshakeResult, *protocol.Conn, error) {
	nc, err := s.cfg.transport().Dial(ctx, s.cfg.LocalIP, s.controlAddr())
	if err != nil {
		return nil, nil, err
	}
	conn := protocol.NewConn(nc)
	if err := s.sm.Transition(StateHandshake); err != nil {
		conn.Close()
		return nil, nil, err
	}
	hs, err := workerHandshake(conn, s.cfg)
	if err != nil {
		return nil, nil, err
	}
	return hs, conn, nil
}

// establishCoordinator listens, verifies the caller's IP and runs the
// coordinator handshake, tolerating up to maxAuthStrikes wrong-token
// attempts and one other retryable failure while keeping the listener open;
// the listener closes as soon as one handshake succeeds.
func (s *session) establishCoordinator(ctx context.Context) (*handshakeResult, *protocol.Conn, error) {
	ln, err := s.cfg.transport().Listen(ctx, s.controlAddr())
	if err != nil {
		return nil, nil, err
	}
	// A blocked Accept ignores ctx; the watcher closes the listener on
	// cancellation. stop makes the watcher exit once establish returns.
	stop := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		select {
		case <-ctx.Done():
		case <-stop:
		}
		ln.Close()
	}()
	defer func() {
		close(stop)
		watcher.Wait()
	}()

	authStrikes, retries := 0, 0
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, fmt.Errorf("accept on %s: %w", s.controlAddr(), err)
		}
		if err := verifyPeerIP(nc, s.cfg.PeerIP); err != nil {
			// Closed without a response; keep listening, no strike.
			s.log.Warn("rejected connection from unexpected address", "err", err)
			continue
		}
		conn := protocol.NewConn(nc)
		hs, err := coordinatorHandshake(conn, s.cfg)
		if err == nil {
			// Success: the deferred close shuts the listener; nobody else
			// gets to connect.
			if terr := s.sm.Transition(StateHandshake); terr != nil {
				conn.Close()
				return nil, nil, terr
			}
			return hs, conn, nil
		}
		switch {
		case errors.Is(err, errAuthFailed):
			authStrikes++
			s.log.Warn("handshake auth strike", "strikes", authStrikes)
			if authStrikes >= maxAuthStrikes {
				return nil, nil, fmt.Errorf("giving up after %d auth failures: %w", authStrikes, err)
			}
		case handshakeRetryable(err):
			retries++
			s.log.Warn("retryable handshake failure", "attempt", retries, "err", err)
			if retries >= maxHandshakeRetries {
				return nil, nil, fmt.Errorf("giving up after %d handshake failures: %w", retries, err)
			}
		default:
			return nil, nil, err
		}
	}
}

// mintAndWrite runs build with a freshly minted message ID and writes the
// returned envelope. Mint and write MUST form one atomic step: the receiver
// drops any frame whose per-role sequence is non-increasing, so an ID minted
// first has to reach the wire first. Every post-handshake writer (event
// loop, heartbeater, plan goroutine, transfer callback, farewell) sends
// through here. The minted ID is returned even when build or the write
// fails.
func (s *session) mintAndWrite(build func(msgID string) (*protocol.Envelope, error)) (string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	msgID := s.ids.Next()
	env, err := build(msgID)
	if err != nil {
		return msgID, err
	}
	if h := s.cfg.hooks.beforeWrite; h != nil {
		h(env)
	}
	return msgID, s.write(env)
}

// send mints a message ID, builds one envelope of type typ carrying payload
// (correlated via inReplyTo when non-empty) and writes it through
// mintAndWrite, returning the minted message ID.
func (s *session) send(typ protocol.MessageType, inReplyTo string, payload any) (string, error) {
	return s.mintAndWrite(func(msgID string) (*protocol.Envelope, error) {
		env, err := protocol.NewEnvelope(typ, s.testID, msgID, payload)
		if err != nil {
			return nil, err
		}
		env.InReplyTo = inReplyTo
		return env, nil
	})
}

// write sends one frame and records the send time in the session clock's
// timebase so the heartbeater can compare ages consistently even under a
// fake clock. All session frame writes go through here.
func (s *session) write(env *protocol.Envelope) error {
	if err := s.conn.WriteEnvelope(env); err != nil {
		return err
	}
	s.lastSend.Store(s.clk.Now().UnixNano())
	return nil
}

// lastSendTime returns the time of the last successful write, zero if none.
func (s *session) lastSendTime() time.Time {
	n := s.lastSend.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// sendEvent delivers ev to the loop; it never blocks past session teardown
// because it selects on the session context.
func (s *session) sendEvent(ev event) {
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}

// readLoop is the connection reader goroutine: it forwards every inbound
// frame and exits after reporting the first read error (exactly one
// evConnErr). A blocked read ignores ctx; teardown unblocks it by closing
// the connection before waiting on the WaitGroup.
func (s *session) readLoop() {
	defer s.wg.Done()
	for {
		env, err := s.conn.ReadEnvelope()
		if err != nil {
			select {
			case s.events <- evConnErr{err: err}:
			case <-s.ctx.Done():
			}
			return
		}
		select {
		case s.events <- evFrame{env: env}:
		case <-s.ctx.Done():
			return
		}
	}
}

// heartbeatLoop sends periodic liveness frames: a ticker at the heartbeat
// interval whose ticks write a heartbeat only when the last send is at least
// 80% of the interval old — recent traffic already proves liveness. It is
// one of the two sanctioned connection writers besides the event loop.
func (s *session) heartbeatLoop() {
	defer s.wg.Done()
	interval := s.cfg.heartbeatInterval()
	threshold := interval * 4 / 5
	ticker := s.clk.NewTicker(interval)
	defer ticker.Stop()
	var seq uint64
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C():
			sent := false
			last := s.lastSendTime()
			if last.IsZero() || s.clk.Now().Sub(last) >= threshold {
				seq++
				_, err := s.send(protocol.TypeHeartbeat, "", protocol.Heartbeat{
					Seq:      seq,
					State:    string(s.sm.Current()),
					ActiveOp: s.activeOpName(),
				})
				if err != nil {
					// The reader surfaces connection death; just note it.
					s.log.Debug("heartbeat send failed", "err", err)
					seq--
				} else {
					sent = true
				}
			}
			if h := s.cfg.hooks.afterHeartbeatTick; h != nil {
				h(sent)
			}
		}
	}
}

// activeOpName returns the in-flight op's name, or "".
func (s *session) activeOpName() string {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.activeOp == nil {
		return ""
	}
	return s.activeOp.op
}

// startStdin launches the interactive prompt pump. The goroutine is never in
// the WaitGroup: when stdin supports read deadlines (Linux ttys and pipes)
// teardown unblocks it via SetReadDeadline; otherwise (a regular file) it is
// the one sanctioned orphan — its sends select on the session context, so it
// can never block anything, and process exit reaps it.
func (s *session) startStdin() {
	if s.cfg.NonInteractive || s.cfg.Stdin == nil {
		return
	}
	if f, ok := s.cfg.Stdin.(*os.File); ok {
		if err := f.SetReadDeadline(time.Time{}); err == nil {
			s.stdinFile = f
		}
	}
	go s.stdinPump()
}

// stdinPump reads operator lines until EOF (which means quit) or teardown.
func (s *session) stdinPump() {
	sc := bufio.NewScanner(s.cfg.Stdin)
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		select {
		case s.events <- evStdin{line: line}:
		case <-s.ctx.Done():
			return
		}
	}
	select {
	case s.events <- evStdinEOF{}:
	case <-s.ctx.Done():
	}
}

// unblockStdin wakes a pump blocked in Read, when the reader supports it.
func (s *session) unblockStdin() {
	if s.stdinFile != nil {
		if err := s.stdinFile.SetReadDeadline(s.clk.Now()); err != nil {
			s.log.Debug("unblock stdin failed", "err", err)
		}
	}
}

// shutdown runs the one ordered teardown (docs/design/proto.md §9): cancel
// the session ctx, cancel and wait the active op, best-effort farewell abort
// under its own short deadline, close the conn (unblocking the reader),
// close pending calls, drain the WaitGroup, unblock stdin, enter the final
// state, and assemble the Outcome.
func (s *session) shutdown(spec finishSpec) (Outcome, error) {
	s.cancel()
	s.opMu.Lock()
	op := s.activeOp
	s.opMu.Unlock()
	if op != nil {
		op.cancel()
		<-op.done
	}
	if spec.farewell {
		s.sendFarewell(spec.farewellReason, spec.farewellStage)
	}
	s.conn.Close()
	s.closePendingCalls()
	s.wg.Wait()
	s.unblockStdin()
	if err := s.sm.Transition(spec.state); err != nil {
		s.log.Warn("final state transition rejected", "err", err)
	}
	return s.outcome(spec.reason), spec.err
}

// sendFarewell writes the best-effort abort frame bounded by
// farewellWriteTimeout: the write runs in a helper goroutine and the
// connection is closed either after the write finished or after the deadline
// — closing unblocks a stalled write (including one still holding the write
// lock ahead of the helper), so the helper always exits promptly and
// shutdown is never delayed by a dead peer (proto.md pitfall 10).
func (s *session) sendFarewell(reason, stage string) {
	done := make(chan error, 1)
	go func() {
		_, err := s.send(protocol.TypeAbort, "", protocol.Abort{
			Reason:    reason,
			Stage:     stage,
			Initiator: string(s.cfg.Role),
		})
		done <- err
	}()
	timedOut := false
	select {
	case werr := <-done:
		if werr != nil {
			s.log.Warn("farewell abort write failed", "err", werr)
		}
	case <-s.clk.After(farewellWriteTimeout):
		timedOut = true
		s.log.Warn("farewell abort write timed out")
	}
	s.conn.Close()
	if timedOut {
		<-done // returns promptly: the close unblocked the write
	}
}
