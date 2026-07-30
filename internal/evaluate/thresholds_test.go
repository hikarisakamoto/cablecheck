package evaluate

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
)

func TestDefaultThresholdsForRulesVersion180(t *testing.T) {
	want := Thresholds{
		CRCPoorAbove:                  10,
		CRCFailedAbove:                1000,
		CRCCorroboratingPingLossAbove: 1,
		CarrierFailedAt:               3,
		FrameSizePoorAbove:            10,
		CarrierPHYPoorAbove:           10,
		CarrierPHYFailedAbove:         1000,
		NegotiatedSpeedPoorAt:         0.5,
		PingLossPoorAbove:             0.1,
		PingSpikesWarningAbove:        5,
		PingGapPoorAbove:              time.Second,
		TCPRetransWarningAt:           0.0001,
		TCPRetransPoorAbove:           0.01,
		UDPLossWarningAt:              0.5,
		UDPLossPoorAbove:              2,
		UDPJitterWarningAbove:         5,
		UDPReorderWarningAbove:        0.1,
		TCPThroughput100M:             ThroughputBands{PassAt: 0.9, InfoAt: 0.9, WarningAt: 0.7, PassDisabled: true},
		TCPThroughput1G:               ThroughputBands{PassAt: 0.9, InfoAt: 0.7, WarningAt: 0.4},
		TCPThroughputFallback:         ThroughputBands{PassAt: 0.9, InfoAt: 0.7, WarningAt: 0.4},
		TCPCoVWarningAt:               0.15,
		TCPCoVPoorAbove:               0.30,
		TCPCollapsePoorAt:             3,
		TCPAsymmetryWarnAbove:         0.30,
		CPUHostLimitedAbove:           90,
		HostRingDropRateAbove:         0.000001,
		HostRingDropFloor:             100,
		UDPTargetReachedAt:            0.90,
		UDPNearSaturationAbove:        0.95,
		FailedScoreBand:               ScoreBand{Min: 0, Max: 25},
		PoorScoreBand:                 ScoreBand{Min: 26, Max: 50},
		WarningScoreBand:              ScoreBand{Min: 51, Max: 79},
		GoodScoreBand:                 ScoreBand{Min: 80, Max: 94},
		ExcellentScoreBand:            ScoreBand{Min: 95, Max: 100},
	}
	if RulesVersion != "1.9.0" {
		t.Fatalf("RulesVersion = %q; review the pinned default thresholds before updating this test", RulesVersion)
	}
	if got := Default(); !reflect.DeepEqual(got, want) {
		t.Errorf("Default() =\n%+v\nwant\n%+v", got, want)
	}

	modified := Default()
	modified.UDPLossWarningAt = 99
	if Default().UDPLossWarningAt == modified.UDPLossWarningAt {
		t.Error("Default returned shared mutable state")
	}
}

