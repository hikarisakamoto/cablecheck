// Package model holds CableCheck's pure data types: the report structure
// that is the single JSON surface of a test run, plus the bitrate and
// duration value types used across the tool.
//
// The package imports only the standard library and contains no behavior
// beyond parsing/formatting of its value types, so every other package
// (parsers, test suite, evaluator, reporting) can depend on it without
// coupling to each other.
package model

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the current report schema version, written into every
// report's schemaVersion field. Consumers must check the major component.
const SchemaVersion = "1.0.0"

// Report is the complete record of one CableCheck run and the single source
// for all rendered outputs (report.json, report.md, summary.txt, report.html).
//
// The JSON field names of the spec fields are verbatim and stable; findings,
// rawFiles, link and machines are additive extensions. Durations use
// model.Duration (raw time.Duration is banned — a reflection test enforces
// it) and timestamps marshal as RFC 3339. The session token has no field
// anywhere in the report, so it cannot leak by construction.
type Report struct {
	// SchemaVersion is the report schema version, e.g. "1.0.0".
	SchemaVersion string `json:"schemaVersion"`
	// ToolVersion is the CableCheck version that produced the report.
	ToolVersion string `json:"toolVersion"`
	// ProtocolVersion is the peer protocol version used, e.g. "1".
	ProtocolVersion string `json:"protocolVersion"`
	// TestID is the session identifier, e.g. "ct-20260715-143205-a1b2c3d4".
	TestID string `json:"testId"`
	// StartedAt is when the coordinated test run started.
	StartedAt time.Time `json:"startedAt"`
	// FinishedAt is when the run finished (or was aborted).
	FinishedAt time.Time `json:"finishedAt"`
	// Duration is the wall-clock length of the run.
	Duration Duration `json:"duration"`
	// Configuration echoes the effective configuration (never the token).
	Configuration ConfigEcho `json:"configuration"`
	// PC1 describes the machine that ran as pc1 (coordinator).
	PC1 PeerReport `json:"pc1"`
	// PC2 describes the machine that ran as pc2 (worker).
	PC2 PeerReport `json:"pc2"`
	// Tests holds every test result of the run.
	Tests TestsSection `json:"tests"`
	// InitialCounters are the per-peer counter snapshots taken before testing.
	InitialCounters PeerCounters `json:"initialCounters"`
	// FinalCounters are the per-peer counter snapshots taken after testing.
	FinalCounters PeerCounters `json:"finalCounters"`
	// CycleCounters are the pre-load counter snapshots for each completed soak
	// cycle, in cycle order. They are omitted for quick and standard runs.
	CycleCounters []PeerCounters `json:"cycleCounters,omitempty"`
	// CounterDeltas are the per-peer counter changes across the run.
	CounterDeltas PeerCounterDeltas `json:"counterDeltas"`
	// MonitoringEvents is the timeline of link events observed during the run.
	MonitoringEvents []MonitoringEvent `json:"monitoringEvents"`
	// Warnings are non-fatal issues encountered during the run.
	Warnings []string `json:"warnings"`
	// SkippedTests lists planned tests that did not run, with reasons.
	SkippedTests []SkippedTest `json:"skippedTests"`
	// UDPRateAssumed is true when the UDP target rate used the unknown-link
	// fallback rather than being derived from a negotiated speed.
	UDPRateAssumed bool `json:"udpRateAssumed,omitempty"`
	// Classification is the overall health verdict.
	Classification HealthClass `json:"classification"`
	// Score is the 0-100 health score; nil (JSON null) for INCONCLUSIVE.
	Score *int `json:"score"`
	// ClassificationReasons are the one-line reasons behind the verdict.
	ClassificationReasons []string `json:"classificationReasons"`
	// Recommendations are deduplicated, actionable follow-ups.
	Recommendations []string `json:"recommendations"`
	// Partial is true when the run was interrupted and the report covers
	// only the tests that completed.
	Partial bool `json:"partial"`
	// SoakCyclesCompleted is the number of soak test cycles that ran to
	// completion. MarshalJSON includes it for every soak report, including
	// zero-cycle runs, while keeping it absent from other modes.
	SoakCyclesCompleted int `json:"soakCyclesCompleted,omitempty"`
	// Failure describes what broke, for abnormal endings.
	Failure *FailureDetails `json:"failure,omitempty"`

	// Findings is the full rule-evidence list backing the classification
	// (additive field).
	Findings []Finding `json:"findings,omitempty"`
	// RawFiles indexes the raw artifacts stored next to the report
	// (additive field).
	RawFiles []RawFileRef `json:"rawFiles,omitempty"`
	// Link holds before/after link negotiation state per peer
	// (additive field).
	Link *LinkSection `json:"link,omitempty"`
	// Machines duplicates the pc1/pc2 machine descriptions as a pair, for
	// consumers that want them under one key (additive field).
	Machines *MachinePair `json:"machines,omitempty"`
}

