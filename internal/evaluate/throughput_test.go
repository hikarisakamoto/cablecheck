package evaluate

import (
	"strings"
	"testing"

	"cablecheck/internal/model"
)

func TestPerfBandsSelectsSpeedTier(t *testing.T) {
	thresholds := Default()
	thresholds.TCPThroughput100M = ThroughputBands{PassAt: 0.91, InfoAt: 0.81, WarningAt: 0.61}
	thresholds.TCPThroughput1G = ThroughputBands{PassAt: 0.92, InfoAt: 0.72, WarningAt: 0.42}
	thresholds.TCPThroughputFallback = ThroughputBands{PassAt: 0.93, InfoAt: 0.73, WarningAt: 0.43}

	tests := []struct {
		name  string
		speed model.Bitrate
		want  ThroughputBands
		ok    bool
	}{
		{"unknown", 0, ThroughputBands{}, false},
		{"10M", 10_000_000, thresholds.TCPThroughput100M, true},
		{"100M", 100_000_000, thresholds.TCPThroughput100M, true},
		{"above 100M", 100_000_001, thresholds.TCPThroughput1G, true},
		{"1G", 1_000_000_000, thresholds.TCPThroughput1G, true},
		{"above 1G", 1_000_000_001, thresholds.TCPThroughputFallback, true},
		{"2.5G", 2_500_000_000, thresholds.TCPThroughputFallback, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := thresholds.perfBands(tc.speed)
			if ok != tc.ok || got != tc.want {
				t.Errorf("perfBands(%s) = (%+v, %t), want (%+v, %t)", tc.speed, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestClassifyThroughputBoundaries(t *testing.T) {
	thresholds := Default()
	thresholds.TCPThroughput100M = ThroughputBands{PassAt: 0.90, InfoAt: 0.80, WarningAt: 0.60}

	tests := []struct {
		name      string
		measured  model.Bitrate
		wantSev   model.Severity
		deviation bool
	}{
		{"at pass", 90_000_000, 0, false},
		{"below pass", 89_999_999, model.SevInfo, true},
		{"at info", 80_000_000, model.SevInfo, true},
		{"below info", 79_999_999, model.SevWarning, true},
		{"at warning", 60_000_000, model.SevWarning, true},
		{"below warning", 59_999_999, model.SevPoor, true},
		{"zero", 0, model.SevPoor, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, gotSev, gotDeviation := classifyThroughput(tc.measured, 100_000_000, thresholds)
			if gotSev != tc.wantSev || gotDeviation != tc.deviation {
				t.Errorf("classifyThroughput() = (%v, %t), want (%v, %t)", gotSev, gotDeviation, tc.wantSev, tc.deviation)
			}
		})
	}

	if ratio, _, deviation := classifyThroughput(50_000_000, 0, thresholds); ratio != 0 || deviation {
		t.Errorf("unknown speed = (ratio %v, deviation %t), want (0, false)", ratio, deviation)
	}
}

func TestDefaultThroughputTierBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		negotiated model.Bitrate
		measured   model.Bitrate
		wantSev    model.Severity
		deviation  bool
	}{
		{"10M line rate still info", 10_000_000, 10_000_000, model.SevInfo, true},
		{"10M below warning is poor", 10_000_000, 6_000_000, model.SevPoor, true},
		{"100M at info", 100_000_000, 90_000_000, model.SevInfo, true},
		{"100M below info", 100_000_000, 89_999_999, model.SevWarning, true},
		{"100M at warning", 100_000_000, 70_000_000, model.SevWarning, true},
		{"100M below warning", 100_000_000, 69_999_999, model.SevPoor, true},
		{"1G at pass", 1_000_000_000, 900_000_000, 0, false},
		{"1G below pass", 1_000_000_000, 899_999_999, model.SevInfo, true},
		{"1G at info", 1_000_000_000, 700_000_000, model.SevInfo, true},
		{"1G below info", 1_000_000_000, 699_999_999, model.SevWarning, true},
		{"1G at warning", 1_000_000_000, 400_000_000, model.SevWarning, true},
		{"1G below warning", 1_000_000_000, 399_999_999, model.SevPoor, true},
		{"fallback at pass", 2_500_000_000, 2_250_000_000, 0, false},
		{"fallback below pass", 2_500_000_000, 2_249_999_999, model.SevInfo, true},
		{"fallback at info", 2_500_000_000, 1_750_000_000, model.SevInfo, true},
		{"fallback below info", 2_500_000_000, 1_749_999_999, model.SevWarning, true},
		{"fallback at warning", 2_500_000_000, 1_000_000_000, model.SevWarning, true},
		{"fallback below warning", 2_500_000_000, 999_999_999, model.SevPoor, true},
	}
	thresholds := Default()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, gotSev, gotDeviation := classifyThroughput(tc.measured, tc.negotiated, thresholds)
			if gotSev != tc.wantSev || gotDeviation != tc.deviation {
				t.Errorf("classifyThroughput() = (%v, %t), want (%v, %t)", gotSev, gotDeviation, tc.wantSev, tc.deviation)
			}
		})
	}
}

