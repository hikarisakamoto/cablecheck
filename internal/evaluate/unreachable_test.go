package evaluate

import (
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
	if fd := rule.Evaluate(&Facts{ThroughputUnreachable: true}); fd == nil || fd.Severity != model.SevMarker {
		t.Errorf("LIM-05 on unreachable throughput = %+v, want one SevMarker finding", fd)
	}
	if fd := rule.Evaluate(&Facts{}); fd != nil {
		t.Errorf("LIM-05 without unreachable throughput = %+v, want nil", fd)
	}
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
