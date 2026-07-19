package peer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/protocol"
)

// Transport establishes the control connection. It is the testability seam:
// production uses the real TCP implementation, unit tests inject a net.Pipe
// backed fake.
type Transport interface {
	// Listen opens a listener on addr ("ip:port"); the coordinator binds
	// only its --local-ip, never the wildcard address.
	Listen(ctx context.Context, addr string) (net.Listener, error)
	// Dial connects to addr with its source bound to localIP; the worker
	// implementation retries within its dial budget, so a coordinator
	// started second is still reached.
	Dial(ctx context.Context, localIP netip.Addr, addr string) (net.Conn, error)
}

// Dial policy of the real transport (docs/design/proto.md §3): 5s per
// attempt, retry every 2s, give up after 60s total.
const (
	dialAttemptTimeout = 5 * time.Second
	dialRetryDelay     = 2 * time.Second
	dialTotalBudget    = 60 * time.Second
)

// tcpTransport is the production Transport: plain TCP with
// protocol.ConfigureKeepAlive applied to every connection, and the worker's
// retrying dial.
type tcpTransport struct {
	clk clock.Clock
}

// newTCPTransport returns the real TCP Transport; clk paces dial retries.
func newTCPTransport(clk clock.Clock) Transport {
	return &tcpTransport{clk: clk}
}

// Listen implements Transport. addr must carry the local interface IP; the
// returned listener applies TCP keepalive to every accepted connection.
func (t *tcpTransport) Listen(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("peer: listen on %s: %w", addr, err)
	}
	return &keepAliveListener{Listener: ln}, nil
}

// keepAliveListener applies protocol.ConfigureKeepAlive to accepted conns.
type keepAliveListener struct {
	net.Listener
}

// Accept implements net.Listener. A connection whose keepalive cannot be
// configured is closed and the error returned; the listener itself stays
// usable.
func (l *keepAliveListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if kerr := protocol.ConfigureKeepAlive(nc); kerr != nil {
		nc.Close()
		return nil, fmt.Errorf("peer: configure keepalive on accepted conn: %w", kerr)
	}
	return nc, nil
}

// Dial implements Transport: 5s per attempt, retrying every 2s for up to 60s
// total (the peer may simply not be started yet), honoring ctx cancellation
// between and during attempts. TCP keepalive is applied before returning.
func (t *tcpTransport) Dial(ctx context.Context, localIP netip.Addr, addr string) (net.Conn, error) {
	deadline := t.clk.Now().Add(dialTotalBudget)
	d := net.Dialer{
		Timeout:   dialAttemptTimeout,
		LocalAddr: &net.TCPAddr{IP: net.IP(localIP.Unmap().AsSlice())},
	}
	var lastErr error
	for {
		nc, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			if kerr := protocol.ConfigureKeepAlive(nc); kerr != nil {
				nc.Close()
				return nil, fmt.Errorf("peer: configure keepalive on dialed conn: %w", kerr)
			}
			return nc, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, fmt.Errorf("peer: dial %s: %w (last attempt: %v)", addr, ctx.Err(), lastErr)
		}
		if t.clk.Now().Add(dialRetryDelay).After(deadline) {
			return nil, fmt.Errorf("peer: dial %s: gave up after %s: %w", addr, dialTotalBudget, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("peer: dial %s: %w (last attempt: %v)", addr, ctx.Err(), lastErr)
		case <-t.clk.After(dialRetryDelay):
		}
	}
}

// errWrongPeerIP reports a connection arriving from an address other than
// the expected peer IP.
var errWrongPeerIP = errors.New("peer: connection from unexpected address")

// verifyPeerIP checks that nc's remote IP equals want, ignoring the port and
// IPv4-in-IPv6 mapping. On mismatch — including an unparseable remote
// address — it closes nc without writing anything (the handshake failure
// table mandates no response to unexpected callers) and returns an error
// wrapping errWrongPeerIP; the coordinator logs it and keeps listening.
func verifyPeerIP(nc net.Conn, want netip.Addr) error {
	remote := nc.RemoteAddr().String()
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		nc.Close()
		return fmt.Errorf("%w: unparseable remote address %q", errWrongPeerIP, remote)
	}
	got, err := netip.ParseAddr(host)
	if err != nil {
		nc.Close()
		return fmt.Errorf("%w: unparseable remote IP %q", errWrongPeerIP, host)
	}
	if got.Unmap() != want.Unmap() {
		nc.Close()
		return fmt.Errorf("%w: got %s, want %s", errWrongPeerIP, got.Unmap(), want.Unmap())
	}
	return nil
}