func TestDefaultThresholdsValid(t *testing.T) {
	thresholds := Default()
	if thresholds.CRCPoorAbove >= thresholds.CRCFailedAbove {
		t.Error("CRC severity thresholds are not ordered")
	}
	if thresholds.CarrierPHYPoorAbove >= thresholds.CarrierPHYFailedAbove {
		t.Error("carrier/PHY severity thresholds are not ordered")
	}
	if thresholds.TCPRetransWarningAt >= thresholds.TCPRetransPoorAbove {
		t.Error("TCP retransmit thresholds are not ordered")
	}
	if thresholds.HostRingDropRateAbove <= 0 || thresholds.HostRingDropRateAbove >= 1 {
		t.Errorf("host receive-ring drop rate = %v, want a share in (0,1)", thresholds.HostRingDropRateAbove)
	}
	if thresholds.HostRingDropFloor == 0 {
		t.Error("host receive-ring drop floor is 0, which makes any single drop a host limitation again")
	}
	if thresholds.UDPLossWarningAt >= thresholds.UDPLossPoorAbove {
		t.Error("UDP loss thresholds are not ordered")
	}
	if thresholds.NegotiatedSpeedPoorAt <= 0 || thresholds.NegotiatedSpeedPoorAt >= 1 {
		t.Errorf("negotiated-speed poor ratio = %v, want (0,1)", thresholds.NegotiatedSpeedPoorAt)
	}
	if thresholds.TCPCoVWarningAt >= thresholds.TCPCoVPoorAbove {
		t.Error("TCP CoV thresholds are not ordered")
	}
	for name, band := range map[string]ThroughputBands{
		"100M":     thresholds.TCPThroughput100M,
		"1G":       thresholds.TCPThroughput1G,
		"fallback": thresholds.TCPThroughputFallback,
	} {
		ordered := 0 < band.WarningAt && band.WarningAt < band.InfoAt && band.InfoAt <= band.PassAt && band.PassAt <= 1
		if !ordered || (!band.PassDisabled && band.InfoAt == band.PassAt) {
			t.Errorf("TCP throughput %s thresholds are not ordered within (0,1]: %+v", name, band)
		}
	}
	for name, value := range map[string]float64{
		"UDP target ratio": thresholds.UDPTargetReachedAt,
		"UDP saturation":   thresholds.UDPNearSaturationAbove,
	} {
		if value <= 0 || value > 1 {
			t.Errorf("%s = %v, want (0,1]", name, value)
		}
	}
	if thresholds.PingGapPoorAbove <= 0 || thresholds.CarrierFailedAt == 0 || thresholds.TCPCollapsePoorAt <= 0 {
		t.Error("duration and count thresholds must be positive")
	}

	bands := []ScoreBand{
		thresholds.FailedScoreBand,
		thresholds.PoorScoreBand,
		thresholds.WarningScoreBand,
		thresholds.GoodScoreBand,
		thresholds.ExcellentScoreBand,
	}
	for i, band := range bands {
		if band.Min > band.Max {
			t.Errorf("score band %d is inverted: %+v", i, band)
		}
		if i > 0 && band.Min != bands[i-1].Max+1 {
			t.Errorf("score bands %d and %d are not contiguous: %+v then %+v", i-1, i, bands[i-1], band)
		}
	}
	if bands[0].Min != 0 || bands[len(bands)-1].Max != 100 {
		t.Errorf("score bands do not cover 0-100: %v", bands)
	}
}