func TestThroughputRuleAndScoreShareClassification(t *testing.T) {
	thresholds := Default()
	thresholds.TCPThroughput100M = ThroughputBands{PassAt: 0.90, InfoAt: 0.80, WarningAt: 0.60}
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}

	tests := []struct {
		name        string
		measured    model.Bitrate
		wantFinding bool
		wantSev     model.Severity
		wantScore   int
	}{
		{"pass", 95_000_000, false, 0, 100},
		{"info", 85_000_000, true, model.SevInfo, 100},
		{"warning", 70_000_000, true, model.SevWarning, 90},
		{"poor", 50_000_000, true, model.SevPoor, 75},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := &Facts{NegotiatedSpeed: 100_000_000}
			facts.Dir[0] = DirFacts{TCPAvailable: true, TCPBitrate: tc.measured}
			finding := rulePERF01(facts, thresholds)
			if (finding != nil) != tc.wantFinding {
				t.Fatalf("finding = %+v, want present %t", finding, tc.wantFinding)
			}
			if finding != nil && finding.Severity != tc.wantSev {
				t.Errorf("finding severity = %v, want %v", finding.Severity, tc.wantSev)
			}
			score := scoreFor(facts, nil, model.HealthExcellent, thresholds)
			if score == nil || *score != tc.wantScore {
				t.Errorf("score = %v, want %d", score, tc.wantScore)
			}
		})
	}
}

func TestThroughputUsesWorstAvailableDirection(t *testing.T) {
	thresholds := Default()
	thresholds.TCPThroughput1G = ThroughputBands{PassAt: 0.90, InfoAt: 0.80, WarningAt: 0.60}
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	facts := &Facts{NegotiatedSpeed: 1_000_000_000}
	facts.Dir[0] = DirFacts{TCPAvailable: true, TCPBitrate: 850_000_000}
	facts.Dir[1] = DirFacts{TCPAvailable: true, TCPBitrate: 500_000_000}

	finding := rulePERF01(facts, thresholds)
	if finding == nil || finding.Severity != model.SevPoor {
		t.Fatalf("finding = %+v, want poor from worse direction", finding)
	}
	if len(finding.Evidence) != 2 {
		t.Errorf("evidence = %v, want both deviating directions", finding.Evidence)
	}
	score := scoreFor(facts, nil, model.HealthExcellent, thresholds)
	if score == nil || *score != 70 { // -25 throughput, -5 asymmetry
		t.Errorf("score = %v, want 70", score)
	}

	facts.Dir[1].TCPAvailable = false
	finding = rulePERF01(facts, thresholds)
	if finding == nil || finding.Severity != model.SevInfo {
		t.Errorf("finding with poor direction unavailable = %+v, want info", finding)
	}
}

func TestThroughputFallbackUsesActualHighSpeedDenominator(t *testing.T) {
	thresholds := Default()
	thresholds.TCPThroughputFallback = ThroughputBands{PassAt: 0.90, InfoAt: 0.70, WarningAt: 0.40}

	ratio, severity, deviation := classifyThroughput(2_000_000_000, 2_500_000_000, thresholds)
	if ratio != 0.8 || severity != model.SevInfo || !deviation {
		t.Errorf("2G/2.5G classification = (ratio %v, severity %v, deviation %t), want (0.8, info, true)", ratio, severity, deviation)
	}
}

func TestDefaultLowSpeedPolicyNeverPasses(t *testing.T) {
	tests := []struct {
		name      string
		measured  model.Bitrate
		wantClass model.HealthClass
		wantSev   model.Severity
	}{
		{"line rate is still informational", 100_000_000, model.HealthGood, model.SevInfo},
		{"94 percent is informational", 94_000_000, model.HealthGood, model.SevInfo},
		{"80 percent warns", 80_000_000, model.HealthWarning, model.SevWarning},
		{"60 percent is poor", 60_000_000, model.HealthPoor, model.SevPoor},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := cleanFacts()
			facts.NegotiatedSpeed = 100_000_000
			facts.ExpectedSpeed = 100_000_000
			for i := range facts.Dir {
				facts.Dir[i].TCPBitrate = tc.measured
			}
			result := Evaluate(facts)
			if result.Class != tc.wantClass {
				t.Fatalf("class = %v, want %v (findings %v)", result.Class, tc.wantClass, findingIDs(result))
			}
			finding := findingByID(result, "PERF-01")
			if finding == nil || finding.Severity != tc.wantSev {
				t.Errorf("PERF-01 = %+v, want severity %v", finding, tc.wantSev)
			}
		})
	}
}

