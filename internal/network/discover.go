// Package network discovers and classifies the local network interface that
// carries the cable under test, and probes control/data port availability.
//
// Discovery selects an interface by exact ownership of the operator-supplied
// local IP (`ip -j addr show` through the injected runner), classifies it via
// sysfs (loopback / virtual / wireless / USB, driver name), and rejects
// anything that is not a physical Ethernet NIC unless the caller opted in
// with --allow-virtual-interface. Guessing by subnet containment is
// deliberately not used for selection — an IP inside an interface's subnet
// but not assigned is a config error — but containment does power the
// "did you mean" hint on failure and, under --allow-virtual-interface, the
// loopback match that enables the single-machine demo.
package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
)

// ipTimeout bounds the `ip -j addr show` invocation; enumeration is a pure
// netlink dump and never legitimately takes longer.
const ipTimeout = 10 * time.Second

var (
	// ErrMalformedIPOutput reports `ip -j addr show` output that could not
	// be parsed as the expected JSON array.
	ErrMalformedIPOutput = errors.New("cannot parse ip -j addr show output")

	// ErrIPNotAssigned reports that the requested local IP is not assigned
	// to any interface (or, with an explicit --interface, not to that one).
	ErrIPNotAssigned = errors.New("IP address is not assigned")

	// ErrInterfaceNotFound reports an explicit --interface name that does
	// not exist on this machine.
	ErrInterfaceNotFound = errors.New("no such network interface")

	// ErrNotPhysical reports a loopback, virtual, or wireless interface
	// selected without --allow-virtual-interface.
	ErrNotPhysical = errors.New("interface is not a physical Ethernet NIC")

	// ErrInterfaceDown reports an interface whose operstate is down — a
	// preflight-failing condition, since no traffic can cross the cable.
	ErrInterfaceDown = errors.New("interface is down")
)

// DiscoverOptions tunes interface selection and classification.
type DiscoverOptions struct {
	// Interface, when non-empty, is the explicit --interface override: it
	// skips IP-based selection but the named interface must still own the
	// local IP and still passes classification.
	Interface string
	// AllowVirtual mirrors --allow-virtual-interface: loopback, virtual and
	// wireless interfaces are accepted instead of rejected; the returned
	// Class flags stay set so the caller can warn.
	AllowVirtual bool
	// SysfsRoot overrides the /sys/class/net directory used for
	// classification; empty means DefaultSysfsRoot. Tests inject a
	// t.TempDir tree here.
	SysfsRoot string
}

// Iface describes the discovered local interface.
type Iface struct {
	// Name is the interface name as reported by ip (canonical, even when
	// selected via an altname).
	Name string
	// Index is the kernel interface index.
	Index int
	// MTU is the interface MTU, which sizes full-frame ping and iperf3 UDP
	// payloads (MTU-28).
	MTU int
	// Operstate is the lowercase operational state ("up", "down",
	// "unknown", ...).
	Operstate string
	// MAC is the hardware address; empty for devices without one (wg, tun).
	MAC string
	// PrefixLen is the prefix length of the matched local IP assignment.
	PrefixLen int
	// Class is the physical/virtual classification evidence.
	Class Class
}

// Discover finds the interface owning localIP via `ip -j addr show` run
// through r, classifies it against sysfs, and validates that it is usable for
// a cable test. The returned Iface carries the classification flags even when
// AllowVirtual let a non-physical interface through, so callers can warn.
func Discover(ctx context.Context, r runner.Runner, localIP netip.Addr, opts DiscoverOptions) (Iface, error) {
	links, err := listLinks(ctx, r)
	if err != nil {
		return Iface{}, err
	}

	var link parser.IPLink
	var prefixLen int
	if opts.Interface != "" {
		link, prefixLen, err = selectByName(links, opts.Interface, localIP, opts.AllowVirtual)
	} else {
		link, prefixLen, err = selectByIP(links, localIP, opts.AllowVirtual)
	}
	if err != nil {
		return Iface{}, err
	}

	root := opts.SysfsRoot
	if root == "" {
		root = DefaultSysfsRoot
	}
	linkType := link.LinkType
	loopback := isLoopbackLink(link)
	if linkType == "" {
		// ip did not report a link_type; fall back to the sysfs ARPHRD
		// number for the highest-priority rules.
		sysType, sysLoop := sysfsLinkType(root, link.Ifname)
		linkType = sysType
		loopback = loopback || sysLoop
	}
	cls := classify(root, link.Ifname, linkType, loopback)

	if (cls.Loopback || cls.Virtual || cls.Wireless) && !opts.AllowVirtual {
		return Iface{}, fmt.Errorf("interface %s: %s: %w (pass --allow-virtual-interface to test it anyway)",
			link.Ifname, cls.Reason, ErrNotPhysical)
	}
	if link.Operstate == "down" || link.Operstate == "lowerlayerdown" {
		return Iface{}, fmt.Errorf("interface %s has operstate %q: %w", link.Ifname, link.Operstate, ErrInterfaceDown)
	}

	return Iface{
		Name:      link.Ifname,
		Index:     link.Ifindex,
		MTU:       link.MTU,
		Operstate: link.Operstate,
		MAC:       link.Address,
		PrefixLen: prefixLen,
		Class:     cls,
	}, nil
}