func TestRulesHonorSuppliedThresholds(t *testing.T) {
	tests := []struct {
		name       string
		ruleID     string
		configure  func(*Thresholds)
		facts      *Facts
		want       model.Severity
		wantAbsent bool
	}{
		{"CRC poor", "PHY-02", func(v *Thresholds) { v.CRCPoorAbove = 20 }, &Facts{PC1: sideWithCRC(15)}, model.SevWarning, false},
		{"CRC failed", "PHY-02", func(v *Thresholds) { v.CRCFailedAbove = 2000 }, &Facts{PC1: sideWithCRC(1500)}, model.SevPoor, false},
		{"CRC corroborating loss", "PHY-02", func(v *Thresholds) { v.CRCCorroboratingPingLossAbove = 5 }, func() *Facts {
			f := &Facts{PC1: sideWithCRC(50)}
			f.Dir[0].PingLossPct = 2
			return f
		}(), model.SevPoor, false},
		{"carrier failed", "PHY-03", func(v *Thresholds) { v.CarrierFailedAt = 10 }, &Facts{PC1: SideFacts{CarrierEvents: 4, DeltaOK: true}}, model.SevPoor, false},
		{"frame size poor", "PHY-09", func(v *Thresholds) { v.FrameSizePoorAbove = 20 }, &Facts{PC1: SideFacts{JabberSizeErrors: 15, DeltaOK: true}}, model.SevWarning, false},
		{"carrier PHY poor", "PHY-11", func(v *Thresholds) { v.CarrierPHYPoorAbove = 20 }, &Facts{PC1: sideWithCarrierPHY(15)}, model.SevWarning, false},
		{"carrier PHY failed", "PHY-11", func(v *Thresholds) { v.CarrierPHYFailedAbove = 2000 }, &Facts{PC1: sideWithCarrierPHY(1500)}, model.SevPoor, false},
		{"reduced speed poor", "PHY-06", func(v *Thresholds) { v.NegotiatedSpeedPoorAt = 0.05 }, &Facts{NegotiatedSpeed: 100_000_000, ExpectedSpeed: 1_000_000_000}, model.SevWarning, false},
		{"ping loss poor", "TR-01", func(v *Thresholds) { v.PingLossPoorAbove = 2 }, &Facts{Dir: [2]DirFacts{{PingLossPct: 1}}}, model.SevWarning, false},
		{"ping spikes", "TR-05", func(v *Thresholds) { v.PingSpikesWarningAbove = 10 }, &Facts{Dir: [2]DirFacts{{PingSpikes: 6}}}, 0, true},
		{"ping gap", "TR-05", func(v *Thresholds) { v.PingGapPoorAbove = 2 * time.Second }, &Facts{Dir: [2]DirFacts{{PingMaxGap: 1500 * time.Millisecond}}}, 0, true},
		{"retransmit warning", "TR-06", func(v *Thresholds) { v.TCPRetransWarningAt = 0.02 }, &Facts{Dir: [2]DirFacts{{TCPAvailable: true, TCPRetransRate: 0.005}}}, 0, true},
		{"retransmit poor", "TR-06", func(v *Thresholds) { v.TCPRetransPoorAbove = 0.05 }, &Facts{Dir: [2]DirFacts{{TCPAvailable: true, TCPRetransRate: 0.02}}}, model.SevWarning, false},
		{"UDP loss warning", "TR-07", func(v *Thresholds) { v.UDPLossWarningAt = 3 }, &Facts{Dir: [2]DirFacts{{UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 1}}}, 0, true},
		{"UDP loss poor", "TR-07", func(v *Thresholds) { v.UDPLossPoorAbove = 5 }, &Facts{Dir: [2]DirFacts{{UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 3}}}, model.SevWarning, false},
		{"UDP jitter", "TR-08", func(v *Thresholds) { v.UDPJitterWarningAbove = 10 }, &Facts{Dir: [2]DirFacts{{UDPAvailable: true, UDPJitterMs: 6}}}, 0, true},
		{"UDP reorder", "TR-09", func(v *Thresholds) { v.UDPReorderWarningAbove = 1 }, &Facts{Dir: [2]DirFacts{{UDPAvailable: true, UDPOutOfOrderPct: 0.2}}}, 0, true},
		{"throughput pass", "PERF-01", func(v *Thresholds) {
			v.TCPThroughput100M = ThroughputBands{PassAt: 0.8, InfoAt: 0.6, WarningAt: 0.2}
		}, throughputFacts(85), 0, true},
		{"throughput info", "PERF-01", func(v *Thresholds) { v.TCPThroughput100M.InfoAt = 0.6 }, throughputFacts(65), model.SevInfo, false},
		{"throughput warning", "PERF-01", func(v *Thresholds) { v.TCPThroughput100M.WarningAt = 0.2 }, throughputFacts(30), model.SevWarning, false},
		{"CoV warning", "PERF-02", func(v *Thresholds) { v.TCPCoVWarningAt = 0.25 }, &Facts{Dir: [2]DirFacts{{TCPAvailable: true, TCPCoV: 0.2}}}, 0, true},
		{"CoV poor", "PERF-02", func(v *Thresholds) { v.TCPCoVPoorAbove = 0.5 }, &Facts{Dir: [2]DirFacts{{TCPAvailable: true, TCPCoV: 0.4}}}, model.SevWarning, false},
		{"collapse poor", "PERF-03", func(v *Thresholds) { v.TCPCollapsePoorAt = 5 }, &Facts{Dir: [2]DirFacts{{TCPCollapses: 3}}}, model.SevWarning, false},
		{"asymmetry", "PERF-04", func(v *Thresholds) { v.TCPAsymmetryWarnAbove = 0.5 }, asymmetryFacts(100, 60), 0, true},
		{"CPU host marker", "HOST-01", func(v *Thresholds) { v.CPUHostLimitedAbove = 50 }, &Facts{MaxCPUPct: 60}, model.SevMarker, false},
		{"receive-ring drop rate", "HOST-04", func(v *Thresholds) { v.HostRingDropRateAbove = 0.5 },
			&Facts{PC1: SideFacts{MissedErrors: 100, FramesReceived: 1_000, DeltaOK: true}}, 0, true},
		{"receive-ring drop rate exceeded", "HOST-04", func(v *Thresholds) { v.HostRingDropRateAbove = 0.05 },
			&Facts{PC1: SideFacts{MissedErrors: 100, FramesReceived: 1_000, DeltaOK: true}}, model.SevMarker, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thresholds := Default()
			tc.configure(&thresholds)
			finding := ruleByID(t, tc.ruleID).Evaluate(tc.facts, thresholds)
			if tc.wantAbsent {
				if finding != nil {
					t.Fatalf("finding = %+v, want none", finding)
				}
				return
			}
			if finding == nil {
				t.Fatalf("finding = nil, want severity %v", tc.want)
			}
			if finding.Severity != tc.want {
				t.Errorf("severity = %v, want %v", finding.Severity, tc.want)
			}
		})
	}
}

