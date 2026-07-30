package evaluate

import (
	"math"
	"math/rand"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"cablecheck/internal/model"
)

// band returns the configured inclusive score band for a class.
func band(t *testing.T, class model.HealthClass) (int, int) {
	t.Helper()
	if class == model.HealthInconclusive {
		t.Fatalf("no score band for class %v", class)
	}
	b := Default().scoreBand(class)
	return b.Min, b.Max
}

func TestScoreClampedToClassBand(t *testing.T) {
	t.Run("excellent clean run scores 100", func(t *testing.T) {
		res := Evaluate(cleanFacts())
		if res.Class != model.HealthExcellent {
			t.Fatalf("class = %v, want EXCELLENT", res.Class)
		}
		if res.Score == nil || *res.Score != 100 {
			t.Errorf("score = %v, want 100", res.Score)
		}
	})

	t.Run("good info deviation clamps 100 down to 94", func(t *testing.T) {
		f := cleanFacts()
		f.Dir[0].TCPBitrate = 800_000_000 // ratio 0.8 -> PERF-01 info note, no deduction
		f.Dir[1].TCPBitrate = 800_000_000
		res := Evaluate(f)
		if res.Class != model.HealthGood {
			t.Fatalf("class = %v, want GOOD (findings %v)", res.Class, findingIDs(res))
		}
		if res.Score == nil || *res.Score != 94 {
			t.Errorf("score = %v, want 94 (raw 100 clamped into GOOD band)", res.Score)
		}
	})

	t.Run("warning with heavy deductions clamps up to 51", func(t *testing.T) {
		f := cleanFacts()
		f.PC1.CRCClassErrors = 10 // PHY-02 WARNING, -20
		f.Dir[0].UDPJitterMs = 6  // TR-08 WARNING, -5
		f.Dir[0].TCPRetransRate = 0.005
		f.Dir[1].TCPRetransRate = 0.005 // TR-06 WARNING, -5 per direction
		f.Dir[0].TCPCoV = 0.20          // PERF-02 WARNING, -5
		f.Dir[0].TCPCollapses = 2       // PERF-03 WARNING, -5 each
		res := Evaluate(f)
		if res.Class != model.HealthWarning {
			t.Fatalf("class = %v, want WARNING (findings %v)", res.Class, findingIDs(res))
		}
		if res.Score == nil || *res.Score != 51 {
			t.Errorf("score = %v, want 51 (raw 50 clamped into WARNING band)", res.Score)
		}
	})

	t.Run("poor half duplex clamps down to 50", func(t *testing.T) {
		f := cleanFacts()
		f.HalfDuplex = true // PHY-05 POOR, deduction only -25
		res := Evaluate(f)
		if res.Class != model.HealthPoor {
			t.Fatalf("class = %v, want POOR (findings %v)", res.Class, findingIDs(res))
		}
		if res.Score == nil || *res.Score != 50 {
			t.Errorf("score = %v, want 50 (raw 75 clamped into POOR band)", res.Score)
		}
	})

	t.Run("failed link loss clamps down to 25", func(t *testing.T) {
		f := cleanFacts()
		f.LinkUpAtEnd = false // PHY-01 FAILED with no deduction row
		res := Evaluate(f)
		if res.Class != model.HealthFailed {
			t.Fatalf("class = %v, want FAILED (findings %v)", res.Class, findingIDs(res))
		}
		if res.Score == nil || *res.Score != 25 {
			t.Errorf("score = %v, want 25 (raw 100 clamped into FAILED band)", res.Score)
		}
	})

	t.Run("inconclusive has no score", func(t *testing.T) {
		f := cleanFacts()
		f.VirtualInterface = true
		res := Evaluate(f)
		if res.Class != model.HealthInconclusive {
			t.Fatalf("class = %v, want INCONCLUSIVE", res.Class)
		}
		if res.Score != nil {
			t.Errorf("score = %v, want nil for INCONCLUSIVE", *res.Score)
		}
	})

	t.Run("every classified result stays inside its band", func(t *testing.T) {
		scenarios := map[string]*Facts{
			"clean":       cleanFacts(),
			"crc storm":   func() *Facts { f := cleanFacts(); f.PC1.CRCClassErrors = 2000; return f }(),
			"carrier":     func() *Facts { f := cleanFacts(); f.PC2.CarrierEvents = 5; return f }(),
			"ping loss":   func() *Facts { f := cleanFacts(); f.Dir[1].PingLossPct = 3; return f }(),
			"slow tcp":    func() *Facts { f := cleanFacts(); f.Dir[0].TCPBitrate = 500_000_000; return f }(),
			"half duplex": func() *Facts { f := cleanFacts(); f.HalfDuplex = true; return f }(),
		}
		for name, f := range scenarios {
			res := Evaluate(f)
			if res.Class == model.HealthInconclusive {
				if res.Score != nil {
					t.Errorf("%s: score = %v, want nil for INCONCLUSIVE", name, *res.Score)
				}
				continue
			}
			if res.Score == nil {
				t.Errorf("%s: score = nil, want a value for class %v", name, res.Class)
				continue
			}
			lo, hi := band(t, res.Class)
			if *res.Score < lo || *res.Score > hi {
				t.Errorf("%s: score %d outside band [%d,%d] for class %v", name, *res.Score, lo, hi, res.Class)
			}
		}
	})
}

