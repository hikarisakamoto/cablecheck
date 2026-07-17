package network

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// ProbePortFree verifies at preflight that port is bindable on ip for BOTH
// TCP and UDP, by binding and immediately closing each socket. Both families
// matter: iperf3's UDP data phase reuses the same port number over UDP. A
// specific-IP bind also collides with wildcard binds held by other processes,
// so foreign listeners are caught too. The returned error names the port and
// family that failed.
func ProbePortFree(ip netip.Addr, port uint16) error {
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))

	tl, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %d/tcp on %s is not available: %w", port, ip, err)
	}
	defer tl.Close()

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("port %d/udp on %s is not available: %w", port, ip, err)
	}
	return pc.Close()
}
