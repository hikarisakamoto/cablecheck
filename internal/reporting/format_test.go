package reporting

import (
	"math"
	"slices"
	"testing"

	"cablecheck/internal/model"
)

func TestFormatMbps(t *testing.T) {
	tests := []struct {
		name string
		bps  float64
		want string
	}{
		{name: "zero", bps: 0, want: "0.0 Mbps"},
		{name: "sub-megabit", bps: 500_000, want: "0.5 Mbps"},
		{name: "example", bps: 950_000_000, want: "950.0 Mbps"},
		{name: "rounding", bps: 1_256_000, want: "1.3 Mbps"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FormatMbps(test.bps); got != test.want {
				t.Errorf("FormatMbps(%v) = %q, want %q", test.bps, got, test.want)
			}
		})
	}
}

func TestFormatBps(t *testing.T) {
	tests := []struct {
		bps  float64
		want string
	}{
		{bps: -1, want: "0 bit/s"},
		{bps: 0, want: "0 bit/s"},
		{bps: 999, want: "999 bit/s"},
		{bps: 1_000, want: "1.0 Kbit/s"},
		{bps: 999_999, want: "1000.0 Kbit/s"},
		{bps: 1_000_000, want: "1.0 Mbit/s"},
		{bps: 999_999_999, want: "1000.0 Mbit/s"},
		{bps: 1_000_000_000, want: "1.00 Gbit/s"},
		{bps: 1_234_000_000, want: "1.23 Gbit/s"},
	}
	for _, test := range tests {
		if got := FormatBps(test.bps); got != test.want {
			t.Errorf("FormatBps(%v) = %q, want %q", test.bps, got, test.want)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	tests := []struct {
		value float64
		want  string
	}{
		{value: 0, want: "0.00%"},
		{value: 1.256, want: "1.26%"},
		{value: -0.5, want: "-0.50%"},
	}
	for _, test := range tests {
		if got := FormatPercent(test.value); got != test.want {
			t.Errorf("FormatPercent(%v) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestFormatSpeedMbps(t *testing.T) {
	for _, test := range []struct {
		value int
		want  string
	}{
		{value: -1, want: "unknown"},
		{value: 0, want: "unknown"},
		{value: 1000, want: "1000 Mb/s"},
	} {
		if got := FormatSpeedMbps(test.value); got != test.want {
			t.Errorf("FormatSpeedMbps(%d) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestHealthFormatting(t *testing.T) {
	tests := []struct {
		class   model.HealthClass
		slug    string
		label   string
		verdict string
	}{
		{model.HealthExcellent, "excellent", "Excellent", "The cable passed every test with no anomalies."},
		{model.HealthGood, "good", "Good", "The cable is healthy; only minor deviations were observed."},
		{model.HealthWarning, "warning", "Warning", "The cable works but shows signs that deserve attention."},
		{model.HealthPoor, "poor", "Poor", "The cable exhibits significant problems and should be replaced."},
		{model.HealthFailed, "failed", "Failed", "The cable failed critical tests and must be replaced."},
		{model.HealthInconclusive, "inconclusive", "Inconclusive", "The collected evidence does not support a verdict about the cable."},
	}
	for _, test := range tests {
		t.Run(string(test.class), func(t *testing.T) {
			if got := HealthSlug(test.class); got != test.slug {
				t.Errorf("HealthSlug(%q) = %q, want %q", test.class, got, test.slug)
			}
			if got := HealthLabel(test.class); got != test.label {
				t.Errorf("HealthLabel(%q) = %q, want %q", test.class, got, test.label)
			}
			if got := VerdictText(test.class); got != test.verdict {
				t.Errorf("VerdictText(%q) = %q, want %q", test.class, got, test.verdict)
			}
		})
	}

	for _, class := range []model.HealthClass{"", "BROKEN"} {
		if got := HealthSlug(class); got != "unknown" {
			t.Errorf("HealthSlug(%q) = %q, want unknown", class, got)
		}
		if got := HealthLabel(class); got != "Unknown" {
			t.Errorf("HealthLabel(%q) = %q, want Unknown", class, got)
		}
		if got := VerdictText(class); got != "" {
			t.Errorf("VerdictText(%q) = %q, want empty", class, got)
		}
	}
}

func TestSeverityFormatting(t *testing.T) {
	tests := []struct {
		severity model.Severity
		slug     string
		label    string
	}{
		{model.SevInfo, "info", "Info"},
		{model.SevWarning, "warning", "Warning"},
		{model.SevPoor, "poor", "Poor"},
		{model.SevFailed, "failed", "Failed"},
		{model.SevMarker, "marker", "Marker"},
		{model.Severity(-1), "unknown", "Unknown"},
		{model.Severity(99), "unknown", "Unknown"},
	}
	for _, test := range tests {
		if got := SeveritySlug(test.severity); got != test.slug {
			t.Errorf("SeveritySlug(%d) = %q, want %q", test.severity, got, test.slug)
		}
		if got := SeverityLabel(test.severity); got != test.label {
			t.Errorf("SeverityLabel(%d) = %q, want %q", test.severity, got, test.label)
		}
	}
}

func TestScoreOrNA(t *testing.T) {
	if got := ScoreOrNA(nil); got != "N/A" {
		t.Errorf("ScoreOrNA(nil) = %q, want N/A", got)
	}
	for _, score := range []int{0, 92, 100} {
		want := map[int]string{0: "0/100", 92: "92/100", 100: "100/100"}[score]
		if got := ScoreOrNA(&score); got != want {
			t.Errorf("ScoreOrNA(%d) = %q, want %q", score, got, want)
		}
	}
}

func TestBarPct(t *testing.T) {
	tests := []struct {
		name       string
		value, max float64
		want       float64
	}{
		{name: "negative value", value: -1, max: 10, want: 0},
		{name: "zero", value: 0, max: 10, want: 0},
		{name: "half", value: 5, max: 10, want: 50},
		{name: "full", value: 10, max: 10, want: 100},
		{name: "over", value: 20, max: 10, want: 100},
		{name: "zero max", value: 1, max: 0, want: 0},
		{name: "negative max", value: 1, max: -1, want: 0},
		{name: "nan value", value: math.NaN(), max: 1, want: 0},
		{name: "nan max", value: 1, max: math.NaN(), want: 0},
		{name: "positive infinity", value: math.Inf(1), max: 1, want: 100},
		{name: "negative infinity", value: math.Inf(-1), max: 1, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := barPct(test.value, test.max); got != test.want {
				t.Errorf("barPct(%v, %v) = %v, want %v", test.value, test.max, got, test.want)
			}
		})
	}
}

func TestSortedIntKeys(t *testing.T) {
	values := map[int]float64{99: 0.31, 50: 0.21, 95: 0.26, 90: 0.24}
	if got, want := sortedIntKeys(values), []int{50, 90, 95, 99}; !slices.Equal(got, want) {
		t.Errorf("sortedIntKeys() = %v, want %v", got, want)
	}
	if got := sortedIntKeys(map[int]float64(nil)); len(got) != 0 {
		t.Errorf("sortedIntKeys(nil) = %v, want empty", got)
	}
	if len(values) != 4 {
		t.Errorf("sortedIntKeys mutated input: len = %d, want 4", len(values))
	}
}

func TestToASCII(t *testing.T) {
	if got, want := string(toASCII("ASCII — en–arrow→ snowman☃")), "ASCII - en-arrow-> snowman?"; got != want {
		t.Errorf("toASCII() = %q, want %q", got, want)
	}
}
