package reporting

import (
	"fmt"
	"strings"

	"cablecheck/internal/model"
)

// summaryMaxItems caps the findings and recommendations shown in the summary.
const summaryMaxItems = 3

// RenderSummary renders the ~30-line plain-ASCII terminal summary: verdict,
// link line, headline metrics per direction, and the top findings and
// recommendations. Like RenderMarkdown it is a pure function of the report.
func RenderSummary(r *model.Report) []byte {
	var b strings.Builder
	addf := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}

	addf("CableCheck summary")
	addf("==================")
	addf("Test:     %s  (mode %s)", orUnknown(r.TestID), orUnknown(r.Configuration.Mode))
	addf("Started:  %s   Duration: %s", fmtTime(r.StartedAt), r.Duration.String())
	addf("Result:   %s%s%s", orUnknown(string(r.Classification)), scoreText(r.Score), partialText(r.Partial))
	addf("Link:     %s", summaryLinkLine(r))
	addf("")
	addf("Ping loss:        %s", perDirection(r.Tests.Ping, func(p model.PingResult) (string, string) {
		return p.Direction, fmt.Sprintf("%.2f%%", p.LossPercent)
	}))
	addf("TCP throughput:   %s", perDirection(r.Tests.TCP, func(t model.TCPResult) (string, string) {
		return t.Direction, fmtBps(t.ReceiverBitsPerSecond)
	}))
	addf("TCP retransmits:  %s", perDirection(r.Tests.TCP, func(t model.TCPResult) (string, string) {
		if t.Retransmissions == nil {
			return t.Direction, "n/a"
		}
		return t.Direction, fmt.Sprintf("%d", *t.Retransmissions)
	}))
	addf("UDP loss:         %s", perDirection(r.Tests.UDP, func(u model.UDPResult) (string, string) {
		return u.Direction, fmt.Sprintf("%.2f%%", u.LossPercent)
	}))
	addf("")

	if len(r.Findings) == 0 {
		addf("Findings: none")
	} else {
		addf("Findings (top %d of %d):", min(summaryMaxItems, len(r.Findings)), len(r.Findings))
		for _, fd := range r.Findings[:min(summaryMaxItems, len(r.Findings))] {
			addf("  [%s] %s: %s", fd.Severity, fd.RuleID, fd.Text)
		}
	}
	if len(r.Recommendations) == 0 {
		addf("Recommendations: none")
	} else {
		addf("Recommendations (top %d of %d):", min(summaryMaxItems, len(r.Recommendations)), len(r.Recommendations))
		for _, rec := range r.Recommendations[:min(summaryMaxItems, len(r.Recommendations))] {
			addf("  - %s", rec)
		}
	}
	addf("")
	addf("Full report: report.md and report.json in the report directory.")

	return toASCII(b.String())
}

// scoreText renders the score suffix of the result line.
func scoreText(score *int) string {
	if score == nil {
		return ""
	}
	return fmt.Sprintf("   Score: %d/100", *score)
}

// partialText marks interrupted runs on the result line.
func partialText(partial bool) string {
	if partial {
		return "   (PARTIAL RUN)"
	}
	return ""
}

// summaryLinkLine builds the one-line link description from the captured link
// state, falling back to the NIC descriptions.
func summaryLinkLine(r *model.Report) string {
	speed, duplex := "unknown", "unknown"
	if r.Link != nil && r.Link.PC1.Before != nil {
		speed = fmtSpeedMbps(r.Link.PC1.Before.SpeedMbps)
		duplex = orUnknown(r.Link.PC1.Before.Duplex)
	} else if r.PC1.NIC.SpeedMbps > 0 {
		speed = fmtSpeedMbps(r.PC1.NIC.SpeedMbps)
		duplex = orUnknown(r.PC1.NIC.Duplex)
	}
	nics := ""
	if r.PC1.NIC.Name != "" || r.PC2.NIC.Name != "" {
		nics = fmt.Sprintf("  (pc1 %s, pc2 %s)", orUnknown(r.PC1.NIC.Name), orUnknown(r.PC2.NIC.Name))
	}
	return fmt.Sprintf("%s %s duplex%s", speed, duplex, nics)
}

// perDirection renders "pc1->pc2 X | pc2->pc1 Y" for a per-direction result
// slice, or "not run" when the slice is empty.
func perDirection[T any](results []T, extract func(T) (string, string)) string {
	vals := map[string]string{}
	for _, res := range results {
		dir, text := extract(res)
		if _, exists := vals[dir]; !exists {
			vals[dir] = text
		}
	}
	if len(vals) == 0 {
		return "not run"
	}
	cell := func(dir, label string) string {
		if v, ok := vals[dir]; ok {
			return label + " " + v
		}
		return label + " n/a"
	}
	return cell(model.DirectionPC1ToPC2, "pc1->pc2") + "  |  " + cell(model.DirectionPC2ToPC1, "pc2->pc1")
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