// MarshalJSON keeps soakCyclesCompleted mode-specific without letting
// omitempty erase a legitimate zero-cycle soak result.
func (r Report) MarshalJSON() ([]byte, error) {
	type reportAlias Report
	if r.Configuration.Mode != "soak" {
		return json.Marshal(reportAlias(r))
	}
	return json.Marshal(struct {
		reportAlias
		SoakCyclesCompleted int `json:"soakCyclesCompleted"`
	}{
		reportAlias:         reportAlias(r),
		SoakCyclesCompleted: r.SoakCyclesCompleted,
	})
}

// ConfigEcho echoes the effective run configuration into the report.
//
// It deliberately has NO token field: the session token is unrepresentable
// in the report, so it can never leak through rendering or transfer.
type ConfigEcho struct {
	// Role is "pc1" or "pc2" (the side that wrote this report).
	Role string `json:"role"`
	// LocalIP is the tested interface's IP on this side.
	LocalIP string `json:"localIp"`
	// PeerIP is the tested interface's IP on the other side.
	PeerIP string `json:"peerIp"`
	// Interface is the tested interface name (may be auto-discovered).
	Interface string `json:"interface,omitempty"`
	// Mode is "quick", "standard" or "soak".
	Mode string `json:"mode"`
	// ControlPort is the TCP port of the control connection.
	ControlPort uint16 `json:"controlPort"`
	// IperfPort is the base port used for iperf3 tests.
	IperfPort uint16 `json:"iperfPort"`
	// TCPDuration is the per-run TCP test duration.
	TCPDuration Duration `json:"tcpDuration"`
	// UDPDuration is the per-run UDP test duration.
	UDPDuration Duration `json:"udpDuration"`
	// UDPRate is the UDP target rate (auto-derived when not set explicitly).
	UDPRate Bitrate `json:"udpRate"`
	// ParallelStreams is the iperf3 -P value.
	ParallelStreams int `json:"parallelStreams"`
	// PingCount is the number of echo requests per ping test.
	PingCount int `json:"pingCount"`
	// PingInterval is the requested inter-packet ping interval.
	PingInterval Duration `json:"pingInterval"`
	// TCPRepeats is how many times each one-way TCP test runs.
	TCPRepeats int `json:"tcpRepeats"`
	// SoakDuration is the total soak length (soak mode only).
	SoakDuration Duration `json:"soakDuration,omitempty"`
	// SoakLoad is "periodic" or "continuous" (soak mode only).
	SoakLoad string `json:"soakLoad,omitempty"`
	// MonitorInterval is the sysfs link-monitor polling interval.
	MonitorInterval Duration `json:"monitorInterval"`
	// CableTest reports whether ethtool --cable-test was requested.
	CableTest bool `json:"cableTest"`
	// CableTestTDR reports whether ethtool --cable-test-tdr was requested.
	CableTestTDR bool `json:"cableTestTdr"`
	// Output is the parent directory configured for the report directory.
	Output string `json:"output"`
	// Verbose reports whether verbose logging was on.
	Verbose bool `json:"verbose"`
	// NonInteractive reports whether the run skipped interactive prompts.
	NonInteractive bool `json:"nonInteractive"`
	// NoSudo reports whether the sudo probe was skipped.
	NoSudo bool `json:"noSudo"`
	// NoReportTransfer reports whether the PC1->PC2 report copy was disabled.
	NoReportTransfer bool `json:"noReportTransfer"`
	// AllowVirtualInterface reports whether virtual/loopback interfaces
	// were permitted (demo/diagnostic mode).
	AllowVirtualInterface bool `json:"allowVirtualInterface"`
	// TokenGenerated is true when PC1 auto-generated the session token.
	// The token itself is never present in the report.
	TokenGenerated bool `json:"tokenGenerated"`
}

