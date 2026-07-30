package evaluate

import (
	"math"
	"math/rand"
	"testing"
	"testing/quick"
	"time"

	"cablecheck/internal/model"
)

const noFindingSeverity = -1

// findingSeverity places an absent finding below the ordinal severity ladder.
// Markers are deliberately rejected: they qualify evidence but are not health
// severities and therefore have no monotonic ordering.
func findingSeverity(finding *model.Finding) (int, bool) {
	if finding == nil {
		return noFindingSeverity, true
	}
	if finding.Severity < model.SevInfo || finding.Severity > model.SevFailed {
		return 0, false
	}
	return int(finding.Severity), true
}

// countAcrossBands maps a quick uint16 monotonically onto [0, ceiling]. The
// integer counter rules band at small values (CRCPoorAbove=10,
// CarrierFailedAt=3, FrameSizePoorAbove=10); feeding the raw 0..65535 counter
// would land almost every draw far above every threshold, so the monotonic
// property would practically never place a better/worse pair on opposite sides
// of a band edge and would stay blind to a Poor/Failed comparator flip there.
// Compressing the domain to just past the top band exercises the whole ladder.
// The mapping is order-preserving (computed in uint64 to avoid overflow), so a
// non-decreasing input can never make the resulting severity drop — no spurious
// property failure is possible.
func countAcrossBands(value uint16, ceiling uint64) uint64 {
	return uint64(value) * (ceiling + 1) / (uint64(^uint16(0)) + 1)
}

