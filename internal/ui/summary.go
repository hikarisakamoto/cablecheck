package ui

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"cablecheck/internal/model"
	"cablecheck/internal/reporting"
)

const summaryMaxItems = 3

var terminalSummaryTemplate = template.Must(template.New("terminal-summary").Parse(`CableCheck result
cable health: {{.Classification}}  Score: {{.Score}}{{if .Partial}}  (PARTIAL RUN){{end}}
{{.Verdict}}

Link: {{.Link}}
Ping loss:       {{.Ping}}
TCP throughput:  {{.TCPThroughput}}
TCP retransmits: {{.TCPRetransmits}}
UDP loss:        {{.UDP}}

{{if .Findings}}Findings (top {{len .Findings}} of {{.FindingCount}}):
{{range .Findings}}[{{.Severity}}] {{.RuleID}}: {{.Text}}
{{end}}{{else}}Findings: none
{{end}}{{if .Recommendations}}Recommendations (top {{len .Recommendations}} of {{.RecommendationCount}}):
{{range .Recommendations}}- {{.}}
{{end}}{{else}}Recommendations: none
{{end}}
Report: {{.ReportDir}}`))

type terminalSummaryView struct {
	Classification      string
	Score               string
	Partial             bool
	Verdict             string
	Link                string
	Ping                string
	TCPThroughput       string
	TCPRetransmits      string
	UDP                 string
	Findings            []terminalFinding
	FindingCount        int
	Recommendations     []string
	RecommendationCount int
	ReportDir           string
}

type terminalFinding struct {
	Severity string
	RuleID   string
	Text     string
	value    model.Severity
}

// Summary writes the default full end-of-run presentation from the in-memory
// report. A nil report is ignored so optional app hooks remain nil-safe.
func (r *Renderer) Summary(rep *model.Report, dir string) {
	if rep == nil {
		return
	}
	view := makeTerminalSummaryView(rep, dir)
	var rendered bytes.Buffer
	if err := terminalSummaryTemplate.Execute(&rendered, view); err != nil {
		return // the package-owned, startup-parsed template cannot fail in practice
	}

	severityByLine := make(map[string]model.Severity, len(view.Findings))
	for _, finding := range view.Findings {
		line := fmt.Sprintf("[%s] %s: %s", finding.Severity, finding.RuleID, finding.Text)
		severityByLine[line] = finding.value
	}
	plainLines := strings.Split(strings.TrimSuffix(rendered.String(), "\n"), "\n")
	lines := make([]boxLine, 0, len(plainLines))
	for i, line := range plainLines {
		styled := boxLine{text: line}
		if i == 0 {
			styled.sgr = "\x1b[1;36m"
		} else if strings.HasPrefix(line, "cable health:") {
			styled.health = rep.Classification
		} else if severityForLine, ok := severityByLine[line]; ok {
			styled.severity = severityForLine
			styled.hasSeverity = true
		}
		lines = append(lines, styled)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(r.w)
	r.writeBox(lines, false)
}

func makeTerminalSummaryView(rep *model.Report, dir string) terminalSummaryView {
	findings := rep.Findings[:min(summaryMaxItems, len(rep.Findings))]
	viewFindings := make([]terminalFinding, 0, len(findings))
	for _, finding := range findings {
		viewFindings = append(viewFindings, terminalFinding{
			Severity: reporting.SeverityLabel(finding.Severity),
			RuleID:   finding.RuleID,
			Text:     finding.Text,
			value:    finding.Severity,
		})
	}
	recommendations := append([]string(nil), rep.Recommendations[:min(summaryMaxItems, len(rep.Recommendations))]...)
	verdict := reporting.VerdictText(rep.Classification)
	if verdict == "" {
		verdict = "No verdict text is available for this classification."
	}
	return terminalSummaryView{
		Classification: string(rep.Classification),
		Score:          reporting.ScoreOrNA(rep.Score),
		Partial:        rep.Partial,
		Verdict:        verdict,
		Link:           terminalLinkLine(rep),
		Ping: perDirection(rep.Tests.Ping, func(value model.PingResult) (string, string) {
			return value.Direction, reporting.FormatPercent(value.LossPercent)
		}),
		TCPThroughput: perDirection(rep.Tests.TCP, func(value model.TCPResult) (string, string) {
			return value.Direction, reporting.FormatBps(value.ReceiverBitsPerSecond)
		}),
		TCPRetransmits: perDirection(rep.Tests.TCP, func(value model.TCPResult) (string, string) {
			if value.Retransmissions == nil {
				return value.Direction, "n/a"
			}
			return value.Direction, fmt.Sprintf("%d", *value.Retransmissions)
		}),
		UDP: perDirection(rep.Tests.UDP, func(value model.UDPResult) (string, string) {
			return value.Direction, reporting.FormatPercent(value.LossPercent)
		}),
		Findings:            viewFindings,
		FindingCount:        len(rep.Findings),
		Recommendations:     recommendations,
		RecommendationCount: len(rep.Recommendations),
		ReportDir:           dir,
	}
}

func terminalLinkLine(rep *model.Report) string {
	speed, duplex := "unknown", "unknown"
	if rep.Link != nil && rep.Link.PC1.Before != nil {
		speed = reporting.FormatSpeedMbps(rep.Link.PC1.Before.SpeedMbps)
		duplex = valueOrUnknown(rep.Link.PC1.Before.Duplex)
	} else if rep.PC1.NIC.SpeedMbps > 0 {
		speed = reporting.FormatSpeedMbps(rep.PC1.NIC.SpeedMbps)
		duplex = valueOrUnknown(rep.PC1.NIC.Duplex)
	}
	nics := ""
	if rep.PC1.NIC.Name != "" || rep.PC2.NIC.Name != "" {
		nics = fmt.Sprintf("  (pc1 %s, pc2 %s)", valueOrUnknown(rep.PC1.NIC.Name), valueOrUnknown(rep.PC2.NIC.Name))
	}
	return fmt.Sprintf("%s %s duplex%s", speed, duplex, nics)
}

func perDirection[T any](values []T, extract func(T) (string, string)) string {
	byDirection := make(map[string]string, 2)
	for _, value := range values {
		direction, text := extract(value)
		if _, exists := byDirection[direction]; !exists {
			byDirection[direction] = text
		}
	}
	if len(byDirection) == 0 {
		return "not run"
	}
	cell := func(direction, label string) string {
		if value, ok := byDirection[direction]; ok {
			return label + " " + value
		}
		return label + " n/a"
	}
	return cell(model.DirectionPC1ToPC2, "pc1->pc2") + "  |  " + cell(model.DirectionPC2ToPC1, "pc2->pc1")
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
