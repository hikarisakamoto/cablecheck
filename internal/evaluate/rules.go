package evaluate

import (
	"fmt"
	"time"

	"cablecheck/internal/model"
)

// Rule is one evaluation rule: a stable ID, the evidence category it
// inspects, and a pure evaluation function that returns nil when the rule
// passed or does not apply.
type Rule struct {
	// ID is the stable rule identifier, e.g. "PHY-02".
	ID string
	// Category is the evidence layer the rule inspects.
	Category model.Category
	// Evaluate inspects the facts and returns a finding, or nil when the
	// rule passed or is not applicable.
	Evaluate func(f *Facts) *model.Finding
}

// dirNames labels the two traffic directions for evidence text.
var dirNames = [2]string{"pc1->pc2", "pc2->pc1"}

// Rules returns the full rule list in its fixed, deterministic evaluation
// order: PHY-01..10, TR-01..09, PERF-01..04, HOST-01..03, LIM-01..04.
func Rules() []Rule {
	return []Rule{
		{ID: "PHY-01", Category: model.CategoryPhysical, Evaluate: rulePHY01},
		{ID: "PHY-02", Category: model.CategoryPhysical, Evaluate: rulePHY02},
		{ID: "PHY-03", Category: model.CategoryPhysical, Evaluate: rulePHY03},
		{ID: "PHY-04", Category: model.CategoryPhysical, Evaluate: rulePHY04},
		{ID: "PHY-05", Category: model.CategoryPhysical, Evaluate: rulePHY05},
		{ID: "PHY-06", Category: model.CategoryPhysical, Evaluate: rulePHY06},
		{ID: "PHY-07", Category: model.CategoryPhysical, Evaluate: rulePHY07},
		{ID: "PHY-08", Category: model.CategoryPhysical, Evaluate: rulePHY08},
		{ID: "PHY-09", Category: model.CategoryPhysical, Evaluate: rulePHY09},
		{ID: "PHY-10", Category: model.CategoryPhysical, Evaluate: rulePHY10},
		{ID: "TR-01", Category: model.CategoryTransport, Evaluate: ruleTR01},
		{ID: "TR-02", Category: model.CategoryTransport, Evaluate: ruleTR02},
		{ID: "TR-03", Category: model.CategoryTransport, Evaluate: ruleTR03},
		{ID: "TR-04", Category: model.CategoryTransport, Evaluate: ruleTR04},
		{ID: "TR-05", Category: model.CategoryTransport, Evaluate: ruleTR05},
		{ID: "TR-06", Category: model.CategoryTransport, Evaluate: ruleTR06},
		{ID: "TR-07", Category: model.CategoryTransport, Evaluate: ruleTR07},
		{ID: "TR-08", Category: model.CategoryTransport, Evaluate: ruleTR08},
		{ID: "TR-09", Category: model.CategoryTransport, Evaluate: ruleTR09},
		{ID: "PERF-01", Category: model.CategoryPerformance, Evaluate: rulePERF01},
		{ID: "PERF-02", Category: model.CategoryPerformance, Evaluate: rulePERF02},
		{ID: "PERF-03", Category: model.CategoryPerformance, Evaluate: rulePERF03},
		{ID: "PERF-04", Category: model.CategoryPerformance, Evaluate: rulePERF04},
		{ID: "HOST-01", Category: model.CategoryHost, Evaluate: ruleHOST01},
		{ID: "HOST-02", Category: model.CategoryHost, Evaluate: ruleHOST02},
		{ID: "HOST-03", Category: model.CategoryHost, Evaluate: ruleHOST03},
		{ID: "LIM-01", Category: model.CategoryLimitation, Evaluate: ruleLIM01},
		{ID: "LIM-02", Category: model.CategoryLimitation, Evaluate: ruleLIM02},
		{ID: "LIM-03", Category: model.CategoryLimitation, Evaluate: ruleLIM03},
		{ID: "LIM-04", Category: model.CategoryLimitation, Evaluate: ruleLIM04},
	}
}

// crcTotal sums the CRC-class error deltas of the sides whose deltas are
// reliable.
func crcTotal(f *Facts) uint64 {
	var total uint64
	if f.PC1.DeltaOK {
		total += f.PC1.CRCClassErrors
	}
	if f.PC2.DeltaOK {
		total += f.PC2.CRCClassErrors
	}
	return total
}

