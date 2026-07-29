package reporting

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"cablecheck/internal/model"
)

func TestRenderComparisonGolden(t *testing.T) {
	baseline := cloneComparisonReport(t, goldenReport())
	candidate := cloneComparisonReport(t, goldenReport())
	baseScore, candidateScore := 88, 71
	baseline.TestID = "baseline-known-good"
	baseline.Classification = model.HealthGood
	baseline.Score = &baseScore
	candidate.TestID = "candidate-suspect"
	candidate.Classification = model.HealthWarning
	candidate.Score = &candidateScore
	candidate.SchemaVersion = "1.3.0"
	candidate.ToolVersion = "1.1.0"
	candidate.Configuration.Mode = "quick"
	candidate.Configuration.TCPDuration = baseline.Configuration.TCPDuration / 2
	candidate.PC2.NIC.MAC = "aa:bb:cc:99:88:77"
	for _, endpoint := range []*model.LinkEndpoint{&candidate.Link.PC1, &candidate.Link.PC2} {
		endpoint.Before.SpeedMbps = 100
		endpoint.After.SpeedMbps = 100
	}

	for side, set := range map[string]model.CounterDeltaSet{
		"pc1": baseline.CounterDeltas.PC1,
		"pc2": baseline.CounterDeltas.PC2,
	} {
		set["rx_crc"] = model.CounterDelta{Delta: 12, OK: true}
		set["rx_frame"] = model.CounterDelta{Delta: 3, OK: true}
		set["link_resets"] = model.CounterDelta{Delta: 2, OK: true}
		set["tx_carrier"] = model.CounterDelta{Delta: 1, OK: true}
		set["phy_errors"] = model.CounterDelta{Delta: 2, OK: true}
		if side == "pc2" {
			delete(candidate.CounterDeltas.PC2, "rx_frame")
		}
	}
	baseline.Tests.Ping[0].LossPercent = 2.25
	baseline.Tests.Ping[1].LossPercent = 1.00
	baseline.Tests.FullSizePing[0].LossPercent = 1.50
	for i := range baseline.Tests.TCP {
		baseline.Tests.TCP[i].ReceiverBitsPerSecond -= 150_000_000
		retransmits := uint64(9 + i)
		baseline.Tests.TCP[i].Retransmissions = &retransmits
	}
	baseline.Tests.UDP[0].LossPercent = 1.25
	baseline.Tests.UDP[1].LossPercent = 0.75
	candidate.Tests.UDP[1].TargetBps = 70_000_000

	unchanged := model.Finding{RuleID: "TR-01", Category: model.CategoryTransport, Severity: model.SevWarning, Text: "Packet loss was observed."}
	baseline.Findings = []model.Finding{
		{RuleID: "PHY-01", Category: model.CategoryPhysical, Severity: model.SevWarning, Text: "Physical errors increased."},
		unchanged,
		{RuleID: "PERF-01", Category: model.CategoryPerformance, Severity: model.SevWarning, Text: "Throughput was low."},
	}
	candidate.Findings = []model.Finding{
		{RuleID: "PHY-01", Category: model.CategoryPhysical, Severity: model.SevPoor, Text: "Physical errors remain significant."},
		unchanged,
		{RuleID: "HOST-01", Category: model.CategoryHost, Severity: model.SevMarker, Text: "Host load may limit throughput."},
	}

	checkGolden(t, "compare.txt", RenderComparison(baseline, candidate))
}

// TestComparisonSurfacesDriverRXErrorAggregate pins that the driver's own
// receive-error aggregate gets its own comparison row. It is the only corruption
// evidence Realtek NICs report, and folding it into the per-cause CRC row would
// double-count every error a driver reports both ways.
func TestComparisonSurfacesDriverRXErrorAggregate(t *testing.T) {
	baseline := cloneComparisonReport(t, goldenReport())
	candidate := cloneComparisonReport(t, goldenReport())
	baseline.CounterDeltas.PC1["rx_errors_total"] = model.CounterDelta{Delta: 0, OK: true}
	candidate.CounterDeltas.PC1["rx_errors_total"] = model.CounterDelta{Delta: 482, OK: true}
	baseline.CounterDeltas.PC1["rx_crc"] = model.CounterDelta{Delta: 0, OK: true}
	candidate.CounterDeltas.PC1["rx_crc"] = model.CounterDelta{Delta: 0, OK: true}

	var aggregate, crc *comparisonMetric
	metrics := comparisonMetrics(baseline, candidate)
	for i, row := range metrics {
		switch {
		case strings.Contains(row.name, "aggregate") && strings.Contains(row.name, "pc1"):
			aggregate = &metrics[i]
		case strings.HasPrefix(row.name, "CRC/framing errors (pc1)"):
			crc = &metrics[i]
		}
	}
	if aggregate == nil {
		t.Fatalf("no driver receive-error aggregate row for pc1")
	}
	if aggregate.candidate != "482" {
		t.Errorf("aggregate row candidate = %q, want %q", aggregate.candidate, "482")
	}
	if crc == nil || crc.candidate != "0" {
		t.Errorf("CRC/framing row = %+v, want the aggregate kept out of it (candidate %q)", crc, "0")
	}
}

