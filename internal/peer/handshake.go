package peer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/protocol"
)

// Abort reasons used during the handshake (and reused by the session).
const (
	abortAuthFailed        = "auth_failed"
	abortVersionMismatch   = "version_mismatch"
	abortProtocolError     = "protocol_error"
	abortCapabilityMissing = "capability_missing"
)

// stageHandshake is the Abort.Stage value for handshake-time aborts.
const stageHandshake = "handshake"

// Typed handshake failures. The session layer maps them to the retry policy
// (coordinator keeps listening after retryable failures, three auth strikes
// allowed) and to exit codes (worker token rejection is a config error).
var (
	// errAuthFailed: the connecting peer presented a wrong token
	// (coordinator side). Retryable; the session enforces three strikes.
	errAuthFailed = errors.New("peer: handshake auth failed: peer presented a wrong token")
	// errTokenRejected: our token was rejected by the coordinator (worker
	// side). A local configuration error, never retried.
	errTokenRejected = errors.New("peer: token rejected by coordinator")
	// errVersionMismatch: the peers speak different protocol versions.
	// Fatal on both sides.
	errVersionMismatch = errors.New("peer: protocol version mismatch")
	// errCapabilityMissing: a required capability (iperf3 JSON output) is
	// absent on one side. Fatal on both sides.
	errCapabilityMissing = errors.New("peer: required capability missing")
	// errProtocol: the peer violated the handshake sequence (wrong frame
	// type, malformed frame, bad role or IP claim, dead connection).
	// Retryable on the coordinator.
	errProtocol = errors.New("peer: handshake protocol error")
	// errHandshakeTimeout: the handshake exceeded its budget. Retryable on
	// the coordinator (the session allows one retry, then exits).
	errHandshakeTimeout = errors.New("peer: handshake timed out")
	// errPeerAborted: the peer aborted (or vanished) during the handshake
	// for a reason other than the specific ones above.
	errPeerAborted = errors.New("peer: peer aborted during handshake")
)

// handshakeRetryable reports whether a coordinator-side handshake failure
// leaves the listener usable for another attempt. Version and capability
// mismatches and worker-side failures are fatal.
func handshakeRetryable(err error) bool {
	return errors.Is(err, errAuthFailed) ||
		errors.Is(err, errProtocol) ||
		errors.Is(err, errHandshakeTimeout)
}

// handshakeResult is what a successful handshake half hands to the session.
type handshakeResult struct {
	// TestID is the session ID assigned by the coordinator; every later
	// envelope carries it.
	TestID string
	// PeerVersion is the peer's cablecheck build version.
	PeerVersion string
	// PeerCaps is the peer's capability announcement.
	PeerCaps protocol.Capabilities
	// IDs is the message-ID maker used during the handshake; the session
	// continues it so IDs stay monotonic across the connection.
	IDs *protocol.MessageIDMaker
	// Dedup is the duplicate detector already fed with the handshake
	// frames; the session keeps using it.
	Dedup *protocol.DuplicateDetector
}

// newTestID mints a session test ID: "ct-YYYYMMDD-HHMMSS-<8hex>" with the
// timestamp from clk (UTC) and 4 bytes from crypto/rand.
func newTestID(clk clock.Clock) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("peer: generate test ID: %w", err)
	}
	return "ct-" + clk.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// sendAbort writes a best-effort abort frame; write failures are returned
// for logging only — the caller is already failing the handshake.
func sendAbort(conn *protocol.Conn, ids *protocol.MessageIDMaker, cfg Config, testID, reason, detail string) error {
	env, err := protocol.NewEnvelope(protocol.TypeAbort, testID, ids.Next(), protocol.Abort{
		Reason:    reason,
		Stage:     stageHandshake,
		Detail:    detail,
		Initiator: string(cfg.Role),
	})
	if err != nil {
		return err
	}
	return conn.WriteEnvelope(env)
}

// readHandshakeFrame reads one frame with the connection's read deadline
// capped at the absolute handshake deadline, and runs the duplicate check.
// Deadline expiry (before or during the read) maps to errHandshakeTimeout;
// duplicates are protocol errors — the handshake sequence is strict.
func readHandshakeFrame(conn *protocol.Conn, clk clock.Clock, deadline time.Time, dedup *protocol.DuplicateDetector) (*protocol.Envelope, error) {
	remaining := deadline.Sub(clk.Now())
	if remaining <= 0 {
		return nil, errHandshakeTimeout
	}
	conn.SetIdleTimeout(remaining)
	env, err := conn.ReadEnvelope()
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", errHandshakeTimeout, err)
		}
		return nil, err
	}
	if ok, reason := dedup.Observe(env.MessageID); !ok {
		return nil, fmt.Errorf("%w: %s", errProtocol, reason)
	}
	return env, nil
}

