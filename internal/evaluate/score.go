package evaluate

import (
	"math"

	"cablecheck/internal/model"
)

// scoreFor computes the 0-100 health score: start at 100, apply the deduction
// table (§4.5 of the design), then clamp into the class band so score and
// class can never contradict each other. INCONCLUSIVE runs get no score.
func scoreFor(f *Facts, findings []model.Finding, class model.HealthClass, thresholds Thresholds) *int {
	if class == model.HealthInconclusive {
		return nil
	}
	hostLimited := hasFinding(findings, "HOST-01") || hasFinding(findings, "HOST-03")
	s := 100.0

	// Physical counter movement.
	s -= math.Min(40, 2*float64(crcTotal(f)))
	s -= math.Min(45, 15*float64(carrierEvents(f)))
	if f.Renegotiations > 0 {
		s -= 10
	}
	if f.HalfDuplex {
		s -= 25
	}
	if phy06cond(f) {
		s -= 15
	}

	// Per-direction transport deductions.
	udpGated := f.MaxCPUPct <= thresholds.CPUHostLimitedAbove
	for i := range f.Dir {
		d := &f.Dir[i]
		if d.PingLossPct > 0 {
			s -= math.Min(40, d.PingLossPct*20)
		}
		if d.TCPAvailable {
			switch {
			case d.TCPRetransRate > thresholds.TCPRetransPoorAbove:
				s -= 15
			case d.TCPRetransRate >= thresholds.TCPRetransWarningAt:
				s -= 5
			}
		}
		if udpGated && d.UDPAvailable && d.UDPTargetReached {
			switch {
			case d.UDPLossPct > thresholds.UDPLossPoorAbove:
				s -= 15
			case d.UDPLossPct >= thresholds.UDPLossWarningAt:
				s -= 5
			}
		}
	}
	if hasFinding(findings, "TR-02") {
		s -= 20 // full-size loss with clean standard ping
	}

	// Performance deductions (worst direction for CoV and throughput ratio).
	cov := 0.0
	for i := range f.Dir {
		if f.Dir[i].TCPAvailable && f.Dir[i].TCPCoV > cov {
			cov = f.Dir[i].TCPCoV
		}
	}
	switch {
	case cov > thresholds.TCPCoVPoorAbove:
		s -= 15
	case cov >= thresholds.TCPCoVWarningAt:
		s -= 5
	}
	s -= math.Min(20, 5*float64(f.Dir[0].TCPCollapses+f.Dir[1].TCPCollapses))
	if !hostLimited && f.NegotiatedSpeed > 0 {
		ratio := math.Inf(1)
		for i := range f.Dir {
			if f.Dir[i].TCPAvailable {
				if r := float64(f.Dir[i].TCPBitrate) / float64(f.NegotiatedSpeed); r < ratio {
					ratio = r
				}
			}
		}
		switch {
		case math.IsInf(ratio, 1):
			// no TCP result; nothing to deduct
		case ratio < thresholds.TCPThroughputWarningAt:
			s -= 25
		case ratio < thresholds.TCPThroughputInfoAt:
			s -= 10
		}
	}
	if asymmetryRatio(f) > thresholds.TCPAsymmetryWarnAbove {
		s -= 5
	}
	if f.Dir[0].UDPJitterMs > thresholds.UDPJitterWarningAbove || f.Dir[1].UDPJitterMs > thresholds.UDPJitterWarningAbove {
		s -= 5
	}

	v := clampToBand(int(math.Round(s)), class, thresholds)
	return &v
}

// asymmetryRatio returns |dir0-dir1|/max of the TCP bitrates, 0 when either
// direction is missing.
func asymmetryRatio(f *Facts) float64 {
	if !f.Dir[0].TCPAvailable || !f.Dir[1].TCPAvailable {
		return 0
	}
	a, b := float64(f.Dir[0].TCPBitrate), float64(f.Dir[1].TCPBitrate)
	hi := math.Max(a, b)
	if hi <= 0 {
		return 0
	}
	return math.Abs(a-b) / hi
}

// clampToBand forces the score into the configured band of the class.
func clampToBand(v int, class model.HealthClass, thresholds Thresholds) int {
	band := thresholds.scoreBand(class)
	if v < band.Min {
		return band.Min
	}
	if v > band.Max {
		return band.Max
	}
	return v
}
