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

	PingLossPoorAbove      float64
	PingSpikesWarningAbove int
	PingGapPoorAbove       time.Duration
	TCPRetransWarningAt    float64
	TCPRetransPoorAbove    float64
	UDPLossWarningAt       float64
	UDPLossPoorAbove       float64
	UDPJitterWarningAbove  float64
	UDPReorderWarningAbove float64

	TCPThroughputPassAt    float64
	TCPThroughputInfoAt    float64
	TCPThroughputWarningAt float64
	TCPCoVWarningAt        float64
	TCPCoVPoorAbove        float64
	TCPCollapseBelowMedian float64
	TCPCollapsePoorAt      int
	TCPAsymmetryWarnAbove  float64

	CPUHostLimitedAbove    float64
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

		PingLossPoorAbove:      0.1,
		PingSpikesWarningAbove: 5,
		PingGapPoorAbove:       time.Second,
		TCPRetransWarningAt:    0.001,
		TCPRetransPoorAbove:    0.01,
		UDPLossWarningAt:       0.5,
		UDPLossPoorAbove:       2,
		UDPJitterWarningAbove:  5,
		UDPReorderWarningAbove: 0.1,

		TCPThroughputPassAt:    0.9,
		TCPThroughputInfoAt:    0.7,
		TCPThroughputWarningAt: 0.4,
		TCPCoVWarningAt:        0.15,
		TCPCoVPoorAbove:        0.30,
		TCPCollapseBelowMedian: 0.5,
		TCPCollapsePoorAt:      3,
		TCPAsymmetryWarnAbove:  0.30,

		CPUHostLimitedAbove:    90,
		UDPTargetReachedAt:     0.90,
		UDPNearSaturationAbove: 0.95,

		FailedScoreBand:    ScoreBand{Min: 0, Max: 25},
		PoorScoreBand:      ScoreBand{Min: 26, Max: 50},
		WarningScoreBand:   ScoreBand{Min: 51, Max: 79},
		GoodScoreBand:      ScoreBand{Min: 80, Max: 94},
		ExcellentScoreBand: ScoreBand{Min: 95, Max: 100},
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
