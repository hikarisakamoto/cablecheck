package peer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"regexp"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// testToken is the shared secret both testConfigs sides agree on.
const testToken = "sekrit-token-0001"

// pipeTransport is a Transport backed by net.Pipe: Dial hands the server end
// of a fresh pipe to the pending listener and returns the client end.
// Accepted conns report remote as their address so the session's peer-IP
// verification (which net.Pipe's "pipe" address would fail) passes.
type pipeTransport struct {
	pending chan net.Conn
	remote  string
}

func newPipeTransport() *pipeTransport {
	return &pipeTransport{pending: make(chan net.Conn, 4), remote: "192.168.1.2:40000"}
}

// Listen implements Transport.
func (p *pipeTransport) Listen(ctx context.Context, addr string) (net.Listener, error) {
	return &pipeListener{tr: p, ctx: ctx, addr: addr}, nil
}

// Dial implements Transport.
func (p *pipeTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case p.pending <- server:
		return client, nil
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	}
}

type pipeListener struct {
	tr   *pipeTransport
	ctx  context.Context
	addr string
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.tr.pending:
		return &addrConn{Conn: c, remote: l.tr.remote}, nil
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}
}

func (l *pipeListener) Close() error   { return nil }
func (l *pipeListener) Addr() net.Addr { return fakeAddr(l.addr) }

// fakeAddr is a net.Addr with a fixed string form.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// testConfigs returns a matched coordinator/worker Config pair sharing token
// and a fixed fake clock.
func testConfigs(t *testing.T) (cfg1, cfg2 Config) {
	t.Helper()
	fc := clocktest.New(time.Date(2026, 7, 17, 3, 4, 5, 0, time.UTC))
	cfg1 = Config{
		Role:        RolePC1,
		LocalIP:     netip.MustParseAddr("192.168.1.1"),
		PeerIP:      netip.MustParseAddr("192.168.1.2"),
		ControlPort: 8443,
		Token:       testToken,
		Version:     "0.1.0-pc1",
		Caps: protocol.Capabilities{
			CablecheckVersion: "0.1.0-pc1",
			OS:                "linux/amd64",
			Iperf3:            protocol.Iperf3Caps{Version: "3.16", JSON: true, Reverse: true, Bidir: true, UDP: true, OneOff: true},
			NIC:               protocol.NICInfo{Name: "enp1s0", Driver: "e1000e", SpeedMbps: 1000, Duplex: "full", MTU: 1500},
		},
		Clock: fc,
	}
	cfg2 = Config{
		Role:        RolePC2,
		LocalIP:     netip.MustParseAddr("192.168.1.2"),
		PeerIP:      netip.MustParseAddr("192.168.1.1"),
		ControlPort: 8443,
		Token:       testToken,
		Version:     "0.1.0-pc2",
		Caps: protocol.Capabilities{
			CablecheckVersion: "0.1.0-pc2",
			OS:                "linux/arm64",
			Iperf3:            protocol.Iperf3Caps{Version: "3.9", JSON: true, Reverse: true, UDP: true, OneOff: true},
			NIC:               protocol.NICInfo{Name: "enp2s0", Driver: "r8169", SpeedMbps: 1000, Duplex: "full", MTU: 1500},
		},
		Clock: fc,
	}
	return cfg1, cfg2
}

// hsOut carries one handshake half's outcome across goroutines.
type hsOut struct {
	res *handshakeResult
	err error
}

var testIDPattern = regexp.MustCompile(`^ct-20260717-030405-[0-9a-f]{8}$`)