func TestRuleSeverityBoundaries(t *testing.T) {
	thresholds := Default()
	tests := []struct {
		name   string
		ruleID string
		facts  *Facts
		want   int
	}{
		{
			name:   "CRC poor boundary with high ping loss stays warning",
			ruleID: "PHY-02",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CRCClassErrors = thresholds.CRCPoorAbove
				f.Dir[0].PingLossPct = math.Nextafter(thresholds.CRCCorroboratingPingLossAbove, math.Inf(1))
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "carrier zero passes",
			ruleID: "PHY-03",
			facts:  cleanFacts(),
			want:   noFindingSeverity,
		},
		{
			name:   "carrier below failed boundary is poor",
			ruleID: "PHY-03",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CarrierEvents = thresholds.CarrierFailedAt - 1
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "carrier failed boundary is inclusive",
			ruleID: "PHY-03",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CarrierEvents = thresholds.CarrierFailedAt
				return f
			}(),
			want: int(model.SevFailed),
		},
		{
			name:   "monitor fallback below failed boundary is poor",
			ruleID: "PHY-03",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.DeltaOK = false
				f.PC2.DeltaOK = false
				f.MonitorCarrierTransitions = thresholds.CarrierFailedAt - 1
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "monitor fallback failed boundary is inclusive",
			ruleID: "PHY-03",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.DeltaOK = false
				f.PC2.DeltaOK = false
				f.MonitorCarrierTransitions = thresholds.CarrierFailedAt
				return f
			}(),
			want: int(model.SevFailed),
		},
		{
			name:   "frame size poor boundary stays warning",
			ruleID: "PHY-09",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.JabberSizeErrors = thresholds.FrameSizePoorAbove
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "above frame size poor boundary is poor",
			ruleID: "PHY-09",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.JabberSizeErrors = thresholds.FrameSizePoorAbove + 1
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "correlated UDP loss poor boundary passes",
			ruleID: "PHY-10",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CRCClassErrors = 1
				f.Dir[0].UDPLossPct = thresholds.UDPLossPoorAbove
				return f
			}(),
			want: noFindingSeverity,
		},
		{
			name:   "above correlated UDP loss poor boundary fails",
			ruleID: "PHY-10",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CRCClassErrors = 1
				f.Dir[0].UDPLossPct = math.Nextafter(thresholds.UDPLossPoorAbove, math.Inf(1))
				return f
			}(),
			want: int(model.SevFailed),
		},
		{
			name:   "carrier PHY poor boundary stays warning",
			ruleID: "PHY-11",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CarrierPHYErrors = thresholds.CarrierPHYPoorAbove
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "above carrier PHY poor boundary is poor",
			ruleID: "PHY-11",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CarrierPHYErrors = thresholds.CarrierPHYPoorAbove + 1
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "carrier PHY failed boundary stays poor",
			ruleID: "PHY-11",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CarrierPHYErrors = thresholds.CarrierPHYFailedAbove
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "above carrier PHY failed boundary fails",
			ruleID: "PHY-11",
			facts: func() *Facts {
				f := cleanFacts()
				f.PC1.CarrierPHYErrors = thresholds.CarrierPHYFailedAbove + 1
				return f
			}(),
			want: int(model.SevFailed),
		},
		{
			name:   "spike boundary passes",
			ruleID: "TR-05",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].PingSpikes = thresholds.PingSpikesWarningAbove
				return f
			}(),
			want: noFindingSeverity,
		},
		{
			name:   "above spike boundary warns",
			ruleID: "TR-05",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].PingSpikes = thresholds.PingSpikesWarningAbove + 1
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "gap boundary passes",
			ruleID: "TR-05",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].PingMaxGap = thresholds.PingGapPoorAbove
				return f
			}(),
			want: noFindingSeverity,
		},
		{
			name:   "above gap boundary is poor",
			ruleID: "TR-05",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].PingMaxGap = thresholds.PingGapPoorAbove + time.Nanosecond
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "jitter boundary passes",
			ruleID: "TR-08",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].UDPJitterMs = thresholds.UDPJitterWarningAbove
				return f
			}(),
			want: noFindingSeverity,
		},
		{
			name:   "above jitter boundary warns",
			ruleID: "TR-08",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].UDPJitterMs = math.Nextafter(thresholds.UDPJitterWarningAbove, math.Inf(1))
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "reorder boundary passes",
			ruleID: "TR-09",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].UDPOutOfOrderPct = thresholds.UDPReorderWarningAbove
				return f
			}(),
			want: noFindingSeverity,
		},
		{
			name:   "above reorder boundary warns",
			ruleID: "TR-09",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].UDPOutOfOrderPct = math.Nextafter(thresholds.UDPReorderWarningAbove, math.Inf(1))
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "CoV below warning boundary passes",
			ruleID: "PERF-02",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].TCPCoV = math.Nextafter(thresholds.TCPCoVWarningAt, math.Inf(-1))
				return f
			}(),
			want: noFindingSeverity,
		},
		{
			name:   "CoV warning boundary is inclusive",
			ruleID: "PERF-02",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].TCPCoV = thresholds.TCPCoVWarningAt
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "CoV poor boundary stays warning",
			ruleID: "PERF-02",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].TCPCoV = thresholds.TCPCoVPoorAbove
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "above CoV poor boundary is poor",
			ruleID: "PERF-02",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].TCPCoV = math.Nextafter(thresholds.TCPCoVPoorAbove, math.Inf(1))
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "collapse below poor boundary warns",
			ruleID: "PERF-03",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].TCPCollapses = thresholds.TCPCollapsePoorAt - 1
				return f
			}(),
			want: int(model.SevWarning),
		},
		{
			name:   "collapse poor boundary is inclusive",
			ruleID: "PERF-03",
			facts: func() *Facts {
				f := cleanFacts()
				f.Dir[0].TCPCollapses = thresholds.TCPCollapsePoorAt
				return f
			}(),
			want: int(model.SevPoor),
		},
		{
			name:   "asymmetry boundary passes",
			ruleID: "PERF-04",
			facts:  asymmetryFacts(100, 70),
			want:   noFindingSeverity,
		},
		{
			name:   "above asymmetry boundary warns",
			ruleID: "PERF-04",
			facts:  asymmetryFacts(100, 69),
			want:   int(model.SevWarning),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ordinal := findingSeverity(ruleByID(t, tc.ruleID).Evaluate(tc.facts, thresholds))
			if !ordinal {
				t.Fatalf("%s returned a marker, want ordinal severity", tc.ruleID)
			}
			if got != tc.want {
				t.Errorf("%s severity rank = %d, want %d", tc.ruleID, got, tc.want)
			}
		})
	}

	t.Run("CPU host boundary is exclusive", func(t *testing.T) {
		rule := ruleByID(t, "HOST-01")
		if finding := rule.Evaluate(&Facts{MaxCPUPct: thresholds.CPUHostLimitedAbove}, thresholds); finding != nil {
			t.Errorf("HOST-01 at boundary = %+v, want no finding", finding)
		}
		above := math.Nextafter(thresholds.CPUHostLimitedAbove, math.Inf(1))
		finding := rule.Evaluate(&Facts{MaxCPUPct: above}, thresholds)
		if finding == nil || finding.Severity != model.SevMarker {
			t.Errorf("HOST-01 above boundary = %+v, want marker", finding)
		}
	})
}

