package evaluate

import (
	"slices"
	"strings"
	"testing"

	"cablecheck/internal/model"
)

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

// TestRuleLIM01NamesTheActualCounterGap pins the two counter-evidence wordings:
// an interrupted run that captured a baseline but no final snapshot must say
// so, not claim the NICs never exposed counters at all — and only an
// interrupted run may make that claim.
func TestRuleLIM01NamesTheActualCounterGap(t *testing.T) {
	rule := ruleByID(t, "LIM-01")

	t.Run("interrupted run with a baseline names the missing final snapshot", func(t *testing.T) {
		f := &Facts{Partial: true}
		f.PC1.BaselineCaptured = true
		f.PC2.BaselineCaptured = true
		fd := evaluateRule(rule, f)
		if fd == nil || fd.Severity != model.SevMarker {
			t.Fatalf("LIM-01 on an aborted run = %+v, want one SevMarker finding", fd)
		}
		joined := strings.Join(fd.Evidence, "; ")
		if !strings.Contains(joined, "final NIC counter snapshot missing") {
			t.Errorf("evidence = %q, want the missing-final-snapshot wording", joined)
		}
		if strings.Contains(joined, "NIC error counters unavailable on both sides") {
			t.Errorf("evidence = %q, must not claim counters were never available", joined)
		}
	})

	t.Run("counters never available", func(t *testing.T) {
		fd := evaluateRule(rule, &Facts{Partial: true})
		if fd == nil || fd.Severity != model.SevMarker {
			t.Fatalf("LIM-01 without any counters = %+v, want one SevMarker finding", fd)
		}
		if joined := strings.Join(fd.Evidence, "; "); !strings.Contains(joined, "NIC error counters unavailable on both sides") {
			t.Errorf("evidence = %q, want the never-available wording", joined)
		}
	})

	t.Run("one side's baseline is enough to name the interruption", func(t *testing.T) {
		f := &Facts{Partial: true}
		f.PC1.BaselineCaptured = true
		fd := evaluateRule(rule, f)
		if fd == nil {
			t.Fatal("LIM-01 = nil, want a finding")
		}
		if joined := strings.Join(fd.Evidence, "; "); !strings.Contains(joined, "final NIC counter snapshot missing") {
			t.Errorf("evidence = %q, want the missing-final-snapshot wording", joined)
		}
	})

	t.Run("a completed run never claims interruption", func(t *testing.T) {
		f := &Facts{}
		f.PC1.BaselineCaptured = true
		f.PC2.BaselineCaptured = true
		fd := evaluateRule(rule, f)
		if fd == nil {
			t.Fatal("LIM-01 = nil, want a finding")
		}
		joined := strings.Join(fd.Evidence, "; ")
		if strings.Contains(joined, "interrupted") {
			t.Errorf("evidence = %q, must not claim interruption on a completed run", joined)
		}
		if !strings.Contains(joined, "NIC error counters unavailable on both sides") {
			t.Errorf("evidence = %q, want the never-available wording", joined)
		}
	})
}

// TestRuleLIM02NamesInterruptedSide pins the one-sided variant of the same
// truthfulness fix: after an aborted run the peer side often holds only a
// baseline (the local side salvaged its final snapshot), and LIM-02 must name
// the interruption instead of claiming that NIC exposes no receive-error
// counter.
func TestRuleLIM02NamesInterruptedSide(t *testing.T) {
	rule := ruleByID(t, "LIM-02")

	t.Run("interrupted baseline-only side", func(t *testing.T) {
		f := &Facts{Partial: true}
		f.PC1.RXErrorEvidence = true
		f.PC2.BaselineCaptured = true
		fd := evaluateRule(rule, f)
		if fd == nil {
			t.Fatal("LIM-02 = nil, want a finding")
		}
		joined := strings.Join(fd.Evidence, "; ")
		if !strings.Contains(joined, "pc2") || !strings.Contains(joined, "final counter snapshot is missing") {
			t.Errorf("evidence = %q, want pc2's missing-final-snapshot wording", joined)
		}
		if strings.Contains(joined, "exposes no receive-error counter") {
			t.Errorf("evidence = %q, must not claim the NIC exposes no counter", joined)
		}
	})

	t.Run("genuinely counterless side keeps the exposure wording", func(t *testing.T) {
		f := &Facts{}
		f.PC1.RXErrorEvidence = true
		fd := evaluateRule(rule, f)
		if fd == nil {
			t.Fatal("LIM-02 = nil, want a finding")
		}
		if joined := strings.Join(fd.Evidence, "; "); !strings.Contains(joined, "pc2 exposes no receive-error counter") {
			t.Errorf("evidence = %q, want the exposure wording", joined)
		}
	})
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
		if !slices.ContainsFunc(res.Recommendations, func(r string) bool { return strings.Contains(r, "data port") }) {
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
