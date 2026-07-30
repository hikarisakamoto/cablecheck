package reporting

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"cablecheck/internal/model"
)

type metricRelation string

const (
	relationBetter      metricRelation = "BETTER"
	relationWorse       metricRelation = "WORSE"
	relationSame        metricRelation = "SAME"
	relationUnavailable metricRelation = "N/A"
)

type comparisonMetric struct {
	name      string
	baseline  string
	candidate string
	relation  metricRelation
}

type measuredUint struct {
	value    uint64
	coverage string
	ok       bool
}

type measuredFloat struct {
	value float64
	ok    bool
}

// RenderComparison renders a deterministic, plain-text A/B comparison of two
// saved reports. It presents recorded measurements and verdicts only; it does
// not apply health rules or otherwise re-evaluate either report.
func RenderComparison(baseline, candidate *model.Report) []byte {
	var b strings.Builder
	addf := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}

	assessment, explanation := savedAssessment(baseline.Classification, candidate.Classification)
	addf("CableCheck comparison")
	addf("=====================")
	addf("Baseline:  %s  %s  score %s  mode %s", orUnknown(baseline.TestID),
		orUnknown(string(baseline.Classification)), ScoreOrNA(baseline.Score), orUnknown(baseline.Configuration.Mode))
	addf("Candidate: %s  %s  score %s  mode %s", orUnknown(candidate.TestID),
		orUnknown(string(candidate.Classification)), ScoreOrNA(candidate.Score), orUnknown(candidate.Configuration.Mode))
	addf("Saved verdict: %s -> %s", orUnknown(string(baseline.Classification)), orUnknown(string(candidate.Classification)))
	addf("Assessment: %s - %s", assessment, explanation)
	addf("")

	addf("Comparability")
	addf("-------------")
	warnings := comparabilityWarnings(baseline, candidate)
	if len(warnings) == 0 {
		addf("No comparability warnings; speed, mode, NICs, and test parameters match.")
	} else {
		for _, warning := range warnings {
			addf("WARNING: %s", warning)
		}
	}
	addf("")

	metrics := comparisonMetrics(baseline, candidate)
	metricWidth := 34
	for _, metric := range metrics {
		metricWidth = max(metricWidth, len(metric.name))
	}
	addf("Metrics")
	addf("-------")
	addf("%-*s %-18s %-18s %s", metricWidth, "Metric", "Baseline", "Candidate", "Change")
	addf("%-*s %-18s %-18s %s", metricWidth, strings.Repeat("-", metricWidth), strings.Repeat("-", 18), strings.Repeat("-", 18), strings.Repeat("-", 6))
	tally := map[metricRelation]int{}
	for _, metric := range metrics {
		addf("%-*s %-18s %-18s %s", metricWidth, metric.name, metric.baseline, metric.candidate, metric.relation)
		tally[metric.relation]++
	}
	addf("")
	addf("Metric tally: BETTER %d | WORSE %d | SAME %d | N/A %d",
		tally[relationBetter], tally[relationWorse], tally[relationSame], tally[relationUnavailable])
	addf("")

	renderFindingDiff(&b, baseline.Findings, candidate.Findings)
	addf("")
	addf("Conclusion")
	addf("----------")
	addf("Candidate assessment: %s", assessment)
	addf("The saved classifications are authoritative; the descriptive metric tally cannot override them.")

	return toASCII(b.String())
}

func savedAssessment(baseline, candidate model.HealthClass) (string, string) {
	if baseline == model.HealthInconclusive || candidate == model.HealthInconclusive {
		if baseline == candidate {
			return "INCONCLUSIVE", "both saved reports are inconclusive"
		}
		return "INCONCLUSIVE", "one saved report is inconclusive, so no directional comparison is valid"
	}
	baseRank, baseOK := healthRank(baseline)
	candidateRank, candidateOK := healthRank(candidate)
	if !baseOK || !candidateOK {
		return "INCONCLUSIVE", "a saved classification is unknown"
	}
	switch {
	case candidateRank < baseRank:
		return "BETTER", "the candidate saved classification improved"
	case candidateRank > baseRank:
		return "WORSE", "the candidate saved classification regressed"
	default:
		return "UNCHANGED", "both reports have the same saved classification"
	}
}

