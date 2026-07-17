package network

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

func TestProbePortFree(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")

	t.Run("free port passes", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := uint16(l.Addr().(*net.TCPAddr).Port)
		l.Close()

		if err := ProbePortFree(ip, port); err != nil {
			t.Errorf("ProbePortFree(%d) = %v, want nil", port, err)
		}
	})

	t.Run("tcp occupied names the port", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer l.Close()
		port := l.Addr().(*net.TCPAddr).Port

		err = ProbePortFree(ip, uint16(port))
		if err == nil {
			t.Fatalf("ProbePortFree(%d) = nil, want error while TCP listener holds it", port)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(port)) {
			t.Errorf("error %q does not name port %d", err, port)
		}
		if !strings.Contains(err.Error(), "tcp") {
			t.Errorf("error %q does not name the tcp family", err)
		}
	})

	t.Run("udp occupied names the port", func(t *testing.T) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen packet: %v", err)
		}
		defer pc.Close()
		port := pc.LocalAddr().(*net.UDPAddr).Port

		err = ProbePortFree(ip, uint16(port))
		if err == nil {
			t.Fatalf("ProbePortFree(%d) = nil, want error while UDP socket holds it", port)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(port)) {
			t.Errorf("error %q does not name port %d", err, port)
		}
		if !strings.Contains(err.Error(), "udp") {
			t.Errorf("error %q does not name the udp family", err)
		}
	})
}
