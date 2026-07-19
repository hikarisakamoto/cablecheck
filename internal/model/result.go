package model

import "time"

// Direction labels for per-direction test results.
const (
	// DirectionPC1ToPC2 marks traffic flowing from PC1 to PC2.
	DirectionPC1ToPC2 = "pc1_to_pc2"
	// DirectionPC2ToPC1 marks traffic flowing from PC2 to PC1.
	DirectionPC2ToPC1 = "pc2_to_pc1"
)

// CPUUsage is iperf3's cpu_utilization_percent block: percentages on the
// local (host) side and the remote side of a test.
type CPUUsage struct {
	// HostTotal is total CPU % on the side running the iperf3 client.
	HostTotal float64 `json:"hostTotal"`
	// HostUser is user-space CPU % on the client side.
	HostUser float64 `json:"hostUser"`
	// HostSystem is kernel CPU % on the client side.
	HostSystem float64 `json:"hostSystem"`
	// RemoteTotal is total CPU % on the server side.
	RemoteTotal float64 `json:"remoteTotal"`
	// RemoteUser is user-space CPU % on the server side.
	RemoteUser float64 `json:"remoteUser"`
	// RemoteSystem is kernel CPU % on the server side.
	RemoteSystem float64 `json:"remoteSystem"`
}

// TCPInterval is one per-second iperf3 interval summary row.
type TCPInterval struct {
	// StartSec is the interval start offset in seconds.
	StartSec float64 `json:"startSec"`
	// EndSec is the interval end offset in seconds.
	EndSec float64 `json:"endSec"`
	// Bytes transferred during the interval.
	Bytes uint64 `json:"bytes"`
	// BitsPerSecond is the interval throughput.
	BitsPerSecond float64 `json:"bitsPerSecond"`
	// Retransmits is the interval's TCP retransmissions; nil when the
	// running iperf3 does not report them (absent is not zero).
	Retransmits *uint64 `json:"retransmits,omitempty"`
}

// TCPResult is one one-way TCP throughput test.
type TCPResult struct {
	// Direction is DirectionPC1ToPC2 or DirectionPC2ToPC1.
	Direction string `json:"direction"`
	// Incomplete marks a placeholder or partial result from a TCP run that
	// did not finish. Evaluators must not treat its metrics as measurements.
	Incomplete bool `json:"incomplete,omitempty"`
	// Duration is the configured test duration.
	Duration Duration `json:"duration"`
	// ParallelStreams is the iperf3 -P value.
	ParallelStreams int `json:"parallelStreams"`
	// SenderBitsPerSecond is the sender-side throughput (end.sum_sent).
	SenderBitsPerSecond float64 `json:"senderBitsPerSecond"`
	// ReceiverBitsPerSecond is the receiver-side throughput (end.sum_received).
	ReceiverBitsPerSecond float64 `json:"receiverBitsPerSecond"`
	// Retransmissions is the total TCP retransmissions on the sender side;
	// nil when the running iperf3 does not report them (absent is not zero).
	Retransmissions *uint64 `json:"retransmissions,omitempty"`
	// IntervalResults are the per-second interval rows.
	IntervalResults []TCPInterval `json:"intervalResults,omitempty"`
	// ThroughputVariation is the coefficient of variation (stdev/mean) of
	// the per-interval bitrates, first interval excluded.
	ThroughputVariation float64 `json:"throughputVariation"`
	// MinimumIntervalBps is the slowest interval's throughput.
	MinimumIntervalBps float64 `json:"minimumIntervalBps"`
	// MaximumIntervalBps is the fastest interval's throughput.
	MaximumIntervalBps float64 `json:"maximumIntervalBps"`
	// CPUUtilization is iperf3's CPU report for both sides.
	CPUUtilization CPUUsage `json:"cpuUtilization"`
	// Warnings are non-fatal notes about this test run.
	Warnings []string `json:"warnings,omitempty"`
}

// PingSpike is one reply whose RTT stands far above the median.
type PingSpike struct {
	// Seq is the icmp_seq of the spiking reply.
	Seq int `json:"seq"`
	// RTTMs is the spiking round-trip time in milliseconds.
	RTTMs float64 `json:"rttMs"`
}

// SeqRun is a run of consecutive missing icmp_seq values.
type SeqRun struct {
	// FirstSeq is the first missing sequence number of the run.
	FirstSeq int `json:"firstSeq"`
	// Len is the number of consecutive missing sequence numbers.
	Len int `json:"len"`
}