func TestSavedAssessmentUsesOnlyClassifications(t *testing.T) {
	tests := []struct {
		name      string
		baseline  model.HealthClass
		candidate model.HealthClass
		want      string
	}{
		{"improved", model.HealthPoor, model.HealthGood, "BETTER"},
		{"regressed", model.HealthGood, model.HealthFailed, "WORSE"},
		{"same", model.HealthWarning, model.HealthWarning, "UNCHANGED"},
		{"baseline inconclusive", model.HealthInconclusive, model.HealthGood, "INCONCLUSIVE"},
		{"candidate inconclusive", model.HealthGood, model.HealthInconclusive, "INCONCLUSIVE"},
		{"both inconclusive", model.HealthInconclusive, model.HealthInconclusive, "INCONCLUSIVE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := savedAssessment(tc.baseline, tc.candidate)
			if got != tc.want {
				t.Errorf("savedAssessment(%s, %s) = %q, want %q", tc.baseline, tc.candidate, got, tc.want)
			}
		})
	}
}

func TestSavedAssessmentAllConclusiveTransitions(t *testing.T) {
	classes := []model.HealthClass{
		model.HealthExcellent,
		model.HealthGood,
		model.HealthWarning,
		model.HealthPoor,
		model.HealthFailed,
	}
	for baselineRank, baseline := range classes {
		for candidateRank, candidate := range classes {
			want := "UNCHANGED"
			switch {
			case candidateRank < baselineRank:
				want = "BETTER"
			case candidateRank > baselineRank:
				want = "WORSE"
			}
			if got, _ := savedAssessment(baseline, candidate); got != want {
				t.Errorf("savedAssessment(%s, %s) = %q, want %q", baseline, candidate, got, want)
			}
		}
	}
}

func TestRenderComparisonMetricTallyCannotOverrideVerdict(t *testing.T) {
	baseline := goldenReport()
	candidate := cloneComparisonReport(t, baseline)
	baseline.Classification = model.HealthExcellent
	candidate.Classification = model.HealthFailed
	for i := range baseline.Tests.Ping {
		baseline.Tests.Ping[i].LossPercent = 5
	}

	output := string(RenderComparison(baseline, candidate))
	if !strings.Contains(output, "Assessment: WORSE") || !strings.Contains(output, "Candidate assessment: WORSE") {
		t.Fatalf("saved classification regression was not authoritative:\n%s", output)
	}
	if !strings.Contains(output, "Metric tally: BETTER 2") {
		t.Errorf("test did not establish opposing better metric votes:\n%s", output)
	}
}

func TestComparisonUnavailableAndDisplayPrecision(t *testing.T) {
	row := percentMetric("loss", measuredFloat{value: 1.001, ok: true}, measuredFloat{value: 1.004, ok: true}, false)
	if row.relation != relationSame || row.baseline != "1.00%" || row.candidate != "1.00%" {
		t.Errorf("rounded row = %+v, want visibly equal SAME values", row)
	}

	row = uintMetric("counter",
		measuredUint{value: 0, coverage: "rx_crc", ok: true},
		measuredUint{value: 0, coverage: "rx_crc,rx_frame", ok: true}, false, formatUint)
	if row.relation != relationUnavailable {
		t.Errorf("mismatched counter coverage relation = %s, want N/A", row.relation)
	}
}

func TestComparisonTCPIncompleteIsUnavailable(t *testing.T) {
	report := goldenReport()
	report.Tests.TCP[0].Incomplete = true
	got := tcpMetricsForDirection(report, model.DirectionPC1ToPC2)
	if got.throughput.ok || got.retransmits.ok {
		t.Errorf("incomplete TCP metrics = %+v, want unavailable", got)
	}
}

func TestComparisonPlaceholderLossIsUnavailable(t *testing.T) {
	ping := []model.PingResult{{
		Direction:   model.DirectionPC1ToPC2,
		Transmitted: 0,
		LossPercent: 0,
	}}
	if got := worstPingLoss(ping, model.DirectionPC1ToPC2); got.ok {
		t.Errorf("placeholder ping loss = %+v, want unavailable", got)
	}

	key := udpMetricKey{direction: model.DirectionPC1ToPC2, targetBps: 800_000_000}
	udp := []model.UDPResult{{
		Direction:   key.direction,
		TargetBps:   key.targetBps,
		LossPercent: 0,
	}}
	if got := udpLoss(udp, key); got.ok {
		t.Errorf("placeholder UDP loss = %+v, want unavailable", got)
	}
}

func TestRenderComparisonPureAndDoesNotMutateInputs(t *testing.T) {
	baseline := goldenReport()
	candidate := cloneComparisonReport(t, baseline)
	beforeBaseline, err := json.Marshal(baseline)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	beforeCandidate, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}

	first := RenderComparison(baseline, candidate)
	second := RenderComparison(baseline, candidate)
	if !bytes.Equal(first, second) {
		t.Error("RenderComparison returned different bytes for the same reports")
	}
	afterBaseline, _ := json.Marshal(baseline)
	afterCandidate, _ := json.Marshal(candidate)
	if !bytes.Equal(beforeBaseline, afterBaseline) || !bytes.Equal(beforeCandidate, afterCandidate) {
		t.Error("RenderComparison mutated an input report")
	}
}

func cloneComparisonReport(t *testing.T, report *model.Report) *model.Report {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report clone: %v", err)
	}
	var clone model.Report
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatalf("unmarshal report clone: %v", err)
	}
	return &clone
}