// versionDetail formats the "ours=X theirs=Y" abort detail for a protocol
// version mismatch.
func versionDetail(theirs string) string {
	return fmt.Sprintf("ours=%s theirs=%s", protocol.Version, theirs)
}

// checkAssignedTestID enforces the post-hello_ack rule: an envelope carrying
// a non-empty test ID different from the assigned one is a protocol error.
func checkAssignedTestID(env *protocol.Envelope, testID string) error {
	if env.TestID != "" && env.TestID != testID {
		return fmt.Errorf("%w: envelope testId %q does not match assigned %q", errProtocol, env.TestID, testID)
	}
	return nil
}

// requireIperf3JSON enforces the one required capability: iperf3 JSON output
// on both sides.
func requireIperf3JSON(caps protocol.Capabilities) error {
	if !caps.Iperf3.JSON {
		return fmt.Errorf("%w: peer iperf3 (version %q) lacks JSON output support", errCapabilityMissing, caps.Iperf3.Version)
	}
	return nil
}

// helloPeerIPMatches validates the hello's peerIp claim against our local
// IP, tolerating IPv4-in-IPv6 mapping.
func helloPeerIPMatches(claim string, localIP netip.Addr) bool {
	a, err := netip.ParseAddr(claim)
	return err == nil && a.Unmap() == localIP.Unmap()
}

// helloLocalIPMatches validates the worker's claimed local interface address
// against the coordinator's configured peer address.
func helloLocalIPMatches(claim string, peerIP netip.Addr) bool {
	a, err := netip.ParseAddr(claim)
	return err == nil && a.Unmap() == peerIP.Unmap()
}