// PingResult is one ping stability test in one direction, parsed per-packet
// from iputils ping output.
type PingResult struct {
	// Direction is DirectionPC1ToPC2 or DirectionPC2ToPC1.
	Direction string `json:"direction"`
	// Target is the address that was pinged.
	Target string `json:"target"`
	// Transmitted is the number of echo requests sent.
	Transmitted int `json:"transmitted"`
	// Received is the number of echo replies received.
	Received int `json:"received"`
	// Duplicates counts DUP! replies (tracked separately from Received;
	// duplicates on a direct cable are themselves evidence).
	Duplicates int `json:"duplicates"`
	// SendErrors counts local send failures (e.g. EMSGSIZE from -M do).
	SendErrors int `json:"sendErrors"`
	// IcmpErrors counts ICMP error responses (unreachable, frag needed, ...).
	IcmpErrors int `json:"icmpErrors"`
	// LossPercent is the packet loss percentage from the summary line.
	LossPercent float64 `json:"lossPercent"`
	// RTTMinMs is the minimum round-trip time in milliseconds.
	RTTMinMs float64 `json:"rttMinMs"`
	// RTTAvgMs is the average round-trip time in milliseconds.
	RTTAvgMs float64 `json:"rttAvgMs"`
	// RTTMaxMs is the maximum round-trip time in milliseconds.
	RTTMaxMs float64 `json:"rttMaxMs"`
	// RTTMdevMs is the mean deviation of the round-trip time in milliseconds.
	RTTMdevMs float64 `json:"rttMdevMs"`
	// Percentiles maps percentile rank (50, 90, 95, 99) to RTT in
	// milliseconds, nearest-rank over the first reply per sequence number.
	Percentiles map[int]float64 `json:"percentiles,omitempty"`
	// Spikes lists replies with RTT far above the median.
	Spikes []PingSpike `json:"spikes,omitempty"`
	// MissingSeqRuns localizes which packets vanished.
	MissingSeqRuns []SeqRun `json:"missingSeqRuns,omitempty"`
	// LongestMissingRunLen is the count of consecutive missing icmp_seq values
	// in the longest missing-sequence run.
	LongestMissingRunLen int `json:"longestSeqGap"`
	// LongestGapMs is the longest gap between consecutive reply timestamps
	// (from ping -D), capturing burst loss and stalls.
	LongestGapMs float64 `json:"longestGapMs"`
	// IntervalUsedSec is the -i interval actually used after the
	// permission-fallback ladder (0.02 / 0.2 / 1.0).
	IntervalUsedSec float64 `json:"intervalUsedSec"`
	// ExitCode is ping's exit code (1 with a parsed summary = loss, not error).
	ExitCode int `json:"exitCode"`
	// UnparsedLines counts output lines the parser did not recognize.
	UnparsedLines int `json:"unparsedLines"`
}

// UDPResult is one one-way UDP loss/jitter test. Loss figures are the
// server-observed values reported back in the client's end.sum.
type UDPResult struct {
	// Direction is DirectionPC1ToPC2 or DirectionPC2ToPC1.
	Direction string `json:"direction"`
	// TargetBps is the requested send rate in bits per second.
	TargetBps uint64 `json:"targetBps"`
	// ActualSenderBps is the achieved send rate.
	ActualSenderBps float64 `json:"actualSenderBps"`
	// ActualReceiverBps is the rate observed by the receiver.
	ActualReceiverBps float64 `json:"actualReceiverBps"`
	// LostPackets is the number of datagrams lost.
	LostPackets int64 `json:"lostPackets"`
	// TotalPackets is the number of datagrams sent.
	TotalPackets int64 `json:"totalPackets"`
	// LossPercent is the datagram loss percentage.
	LossPercent float64 `json:"lossPercent"`
	// JitterMs is the RFC 1889 jitter in milliseconds.
	JitterMs float64 `json:"jitterMs"`
	// OutOfOrder counts datagrams received out of order; nil when the
	// running iperf3 does not report it (absent is not zero).
	OutOfOrder *int64 `json:"outOfOrder,omitempty"`
	// CPU is iperf3's CPU report for both sides.
	CPU CPUUsage `json:"cpu"`
}