func healthRank(class model.HealthClass) (int, bool) {
	switch class {
	case model.HealthExcellent:
		return 0, true
	case model.HealthGood:
		return 1, true
	case model.HealthWarning:
		return 2, true
	case model.HealthPoor:
		return 3, true
	case model.HealthFailed:
		return 4, true
	default:
		return 0, false
	}
}

func comparabilityWarnings(baseline, candidate *model.Report) []string {
	var warnings []string
	if baseline.SchemaVersion != candidate.SchemaVersion {
		warnings = append(warnings, fmt.Sprintf("schema versions differ (%s vs %s)",
			orUnknown(baseline.SchemaVersion), orUnknown(candidate.SchemaVersion)))
	}
	if baseline.ToolVersion != candidate.ToolVersion {
		warnings = append(warnings, fmt.Sprintf("CableCheck versions differ (%s vs %s)",
			orUnknown(baseline.ToolVersion), orUnknown(candidate.ToolVersion)))
	}
	if baseline.Configuration.Mode != candidate.Configuration.Mode {
		warnings = append(warnings, fmt.Sprintf("test modes differ (%s vs %s)",
			orUnknown(baseline.Configuration.Mode), orUnknown(candidate.Configuration.Mode)))
	}

	baseSpeed := negotiatedSpeed(baseline)
	candidateSpeed := negotiatedSpeed(candidate)
	switch {
	case !baseSpeed.ok || !candidateSpeed.ok:
		warnings = append(warnings, "negotiated speed could not be verified in both reports")
	case baseSpeed.value != candidateSpeed.value:
		warnings = append(warnings, fmt.Sprintf("negotiated speeds differ (%d Mb/s vs %d Mb/s)",
			baseSpeed.value, candidateSpeed.value))
	}

	warnings = append(warnings, nicWarning("pc1", baseline.PC1.NIC, candidate.PC1.NIC)...)
	warnings = append(warnings, nicWarning("pc2", baseline.PC2.NIC, candidate.PC2.NIC)...)
	if fields := differingTestParameters(baseline.Configuration, candidate.Configuration); len(fields) > 0 {
		warnings = append(warnings, "test parameters differ: "+strings.Join(fields, ", "))
	}
	return warnings
}

func nicWarning(side string, baseline, candidate model.NICReport) []string {
	if baseline.MAC != "" && candidate.MAC != "" {
		if strings.EqualFold(baseline.MAC, candidate.MAC) {
			return nil
		}
		return []string{fmt.Sprintf("%s NICs differ (%s vs %s)", side,
			strings.ToLower(baseline.MAC), strings.ToLower(candidate.MAC))}
	}
	if baseline.Name != "" && baseline.Driver != "" && candidate.Name != "" && candidate.Driver != "" {
		baseIdentity := baseline.Name + "/" + baseline.Driver
		candidateIdentity := candidate.Name + "/" + candidate.Driver
		if baseIdentity == candidateIdentity {
			return nil
		}
		return []string{fmt.Sprintf("%s NICs differ (%s vs %s)", side, baseIdentity, candidateIdentity)}
	}
	return []string{side + " NIC identity could not be verified in both reports"}
}

func differingTestParameters(a, b model.ConfigEcho) []string {
	var fields []string
	add := func(name string, differs bool) {
		if differs {
			fields = append(fields, name)
		}
	}
	add("TCP duration", a.TCPDuration != b.TCPDuration)
	add("UDP duration", a.UDPDuration != b.UDPDuration)
	add("UDP rate", a.UDPRate != b.UDPRate)
	add("parallel streams", a.ParallelStreams != b.ParallelStreams)
	add("ping count", a.PingCount != b.PingCount)
	add("ping interval", a.PingInterval != b.PingInterval)
	add("TCP repeats", a.TCPRepeats != b.TCPRepeats)
	add("soak duration", a.SoakDuration != b.SoakDuration)
	add("soak load", a.SoakLoad != b.SoakLoad)
	add("cable test", a.CableTest != b.CableTest)
	add("cable-test TDR", a.CableTestTDR != b.CableTestTDR)
	return fields
}

