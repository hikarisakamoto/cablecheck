package evaluate

import (
	"slices"
	"strings"
	"testing"

	"cablecheck/internal/model"
)

func recsContain(res Result, sub string) bool {
	for _, r := range res.Recommendations {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

// TestRuleLIM05 pins the data-port-unreachable limitation marker.
func TestRuleLIM05(t *testing.T) {
	rule := ruleByID(t, "LIM-05")
	if fd := evaluateRule(rule, &Facts{ThroughputUnreachable: true}); fd == nil || fd.Severity != model.SevMarker {
		t.Errorf("LIM-05 on unreachable throughput = %+v, want one SevMarker finding", fd)
	}
	if fd := evaluateRule(rule, &Facts{}); fd != nil {
		t.Errorf("LIM-05 without unreachable throughput = %+v, want nil", fd)
	}
}

// TestReceiveErrorEvidenceGapIsALimitation pins the blind-side guard: a side
// whose driver exposes no receive-error counter never measured corruption, so a
// clean-looking verdict must be downgraded rather than certified.
func TestReceiveErrorEvidenceGapIsALimitation(t *testing.T) {
	t.Run("neither side measured receive errors", func(t *testing.T) {
		f := cleanFacts()
		f.PC1.RXErrorEvidence = false
		f.PC2.RXErrorEvidence = false
		res := Evaluate(f)
		if !slices.Contains(findingIDs(res), "LIM-01") {
			t.Errorf("findings = %v, want LIM-01: neither side can prove the link was clean", findingIDs(res))
		}
		if res.Class != model.HealthInconclusive {
			t.Errorf("class = %v, want INCONCLUSIVE", res.Class)
		}
	})

	t.Run("one side measured receive errors", func(t *testing.T) {
		f := cleanFacts()
		f.PC2.RXErrorEvidence = false
		res := Evaluate(f)
		if !slices.Contains(findingIDs(res), "LIM-02") {
			t.Errorf("findings = %v, want LIM-02 for the one unmeasured side", findingIDs(res))
		}
		if res.Class != model.HealthGood {
			t.Errorf("class = %v, want GOOD: half the link's receive path was never measured", res.Class)
		}
	})

	t.Run("both sides measured receive errors", func(t *testing.T) {
		res := Evaluate(cleanFacts())
		for _, id := range findingIDs(res) {
			if id == "LIM-01" || id == "LIM-02" {
				t.Errorf("findings = %v, want no evidence-gap limitation", findingIDs(res))
			}
		}
		if res.Class != model.HealthExcellent {
			t.Errorf("class = %v, want EXCELLENT", res.Class)
		}
	})

	t.Run("a real fault still wins over the evidence gap", func(t *testing.T) {
		f := cleanFacts()
		f.PC1.RXErrorEvidence = false
		f.PC2.RXErrorEvidence = false
		f.PC2.CRCClassErrors = 500
		if res := Evaluate(f); res.Class != model.HealthPoor {
			t.Errorf("class = %v, want POOR: a limitation must never hide measured corruption", res.Class)
		}
	})
}

// TestClassifyThroughputUnreachable covers the two verdicts that matter: an
// otherwise-clean run with an unreachable data port is INCONCLUSIVE with the
// firewall recommendation, but a genuine physical failure still wins.
func TestClassifyThroughputUnreachable(t *testing.T) {
	t.Run("clean layer is inconclusive with a firewall hint", func(t *testing.T) {
		f := cleanFacts()
		for i := range f.Dir {
			f.Dir[i].TCPAvailable = false
			f.Dir[i].TCPBitrate = 0
			f.Dir[i].UDPAvailable = false
		}
		f.ThroughputUnreachable = true
		res := Evaluate(f)
		if res.Class != model.HealthInconclusive {
			t.Errorf("class = %v, want INCONCLUSIVE (LIM-05)", res.Class)
		}
		if res.Score != nil {
			t.Errorf("score = %v, want nil for INCONCLUSIVE", *res.Score)
		}
		if !recsContain(res, "data port") {
			t.Errorf("recommendations = %v, want the firewall/data-port hint", res.Recommendations)
		}
	})

	t.Run("real physical failure still wins", func(t *testing.T) {
		f := cleanFacts()
		f.LinkUpAtEnd = false // physical FAILED short-circuits above the cap
		f.ThroughputUnreachable = true
		res := Evaluate(f)
		if res.Class != model.HealthFailed {
			t.Errorf("class = %v, want FAILED (LIM-05 must not mask a real cable fault)", res.Class)
		}
	})
}

// TestFactsThroughputUnreachable pins the marker-based detection: the
// unreachable skip sets the fact, a tool-missing skip does not.
func TestFactsThroughputUnreachable(t *testing.T) {
	unreachable := &model.Report{SkippedTests: []model.SkippedTest{
		{Name: "tcp", Reason: model.SkipReasonUnreachable + ": tcp throughput client could not connect: refused"},
	}}
	if !FactsFromReport(unreachable).ThroughputUnreachable {
		t.Error("ThroughputUnreachable = false for an unreachable-marked skip, want true")
	}
	toolMissing := &model.Report{SkippedTests: []model.SkippedTest{
		{Name: "tcp", Reason: "peer could not run iperf3_client_run: iperf3 not installed"},
	}}
	if FactsFromReport(toolMissing).ThroughputUnreachable {
		t.Error("ThroughputUnreachable = true for a tool-missing skip, want false")
	}
}