// jabberTotal sums the frame-size error deltas of the reliable sides.
func jabberTotal(f *Facts) uint64 {
	var total uint64
	if f.PC1.DeltaOK {
		total += f.PC1.JabberSizeErrors
	}
	if f.PC2.DeltaOK {
		total += f.PC2.JabberSizeErrors
	}
	return total
}

// carrierEvents returns the worst per-side carrier event count among the
// reliable sides (both sides observe the same physical link, so summing would
// double-count one bounce).
func carrierEvents(f *Facts) uint64 {
	var worst uint64
	if f.PC1.DeltaOK && f.PC1.CarrierEvents > worst {
		worst = f.PC1.CarrierEvents
	}
	if f.PC2.DeltaOK && f.PC2.CarrierEvents > worst {
		worst = f.PC2.CarrierEvents
	}
	return worst
}

// crcEvidence lists the per-side CRC-class deltas of the reliable sides.
func crcEvidence(f *Facts) []string {
	var ev []string
	if f.PC1.DeltaOK && f.PC1.CRCClassErrors > 0 {
		ev = append(ev, fmt.Sprintf("pc1: CRC-class error counters +%d during the test", f.PC1.CRCClassErrors))
	}
	if f.PC2.DeltaOK && f.PC2.CRCClassErrors > 0 {
		ev = append(ev, fmt.Sprintf("pc2: CRC-class error counters +%d during the test", f.PC2.CRCClassErrors))
	}
	return ev
}

// anyPingLossOver reports whether any direction lost more than pct percent of
// standard pings.
func anyPingLossOver(f *Facts, pct float64) bool {
	return f.Dir[0].PingLossPct > pct || f.Dir[1].PingLossPct > pct
}

// phy06cond is the reduced-speed condition shared by PHY-06, PHY-07 and the
// score table: both speeds known and the link negotiated below expectation.
func phy06cond(f *Facts) bool {
	return f.NegotiatedSpeed > 0 && f.ExpectedSpeed > 0 && f.NegotiatedSpeed < f.ExpectedSpeed
}

func rulePHY01(f *Facts) *model.Finding {
	if f.LinkUpAtEnd {
		return nil
	}
	return &model.Finding{
		RuleID:   "PHY-01",
		Category: model.CategoryPhysical,
		Severity: model.SevFailed,
		Text:     "The link was down when testing ended.",
		Evidence: []string{"post-test link state reports no carrier on at least one side"},
	}
}

func rulePHY02(f *Facts) *model.Finding {
	total := crcTotal(f)
	if total == 0 {
		return nil
	}
	lossy := anyPingLossOver(f, 1)
	sev := model.SevWarning
	switch {
	case total > 1000 || (total > 10 && lossy):
		sev = model.SevFailed
	case total > 10:
		sev = model.SevPoor
	}
	ev := crcEvidence(f)
	if total > 10 && lossy {
		ev = append(ev, "ping loss above 1% corroborates the counter movement")
	}
	return &model.Finding{
		RuleID:   "PHY-02",
		Category: model.CategoryPhysical,
		Severity: sev,
		Text:     fmt.Sprintf("CRC-class receive errors incremented by %d during the test.", total),
		Evidence: ev,
	}
}

func rulePHY03(f *Facts) *model.Finding {
	events := carrierEvents(f)
	if events == 0 {
		return nil
	}
	sev := model.SevPoor
	if events >= 3 {
		sev = model.SevFailed
	}
	return &model.Finding{
		RuleID:   "PHY-03",
		Category: model.CategoryPhysical,
		Severity: sev,
		Text:     fmt.Sprintf("The link bounced %d time(s) during the test.", events),
		Evidence: []string{fmt.Sprintf("carrier change counter advanced by %d on the worse side", events)},
	}
}

func rulePHY04(f *Facts) *model.Finding {
	if f.Renegotiations < 1 {
		return nil
	}
	return &model.Finding{
		RuleID:   "PHY-04",
		Category: model.CategoryPhysical,
		Severity: model.SevPoor,
		Text:     fmt.Sprintf("The link renegotiated speed or duplex %d time(s) mid-test.", f.Renegotiations),
		Evidence: []string{fmt.Sprintf("link monitor observed %d renegotiation event(s)", f.Renegotiations)},
	}
}