func TestScoreUsesCarriedCollapseIntervalLengths(t *testing.T) {
	report := &model.Report{Tests: model.TestsSection{TCP: []model.TCPResult{{
		Direction:             model.DirectionPC1ToPC2,
		ReceiverBitsPerSecond: 900_000_000,
		Collapses:             []model.TCPCollapseEvent{{Len: 2}},
	}}}}
	facts := FactsFromReport(report)
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}

	score := scoreFor(facts, nil, model.HealthExcellent, thresholds)
	if score == nil || *score != 90 {
		t.Errorf("score = %v, want 90 from two carried collapsed intervals", score)
	}
}

func TestNonCPUHostLimitSuppressesHostSensitivePerformanceDeductions(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}

	tests := []struct {
		name        string
		facts       *Facts
		wantUngated int
	}{
		{
			name: "coefficient of variation",
			facts: &Facts{Dir: [2]DirFacts{{
				TCPAvailable: true,
				TCPCoV:       0.31,
			}}},
			wantUngated: 85,
		},
		{
			name: "collapse intervals",
			facts: &Facts{Dir: [2]DirFacts{{
				TCPAvailable: true,
				TCPCollapses: 2,
			}}},
			wantUngated: 90,
		},
		{
			name:        "directional asymmetry",
			facts:       asymmetryFacts(100, 60),
			wantUngated: 95,
		},
		{
			name: "combined symptoms",
			facts: &Facts{Dir: [2]DirFacts{
				{TCPAvailable: true, TCPBitrate: 100, TCPCoV: 0.31, TCPCollapses: 4},
				{TCPAvailable: true, TCPBitrate: 60},
			}},
			wantUngated: 60,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ungated := scoreFor(tc.facts, nil, model.HealthExcellent, thresholds)
			if ungated == nil || *ungated != tc.wantUngated {
				t.Fatalf("ungated score = %v, want %d", ungated, tc.wantUngated)
			}

			for _, ruleID := range []string{"HOST-03", "HOST-04"} {
				findings := []model.Finding{{RuleID: ruleID}}
				got := scoreFor(tc.facts, findings, model.HealthExcellent, thresholds)
				if got == nil || *got != 100 {
					t.Errorf("score with %s = %v, want 100", ruleID, got)
				}
			}
		})
	}
}