// coordinatorHandshake runs PC1's half of the handshake on an accepted (and
// peer-IP-verified) connection: read hello, validate version/token/role/IP
// claim, assign the test ID, answer hello_ack, read the worker's
// capabilities, check required capabilities, send our own. It restores the
// connection's idle timeout to the session value before returning
// successfully. Failures close the connection and return the typed errors
// above after a best-effort abort frame; handshakeRetryable tells the caller
// whether the listener is still usable (docs/design/proto.md §3 failure
// table).
func coordinatorHandshake(conn *protocol.Conn, cfg Config) (res *handshakeResult, err error) {
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	clk := cfg.clock()
	log := cfg.logger()
	ids := protocol.NewMessageIDMaker(string(cfg.Role))
	dedup := protocol.NewDuplicateDetector()
	deadline := clk.Now().Add(protocol.HandshakeTimeout)

	// The first frame must arrive within HelloTimeout of the accept.
	helloDeadline := clk.Now().Add(protocol.HelloTimeout)
	if helloDeadline.After(deadline) {
		helloDeadline = deadline
	}
	env, err := readHandshakeFrame(conn, clk, helloDeadline, dedup)
	if err != nil {
		// Hello timeout and frame errors alike: abort(protocol_error)
		// best-effort, keep listening.
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, "expected hello as first frame")
		if errors.Is(err, errHandshakeTimeout) || errors.Is(err, errProtocol) {
			return nil, fmt.Errorf("%w: reading hello", err)
		}
		return nil, fmt.Errorf("%w: reading hello: %v", errProtocol, err)
	}
	if env.Type != protocol.TypeHello {
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, fmt.Sprintf("expected hello, got %s", env.Type))
		return nil, fmt.Errorf("%w: first frame is %s, want hello", errProtocol, env.Type)
	}
	if protocol.CheckVersion(env) != nil {
		detail := versionDetail(env.ProtocolVersion)
		_ = sendAbort(conn, ids, cfg, "", abortVersionMismatch, detail)
		return nil, fmt.Errorf("%w (%s)", errVersionMismatch, detail)
	}
	hello, err := protocol.DecodePayload[protocol.Hello](env)
	if err != nil {
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, "malformed hello payload")
		return nil, fmt.Errorf("%w: %v", errProtocol, err)
	}
	if !protocol.TokenEqual(hello.Token, cfg.Token) {
		// No detail, ever: nothing that could help guess the token.
		_ = sendAbort(conn, ids, cfg, "", abortAuthFailed, "")
		log.Warn("handshake auth failed: peer presented a wrong token")
		return nil, errAuthFailed
	}
	if hello.Role != string(RolePC2) {
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, fmt.Sprintf("unexpected role %q", hello.Role))
		return nil, fmt.Errorf("%w: hello role %q, want %q", errProtocol, hello.Role, RolePC2)
	}
	if !helloPeerIPMatches(hello.PeerIP, cfg.LocalIP) {
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, fmt.Sprintf("hello peerIp %q does not match our local IP", hello.PeerIP))
		return nil, fmt.Errorf("%w: hello peerIp %q does not match local IP %s", errProtocol, hello.PeerIP, cfg.LocalIP)
	}
	if !helloLocalIPMatches(hello.LocalIP, cfg.PeerIP) {
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, fmt.Sprintf("hello localIp %q does not match expected peer IP", hello.LocalIP))
		return nil, fmt.Errorf("%w: hello localIp %q does not match expected peer IP %s", errProtocol, hello.LocalIP, cfg.PeerIP)
	}

	testID, err := newTestID(clk)
	if err != nil {
		return nil, err
	}
	ack, err := protocol.NewEnvelope(protocol.TypeHelloAck, testID, ids.Next(), protocol.HelloAck{
		TestID:            testID,
		CablecheckVersion: cfg.Version,
	})
	if err != nil {
		return nil, err
	}
	ack.InReplyTo = env.MessageID
	if err := conn.WriteEnvelope(ack); err != nil {
		return nil, fmt.Errorf("%w: writing hello_ack: %v", errProtocol, err)
	}

	// Worker sends its capabilities first.
	capsEnv, err := readHandshakeFrame(conn, clk, deadline, dedup)
	if err != nil {
		if errors.Is(err, errHandshakeTimeout) {
			return nil, fmt.Errorf("%w: waiting for capabilities", err)
		}
		_ = sendAbort(conn, ids, cfg, testID, abortProtocolError, "reading capabilities failed")
		return nil, fmt.Errorf("%w: reading capabilities: %v", errProtocol, err)
	}
	if verr := protocol.CheckVersion(capsEnv); verr != nil {
		detail := versionDetail(capsEnv.ProtocolVersion)
		_ = sendAbort(conn, ids, cfg, testID, abortVersionMismatch, detail)
		return nil, fmt.Errorf("%w (%s)", errVersionMismatch, detail)
	}
	if capsEnv.Type != protocol.TypeCapabilities {
		_ = sendAbort(conn, ids, cfg, testID, abortProtocolError, fmt.Sprintf("expected capabilities, got %s", capsEnv.Type))
		return nil, fmt.Errorf("%w: expected capabilities, got %s", errProtocol, capsEnv.Type)
	}
	if err := checkAssignedTestID(capsEnv, testID); err != nil {
		_ = sendAbort(conn, ids, cfg, testID, abortProtocolError, "capabilities carry a foreign testId")
		return nil, err
	}
	peerCaps, err := protocol.DecodePayload[protocol.Capabilities](capsEnv)
	if err != nil {
		_ = sendAbort(conn, ids, cfg, testID, abortProtocolError, "malformed capabilities payload")
		return nil, fmt.Errorf("%w: %v", errProtocol, err)
	}
	if err := requireIperf3JSON(*peerCaps); err != nil {
		_ = sendAbort(conn, ids, cfg, testID, abortCapabilityMissing, err.Error())
		return nil, err
	}

	ourCaps, err := protocol.NewEnvelope(protocol.TypeCapabilities, testID, ids.Next(), cfg.Caps)
	if err != nil {
		return nil, err
	}
	if err := conn.WriteEnvelope(ourCaps); err != nil {
		return nil, fmt.Errorf("%w: writing capabilities: %v", errProtocol, err)
	}

	conn.SetIdleTimeout(cfg.idleTimeout())
	log.Info("handshake complete", "testId", testID, "peerVersion", hello.CablecheckVersion)
	return &handshakeResult{
		TestID:      testID,
		PeerVersion: hello.CablecheckVersion,
		PeerCaps:    *peerCaps,
		IDs:         ids,
		Dedup:       dedup,
	}, nil
}

// mapWorkerAbort converts an abort frame received during the worker's
// handshake into the matching typed error.
func mapWorkerAbort(env *protocol.Envelope) error {
	ab, err := protocol.DecodePayload[protocol.Abort](env)
	if err != nil {
		return fmt.Errorf("%w: malformed abort during handshake: %v", errProtocol, err)
	}
	switch ab.Reason {
	case abortAuthFailed:
		return errTokenRejected
	case abortVersionMismatch:
		return fmt.Errorf("%w (%s)", errVersionMismatch, ab.Detail)
	case abortCapabilityMissing:
		return fmt.Errorf("%w: %s", errCapabilityMissing, ab.Detail)
	default:
		return fmt.Errorf("%w: reason %q, detail %q", errPeerAborted, ab.Reason, ab.Detail)
	}
}