func rulePHY05(f *Facts) *model.Finding {
	if !f.HalfDuplex {
		return nil
	}
	return &model.Finding{
		RuleID:   "PHY-05",
		Category: model.CategoryPhysical,
		Severity: model.SevPoor,
		Text:     "The link negotiated half duplex — direct cable links must be full duplex.",
		Evidence: []string{"negotiated duplex = half on at least one side"},
	}
}

func rulePHY06(f *Facts) *model.Finding {
	if !phy06cond(f) {
		return nil
	}
	return &model.Finding{
		RuleID:   "PHY-06",
		Category: model.CategoryPhysical,
		Severity: model.SevWarning,
		Text: fmt.Sprintf("The link negotiated %s although both NICs support %s. "+
			"Possible causes: cable wiring or a damaged pair (1000BASE-T needs all four pairs), NIC or driver configuration.",
			f.NegotiatedSpeed, f.ExpectedSpeed),
		Evidence: []string{fmt.Sprintf("negotiated %s < expected %s", f.NegotiatedSpeed, f.ExpectedSpeed)},
	}
}

func rulePHY07(f *Facts) *model.Finding {
	if !phy06cond(f) || crcTotal(f) == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "PHY-07",
		Category: model.CategoryPhysical,
		Severity: model.SevPoor,
		Text: fmt.Sprintf("The link runs below its expected speed (%s < %s) and physical error counters moved — strong cable evidence.",
			f.NegotiatedSpeed, f.ExpectedSpeed),
		Evidence: append([]string{fmt.Sprintf("negotiated %s < expected %s", f.NegotiatedSpeed, f.ExpectedSpeed)}, crcEvidence(f)...),
	}
}

func rulePHY08(f *Facts) *model.Finding {
	if !f.CableTestRan {
		return nil
	}
	worst := model.Severity(-1)
	var ev []string
	for _, pair := range f.CableTestPairs {
		var sev model.Severity
		switch pair.Status {
		case model.PairOpen, model.PairShortIntra, model.PairShortInter:
			sev = model.SevFailed
		case model.PairImpedance:
			sev = model.SevPoor
		case model.PairUnspecified:
			sev = model.SevWarning
		default:
			continue
		}
		if pair.HasFault && pair.FaultMeters > 0 {
			ev = append(ev, fmt.Sprintf("pair %s: %s at ~%.1fm", pair.Pair, pair.Status, pair.FaultMeters))
		} else {
			ev = append(ev, fmt.Sprintf("pair %s: %s", pair.Pair, pair.Status))
		}
		if sev > worst {
			worst = sev
		}
	}
	if worst < model.SevWarning {
		return nil
	}
	return &model.Finding{
		RuleID:   "PHY-08",
		Category: model.CategoryPhysical,
		Severity: worst,
		Text:     "Cable diagnostics report a wiring fault.",
		Evidence: ev,
	}
}

func rulePHY09(f *Facts) *model.Finding {
	total := jabberTotal(f)
	if total == 0 {
		return nil
	}
	sev := model.SevWarning
	if total > 10 {
		sev = model.SevPoor
	}
	return &model.Finding{
		RuleID:   "PHY-09",
		Category: model.CategoryPhysical,
		Severity: sev,
		Text:     fmt.Sprintf("Frame-size error counters (jabber/oversize/undersize/length) incremented by %d.", total),
		Evidence: []string{fmt.Sprintf("frame-size error counters +%d across both sides", total)},
	}
}

func rulePHY10(f *Facts) *model.Finding {
	if crcTotal(f) == 0 {
		return nil
	}
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if d.UDPAvailable && d.UDPTargetReached && d.UDPLossPct > 2 {
			ev = append(ev, fmt.Sprintf("%s: UDP loss %.2f%% at target rate", dirNames[i], d.UDPLossPct))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "PHY-10",
		Category: model.CategoryPhysical,
		Severity: model.SevFailed,
		Text:     "UDP loss above 2% correlates with physical error counter movement.",
		Evidence: append(ev, crcEvidence(f)...),
	}
}

