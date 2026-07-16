// Package config validates and resolves the cablecheck run configuration.
// Raw flag values are checked fail-fast in a fixed order (mode, role, IPs,
// ports, token, presets and bounds, UDP rate, output directory), mode
// presets fill every test parameter the user did not set explicitly, and
// the session token is generated on PC1 when absent. The package performs
// no network I/O and imports only internal/model and the standard library.
package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"cablecheck/internal/model"
)

// Role identifies which side of the test this process runs as.
type Role string

// Valid roles: PC1 coordinates the test plan, PC2 executes requests.
const (
	// RolePC1 is the coordinator; it drives the test plan and generates
	// the session token when none is supplied.
	RolePC1 Role = "pc1"
	// RolePC2 is the worker; it requires the token displayed by PC1.
	RolePC2 Role = "pc2"
)

// Mode selects the test depth and duration profile.
type Mode string

// Valid modes, from fastest to longest-running.
const (
	// ModeQuick runs a short single-pass test (about two minutes).
	ModeQuick Mode = "quick"
	// ModeStandard runs longer transfers with repeats and full monitoring.
	ModeStandard Mode = "standard"
	// ModeSoak repeats test cycles for a configured wall-clock duration.
	ModeSoak Mode = "soak"
)

// SoakLoad selects how traffic is generated during a soak run.
type SoakLoad string

// Valid soak load profiles.
const (
	// SoakLoadPeriodic runs test cycles with idle gaps between them.
	SoakLoadPeriodic SoakLoad = "periodic"
	// SoakLoadContinuous keeps traffic flowing for the whole soak window.
	SoakLoadContinuous SoakLoad = "continuous"
)

// RunConfig is the fully validated and preset-resolved configuration for a
// `cablecheck run` invocation. All fields are ready to use: IPs are parsed
// and unmapped IPv4, durations are bounds-checked, derived parameters
// (PingCount, PingInterval, TCPRepeats) are filled from the mode preset,
// and OutputDir is an absolute cleaned path to an existing directory.
type RunConfig struct {
	// Role is pc1 (coordinator) or pc2 (worker).
	Role Role
	// LocalIP is the validated IPv4 address of this machine's test interface.
	LocalIP netip.Addr
	// PeerIP is the validated IPv4 address of the other machine.
	PeerIP netip.Addr
	// Interface optionally overrides interface auto-discovery ("" = discover).
	Interface string
	// Mode is the resolved test mode.
	Mode Mode
	// ControlPort is the TCP port for the control protocol.
	ControlPort uint16
	// IperfPort is the base iperf3 port; IperfPort+1 is reserved for the
	// bidirectional fallback.
	IperfPort uint16
	// Token is the shared session secret. Never log it directly; RunConfig's
	// LogValue redacts it.
	Token string
	// TokenGenerated is true when Token was auto-generated on PC1 and must
	// be displayed to the user for copying to PC2.
	TokenGenerated bool
	// TCPDuration is the length of each TCP throughput phase.
	TCPDuration time.Duration
	// UDPDuration is the length of each UDP loss/jitter phase.
	UDPDuration time.Duration
	// UDPRate is the explicit UDP send rate; 0 means auto-derive 80% of the
	// negotiated link speed at run time (see DefaultUDPRate).
	UDPRate model.Bitrate
	// ParallelStreams is the iperf3 -P stream count (1-16).
	ParallelStreams int
	// PingCount is the number of echo requests per ping phase (derived from
	// the mode preset; not a flag).
	PingCount int
	// PingInterval is the spacing between echo requests (derived; not a flag).
	PingInterval time.Duration
	// TCPRepeats is how many times each TCP phase runs (derived; in soak
	// mode it applies per cycle).
	TCPRepeats int
	// SoakDuration is the total soak wall-clock budget (soak mode only;
	// zero otherwise).
	SoakDuration time.Duration
	// SoakLoad is the soak traffic profile (soak mode only; empty otherwise).
	SoakLoad SoakLoad
	// MonitorInterval is the sysfs link-monitor polling period.
	MonitorInterval time.Duration
	// CableTest enables the opt-in ethtool --cable-test diagnostic.
	CableTest bool
	// CableTestTDR enables ethtool --cable-test-tdr (implies CableTest).
	CableTestTDR bool
	// OutputDir is the absolute, cleaned parent directory for the report
	// directory. It exists and is a directory.
	OutputDir string
	// Verbose enables verbose progress output.
	Verbose bool
	// NonInteractive skips the interactive start prompt.
	NonInteractive bool
	// NoSudo skips the sudo availability probe (cable tests become
	// unavailable rather than prompting).
	NoSudo bool
	// NoReportTransfer disables sending the report files to PC2.
	NoReportTransfer bool
	// AllowVirtualInterface permits loopback/virtual interfaces (and
	// loopback IPs) for single-machine demos.
	AllowVirtualInterface bool
}

