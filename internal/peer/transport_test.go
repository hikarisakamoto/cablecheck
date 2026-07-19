package peer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/testutil"
)

// addrConn wraps a net.Conn with a fixed remote address string, letting
// net.Pipe stand in for a TCP conn in RemoteAddr-sensitive tests.
type addrConn struct {
	net.Conn
	remote string
}

// RemoteAddr returns the configured fake remote address.
func (a *addrConn) RemoteAddr() net.Addr { return fakeAddr(a.remote) }

type recordingDialTransport struct {
	localIP netip.Addr
	remote  string
}

func (t *recordingDialTransport) Listen(context.Context, string) (net.Listener, error) {
	return nil, errors.New("unexpected Listen")
}

func (t *recordingDialTransport) Dial(_ context.Context, localIP netip.Addr, remote string) (net.Conn, error) {
	t.localIP = localIP
	t.remote = remote
	return nil, syscall.ECONNREFUSED
}

func TestWorkerDialUsesConfiguredLocalIP(t *testing.T) {
	tr := &recordingDialTransport{}
	cfg := Config{
		Role:        RolePC2,
		LocalIP:     netip.MustParseAddr("192.168.50.20"),
		PeerIP:      netip.MustParseAddr("192.168.50.10"),
		ControlPort: 8443,
		Transport:   tr,
	}
	s := newSession(cfg, nil, nil)
	s.sm = NewStateMachine(StateConnecting, nil)
	_, _, err := s.establishWorker(t.Context())
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("establishWorker error = %v, want ECONNREFUSED", err)
	}
	if tr.localIP != cfg.LocalIP {
		t.Errorf("Dial local IP = %s, want configured %s", tr.localIP, cfg.LocalIP)
	}
	if tr.remote != "192.168.50.10:8443" {
		t.Errorf("Dial remote = %q, want 192.168.50.10:8443", tr.remote)
	}
}

// TestHandshakeWrongPeerIP asserts verifyPeerIP closes a connection from an
// unexpected IP without sending anything, and leaves a matching connection
// open (including the IPv4-in-IPv6 mapped form).
func TestHandshakeWrongPeerIP(t *testing.T) {
	want := netip.MustParseAddr("192.168.1.2")

	t.Run("mismatch-closed-no-response", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		// Hang guard only; armed before the close, since net.Pipe rejects
		// deadline changes once either end is closed.
		if derr := client.SetReadDeadline(time.Now().Add(testutil.TestTimeout(t))); derr != nil {
			t.Fatalf("SetReadDeadline: %v", derr)
		}

		err := verifyPeerIP(&addrConn{Conn: server, remote: "192.168.1.99:40000"}, want)
		if err == nil {
			t.Fatal("verifyPeerIP: nil error for mismatched remote IP")
		}
		if !errors.Is(err, errWrongPeerIP) {
			t.Errorf("verifyPeerIP error = %v, want errWrongPeerIP", err)
		}
		// The conn must be closed with no bytes ever written: the other end
		// must observe immediate EOF.
		buf := make([]byte, 1)
		n, rerr := client.Read(buf)
		if n != 0 || rerr != io.EOF {
			t.Errorf("peer Read = (%d, %v), want (0, io.EOF): rejected conn must get no response", n, rerr)
		}
	})

	t.Run("match-stays-open", func(t *testing.T) {
		for _, remote := range []string{"192.168.1.2:40000", "[::ffff:192.168.1.2]:40000"} {
			client, server := net.Pipe()
			wrapped := &addrConn{Conn: server, remote: remote}
			if err := verifyPeerIP(wrapped, want); err != nil {
				t.Errorf("verifyPeerIP(%q): %v, want nil", remote, err)
			}
			// Still usable: a byte written on the accepted side arrives.
			go func() {
				wrapped.Write([]byte{0x42})
				wrapped.Close()
			}()
			if derr := client.SetReadDeadline(time.Now().Add(testutil.TestTimeout(t))); derr != nil {
				t.Fatalf("SetReadDeadline: %v", derr)
			}
			buf := make([]byte, 1)
			if n, rerr := io.ReadFull(client, buf); n != 1 || rerr != nil {
				t.Errorf("Read after verifyPeerIP(%q) = (%d, %v), want the conn open", remote, n, rerr)
			}
			client.Close()
		}
	})

	t.Run("unparseable-remote-closed", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		if derr := client.SetReadDeadline(time.Now().Add(testutil.TestTimeout(t))); derr != nil {
			t.Fatalf("SetReadDeadline: %v", derr)
		}
		err := verifyPeerIP(&addrConn{Conn: server, remote: "pipe"}, want)
		if !errors.Is(err, errWrongPeerIP) {
			t.Errorf("verifyPeerIP(unparseable) error = %v, want errWrongPeerIP", err)
		}
		buf := make([]byte, 1)
		if n, rerr := client.Read(buf); n != 0 || rerr != io.EOF {
			t.Errorf("peer Read = (%d, %v), want (0, io.EOF)", n, rerr)
		}
	})
}