func ruleTR01(f *Facts) *model.Finding {
	worst := model.Severity(-1)
	var ev []string
	for i := range f.Dir {
		loss := f.Dir[i].PingLossPct
		if loss <= 0 {
			continue
		}
		sev := model.SevWarning
		if loss > 0.1 {
			sev = model.SevPoor
		}
		ev = append(ev, fmt.Sprintf("%s: %.2f%% ping loss", dirNames[i], loss))
		if sev > worst {
			worst = sev
		}
	}
	if worst < model.SevWarning {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-01",
		Category: model.CategoryTransport,
		Severity: worst,
		Text:     "Ping packets were lost on a direct cable link.",
		Evidence: ev,
	}
}

func ruleTR02(f *Facts) *model.Finding {
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if d.FullSizeAvailable && d.FullSizeLossPct > 0 && d.PingLossPct == 0 {
			ev = append(ev, fmt.Sprintf("%s: %.2f%% full-size loss with 0%% standard-size loss", dirNames[i], d.FullSizeLossPct))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-02",
		Category: model.CategoryTransport,
		Severity: model.SevPoor,
		Text:     "Full-size frames are lost while standard frames pass — frame-size-dependent corruption.",
		Evidence: ev,
	}
}

func ruleTR03(f *Facts) *model.Finding {
	total := f.Dir[0].FragErrors + f.Dir[1].FragErrors
	if total == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-03",
		Category: model.CategoryTransport,
		Severity: model.SevWarning,
		Text:     "Don't-fragment pings failed — an MTU mismatch (configuration, not cable).",
		Evidence: []string{fmt.Sprintf("%d fragmentation-needed failure(s) during full-size ping", total)},
	}
}

func ruleTR04(f *Facts) *model.Finding {
	total := f.Dir[0].PingDuplicates + f.Dir[1].PingDuplicates
	if total == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-04",
		Category: model.CategoryTransport,
		Severity: model.SevWarning,
		Text:     "Duplicate ping replies were received — duplicates on a direct cable are anomalous.",
		Evidence: []string{fmt.Sprintf("%d duplicate repl(ies) across both directions", total)},
	}
}

func ruleTR05(f *Facts) *model.Finding {
	worst := model.Severity(-1)
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if d.PingSpikes > 5 {
			ev = append(ev, fmt.Sprintf("%s: %d RTT spikes above 10x the median", dirNames[i], d.PingSpikes))
			if model.SevWarning > worst {
				worst = model.SevWarning
			}
		}
		if d.PingMaxGap > time.Second {
			ev = append(ev, fmt.Sprintf("%s: longest reply gap %s", dirNames[i], d.PingMaxGap))
			if model.SevPoor > worst {
				worst = model.SevPoor
			}
		}
	}
	if worst < model.SevWarning {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-05",
		Category: model.CategoryTransport,
		Severity: worst,
		Text:     "Round-trip times are unstable.",
		Evidence: ev,
	}
}

func ruleTR06(f *Facts) *model.Finding {
	worst := model.Severity(-1)
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if !d.TCPAvailable || d.TCPRetransRate < 0.001 {
			continue
		}
		sev := model.SevWarning
		if d.TCPRetransRate > 0.01 {
			sev = model.SevPoor
		}
		ev = append(ev, fmt.Sprintf("%s: estimated retransmit rate %.2f%% (retransmits / (bytes/MSS %d))",
			dirNames[i], d.TCPRetransRate*100, defaultMSS))
		if sev > worst {
			worst = sev
		}
	}
	if worst < model.SevWarning {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-06",
		Category: model.CategoryTransport,
		Severity: worst,
		Text:     "TCP retransmissions are elevated for a direct cable link.",
		Evidence: ev,
	}
}

func ruleTR07(f *Facts) *model.Finding {
	if f.MaxCPUPct > 90 || f.UDPNearSaturation {
		return nil // host or configuration limits speak instead (HOST-01 / LIM rules)
	}
	worst := model.Severity(-1)
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if !d.UDPAvailable || !d.UDPTargetReached || d.UDPLossPct < 0.5 {
			continue
		}
		sev := model.SevWarning
		if d.UDPLossPct > 2 {
			sev = model.SevPoor
		}
		line := fmt.Sprintf("%s: %.2f%% UDP loss at target rate", dirNames[i], d.UDPLossPct)
		if f.UDPRateAssumed {
			line += " (at assumed rate)"
		}
		ev = append(ev, line)
		if sev > worst {
			worst = sev
		}
	}
	if worst < model.SevWarning {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-07",
		Category: model.CategoryTransport,
		Severity: worst,
		Text:     "UDP datagrams were lost although the sender reached its target rate.",
		Evidence: ev,
	}
}

