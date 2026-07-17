package evaluate

import (
	"reflect"
	"testing"

	"cablecheck/internal/model"
)

// cleanFacts describes a flawless gigabit run: link up, reliable clean
// counters, full-rate TCP both ways, lossless UDP at target rate. Evaluate on
// it must yield EXCELLENT with no findings.
func cleanFacts() *Facts {
	f := &Facts{
		NegotiatedSpeed: 1_000_000_000,
		ExpectedSpeed:   1_000_000_000,
		LinkUpAtEnd:     true,
	}
	f.PC1 = SideFacts{DeltaOK: true, CountersAvailable: true}
	f.PC2 = SideFacts{DeltaOK: true, CountersAvailable: true}
	for i := range f.Dir {
		f.Dir[i].TCPAvailable = true
		f.Dir[i].TCPBitrate = 941_000_000
		f.Dir[i].UDPAvailable = true
		f.Dir[i].UDPTargetReached = true
	}
	return f
}

// findingIDs extracts the rule IDs of a result's findings in order.
func findingIDs(res Result) []string {
	ids := make([]string, 0, len(res.Findings))
	for _, fd := range res.Findings {
		ids = append(ids, fd.RuleID)
	}
	return ids
}

func TestClassifyFoldOrdering(t *testing.T) {
	t.Run("clean run is excellent", func(t *testing.T) {
		res := Evaluate(cleanFacts())
		if res.Class != model.HealthExcellent {
			t.Errorf("class = %v, want EXCELLENT (findings %v)", res.Class, findingIDs(res))
		}
		if len(res.Findings) != 0 {
			t.Errorf("findings = %v, want none", findingIDs(res))
		}
	})

	t.Run("physical failed dominates host limits", func(t *testing.T) {
		f := cleanFacts()
		f.LinkUpAtEnd = false
		f.MaxCPUPct = 95
		res := Evaluate(f)
		if res.Class != model.HealthFailed {
			t.Errorf("class = %v, want FAILED (physical evidence is never softened)", res.Class)
		}
	})

	t.Run("physical poor dominates host limits", func(t *testing.T) {
		f := cleanFacts()
		f.PC1.CarrierEvents = 2
		f.MaxCPUPct = 95
		res := Evaluate(f)
		if res.Class != model.HealthPoor {
			t.Errorf("class = %v, want POOR (physical evidence is never softened)", res.Class)
		}
	})

	t.Run("host limited performance poor is inconclusive", func(t *testing.T) {
		f := cleanFacts()
		f.Dir[0].TCPBitrate = 300_000_000 // ratio 0.3 -> PERF-01 POOR (host sensitive)
		f.Dir[1].TCPBitrate = 300_000_000
		f.MaxCPUPct = 95 // HOST-01 marker
		res := Evaluate(f)
		if res.Class != model.HealthInconclusive {
			t.Errorf("class = %v, want INCONCLUSIVE (all POOR findings are host-sensitive)", res.Class)
		}
		if res.Score != nil {
			t.Errorf("score = %v, want nil for INCONCLUSIVE", *res.Score)
		}
	})

	t.Run("performance poor without host marker stays poor", func(t *testing.T) {
		f := cleanFacts()
		f.Dir[0].TCPBitrate = 300_000_000
		f.Dir[1].TCPBitrate = 300_000_000
		res := Evaluate(f)
		if res.Class != model.HealthPoor {
			t.Errorf("class = %v, want POOR", res.Class)
		}
	})

	t.Run("transport poor is not host sensitive and stays poor", func(t *testing.T) {
		f := cleanFacts()
		f.Dir[0].PingLossPct = 0.5 // TR-01 POOR, not host-sensitive
		f.MaxCPUPct = 95
		res := Evaluate(f)
		if res.Class != model.HealthPoor {
			t.Errorf("class = %v, want POOR (ping loss is not host-sensitive)", res.Class)
		}
	})

	t.Run("virtual interface forces inconclusive", func(t *testing.T) {
		f := cleanFacts()
		f.VirtualInterface = true
		res := Evaluate(f)
		if res.Class != model.HealthInconclusive {
			t.Errorf("class = %v, want INCONCLUSIVE (HOST-02)", res.Class)
		}
	})

	t.Run("partial run downgrades excellent to inconclusive", func(t *testing.T) {
		f := cleanFacts()
		f.Partial = true
		res := Evaluate(f)
		if res.Class != model.HealthInconclusive {
			t.Errorf("class = %v, want INCONCLUSIVE (LIM-03)", res.Class)
		}
	})

	t.Run("partial run keeps warning evidence", func(t *testing.T) {
		f := cleanFacts()
		f.Partial = true
		f.Dir[0].PingLossPct = 0.05 // TR-01 WARNING
		res := Evaluate(f)
		if res.Class != model.HealthWarning {
			t.Errorf("class = %v, want WARNING (LIM-03 only downgrades EXCELLENT/GOOD)", res.Class)
		}
	})

	t.Run("missing critical evidence downgrades to inconclusive", func(t *testing.T) {
		f := cleanFacts()
		for i := range f.Dir {
			f.Dir[i].TCPAvailable = false
			f.Dir[i].TCPBitrate = 0
		}
		res := Evaluate(f)
		if res.Class != model.HealthInconclusive {
			t.Errorf("class = %v, want INCONCLUSIVE (LIM-01, no TCP at all)", res.Class)
		}
	})

	t.Run("noncritical unavailable caps excellent to good", func(t *testing.T) {
		f := cleanFacts()
		f.Unavailable = []string{"udp"}
		res := Evaluate(f)
		if res.Class != model.HealthGood {
			t.Errorf("class = %v, want GOOD (LIM-02 caps EXCELLENT)", res.Class)
		}
	})

	t.Run("noncritical unavailable leaves warning alone", func(t *testing.T) {
		f := cleanFacts()
		f.Unavailable = []string{"udp"}
		f.Dir[0].PingLossPct = 0.05
		res := Evaluate(f)
		if res.Class != model.HealthWarning {
			t.Errorf("class = %v, want WARNING (LIM-02 only caps EXCELLENT)", res.Class)
		}
	})

	t.Run("physical warning yields warning", func(t *testing.T) {
		f := cleanFacts()
		f.NegotiatedSpeed = 100_000_000 // PHY-06 vs 1G expected
		f.Dir[0].TCPBitrate = 94_000_000
		f.Dir[1].TCPBitrate = 94_000_000
		res := Evaluate(f)
		if res.Class != model.HealthWarning {
			t.Errorf("class = %v, want WARNING (PHY-06 reduced speed)", res.Class)
		}
	})
}