// reserveClosedPort grabs an ephemeral loopback port and immediately releases
// it, returning an address that dials to "connection refused" until the test
// re-listens on it.
func reserveClosedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return addr
}

// dialOut carries a Dial outcome across goroutines.
type dialOut struct {
	conn net.Conn
	err  error
}

// TestTCPTransportListenAndAccept exercises the real transport's listener:
// the happy accept path (with keepalive configuration applied), the listen
// error path, and Accept after Close.
func TestTCPTransportListenAndAccept(t *testing.T) {
	testutil.LeakCheck(t)
	tr := newTCPTransport(clocktest.New(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)))
	ctx := t.Context()

	t.Run("accept-configures-conn", func(t *testing.T) {
		ln, err := tr.Listen(ctx, "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		defer ln.Close()
		accepted := make(chan dialOut, 1)
		go func() {
			c, aerr := ln.Accept()
			accepted <- dialOut{conn: c, err: aerr}
		}()
		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial listener: %v", err)
		}
		defer client.Close()
		out := <-accepted
		if out.err != nil {
			t.Fatalf("Accept: %v", out.err)
		}
		defer out.conn.Close()
		// The accepted conn must be live end to end.
		if _, err := client.Write([]byte{0x42}); err != nil {
			t.Fatalf("client write: %v", err)
		}
		if err := out.conn.SetReadDeadline(time.Now().Add(testutil.TestTimeout(t))); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		buf := make([]byte, 1)
		if _, err := io.ReadFull(out.conn, buf); err != nil || buf[0] != 0x42 {
			t.Errorf("accepted read = (%#x, %v), want (0x42, nil)", buf[0], err)
		}
	})

	t.Run("listen-error", func(t *testing.T) {
		ln, err := tr.Listen(ctx, "127.0.0.1:65536") // port out of range
		if err == nil {
			ln.Close()
			t.Fatal("Listen(127.0.0.1:65536): nil error, want failure")
		}
		if !strings.Contains(err.Error(), "127.0.0.1:65536") {
			t.Errorf("Listen error %q does not name the address", err)
		}
	})

	t.Run("accept-after-close", func(t *testing.T) {
		ln, err := tr.Listen(ctx, "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if c, aerr := ln.Accept(); aerr == nil {
			c.Close()
			t.Error("Accept after Close: nil error, want failure")
		}
	})
}