// TestHandshakeHappyPath runs both handshake halves concurrently over the
// pipe Transport and asserts the test ID and capabilities exchange.
func TestHandshakeHappyPath(t *testing.T) {
	testutil.LeakCheck(t)
	cfg1, cfg2 := testConfigs(t)
	tr := newPipeTransport()
	cfg1.Transport = tr
	cfg2.Transport = tr
	ctx := t.Context()

	coord := make(chan hsOut, 1)
	go func() {
		ln, err := tr.Listen(ctx, "192.168.1.1:8443")
		if err != nil {
			coord <- hsOut{err: err}
			return
		}
		nc, err := ln.Accept()
		if err != nil {
			coord <- hsOut{err: err}
			return
		}
		conn := protocol.NewConn(nc)
		defer conn.Close()
		res, err := coordinatorHandshake(conn, cfg1)
		coord <- hsOut{res: res, err: err}
	}()

	nc, err := tr.Dial(ctx, "192.168.1.1:8443")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn := protocol.NewConn(nc)
	defer conn.Close()
	res2, err2 := workerHandshake(conn, cfg2)
	out1 := <-coord

	if out1.err != nil {
		t.Fatalf("coordinatorHandshake: %v", out1.err)
	}
	if err2 != nil {
		t.Fatalf("workerHandshake: %v", err2)
	}
	res1 := out1.res
	if !testIDPattern.MatchString(res1.TestID) {
		t.Errorf("coordinator TestID %q does not match %v", res1.TestID, testIDPattern)
	}
	if res1.TestID != res2.TestID {
		t.Errorf("test IDs disagree: coordinator %q, worker %q", res1.TestID, res2.TestID)
	}
	if res1.PeerCaps != cfg2.Caps {
		t.Errorf("coordinator PeerCaps = %+v, want worker caps %+v", res1.PeerCaps, cfg2.Caps)
	}
	if res2.PeerCaps != cfg1.Caps {
		t.Errorf("worker PeerCaps = %+v, want coordinator caps %+v", res2.PeerCaps, cfg1.Caps)
	}
	if res1.PeerVersion != cfg2.Version {
		t.Errorf("coordinator PeerVersion = %q, want %q", res1.PeerVersion, cfg2.Version)
	}
	if res2.PeerVersion != cfg1.Version {
		t.Errorf("worker PeerVersion = %q, want %q", res2.PeerVersion, cfg1.Version)
	}
	if res1.IDs == nil || res2.IDs == nil {
		t.Error("handshake results must carry the session MessageIDMaker")
	}
}

// scriptConn is a synchronous net.Conn stub for handshake failure tests. Read
// serves a fixed sequence of pre-encoded inbound frames and, once they are
// consumed, fails with exhaustErr — os.ErrDeadlineExceeded simulates the read
// deadline expiring (readHandshakeFrame maps it to errHandshakeTimeout),
// io.EOF simulates the peer vanishing. Write captures every outbound frame
// for later decoding. Everything runs on the test goroutine: no pipes, no
// helper goroutines, no real deadlines, so the tests are instant and
// deterministic.
type scriptConn struct {
	rd         *bytes.Reader
	exhaustErr error
	wrote      bytes.Buffer
	reads      int
	closed     bool
}

// newScriptConn queues frames for reading and installs exhaustErr as the
// error once they run out.
func newScriptConn(t *testing.T, exhaustErr error, frames ...*protocol.Envelope) *scriptConn {
	t.Helper()
	var buf bytes.Buffer
	for _, env := range frames {
		buf.Write(encodeFrame(t, env))
	}
	return &scriptConn{rd: bytes.NewReader(buf.Bytes()), exhaustErr: exhaustErr}
}

// Read implements net.Conn: scripted frames, then exhaustErr.
func (c *scriptConn) Read(p []byte) (int, error) {
	c.reads++
	if n, _ := c.rd.Read(p); n > 0 {
		return n, nil
	}
	return 0, c.exhaustErr
}

// Write implements net.Conn by capturing outbound bytes.
func (c *scriptConn) Write(p []byte) (int, error) { return c.wrote.Write(p) }

// Close implements net.Conn.
func (c *scriptConn) Close() error { c.closed = true; return nil }

// LocalAddr implements net.Conn.
func (c *scriptConn) LocalAddr() net.Addr { return fakeAddr("script-local") }

// RemoteAddr implements net.Conn.
func (c *scriptConn) RemoteAddr() net.Addr { return fakeAddr("script-remote") }

