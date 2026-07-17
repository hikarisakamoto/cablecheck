package evaluate

import (
	"testing"

	"cablecheck/internal/model"
)

// band returns the inclusive score band for a class, mirroring §4.5 of the
// design: FAILED <=25, POOR 26-50, WARNING 51-79, GOOD 80-94, EXCELLENT 95-100.
func band(t *testing.T, class model.HealthClass) (int, int) {
	t.Helper()
	switch class {
	case model.HealthFailed:
		return 0, 25
	case model.HealthPoor:
		return 26, 50
	case model.HealthWarning:
		return 51, 79
	case model.HealthGood:
		return 80, 94
	case model.HealthExcellent:
		return 95, 100
	}
	t.Fatalf("no score band for class %v", class)
	return 0, 0
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
