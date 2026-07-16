package protocol_test

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

// pipeConns wraps both ends of a net.Pipe in protocol.Conns. Both ends are
// closed via t.Cleanup so peers blocked on the synchronous pipe always
// unblock, even when the test fails early.
func pipeConns(t *testing.T) (a, b *protocol.Conn) {
	t.Helper()
	p1, p2 := net.Pipe()
	a, b = protocol.NewConn(p1), protocol.NewConn(p2)
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return a, b
}

// writeRawFrame writes a length-prefixed frame around body to w. It reports
// failures via t.Errorf only, so it is safe from non-test goroutines.
func writeRawFrame(t *testing.T, w io.Writer, body []byte) {
	t.Helper()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		t.Errorf("write frame header: %v", err)
		return
	}
	if _, err := w.Write(body); err != nil {
		t.Errorf("write frame body: %v", err)
	}
}

// TestFrameRoundTrip sends a mix of envelopes across a net.Pipe and verifies
// every field and payload survives the encode/frame/decode cycle.
func TestFrameRoundTrip(t *testing.T) {
	testutil.LeakCheck(t)
	a, b := pipeConns(t)

	hello, err := protocol.NewEnvelope(protocol.TypeHello, "", "pc2-00000001", protocol.Hello{
		Token: "tok", Role: "pc2", CablecheckVersion: "1.0.0", LocalIP: "192.168.1.2", PeerIP: "192.168.1.1",
	})
	if err != nil {
		t.Fatalf("NewEnvelope(hello): %v", err)
	}
	ack, err := protocol.NewEnvelope(protocol.TypeHelloAck, "ct-x", "pc1-00000001", protocol.HelloAck{
		TestID: "ct-x", CablecheckVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("NewEnvelope(hello_ack): %v", err)
	}
	ack.InReplyTo = "pc2-00000001"
	hb, err := protocol.NewEnvelope(protocol.TypeHeartbeat, "ct-x", "pc1-00000002", protocol.Heartbeat{
		Seq: 7, State: "testing", ActiveOp: "ping_run",
	})
	if err != nil {
		t.Fatalf("NewEnvelope(heartbeat): %v", err)
	}
	want := []*protocol.Envelope{hello, ack, hb}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, env := range want {
			if err := a.WriteEnvelope(env); err != nil {
				t.Errorf("WriteEnvelope(%s): %v", env.MessageID, err)
				return
			}
		}
	}()

	got := make([]*protocol.Envelope, 0, len(want))
	for i := range want {
		env, err := b.ReadEnvelope()
		if err != nil {
			t.Fatalf("ReadEnvelope #%d: %v", i, err)
		}
		got = append(got, env)
	}
	testutil.WaitFor(t, done, "round-trip writer goroutine")

	for i, w := range want {
		g := got[i]
		if g.ProtocolVersion != w.ProtocolVersion || g.TestID != w.TestID ||
			g.MessageID != w.MessageID || g.InReplyTo != w.InReplyTo || g.Type != w.Type {
			t.Errorf("envelope #%d fields = %+v, want %+v", i, g, w)
		}
		if !g.Timestamp.Equal(w.Timestamp) {
			t.Errorf("envelope #%d timestamp = %v, want %v", i, g.Timestamp, w.Timestamp)
		}
	}

	h, err := protocol.DecodePayload[protocol.Hello](got[0])
	if err != nil {
		t.Fatalf("DecodePayload[Hello]: %v", err)
	}
	if h.Token != "tok" || h.Role != "pc2" || h.PeerIP != "192.168.1.1" {
		t.Errorf("hello payload = %+v", h)
	}
	beat, err := protocol.DecodePayload[protocol.Heartbeat](got[2])
	if err != nil {
		t.Fatalf("DecodePayload[Heartbeat]: %v", err)
	}
	if beat.Seq != 7 || beat.State != "testing" || beat.ActiveOp != "ping_run" {
		t.Errorf("heartbeat payload = %+v", beat)
	}
}

// dribbleConn delivers reads in tiny rotating chunks so no consumer can
// assume one Read yields one frame. Deadlines still apply to the wrapped end.
type dribbleConn struct {
	net.Conn
	r io.Reader
}

// Read reads from the dribbling wrapper instead of the raw conn.
func (d *dribbleConn) Read(p []byte) (int, error) { return d.r.Read(p) }