// BidirDirection is one direction of a bidirectional stress test, always
// derived from per-stream sums (top-level bidir sums are buggy in
// iperf3 3.7-3.11).
type BidirDirection struct {
	// SenderBitsPerSecond is the sender-side throughput.
	SenderBitsPerSecond float64 `json:"senderBitsPerSecond"`
	// ReceiverBitsPerSecond is the receiver-side throughput.
	ReceiverBitsPerSecond float64 `json:"receiverBitsPerSecond"`
	// Retransmissions is the sender-side TCP retransmissions; nil when the
	// running iperf3 does not report them.
	Retransmissions *uint64 `json:"retransmissions,omitempty"`
}

// BidirResult is the bidirectional TCP stress test, either a true iperf3
// --bidir run or the coordinated two-phase fallback.
type BidirResult struct {
	// Duration is the configured test duration.
	Duration Duration `json:"duration"`
	// ParallelStreams is the iperf3 -P value (per direction).
	ParallelStreams int `json:"parallelStreams"`
	// PC1ToPC2 is the PC1 -> PC2 direction.
	PC1ToPC2 BidirDirection `json:"pc1ToPc2"`
	// PC2ToPC1 is the PC2 -> PC1 direction.
	PC2ToPC1 BidirDirection `json:"pc2ToPc1"`
	// CPUUtilization is iperf3's CPU report for both sides.
	CPUUtilization CPUUsage `json:"cpuUtilization"`
	// TwoPhaseFallback is true when --bidir was unavailable on either peer
	// and the test ran as two coordinated one-way phases instead.
	TwoPhaseFallback bool `json:"twoPhaseFallback"`
	// Warnings are non-fatal notes about this test run.
	Warnings []string `json:"warnings,omitempty"`
}

// LinkSettings is the parsed `ethtool <if>` link state of one interface at
// one point in time.
type LinkSettings struct {
	// SpeedMbps is the negotiated speed in Mb/s; -1 for "Unknown!"
	// (no link or virtual interface).
	SpeedMbps int `json:"speedMbps"`
	// Duplex is "full", "half" or "unknown".
	Duplex string `json:"duplex"`
	// LinkDetected reports "Link detected: yes".
	LinkDetected bool `json:"linkDetected"`
	// AutoNeg is "on", "off" or "unknown".
	AutoNeg string `json:"autoNeg"`
	// Port is the physical port type, e.g. "Twisted Pair".
	Port string `json:"port"`
	// SupportedPorts lists the supported port types.
	SupportedPorts []string `json:"supportedPorts,omitempty"`
	// SupportedModes lists the locally supported link modes ("1000baseT/Full").
	SupportedModes []string `json:"supportedModes,omitempty"`
	// AdvertisedModes lists the locally advertised link modes.
	AdvertisedModes []string `json:"advertisedModes,omitempty"`
	// PartnerModes lists the link modes advertised by the link partner;
	// empty with autoneg on and link up means "partner did not report".
	PartnerModes []string `json:"partnerModes,omitempty"`
	// MDIX is the MDI-X state, e.g. "on (auto)".
	MDIX string `json:"mdix,omitempty"`
	// Raw preserves every simple "key: value" line of the ethtool output.
	Raw map[string]string `json:"raw,omitempty"`
}

// PairStatus is the decoded per-pair outcome of `ethtool --cable-test`.
type PairStatus string

// Cable-test pair status codes, mapped from ethtool's netlink-era text.
// Unknown codes map to PairUnspecified with the raw text preserved.
const (
	PairOK          PairStatus = "OK"
	PairOpen        PairStatus = "OPEN"
	PairShortIntra  PairStatus = "SHORT_INTRA"
	PairShortInter  PairStatus = "SHORT_INTER"
	PairImpedance   PairStatus = "IMPEDANCE"
	PairUnspecified PairStatus = "UNSPECIFIED"
)

// CablePairResult is the cable-test outcome for one wire pair.
type CablePairResult struct {
	// Pair is the pair letter "A" through "D".
	Pair string `json:"pair"`
	// Status is the decoded pair status.
	Status PairStatus `json:"status"`
	// RawCode preserves ethtool's verbatim code text.
	RawCode string `json:"rawCode,omitempty"`
	// FaultMeters is the distance to the fault in meters, when reported.
	FaultMeters float64 `json:"faultMeters,omitempty"`
	// HasFault is true when the pair has a fault (and gives FaultMeters
	// meaning even when the distance is 0).
	HasFault bool `json:"hasFault"`
}

