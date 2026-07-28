package evaluate

import (
	"slices"
	"testing"

	"cablecheck/internal/model"
)

func TestRecommendRuleMappings(t *testing.T) {
	tests := []struct {
		name    string
		ruleIDs []string
		want    string
	}{
		{
			name:    "intermittent link",
			ruleIDs: []string{"PHY-01", "PHY-03", "PHY-04"},
			want:    recFlaky,
		},
		{
			name:    "replace cable",
			ruleIDs: []string{"PHY-02", "PHY-09", "PHY-10", "PHY-11"},
			want:    recCable,
		},
		{
			name:    "half duplex",
			ruleIDs: []string{"PHY-05"},
			want:    "Half duplex usually means autonegotiation failure: enable autoneg on both sides; replace the cable.",
		},
		{
			name:    "reduced speed",
			ruleIDs: []string{"PHY-06", "PHY-07"},
			want:    recSpeed,
		},
		{
			name:    "cable test fault",
			ruleIDs: []string{"PHY-08"},
			want:    recCableTest,
		},
		{
			name:    "retest TCP throughput",
			ruleIDs: []string{"TR-06"},
			want:    recTCPRetest,
		},
		{
			name:    "retest UDP throughput",
			ruleIDs: []string{"TR-07"},
			want:    recUDPRetest,
		},
		{
			name:    "host limited",
			ruleIDs: []string{"HOST-01", "HOST-03", "HOST-04"},
			want:    recHost,
		},
		{
			name:    "virtual interface",
			ruleIDs: []string{"HOST-02"},
			want:    "Rerun on the physical interface — a virtual interface cannot exercise the cable.",
		},
		{
			name:    "missing critical coverage",
			ruleIDs: []string{"LIM-01"},
			want:    recCoverage,
		},
		{
			name:    "data port unreachable",
			ruleIDs: []string{"LIM-05"},
			want:    "iperf3 could not connect to the peer on the data port — check the host firewall (ufw/firewalld) on the receiving side and confirm the data port is open.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, ruleID := range tc.ruleIDs {
				got := recommend(findingsWithRuleIDs(ruleID), model.HealthWarning)
				if want := []string{tc.want}; !slices.Equal(got, want) {
					t.Errorf("recommend(%s) = %q, want %q", ruleID, got, want)
				}
			}
		})
	}
}

func TestRecommendGroundsRuleSpecificActions(t *testing.T) {
	tests := []struct {
		name     string
		finding  model.Finding
		action   string
		evidence string
	}{
		{
			name:     "reduced speed above gigabit",
			finding:  model.Finding{RuleID: "PHY-06", Evidence: []string{"negotiated 1G < expected 2.5G"}},
			action:   recSpeed,
			evidence: "negotiated 1G < expected 2.5G",
		},
		{
			name:     "TCP retransmissions",
			finding:  model.Finding{RuleID: "TR-06", Evidence: []string{"pc1->pc2: estimated retransmit rate 1.25%"}},
			action:   recTCPRetest,
			evidence: "pc1->pc2: estimated retransmit rate 1.25%",
		},
		{
			name:     "UDP loss",
			finding:  model.Finding{RuleID: "TR-07", Evidence: []string{"pc2->pc1: UDP loss 2.50%"}},
			action:   recUDPRetest,
			evidence: "pc2->pc1: UDP loss 2.50%",
		},
		{
			name:     "cable impedance without distance",
			finding:  model.Finding{RuleID: "PHY-08", Evidence: []string{"pair A: IMPEDANCE"}},
			action:   recCableTest,
			evidence: "pair A: IMPEDANCE",
		},
		{
			name:     "missing TCP coverage without tool attribution",
			finding:  model.Finding{RuleID: "LIM-01", Evidence: []string{"no TCP throughput result in either direction"}},
			action:   recCoverage,
			evidence: "no TCP throughput result in either direction",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := []string{tc.action + " Evidence from this run: " + tc.evidence + "."}
			if got := recommend([]model.Finding{tc.finding}, model.HealthWarning); !slices.Equal(got, want) {
				t.Errorf("recommend() = %q, want %q", got, want)
			}
		})
	}
}