// NICReport describes the tested network interface of one peer.
type NICReport struct {
	// Name is the interface name, e.g. "enp3s0".
	Name string `json:"name"`
	// Driver is the kernel driver, e.g. "e1000e".
	Driver string `json:"driver"`
	// SpeedMbps is the negotiated speed in Mb/s; -1 when unknown.
	SpeedMbps int `json:"speedMbps"`
	// Duplex is "full", "half" or "unknown".
	Duplex string `json:"duplex"`
	// MTU is the configured MTU.
	MTU int `json:"mtu"`
	// MAC is the hardware address.
	MAC string `json:"mac"`
	// USB is true for USB-attached adapters (a known throughput limiter).
	USB bool `json:"usb"`
}

// PeerReport describes one peer machine and its environment.
type PeerReport struct {
	// Hostname is the machine's host name.
	Hostname string `json:"hostname"`
	// Kernel is the kernel release (uname -r).
	Kernel string `json:"kernel"`
	// OS is the platform, e.g. "linux/amd64".
	OS string `json:"os"`
	// NIC describes the tested interface.
	NIC NICReport `json:"nic"`
	// ToolVersions maps tool name to detected version, e.g.
	// {"iperf3": "3.16", "ethtool": "6.7", "ping": "iputils-20240117"}.
	ToolVersions map[string]string `json:"toolVersions"`
}

// MachinePair groups both peers' machine descriptions under one key.
type MachinePair struct {
	// PC1 describes the pc1 machine.
	PC1 PeerReport `json:"pc1"`
	// PC2 describes the pc2 machine.
	PC2 PeerReport `json:"pc2"`
}

// TestsSection groups every test result of the run. Absent tests (not part
// of the mode, or skipped) are simply empty/nil; the reasons live in
// Report.SkippedTests.
type TestsSection struct {
	// Ping holds the quick ping stability tests (one per direction).
	Ping []PingResult `json:"ping,omitempty"`
	// FullSizePing holds the full-MTU, don't-fragment ping tests.
	FullSizePing []PingResult `json:"fullSizePing,omitempty"`
	// TCP holds the one-way TCP throughput tests.
	TCP []TCPResult `json:"tcp,omitempty"`
	// UDP holds the one-way UDP loss/jitter tests.
	UDP []UDPResult `json:"udp,omitempty"`
	// Bidirectional holds the bidirectional stress test.
	Bidirectional *BidirResult `json:"bidirectional,omitempty"`
	// CableTest holds the ethtool cable diagnostics.
	CableTest *CableTestResult `json:"cableTest,omitempty"`
}

// LinkEndpoint is one peer's link state before and after the run.
type LinkEndpoint struct {
	// Before is the link state captured before testing.
	Before *LinkSettings `json:"before,omitempty"`
	// After is the link state captured after testing.
	After *LinkSettings `json:"after,omitempty"`
}

// LinkSection holds both peers' before/after link negotiation state.
type LinkSection struct {
	// PC1 is pc1's link state.
	PC1 LinkEndpoint `json:"pc1"`
	// PC2 is pc2's link state.
	PC2 LinkEndpoint `json:"pc2"`
}