func ruleTR08(f *Facts) *model.Finding {
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if d.UDPAvailable && d.UDPJitterMs > 5 {
			ev = append(ev, fmt.Sprintf("%s: %.2f ms jitter", dirNames[i], d.UDPJitterMs))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-08",
		Category: model.CategoryTransport,
		Severity: model.SevWarning,
		Text:     "UDP jitter above 5 ms on a direct link.",
		Evidence: ev,
	}
}

func ruleTR09(f *Facts) *model.Finding {
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if d.UDPAvailable && d.UDPOutOfOrderPct > 0.1 {
			ev = append(ev, fmt.Sprintf("%s: %.2f%% datagrams out of order", dirNames[i], d.UDPOutOfOrderPct))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "TR-09",
		Category: model.CategoryTransport,
		Severity: model.SevWarning,
		Text:     "UDP datagrams arrived out of order — reordering should not happen on a direct cable.",
		Evidence: ev,
	}
}

func rulePERF01(f *Facts) *model.Finding {
	if f.NegotiatedSpeed == 0 {
		return nil
	}
	worst := model.Severity(-1)
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if !d.TCPAvailable {
			continue
		}
		ratio := float64(d.TCPBitrate) / float64(f.NegotiatedSpeed)
		var sev model.Severity
		switch {
		case ratio >= 0.9:
			continue
		case ratio >= 0.7:
			sev = model.SevInfo
		case ratio >= 0.4:
			sev = model.SevWarning
		default:
			sev = model.SevPoor
		}
		ev = append(ev, fmt.Sprintf("%s: %s TCP = %.0f%% of the %s link", dirNames[i], d.TCPBitrate, ratio*100, f.NegotiatedSpeed))
		if sev > worst {
			worst = sev
		}
	}
	if worst < model.SevInfo || len(ev) == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:        "PERF-01",
		Category:      model.CategoryPerformance,
		Severity:      worst,
		Text:          "TCP throughput is below the negotiated link speed.",
		Evidence:      ev,
		HostSensitive: true,
	}
}

func rulePERF02(f *Facts) *model.Finding {
	worst := model.Severity(-1)
	var ev []string
	for i := range f.Dir {
		d := &f.Dir[i]
		if !d.TCPAvailable || d.TCPCoV < 0.15 {
			continue
		}
		sev := model.SevWarning
		if d.TCPCoV > 0.30 {
			sev = model.SevPoor
		}
		ev = append(ev, fmt.Sprintf("%s: throughput coefficient of variation %.0f%%", dirNames[i], d.TCPCoV*100))
		if sev > worst {
			worst = sev
		}
	}
	if worst < model.SevWarning {
		return nil
	}
	return &model.Finding{
		RuleID:        "PERF-02",
		Category:      model.CategoryPerformance,
		Severity:      worst,
		Text:          "TCP throughput is unstable across intervals.",
		Evidence:      ev,
		HostSensitive: true,
	}
}

func rulePERF03(f *Facts) *model.Finding {
	total := f.Dir[0].TCPCollapses + f.Dir[1].TCPCollapses
	if total == 0 {
		return nil
	}
	sev := model.SevWarning
	if total >= 3 {
		sev = model.SevPoor
	}
	return &model.Finding{
		RuleID:        "PERF-03",
		Category:      model.CategoryPerformance,
		Severity:      sev,
		Text:          fmt.Sprintf("TCP throughput collapsed below half the median in %d interval(s).", total),
		Evidence:      []string{fmt.Sprintf("%d interval(s) under 50%% of the median interval bitrate", total)},
		HostSensitive: true,
	}
}