// TestFramePartialReads decodes frames whose bytes arrive in 1-2 byte
// fragments, including a fragment boundary exactly after the 4-byte header.
func TestFramePartialReads(t *testing.T) {
	testutil.LeakCheck(t)

	t.Run("dribble", func(t *testing.T) {
		p1, p2 := net.Pipe()
		// maxChunk 2 rotates read sizes 1,2,1,2..., so cumulative bytes hit
		// 1, 3, 4: the third read of the first frame ends exactly after the
		// 4-byte header.
		reader := protocol.NewConn(&dribbleConn{Conn: p1, r: testutil.Dribble(p1, 2)})
		writer := protocol.NewConn(p2)
		t.Cleanup(func() {
			reader.Close()
			writer.Close()
		})

		const frames = 3
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 1; i <= frames; i++ {
				env, err := protocol.NewEnvelope(protocol.TypeHeartbeat, "ct-x",
					fmt.Sprintf("pc1-%08d", i), protocol.Heartbeat{Seq: uint64(i), State: "testing"})
				if err != nil {
					t.Errorf("NewEnvelope #%d: %v", i, err)
					return
				}
				if err := writer.WriteEnvelope(env); err != nil {
					t.Errorf("WriteEnvelope #%d: %v", i, err)
					return
				}
			}
		}()

		for i := 1; i <= frames; i++ {
			env, err := reader.ReadEnvelope()
			if err != nil {
				t.Fatalf("ReadEnvelope #%d over dribbling conn: %v", i, err)
			}
			beat, err := protocol.DecodePayload[protocol.Heartbeat](env)
			if err != nil {
				t.Fatalf("DecodePayload #%d: %v", i, err)
			}
			if beat.Seq != uint64(i) {
				t.Errorf("frame #%d seq = %d, want %d", i, beat.Seq, i)
			}
		}
		testutil.WaitFor(t, done, "dribbling writer goroutine")
	})

	t.Run("split_after_header", func(t *testing.T) {
		p1, p2 := net.Pipe()
		reader := protocol.NewConn(p1)
		t.Cleanup(func() {
			reader.Close()
			p2.Close()
		})

		env, err := protocol.NewEnvelope(protocol.TypeReady, "ct-x", "pc2-00000002", protocol.Ready{NonInteractive: true})
		if err != nil {
			t.Fatalf("NewEnvelope: %v", err)
		}
		body, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
			// Two separate writes: the stream pauses exactly after the
			// 4-byte header before any body byte exists.
			if _, err := p2.Write(hdr[:]); err != nil {
				t.Errorf("write header: %v", err)
				return
			}
			if _, err := p2.Write(body); err != nil {
				t.Errorf("write body: %v", err)
			}
		}()

		got, err := reader.ReadEnvelope()
		if err != nil {
			t.Fatalf("ReadEnvelope with header/body split: %v", err)
		}
		if got.Type != protocol.TypeReady || got.MessageID != "pc2-00000002" {
			t.Errorf("envelope = %+v", got)
		}
		testutil.WaitFor(t, done, "split writer goroutine")
	})
}