// SetDeadline implements net.Conn; deadlines are ignored — timeout behavior
// is injected via exhaustErr instead.
func (c *scriptConn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline implements net.Conn (ignored, see SetDeadline).
func (c *scriptConn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline implements net.Conn (ignored, see SetDeadline).
func (c *scriptConn) SetWriteDeadline(time.Time) error { return nil }

// encodeFrame renders env in the wire format (4-byte big-endian length
// prefix + JSON body) for a scriptConn's inbound queue.
func encodeFrame(t *testing.T, env *protocol.Envelope) []byte {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	return buf
}

// decodeFrames parses the concatenated frames captured by scriptConn.Write.
func decodeFrames(t *testing.T, raw []byte) []*protocol.Envelope {
	t.Helper()
	var envs []*protocol.Envelope
	for len(raw) > 0 {
		if len(raw) < 4 {
			t.Fatalf("decodeFrames: %d stray trailing bytes", len(raw))
		}
		n := int(binary.BigEndian.Uint32(raw))
		if len(raw)-4 < n {
			t.Fatalf("decodeFrames: truncated frame: declared %d, have %d", n, len(raw)-4)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw[4:4+n], &env); err != nil {
			t.Fatalf("decodeFrames: %v", err)
		}
		envs = append(envs, &env)
		raw = raw[4+n:]
	}
	return envs
}

// mustEnvelope builds an envelope or fails the test.
func mustEnvelope(t *testing.T, typ protocol.MessageType, testID, msgID string, payload any) *protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(typ, testID, msgID, payload)
	if err != nil {
		t.Fatalf("NewEnvelope(%s): %v", typ, err)
	}
	return env
}

// helloFrame builds the worker's opening hello, valid for testConfigs' cfg1;
// mut, when non-nil, bends one field for a failure case.
func helloFrame(t *testing.T, mut func(*protocol.Hello)) *protocol.Envelope {
	t.Helper()
	h := protocol.Hello{
		Token: testToken, Role: "pc2", CablecheckVersion: "0.1.0-pc2",
		LocalIP: "192.168.1.2", PeerIP: "192.168.1.1",
	}
	if mut != nil {
		mut(&h)
	}
	return mustEnvelope(t, protocol.TypeHello, "", "pc2-00000001", h)
}

// helloAckFrame builds the coordinator's hello_ack assigning testID.
func helloAckFrame(t *testing.T, testID string) *protocol.Envelope {
	t.Helper()
	return mustEnvelope(t, protocol.TypeHelloAck, testID, "pc1-00000001", protocol.HelloAck{
		TestID: testID, CablecheckVersion: "0.1.0-pc1",
	})
}

// capsFrame builds a capabilities frame; jsonOK selects whether the sender's
// iperf3 supports JSON output (the one required capability).
func capsFrame(t *testing.T, testID, msgID string, jsonOK bool) *protocol.Envelope {
	t.Helper()
	return mustEnvelope(t, protocol.TypeCapabilities, testID, msgID, protocol.Capabilities{
		CablecheckVersion: "0.1.0-peer",
		OS:                "linux/amd64",
		Iperf3:            protocol.Iperf3Caps{Version: "3.16", JSON: jsonOK},
	})
}

// abortFrame builds a coordinator-initiated handshake abort.
func abortFrame(t *testing.T, reason, detail string) *protocol.Envelope {
	t.Helper()
	return mustEnvelope(t, protocol.TypeAbort, "", "pc1-00000001", protocol.Abort{
		Reason: reason, Stage: stageHandshake, Detail: detail, Initiator: "pc1",
	})
}

// junkPayload replaces env's payload with JSON that cannot decode into any
// payload struct.
func junkPayload(env *protocol.Envelope) *protocol.Envelope {
	env.Payload = json.RawMessage(`[]`)
	return env
}

// strp returns a pointer to s, for hsCase.wantAbortDetail.
func strp(s string) *string { return &s }

// hsCase is one row of the handshake failure table (docs/design/proto.md §3):
// a script of inbound peer frames plus everything the session layer builds
// on — the typed error, its handshakeRetryable classification, and the abort
// frame (if any) sent to the peer.
type hsCase struct {
	name string
	// worker runs workerHandshake against a scripted coordinator instead of
	// coordinatorHandshake against a scripted worker.
	worker bool
	// frames are served to the handshake in order; nil means the peer never
	// sends anything.
	frames func(t *testing.T) []*protocol.Envelope
	// exhaust is the read error once the script runs dry; nil means
	// os.ErrDeadlineExceeded (the read deadline expiring).
	exhaust error
	// wantErr is the expected typed error (matched with errors.Is).
	wantErr error
	// retryable is the expected handshakeRetryable classification. The
	// session consults it on the coordinator only; for worker rows it
	// merely pins the error value's classification.
	retryable bool
	// wantAbortReason is the abort the scripted peer must receive; ""
	// asserts that no abort frame is sent at all.
	wantAbortReason string
	// wantAbortDetail, when non-nil, is the exact required abort detail.
	wantAbortDetail *string
}

// run executes the row and asserts every expectation.
func (tc hsCase) run(t *testing.T) {
	cfg1, cfg2 := testConfigs(t)
	cfg, handshake := cfg1, coordinatorHandshake
	if tc.worker {
		cfg, handshake = cfg2, workerHandshake
	}
	exhaust := tc.exhaust
	if exhaust == nil {
		exhaust = os.ErrDeadlineExceeded
	}
	var frames []*protocol.Envelope
	if tc.frames != nil {
		frames = tc.frames(t)
	}
	sc := newScriptConn(t, exhaust, frames...)

	res, err := handshake(protocol.NewConn(sc), cfg)

	if res != nil {
		t.Errorf("handshake result = %+v, want nil", res)
	}
	if !errors.Is(err, tc.wantErr) {
		t.Errorf("handshake error = %v, want %v", err, tc.wantErr)
	}
	if got := handshakeRetryable(err); got != tc.retryable {
		t.Errorf("handshakeRetryable(%v) = %t, want %t", err, got, tc.retryable)
	}
	if !sc.closed {
		t.Error("a failed handshake must close the connection")
	}

	var abort *protocol.Envelope
	for _, env := range decodeFrames(t, sc.wrote.Bytes()) {
		if env.Type == protocol.TypeAbort {
			abort = env
		}
	}
	if tc.wantAbortReason == "" {
		if abort != nil {
			t.Errorf("unexpected abort frame sent: %s", abort.Payload)
		}
		return
	}
	if abort == nil {
		t.Fatalf("no abort frame sent, want abort(%s)", tc.wantAbortReason)
	}
	ab, derr := protocol.DecodePayload[protocol.Abort](abort)
	if derr != nil {
		t.Fatalf("decode sent abort: %v", derr)
	}
	if ab.Reason != tc.wantAbortReason {
		t.Errorf("abort reason = %q, want %q", ab.Reason, tc.wantAbortReason)
	}
	if tc.wantAbortDetail != nil && ab.Detail != *tc.wantAbortDetail {
		t.Errorf("abort detail = %q, want %q", ab.Detail, *tc.wantAbortDetail)
	}
	if ab.Stage != stageHandshake {
		t.Errorf("abort stage = %q, want %q", ab.Stage, stageHandshake)
	}
	if ab.Initiator != string(cfg.Role) {
		t.Errorf("abort initiator = %q, want %q", ab.Initiator, cfg.Role)
	}
}

// TestHandshakeTokenMismatch asserts the coordinator answers a wrong token
// with abort(auth_failed) carrying no detail and reports a retryable
// rejection (the 3-strikes policy lives in the session), while the worker
// maps the same abort to a fatal config error.
func TestHandshakeTokenMismatch(t *testing.T) {
	for _, tc := range []hsCase{
		{
			name: "coordinator-rejects",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloFrame(t, func(h *protocol.Hello) { h.Token = "totally-wrong" })}
			},
			wantErr:         errAuthFailed,
			retryable:       true,
			wantAbortReason: abortAuthFailed,
			wantAbortDetail: strp(""), // never anything that could help guess the token
		},
		{
			name:   "worker-config-error",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{abortFrame(t, abortAuthFailed, "")}
			},
			wantErr:   errTokenRejected,
			retryable: false,
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestHandshakeVersionMismatch asserts a protocol-version mismatch aborts
// with the "ours=... theirs=..." detail and is fatal (non-retryable) on both
// sides, whichever frame reveals it.
func TestHandshakeVersionMismatch(t *testing.T) {
	for _, tc := range []hsCase{
		{
			name: "coordinator-detects-in-hello",
			frames: func(t *testing.T) []*protocol.Envelope {
				env := helloFrame(t, nil)
				env.ProtocolVersion = "2" // future peer
				return []*protocol.Envelope{env}
			},
			wantErr:         errVersionMismatch,
			retryable:       false,
			wantAbortReason: abortVersionMismatch,
			wantAbortDetail: strp("ours=1 theirs=2"),
		},
		{
			name: "coordinator-detects-in-capabilities",
			frames: func(t *testing.T) []*protocol.Envelope {
				caps := capsFrame(t, "", "pc2-00000002", true)
				caps.ProtocolVersion = "2"
				return []*protocol.Envelope{helloFrame(t, nil), caps}
			},
			wantErr:         errVersionMismatch,
			retryable:       false,
			wantAbortReason: abortVersionMismatch,
			wantAbortDetail: strp("ours=1 theirs=2"),
		},
		{
			name:   "worker-receives-abort",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{abortFrame(t, abortVersionMismatch, "ours=1 theirs=2")}
			},
			wantErr:   errVersionMismatch,
			retryable: false,
		},
		{
			name:   "worker-detects-in-hello-ack",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				env := helloAckFrame(t, "ct-x")
				env.ProtocolVersion = "0" // stale coordinator
				return []*protocol.Envelope{env}
			},
			wantErr:         errVersionMismatch,
			retryable:       false,
			wantAbortReason: abortVersionMismatch,
			wantAbortDetail: strp("ours=1 theirs=0"),
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestHandshakeCapabilityMissing asserts a peer without iperf3 JSON output
// support is fatal on both sides: abort(capability_missing) is sent by
// whichever side detects it, and the failure is never retryable.
func TestHandshakeCapabilityMissing(t *testing.T) {
	for _, tc := range []hsCase{
		{
			name: "coordinator-detects",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloFrame(t, nil), capsFrame(t, "", "pc2-00000002", false)}
			},
			wantErr:         errCapabilityMissing,
			retryable:       false,
			wantAbortReason: abortCapabilityMissing,
		},
		{
			name:   "worker-detects",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloAckFrame(t, "ct-x"), capsFrame(t, "ct-x", "pc1-00000002", false)}
			},
			wantErr:         errCapabilityMissing,
			retryable:       false,
			wantAbortReason: abortCapabilityMissing,
		},
		{
			name:   "worker-receives-abort",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{abortFrame(t, abortCapabilityMissing, "iperf3 lacks JSON output")}
			},
			wantErr:   errCapabilityMissing,
			retryable: false,
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestHandshakeTimeout asserts deadline expiry maps to errHandshakeTimeout at
// both coordinator stages (hello wait and capabilities wait) and on the
// worker, with the abort behavior of the proto.md §3 failure table: hello
// timeout answers abort(protocol_error) best-effort; a later stall just
// closes.
func TestHandshakeTimeout(t *testing.T) {
	for _, tc := range []hsCase{
		{
			name:            "coordinator-hello-timeout",
			wantErr:         errHandshakeTimeout,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("expected hello as first frame"),
		},
		{
			name: "coordinator-capabilities-timeout",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloFrame(t, nil)}
			},
			wantErr:   errHandshakeTimeout,
			retryable: true,
			// No abort: past the hello stage a timeout just closes.
		},
		{
			name:      "worker-hello-ack-timeout",
			worker:    true,
			wantErr:   errHandshakeTimeout,
			retryable: true, // classification only; the worker session never retries
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestReadHandshakeFrameDeadlineAlreadyPassed pins the pre-read branch of
// readHandshakeFrame: an already-expired absolute deadline fails with
// errHandshakeTimeout without touching the connection.
func TestReadHandshakeFrameDeadlineAlreadyPassed(t *testing.T) {
	fc := clocktest.New(time.Date(2026, 7, 17, 3, 4, 5, 0, time.UTC))
	sc := newScriptConn(t, os.ErrDeadlineExceeded)
	_, err := readHandshakeFrame(protocol.NewConn(sc), fc, fc.Now().Add(-time.Second), protocol.NewDuplicateDetector())
	if !errors.Is(err, errHandshakeTimeout) {
		t.Errorf("readHandshakeFrame(expired deadline) = %v, want errHandshakeTimeout", err)
	}
	if sc.reads != 0 {
		t.Errorf("readHandshakeFrame read the conn %d times, want 0 (deadline already passed)", sc.reads)
	}
}

// TestHandshakeProtocolErrors walks the errProtocol family of the proto.md §3
// failure table: sequence violations, malformed payloads, bad hello claims,
// duplicate message IDs and foreign test IDs. All are retryable on the
// coordinator and answered with abort(protocol_error).
func TestHandshakeProtocolErrors(t *testing.T) {
	for _, tc := range []hsCase{
		{
			name: "coordinator-first-frame-not-hello",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{capsFrame(t, "", "pc2-00000001", true)}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("expected hello, got capabilities"),
		},
		{
			name: "coordinator-malformed-hello-payload",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{junkPayload(helloFrame(t, nil))}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("malformed hello payload"),
		},
		{
			name: "coordinator-wrong-hello-role",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloFrame(t, func(h *protocol.Hello) { h.Role = "pc1" })}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
		},
		{
			name: "coordinator-hello-peer-ip-claim-mismatch",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloFrame(t, func(h *protocol.Hello) { h.PeerIP = "192.168.1.99" })}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
		},
		{
			name: "coordinator-duplicate-message-id",
			frames: func(t *testing.T) []*protocol.Envelope {
				// The capabilities frame reuses the hello's message ID.
				return []*protocol.Envelope{helloFrame(t, nil), capsFrame(t, "", "pc2-00000001", true)}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
		},
		{
			name: "coordinator-foreign-testid-in-capabilities",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloFrame(t, nil), capsFrame(t, "ct-evil", "pc2-00000002", true)}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("capabilities carry a foreign testId"),
		},
		{
			name: "coordinator-malformed-capabilities-payload",
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloFrame(t, nil), junkPayload(capsFrame(t, "", "pc2-00000002", true))}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("malformed capabilities payload"),
		},
		{
			name:            "coordinator-peer-vanishes-before-hello",
			exhaust:         io.EOF,
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("expected hello as first frame"),
		},
		{
			name:   "worker-wrong-reply-type",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{mustEnvelope(t, protocol.TypeReady, "", "pc1-00000001", nil)}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("expected hello_ack, got ready"),
		},
		{
			name:   "worker-malformed-hello-ack-payload",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{junkPayload(helloAckFrame(t, "ct-x"))}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("malformed hello_ack payload"),
		},
		{
			name:   "worker-hello-ack-without-testid",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloAckFrame(t, "")}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("hello_ack carries no testId"),
		},
		{
			name:   "worker-foreign-testid-in-capabilities",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{helloAckFrame(t, "ct-x"), capsFrame(t, "ct-y", "pc1-00000002", true)}
			},
			wantErr:         errProtocol,
			retryable:       true,
			wantAbortReason: abortProtocolError,
			wantAbortDetail: strp("capabilities carry a foreign testId"),
		},
		{
			name:   "worker-malformed-abort-payload",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{junkPayload(abortFrame(t, abortAuthFailed, ""))}
			},
			wantErr:   errProtocol,
			retryable: true,
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestHandshakePeerAborted asserts the residual worker-side failures: the
// peer vanishing mid-handshake and an abort with an unrecognized reason both
// map to errPeerAborted and are fatal without an answering abort.
func TestHandshakePeerAborted(t *testing.T) {
	for _, tc := range []hsCase{
		{
			name:      "worker-eof-waiting-for-hello-ack",
			worker:    true,
			exhaust:   io.EOF,
			wantErr:   errPeerAborted,
			retryable: false,
		},
		{
			name:   "worker-abort-with-other-reason",
			worker: true,
			frames: func(t *testing.T) []*protocol.Envelope {
				return []*protocol.Envelope{abortFrame(t, "user_interrupt", "operator hit ctrl-c")}
			},
			wantErr:   errPeerAborted,
			retryable: false,
		},
	} {
		t.Run(tc.name, tc.run)
	}
}