// TestTCPTransportDialRetryThenSuccess drives the worker dial loop with a
// fake clock: the first attempt is refused (nothing listens yet), Dial parks
// on the retry timer, the test then opens the listener and advances the
// clock, and the second attempt must succeed.
func TestTCPTransportDialRetryThenSuccess(t *testing.T) {
	testutil.LeakCheck(t)
	fc := clocktest.New(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC))
	tr := newTCPTransport(fc)
	addr := reserveClosedPort(t)
	ctx := t.Context()

	res := make(chan dialOut, 1)
	go func() {
		c, err := tr.Dial(ctx, netip.MustParseAddr("127.0.0.1"), addr)
		res <- dialOut{conn: c, err: err}
	}()

	// The retry timer registering proves the first attempt already failed.
	fc.BlockUntilWaiters(1)
	ln, err := tr.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen on reserved port: %v", err)
	}
	defer ln.Close()
	accepted := make(chan dialOut, 1)
	go func() {
		c, aerr := ln.Accept()
		accepted <- dialOut{conn: c, err: aerr}
	}()

	fc.Advance(dialRetryDelay) // wake the retry; this attempt succeeds
	out := <-res
	if out.err != nil {
		t.Fatalf("Dial after retry: %v", out.err)
	}
	defer out.conn.Close()
	acc := <-accepted
	if acc.err != nil {
		t.Fatalf("Accept: %v", acc.err)
	}
	defer acc.conn.Close()
	// Prove the dialed conn is the live one the listener accepted.
	if _, err := out.conn.Write([]byte{0x7f}); err != nil {
		t.Fatalf("write on dialed conn: %v", err)
	}
	if err := acc.conn.SetReadDeadline(time.Now().Add(testutil.TestTimeout(t))); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(acc.conn, buf); err != nil || buf[0] != 0x7f {
		t.Errorf("accepted read = (%#x, %v), want (0x7f, nil)", buf[0], err)
	}
}

// TestTCPTransportDialGivesUpAfterBudget exhausts the 60s dial budget against
// a port nobody listens on and asserts the give-up error wraps the last
// attempt's failure.
func TestTCPTransportDialGivesUpAfterBudget(t *testing.T) {
	testutil.LeakCheck(t)
	fc := clocktest.New(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC))
	tr := newTCPTransport(fc)
	addr := reserveClosedPort(t)

	res := make(chan dialOut, 1)
	go func() {
		c, err := tr.Dial(t.Context(), netip.MustParseAddr("127.0.0.1"), addr)
		res <- dialOut{conn: c, err: err}
	}()

	// Dial waits between attempts while now+retryDelay <= deadline, i.e.
	// exactly budget/retryDelay times; after the last advance the final
	// attempt fails and the loop gives up instead of registering a timer.
	for range int(dialTotalBudget / dialRetryDelay) {
		fc.BlockUntilWaiters(1)
		fc.Advance(dialRetryDelay)
	}
	out := <-res
	if out.conn != nil {
		out.conn.Close()
		t.Fatal("Dial returned a conn, want budget-exhaustion failure")
	}
	if !strings.Contains(out.err.Error(), "gave up after") {
		t.Errorf("Dial error = %v, want a give-up error", out.err)
	}
	if !errors.Is(out.err, syscall.ECONNREFUSED) {
		t.Errorf("Dial error = %v, want it to wrap the last attempt's ECONNREFUSED", out.err)
	}
}

// TestTCPTransportDialContextCanceled pins both cancellation exits of the
// dial loop: cancellation while parked between attempts, and a context that
// is already canceled when an attempt runs.
func TestTCPTransportDialContextCanceled(t *testing.T) {
	t.Run("between-attempts", func(t *testing.T) {
		testutil.LeakCheck(t)
		fc := clocktest.New(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC))
		tr := newTCPTransport(fc)
		addr := reserveClosedPort(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		res := make(chan dialOut, 1)
		go func() {
			c, err := tr.Dial(ctx, netip.MustParseAddr("127.0.0.1"), addr)
			res <- dialOut{conn: c, err: err}
		}()
		fc.BlockUntilWaiters(1) // parked on the retry timer
		cancel()
		out := <-res
		if out.conn != nil {
			out.conn.Close()
			t.Fatal("Dial returned a conn, want cancellation failure")
		}
		if !errors.Is(out.err, context.Canceled) {
			t.Errorf("Dial error = %v, want context.Canceled", out.err)
		}
		if !strings.Contains(out.err.Error(), "last attempt") {
			t.Errorf("Dial error %q should mention the last attempt's failure", out.err)
		}
	})

	t.Run("during-attempt", func(t *testing.T) {
		testutil.LeakCheck(t)
		fc := clocktest.New(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC))
		tr := newTCPTransport(fc)
		addr := reserveClosedPort(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // the very first attempt sees a dead context

		conn, err := tr.Dial(ctx, netip.MustParseAddr("127.0.0.1"), addr)
		if conn != nil {
			conn.Close()
			t.Fatal("Dial returned a conn, want cancellation failure")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Dial error = %v, want context.Canceled", err)
		}
	})
}
