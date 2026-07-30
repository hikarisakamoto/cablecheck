package reporting

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"cablecheck/internal/model"
	"cablecheck/internal/testutil"
)

func TestTCPTrialSpread(t *testing.T) {
	tests := []struct {
		name      string
		results   []model.TCPResult
		direction string
		want      *model.TCPTrialStats
		wantCoV   float64
		wantNil   bool
	}{
		{
			name: "intermittent three trial spread",
			results: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 310e6},
			},
			direction: model.DirectionPC1ToPC2,
			want: &model.TCPTrialStats{
				CompletedTrials:      3,
				MinimumBitsPerSecond: 310e6,
				MedianBitsPerSecond:  940e6,
				MaximumBitsPerSecond: 940e6,
			},
			wantCoV: 0.4068285590388356,
		},
		{
			name: "two trials use lower median",
			results: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 900e6},
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 700e6},
			},
			direction: model.DirectionPC1ToPC2,
			want: &model.TCPTrialStats{
				CompletedTrials:      2,
				MinimumBitsPerSecond: 700e6,
				MedianBitsPerSecond:  700e6,
				MaximumBitsPerSecond: 900e6,
			},
			wantCoV: 0.125,
		},
		{
			name: "sender fallback and completed zero",
			results: []model.TCPResult{
				{Direction: model.DirectionPC2ToPC1, SenderBitsPerSecond: 800e6},
				{Direction: model.DirectionPC2ToPC1},
			},
			direction: model.DirectionPC2ToPC1,
			want: &model.TCPTrialStats{
				CompletedTrials:      2,
				MinimumBitsPerSecond: 0,
				MedianBitsPerSecond:  0,
				MaximumBitsPerSecond: 800e6,
			},
			wantCoV: 1,
		},
		{
			name: "equal zero trials have finite zero variation",
			results: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2},
				{Direction: model.DirectionPC1ToPC2},
			},
			direction: model.DirectionPC1ToPC2,
			want: &model.TCPTrialStats{
				CompletedTrials:      2,
				MinimumBitsPerSecond: 0,
				MedianBitsPerSecond:  0,
				MaximumBitsPerSecond: 0,
			},
		},
		{
			name: "large finite rates avoid variance overflow",
			results: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: math.MaxFloat64},
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: math.MaxFloat64 / 2},
			},
			direction: model.DirectionPC1ToPC2,
			want: &model.TCPTrialStats{
				CompletedTrials:      2,
				MinimumBitsPerSecond: math.MaxFloat64 / 2,
				MedianBitsPerSecond:  math.MaxFloat64 / 2,
				MaximumBitsPerSecond: math.MaxFloat64,
			},
			wantCoV: 1.0 / 3.0,
		},
		{
			name: "incomplete and unknown results excluded",
			results: []model.TCPResult{
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 900e6},
				{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 1, Incomplete: true},
				{Direction: "future_direction", ReceiverBitsPerSecond: 800e6},
			},
			direction: model.DirectionPC1ToPC2,
			wantNil:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]model.TCPResult(nil), tc.results...)
			got := tcpTrialStats(tc.results, tc.direction)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("tcpTrialStats() = %+v, want nil", got)
				}
			} else {
				if got == nil {
					t.Fatal("tcpTrialStats() = nil, want statistics")
				}
				want := *tc.want
				want.CoefficientOfVariation = got.CoefficientOfVariation
				if !reflect.DeepEqual(*got, want) {
					t.Errorf("tcpTrialStats() = %+v, want %+v", *got, want)
				}
				if math.Abs(got.CoefficientOfVariation-tc.wantCoV) > 1e-12 {
					t.Errorf("CoV = %.15g, want %.15g", got.CoefficientOfVariation, tc.wantCoV)
				}
				if math.IsNaN(got.CoefficientOfVariation) || math.IsInf(got.CoefficientOfVariation, 0) {
					t.Errorf("CoV is not finite: %v", got.CoefficientOfVariation)
				}
			}
			if !reflect.DeepEqual(tc.results, before) {
				t.Errorf("tcpTrialStats mutated input:\n before: %+v\n  after: %+v", before, tc.results)
			}
		})
	}
}