func rulePERF04(f *Facts) *model.Finding {
	ratio := asymmetryRatio(f)
	if ratio <= 0.30 {
		return nil
	}
	d0, d1 := &f.Dir[0], &f.Dir[1]
	return &model.Finding{
		RuleID:   "PERF-04",
		Category: model.CategoryPerformance,
		Severity: model.SevWarning,
		Text:     "TCP throughput is asymmetric between the two directions.",
		Evidence: []string{fmt.Sprintf("pc1->pc2 %s vs pc2->pc1 %s (%.0f%% difference)",
			d0.TCPBitrate, d1.TCPBitrate, ratio*100)},
		HostSensitive: true,
	}
}

func ruleHOST01(f *Facts) *model.Finding {
	if f.MaxCPUPct <= 90 {
		return nil
	}
	return &model.Finding{
		RuleID:   "HOST-01",
		Category: model.CategoryHost,
		Severity: model.SevMarker,
		Text:     "CPU was saturated during throughput testing — performance results may be host-limited.",
		Evidence: []string{fmt.Sprintf("max iperf3 CPU utilization %.1f%% > 90%%", f.MaxCPUPct)},
	}
}

func ruleHOST02(f *Facts) *model.Finding {
	if !f.VirtualInterface {
		return nil
	}
	return &model.Finding{
		RuleID:   "HOST-02",
		Category: model.CategoryHost,
		Severity: model.SevMarker,
		Text:     "A virtual interface was tested — the results say nothing about a physical cable.",
		Evidence: []string{"tested interface has no kernel driver binding (virtual)"},
	}
}

func ruleHOST03(f *Facts) *model.Finding {
	if !f.USBAdapter || rulePERF01(f) == nil {
		return nil
	}
	return &model.Finding{
		RuleID:   "HOST-03",
		Category: model.CategoryHost,
		Severity: model.SevMarker,
		Text:     "A USB network adapter was used and throughput fell short — USB adapters are a known bottleneck.",
		Evidence: []string{"tested interface is USB-attached"},
	}
}

func ruleLIM01(f *Facts) *model.Finding {
	noTCP := !f.Dir[0].TCPAvailable && !f.Dir[1].TCPAvailable
	noCounters := !f.PC1.CountersAvailable && !f.PC2.CountersAvailable
	if !noTCP && !noCounters {
		return nil
	}
	var ev []string
	if noTCP {
		ev = append(ev, "no TCP throughput result in either direction")
	}
	if noCounters {
		ev = append(ev, "NIC error counters unavailable on both sides")
	}
	return &model.Finding{
		RuleID:   "LIM-01",
		Category: model.CategoryLimitation,
		Severity: model.SevMarker,
		Text:     "Critical evidence is missing — a clean-looking result would not be trustworthy.",
		Evidence: ev,
	}
}

// noncriticalTests names the tests whose absence caps EXCELLENT to GOOD but
// never invalidates the run.
var noncriticalTests = map[string]bool{
	"ping":           true,
	"udp":            true,
	"bidir":          true,
	"bidirectional":  true,
	"full_size_ping": true,
	"fullsize_ping":  true,
	"cable_test":     true,
	"cable_test_tdr": true,
}

func ruleLIM02(f *Facts) *model.Finding {
	var ev []string
	for _, name := range f.Unavailable {
		if noncriticalTests[name] {
			ev = append(ev, fmt.Sprintf("test %q could not run", name))
		}
	}
	if len(ev) == 0 {
		return nil
	}
	return &model.Finding{
		RuleID:   "LIM-02",
		Category: model.CategoryLimitation,
		Severity: model.SevMarker,
		Text:     "Some non-critical tests could not run — coverage is reduced.",
		Evidence: ev,
	}
}

func ruleLIM03(f *Facts) *model.Finding {
	if !f.Partial {
		return nil
	}
	return &model.Finding{
		RuleID:   "LIM-03",
		Category: model.CategoryLimitation,
		Severity: model.SevMarker,
		Text:     "The run was interrupted — the report covers only the tests that completed.",
		Evidence: []string{"partial run (interrupt or abort)"},
	}
}

func ruleLIM04(f *Facts) *model.Finding {
	if !f.UDPRateAssumed {
		return nil
	}
	return &model.Finding{
		RuleID:   "LIM-04",
		Category: model.CategoryLimitation,
		Severity: model.SevInfo,
		Text:     "The UDP target rate was assumed (link speed unknown) — UDP findings are relative to that assumption.",
		Evidence: []string{"UDP tested at an assumed rate"},
	}
}