// TestNegligibleReceiveRingDropsDoNotSilenceThePerformanceScore is the field
// regression behind a 100/100 verdict on a run that visibly collapsed: two
// dropped frames out of 18.5 million raised HOST-04, and HOST-04 suppresses
// every host-sensitive performance deduction.
func TestNegligibleReceiveRingDropsDoNotSilenceThePerformanceScore(t *testing.T) {
	thresholds := Default()
	thresholds.WarningScoreBand = ScoreBand{Min: 0, Max: 100}
	factsWithDrops := func(missed, frames uint64) *Facts {
		f := cleanFacts()
		f.PC2.MissedErrors = missed
		f.PC2.FramesReceived = frames
		f.Dir[0].TCPCollapses = 2
		return f
	}

	t.Run("negligible drops leave the collapse deduction in place", func(t *testing.T) {
		res := evaluate(factsWithDrops(2, 18_500_000), thresholds)
		if slices.Contains(findingIDs(res), "HOST-04") {
			t.Errorf("findings = %v, want no HOST-04 for 2 drops in 18.5M frames", findingIDs(res))
		}
		if res.Score == nil || *res.Score != 90 {
			t.Errorf("score = %v, want 90 (two collapse intervals cost 10)", res.Score)
		}
	})

	t.Run("drops above the rate still gate the deduction", func(t *testing.T) {
		res := evaluate(factsWithDrops(2_000, 18_500_000), thresholds)
		if !slices.Contains(findingIDs(res), "HOST-04") {
			t.Errorf("findings = %v, want HOST-04 for a receive ring dropping 2000 frames", findingIDs(res))
		}
		if res.Score == nil || *res.Score != 100 {
			t.Errorf("score = %v, want 100: a genuinely starved ring gates performance deductions", res.Score)
		}
	})
}

func TestCPUHostLimitGatesTCPDeductionsPerDirection(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}

	t.Run("clean direction retains variation and collapse deductions", func(t *testing.T) {
		facts := &Facts{Dir: [2]DirFacts{
			{
				TCPAvailable: true, TCPBitrate: 100, TCPCoV: 0.31, TCPCollapses: 2,
				TCPMaxCPUPct: 95, TCPSenderMaxCPUPct: 95,
			},
			{
				TCPAvailable: true, TCPBitrate: 60, TCPCoV: 0.20, TCPCollapses: 2,
				TCPMaxCPUPct: 20, TCPSenderMaxCPUPct: 20,
			},
		}}

		score := scoreFor(facts, []model.Finding{{RuleID: "HOST-01"}}, model.HealthExcellent, thresholds)
		if score == nil || *score != 85 {
			t.Errorf("score = %v, want 85 from clean-direction CoV and two collapse intervals", score)
		}
	})

	t.Run("clean direction retains throughput deduction", func(t *testing.T) {
		facts := &Facts{NegotiatedSpeed: 1_000_000_000, Dir: [2]DirFacts{
			{
				TCPAvailable: true, TCPBitrate: 300_000_000,
				TCPMaxCPUPct: 95, TCPSenderMaxCPUPct: 95,
			},
			{
				TCPAvailable: true, TCPBitrate: 600_000_000,
				TCPMaxCPUPct: 20, TCPSenderMaxCPUPct: 20,
			},
		}}

		score := scoreFor(facts, []model.Finding{{RuleID: "HOST-01"}}, model.HealthExcellent, thresholds)
		if score == nil || *score != 90 {
			t.Errorf("score = %v, want 90 from clean-direction warning throughput", score)
		}
	})
}

func TestCPUHostLimitGatesUDPDeductionsPerDirection(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	facts := &Facts{MaxCPUPct: 95, Dir: [2]DirFacts{
		{
			UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 5,
			UDPMaxCPUPct: 95,
		},
		{
			UDPAvailable: true, UDPTargetReached: true, UDPLossPct: 1,
			UDPMaxCPUPct: 20,
		},
	}}

	score := scoreFor(facts, []model.Finding{{RuleID: "HOST-01"}}, model.HealthExcellent, thresholds)
	if score == nil || *score != 95 {
		t.Errorf("score = %v, want 95 from only the clean direction's warning loss", score)
	}
}

