package app

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"cablecheck/internal/config"
	"cablecheck/internal/model"
	"cablecheck/internal/network"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner"
	"cablecheck/internal/testsuite"
)

// preflightInfo is everything preflight measured: the interface under test,
// its link state, the local tool inventory and the capabilities advertised
// to the peer in the handshake.
type preflightInfo struct {
	// Iface is the discovered and classified interface under test.
	Iface network.Iface
	// Link is the parsed ethtool link state (zero value when unreadable).
	Link model.LinkSettings
	// LinkObservations are the human-worded link findings from ObserveLink.
	LinkObservations []string
	// Caps is the capability set exchanged in the handshake.
	Caps protocol.Capabilities
	// LocalIperfCaps is this side's detected iperf3 capability set; the
	// quick plan ANDs it with the peer's answer to choose the native
	// --bidir path or the two-phase fallback, and the worker serves it via
	// OpIperfCaps.
	LocalIperfCaps model.Iperf3Caps
	// ToolVersions maps tool name to its detected version string.
	ToolVersions map[string]string
	// Warnings are non-fatal preflight findings, echoed into the report.
	Warnings []string
}

// requiredTools maps each required executable to its distro package names
// (Arch, Debian/Ubuntu, Fedora) for the install hint.
var requiredTools = []struct {
	name             string
	pacman, apt, dnf string
}{
	{"ip", "iproute2", "iproute2", "iproute"},
	{"ping", "iputils", "iputils-ping", "iputils"},
	{"iperf3", "iperf3", "iperf3", "iperf3"},
	{"ethtool", "ethtool", "ethtool", "ethtool"},
}

// probeTimeout bounds every preflight tool invocation.
const probeTimeout = 10 * time.Second

// preflight runs the ordered local checks (docs/design/clieval.md §6 order,
// run-scoped subset): required tools present, ping is iputils, interface
// discovery and classification, interface up, iperf data ports free,
// stale-process scan, output directory writable, sudo probe, iperf3
// capability detection. The results feed protocol.Capabilities. Every
// returned error is a configuration/dependency failure (exit 4).
func (a *App) preflight(ctx context.Context) (*preflightInfo, error) {
	r := a.deps.Runner
	pf := &preflightInfo{ToolVersions: map[string]string{}}

	// 1. Required tools on PATH, with install hints.
	for _, tool := range requiredTools {
		if _, err := r.LookPath(tool.name); err != nil {
			return nil, fmt.Errorf(
				"required tool %q not found in PATH (install it: pacman -S %s | apt install %s | dnf install %s): %w",
				tool.name, tool.pacman, tool.apt, tool.dnf, err)
		}
	}

	// 2. ping must be iputils: busybox ping lacks the flags and the output
	// grammar every parser relies on.
	pingV, err := r.Run(ctx, runner.CommandSpec{
		Name: "ping", Args: []string{"-V"}, Timeout: probeTimeout, Label: "ping-version",
	})
	if err != nil {
		return nil, fmt.Errorf("probe ping version: %w", err)
	}
	pingVariant := firstLine(pingV.Stdout)
	if !strings.Contains(pingVariant, "iputils") {
		return nil, fmt.Errorf(
			"unsupported ping implementation %q: cablecheck requires iputils ping (busybox ping is not supported)",
			pingVariant)
	}
	pf.ToolVersions["ping"] = pingVariant

	// 3. Local IP on this machine + interface discovery and classification.
	iface, err := network.Discover(ctx, r, a.cfg.LocalIP, network.DiscoverOptions{
		Interface:    a.cfg.Interface,
		AllowVirtual: a.cfg.AllowVirtualInterface,
		SysfsRoot:    a.sysfsRoot,
	})
	if err != nil {
		return nil, fmt.Errorf("interface discovery: %w", err)
	}
	pf.Iface = iface
	if iface.Class.Virtual || iface.Class.Loopback || iface.Class.Wireless {
		kind := "virtual"
		switch {
		case iface.Class.Loopback:
			kind = "loopback (virtual)"
		case iface.Class.Wireless:
			kind = "wireless (virtual)"
		}
		warning := fmt.Sprintf(
			"interface %s is a %s interface (%s); results cannot prove anything about a physical cable",
			iface.Name, kind, iface.Class.Reason)
		pf.Warnings = append(pf.Warnings, warning)
		fmt.Fprintf(a.deps.Stdout, "\n*** WARNING: %s ***\n\n", warning)
	}

	// 4. Interface must not be administratively/physically down. The
	// loopback's "unknown" operstate is tolerated (demo mode).
	if iface.Operstate == "down" {
		return nil, fmt.Errorf("interface %s is down (operstate %q); bring the link up first",
			iface.Name, iface.Operstate)
	}

	// 5. Data ports free. The control port needs no probe here: PC1 already
	// holds it (Start bound the listener — binding is the check), and PC2
	// only dials it.
	for _, port := range []uint16{a.cfg.IperfPort, a.cfg.IperfPort + 1} {
		if err := network.ProbePortFree(a.cfg.LocalIP.Unmap(), port); err != nil {
			return nil, fmt.Errorf("iperf3 data port check: %w", err)
		}
	}

	// 6. Stale cablecheck-owned processes from earlier runs.
	if _, err := testsuite.PreflightStaleProcesses(a.stateDir()); err != nil {
		return nil, fmt.Errorf("stale process scan: %w", err)
	}

	// 7. Report parent directory writable (temp-file probe).
	probe, err := os.CreateTemp(a.cfg.OutputDir, ".cablecheck-writable-*")
	if err != nil {
		return nil, fmt.Errorf("output directory %s is not writable: %w", a.cfg.OutputDir, err)
	}
	probe.Close()
	if err := os.Remove(probe.Name()); err != nil {
		pf.Warnings = append(pf.Warnings, fmt.Sprintf("could not remove writability probe %s: %v", probe.Name(), err))
	}

	// 8. Passwordless sudo (skipped with --no-sudo). Only ever needed for
	// the opt-in cable tests, so unavailability is a warning, not an error.
	sudoOK := false
	if !a.cfg.NoSudo {
		sudoOK = testsuite.SudoProbe(ctx, r)
		if !sudoOK {
			pf.Warnings = append(pf.Warnings,
				"passwordless sudo unavailable; cable diagnostics (--cable-test) would be skipped")
		}
	}

	// 9. iperf3 capability detection (version window + --help agreement).
	iperfCaps, err := testsuite.DetectIperfCaps(ctx, r)
	if err != nil {
		return nil, err
	}
	pf.ToolVersions["iperf3"] = iperfCaps.Version
	pf.LocalIperfCaps = iperfCaps

	// 10. Remaining tool versions and link state (best effort).
	if res, err := r.Run(ctx, runner.CommandSpec{
		Name: "ethtool", Args: []string{"--version"}, Timeout: probeTimeout, Label: "ethtool-version",
	}); err == nil {
		pf.ToolVersions["ethtool"] = firstLine(res.Stdout)
	}
	link := &testsuite.LinkInspector{R: r, IfName: iface.Name}
	if settings, obs, err := link.Inspect(ctx); err == nil {
		pf.Link = settings
		pf.LinkObservations = obs
	} else {
		pf.Link.SpeedMbps = -1
		pf.Link.Duplex = "unknown"
		pf.Warnings = append(pf.Warnings, fmt.Sprintf("could not read link settings for %s: %v", iface.Name, err))
	}

	pf.Caps = protocol.Capabilities{
		CablecheckVersion: a.deps.Build.Version,
		OS:                runtime.GOOS + "/" + runtime.GOARCH,
		Kernel:            kernelRelease(),
		Iperf3:            toProtocolIperfCaps(iperfCaps),
		EthtoolVersion:    pf.ToolVersions["ethtool"],
		PingVariant:       pingVariant,
		NIC: protocol.NICInfo{
			Name:      iface.Name,
			Driver:    iface.Class.Driver,
			SpeedMbps: pf.Link.SpeedMbps,
			Duplex:    pf.Link.Duplex,
			MTU:       iface.MTU,
			MAC:       iface.MAC,
		},
		SudoAvailable:        sudoOK,
		CableTestSupported:   false, // cable diagnostics land in a later version
		AcceptReportTransfer: !a.cfg.NoReportTransfer,
	}
	return pf, nil
}

