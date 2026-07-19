package peer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/protocol"
)

// Role identifies which side of the session this process plays.
type Role string

const (
	// RolePC1 is the coordinator: it listens, assigns the test ID and
	// drives the test plan.
	RolePC1 Role = "pc1"
	// RolePC2 is the worker: it dials and executes requested operations.
	RolePC2 Role = "pc2"
)

// Config carries everything a peer session needs. The zero value of every
// optional field selects a sensible default (see the accessor methods);
// required fields are Role, LocalIP, PeerIP, ControlPort, Token and Version.
type Config struct {
	// Role selects coordinator (RolePC1) or worker (RolePC2) behavior.
	Role Role
	// LocalIP is the interface-under-test address this side binds.
	LocalIP netip.Addr
	// PeerIP is the other side's address; connections from any other IP
	// are dropped without a response.
	PeerIP netip.Addr
	// ControlPort is the TCP port of the control channel.
	ControlPort uint16
	// Token is the shared secret; compared with protocol.TokenEqual and
	// never logged.
	Token string
	// NonInteractive skips the stdin prompt and auto-starts.
	NonInteractive bool
	// NoReportTransfer disables the PC1→PC2 report transfer.
	NoReportTransfer bool
	// Version is this build's cablecheck version, exchanged in the
	// handshake.
	Version string
	// Caps describes this side's tools and NIC, from preflight.
	Caps protocol.Capabilities
	// Clock supplies time; nil means the real clock.
	Clock clock.Clock
	// Logger receives session logging; nil discards.
	Logger *slog.Logger
	// Stdin feeds the interactive prompt; os.Stdin in production.
	Stdin io.Reader
	// Stdout receives operator-facing prompt output (countdown ticks,
	// status lines); nil discards. This is user interaction, not logging.
	Stdout io.Writer
	// Transport establishes the control connection; nil means real TCP.
	Transport Transport

	// Mode is the test mode announced in start_confirmation (quick,
	// standard or soak).
	Mode string
	// Steps lists display names of the planned steps, sent in
	// start_confirmation to drive the worker's "[1/8]" numbering.
	Steps []string

	// SendReports, when set on the coordinator, is invoked in its own
	// goroutine after the plan succeeds (state generating_report) to stream
	// the report files to the peer over the given ReportChannel. A nil
	// callback (or a peer without AcceptReportTransfer, or
	// NoReportTransfer) skips the transfer. Its error is a warning only.
	SendReports func(ctx context.Context, rt *ReportChannel) error
	// ReceiveReports, when set on the worker, is invoked in its own
	// goroutine when the report manifest arrives; it consumes the routed
	// report/report_chunk frames from the ReportChannel and acknowledges
	// them. A worker without the callback declines the manifest.
	ReceiveReports func(ctx context.Context, rt *ReportChannel) error
	// Complete optionally supplies the final-verdict payload for this
	// side's complete frame; nil sends a zero Complete.
	Complete func() protocol.Complete
	// OnHandshake, when set, is invoked once after a successful handshake
	// with the coordinator-assigned test ID.
	OnHandshake func(testID string)
	// OnState, when set, observes every successful session state transition.
	// It must be fast and must not re-enter the session.
	OnState func(from, to State)

	// HeartbeatInterval overrides the 5s heartbeat ticker period; zero
	// means protocol.HeartbeatInterval. Test-tunable.
	HeartbeatInterval time.Duration
	// IdleTimeout overrides the per-frame read deadline; zero means
	// protocol.DefaultIdleTimeout. Test-tunable.
	IdleTimeout time.Duration
	// CallGrace overrides how long the coordinator waits for a test_result
	// beyond the request's own TimeoutMs budget; zero means
	// defaultCallGrace. Test-tunable.
	CallGrace time.Duration

	// hooks are test-only observation points; the zero value installs none.
	hooks sessionHooks
}

