package reporting

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"strings"

	"cablecheck/internal/model"
)

//go:embed templates/report.html.tmpl
var reportHTMLSource string

const htmlFallback = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>CableCheck Report</title></head><body><main><h1>CableCheck Report</h1><p>The report could not be rendered.</p></main></body></html>
`

var reportHTMLTemplate = template.Must(template.New("report.html").Funcs(template.FuncMap{
	"add":                func(a, b int) int { return a + b },
	"chartPct":           chartPct,
	"directionLabel":     dirLabel,
	"formatBps":          FormatBps,
	"formatPercent":      FormatPercent,
	"formatRatioPercent": func(value float64) string { return FormatPercent(value * 100) },
	"formatSpeedMbps":    FormatSpeedMbps,
	"formatUintBps":      func(value uint64) string { return FormatBps(float64(value)) },
	"formatTime":         fmtTime,
	"healthLabel":        HealthLabel,
	"healthSlug":         HealthSlug,
	"join":               strings.Join,
	"orUnknown":          orUnknown,
	"scoreOrNA":          ScoreOrNA,
	"severityLabel":      SeverityLabel,
	"severitySlug":       SeveritySlug,
	"skipReason":         skipReason,
	"verdictText":        VerdictText,
	"yesNo":              yesNo,
}).Parse(reportHTMLSource))

// chartBar is one deterministic, numeric SVG bar. Slug is selected from a
// fixed direction switch and is never copied from report data.
type chartBar struct {
	Label string
	Value float64
	Max   float64
	Slug  string
}

type htmlLinkRow struct {
	Side     string
	Phase    string
	Settings model.LinkSettings
}

type htmlCounterRow struct {
	Name string
	PC1  string
	PC2  string
}

type htmlPercentileRow struct {
	Rank  int
	Value float64
}

type htmlPingRow struct {
	Result      model.PingResult
	Percentiles []htmlPercentileRow
}

type htmlPingSection struct {
	Rows []htmlPingRow
	Bars []chartBar
}

type htmlTCPRow struct {
	Run     int
	Result  model.TCPResult
	Retrans string
}

type htmlTCPSection struct {
	Rows []htmlTCPRow
	Bars []chartBar
}

type htmlBidirRow struct {
	Direction string
	Sender    float64
	Receiver  float64
	Retrans   string
}

type htmlCPURow struct {
	Test string
	CPU  model.CPUUsage
}

type htmlToolRow struct {
	Tool string
	PC1  string
	PC2  string
}

type htmlView struct {
	Report *model.Report

	LinkRows         []htmlLinkRow
	LinkEvents       []model.MonitoringEvent
	CountersCaptured bool
	BaselineRows     []htmlCounterRow
	DeltaRows        []htmlCounterRow
	Ping             htmlPingSection
	FullSizePing     htmlPingSection
	TCPPC1ToPC2      htmlTCPSection
	TCPPC2ToPC1      htmlTCPSection
	BidirRows        []htmlBidirRow
	BidirBars        []chartBar
	CPURows          []htmlCPURow
	ToolRows         []htmlToolRow
	ShowSoak         bool
}

// RenderHTML renders the complete self-contained HTML report. Template parse
// errors are programmer errors caught at initialization; an unexpected
// execution error returns a small valid document rather than panicking or
// exposing report-derived data through an error string.
func RenderHTML(r *model.Report) []byte {
	if r == nil {
		r = &model.Report{}
	}
	return executeHTML(reportHTMLTemplate, newHTMLView(r))
}

func executeHTML(t *template.Template, view htmlView) []byte {
	var out bytes.Buffer
	if err := t.Execute(&out, view); err != nil {
		return []byte(htmlFallback)
	}
	return out.Bytes()
}

func newHTMLView(r *model.Report) htmlView {
	v := htmlView{
		Report:           r,
		CountersCaptured: r.InitialCounters.PC1 != nil || r.InitialCounters.PC2 != nil,
		ShowSoak:         r.Configuration.SoakDuration != 0,
	}

	if r.Link != nil {
		addLink := func(side, phase string, settings *model.LinkSettings) {
			if settings != nil {
				v.LinkRows = append(v.LinkRows, htmlLinkRow{Side: side, Phase: phase, Settings: *settings})
			}
		}
		addLink("pc1", "before", r.Link.PC1.Before)
		addLink("pc1", "after", r.Link.PC1.After)
		addLink("pc2", "before", r.Link.PC2.Before)
		addLink("pc2", "after", r.Link.PC2.After)
	}

	for _, event := range r.MonitoringEvents {
		if linkEventTypes[event.Type] {
			v.LinkEvents = append(v.LinkEvents, event)
		}
	}

	for _, key := range unionKeys(standardOf(r.InitialCounters.PC1), standardOf(r.InitialCounters.PC2)) {
		v.BaselineRows = append(v.BaselineRows, htmlCounterRow{
			Name: key,
			PC1:  counterCell(r.InitialCounters.PC1, key),
			PC2:  counterCell(r.InitialCounters.PC2, key),
		})
	}
	for _, key := range unionKeys(r.CounterDeltas.PC1, r.CounterDeltas.PC2) {
		v.DeltaRows = append(v.DeltaRows, htmlCounterRow{
			Name: key,
			PC1:  deltaCell(r.CounterDeltas.PC1, key),
			PC2:  deltaCell(r.CounterDeltas.PC2, key),
		})
	}

	v.Ping = makeHTMLPingSection(r.Tests.Ping)
	v.FullSizePing = makeHTMLPingSection(r.Tests.FullSizePing)
	linkBPS := negotiatedLinkBPS(r)
	v.TCPPC1ToPC2 = makeHTMLTCPSection(r.Tests.TCP, model.DirectionPC1ToPC2, linkBPS)
	v.TCPPC2ToPC1 = makeHTMLTCPSection(r.Tests.TCP, model.DirectionPC2ToPC1, linkBPS)

	if bidir := r.Tests.Bidirectional; bidir != nil {
		v.BidirRows = []htmlBidirRow{
			{Direction: dirLabel(model.DirectionPC1ToPC2), Sender: bidir.PC1ToPC2.SenderBitsPerSecond, Receiver: bidir.PC1ToPC2.ReceiverBitsPerSecond, Retrans: optionalUint(bidir.PC1ToPC2.Retransmissions)},
			{Direction: dirLabel(model.DirectionPC2ToPC1), Sender: bidir.PC2ToPC1.SenderBitsPerSecond, Receiver: bidir.PC2ToPC1.ReceiverBitsPerSecond, Retrans: optionalUint(bidir.PC2ToPC1.Retransmissions)},
		}
		v.BidirBars = bidirectionalThroughputBars(bidir, linkBPS)
	}

	for _, result := range r.Tests.TCP {
		v.CPURows = append(v.CPURows, htmlCPURow{Test: "TCP " + dirLabel(result.Direction), CPU: result.CPUUtilization})
	}
	if bidir := r.Tests.Bidirectional; bidir != nil {
		v.CPURows = append(v.CPURows, htmlCPURow{Test: "Bidirectional", CPU: bidir.CPUUtilization})
	}
	for _, result := range r.Tests.UDP {
		v.CPURows = append(v.CPURows, htmlCPURow{Test: "UDP " + dirLabel(result.Direction), CPU: result.CPU})
	}

	for _, tool := range unionKeys(r.PC1.ToolVersions, r.PC2.ToolVersions) {
		v.ToolRows = append(v.ToolRows, htmlToolRow{
			Tool: tool,
			PC1:  orUnknown(r.PC1.ToolVersions[tool]),
			PC2:  orUnknown(r.PC2.ToolVersions[tool]),
		})
	}
	return v
}

func makeHTMLPingSection(results []model.PingResult) htmlPingSection {
	section := htmlPingSection{Bars: latencyBars(results)}
	for _, result := range results {
		row := htmlPingRow{Result: result}
		for _, rank := range sortedIntKeys(result.Percentiles) {
			row.Percentiles = append(row.Percentiles, htmlPercentileRow{Rank: rank, Value: result.Percentiles[rank]})
		}
		section.Rows = append(section.Rows, row)
	}
	return section
}

func makeHTMLTCPSection(results []model.TCPResult, direction string, max float64) htmlTCPSection {
	var selected []model.TCPResult
	for _, result := range results {
		if result.Direction != direction {
			continue
		}
		selected = append(selected, result)
	}
	section := htmlTCPSection{Bars: throughputBars(selected, max)}
	for i, result := range selected {
		section.Rows = append(section.Rows, htmlTCPRow{Run: i + 1, Result: result, Retrans: optionalUint(result.Retransmissions)})
	}
	return section
}

// throughputBars charts delivered (receiver-observed) TCP throughput only.
// Incomplete placeholders are retained in metric tables but excluded here.
func throughputBars(results []model.TCPResult, max float64) []chartBar {
	if max <= 0 || !isFinite(max) {
		for _, result := range results {
			if !result.Incomplete && isFinite(result.ReceiverBitsPerSecond) && result.ReceiverBitsPerSecond > max {
				max = result.ReceiverBitsPerSecond
			}
		}
	}
	var bars []chartBar
	for i, result := range results {
		if result.Incomplete {
			continue
		}
		bars = append(bars, chartBar{
			Label: fmt.Sprintf("Run %d", i+1),
			Value: finiteNonNegative(result.ReceiverBitsPerSecond),
			Max:   max,
			Slug:  directionSlug(result.Direction),
		})
	}
	return bars
}

// latencyBars charts average RTT by direction, preserving result order.
func latencyBars(results []model.PingResult) []chartBar {
	var max float64
	for _, result := range results {
		value := finiteNonNegative(result.RTTAvgMs)
		if value > max {
			max = value
		}
	}
	bars := make([]chartBar, 0, len(results))
	for _, result := range results {
		bars = append(bars, chartBar{
			Label: dirLabel(result.Direction),
			Value: finiteNonNegative(result.RTTAvgMs),
			Max:   max,
			Slug:  directionSlug(result.Direction),
		})
	}
	return bars
}

func bidirectionalThroughputBars(result *model.BidirResult, max float64) []chartBar {
	values := []struct {
		direction string
		value     float64
	}{
		{model.DirectionPC1ToPC2, result.PC1ToPC2.ReceiverBitsPerSecond},
		{model.DirectionPC2ToPC1, result.PC2ToPC1.ReceiverBitsPerSecond},
	}
	if max <= 0 || !isFinite(max) {
		for _, value := range values {
			if isFinite(value.value) && value.value > max {
				max = value.value
			}
		}
	}
	bars := make([]chartBar, 0, len(values))
	for _, value := range values {
		bars = append(bars, chartBar{
			Label: dirLabel(value.direction),
			Value: finiteNonNegative(value.value),
			Max:   max,
			Slug:  directionSlug(value.direction),
		})
	}
	return bars
}

func negotiatedLinkBPS(r *model.Report) float64 {
	var speed int
	for _, candidate := range []int{r.PC1.NIC.SpeedMbps, r.PC2.NIC.SpeedMbps} {
		if candidate > 0 && (speed == 0 || candidate < speed) {
			speed = candidate
		}
	}
	return float64(speed) * 1e6
}

func directionSlug(direction string) string {
	switch direction {
	case model.DirectionPC1ToPC2:
		return "pc1-to-pc2"
	case model.DirectionPC2ToPC1:
		return "pc2-to-pc1"
	default:
		return "unknown"
	}
}

func optionalUint(value *uint64) string {
	if value == nil {
		return "not reported"
	}
	return fmt.Sprintf("%d", *value)
}

func finiteNonNegative(value float64) float64 {
	if !isFinite(value) || value < 0 {
		return 0
	}
	return value
}

// chartPct keeps SVG numeric attributes compact and byte-stable without
// exposing binary floating-point tails such as 93.91000000000001.
func chartPct(value, max float64) float64 {
	return math.Round(barPct(value, max)*100) / 100
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
