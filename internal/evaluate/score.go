package evaluate

import (
	"math"

	"cablecheck/internal/model"
)

// scoreFor computes the 0-100 health score: start at 100, apply the deduction
// table (§4.5 of the design), then clamp into the class band so score and
// class can never contradict each other. INCONCLUSIVE runs get no score.
func scoreFor(f *Facts, findings []model.Finding, class model.HealthClass) *int {
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
	udpGated := f.MaxCPUPct <= 90
	for i := range f.Dir {
		d := &f.Dir[i]
		if d.PingLossPct > 0 {
			s -= math.Min(40, d.PingLossPct*20)
		}
		if d.TCPAvailable {
			switch {
			case d.TCPRetransRate > 0.01:
				s -= 15
			case d.TCPRetransRate >= 0.001:
				s -= 5
			}
		}
		if udpGated && d.UDPAvailable && d.UDPTargetReached {
			switch {
			case d.UDPLossPct > 2:
				s -= 15
			case d.UDPLossPct >= 0.5:
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
	case cov > 0.30:
		s -= 15
	case cov >= 0.15:
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
		case ratio < 0.4:
			s -= 25
		case ratio < 0.7:
			s -= 10
		}
	}
	if asymmetryRatio(f) > 0.30 {
		s -= 5
	}
	if f.Dir[0].UDPJitterMs > 5 || f.Dir[1].UDPJitterMs > 5 {
		s -= 5
	}

	v := clampToBand(int(math.Round(s)), class)
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

// clampToBand forces the score into the band of the class: FAILED <=25,
// POOR 26-50, WARNING 51-79, GOOD 80-94, EXCELLENT 95-100.
func clampToBand(v int, class model.HealthClass) int {
	lo, hi := 0, 100
	switch class {
	case model.HealthFailed:
		lo, hi = 0, 25
	case model.HealthPoor:
		lo, hi = 26, 50
	case model.HealthWarning:
		lo, hi = 51, 79
	case model.HealthGood:
		lo, hi = 80, 94
	case model.HealthExcellent:
		lo, hi = 95, 100
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