// sessionHooks bundles the unexported test hooks a session honors. Every
// field may be nil; hooks must be fast and must not re-enter the session.
type sessionHooks struct {
	// onState observes every successful state transition.
	onState func(from, to State)
	// afterEvent runs after the event loop finished handling an event.
	afterEvent func(ev event)
	// afterHeartbeatTick runs after each heartbeat tick evaluation with
	// whether a heartbeat frame was written.
	afterHeartbeatTick func(sent bool)
	// beforeWrite runs inside the session write path between minting a
	// message ID and writing the frame, under the write lock.
	beforeWrite func(env *protocol.Envelope)
}

// defaultCallGrace is how long the coordinator keeps waiting for a
// test_result beyond the request's own TimeoutMs budget before declaring the
// worker wedged (docs/design/proto.md §6). It is deliberately a dedicated
// constant rather than a reuse of protocol.DefaultWriteTimeout: the two are
// only coincidentally equal, and retuning frame-write pacing must not
// silently change RPC deadlines.
const defaultCallGrace = 10 * time.Second

// clock returns the configured Clock, defaulting to the real one.
func (c Config) clock() clock.Clock {
	if c.Clock != nil {
		return c.Clock
	}
	return clock.Real{}
}

// logger returns the configured Logger, defaulting to a discard logger.
func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// stdout returns the configured Stdout, defaulting to a discard writer.
func (c Config) stdout() io.Writer {
	if c.Stdout != nil {
		return c.Stdout
	}
	return io.Discard
}

// transport returns the configured Transport, defaulting to real TCP.
func (c Config) transport() Transport {
	if c.Transport != nil {
		return c.Transport
	}
	return newTCPTransport(c.clock())
}

// heartbeatInterval returns the effective heartbeat ticker period.
func (c Config) heartbeatInterval() time.Duration {
	if c.HeartbeatInterval > 0 {
		return c.HeartbeatInterval
	}
	return protocol.HeartbeatInterval
}

// idleTimeout returns the effective per-frame read deadline.
func (c Config) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return protocol.DefaultIdleTimeout
}

// callGrace returns the effective coordinator-side RPC grace beyond the
// request's TimeoutMs.
func (c Config) callGrace() time.Duration {
	if c.CallGrace > 0 {
		return c.CallGrace
	}
	return defaultCallGrace
}

// RemoteCaller is what the test plan programs against on the coordinator:
// each Call is one test_request → test_result RPC to the worker.
type RemoteCaller interface {
	// Call sends a test_request for op and blocks until the correlated
	// test_result arrives, the timeout (plus the session's call grace)
	// expires, or the session dies. onProgress may be nil; when set it is
	// invoked from the session event loop goroutine and must not block.
	Call(ctx context.Context, op string, params any, timeout time.Duration,
		onProgress func(protocol.TestProgress)) (*protocol.TestResult, error)
	// Warn sends a non-fatal warning frame to the peer.
	Warn(code, text string)
}

// PlanFunc drives the whole test sequence on the coordinator, including
// PC1's local steps; it runs in its own goroutine owned by the session.
type PlanFunc func(ctx context.Context, rc RemoteCaller) error

// OpHandler executes one requested operation on the worker. At most one op
// runs at a time; progress must not block.
type OpHandler interface {
	// HandleOp runs op with the given raw params under ctx, reporting
	// interim progress via progress. It returns the typed result value
	// (marshaled into test_result.result), a status of "ok", "failed",
	// "timeout", "unavailable" or "rejected", and an error for the
	// non-ok statuses.
	HandleOp(ctx context.Context, op string, params json.RawMessage,
		progress func(protocol.TestProgress)) (result any, status string, err error)
}

// Outcome summarizes a finished session for the caller.
type Outcome struct {
	// FinalState is the terminal state the session reached.
	FinalState State
	// PeerComplete is the peer's complete frame, nil if never received.
	PeerComplete *protocol.Complete
	// AbortReason is the abort reason when the session aborted, else "".
	AbortReason string
	// PeerCaps is the peer's capabilities from the handshake.
	PeerCaps protocol.Capabilities
	// TestID is the session's assigned test ID, "" if the handshake never
	// completed.
	TestID string
}