// listLinks runs `ip -j addr show` and parses its JSON array.
func listLinks(ctx context.Context, r runner.Runner) ([]parser.IPLink, error) {
	res, err := r.Run(ctx, runner.CommandSpec{
		Name:    "ip",
		Args:    []string{"-j", "addr", "show"},
		Timeout: ipTimeout,
		Label:   "ip-addr-show",
	})
	if err != nil {
		return nil, fmt.Errorf("run ip -j addr show: %w", err)
	}
	if res.Failed() {
		return nil, fmt.Errorf("ip -j addr show exited %d: %s",
			res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	links, err := parser.ParseIPAddr(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedIPOutput, err)
	}
	return links, nil
}

// selectByIP picks the interface owning ip by exact addr_info match. With
// allowVirtual, a loopback interface whose prefix merely contains ip is also
// accepted (the 127.0.0.2 single-machine demo).
func selectByIP(links []parser.IPLink, ip netip.Addr, allowVirtual bool) (parser.IPLink, int, error) {
	for _, l := range links {
		if plen, ok := ownsIP(l, ip); ok {
			return l, plen, nil
		}
	}
	if allowVirtual {
		for _, l := range links {
			if !isLoopbackLink(l) {
				continue
			}
			if plen, ok := prefixContains(l, ip); ok {
				return l, plen, nil
			}
		}
	}
	return parser.IPLink{}, 0, notAssignedError(links, ip)
}

// selectByName resolves an explicit --interface override (by name or
// altname) and validates that it owns ip under the same rules as selectByIP.
func selectByName(links []parser.IPLink, name string, ip netip.Addr, allowVirtual bool) (parser.IPLink, int, error) {
	idx := slices.IndexFunc(links, func(l parser.IPLink) bool {
		return l.Ifname == name || slices.Contains(l.Altnames, name)
	})
	if idx < 0 {
		return parser.IPLink{}, 0, fmt.Errorf("interface %q: %w", name, ErrInterfaceNotFound)
	}
	l := links[idx]
	if plen, ok := ownsIP(l, ip); ok {
		return l, plen, nil
	}
	if allowVirtual && isLoopbackLink(l) {
		if plen, ok := prefixContains(l, ip); ok {
			return l, plen, nil
		}
	}
	detail := "it has no IPv4 addresses"
	if addrs := inetAddrs(l); len(addrs) > 0 {
		detail = "it has " + strings.Join(addrs, ", ")
	}
	return parser.IPLink{}, 0, fmt.Errorf("%s: %w to interface %s (%s)", ip, ErrIPNotAssigned, l.Ifname, detail)
}

// ownsIP reports whether l has ip assigned (exact IPv4 addr_info match) and
// returns the assignment's prefix length.
func ownsIP(l parser.IPLink, ip netip.Addr) (int, bool) {
	for _, ai := range l.AddrInfo {
		if ai.Family != "inet" {
			continue
		}
		addr, err := netip.ParseAddr(ai.Local)
		if err != nil {
			continue
		}
		if addr == ip {
			return ai.Prefixlen, true
		}
	}
	return 0, false
}

// prefixContains reports whether one of l's IPv4 prefixes contains ip, and
// returns that prefix length.
func prefixContains(l parser.IPLink, ip netip.Addr) (int, bool) {
	for _, ai := range l.AddrInfo {
		if ai.Family != "inet" {
			continue
		}
		addr, err := netip.ParseAddr(ai.Local)
		if err != nil {
			continue
		}
		prefix, err := addr.Prefix(ai.Prefixlen)
		if err != nil {
			continue
		}
		if prefix.Contains(ip) {
			return ai.Prefixlen, true
		}
	}
	return 0, false
}

// isLoopbackLink reports whether ip metadata already marks l as loopback.
func isLoopbackLink(l parser.IPLink) bool {
	return l.LinkType == "loopback" || slices.Contains(l.Flags, "LOOPBACK")
}

// inetAddrs lists l's IPv4 assignments as "addr/plen" strings.
func inetAddrs(l parser.IPLink) []string {
	var out []string
	for _, ai := range l.AddrInfo {
		if ai.Family == "inet" {
			out = append(out, fmt.Sprintf("%s/%d", ai.Local, ai.Prefixlen))
		}
	}
	return out
}

// notAssignedError builds the selection-failure error: a same-subnet
// "did you mean" hint when one exists, otherwise the list of assigned IPv4
// addresses so the operator can spot the typo.
func notAssignedError(links []parser.IPLink, ip netip.Addr) error {
	var hints, assigned []string
	for _, l := range links {
		for _, ai := range l.AddrInfo {
			if ai.Family != "inet" {
				continue
			}
			assigned = append(assigned, fmt.Sprintf("%s/%d on %s", ai.Local, ai.Prefixlen, l.Ifname))
			addr, err := netip.ParseAddr(ai.Local)
			if err != nil {
				continue
			}
			prefix, err := addr.Prefix(ai.Prefixlen)
			if err != nil {
				continue
			}
			if prefix.Contains(ip) {
				hints = append(hints, fmt.Sprintf("%s has %s/%d in the same subnet", l.Ifname, ai.Local, ai.Prefixlen))
			}
		}
	}
	switch {
	case len(hints) > 0:
		return fmt.Errorf("%s: %w to any interface (%s — did you mean that?)",
			ip, ErrIPNotAssigned, strings.Join(hints, "; "))
	case len(assigned) > 0:
		return fmt.Errorf("%s: %w to any interface (assigned IPv4 addresses: %s)",
			ip, ErrIPNotAssigned, strings.Join(assigned, ", "))
	default:
		return fmt.Errorf("%s: %w to any interface (no IPv4 addresses found on this machine)",
			ip, ErrIPNotAssigned)
	}
}