func comparisonMetrics(baseline, candidate *model.Report) []comparisonMetric {
	metrics := []comparisonMetric{
		uintMetric("Negotiated speed", negotiatedSpeed(baseline), negotiatedSpeed(candidate), true, func(v uint64) string {
			return fmt.Sprintf("%d Mb/s", v)
		}),
	}

	counterGroups := []struct {
		name string
		keys []string
	}{
		{"CRC/framing errors", []string{"rx_crc", "rx_frame", "rx_align", "rx_symbol"}},
		// Kept out of the per-cause row on purpose: drivers that report an error
		// both ways would otherwise be counted twice. On Realtek NICs this is the
		// only receive-error evidence there is.
		{"Driver RX-error aggregate", []string{"rx_errors_total"}},
		{"Frame-size errors", []string{"jabber", "oversize", "undersize", "rx_length"}},
		{"FIFO overruns", []string{"rx_fifo"}},
		{"Missed RX errors", []string{"rx_missed"}},
		{"Carrier events", []string{"link_resets"}},
		{"Carrier/PHY errors", []string{"tx_carrier", "phy_errors"}},
	}
	for _, group := range counterGroups {
		for _, side := range []string{"pc1", "pc2"} {
			baseSet, candidateSet := counterSet(baseline, side), counterSet(candidate, side)
			baseValue := counterGroup(baseSet, group.keys)
			candidateValue := counterGroup(candidateSet, group.keys)
			if group.name == "Carrier events" {
				baseValue = subtractSelfInflicted(baseValue, selfInflictedCarrier(baseline, side))
				candidateValue = subtractSelfInflicted(candidateValue, selfInflictedCarrier(candidate, side))
			}
			metrics = append(metrics, uintMetric(group.name+" ("+side+")", baseValue, candidateValue, false, formatUint))
		}
	}

	for _, direction := range []string{model.DirectionPC1ToPC2, model.DirectionPC2ToPC1} {
		label := comparisonDirectionLabel(direction)
		metrics = append(metrics,
			percentMetric("Ping loss ("+label+")", worstPingLoss(baseline.Tests.Ping, direction), worstPingLoss(candidate.Tests.Ping, direction), false),
			percentMetric("Full-size loss ("+label+")", worstPingLoss(baseline.Tests.FullSizePing, direction), worstPingLoss(candidate.Tests.FullSizePing, direction), false),
		)
		baseTCP := tcpMetricsForDirection(baseline, direction)
		candidateTCP := tcpMetricsForDirection(candidate, direction)
		metrics = append(metrics,
			floatMetric("TCP throughput ("+label+")", baseTCP.throughput, candidateTCP.throughput, true, 100_000,
				func(v float64) string { return fmt.Sprintf("%.1f Mbps", v/1e6) }),
			uintMetric("TCP retransmits ("+label+")", baseTCP.retransmits, candidateTCP.retransmits, false, formatUint),
		)
	}

	for _, key := range udpMetricKeys(baseline.Tests.UDP, candidate.Tests.UDP) {
		name := fmt.Sprintf("UDP loss (%s, %s)", comparisonDirectionLabel(key.direction), FormatBps(float64(key.targetBps)))
		metrics = append(metrics, percentMetric(name,
			udpLoss(baseline.Tests.UDP, key), udpLoss(candidate.Tests.UDP, key), false))
	}
	return metrics
}

func comparisonDirectionLabel(direction string) string {
	switch direction {
	case model.DirectionPC1ToPC2:
		return "pc1 -> pc2"
	case model.DirectionPC2ToPC1:
		return "pc2 -> pc1"
	default:
		return orUnknown(direction)
	}
}

func uintMetric(name string, baseline, candidate measuredUint, higherIsBetter bool, format func(uint64) string) comparisonMetric {
	row := comparisonMetric{name: name, baseline: "n/a", candidate: "n/a", relation: relationUnavailable}
	if baseline.ok {
		row.baseline = format(baseline.value)
	}
	if candidate.ok {
		row.candidate = format(candidate.value)
	}
	if !baseline.ok || !candidate.ok || baseline.coverage != candidate.coverage {
		return row
	}
	row.relation = orderedRelation(baseline.value, candidate.value, higherIsBetter)
	return row
}

func percentMetric(name string, baseline, candidate measuredFloat, higherIsBetter bool) comparisonMetric {
	return floatMetric(name, baseline, candidate, higherIsBetter, 0.01,
		func(v float64) string { return fmt.Sprintf("%.2f%%", v) })
}

