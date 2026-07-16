package protocol

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the protocol version string carried in every envelope. Exact
// match is required; there is no negotiation in v1.
const Version = "1"

// Framing and timing constants, shared by both peers. One frame-size limit
// everywhere means one code path and no desync risk; report chunks fit
// inside MaxFrameSize by construction.
const (
	// MaxFrameSize is the hard cap on a frame's JSON body, both directions,
	// all message types.
	MaxFrameSize = 1 << 20
	// MinFrameSize is the smallest syntactically valid JSON body ("{}").
	MinFrameSize = 2
	// DefaultWriteTimeout bounds each frame write.
	DefaultWriteTimeout = 10 * time.Second
	// DefaultIdleTimeout bounds the wait for each inbound frame; at
	// HeartbeatInterval it equals roughly four missed heartbeats.
	DefaultIdleTimeout = 20 * time.Second
	// HandshakeTimeout is the budget for the whole handshake, measured from
	// accept/connect.
	HandshakeTimeout = 30 * time.Second
	// HelloTimeout bounds accept → first frame on PC1.
	HelloTimeout = 10 * time.Second
	// HeartbeatInterval is the heartbeat ticker period on both sides.
	HeartbeatInterval = 5 * time.Second
	// ChunkSize is the raw byte size of one report_chunk payload (256 KiB;
	// ~342 KiB after base64, comfortably under MaxFrameSize).
	ChunkSize = 256 << 10
	// MaxTransferFileSize caps a single transferred report file (8 MiB).
	MaxTransferFileSize = 8 << 20
	// MaxTransferTotal caps the sum of all transferred report files
	// (16 MiB).
	MaxTransferTotal = 16 << 20
)

// readBufSize is the bufio.Reader buffer in front of the connection.
const readBufSize = 64 << 10

// FrameError reports a fatal framing violation: a declared length outside
// [MinFrameSize, MaxFrameSize], a body that does not parse as a JSON
// envelope, or an outbound envelope that would exceed MaxFrameSize. A
// corrupt length prefix cannot be resynchronized, so on the read side the
// caller must close the connection; on the write side it is a caller bug
// (the frame is never truncated or sent).
type FrameError struct {
	// Len is the offending frame length: declared by the peer on reads,
	// attempted locally on writes.
	Len uint64
	// Reason describes the violation.
	Reason string
	// Err is the underlying cause, if any (e.g. the JSON error).
	Err error
}

// Error implements the error interface.
func (e *FrameError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("protocol: fatal frame error (len %d): %s: %v", e.Len, e.Reason, e.Err)
	}
	return fmt.Sprintf("protocol: fatal frame error (len %d): %s", e.Len, e.Reason)
}

// Unwrap returns the underlying cause, if any.
func (e *FrameError) Unwrap() error { return e.Err }

// Conn frames envelopes over a net.Conn. It owns all reads from the
// underlying connection (a single buffered reader; never read nc elsewhere
// after construction) and serializes writes internally, so the event loop,
// heartbeater and executor may all call WriteEnvelope concurrently.
type Conn struct {
	nc          net.Conn
	br          *bufio.Reader // sole reader of nc after construction
	wmu         sync.Mutex    // serializes WriteEnvelope
	maxFrame    uint32
	writeTO     time.Duration
	idleTimeout atomic.Int64 // nanoseconds; mutable for cable-test link-loss windows
	lastSend    atomic.Int64 // unix nanos of last successful write
	closeOnce   sync.Once
	closeErr    error
}

// NewConn wraps nc for framed envelope exchange with the default limits
// (MaxFrameSize, DefaultWriteTimeout, DefaultIdleTimeout).
func NewConn(nc net.Conn) *Conn {
	c := &Conn{
		nc:       nc,
		br:       bufio.NewReaderSize(nc, readBufSize),
		maxFrame: MaxFrameSize,
		writeTO:  DefaultWriteTimeout,
	}
	c.idleTimeout.Store(int64(DefaultIdleTimeout))
	return c
}

// ReadEnvelope blocks until one whole frame arrives and returns its decoded
// envelope. A single absolute read deadline — now plus the idle timeout, set
// fresh per frame — covers both the 4-byte header and the body, so a stalled
// body times out too. Length violations and unparseable bodies return a
// *FrameError, after which the stream is unsyncable and the caller must
// close the connection; deadline errors match os.ErrDeadlineExceeded.
func (c *Conn) ReadEnvelope() (*Envelope, error) {
	deadline := wallClock.Now().Add(time.Duration(c.idleTimeout.Load()))
	if err := c.nc.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < MinFrameSize || n > c.maxFrame {
		// Reject on the header alone, before any body allocation or read.
		return nil, &FrameError{Len: uint64(n), Reason: fmt.Sprintf("frame length outside [%d, %d]", MinFrameSize, c.maxFrame)}
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return nil, err
	}
	var env Envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return nil, &FrameError{Len: uint64(n), Reason: "invalid envelope JSON", Err: err}
	}
	return &env, nil
}

// WriteEnvelope marshals env and writes the length prefix and body as one
// buffer in one Write under the write mutex and a per-write deadline, so
// concurrent writers can never interleave frame bytes. An envelope that
// would exceed MaxFrameSize returns a *FrameError without touching the
// wire. Any wire error is fatal to the session.
func (c *Conn) WriteEnvelope(env *Envelope) error {
	if env == nil {
		return errors.New("protocol: WriteEnvelope: nil envelope")
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("protocol: marshal envelope: %w", err)
	}
	if len(body) > int(c.maxFrame) {
		return &FrameError{Len: uint64(len(body)), Reason: fmt.Sprintf("encoded envelope exceeds MaxFrameSize %d; never truncated", c.maxFrame)}
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(body)))
	copy(buf[4:], body)

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.nc.SetWriteDeadline(wallClock.Now().Add(c.writeTO)); err != nil {
		return err
	}
	if _, err := c.nc.Write(buf); err != nil {
		return err
	}
	c.lastSend.Store(wallClock.Now().UnixNano())
	return nil
}

// SetIdleTimeout changes the per-frame read deadline used by subsequent
// ReadEnvelope calls. It exists for the cable-test feature, which
// pre-arranges a link-loss window on both sides before the link goes down.
func (c *Conn) SetIdleTimeout(d time.Duration) {
	c.idleTimeout.Store(int64(d))
}

// LastSend returns the time of the last successful WriteEnvelope, or the
// zero time if nothing has been sent. The heartbeater uses it to skip sends
// while other traffic keeps the line demonstrably alive.
func (c *Conn) LastSend() time.Time {
	n := c.lastSend.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Close closes the underlying connection, unblocking any pending read. It
// is idempotent: repeated calls return the first result.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.nc.Close() })
	return c.closeErr
}

// ConfigureKeepAlive enables TCP keepalive probing on nc (Idle 5s, Interval
// 5s, Count 3) as backup liveness detection underneath the protocol
// heartbeats. Connections that are not *net.TCPConn (net.Pipe in tests) are
// left untouched and return nil.
func ConfigureKeepAlive(nc net.Conn) error {
	tc, ok := nc.(*net.TCPConn)
	if !ok {
		return nil
	}
	return tc.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     5 * time.Second,
		Interval: 5 * time.Second,
		Count:    3,
	})
}
