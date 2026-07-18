package evaluate

import (
	"strings"
	"testing"

	"cablecheck/internal/model"
)

// exitCodeForClass mirrors app.ExitCodeFor without importing the app package
// (app imports evaluate, so the dependency can only run one way). The golden
// scenarios pin the class → exit-code contract end to end: 0 GOOD/EXCELLENT,
// 1 WARNING, 2 POOR/FAILED, 3 INCONCLUSIVE.
func exitCodeForClass(c model.HealthClass) int {
	switch c {
	case model.HealthExcellent, model.HealthGood:
		return 0
	case model.HealthWarning:
		return 1
	case model.HealthPoor, model.HealthFailed:
		return 2
	case model.HealthInconclusive:
		return 3
	default:
		return 7
	}
}

// goldenFacts assembles the five canonical scenario Facts that mirror the
// example reports in the plan. Each is the flat evidence a real run would
// produce; the evaluator must classify each exactly.

// factsHealthyGigabit: a flawless 1 Gb/s run — link up, clean reliable
// counters, ~941 Mb/s TCP both ways, lossless UDP at target rate. → EXCELLENT.
func factsHealthyGigabit() *Facts {
	f := &Facts{
		NegotiatedSpeed: 1_000_000_000,
		ExpectedSpeed:   1_000_000_000,
		LinkUpAtEnd:     true,
	}
	f.PC1 = SideFacts{DeltaOK: true, CountersAvailable: true}
	f.PC2 = SideFacts{DeltaOK: true, CountersAvailable: true}
	for i := range f.Dir {
		f.Dir[i] = DirFacts{
			TCPAvailable:     true,
			TCPBitrate:       941_000_000,
			UDPAvailable:     true,
			UDPTargetReached: true,
		}
	}
	return f
}

// factsReducedSpeed: both NICs are 1 Gb/s-capable but the link came up at
// 100 Mb/s with otherwise clean physical evidence. PHY-06 fires WARNING. TCP
// runs at ~94 Mb/s of the 100 Mb/s link (≥90%, no PERF-01). → WARNING.
func factsReducedSpeed() *Facts {
	f := &Facts{
		NegotiatedSpeed: 100_000_000,
		ExpectedSpeed:   1_000_000_000,
		LinkUpAtEnd:     true,
	}
	f.PC1 = SideFacts{DeltaOK: true, CountersAvailable: true}
	f.PC2 = SideFacts{DeltaOK: true, CountersAvailable: true}
	for i := range f.Dir {
		f.Dir[i] = DirFacts{
			TCPAvailable:     true,
			TCPBitrate:       94_000_000,
			UDPAvailable:     true,
			UDPTargetReached: true,
		}
	}
	return f
}

// factsCRCErrors: a Realtek-style run where CRC-class counters climbed by 42
// during the test — PHY-02 in the POOR band (11-1000, no corroborating ping
// loss). Everything else is clean. → POOR.
func factsCRCErrors() *Facts {
	f := &Facts{
		NegotiatedSpeed: 1_000_000_000,
		ExpectedSpeed:   1_000_000_000,
		LinkUpAtEnd:     true,
	}
	f.PC1 = SideFacts{CRCClassErrors: 42, DeltaOK: true, CountersAvailable: true}
	f.PC2 = SideFacts{DeltaOK: true, CountersAvailable: true}
	for i := range f.Dir {
		f.Dir[i] = DirFacts{
			TCPAvailable:     true,
			TCPBitrate:       930_000_000,
			UDPAvailable:     true,
			UDPTargetReached: true,
		}
	}
	return f
}

// factsHostLimited: throughput far below the link (PERF-01 POOR, host
// sensitive), CPU pinned above 90% (HOST-01), and a spotless physical layer.
// The performance POOR is softened to INCONCLUSIVE because the cable was
// never proven bad. → INCONCLUSIVE.
func factsHostLimited() *Facts {
	f := &Facts{
		NegotiatedSpeed: 1_000_000_000,
		ExpectedSpeed:   1_000_000_000,
		LinkUpAtEnd:     true,
		MaxCPUPct:       96,
	}
	f.PC1 = SideFacts{DeltaOK: true, CountersAvailable: true}
	f.PC2 = SideFacts{DeltaOK: true, CountersAvailable: true}
	for i := range f.Dir {
		f.Dir[i] = DirFacts{
			TCPAvailable:     true,
			TCPBitrate:       250_000_000, // ratio 0.25 → PERF-01 POOR
			UDPAvailable:     true,
			UDPTargetReached: true,
		}
	}
	return f
}

// factsDisconnected: the link was down when testing ended (PHY-01 FAILED) and
// it bounced repeatedly (PHY-03 ≥3 → FAILED). No traffic completed. → FAILED.
func factsDisconnected() *Facts {
	f := &Facts{
		NegotiatedSpeed: 1_000_000_000,
		ExpectedSpeed:   1_000_000_000,
		LinkUpAtEnd:     false,
	}
	f.PC1 = SideFacts{CarrierEvents: 4, DeltaOK: true, CountersAvailable: true}
	f.PC2 = SideFacts{DeltaOK: true, CountersAvailable: true}
	return f
}