// TestFrameMaxSize accepts a frame of exactly MaxFrameSize and rejects
// oversized declarations with a typed FrameError before reading (or
// allocating) any body.
func TestFrameMaxSize(t *testing.T) {
	testutil.LeakCheck(t)

	t.Run("exactly_max_accepted", func(t *testing.T) {
		p1, p2 := net.Pipe()
		reader := protocol.NewConn(p1)
		t.Cleanup(func() {
			reader.Close()
			p2.Close()
		})

		prefix := `{"protocolVersion":"1","testId":"ct-x","messageId":"pc1-00000001","type":"heartbeat","pad":"`
		const suffix = `"}`
		body := prefix + strings.Repeat("x", protocol.MaxFrameSize-len(prefix)-len(suffix)) + suffix
		if len(body) != protocol.MaxFrameSize {
			t.Fatalf("test bug: body length %d, want %d", len(body), protocol.MaxFrameSize)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			writeRawFrame(t, p2, []byte(body))
		}()

		env, err := reader.ReadEnvelope()
		if err != nil {
			t.Fatalf("ReadEnvelope on exactly-MaxFrameSize frame: %v", err)
		}
		if env.Type != protocol.TypeHeartbeat || env.MessageID != "pc1-00000001" {
			t.Errorf("envelope = %+v", env)
		}
		testutil.WaitFor(t, done, "max-size writer goroutine")
	})

	for _, tc := range []struct {
		name   string
		length uint32
	}{
		{"max_plus_one_rejected", protocol.MaxFrameSize + 1},
		{"all_ones_header_rejected", 0xFFFFFFFF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p1, p2 := net.Pipe()
			reader := protocol.NewConn(p1)
			t.Cleanup(func() {
				reader.Close()
				p2.Close()
			})
			// No body is ever written: ReadEnvelope must reject on the
			// header alone. An implementation that allocated or tried to
			// read a body would instead stall into this short idle timeout
			// and fail the FrameError assertion below.
			reader.SetIdleTimeout(2 * time.Second)

			done := make(chan struct{})
			go func() {
				defer close(done)
				var hdr [4]byte
				binary.BigEndian.PutUint32(hdr[:], tc.length)
				if _, err := p2.Write(hdr[:]); err != nil {
					t.Errorf("write oversized header: %v", err)
				}
			}()

			_, err := reader.ReadEnvelope()
			var fe *protocol.FrameError
			if !errors.As(err, &fe) {
				t.Fatalf("ReadEnvelope error = %v (%T), want *protocol.FrameError", err, err)
			}
			if fe.Len != uint64(tc.length) {
				t.Errorf("FrameError.Len = %d, want %d", fe.Len, tc.length)
			}
			testutil.WaitFor(t, done, "oversized-header writer goroutine")
		})
	}

	t.Run("write_side_oversize_rejected", func(t *testing.T) {
		p1, p2 := net.Pipe()
		conn := protocol.NewConn(p1)
		t.Cleanup(func() {
			conn.Close()
			p2.Close()
		})
		env, err := protocol.NewEnvelope(protocol.TypeWarning, "ct-x", "pc1-00000001", struct {
			Pad string `json:"pad"`
		}{Pad: strings.Repeat("x", protocol.MaxFrameSize)})
		if err != nil {
			t.Fatalf("NewEnvelope: %v", err)
		}
		// Nobody reads p2: a compliant WriteEnvelope returns a FrameError
		// without touching the wire, so this must not block.
		err = conn.WriteEnvelope(env)
		var fe *protocol.FrameError
		if !errors.As(err, &fe) {
			t.Fatalf("WriteEnvelope oversized error = %v (%T), want *protocol.FrameError", err, err)
		}
		if fe.Len <= protocol.MaxFrameSize {
			t.Errorf("FrameError.Len = %d, want > %d", fe.Len, protocol.MaxFrameSize)
		}
	})
}

// TestFrameZeroLength rejects frames whose declared length is below
// MinFrameSize with a typed FrameError.
func TestFrameZeroLength(t *testing.T) {
	testutil.LeakCheck(t)
	for _, length := range []uint32{0, 1} {
		t.Run(fmt.Sprintf("len_%d", length), func(t *testing.T) {
			p1, p2 := net.Pipe()
			reader := protocol.NewConn(p1)
			t.Cleanup(func() {
				reader.Close()
				p2.Close()
			})

			done := make(chan struct{})
			go func() {
				defer close(done)
				var hdr [4]byte
				binary.BigEndian.PutUint32(hdr[:], length)
				if _, err := p2.Write(hdr[:]); err != nil {
					t.Errorf("write header: %v", err)
					return
				}
				// One filler byte so a buggy length-1 read path would have
				// data available yet still must have rejected already.
				if length > 0 {
					if _, err := p2.Write([]byte("x")); err != nil && !errors.Is(err, io.ErrClosedPipe) {
						t.Errorf("write filler byte: %v", err)
					}
				}
			}()

			_, err := reader.ReadEnvelope()
			var fe *protocol.FrameError
			if !errors.As(err, &fe) {
				t.Fatalf("ReadEnvelope error = %v (%T), want *protocol.FrameError", err, err)
			}
			if fe.Len != uint64(length) {
				t.Errorf("FrameError.Len = %d, want %d", fe.Len, length)
			}
			reader.Close() // unblock the filler write before waiting
			testutil.WaitFor(t, done, "undersized-frame writer goroutine")
		})
	}
}

