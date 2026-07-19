package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cablecheck/internal/model"
)

// Validation bounds (design clieval.md §2.1).
const (
	minPort = 1024
	maxPort = 65535

	minTestDuration = 5 * time.Second
	maxTestDuration = 10 * time.Minute

	minMonitorInterval = 200 * time.Millisecond
	maxMonitorInterval = 30 * time.Second

	minSoakDuration = 60 * time.Second
	maxSoakDuration = 24 * time.Hour

	minParallelStreams = 1
	maxParallelStreams = 16

	minTokenLen = 6
	maxTokenLen = 128

	minUDPRate model.Bitrate = 1_000_000
	maxUDPRate model.Bitrate = 40_000_000_000
)

// Resolve validates raw run flags fail-fast in the canonical order (mode,
// role, local IP, peer IP, IP distinctness, ports, token, mode presets and
// bounds, UDP rate, cable-test implication, output directory) and returns
// the fully resolved configuration.
//
// explicitlySet maps flag names (without dashes, as reported by flag.Visit)
// to true for every flag the user set on the command line; it is the sole
// source of truth for "explicit value wins over preset". A nil map means no
// flag was set explicitly. Errors are *ValidationError except for internal
// failures such as the crypto/rand token source being unavailable.
func Resolve(raw RawRunFlags, explicitlySet map[string]bool) (*RunConfig, error) {
	isSet := func(name string) bool { return explicitlySet[name] }

	// 1. Mode (needed before presets).
	mode := Mode(raw.Mode)
	p, ok := presets[mode]
	if !ok {
		return nil, &ValidationError{"--mode", fmt.Sprintf("invalid mode %q: must be quick, standard, or soak", raw.Mode)}
	}

	// 2. Role.
	role := Role(raw.Role)
	switch role {
	case RolePC1, RolePC2:
	case "":
		return nil, &ValidationError{"--role", "required: pc1 or pc2"}
	default:
		return nil, &ValidationError{"--role", fmt.Sprintf("invalid role %q: must be pc1 or pc2", raw.Role)}
	}

	// 3-4. IP addresses.
	localIP, err := parseIPv4("--local-ip", raw.LocalIP, raw.AllowVirtualInterface)
	if err != nil {
		return nil, err
	}
	peerIP, err := parseIPv4("--peer-ip", raw.PeerIP, raw.AllowVirtualInterface)
	if err != nil {
		return nil, err
	}

	// 5. The two ends must differ.
	if localIP == peerIP {
		return nil, &ValidationError{"--peer-ip", fmt.Sprintf("must differ from --local-ip (both are %q)", localIP)}
	}

	// 6. Ports.
	controlPort, err := validatePort("--control-port", raw.ControlPort)
	if err != nil {
		return nil, err
	}
	iperfPort, err := validatePort("--iperf-port", raw.IperfPort)
	if err != nil {
		return nil, err
	}
	if raw.IperfPort > maxPort-1 {
		return nil, &ValidationError{"--iperf-port", fmt.Sprintf("must be at most %d: port+1 is used by the bidirectional fallback (got %d)", maxPort-1, raw.IperfPort)}
	}
	if raw.ControlPort == raw.IperfPort || raw.ControlPort == raw.IperfPort+1 {
		return nil, &ValidationError{"--control-port", fmt.Sprintf("must not equal --iperf-port (%d) or --iperf-port+1 (%d); those ports are reserved for iperf3", raw.IperfPort, raw.IperfPort+1)}
	}

	// 7. Token.
	token, tokenGenerated, err := resolveToken(role, raw.Token)
	if err != nil {
		return nil, err
	}

	// 8. Mode presets, then bounds.
	tcpDuration := p.tcpDuration
	if isSet("tcp-duration") {
		tcpDuration = raw.TCPDuration
	}
	udpDuration := p.udpDuration
	if isSet("udp-duration") {
		udpDuration = raw.UDPDuration
	}
	parallelStreams := p.parallelStreams
	if isSet("parallel-streams") {
		parallelStreams = raw.ParallelStreams
	}
	monitorInterval := p.monitorInterval
	if isSet("monitor-interval") {
		monitorInterval = raw.MonitorInterval
	}

	var soakDuration time.Duration
	var soakLoad SoakLoad
	if mode == ModeSoak {
		soakDuration = p.soakDuration
		if isSet("soak-duration") {
			soakDuration = raw.SoakDuration
		}
		soakLoad = p.soakLoad
		if isSet("soak-load") {
			soakLoad = SoakLoad(raw.SoakLoad)
		}
		switch soakLoad {
		case SoakLoadPeriodic, SoakLoadContinuous:
		default:
			return nil, &ValidationError{"--soak-load", fmt.Sprintf("invalid soak load %q: must be periodic or continuous", raw.SoakLoad)}
		}
		if err := checkDurationBounds("--soak-duration", soakDuration, minSoakDuration, maxSoakDuration); err != nil {
			return nil, err
		}
	} else {
		if isSet("soak-duration") {
			return nil, &ValidationError{"--soak-duration", "only valid with --mode soak"}
		}
		if isSet("soak-load") {
			return nil, &ValidationError{"--soak-load", "only valid with --mode soak"}
		}
	}

	if err := checkDurationBounds("--tcp-duration", tcpDuration, minTestDuration, maxTestDuration); err != nil {
		return nil, err
	}
	if err := checkDurationBounds("--udp-duration", udpDuration, minTestDuration, maxTestDuration); err != nil {
		return nil, err
	}
	if err := checkDurationBounds("--monitor-interval", monitorInterval, minMonitorInterval, maxMonitorInterval); err != nil {
		return nil, err
	}
	if parallelStreams < minParallelStreams || parallelStreams > maxParallelStreams {
		return nil, &ValidationError{"--parallel-streams", fmt.Sprintf("must be between %d and %d (got %d)", minParallelStreams, maxParallelStreams, parallelStreams)}
	}

	// 9. Explicit UDP rate.
	var udpRate model.Bitrate
	if raw.UDPRate != "" {
		udpRate, err = model.ParseBitrate(raw.UDPRate)
		if err != nil {
			return nil, &ValidationError{"--udp-rate", fmt.Sprintf("invalid bitrate %q: %v", raw.UDPRate, err)}
		}
		if udpRate < minUDPRate || udpRate > maxUDPRate {
			return nil, &ValidationError{"--udp-rate", fmt.Sprintf("must be between %s and %s (got %s)", minUDPRate, maxUDPRate, udpRate)}
		}
	}

	// 10. --cable-test-tdr implies --cable-test.
	cableTest := raw.CableTest || raw.CableTestTDR

	// 11. Output directory.
	outputDir, err := validateOutputDir(raw.Output)
	if err != nil {
		return nil, err
	}

	return &RunConfig{
		Role:                  role,
		LocalIP:               localIP,
		PeerIP:                peerIP,
		Interface:             raw.Interface,
		Mode:                  mode,
		ControlPort:           controlPort,
		IperfPort:             iperfPort,
		Token:                 token,
		TokenGenerated:        tokenGenerated,
		TCPDuration:           tcpDuration,
		UDPDuration:           udpDuration,
		UDPRate:               udpRate,
		ParallelStreams:       parallelStreams,
		PingCount:             p.pingCount,
		PingInterval:          p.pingInterval,
		TCPRepeats:            p.tcpRepeats,
		SoakDuration:          soakDuration,
		SoakLoad:              soakLoad,
		MonitorInterval:       monitorInterval,
		CableTest:             cableTest,
		CableTestTDR:          raw.CableTestTDR,
		OutputDir:             outputDir,
		Verbose:               raw.Verbose,
		NonInteractive:        raw.NonInteractive,
		NoSudo:                raw.NoSudo,
		NoReportTransfer:      raw.NoReportTransfer,
		AllowVirtualInterface: raw.AllowVirtualInterface,
	}, nil
}

