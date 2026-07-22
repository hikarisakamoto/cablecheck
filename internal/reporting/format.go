package reporting

import (
	"fmt"
	"math"
	"slices"
	"time"

	"cablecheck/internal/model"
)

// verdicts maps each health class to its one-line verdict.
var verdicts = map[model.HealthClass]string{
	model.HealthExcellent:    "The cable passed every test with no anomalies.",
	model.HealthGood:         "The cable is healthy; only minor deviations were observed.",
	model.HealthWarning:      "The cable works but shows signs that deserve attention.",
	model.HealthPoor:         "The cable exhibits significant problems and should be replaced.",
	model.HealthFailed:       "The cable failed critical tests and must be replaced.",
	model.HealthInconclusive: "The collected evidence does not support a verdict about the cable.",
}

// dirLabels maps direction identifiers to display labels.
var dirLabels = map[string]string{
	model.DirectionPC1ToPC2: "pc1 → pc2",
	model.DirectionPC2ToPC1: "pc2 → pc1",
}

// FormatMbps renders a bits-per-second value in Mbps with one decimal place.
func FormatMbps(bps float64) string {
	return fmt.Sprintf("%.1f Mbps", bps/1e6)
}

// FormatBps renders a bits-per-second value with a decimal unit.
func FormatBps(bps float64) string {
	return fmtBps(bps)
}

// FormatPercent renders a percentage with two decimals.
func FormatPercent(value float64) string {
	return fmtPct(value)
}

// HealthSlug returns a CSS-safe health-class token.
func HealthSlug(class model.HealthClass) string {
	switch class {
	case model.HealthExcellent:
		return "excellent"
	case model.HealthGood:
		return "good"
	case model.HealthWarning:
		return "warning"
	case model.HealthPoor:
		return "poor"
	case model.HealthFailed:
		return "failed"
	case model.HealthInconclusive:
		return "inconclusive"
	default:
		return "unknown"
	}
}

// HealthLabel returns a human-readable health-class label.
func HealthLabel(class model.HealthClass) string {
	switch class {
	case model.HealthExcellent:
		return "Excellent"
	case model.HealthGood:
		return "Good"
	case model.HealthWarning:
		return "Warning"
	case model.HealthPoor:
		return "Poor"
	case model.HealthFailed:
		return "Failed"
	case model.HealthInconclusive:
		return "Inconclusive"
	default:
		return "Unknown"
	}
}

// VerdictText returns the one-line verdict for a health class.
func VerdictText(class model.HealthClass) string {
	return verdicts[class]
}

// SeveritySlug returns a CSS-safe severity token.
func SeveritySlug(severity model.Severity) string {
	switch severity {
	case model.SevInfo:
		return "info"
	case model.SevWarning:
		return "warning"
	case model.SevPoor:
		return "poor"
	case model.SevFailed:
		return "failed"
	case model.SevMarker:
		return "marker"
	default:
		return "unknown"
	}
}

// SeverityLabel returns a human-readable severity label.
func SeverityLabel(severity model.Severity) string {
	switch severity {
	case model.SevInfo:
		return "Info"
	case model.SevWarning:
		return "Warning"
	case model.SevPoor:
		return "Poor"
	case model.SevFailed:
		return "Failed"
	case model.SevMarker:
		return "Marker"
	default:
		return "Unknown"
	}
}

// ScoreOrNA renders a score out of 100, or N/A when absent.
func ScoreOrNA(score *int) string {
	if score == nil {
		return "N/A"
	}
	return fmt.Sprintf("%d/100", *score)
}

// barPct scales value against max and clamps the result to [0, 100].
func barPct(value, max float64) float64 {
	if max <= 0 || math.IsNaN(max) {
		return 0
	}
	pct := value / max * 100
	switch {
	case math.IsNaN(pct), pct <= 0:
		return 0
	case pct >= 100:
		return 100
	default:
		return pct
	}
}

// sortedIntKeys returns a map's integer keys in ascending order.
func sortedIntKeys(values map[int]float64) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// fmtBps renders a bits-per-second value with a binary-free decimal unit.
func fmtBps(bps float64) string {
	switch {
	case bps <= 0:
		return "0 bit/s"
	case bps >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.1f Kbit/s", bps/1e3)
	default:
		return fmt.Sprintf("%.0f bit/s", bps)
	}
}

// fmtPct renders a percentage with two decimals.
func fmtPct(v float64) string { return fmt.Sprintf("%.2f%%", v) }

// fmtTime renders a timestamp as RFC 3339, or "unknown" for the zero value.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

// fmtSpeedMbps renders a NIC speed, "unknown" for non-positive values.
func fmtSpeedMbps(mbps int) string {
	if mbps <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d Mb/s", mbps)
}

// yesNo renders a boolean as "yes"/"no".
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// orUnknown substitutes "unknown" for empty strings.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// dirLabel renders a direction identifier for humans.
func dirLabel(direction string) string {
	if label, ok := dirLabels[direction]; ok {
		return label
	}
	return orUnknown(direction)
}

// standardOf returns a snapshot's normalized counters, nil-safe.
func standardOf(s *model.CounterSnapshot) map[string]uint64 {
	if s == nil {
		return nil
	}
	return s.Standard
}

// counterCell renders one counter value or "n/a" when the side lacks it.
func counterCell(s *model.CounterSnapshot, key string) string {
	if s == nil {
		return "n/a"
	}
	v, ok := s.Standard[key]
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%d", v)
}

// deltaCell renders one counter delta, annotating resets/wraps.
func deltaCell(set model.CounterDeltaSet, key string) string {
	d, ok := set[key]
	if !ok {
		return "n/a"
	}
	if !d.OK {
		return "unreliable (reset/wrap)"
	}
	return fmt.Sprintf("+%d", d.Delta)
}

// unionKeys returns the sorted union of two map key sets.
func unionKeys[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// toASCII transliterates the handful of typographic runes the rule texts use
// and replaces anything else non-ASCII with '?' — the summary is printed to
// arbitrary terminals and must stay plain ASCII.
func toASCII(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			out = append(out, byte(r))
		case r == '—' || r == '–': // em/en dash
			out = append(out, '-')
		case r == '→': // right arrow
			out = append(out, '-', '>')
		default:
			out = append(out, '?')
		}
	}
	return out
}