func TestReducedSpeedSeverityUsesExpectedSpeedRatio(t *testing.T) {
	thresholds := Default()
	tests := []struct {
		name       string
		negotiated model.Bitrate
		want       model.Severity
	}{
		{"exactly half is poor", 500_000_000, model.SevPoor},
		{"above half warns", 500_000_001, model.SevWarning},
		{"one tenth is poor", 100_000_000, model.SevPoor},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := &Facts{NegotiatedSpeed: tc.negotiated, ExpectedSpeed: 1_000_000_000}
			finding := rulePHY06(facts, thresholds)
			if finding == nil || finding.Severity != tc.want {
				t.Errorf("PHY-06 = %+v, want severity %v", finding, tc.want)
			}
		})
	}
}

func TestThroughputScoreDeductionIsHostGated(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	facts := throughputFacts(30)

	ungated := scoreFor(facts, nil, model.HealthExcellent, thresholds)
	if ungated == nil || *ungated != 75 {
		t.Fatalf("ungated score = %v, want 75", ungated)
	}
	facts.Dir[0].TCPMaxCPUPct = 95
	for _, ruleID := range []string{"HOST-01", "HOST-03", "HOST-04"} {
		findings := []model.Finding{{RuleID: ruleID}}
		got := scoreFor(facts, findings, model.HealthExcellent, thresholds)
		if got == nil || *got != 100 {
			t.Errorf("score with %s = %v, want 100", ruleID, got)
		}
	}
}

// TestThroughputSkippedWhenSpeedUnknown pins the removed NegotiatedSpeed==0
// guards: PERF-01 and the score deduction now rely solely on classifyThroughput
// reporting no deviation for an unknown speed.
func TestThroughputSkippedWhenSpeedUnknown(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	facts := &Facts{NegotiatedSpeed: 0}
	facts.Dir[0] = DirFacts{TCPAvailable: true, TCPBitrate: 50_000_000}
	facts.Dir[1] = DirFacts{TCPAvailable: true, TCPBitrate: 50_000_000}

	if finding := rulePERF01(facts, thresholds); finding != nil {
		t.Errorf("rulePERF01 with unknown speed = %+v, want nil", finding)
	}
	if score := scoreFor(facts, nil, model.HealthExcellent, thresholds); score == nil || *score != 100 {
		t.Errorf("score with unknown speed = %v, want 100 (no deduction)", score)
	}
}

// TestLowSpeedLineRateIsInfoWithNoDeduction locks the <=100M policy: even at
// line rate the tier never passes (info finding), and an info result carries no
// score deduction.
func TestLowSpeedLineRateIsInfoWithNoDeduction(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	facts := &Facts{NegotiatedSpeed: 100_000_000}
	facts.Dir[0] = DirFacts{TCPAvailable: true, TCPBitrate: 100_000_000}
	facts.Dir[1] = DirFacts{TCPAvailable: true, TCPBitrate: 100_000_000}

	finding := rulePERF01(facts, thresholds)
	if finding == nil || finding.Severity != model.SevInfo {
		t.Fatalf("PERF-01 at 100M line rate = %+v, want info (tier never passes)", finding)
	}
	if score := scoreFor(facts, nil, model.HealthExcellent, thresholds); score == nil || *score != 100 {
		t.Errorf("score at 100M line rate = %v, want 100 (info => no deduction)", score)
	}
}

func TestPERF01CapsSingleTrialOutlierOnCleanPhysicalLayer(t *testing.T) {
	thresholds := Default()
	facts := cleanFacts()
	facts.Dir[0] = DirFacts{
		TCPAvailable:            true,
		TCPBitrate:              300_000_000,
		TCPTrialCount:           2,
		TCPThroughputDeviations: 1,
	}
	facts.Dir[1].TCPAvailable = false

	finding := rulePERF01(facts, thresholds)
	if finding == nil || finding.Severity != model.SevWarning {
		t.Fatalf("PERF-01 = %+v, want warning after single-outlier cap", finding)
	}
	if evidence := strings.Join(finding.Evidence, " "); !strings.Contains(evidence, "1 of 2") {
		t.Errorf("PERF-01 evidence = %q, want capped trial count", evidence)
	}

	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	score := scoreFor(facts, nil, model.HealthExcellent, thresholds)
	if score == nil || *score != 90 {
		t.Errorf("score = %v, want warning deduction 10", score)
	}
}