// workerReadReply reads the coordinator's next handshake frame, mapping
// abort frames, version mismatches (answered with our own abort) and wrong
// frame types to typed errors. wantType is the expected message type.
func workerReadReply(conn *protocol.Conn, cfg Config, clk clock.Clock, deadline time.Time, dedup *protocol.DuplicateDetector, ids *protocol.MessageIDMaker, testID string, wantType protocol.MessageType) (*protocol.Envelope, error) {
	env, err := readHandshakeFrame(conn, clk, deadline, dedup)
	if err != nil {
		if errors.Is(err, errHandshakeTimeout) || errors.Is(err, errProtocol) {
			return nil, fmt.Errorf("%w: waiting for %s", err, wantType)
		}
		// EOF/reset before any reply: the peer is gone.
		return nil, fmt.Errorf("%w: connection failed waiting for %s: %v", errPeerAborted, wantType, err)
	}
	if env.Type == protocol.TypeAbort {
		return nil, mapWorkerAbort(env)
	}
	if verr := protocol.CheckVersion(env); verr != nil {
		detail := versionDetail(env.ProtocolVersion)
		_ = sendAbort(conn, ids, cfg, testID, abortVersionMismatch, detail)
		return nil, fmt.Errorf("%w (%s)", errVersionMismatch, detail)
	}
	if env.Type != wantType {
		_ = sendAbort(conn, ids, cfg, testID, abortProtocolError, fmt.Sprintf("expected %s, got %s", wantType, env.Type))
		return nil, fmt.Errorf("%w: expected %s, got %s", errProtocol, wantType, env.Type)
	}
	return env, nil
}

// workerHandshake runs PC2's half of the handshake on a freshly dialed
// connection: send hello, await hello_ack (or a typed abort), send our
// capabilities under the assigned test ID, await the coordinator's
// capabilities and check the required ones. It restores the connection's
// idle timeout to the session value before returning successfully. All
// worker-side failures close the connection and are fatal for the process;
// none are retryable.
func workerHandshake(conn *protocol.Conn, cfg Config) (res *handshakeResult, err error) {
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	clk := cfg.clock()
	log := cfg.logger()
	ids := protocol.NewMessageIDMaker(string(cfg.Role))
	dedup := protocol.NewDuplicateDetector()
	deadline := clk.Now().Add(protocol.HandshakeTimeout)

	hello, err := protocol.NewEnvelope(protocol.TypeHello, "", ids.Next(), protocol.Hello{
		Token:             cfg.Token,
		Role:              string(RolePC2),
		CablecheckVersion: cfg.Version,
		LocalIP:           cfg.LocalIP.String(),
		PeerIP:            cfg.PeerIP.String(),
	})
	if err != nil {
		return nil, err
	}
	if err := conn.WriteEnvelope(hello); err != nil {
		return nil, fmt.Errorf("%w: writing hello: %v", errPeerAborted, err)
	}

	ackEnv, err := workerReadReply(conn, cfg, clk, deadline, dedup, ids, "", protocol.TypeHelloAck)
	if err != nil {
		return nil, err
	}
	ack, err := protocol.DecodePayload[protocol.HelloAck](ackEnv)
	if err != nil {
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, "malformed hello_ack payload")
		return nil, fmt.Errorf("%w: %v", errProtocol, err)
	}
	if ack.TestID == "" {
		_ = sendAbort(conn, ids, cfg, "", abortProtocolError, "hello_ack carries no testId")
		return nil, fmt.Errorf("%w: hello_ack carries no testId", errProtocol)
	}
	testID := ack.TestID

	// Worker sends its capabilities first, then reads the coordinator's.
	ourCaps, err := protocol.NewEnvelope(protocol.TypeCapabilities, testID, ids.Next(), cfg.Caps)
	if err != nil {
		return nil, err
	}
	if err := conn.WriteEnvelope(ourCaps); err != nil {
		return nil, fmt.Errorf("%w: writing capabilities: %v", errPeerAborted, err)
	}

	capsEnv, err := workerReadReply(conn, cfg, clk, deadline, dedup, ids, testID, protocol.TypeCapabilities)
	if err != nil {
		return nil, err
	}
	if err := checkAssignedTestID(capsEnv, testID); err != nil {
		_ = sendAbort(conn, ids, cfg, testID, abortProtocolError, "capabilities carry a foreign testId")
		return nil, err
	}
	peerCaps, err := protocol.DecodePayload[protocol.Capabilities](capsEnv)
	if err != nil {
		_ = sendAbort(conn, ids, cfg, testID, abortProtocolError, "malformed capabilities payload")
		return nil, fmt.Errorf("%w: %v", errProtocol, err)
	}
	if err := requireIperf3JSON(*peerCaps); err != nil {
		_ = sendAbort(conn, ids, cfg, testID, abortCapabilityMissing, err.Error())
		return nil, err
	}

	conn.SetIdleTimeout(cfg.idleTimeout())
	log.Info("handshake complete", "testId", testID, "peerVersion", ack.CablecheckVersion)
	return &handshakeResult{
		TestID:      testID,
		PeerVersion: ack.CablecheckVersion,
		PeerCaps:    *peerCaps,
		IDs:         ids,
		Dedup:       dedup,
	}, nil
}