func TestAsymmetryScoreGateUsesSenderCPU(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	tests := []struct {
		name       string
		dir0Sender float64
		dir1Sender float64
		dir0Max    float64
		want       int
	}{
		{name: "clean senders deduct", dir0Sender: 20, dir1Sender: 30, want: 95},
		{name: "exact boundary deducts", dir0Sender: thresholds.CPUHostLimitedAbove, dir1Sender: 30, want: 95},
		{name: "hot first sender suppresses", dir0Sender: math.Nextafter(thresholds.CPUHostLimitedAbove, math.Inf(1)), dir1Sender: 30, want: 100},
		{name: "hot second sender suppresses", dir0Sender: 20, dir1Sender: 95, want: 100},
		{name: "hot receiver does not suppress sender caveat", dir0Sender: 20, dir1Sender: 30, dir0Max: 95, want: 95},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := asymmetryFacts(100, 60)
			facts.Dir[0].TCPSenderMaxCPUPct = tc.dir0Sender
			facts.Dir[1].TCPSenderMaxCPUPct = tc.dir1Sender
			facts.Dir[0].TCPMaxCPUPct = tc.dir0Max
			score := scoreFor(facts, nil, model.HealthExcellent, thresholds)
			if score == nil || *score != tc.want {
				t.Errorf("score = %v, want %d", score, tc.want)
			}
		})
	}
}

func TestCPUHostLimitScoreGateUsesSuppliedExclusiveThreshold(t *testing.T) {
	thresholds := Default()
	thresholds.CPUHostLimitedAbove = 50
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	facts := &Facts{MaxCPUPct: 50, Dir: [2]DirFacts{{
		TCPAvailable: true,
		TCPCoV:       0.31,
		TCPMaxCPUPct: 50,
	}}}

	atBoundary := ruleHOST01(facts, thresholds)
	if atBoundary != nil {
		t.Fatalf("HOST-01 at supplied boundary = %+v, want nil", atBoundary)
	}
	if score := scoreFor(facts, nil, model.HealthExcellent, thresholds); score == nil || *score != 85 {
		t.Errorf("score at supplied boundary = %v, want 85", score)
	}

	facts.MaxCPUPct = math.Nextafter(thresholds.CPUHostLimitedAbove, math.Inf(1))
	facts.Dir[0].TCPMaxCPUPct = facts.MaxCPUPct
	aboveBoundary := ruleHOST01(facts, thresholds)
	if aboveBoundary == nil {
		t.Fatal("HOST-01 above supplied boundary = nil, want marker")
	}
	if score := scoreFor(facts, []model.Finding{*aboveBoundary}, model.HealthExcellent, thresholds); score == nil || *score != 100 {
		t.Errorf("score above supplied boundary = %v, want 100", score)
	}
}

func TestHostLimitDoesNotSuppressNonHostSensitiveDeductions(t *testing.T) {
	thresholds := Default()
	thresholds.ExcellentScoreBand = ScoreBand{Min: 0, Max: 100}
	facts := &Facts{Dir: [2]DirFacts{{
		TCPAvailable:   true,
		TCPRetransRate: 0.005,
		TCPCoV:         0.31,
		TCPMaxCPUPct:   95,
		UDPJitterMs:    6,
	}}}
	findings := []model.Finding{{RuleID: "HOST-01"}}

	score := scoreFor(facts, findings, model.HealthExcellent, thresholds)
	if score == nil || *score != 90 {
		t.Errorf("host-limited score = %v, want 90 from retransmission and jitter deductions only", score)
	}
}

type classScoreBand struct {
	class model.HealthClass
	band  ScoreBand
}