func TestPERF01OutlierCapRequiresCleanPhysicalEvidence(t *testing.T) {
	base := func() *Facts {
		facts := cleanFacts()
		facts.Dir[0] = DirFacts{
			TCPAvailable:            true,
			TCPBitrate:              300_000_000,
			TCPTrialCount:           2,
			TCPThroughputDeviations: 1,
		}
		facts.Dir[1].TCPAvailable = false
		return facts
	}
	tests := []struct {
		name   string
		mutate func(*Facts)
	}{
		{name: "one trial", mutate: func(f *Facts) { f.Dir[0].TCPTrialCount = 1 }},
		{name: "no deviation", mutate: func(f *Facts) { f.Dir[0].TCPThroughputDeviations = 0 }},
		{name: "repeated deviation", mutate: func(f *Facts) { f.Dir[0].TCPThroughputDeviations = 2 }},
		{name: "pc1 counters unreliable", mutate: func(f *Facts) { f.PC1.DeltaOK = false }},
		{name: "pc2 counters unreliable", mutate: func(f *Facts) { f.PC2.DeltaOK = false }},
		{name: "link down", mutate: func(f *Facts) { f.LinkUpAtEnd = false }},
		{name: "CRC movement", mutate: func(f *Facts) { f.PC1.CRCClassErrors = 1 }},
		{name: "unclassified receive errors", mutate: func(f *Facts) { f.PC1.UnclassifiedRXErrors = 1 }},
		// The cap asserts a clean physical layer. A side that exposed no
		// receive-error counter reports reliable, empty counters by construction,
		// which cannot support that assertion.
		{name: "pc1 never measured receive errors", mutate: func(f *Facts) { f.PC1.RXErrorEvidence = false }},
		{name: "pc2 never measured receive errors", mutate: func(f *Facts) { f.PC2.RXErrorEvidence = false }},
		{name: "carrier event", mutate: func(f *Facts) { f.PC1.CarrierEvents = 1 }},
		{name: "renegotiation", mutate: func(f *Facts) { f.Renegotiations = 1 }},
		{name: "half duplex", mutate: func(f *Facts) { f.HalfDuplex = true }},
		{name: "reduced negotiated speed", mutate: func(f *Facts) { f.ExpectedSpeed = 2_000_000_000 }},
		{name: "cable diagnostic fault", mutate: func(f *Facts) {
			f.CableTestRan = true
			f.CableTestPairs = []model.CablePairResult{{Pair: "A", Status: model.PairOpen}}
		}},
		{name: "frame size errors", mutate: func(f *Facts) { f.PC1.JabberSizeErrors = 1 }},
		{name: "carrier PHY errors", mutate: func(f *Facts) { f.PC1.CarrierPHYErrors = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := base()
			tc.mutate(facts)
			finding := rulePERF01(facts, Default())
			if finding == nil || finding.Severity != model.SevPoor {
				t.Errorf("PERF-01 = %+v, want uncapped poor", finding)
			}
		})
	}
}

func TestPERF01OutlierCapIsPerDirection(t *testing.T) {
	facts := cleanFacts()
	for i := range facts.Dir {
		facts.Dir[i] = DirFacts{
			TCPAvailable:            true,
			TCPBitrate:              300_000_000,
			TCPTrialCount:           2,
			TCPThroughputDeviations: 1,
		}
	}
	facts.Dir[1].TCPThroughputDeviations = 2

	finding := rulePERF01(facts, Default())
	if finding == nil || finding.Severity != model.SevPoor {
		t.Errorf("PERF-01 = %+v, want uncapped poor from pc2->pc1", finding)
	}
}

func TestPERF01CapDoesNotHideCollapseFinding(t *testing.T) {
	facts := cleanFacts()
	facts.Dir[0] = DirFacts{
		TCPAvailable:            true,
		TCPBitrate:              300_000_000,
		TCPTrialCount:           2,
		TCPThroughputDeviations: 1,
		TCPCollapses:            Default().TCPCollapsePoorAt,
	}
	facts.Dir[1].TCPAvailable = false

	result := Evaluate(facts)
	perf01 := findingByID(result, "PERF-01")
	if perf01 == nil || perf01.Severity != model.SevWarning {
		t.Errorf("PERF-01 = %+v, want capped warning", perf01)
	}
	perf03 := findingByID(result, "PERF-03")
	if perf03 == nil || perf03.Severity != model.SevPoor {
		t.Errorf("PERF-03 = %+v, want retained poor collapse evidence", perf03)
	}
	if result.Class != model.HealthPoor {
		t.Errorf("class = %v, want POOR from collapse evidence", result.Class)
	}
}
