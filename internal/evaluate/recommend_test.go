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
			want:    "Cable test reports an open/short pair — replace or re-terminate the cable (the fault distance is listed in the findings).",
		},
		{
			name:    "retest throughput",
			ruleIDs: []string{"TR-06", "TR-07"},
			want:    "Retest with `--parallel-streams 1`; correlate with counter deltas and CPU before blaming the cable.",
		},
		{
			name:    "host limited",
			ruleIDs: []string{"HOST-01", "HOST-03", "HOST-04"},
			want:    "Result appears host-limited: close background load, disable CPU power saving, avoid USB adapters, rerun.",
		},
		{
			name:    "virtual interface",
			ruleIDs: []string{"HOST-02"},
			want:    "Rerun on the physical interface — a virtual interface cannot exercise the cable.",
		},
		{
			name:    "missing tools",
			ruleIDs: []string{"LIM-01"},
			want:    "Install the missing tools (iperf3/ethtool) and rerun for a conclusive result.",
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