type monotonicRuleCase struct {
	name          string
	ruleID        string
	higherIsWorse bool
	withPeer      bool
	prepare       func(*Facts)
	set           func(*Facts, int, uint16)
}

func TestRuleSeverityMonotonic(t *testing.T) {
	const maxUint16 = float64(^uint16(0))
	// Compress the count setters onto a domain just past the top band so a
	// worsening draw actually steps across the severity ladder (see
	// countAcrossBands). +2 clears the last threshold with headroom.
	thresholds := Default()
	crcCeiling := thresholds.CRCFailedAbove + 2
	carrierCeiling := thresholds.CarrierFailedAt + 2
	frameCeiling := thresholds.FrameSizePoorAbove + 2
	carrierPHYCeiling := thresholds.CarrierPHYFailedAbove + 2
	throughputSetter := func(speed model.Bitrate) func(*Facts, int, uint16) {
		return func(f *Facts, direction int, value uint16) {
			f.NegotiatedSpeed = speed
			f.ExpectedSpeed = speed
			f.Dir[direction].TCPAvailable = true
			f.Dir[direction].TCPBitrate = model.Bitrate(float64(speed) * float64(value) / maxUint16)
		}
	}
	tests := []monotonicRuleCase{
		{
			name: "CRC errors", ruleID: "PHY-02", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, side int, value uint16) {
				n := countAcrossBands(value, crcCeiling)
				if side == 0 {
					f.PC1.CRCClassErrors = n
				} else {
					f.PC2.CRCClassErrors = n
				}
			},
		},
		{
			name: "CRC corroborating ping loss", ruleID: "PHY-02", higherIsWorse: true, withPeer: true,
			prepare: func(f *Facts) { f.PC1.CRCClassErrors = Default().CRCPoorAbove + 1 },
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].PingLossPct = float64(value) / 1_000
			},
		},
		{
			name: "carrier events", ruleID: "PHY-03", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, side int, value uint16) {
				n := countAcrossBands(value, carrierCeiling)
				if side == 0 {
					f.PC1.CarrierEvents = n
				} else {
					f.PC2.CarrierEvents = n
				}
			},
		},
		{
			name: "negotiated speed ratio", ruleID: "PHY-06", higherIsWorse: false,
			set: func(f *Facts, _ int, value uint16) {
				f.ExpectedSpeed = 1_000_000_000
				f.NegotiatedSpeed = 1 + model.Bitrate(float64(999_999_999)*float64(value)/maxUint16)
			},
		},
		{
			name: "frame size errors", ruleID: "PHY-09", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, side int, value uint16) {
				n := countAcrossBands(value, frameCeiling)
				if side == 0 {
					f.PC1.JabberSizeErrors = n
				} else {
					f.PC2.JabberSizeErrors = n
				}
			},
		},
		{
			name: "correlated UDP loss", ruleID: "PHY-10", higherIsWorse: true, withPeer: true,
			prepare: func(f *Facts) { f.PC1.CRCClassErrors = 1 },
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].UDPLossPct = float64(value) / 1_000
			},
		},
		{
			name: "carrier PHY errors", ruleID: "PHY-11", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, side int, value uint16) {
				n := countAcrossBands(value, carrierPHYCeiling)
				if side == 0 {
					f.PC1.CarrierPHYErrors = n
				} else {
					f.PC2.CarrierPHYErrors = n
				}
			},
		},
		{
			name: "ping loss", ruleID: "TR-01", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].PingLossPct = float64(value) / 1_000
			},
		},
		{
			name: "RTT spikes", ruleID: "TR-05", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) { f.Dir[direction].PingSpikes = int(value) },
		},
		{
			name: "RTT gap", ruleID: "TR-05", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].PingMaxGap = time.Duration(value) * time.Millisecond
			},
		},
		{
			name: "TCP retransmit rate", ruleID: "TR-06", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].TCPRetransRate = float64(value) / 1_000_000
			},
		},
		{
			name: "UDP loss", ruleID: "TR-07", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].UDPLossPct = float64(value) / 1_000
			},
		},
		{
			name: "UDP jitter", ruleID: "TR-08", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].UDPJitterMs = float64(value) / 100
			},
		},
		{
			name: "UDP reordering", ruleID: "TR-09", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].UDPOutOfOrderPct = float64(value) / 1_000
			},
		},
		{name: "100M TCP throughput", ruleID: "PERF-01", withPeer: true, set: throughputSetter(100_000_000)},
		{name: "1G TCP throughput", ruleID: "PERF-01", withPeer: true, set: throughputSetter(1_000_000_000)},
		{name: "fallback TCP throughput", ruleID: "PERF-01", withPeer: true, set: throughputSetter(2_500_000_000)},
		{
			name: "TCP coefficient of variation", ruleID: "PERF-02", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) {
				f.Dir[direction].TCPCoV = float64(value) / 100_000
			},
		},
		{
			name: "TCP collapse count", ruleID: "PERF-03", higherIsWorse: true, withPeer: true,
			set: func(f *Facts, direction int, value uint16) { f.Dir[direction].TCPCollapses = int(value) },
		},
		{
			name: "TCP asymmetry", ruleID: "PERF-04", higherIsWorse: true,
			prepare: func(f *Facts) {
				f.Dir[0] = DirFacts{TCPAvailable: true, TCPBitrate: 1_000_000_000}
				f.Dir[1] = DirFacts{TCPAvailable: true, TCPBitrate: 1_000_000_000}
			},
			set: func(f *Facts, direction int, value uint16) {
				difference := model.Bitrate(float64(1_000_000_000) * float64(value) / maxUint16)
				f.Dir[direction].TCPBitrate = 1_000_000_000 - difference
			},
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule := ruleByID(t, tc.ruleID)
			evaluatePair := func(betterValue, worseValue, peer uint16) (int, int, bool) {
				better, worse := cleanFacts(), cleanFacts()
				if tc.prepare != nil {
					tc.prepare(better)
					tc.prepare(worse)
				}
				slot := int(peer & 1)
				tc.set(better, slot, betterValue)
				tc.set(worse, slot, worseValue)
				if tc.withPeer {
					tc.set(better, 1-slot, peer)
					tc.set(worse, 1-slot, peer)
				}
				betterRank, betterOrdinal := findingSeverity(rule.Evaluate(better, Default()))
				worseRank, worseOrdinal := findingSeverity(rule.Evaluate(worse, Default()))
				return betterRank, worseRank, betterOrdinal && worseOrdinal
			}

			betterWitness, worseWitness := uint16(0), ^uint16(0)
			if !tc.higherIsWorse {
				betterWitness, worseWitness = worseWitness, betterWitness
			}
			betterRank, worseRank, ordinal := evaluatePair(betterWitness, worseWitness, betterWitness)
			if !ordinal || worseRank <= betterRank {
				t.Fatalf("severity witness did not worsen: rank %d -> %d", betterRank, worseRank)
			}

			property := func(a, b, peer uint16) bool {
				low, high := a, b
				if low > high {
					low, high = high, low
				}
				betterValue, worseValue := low, high
				if !tc.higherIsWorse {
					betterValue, worseValue = high, low
				}
				betterRank, worseRank, ordinal := evaluatePair(betterValue, worseValue, peer)
				return ordinal && worseRank >= betterRank
			}
			config := &quick.Config{
				MaxCount: 1_000,
				Rand:     rand.New(rand.NewSource(int64(11_000 + i))),
			}
			if err := quick.Check(property, config); err != nil {
				t.Fatal(err)
			}
		})
	}
}