// TestFrameTruncatedStream verifies that a peer hanging up mid-body surfaces
// io.ErrUnexpectedEOF from the interrupted io.ReadFull.
func TestFrameTruncatedStream(t *testing.T) {
	testutil.LeakCheck(t)
	p1, p2 := net.Pipe()
	reader := protocol.NewConn(p1)
	t.Cleanup(func() {
		reader.Close()
		p2.Close()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 100)
		frame := append(hdr[:], []byte(`{"partial":true`)...) // 15 of the promised 100 body bytes
		if _, err := p2.Write(frame); err != nil {
			t.Errorf("write truncated frame: %v", err)
			return
		}
		p2.Close() // hang up mid-body
	}()

	_, err := reader.ReadEnvelope()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadEnvelope on truncated stream = %v, want io.ErrUnexpectedEOF", err)
	}
	testutil.WaitFor(t, done, "truncating writer goroutine")
}

// TestWriteConcurrency has two goroutines write frames through one Conn at
// the same time; the reader must decode every frame intact with per-writer
// ordering preserved (run under -race).
func TestWriteConcurrency(t *testing.T) {
	testutil.LeakCheck(t)
	a, b := pipeConns(t)

	const perWriter = 25
	roles := []string{"pc1", "pc2"}
	var wg sync.WaitGroup
	for _, role := range roles {
		maker := protocol.NewMessageIDMaker(role)
		wg.Add(1)
		go func(role string, maker *protocol.MessageIDMaker) {
			defer wg.Done()
			for i := 1; i <= perWriter; i++ {
				env, err := protocol.NewEnvelope(protocol.TypeHeartbeat, "ct-x", maker.Next(),
					protocol.Heartbeat{Seq: uint64(i), State: "testing"})
				if err != nil {
					t.Errorf("NewEnvelope %s #%d: %v", role, i, err)
					return
				}
				if err := a.WriteEnvelope(env); err != nil {
					t.Errorf("WriteEnvelope %s #%d: %v", role, i, err)
					return
				}
			}
		}(role, maker)
	}
	writersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(writersDone)
	}()

	lastSeq := map[string]uint64{}
	counts := map[string]int{}
	for i := 0; i < len(roles)*perWriter; i++ {
		env, err := b.ReadEnvelope()
		if err != nil {
			t.Fatalf("ReadEnvelope #%d: %v", i, err)
		}
		beat, err := protocol.DecodePayload[protocol.Heartbeat](env)
		if err != nil {
			t.Fatalf("frame #%d damaged (interleaved writes?): %v", i, err)
		}
		role, _, ok := strings.Cut(env.MessageID, "-")
		if !ok || (role != "pc1" && role != "pc2") {
			t.Fatalf("frame #%d messageId = %q", i, env.MessageID)
		}
		if want := fmt.Sprintf("%s-%08d", role, beat.Seq); env.MessageID != want {
			t.Errorf("frame #%d messageId = %q, want %q", i, env.MessageID, want)
		}
		if beat.Seq != lastSeq[role]+1 {
			t.Errorf("frames from %s out of order: seq %d after %d", role, beat.Seq, lastSeq[role])
		}
		lastSeq[role] = beat.Seq
		counts[role]++
	}
	for _, role := range roles {
		if counts[role] != perWriter {
			t.Errorf("received %d frames from %s, want %d", counts[role], role, perWriter)
		}
	}
	testutil.WaitFor(t, writersDone, "concurrent writer goroutines")
}

// TestReadDeadline verifies the idle timeout: a blocked ReadEnvelope on a
// silent connection returns an error mapping os.ErrDeadlineExceeded, and not
// before the timeout has really elapsed.
func TestReadDeadline(t *testing.T) {
	testutil.LeakCheck(t)
	a, _ := pipeConns(t)

	const idle = 50 * time.Millisecond
	a.SetIdleTimeout(idle)
	start := time.Now()
	_, err := a.ReadEnvelope()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ReadEnvelope returned nil error with no peer traffic")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("ReadEnvelope idle error = %v, want os.ErrDeadlineExceeded", err)
	}
	if elapsed < idle-5*time.Millisecond {
		t.Errorf("ReadEnvelope returned after %v, before the %v idle timeout", elapsed, idle)
	}
}
