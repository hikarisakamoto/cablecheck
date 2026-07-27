package evaluate

import (
	"math"
	"math/rand"
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
		}

		d.UDPAvailable = rng.Intn(5) != 0
		d.UDPTargetReached = d.UDPAvailable && rng.Intn(5) != 0
		d.UDPLossPct = 0
		d.UDPJitterMs = 0
		d.UDPOutOfOrderPct = 0
		if d.UDPTargetReached {
			d.UDPLossPct = []float64{0, thresholds.UDPLossWarningAt, thresholds.UDPLossPoorAbove, 5}[rng.Intn(4)]
			d.UDPJitterMs = []float64{0, thresholds.UDPJitterWarningAbove, 8}[rng.Intn(3)]
			d.UDPOutOfOrderPct = []float64{0, thresholds.UDPReorderWarningAbove, 1}[rng.Intn(3)]
		}
	}

	f.MaxCPUPct = []float64{0, thresholds.CPUHostLimitedAbove, math.Nextafter(thresholds.CPUHostLimitedAbove, math.Inf(1)), 100}[rng.Intn(4)]
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
			f.Dir[i].UDPAvailable = false
			f.Dir[i].UDPTargetReached = false
			f.Dir[i].UDPLossPct = 0
			f.Dir[i].UDPJitterMs = 0
			f.Dir[i].UDPOutOfOrderPct = 0
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