func TestRecommendGroundsSharedActionsInFindingEvidence(t *testing.T) {
	const crcEvidence = "pc1: CRC-class error counters +42 during the test"
	findings := []model.Finding{
		{RuleID: "PHY-02", Evidence: []string{crcEvidence}},
		{RuleID: "HOST-01", Evidence: []string{"max iperf3 CPU utilization 96.0% > 90%"}},
		{RuleID: "PHY-09", Evidence: []string{"frame-size error counters +3 across both sides", crcEvidence, ""}},
		{RuleID: "TR-01", Evidence: []string{"unmapped evidence must not leak"}},
		{RuleID: "HOST-04", Evidence: []string{
			"pc1: rx_fifo +2 during the test",
			"pc2: rx_missed +3 during the test",
		}},
	}
	want := []string{
		recCable + " Evidence from this run: " + crcEvidence + "; frame-size error counters +3 across both sides.",
		recHost + " Evidence from this run: max iperf3 CPU utilization 96.0% > 90%; pc1: rx_fifo +2 during the test; pc2: rx_missed +3 during the test.",
	}

	if got := recommend(findings, model.HealthWarning); !slices.Equal(got, want) {
		t.Errorf("recommend() = %q, want %q", got, want)
	}
}

func TestRecommendPreservesOrderAndDeduplicatesText(t *testing.T) {
	findings := findingsWithRuleIDs(
		"PHY-06",
		"TR-01",
		"PHY-02",
		"PHY-07",
		"PHY-01",
		"PHY-09",
		"PHY-03",
		"HOST-02",
		"PHY-06",
	)
	want := []string{
		recSpeed,
		recCable,
		recFlaky,
		"Rerun on the physical interface — a virtual interface cannot exercise the cable.",
	}

	if got := recommend(findings, model.HealthWarning); !slices.Equal(got, want) {
		t.Errorf("recommend() = %q, want %q", got, want)
	}
}

func TestRecommendIsolationByClass(t *testing.T) {
	const halfDuplex = "Half duplex usually means autonegotiation failure: enable autoneg on both sides; replace the cable."
	tests := []struct {
		class         model.HealthClass
		wantIsolation bool
	}{
		{class: model.HealthExcellent},
		{class: model.HealthGood},
		{class: model.HealthWarning},
		{class: model.HealthPoor, wantIsolation: true},
		{class: model.HealthFailed, wantIsolation: true},
		{class: model.HealthInconclusive, wantIsolation: true},
	}

	for _, tc := range tests {
		t.Run(string(tc.class), func(t *testing.T) {
			want := []string{halfDuplex}
			if tc.wantIsolation {
				want = append(want, recIsolation)
			}
			if got := recommend(findingsWithRuleIDs("PHY-05"), tc.class); !slices.Equal(got, want) {
				t.Errorf("recommend() = %q, want %q", got, want)
			}
		})
	}
}

func TestRecommendWithoutMappedFindings(t *testing.T) {
	tests := []struct {
		name    string
		ruleIDs []string
		class   model.HealthClass
		want    []string
		wantNil bool
	}{
		{name: "no findings", class: model.HealthExcellent, wantNil: true},
		{name: "unmapped findings", ruleIDs: []string{"TR-01", "PERF-01", "LIM-02", "", "UNKNOWN"}, class: model.HealthWarning, wantNil: true},
		{name: "poor isolation only", class: model.HealthPoor, want: []string{recIsolation}},
		{name: "unknown inconclusive isolation only", ruleIDs: []string{"UNKNOWN"}, class: model.HealthInconclusive, want: []string{recIsolation}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := recommend(findingsWithRuleIDs(tc.ruleIDs...), tc.class)
			if tc.wantNil && got != nil {
				t.Fatalf("recommend() = %#v, want nil", got)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("recommend() = %q, want %q", got, tc.want)
			}
		})
	}
}

func findingsWithRuleIDs(ruleIDs ...string) []model.Finding {
	findings := make([]model.Finding, 0, len(ruleIDs))
	for _, ruleID := range ruleIDs {
		findings = append(findings, model.Finding{RuleID: ruleID})
	}
	return findings
}