func TestEvaluateDeterministic(t *testing.T) {
	build := func() *Facts {
		f := cleanFacts()
		f.PC1.CRCClassErrors = 50
		f.Dir[0].PingLossPct = 2
		f.Dir[0].UDPJitterMs = 6
		f.Partial = true
		return f
	}

	first := Evaluate(build())
	second := Evaluate(build())
	if !reflect.DeepEqual(first, second) {
		t.Errorf("Evaluate is not deterministic:\nfirst  = %+v\nsecond = %+v", first, second)
	}

	third := Evaluate(build())
	if !reflect.DeepEqual(first, third) {
		t.Errorf("Evaluate differs on a third run:\nfirst = %+v\nthird = %+v", first, third)
	}

	wantIDs := []string{"PHY-02", "TR-01", "TR-08", "LIM-03"}
	if got := findingIDs(first); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("finding order = %v, want %v (fixed Rules() order)", got, wantIDs)
	}
	if first.RulesVersion != "1.0.0" {
		t.Errorf("RulesVersion = %q, want 1.0.0", first.RulesVersion)
	}
	if first.Score == nil {
		t.Fatalf("score = nil, want a value for class %v", first.Class)
	}
	if second.Score == nil || *first.Score != *second.Score {
		t.Errorf("score differs between runs: %v vs %v", first.Score, second.Score)
	}
}