// TestClassifyGoldenScenarios pins the five canonical example classifications
// end to end: the fold's verdict, the class → exit-code mapping, and that
// every scenario carries at least one evidence-bearing finding of the rule the
// plan names for it.
func TestClassifyGoldenScenarios(t *testing.T) {
	cases := []struct {
		name     string
		facts    *Facts
		wantCls  model.HealthClass
		wantExit int
		wantRule string // a rule that must fire with non-empty evidence
	}{
		{"healthy-gigabit", factsHealthyGigabit(), model.HealthExcellent, 0, ""},
		{"reduced-speed-100M-on-1G", factsReducedSpeed(), model.HealthWarning, 1, "PHY-06"},
		{"crc-errors", factsCRCErrors(), model.HealthPoor, 2, "PHY-02"},
		{"host-limited", factsHostLimited(), model.HealthInconclusive, 3, "PERF-01"},
		{"disconnected", factsDisconnected(), model.HealthFailed, 2, "PHY-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Evaluate(tc.facts)
			if res.Class != tc.wantCls {
				t.Errorf("class = %s, want %s (findings %v)", res.Class, tc.wantCls, findingIDs(res))
			}
			if got := exitCodeForClass(res.Class); got != tc.wantExit {
				t.Errorf("exit code = %d, want %d", got, tc.wantExit)
			}
			if tc.wantRule != "" {
				// A non-healthy verdict must be defensible: its defining rule
				// fires and carries concrete evidence.
				fd := findingByID(res, tc.wantRule)
				if fd == nil {
					t.Fatalf("scenario is missing its defining rule %s (findings %v)", tc.wantRule, findingIDs(res))
				}
				if len(fd.Evidence) == 0 {
					t.Errorf("%s finding carries no evidence", tc.wantRule)
				}
				if !anyFindingHasEvidence(res) {
					t.Errorf("scenario carries no evidence-bearing finding (findings %v)", findingIDs(res))
				}
			} else {
				// The healthy scenario is EXCELLENT precisely because it has
				// no finding above Info — there is nothing to explain.
				if len(res.Findings) != 0 {
					t.Errorf("healthy scenario has findings %v, want none", findingIDs(res))
				}
			}
		})
	}
}

// anyFindingHasEvidence reports whether any finding carries evidence strings.
func anyFindingHasEvidence(res Result) bool {
	for _, fd := range res.Findings {
		if len(fd.Evidence) > 0 {
			return true
		}
	}
	return false
}

// findingByID returns a pointer to the first finding with the given rule ID,
// or nil.
func findingByID(res Result, id string) *model.Finding {
	for i := range res.Findings {
		if res.Findings[i].RuleID == id {
			return &res.Findings[i]
		}
	}
	return nil
}

// TestRecommendationsDeduped asserts the recommendation generator collapses
// duplicate advice: two rules mapping to the same cable text (PHY-02 and
// PHY-09) yield one line, order is preserved, and the isolation-test line is
// appended exactly once for a POOR/FAILED/INCONCLUSIVE verdict.
func TestRecommendationsDeduped(t *testing.T) {
	t.Run("shared text collapses to one line", func(t *testing.T) {
		f := &Facts{LinkUpAtEnd: true}
		// PHY-02 (crc-class) and PHY-09 (frame-size) both map to recCable.
		f.PC1 = SideFacts{CRCClassErrors: 20, JabberSizeErrors: 20, DeltaOK: true, CountersAvailable: true}
		res := Evaluate(f)
		count := 0
		for _, r := range res.Recommendations {
			if r == recCable {
				count++
			}
		}
		if count != 1 {
			t.Errorf("recCable appears %d times, want exactly 1 (recommendations %q)", count, res.Recommendations)
		}
	})

	t.Run("isolation line appended once for poor verdicts", func(t *testing.T) {
		f := &Facts{LinkUpAtEnd: true}
		f.PC1 = SideFacts{CRCClassErrors: 20, DeltaOK: true, CountersAvailable: true}
		res := Evaluate(f)
		if res.Class != model.HealthPoor {
			t.Fatalf("class = %s, want POOR to trigger the isolation line", res.Class)
		}
		count := 0
		for _, r := range res.Recommendations {
			if r == recIsolation {
				count++
			}
		}
		if count != 1 {
			t.Errorf("isolation line appears %d times, want exactly 1 (recommendations %q)", count, res.Recommendations)
		}
		if res.Recommendations[len(res.Recommendations)-1] != recIsolation {
			t.Errorf("isolation line is not last: %q", res.Recommendations)
		}
	})

	t.Run("no isolation line for a clean verdict", func(t *testing.T) {
		res := Evaluate(factsHealthyGigabit())
		for _, r := range res.Recommendations {
			if r == recIsolation {
				t.Errorf("isolation line present on an EXCELLENT verdict: %q", res.Recommendations)
			}
		}
	})

	t.Run("recommendations have no duplicates at all", func(t *testing.T) {
		// A rich failure with several distinct rules must still produce a
		// duplicate-free list.
		f := &Facts{LinkUpAtEnd: true, HalfDuplex: true}
		f.PC1 = SideFacts{CRCClassErrors: 50, CarrierEvents: 2, DeltaOK: true, CountersAvailable: true}
		f.Dir[0] = DirFacts{PingLossPct: 2}
		res := Evaluate(f)
		seen := map[string]bool{}
		for _, r := range res.Recommendations {
			if seen[r] {
				t.Errorf("duplicate recommendation %q in %q", r, res.Recommendations)
			}
			seen[r] = true
		}
		if !strings.Contains(strings.Join(res.Recommendations, "\n"), recIsolation) {
			t.Errorf("rich failure is missing the isolation line: %q", res.Recommendations)
		}
	})
}