// classScoreBands deliberately names each configured field rather than using
// scoreBand. The property tests must be able to catch a wrong scoreBand switch
// arm instead of reproducing it in their expected result.
func classScoreBands(thresholds Thresholds) []classScoreBand {
	return []classScoreBand{
		{class: model.HealthFailed, band: thresholds.FailedScoreBand},
		{class: model.HealthPoor, band: thresholds.PoorScoreBand},
		{class: model.HealthWarning, band: thresholds.WarningScoreBand},
		{class: model.HealthGood, band: thresholds.GoodScoreBand},
		{class: model.HealthExcellent, band: thresholds.ExcellentScoreBand},
	}
}

func TestScoreForAlwaysInRequestedBand(t *testing.T) {
	thresholds := Default()
	bands := classScoreBands(thresholds)
	property := func(seed uint64) bool {
		facts := randomizedFacts(seed)
		findings := evaluate(facts, thresholds).Findings
		for _, entry := range bands {
			score := scoreFor(facts, findings, entry.class, thresholds)
			if score == nil || *score < entry.band.Min || *score > entry.band.Max {
				return false
			}
		}
		return scoreFor(facts, findings, model.HealthInconclusive, thresholds) == nil
	}
	config := &quick.Config{
		MaxCount: 5_000,
		Rand:     rand.New(rand.NewSource(11_101)),
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateScoreAgreesWithClass(t *testing.T) {
	thresholds := Default()
	bands := classScoreBands(thresholds)
	property := func(seed uint64) bool {
		result := evaluate(randomizedFacts(seed), thresholds)
		if result.Class == model.HealthInconclusive {
			return result.Score == nil
		}
		if result.Score == nil {
			return false
		}

		matches := 0
		matchedClass := model.HealthClass("")
		for _, entry := range bands {
			if *result.Score >= entry.band.Min && *result.Score <= entry.band.Max {
				matches++
				matchedClass = entry.class
			}
		}
		return matches == 1 && matchedClass == result.Class
	}
	config := &quick.Config{
		MaxCount: 5_000,
		Rand:     rand.New(rand.NewSource(11_102)),
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

// randomizedFacts expands a quick-check seed into a bounded, coherent Facts
// value. Raw recursive generation would create impossible states such as
// unavailable tests with measurements, unreliable counters carrying deltas,
// negative counts, and non-finite percentages.
func randomizedFacts(seed uint64) *Facts {
	rng := rand.New(rand.NewSource(int64(seed)))
	thresholds := Default()
	f := cleanFacts()

	// One draw in five is a clean or near-clean gigabit link. The defect
	// injection below compounds many independent failure probabilities and in
	// practice never lands on GOOD or EXCELLENT, so without this branch
	// TestEvaluateScoreAgreesWithClass would never check the score/class
	// agreement invariant for the two healthiest classes.
	if rng.Intn(5) == 0 {
		switch rng.Intn(3) {
		case 0:
			// pristine run: cleanFacts already scores EXCELLENT.
		case 1:
			// info-only throughput deviation (ratio 0.8) demotes to GOOD.
			ratio := model.Bitrate(float64(f.NegotiatedSpeed) * 0.8)
			f.Dir[0].TCPBitrate = ratio
			f.Dir[1].TCPBitrate = ratio
		case 2:
			// a single warning-level blemish yields WARNING.
			f.Dir[0].UDPJitterMs = math.Nextafter(thresholds.UDPJitterWarningAbove, math.Inf(1))
		}
		return f
	}

	speeds := []struct {
		negotiated model.Bitrate
		expected   model.Bitrate
	}{
		{0, 0},
		{100_000_000, 100_000_000},
		{100_000_000, 1_000_000_000},
		{500_000_000, 1_000_000_000},
		{1_000_000_000, 1_000_000_000},
		{2_500_000_000, 2_500_000_000},
	}
	speed := speeds[rng.Intn(len(speeds))]
	f.NegotiatedSpeed = speed.negotiated
	f.ExpectedSpeed = speed.expected
	f.LinkUpAtEnd = rng.Intn(12) != 0
	f.HalfDuplex = rng.Intn(12) == 0
	if rng.Intn(8) == 0 {
		f.Renegotiations = 1 + rng.Intn(4)
	}

	for _, side := range []*SideFacts{&f.PC1, &f.PC2} {
		side.CountersAvailable = rng.Intn(5) != 0
		side.DeltaOK = side.CountersAvailable && rng.Intn(6) != 0
		side.CRCClassErrors = 0
		side.CarrierEvents = 0
		side.JabberSizeErrors = 0
		side.FifoOverrun = 0
		side.MissedErrors = 0
		side.CarrierPHYErrors = 0
		if side.DeltaOK {
			if rng.Intn(4) == 0 {
				side.CRCClassErrors = []uint64{1, thresholds.CRCPoorAbove, thresholds.CRCPoorAbove + 1, thresholds.CRCFailedAbove + 1}[rng.Intn(4)]
			}
			if rng.Intn(6) == 0 {
				side.CarrierEvents = uint64(1 + rng.Intn(int(thresholds.CarrierFailedAt)+2))
			}
			if rng.Intn(5) == 0 {
				side.JabberSizeErrors = uint64(1 + rng.Intn(int(thresholds.FrameSizePoorAbove)+3))
			}
			if rng.Intn(5) == 0 {
				side.CarrierPHYErrors = []uint64{1, thresholds.CarrierPHYPoorAbove, thresholds.CarrierPHYPoorAbove + 1, thresholds.CarrierPHYFailedAbove + 1}[rng.Intn(4)]
			}
			if rng.Intn(5) == 0 {
				side.FifoOverrun = uint64(1 + rng.Intn(20))
			}
			if rng.Intn(5) == 0 {
				side.MissedErrors = uint64(1 + rng.Intn(20))
			}
		}
	}

	pingLosses := []float64{0, 0.05, thresholds.PingLossPoorAbove, math.Nextafter(thresholds.PingLossPoorAbove, math.Inf(1)), 2}
	for i := range f.Dir {
		d := &f.Dir[i]
		d.PingLossPct = pingLosses[rng.Intn(len(pingLosses))]
		if rng.Intn(5) == 0 {
			d.PingDuplicates = rng.Intn(4)
			d.PingSpikes = rng.Intn(thresholds.PingSpikesWarningAbove + 4)
			d.PingMaxGap = timeFromMillis(rng.Intn(1_501))
		}
		d.FullSizeAvailable = rng.Intn(5) != 0
		if d.FullSizeAvailable && rng.Intn(5) == 0 {
			d.FullSizeLossPct = []float64{0.05, 1, 5}[rng.Intn(3)]
			d.FragErrors = rng.Intn(3)
		}

		d.TCPAvailable = rng.Intn(5) != 0
		d.TCPBitrate = 0
		d.TCPRetransRate = 0
		d.TCPCoV = 0
		d.TCPCollapses = 0
		d.TCPMaxCPUPct = 0
		d.TCPSenderMaxCPUPct = 0
		if d.TCPAvailable {
			base := f.NegotiatedSpeed
			if base == 0 {
				base = 1_000_000_000
			}
			ratios := []float64{0.2, 0.4, 0.7, 0.9, 0.98}
			d.TCPBitrate = model.Bitrate(float64(base) * ratios[rng.Intn(len(ratios))])
			d.TCPRetransRate = []float64{0, thresholds.TCPRetransWarningAt, thresholds.TCPRetransPoorAbove, 0.03}[rng.Intn(4)]
			d.TCPCoV = []float64{0, thresholds.TCPCoVWarningAt, thresholds.TCPCoVPoorAbove, 0.5}[rng.Intn(4)]
			d.TCPCollapses = rng.Intn(thresholds.TCPCollapsePoorAt + 3)
			d.TCPSenderMaxCPUPct = []float64{0, thresholds.CPUHostLimitedAbove, 95, 100}[rng.Intn(4)]
			receiverCPU := []float64{0, thresholds.CPUHostLimitedAbove, 95, 100}[rng.Intn(4)]
			d.TCPMaxCPUPct = max(d.TCPSenderMaxCPUPct, receiverCPU)
		}

		d.UDPAvailable = rng.Intn(5) != 0
		d.UDPTargetReached = d.UDPAvailable && rng.Intn(5) != 0
		d.UDPLossPct = 0
		d.UDPJitterMs = 0
		d.UDPOutOfOrderPct = 0
		d.UDPMaxCPUPct = 0
		if d.UDPTargetReached {
			d.UDPLossPct = []float64{0, thresholds.UDPLossWarningAt, thresholds.UDPLossPoorAbove, 5}[rng.Intn(4)]
			d.UDPJitterMs = []float64{0, thresholds.UDPJitterWarningAbove, 8}[rng.Intn(3)]
			d.UDPOutOfOrderPct = []float64{0, thresholds.UDPReorderWarningAbove, 1}[rng.Intn(3)]
			d.UDPMaxCPUPct = []float64{0, thresholds.CPUHostLimitedAbove, 95, 100}[rng.Intn(4)]
		}
	}

	f.MaxCPUPct = []float64{0, thresholds.CPUHostLimitedAbove, math.Nextafter(thresholds.CPUHostLimitedAbove, math.Inf(1)), 100}[rng.Intn(4)]
	for i := range f.Dir {
		f.MaxCPUPct = max(f.MaxCPUPct, f.Dir[i].TCPMaxCPUPct, f.Dir[i].UDPMaxCPUPct)
	}
	f.USBAdapter = rng.Intn(5) == 0
	f.VirtualInterface = rng.Intn(20) == 0
	f.Partial = rng.Intn(12) == 0
	f.UDPRateAssumed = rng.Intn(8) == 0
	if rng.Intn(10) == 0 {
		f.Unavailable = []string{"udp"}
		for i := range f.Dir {
			f.Dir[i].UDPAvailable = false
			f.Dir[i].UDPTargetReached = false
			f.Dir[i].UDPLossPct = 0
			f.Dir[i].UDPJitterMs = 0
			f.Dir[i].UDPOutOfOrderPct = 0
			f.Dir[i].UDPMaxCPUPct = 0
		}
	}
	f.ThroughputUnreachable = rng.Intn(20) == 0
	if f.ThroughputUnreachable {
		f.MaxCPUPct = 0
		f.Unavailable = []string{"tcp", "udp"}
		for i := range f.Dir {
			f.Dir[i].TCPAvailable = false
			f.Dir[i].TCPBitrate = 0
			f.Dir[i].TCPRetransRate = 0
			f.Dir[i].TCPCoV = 0
			f.Dir[i].TCPCollapses = 0
			f.Dir[i].TCPMaxCPUPct = 0
			f.Dir[i].TCPSenderMaxCPUPct = 0
			f.Dir[i].UDPAvailable = false
			f.Dir[i].UDPTargetReached = false
			f.Dir[i].UDPLossPct = 0
			f.Dir[i].UDPJitterMs = 0
			f.Dir[i].UDPOutOfOrderPct = 0
			f.Dir[i].UDPMaxCPUPct = 0
		}
	}

	f.CableTestRan = rng.Intn(8) == 0
	if f.CableTestRan {
		statuses := []model.PairStatus{model.PairOK, model.PairUnspecified, model.PairImpedance, model.PairOpen}
		f.CableTestPairs = []model.CablePairResult{{Pair: "A", Status: statuses[rng.Intn(len(statuses))]}}
	}
	return f
}

func timeFromMillis(milliseconds int) time.Duration {
	return time.Duration(milliseconds) * time.Millisecond
}