// TDRSample is one reflection sample reported by
// `ethtool --cable-test-tdr`.
type TDRSample struct {
	// Pair is the pair letter "A" through "D".
	Pair string `json:"pair"`
	// DistanceM is the sample's distance from the local NIC, in meters.
	DistanceM float64 `json:"distanceM"`
	// Amplitude is the signed reflection amplitude reported by ethtool.
	Amplitude int `json:"amplitude"`
}

// PeerCarrierEvents records each peer's locally observed carrier transitions
// during its own coordinated cable-test window. The values are deliberately
// clock-independent because peer wall clocks are not synchronized.
type PeerCarrierEvents struct {
	PC1 uint64 `json:"pc1"`
	PC2 uint64 `json:"pc2"`
}

// CableTestResult is the outcome of `ethtool --cable-test`. Unavailability
// (unsupported driver, no root, old ethtool) is recorded as Available=false
// with a reason and never counts as a cable failure.
type CableTestResult struct {
	// Available is false when the test could not run at all.
	Available bool `json:"available"`
	// UnavailableReason explains why the test could not run.
	UnavailableReason string `json:"unavailableReason,omitempty"`
	// Pairs holds the per-pair results when the test ran.
	Pairs []CablePairResult `json:"pairs,omitempty"`
	// Samples holds tolerant TDR reflection samples when requested.
	Samples []TDRSample `json:"samples,omitempty"`
	// TDRUnavailableReason explains why a requested TDR diagnostic did not
	// run even though the base cable test was available.
	TDRUnavailableReason string `json:"tdrUnavailableReason,omitempty"`
	// SelfInflictedCarrierEvents holds each side's own monitor count from the
	// disruptive cable-test window.
	SelfInflictedCarrierEvents PeerCarrierEvents `json:"selfInflictedCarrierEvents"`
	// UnparsedLines counts TDR data lines the tolerant parser could not
	// decode. Headers and blank lines do not count.
	UnparsedLines int `json:"unparsedLines,omitempty"`
}

// MonitoringEvent is one timestamped link-state observation from the sysfs
// monitor (carrier loss/restore, speed or duplex change, renegotiation).
type MonitoringEvent struct {
	// At is when the event was observed.
	At time.Time `json:"at"`
	// Type identifies the event, e.g. "carrier_lost", "carrier_restored",
	// "speed_changed", "renegotiation".
	Type string `json:"type"`
	// Detail is a human-readable description of the observed change.
	Detail string `json:"detail,omitempty"`
	// SelfInflicted is true when the event occurred inside the coordinated
	// cable-test window and must not count as spontaneous cable instability.
	SelfInflicted bool `json:"selfInflicted,omitempty"`
}

// SkippedTest records a test that was planned but did not run.
type SkippedTest struct {
	// Name is the test's identifier, e.g. "cable_test_tdr".
	Name string `json:"name"`
	// Reason explains why the test was skipped.
	Reason string `json:"reason"`
}

// FailureDetails describes what broke when a run ends abnormally.
type FailureDetails struct {
	// Stage is where the failure occurred, e.g. "handshake", "tcp_test".
	Stage string `json:"stage"`
	// Error is the failure message.
	Error string `json:"error"`
}

// Iperf3Caps records the iperf3 capabilities detected on one peer. A flag is
// claimed only when both the version number and the --help text agree.
type Iperf3Caps struct {
	// Version is the detected version, e.g. "3.16".
	Version string `json:"version"`
	// JSON reports -J support.
	JSON bool `json:"json"`
	// Reverse reports -R support (never used; recorded for diagnostics).
	Reverse bool `json:"reverse"`
	// Bidir reports --bidir support.
	Bidir bool `json:"bidir"`
	// GetServerOutput reports --get-server-output support.
	GetServerOutput bool `json:"getServerOutput"`
	// UDP reports -u support.
	UDP bool `json:"udp"`
	// OneOff reports -1 (one-off server) support.
	OneOff bool `json:"oneOff"`
}

// RawFileRef indexes one raw artifact stored under the report's raw/
// directory.
type RawFileRef struct {
	// Name is the file name relative to the report directory.
	Name string `json:"name"`
	// SHA256 is the hex digest of the file contents.
	SHA256 string `json:"sha256"`
	// Bytes is the file size.
	Bytes int64 `json:"bytes"`
	// Description says what the file contains.
	Description string `json:"description,omitempty"`
}