// parseIPv4 validates one IP flag value: it must parse, carry no zone,
// unmap to IPv4 (IPv4-mapped IPv6 input is accepted and unmapped), and be
// neither unspecified nor multicast. Loopback addresses are rejected unless
// allowLoopback is set (the single-machine demo path).
func parseIPv4(flagName, value string, allowLoopback bool) (netip.Addr, error) {
	if value == "" {
		return netip.Addr{}, &ValidationError{flagName, "required: the interface's IPv4 address"}
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, &ValidationError{flagName, fmt.Sprintf("not a valid IPv4 address (%q): %v", value, err)}
	}
	if addr.Zone() != "" {
		return netip.Addr{}, &ValidationError{flagName, fmt.Sprintf("not a valid IPv4 address (%q): zoned addresses are not supported", value)}
	}
	addr = addr.Unmap()
	if !addr.Is4() {
		return netip.Addr{}, &ValidationError{flagName, fmt.Sprintf("not a valid IPv4 address (%q): IPv6 is not supported yet; use the interface's IPv4 address", value)}
	}
	if addr.IsUnspecified() {
		return netip.Addr{}, &ValidationError{flagName, "the unspecified address 0.0.0.0 cannot be used; give the interface's real IPv4 address"}
	}
	if addr.IsMulticast() {
		return netip.Addr{}, &ValidationError{flagName, fmt.Sprintf("multicast address %q cannot be used", value)}
	}
	if addr.IsLoopback() && !allowLoopback {
		return netip.Addr{}, &ValidationError{flagName, fmt.Sprintf("loopback address %q requires --allow-virtual-interface (single-machine demo only)", value)}
	}
	return addr, nil
}