func TestThresholdBearingFindingTextUsesPolicy(t *testing.T) {
	thresholds := Default()
	thresholds.UDPLossPoorAbove = 7
	thresholds.UDPJitterWarningAbove = 11
	thresholds.CPUHostLimitedAbove = 50

	physical := &Facts{PC1: sideWithCRC(1)}
	physical.Dir[0] = DirFacts{UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 8}
	assertFindingContains(t, ruleByID(t, "PHY-10").Evaluate(physical, thresholds), "above 7%")

	jitter := &Facts{Dir: [2]DirFacts{{UDPAvailable: true, UDPJitterMs: 12}}}
	assertFindingContains(t, ruleByID(t, "TR-08").Evaluate(jitter, thresholds), "above 11 ms")

	assertFindingContains(t, ruleByID(t, "HOST-01").Evaluate(&Facts{MaxCPUPct: 60}, thresholds), "> 50%")

	collapse := &Facts{Dir: [2]DirFacts{{TCPCollapses: 1}}}
	assertFindingContains(t, ruleByID(t, "PERF-03").Evaluate(collapse, thresholds), "below 50%")
}

func TestFactsFromReportHonorsSuppliedThresholds(t *testing.T) {
	thresholds := Default()
	thresholds.UDPTargetReachedAt = 0.99
	thresholds.UDPNearSaturationAbove = 0.80

	report := &model.Report{
		PC1: model.PeerReport{NIC: model.NICReport{SpeedMbps: 1000}},
		Tests: model.TestsSection{
			UDP: []model.UDPResult{{
				Direction: model.DirectionPC1ToPC2, TargetBps: 850_000_000,
				ActualSenderBps: 807_500_000, LossPercent: 5,
			}},
		},
	}
	facts := factsFromReport(report, thresholds)
	if facts.Dir[0].UDPTargetReached || facts.Dir[0].UDPLossPct != 0 {
		t.Errorf("UDP facts = %+v, want target below custom 99%% gate", facts.Dir[0])
	}
	if !facts.UDPNearSaturation {
		t.Error("UDPNearSaturation = false, want target above custom 80% boundary")
	}
}