// stateDir returns the pidfile state tree to scan for stale processes.
func (a *App) stateDir() string {
	if a.deps.StateDir != "" {
		return a.deps.StateDir
	}
	return runner.DefaultBaseDir()
}

// toProtocolIperfCaps converts the detected iperf3 capabilities into the
// handshake payload shape.
func toProtocolIperfCaps(c model.Iperf3Caps) protocol.Iperf3Caps {
	return protocol.Iperf3Caps{
		Version:         c.Version,
		JSON:            c.JSON,
		Reverse:         c.Reverse,
		Bidir:           c.Bidir,
		GetServerOutput: c.GetServerOutput,
		UDP:             c.UDP,
		OneOff:          c.OneOff,
	}
}

// kernelRelease reads the running kernel version; "" when unreadable.
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// firstLine returns the trimmed first line of b.
func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(line)
}

// echoConfig builds the report's configuration echo (never the token) from
// the resolved run configuration; controlPort is the effective bound port.
func echoConfig(cfg *config.RunConfig, controlPort uint16, ifName string) model.ConfigEcho {
	return model.ConfigEcho{
		Role:                  string(cfg.Role),
		LocalIP:               cfg.LocalIP.Unmap().String(),
		PeerIP:                cfg.PeerIP.Unmap().String(),
		Interface:             ifName,
		Mode:                  string(cfg.Mode),
		ControlPort:           controlPort,
		IperfPort:             cfg.IperfPort,
		TCPDuration:           model.Duration(cfg.TCPDuration),
		UDPDuration:           model.Duration(cfg.UDPDuration),
		UDPRate:               cfg.UDPRate,
		ParallelStreams:       cfg.ParallelStreams,
		PingCount:             cfg.PingCount,
		PingInterval:          model.Duration(cfg.PingInterval),
		TCPRepeats:            cfg.TCPRepeats,
		SoakDuration:          model.Duration(cfg.SoakDuration),
		SoakLoad:              string(cfg.SoakLoad),
		MonitorInterval:       model.Duration(cfg.MonitorInterval),
		CableTest:             cfg.CableTest,
		CableTestTDR:          cfg.CableTestTDR,
		Output:                cfg.OutputDir,
		Verbose:               cfg.Verbose,
		NonInteractive:        cfg.NonInteractive,
		NoSudo:                cfg.NoSudo,
		NoReportTransfer:      cfg.NoReportTransfer,
		AllowVirtualInterface: cfg.AllowVirtualInterface,
		TokenGenerated:        cfg.TokenGenerated,
	}
}
