package evaluate

import (
	"time"

	"cablecheck/internal/model"
)

// ScoreBand is the inclusive numeric score range for one health class.
type ScoreBand struct {
	Min int
	Max int
}

// ThroughputBands is the ordered ratio policy for TCP receiver throughput.
// Ratios are measured bitrate divided by the actual negotiated link speed.
type ThroughputBands struct {
	PassAt       float64
	InfoAt       float64
	WarningAt    float64
	PassDisabled bool
}

// Thresholds is the complete set of calibrated decision boundaries used by
// fact extraction, rules, and scoring. Structural checks such as availability,
// non-zero evidence, and valid denominators are intentionally not thresholds.
//
// Thresholds are internal policy, not user configuration. Production
// evaluation always uses Default; accepting a value explicitly in unexported
// helpers keeps every consumer testable against the same source of truth.
type Thresholds struct {
	CRCPoorAbove                  uint64
	CRCFailedAbove                uint64
	CRCCorroboratingPingLossAbove float64
	CarrierFailedAt               uint64
	FrameSizePoorAbove            uint64
	CarrierPHYPoorAbove           uint64
	CarrierPHYFailedAbove         uint64
	NegotiatedSpeedPoorAt         float64

	PingLossPoorAbove      float64
	PingSpikesWarningAbove int
	PingGapPoorAbove       time.Duration
	TCPRetransWarningAt    float64
	TCPRetransPoorAbove    float64
	UDPLossWarningAt       float64
	UDPLossPoorAbove       float64
	UDPJitterWarningAbove  float64
	UDPReorderWarningAbove float64

	TCPThroughput100M     ThroughputBands
	TCPThroughput1G       ThroughputBands
	TCPThroughputFallback ThroughputBands
	TCPCoVWarningAt       float64
	TCPCoVPoorAbove       float64
	TCPCollapsePoorAt     int
	TCPAsymmetryWarnAbove float64

	CPUHostLimitedAbove    float64
	HostRingDropRateAbove  float64
	HostRingDropFloor      uint64
	UDPTargetReachedAt     float64
	UDPNearSaturationAbove float64

	FailedScoreBand    ScoreBand
	PoorScoreBand      ScoreBand
	WarningScoreBand   ScoreBand
	GoodScoreBand      ScoreBand
	ExcellentScoreBand ScoreBand
}

// Default returns the fixed evaluation policy for RulesVersion. Returning a
// value prevents tests that customize a threshold from mutating global state.
func Default() Thresholds {
	return Thresholds{
		CRCPoorAbove:                  10,
		CRCFailedAbove:                1000,
		CRCCorroboratingPingLossAbove: 1,
		CarrierFailedAt:               3,
		FrameSizePoorAbove:            10,
		CarrierPHYPoorAbove:           10,
		CarrierPHYFailedAbove:         1000,
		NegotiatedSpeedPoorAt:         0.5,

		PingLossPoorAbove:      0.1,
		PingSpikesWarningAbove: 5,
		PingGapPoorAbove:       time.Second,
		// A direct cable has no legitimate congestion source, so the expected
		// retransmit count is zero. The old 0.1% warning boundary tolerated ~4800
		// lost segments per 60 s gigabit trial before saying anything; 0.01% still
		// absorbs slow-start and queue noise while catching a real burst. The poor
		// boundary stays at 1%: retransmissions alone cannot distinguish wire
		// corruption from local queue drops, which the tool does not yet measure.
		TCPRetransWarningAt:    0.0001,
		TCPRetransPoorAbove:    0.01,
		UDPLossWarningAt:       0.5,
		UDPLossPoorAbove:       2,
		UDPJitterWarningAbove:  5,
		UDPReorderWarningAbove: 0.1,

		// Authoritative, fixture-backed <=1G bands. The <=100M tier never passes
		// silently (InfoAt == PassAt with PassDisabled) so a marginal 100M link
		// cannot look clean.
		TCPThroughput100M: ThroughputBands{PassAt: 0.9, InfoAt: 0.9, WarningAt: 0.7, PassDisabled: true},
		TCPThroughput1G:   ThroughputBands{PassAt: 0.9, InfoAt: 0.7, WarningAt: 0.4},
		// >1G is an explicitly UNCALIBRATED caveat: it mirrors the 1G numbers
		// today but is a SEPARATE literal so recalibrating 1G never silently
		// retunes high-speed links. Replace with real 2.5G/5G/10G captures before
		// treating these as authoritative (see #25).
		TCPThroughputFallback: ThroughputBands{PassAt: 0.9, InfoAt: 0.7, WarningAt: 0.4},
		TCPCoVWarningAt:       0.15,
		TCPCoVPoorAbove:       0.30,
		TCPCollapsePoorAt:     3,
		TCPAsymmetryWarnAbove: 0.30,

		CPUHostLimitedAbove: 90,
		// Receive-ring drops gate the whole host-sensitive score, so they must be
		// frequent enough to plausibly limit throughput before they do; below the
		// boundary movement stays visible in the counter deltas but cannot excuse
		// a performance symptom. One frame in a million is deliberately low: a
		// ring overflow drops roughly one in-flight window and then stalls the
		// sender for an RTO, so the TCP symptoms this gates (collapse intervals,
		// retransmissions) are produced by tens to low hundreds of dropped frames
		// out of ~18M received, not by thousands. A higher boundary would leave
		// the POOR -> INCONCLUSIVE safeguard unreachable for TCP-only starvation
		// and blame the cable for the host.
		HostRingDropRateAbove: 0.000001,
		// Absolute escape hatch for the rate above: counters bracket the whole
		// run, so a burst inside one soak cycle is diluted by every clean cycle
		// after it. Beyond this many dropped frames the ring was starved
		// regardless of how much clean traffic followed.
		HostRingDropFloor:      100,
		UDPTargetReachedAt:     0.90,
		UDPNearSaturationAbove: 0.95,

		FailedScoreBand:    ScoreBand{Min: 0, Max: 25},
		PoorScoreBand:      ScoreBand{Min: 26, Max: 50},
		WarningScoreBand:   ScoreBand{Min: 51, Max: 79},
		GoodScoreBand:      ScoreBand{Min: 80, Max: 94},
		ExcellentScoreBand: ScoreBand{Min: 95, Max: 100},
	}
}

// perfBands selects the throughput policy for a negotiated link speed.
// Speeds above 1 Gbit/s use an explicitly conservative fallback until those
// tiers can be calibrated from real high-speed captures. Unknown speed has no
// applicable policy because a throughput ratio cannot be computed.
func (t Thresholds) perfBands(speed model.Bitrate) (ThroughputBands, bool) {
	switch {
	case speed == 0:
		return ThroughputBands{}, false
	case speed <= 100_000_000:
		return t.TCPThroughput100M, true
	case speed <= 1_000_000_000:
		return t.TCPThroughput1G, true
	default:
		return t.TCPThroughputFallback, true
	}
}

// scoreBand returns the configured band for class. Unknown classes retain the
// full configured score range, matching clampToBand's historical behavior.
func (t Thresholds) scoreBand(class model.HealthClass) ScoreBand {
	switch class {
	case model.HealthFailed:
		return t.FailedScoreBand
	case model.HealthPoor:
		return t.PoorScoreBand
	case model.HealthWarning:
		return t.WarningScoreBand
	case model.HealthGood:
		return t.GoodScoreBand
	case model.HealthExcellent:
		return t.ExcellentScoreBand
	default:
		return ScoreBand{Min: t.FailedScoreBand.Min, Max: t.ExcellentScoreBand.Max}
	}
}