func TestTCPTrialSpreadDirectionIsolation(t *testing.T) {
	results := []model.TCPResult{
		{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 800e6},
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 900e6},
		{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 700e6},
	}
	spread := tcpTrialSpread(results)
	if spread == nil {
		t.Fatal("tcpTrialSpread() = nil, want reverse-direction statistics")
	}
	if spread.PC1ToPC2 != nil {
		t.Errorf("PC1ToPC2 = %+v, want nil for one completed trial", spread.PC1ToPC2)
	}
	if spread.PC2ToPC1 == nil || spread.PC2ToPC1.CompletedTrials != 2 {
		t.Errorf("PC2ToPC1 = %+v, want two completed trials", spread.PC2ToPC1)
	}
}

func TestRenderJSONTCPTrialSpread(t *testing.T) {
	tests := []struct {
		name        string
		results     []model.TCPResult
		wantPresent bool
	}{
		{name: "no trials"},
		{name: "one completed trial", results: []model.TCPResult{{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6}}},
		{name: "one completed and one incomplete", results: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 1, Incomplete: true},
		}},
		{name: "two completed trials", wantPresent: true, results: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 310e6},
		}},
		{name: "two completed trials plus incomplete", wantPresent: true, results: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 310e6},
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 1, Incomplete: true},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := &model.Report{Tests: model.TestsSection{
				TCP: tc.results,
				// RenderJSON must replace stale derived data rather than trust it.
				TCPTrialSpread: &model.TCPTrialSpread{PC2ToPC1: &model.TCPTrialStats{CompletedTrials: 99}},
			}}
			got, err := RenderJSON(report)
			testutil.Require(t, err, "RenderJSON")
			if bytes.Contains(got, []byte(`"completedTrials": 99`)) {
				t.Errorf("RenderJSON retained stale aggregate:\n%s", got)
			}

			var decoded struct {
				Tests struct {
					Spread *model.TCPTrialSpread `json:"tcpTrialSpread"`
				} `json:"tests"`
			}
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("Unmarshal RenderJSON output: %v", err)
			}
			if tc.wantPresent != (decoded.Tests.Spread != nil) {
				t.Errorf("tcpTrialSpread present = %t, want %t\n%s", decoded.Tests.Spread != nil, tc.wantPresent, got)
			}
			if tc.wantPresent {
				stats := decoded.Tests.Spread.PC1ToPC2
				if stats == nil || stats.CompletedTrials != 2 || stats.MinimumBitsPerSecond != 310e6 ||
					stats.MedianBitsPerSecond != 310e6 || stats.MaximumBitsPerSecond != 940e6 {
					t.Errorf("PC1ToPC2 spread = %+v, want 2 trials with 310M/310M/940M", stats)
				}
			}
			if report.Tests.TCPTrialSpread.PC2ToPC1.CompletedTrials != 99 {
				t.Errorf("RenderJSON mutated source aggregate: %+v", report.Tests.TCPTrialSpread)
			}
		})
	}
}

func TestTCPTrialStatsPermutationInvariant(t *testing.T) {
	first := []model.TCPResult{
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 310e6},
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 700e6},
	}
	second := []model.TCPResult{first[2], first[0], first[1]}
	gotFirst := tcpTrialStats(first, model.DirectionPC1ToPC2)
	gotSecond := tcpTrialStats(second, model.DirectionPC1ToPC2)
	if !reflect.DeepEqual(gotFirst, gotSecond) {
		t.Errorf("permuted trials produced different statistics:\n first: %+v\nsecond: %+v", gotFirst, gotSecond)
	}
}

func TestTCPTrialSpreadRenderedAcrossHumanReports(t *testing.T) {
	report := &model.Report{Tests: model.TestsSection{TCP: []model.TCPResult{
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 940e6},
		{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 310e6},
	}}}

	for name, output := range map[string]string{
		"markdown": string(RenderMarkdown(report)),
		"html":     string(RenderHTML(report)),
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{"Completed trials", "Lower median", "Inter-trial CoV", "310.0 Mbit/s", "940.0 Mbit/s", "40.68%"} {
				if !strings.Contains(output, want) {
					t.Errorf("%s output omits %q:\n%s", name, want, output)
				}
			}
			if !strings.Contains(output, "Interval CoV") {
				t.Errorf("%s output does not distinguish per-run interval CoV", name)
			}
		})
	}
}