func floatMetric(name string, baseline, candidate measuredFloat, higherIsBetter bool, quantum float64, format func(float64) string) comparisonMetric {
	row := comparisonMetric{name: name, baseline: "n/a", candidate: "n/a", relation: relationUnavailable}
	if baseline.ok {
		row.baseline = format(roundTo(baseline.value, quantum))
	}
	if candidate.ok {
		row.candidate = format(roundTo(candidate.value, quantum))
	}
	if !baseline.ok || !candidate.ok {
		return row
	}
	a := roundTo(baseline.value, quantum)
	b := roundTo(candidate.value, quantum)
	switch {
	case b == a:
		row.relation = relationSame
	case (b > a) == higherIsBetter:
		row.relation = relationBetter
	default:
		row.relation = relationWorse
	}
	return row
}

func orderedRelation[T ~uint64](baseline, candidate T, higherIsBetter bool) metricRelation {
	switch {
	case candidate == baseline:
		return relationSame
	case (candidate > baseline) == higherIsBetter:
		return relationBetter
	default:
		return relationWorse
	}
}

func roundTo(value, quantum float64) float64 {
	return math.Round(value/quantum) * quantum
}

func formatUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}

func negotiatedSpeed(report *model.Report) measuredUint {
	if report.Link != nil {
		for _, settings := range []*model.LinkSettings{
			report.Link.PC1.Before, report.Link.PC2.Before,
			report.Link.PC1.After, report.Link.PC2.After,
		} {
			if settings != nil && settings.SpeedMbps > 0 {
				return measuredUint{value: uint64(settings.SpeedMbps), ok: true}
			}
		}
	}
	for _, speed := range []int{report.PC1.NIC.SpeedMbps, report.PC2.NIC.SpeedMbps} {
		if speed > 0 {
			return measuredUint{value: uint64(speed), ok: true}
		}
	}
	return measuredUint{}
}

func counterSet(report *model.Report, side string) model.CounterDeltaSet {
	if side == "pc1" {
		return report.CounterDeltas.PC1
	}
	return report.CounterDeltas.PC2
}

func counterGroup(set model.CounterDeltaSet, keys []string) measuredUint {
	var value uint64
	var coverage []string
	for _, key := range keys {
		delta, present := set[key]
		if !present {
			continue
		}
		if !delta.OK || ^uint64(0)-value < delta.Delta {
			return measuredUint{}
		}
		value += delta.Delta
		coverage = append(coverage, key)
	}
	if len(coverage) == 0 {
		return measuredUint{}
	}
	return measuredUint{value: value, coverage: strings.Join(coverage, ","), ok: true}
}

func selfInflictedCarrier(report *model.Report, side string) uint64 {
	if report.Tests.CableTest == nil {
		return 0
	}
	if side == "pc1" {
		return report.Tests.CableTest.SelfInflictedCarrierEvents.PC1
	}
	return report.Tests.CableTest.SelfInflictedCarrierEvents.PC2
}

func subtractSelfInflicted(value measuredUint, subtract uint64) measuredUint {
	if !value.ok {
		return value
	}
	if subtract >= value.value {
		value.value = 0
	} else {
		value.value -= subtract
	}
	return value
}

func worstPingLoss(results []model.PingResult, direction string) measuredFloat {
	var result measuredFloat
	for _, ping := range results {
		if ping.Direction != direction || ping.Transmitted <= 0 || ping.LossPercent < 0 {
			continue
		}
		if !result.ok || ping.LossPercent > result.value {
			result = measuredFloat{value: ping.LossPercent, ok: true}
		}
	}
	return result
}

type directionTCPMetrics struct {
	throughput  measuredFloat
	retransmits measuredUint
}

func tcpMetricsForDirection(report *model.Report, direction string) directionTCPMetrics {
	var bitrates []float64
	var retransmits []uint64
	observed := 0
	incomplete := false
	missingRetransmits := false
	for _, result := range report.Tests.TCP {
		if result.Direction != direction {
			continue
		}
		observed++
		if result.Incomplete {
			incomplete = true
			continue
		}
		bitrate := result.ReceiverBitsPerSecond
		if bitrate <= 0 {
			bitrate = result.SenderBitsPerSecond
		}
		if bitrate <= 0 {
			incomplete = true
			continue
		}
		bitrates = append(bitrates, bitrate)
		if result.Retransmissions == nil {
			missingRetransmits = true
		} else {
			retransmits = append(retransmits, *result.Retransmissions)
		}
	}
	expected := report.Configuration.TCPRepeats
	if report.Configuration.Mode == "soak" && expected > 0 {
		expected *= report.SoakCyclesCompleted
	}
	if incomplete || (expected > 0 && observed < expected) || len(bitrates) == 0 {
		return directionTCPMetrics{}
	}
	sort.Float64s(bitrates)
	metrics := directionTCPMetrics{
		throughput: measuredFloat{value: bitrates[(len(bitrates)-1)/2], ok: true},
	}
	// Retransmits report the worst trial, matching how ping loss is reduced here
	// and how the evaluator reads a burst. A median would hide the single-trial
	// burst that decides the verdict, leaving the table saying SAME while the two
	// runs classify differently.
	if !missingRetransmits && len(retransmits) == len(bitrates) {
		metrics.retransmits = measuredUint{value: slices.Max(retransmits), ok: true}
	}
	return metrics
}