func TestScoreHonorsSuppliedThresholds(t *testing.T) {
	assertNoDeduction := func(t *testing.T, facts *Facts, thresholds Thresholds) {
		t.Helper()
		score := scoreFor(facts, nil, model.HealthExcellent, thresholds)
		if score == nil || *score != 100 {
			t.Errorf("score = %v, want 100 with metric inside supplied threshold", score)
		}
	}
	fullBand := func() Thresholds {
		thresholds := Default()
		thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
		return thresholds
	}

	t.Run("retransmit", func(t *testing.T) {
		thresholds := fullBand()
		thresholds.TCPRetransWarningAt = 0.03
		thresholds.TCPRetransPoorAbove = 0.04
		facts := &Facts{Dir: [2]DirFacts{{TCPAvailable: true, TCPRetransRate: 0.02}}}
		assertNoDeduction(t, facts, thresholds)
	})
	t.Run("UDP loss", func(t *testing.T) {
		thresholds := fullBand()
		thresholds.UDPLossWarningAt = 4
		thresholds.UDPLossPoorAbove = 5
		facts := &Facts{Dir: [2]DirFacts{{UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 3}}}
		assertNoDeduction(t, facts, thresholds)
	})
	t.Run("CPU gate", func(t *testing.T) {
		thresholds := fullBand()
		thresholds.CPUHostLimitedAbove = 50
		facts := &Facts{MaxCPUPct: 60, Dir: [2]DirFacts{{
			UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 3, UDPMaxCPUPct: 60,
		}}}
		assertNoDeduction(t, facts, thresholds)
	})
	t.Run("coefficient of variation", func(t *testing.T) {
		thresholds := fullBand()
		thresholds.TCPCoVWarningAt = 0.4
		thresholds.TCPCoVPoorAbove = 0.5
		facts := &Facts{Dir: [2]DirFacts{{TCPAvailable: true, TCPCoV: 0.3}}}
		assertNoDeduction(t, facts, thresholds)
	})
	t.Run("throughput ratio", func(t *testing.T) {
		thresholds := fullBand()
		thresholds.TCPThroughput100M.InfoAt = 0.2
		thresholds.TCPThroughput100M.WarningAt = 0.1
		thresholds.TCPAsymmetryWarnAbove = 0.5
		facts := throughputFacts(30)
		facts.Dir[1] = DirFacts{TCPAvailable: true, TCPBitrate: 20_000_000}
		assertNoDeduction(t, facts, thresholds)
	})
	t.Run("asymmetry", func(t *testing.T) {
		thresholds := fullBand()
		thresholds.TCPAsymmetryWarnAbove = 0.5
		assertNoDeduction(t, asymmetryFacts(100, 60), thresholds)
	})
	t.Run("jitter", func(t *testing.T) {
		thresholds := fullBand()
		thresholds.UDPJitterWarningAbove = 10
		facts := &Facts{Dir: [2]DirFacts{{UDPJitterMs: 6}}}
		assertNoDeduction(t, facts, thresholds)
	})
	t.Run("class band", func(t *testing.T) {
		thresholds := Default()
		thresholds.WarningScoreBand = ScoreBand{Min: 12, Max: 34}
		if got := clampToBand(100, model.HealthWarning, thresholds); got != 34 {
			t.Errorf("clampToBand(100) = %d, want custom maximum 34", got)
		}
		if got := clampToBand(0, model.HealthWarning, thresholds); got != 12 {
			t.Errorf("clampToBand(0) = %d, want custom minimum 12", got)
		}
	})
}

func throughputFacts(percent uint64) *Facts {
	f := &Facts{NegotiatedSpeed: 100_000_000}
	f.Dir[0] = DirFacts{TCPAvailable: true, TCPBitrate: model.Bitrate(percent * 1_000_000)}
	return f
}

func asymmetryFacts(a, b uint64) *Facts {
	return &Facts{Dir: [2]DirFacts{
		{TCPAvailable: true, TCPBitrate: model.Bitrate(a)},
		{TCPAvailable: true, TCPBitrate: model.Bitrate(b)},
	}}
}

func assertFindingContains(t *testing.T, finding *model.Finding, want string) {
	t.Helper()
	if finding == nil {
		t.Fatalf("finding = nil, want text containing %q", want)
	}
	text := finding.Text + " " + strings.Join(finding.Evidence, " ")
	if !strings.Contains(text, want) {
		t.Errorf("finding text = %q, want substring %q", text, want)
	}
}