// LogValue implements slog.LogValuer so that logging a RunConfig can never
// leak the session token: the token is replaced by a redaction marker and
// every other field is emitted as a structured attribute.
func (c RunConfig) LogValue() slog.Value {
	token := ""
	if c.Token != "" {
		token = "[REDACTED]"
	}
	return slog.GroupValue(
		slog.String("role", string(c.Role)),
		slog.String("localIP", c.LocalIP.String()),
		slog.String("peerIP", c.PeerIP.String()),
		slog.String("interface", c.Interface),
		slog.String("mode", string(c.Mode)),
		slog.Int("controlPort", int(c.ControlPort)),
		slog.Int("iperfPort", int(c.IperfPort)),
		slog.String("token", token),
		slog.Bool("tokenGenerated", c.TokenGenerated),
		slog.Duration("tcpDuration", c.TCPDuration),
		slog.Duration("udpDuration", c.UDPDuration),
		slog.String("udpRate", c.UDPRate.String()),
		slog.Int("parallelStreams", c.ParallelStreams),
		slog.Int("pingCount", c.PingCount),
		slog.Duration("pingInterval", c.PingInterval),
		slog.Int("tcpRepeats", c.TCPRepeats),
		slog.Duration("soakDuration", c.SoakDuration),
		slog.String("soakLoad", string(c.SoakLoad)),
		slog.Duration("monitorInterval", c.MonitorInterval),
		slog.Bool("cableTest", c.CableTest),
		slog.Bool("cableTestTDR", c.CableTestTDR),
		slog.String("outputDir", c.OutputDir),
		slog.Bool("verbose", c.Verbose),
		slog.Bool("nonInteractive", c.NonInteractive),
		slog.Bool("noSudo", c.NoSudo),
		slog.Bool("noReportTransfer", c.NoReportTransfer),
		slog.Bool("allowVirtualInterface", c.AllowVirtualInterface),
	)
}

// RawRunFlags holds the raw values parsed from the run subcommand's flag
// set, before any validation or mode-preset resolution. Field names mirror
// the flags; sentinel zero values mean "let the mode preset decide" for the
// fields tracked by the explicitlySet map passed to Resolve.
type RawRunFlags struct {
	// Role is the raw --role value.
	Role string
	// LocalIP is the raw --local-ip value.
	LocalIP string
	// PeerIP is the raw --peer-ip value.
	PeerIP string
	// Interface is the raw --interface value ("" = auto-discover).
	Interface string
	// Mode is the raw --mode value (registered default "quick").
	Mode string
	// ControlPort is the raw --control-port value.
	ControlPort int
	// IperfPort is the raw --iperf-port value.
	IperfPort int
	// Token is the raw --token value ("" = generate on pc1, error on pc2).
	Token string
	// TCPDuration is the raw --tcp-duration value (0 = preset).
	TCPDuration time.Duration
	// UDPDuration is the raw --udp-duration value (0 = preset).
	UDPDuration time.Duration
	// UDPRate is the raw --udp-rate bitrate string ("" = auto at run time).
	UDPRate string
	// ParallelStreams is the raw --parallel-streams value (0 = preset).
	ParallelStreams int
	// SoakDuration is the raw --soak-duration value (soak mode only).
	SoakDuration time.Duration
	// SoakLoad is the raw --soak-load value (soak mode only).
	SoakLoad string
	// MonitorInterval is the raw --monitor-interval value (0 = preset).
	MonitorInterval time.Duration
	// CableTest is the raw --cable-test value.
	CableTest bool
	// CableTestTDR is the raw --cable-test-tdr value.
	CableTestTDR bool
	// Output is the raw --output parent-directory value.
	Output string
	// Verbose is the raw --verbose value.
	Verbose bool
	// NonInteractive is the raw --non-interactive value.
	NonInteractive bool
	// NoSudo is the raw --no-sudo value.
	NoSudo bool
	// NoReportTransfer is the raw --no-report-transfer value.
	NoReportTransfer bool
	// AllowVirtualInterface is the raw --allow-virtual-interface value.
	AllowVirtualInterface bool
}

// ValidationError reports a single invalid flag value. Flag carries the
// user-facing flag name including dashes (for example "--local-ip") so the
// error reads exactly like the command line the user typed.
type ValidationError struct {
	// Flag is the offending flag, including leading dashes.
	Flag string
	// Msg explains what is wrong and what would be accepted.
	Msg string
}

// Error formats the error as "--flag: message".
func (e *ValidationError) Error() string {
	return e.Flag + ": " + e.Msg
}

// UDP rate derivation constants (bits per second).
const (
	// udpRateFloor is the minimum auto-derived UDP rate.
	udpRateFloor model.Bitrate = 10_000_000
	// udpUnknownLinkRate is the fallback UDP rate when the negotiated link
	// speed is unknown.
	udpUnknownLinkRate model.Bitrate = 100_000_000
)

// DefaultUDPRate derives the automatic UDP test rate from the negotiated
// link speed: 80% of the negotiated speed, raised to a 10M floor, then
// capped at 95% of the negotiated speed — the cap wins over the floor, so a
// 10M link yields 9.5M. A negotiated speed of 0 means the link speed is
// unknown (ethtool unavailable or virtual interface); the rate falls back
// to 100M and the returned warning explains that UDP loss results may be
// unreliable.
func DefaultUDPRate(negotiated model.Bitrate) (model.Bitrate, []string) {
	if negotiated == 0 {
		return udpUnknownLinkRate, []string{
			"link speed unknown; defaulting UDP rate to 100M — UDP loss results may be unreliable",
		}
	}
	rate := negotiated * 80 / 100
	if rate < udpRateFloor {
		rate = udpRateFloor
	}
	if ceiling := negotiated * 95 / 100; rate > ceiling {
		rate = ceiling
	}
	return rate, nil
}

// CheckExplicitUDPRate reports whether an explicitly requested UDP rate
// exceeds 95% of the known negotiated link speed. Such a rate is still
// honored, but the caller must surface the returned warning and record the
// near-saturation flag so the evaluator does not blame the cable for
// self-inflicted UDP loss. A negotiated speed of 0 (unknown) yields no
// verdict.
func CheckExplicitUDPRate(rate, negotiated model.Bitrate) (nearSaturation bool, warnings []string) {
	if negotiated == 0 || rate <= negotiated*95/100 {
		return false, nil
	}
	return true, []string{fmt.Sprintf(
		"requested UDP rate %s exceeds 95%% of the negotiated link speed %s; UDP loss at self-inflicted saturation does not indicate a faulty cable",
		rate, negotiated,
	)}
}