// validatePort checks that p is a usable unprivileged port (1024-65535).
// Ports 1-1023 get the dedicated "privileged ports" message; everything
// else out of range gets a bounds message.
func validatePort(flagName string, p int) (uint16, error) {
	switch {
	case p >= 1 && p < minPort:
		return 0, &ValidationError{flagName, fmt.Sprintf("privileged ports are not supported (got %d; use %d-%d)", p, minPort, maxPort)}
	case p < 1 || p > maxPort:
		return 0, &ValidationError{flagName, fmt.Sprintf("must be between %d and %d (got %d)", minPort, maxPort, p)}
	}
	return uint16(p), nil
}

// checkDurationBounds returns a ValidationError when d lies outside
// [minBound, maxBound] (inclusive).
func checkDurationBounds(flagName string, d, minBound, maxBound time.Duration) error {
	if d < minBound || d > maxBound {
		return &ValidationError{flagName, fmt.Sprintf("must be between %v and %v (got %v)", minBound, maxBound, d)}
	}
	return nil
}

// resolveToken applies the token rules: an empty token is generated on PC1
// (and flagged as generated so the CLI displays it) and rejected on PC2; a
// provided token must be 6-128 printable-ASCII characters with no
// whitespace.
func resolveToken(role Role, token string) (string, bool, error) {
	if token == "" {
		if role == RolePC2 {
			return "", false, &ValidationError{"--token", "required on pc2; copy the token displayed by pc1"}
		}
		generated, err := generateToken()
		if err != nil {
			return "", false, fmt.Errorf("generating session token: %w", err)
		}
		return generated, true, nil
	}
	if len(token) < minTokenLen || len(token) > maxTokenLen {
		return "", false, &ValidationError{"--token", fmt.Sprintf("must be %d-%d characters (got %d)", minTokenLen, maxTokenLen, len(token))}
	}
	for i := 0; i < len(token); i++ {
		if c := token[i]; c < '!' || c > '~' {
			return "", false, &ValidationError{"--token", "must contain only printable ASCII characters with no whitespace"}
		}
	}
	return token, false, nil
}

// generateToken returns a 6-digit numeric session token from crypto/rand,
// uniform over 000000-999999. It is deliberately short so it is easy to read
// aloud and retype on the second PC: the token guards a trusted direct link
// against accidental cross-connection, not an untrusted network (see the
// security notes). This is the one sanctioned crypto/rand use in this package.
func generateToken() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// validateOutputDir rejects any ".." path element in the raw value (before
// cleaning, so traversal cannot be smuggled through Clean), resolves the
// path to an absolute cleaned form, and requires it to exist and be a
// directory. Writability probing belongs to preflight, not config.
func validateOutputDir(raw string) (string, error) {
	if raw == "" {
		raw = "."
	}
	for _, elem := range strings.Split(raw, string(filepath.Separator)) {
		if elem == ".." {
			return "", &ValidationError{"--output", fmt.Sprintf("must not contain %q path elements (got %q)", "..", raw)}
		}
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", &ValidationError{"--output", fmt.Sprintf("cannot resolve %q: %v", raw, err)}
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", &ValidationError{"--output", fmt.Sprintf("directory %q does not exist", abs)}
	case err != nil:
		return "", &ValidationError{"--output", fmt.Sprintf("cannot stat %q: %v", abs, err)}
	case !info.IsDir():
		return "", &ValidationError{"--output", fmt.Sprintf("%q is not a directory", abs)}
	}
	return abs, nil
}