type udpMetricKey struct {
	direction string
	targetBps uint64
}

func udpMetricKeys(a, b []model.UDPResult) []udpMetricKey {
	seen := map[udpMetricKey]bool{}
	for _, results := range [][]model.UDPResult{a, b} {
		for _, result := range results {
			if result.Direction == model.DirectionPC1ToPC2 || result.Direction == model.DirectionPC2ToPC1 {
				seen[udpMetricKey{direction: result.Direction, targetBps: result.TargetBps}] = true
			}
		}
	}
	keys := make([]udpMetricKey, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b udpMetricKey) int {
		if a.direction != b.direction {
			if a.direction == model.DirectionPC1ToPC2 {
				return -1
			}
			return 1
		}
		return compareUint64(a.targetBps, b.targetBps)
	})
	return keys
}

func compareUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func udpLoss(results []model.UDPResult, key udpMetricKey) measuredFloat {
	var loss measuredFloat
	for _, result := range results {
		if result.Direction != key.direction || result.TargetBps != key.targetBps ||
			result.TotalPackets <= 0 || result.LossPercent < 0 {
			continue
		}
		if !loss.ok || result.LossPercent > loss.value {
			loss = measuredFloat{value: result.LossPercent, ok: true}
		}
	}
	return loss
}

func renderFindingDiff(b *strings.Builder, baseline, candidate []model.Finding) {
	fmt.Fprintln(b, "Findings")
	fmt.Fprintln(b, "--------")
	baseGroups := groupFindings(baseline)
	candidateGroups := groupFindings(candidate)
	ids := make([]string, 0, len(baseGroups)+len(candidateGroups))
	seen := map[string]bool{}
	for id := range baseGroups {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range candidateGroups {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		fmt.Fprintln(b, "No findings in either report.")
		return
	}
	unchanged := 0
	for _, id := range ids {
		base := baseGroups[id]
		cand := candidateGroups[id]
		switch {
		case len(base) == 0:
			for _, finding := range cand {
				fmt.Fprintf(b, "ADDED:    [%s] %s: %s\n", finding.Severity, finding.RuleID, finding.Text)
			}
		case len(cand) == 0:
			for _, finding := range base {
				fmt.Fprintf(b, "RESOLVED: [%s] %s: %s\n", finding.Severity, finding.RuleID, finding.Text)
			}
		case findingsEqual(base, cand):
			unchanged += len(base)
		default:
			fmt.Fprintf(b, "CHANGED:  %s (%s -> %s)\n", id, findingSummary(base), findingSummary(cand))
		}
	}
	fmt.Fprintf(b, "Unchanged findings: %d\n", unchanged)
}

func groupFindings(findings []model.Finding) map[string][]model.Finding {
	groups := make(map[string][]model.Finding)
	for i, finding := range findings {
		id := finding.RuleID
		if id == "" {
			id = fmt.Sprintf("unknown-%04d", i+1)
		}
		groups[id] = append(groups[id], finding)
	}
	return groups
}

func findingsEqual(a, b []model.Finding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].RuleID != b[i].RuleID || a[i].Category != b[i].Category ||
			a[i].Severity != b[i].Severity || a[i].Text != b[i].Text ||
			a[i].HostSensitive != b[i].HostSensitive || !slices.Equal(a[i].Evidence, b[i].Evidence) {
			return false
		}
	}
	return true
}

func findingSummary(findings []model.Finding) string {
	if len(findings) == 1 {
		return findings[0].Severity.String()
	}
	return fmt.Sprintf("%d entries", len(findings))
}
