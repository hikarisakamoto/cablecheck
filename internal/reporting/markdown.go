package reporting

import (
	"fmt"
	"strings"

	"cablecheck/internal/model"
)

// md is a minimal markdown builder: strings.Builder plus the four shapes the
// report needs (h2, key/value, table, note). One private section function per
// report section keeps each independently testable.
type md struct {
	sb strings.Builder
}

// line appends one raw line.
func (m *md) line(s string) {
	m.sb.WriteString(s)
	m.sb.WriteByte('\n')
}

// blank appends an empty line.
func (m *md) blank() { m.sb.WriteByte('\n') }

// h1 appends the document title.
func (m *md) h1(title string) {
	m.line("# " + title)
	m.blank()
}

// h2 appends a section header followed by a blank line.
func (m *md) h2(title string) {
	m.line("## " + title)
	m.blank()
}

// kv appends one bolded key/value bullet.
func (m *md) kv(key, value string) {
	m.line(fmt.Sprintf("- **%s:** %s", key, value))
}

// note appends a blockquote note (used for "Not run: ..." markers).
func (m *md) note(text string) {
	m.line("> " + text)
	m.blank()
}

// table appends a pipe table; cell text has pipes escaped.
func (m *md) table(headers []string, rows [][]string) {
	esc := func(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
	cells := make([]string, len(headers))
	for i, h := range headers {
		cells[i] = esc(h)
	}
	m.line("| " + strings.Join(cells, " | ") + " |")
	seps := make([]string, len(headers))
	for i := range seps {
		seps[i] = "---"
	}
	m.line("| " + strings.Join(seps, " | ") + " |")
	for _, row := range rows {
		cells = make([]string, len(row))
		for i, c := range row {
			cells[i] = esc(c)
		}
		m.line("| " + strings.Join(cells, " | ") + " |")
	}
	m.blank()
}

// bytes returns the built document.
func (m *md) bytes() []byte { return []byte(m.sb.String()) }

// RenderMarkdown renders the full 23-section markdown report. It is a pure
// function of the report value: absent data renders a "Not run: <reason>"
// note so section numbering stays stable, and all map iteration is sorted so
// two calls on the same report are byte-identical.
func RenderMarkdown(r *model.Report) []byte {
	b := &md{}
	b.h1("CableCheck Report")
	sectionOverall(b, r)
	sectionScore(b, r)
	sectionSession(b, r)
	sectionMachines(b, r)
	sectionLink(b, r)
	sectionLinkEvents(b, r)
	sectionCounterBaseline(b, r)
	sectionCounterDeltas(b, r)
	sectionPing(b, r)
	sectionFullSizePing(b, r)
	sectionTCP(b, r, 11, model.DirectionPC1ToPC2, "TCP Throughput PC1→PC2")
	sectionTCP(b, r, 12, model.DirectionPC2ToPC1, "TCP Throughput PC2→PC1")
	sectionBidir(b, r)
	sectionUDP(b, r)
	sectionCPU(b, r)
	sectionCableTest(b, r)
	sectionMonitoring(b, r)
	sectionFindings(b, r)
	sectionRecommendations(b, r)
	sectionLimitations(b, r)
	sectionConfiguration(b, r)
	sectionToolVersions(b, r)
	sectionRawIndex(b, r)
	return b.bytes()
}

func sectionOverall(b *md, r *model.Report) {
	b.h2("1. Overall Result")
	class := r.Classification
	if class == "" {
		b.note("Not run: the report carries no classification.")
		return
	}
	b.line(fmt.Sprintf("**%s** — %s", class, verdicts[class]))
	b.blank()
}

func sectionScore(b *md, r *model.Report) {
	b.h2("2. Score & Rule Evidence")
	if r.Classification == "" && r.Score == nil && len(r.Findings) == 0 {
		b.note("Not run: the report carries no evaluation.")
		return
	}
	if r.Score != nil {
		b.kv("Score", fmt.Sprintf("%d/100", *r.Score))
	} else {
		b.kv("Score", "n/a (INCONCLUSIVE runs carry no score)")
	}
	for _, reason := range r.ClassificationReasons {
		b.kv("Reason", reason)
	}
	b.blank()
	if len(r.Findings) == 0 {
		b.note("No rule findings — every rule passed.")
		return
	}
	rows := make([][]string, 0, len(r.Findings))
	for _, fd := range r.Findings {
		rows = append(rows, []string{fd.RuleID, string(fd.Category), fd.Severity.String(), fd.Text})
	}
	b.table([]string{"Rule", "Category", "Severity", "Finding"}, rows)
}

func sectionSession(b *md, r *model.Report) {
	b.h2("3. Session Info")
	b.kv("Test ID", orUnknown(r.TestID))
	b.kv("Schema version", orUnknown(r.SchemaVersion))
	b.kv("Tool version", orUnknown(r.ToolVersion))
	b.kv("Protocol version", orUnknown(r.ProtocolVersion))
	b.kv("Started", fmtTime(r.StartedAt))
	b.kv("Finished", fmtTime(r.FinishedAt))
	b.kv("Duration", r.Duration.String())
	b.kv("Mode", orUnknown(r.Configuration.Mode))
	b.kv("Partial run", yesNo(r.Partial))
	if r.Failure != nil {
		b.kv("Failure", fmt.Sprintf("%s (stage: %s)", r.Failure.Error, r.Failure.Stage))
	}
	b.blank()
}

func sectionMachines(b *md, r *model.Report) {
	b.h2("4. Machines & Environment")
	if r.PC1.Hostname == "" && r.PC2.Hostname == "" && r.PC1.NIC.Name == "" && r.PC2.NIC.Name == "" {
		b.note("Not run: machine details were not recorded.")
		return
	}
	rows := [][]string{}
	for _, side := range []struct {
		name string
		peer model.PeerReport
	}{{"pc1", r.PC1}, {"pc2", r.PC2}} {
		nic := side.peer.NIC
		rows = append(rows, []string{
			side.name,
			orUnknown(side.peer.Hostname),
			orUnknown(side.peer.Kernel),
			orUnknown(side.peer.OS),
			orUnknown(nic.Name),
			orUnknown(nic.Driver),
			fmtSpeedMbps(nic.SpeedMbps),
			orUnknown(nic.Duplex),
			fmt.Sprintf("%d", nic.MTU),
			orUnknown(nic.MAC),
			yesNo(nic.USB),
		})
	}
	b.table([]string{"Side", "Hostname", "Kernel", "OS", "NIC", "Driver", "Speed", "Duplex", "MTU", "MAC", "USB"}, rows)
}

func sectionLink(b *md, r *model.Report) {
	b.h2("5. Interface & Link Negotiation")
	if r.Link == nil {
		b.note("Not run: link negotiation state was not captured.")
		return
	}
	rows := [][]string{}
	addRow := func(side, phase string, ls *model.LinkSettings) {
		if ls == nil {
			return
		}
		rows = append(rows, []string{
			side, phase,
			fmtSpeedMbps(ls.SpeedMbps),
			orUnknown(ls.Duplex),
			orUnknown(ls.AutoNeg),
			yesNo(ls.LinkDetected),
			orUnknown(ls.MDIX),
			orUnknown(strings.Join(ls.PartnerModes, ", ")),
		})
	}
	addRow("pc1", "before", r.Link.PC1.Before)
	addRow("pc1", "after", r.Link.PC1.After)
	addRow("pc2", "before", r.Link.PC2.Before)
	addRow("pc2", "after", r.Link.PC2.After)
	if len(rows) == 0 {
		b.note("Not run: link negotiation state was not captured.")
		return
	}
	b.table([]string{"Side", "Phase", "Speed", "Duplex", "Autoneg", "Link", "MDI-X", "Partner modes"}, rows)
}

// linkEventTypes are the monitoring event types that describe the physical
// link itself (shown in section 6; section 17 shows the full timeline).
var linkEventTypes = map[string]bool{
	"carrier_lost":     true,
	"carrier_restored": true,
	"speed_changed":    true,
	"duplex_changed":   true,
	"renegotiation":    true,
}

func sectionLinkEvents(b *md, r *model.Report) {
	b.h2("6. Link Events Timeline")
	rows := [][]string{}
	for _, ev := range r.MonitoringEvents {
		if linkEventTypes[ev.Type] {
			rows = append(rows, []string{fmtTime(ev.At), ev.Type, ev.Detail})
		}
	}
	if len(rows) == 0 {
		b.note("No link events were observed during the run.")
		return
	}
	b.table([]string{"At", "Event", "Detail"}, rows)
}

func sectionCounterBaseline(b *md, r *model.Report) {
	b.h2("7. Counter Baseline")
	pc1, pc2 := r.InitialCounters.PC1, r.InitialCounters.PC2
	if pc1 == nil && pc2 == nil {
		b.note("Not run: no counter snapshots were captured.")
		return
	}
	keys := unionKeys(standardOf(pc1), standardOf(pc2))
	if len(keys) == 0 {
		b.note("Not run: neither side exposes normalized counters (absence is not zero).")
		return
	}
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, []string{k, counterCell(pc1, k), counterCell(pc2, k)})
	}
	b.table([]string{"Counter", "PC1", "PC2"}, rows)
	b.note("Counters absent on a side are not exposed by that hardware — absence is not zero.")
}

func sectionCounterDeltas(b *md, r *model.Report) {
	b.h2("8. Counter Deltas")
	pc1, pc2 := r.CounterDeltas.PC1, r.CounterDeltas.PC2
	if len(pc1) == 0 && len(pc2) == 0 {
		b.note("Not run: counter deltas were not computed.")
		return
	}
	keys := unionKeys(pc1, pc2)
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, []string{k, deltaCell(pc1, k), deltaCell(pc2, k)})
	}
	b.table([]string{"Counter", "PC1 Δ", "PC2 Δ"}, rows)
	b.note("\"unreliable\" marks a counter that reset or wrapped mid-run; its delta is not evidence.")
}

func sectionPing(b *md, r *model.Report) {
	b.h2("9. Ping Stability")
	pingTable(b, r, r.Tests.Ping, "ping")
}

func sectionFullSizePing(b *md, r *model.Report) {
	b.h2("10. Full-Size Ping")
	pingTable(b, r, r.Tests.FullSizePing, "full_size_ping")
}

// pingTable renders one ping result set or its "Not run" note.
func pingTable(b *md, r *model.Report, results []model.PingResult, skipName string) {
	if len(results) == 0 {
		b.note("Not run: " + skipReason(r, skipName, "no result was recorded."))
		return
	}
	rows := make([][]string, 0, len(results))
	for _, p := range results {
		rows = append(rows, []string{
			dirLabel(p.Direction),
			fmt.Sprintf("%d", p.Transmitted),
			fmt.Sprintf("%d", p.Received),
			fmtPct(p.LossPercent),
			fmt.Sprintf("%d", p.Duplicates),
			fmt.Sprintf("%d", p.SendErrors+p.IcmpErrors),
			fmt.Sprintf("%.2f / %.2f / %.2f / %.2f", p.RTTMinMs, p.RTTAvgMs, p.RTTMaxMs, p.RTTMdevMs),
			fmt.Sprintf("%.1f ms", p.LongestGapMs),
		})
	}
	b.table([]string{"Direction", "Sent", "Received", "Loss", "Dup", "Errors", "RTT min/avg/max/mdev (ms)", "Longest gap"}, rows)
}

func sectionTCP(b *md, r *model.Report, number int, direction, title string) {
	b.h2(fmt.Sprintf("%d. %s", number, title))
	var results []model.TCPResult
	for _, tr := range r.Tests.TCP {
		if tr.Direction == direction {
			results = append(results, tr)
		}
	}
	if len(results) == 0 {
		b.note("Not run: " + skipReason(r, "tcp", "no TCP result was recorded for this direction."))
		return
	}
	rows := make([][]string, 0, len(results))
	for i, tr := range results {
		retrans := "not reported"
		if tr.Retransmissions != nil {
			retrans = fmt.Sprintf("%d", *tr.Retransmissions)
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", i+1),
			tr.Duration.String(),
			fmt.Sprintf("%d", tr.ParallelStreams),
			fmtBps(tr.SenderBitsPerSecond),
			fmtBps(tr.ReceiverBitsPerSecond),
			retrans,
			fmtPct(tr.ThroughputVariation * 100),
			fmtBps(tr.MinimumIntervalBps),
			fmtBps(tr.MaximumIntervalBps),
		})
	}
	b.table([]string{"Run", "Duration", "Streams", "Sender", "Receiver", "Retransmits", "CoV", "Min interval", "Max interval"}, rows)
}

func sectionBidir(b *md, r *model.Report) {
	b.h2("13. Bidirectional Stress")
	bd := r.Tests.Bidirectional
	if bd == nil {
		b.note("Not run: " + skipReason(r, "bidirectional", "no bidirectional result was recorded."))
		return
	}
	rows := [][]string{}
	for _, side := range []struct {
		label string
		dir   model.BidirDirection
	}{
		{dirLabels[model.DirectionPC1ToPC2], bd.PC1ToPC2},
		{dirLabels[model.DirectionPC2ToPC1], bd.PC2ToPC1},
	} {
		retrans := "not reported"
		if side.dir.Retransmissions != nil {
			retrans = fmt.Sprintf("%d", *side.dir.Retransmissions)
		}
		rows = append(rows, []string{
			side.label,
			fmtBps(side.dir.SenderBitsPerSecond),
			fmtBps(side.dir.ReceiverBitsPerSecond),
			retrans,
		})
	}
	b.table([]string{"Direction", "Sender", "Receiver", "Retransmits"}, rows)
	if bd.TwoPhaseFallback {
		b.note("Ran as two coordinated one-way phases (--bidir unavailable on a peer), not simultaneously.")
	}
}

func sectionUDP(b *md, r *model.Report) {
	b.h2("14. UDP Loss & Jitter")
	if len(r.Tests.UDP) == 0 {
		b.note("Not run: " + skipReason(r, "udp", "no UDP result was recorded."))
		return
	}
	rows := make([][]string, 0, len(r.Tests.UDP))
	for _, u := range r.Tests.UDP {
		ooo := "not reported"
		if u.OutOfOrder != nil {
			ooo = fmt.Sprintf("%d", *u.OutOfOrder)
		}
		rows = append(rows, []string{
			dirLabel(u.Direction),
			fmtBps(float64(u.TargetBps)),
			fmtBps(u.ActualSenderBps),
			fmtBps(u.ActualReceiverBps),
			fmt.Sprintf("%d/%d", u.LostPackets, u.TotalPackets),
			fmtPct(u.LossPercent),
			fmt.Sprintf("%.2f ms", u.JitterMs),
			ooo,
		})
	}
	b.table([]string{"Direction", "Target", "Sender", "Receiver", "Lost/Total", "Loss", "Jitter", "Out-of-order"}, rows)
}

func sectionCPU(b *md, r *model.Report) {
	b.h2("15. CPU Utilization")
	rows := [][]string{}
	add := func(test string, cpu model.CPUUsage) {
		rows = append(rows, []string{
			test,
			fmt.Sprintf("%.1f%%", cpu.HostTotal),
			fmt.Sprintf("%.1f%%", cpu.RemoteTotal),
		})
	}
	for _, tr := range r.Tests.TCP {
		add("TCP "+dirLabel(tr.Direction), tr.CPUUtilization)
	}
	if bd := r.Tests.Bidirectional; bd != nil {
		add("Bidirectional", bd.CPUUtilization)
	}
	for _, u := range r.Tests.UDP {
		add("UDP "+dirLabel(u.Direction), u.CPU)
	}
	if len(rows) == 0 {
		b.note("Not run: no throughput test reported CPU utilization.")
		return
	}
	b.table([]string{"Test", "Sender CPU", "Receiver CPU"}, rows)
}

func sectionCableTest(b *md, r *model.Report) {
	b.h2("16. Cable Diagnostics")
	ct := r.Tests.CableTest
	if ct == nil {
		b.note("Not run: " + skipReason(r, "cable_test", "cable diagnostics were not requested."))
		return
	}
	if !ct.Available {
		b.note("Not run: " + orUnknown(ct.UnavailableReason))
		return
	}
	rows := make([][]string, 0, len(ct.Pairs))
	for _, pair := range ct.Pairs {
		fault := "-"
		if pair.HasFault {
			fault = fmt.Sprintf("~%.1f m", pair.FaultMeters)
		}
		rows = append(rows, []string{pair.Pair, string(pair.Status), fault})
	}
	b.table([]string{"Pair", "Status", "Fault distance"}, rows)
}

func sectionMonitoring(b *md, r *model.Report) {
	b.h2("17. Monitoring Timeline")
	if len(r.MonitoringEvents) == 0 {
		b.note("No monitoring events were recorded during the run.")
		return
	}
	rows := make([][]string, 0, len(r.MonitoringEvents))
	for _, ev := range r.MonitoringEvents {
		rows = append(rows, []string{fmtTime(ev.At), ev.Type, ev.Detail})
	}
	b.table([]string{"At", "Event", "Detail"}, rows)
}

func sectionFindings(b *md, r *model.Report) {
	b.h2("18. Findings Detail")
	if len(r.Findings) == 0 {
		b.note("No findings — every rule passed.")
		return
	}
	for _, fd := range r.Findings {
		b.line(fmt.Sprintf("- **%s** [%s/%s] %s", fd.RuleID, fd.Category, fd.Severity, fd.Text))
		for _, ev := range fd.Evidence {
			b.line("  - " + ev)
		}
	}
	b.blank()
}

func sectionRecommendations(b *md, r *model.Report) {
	b.h2("19. Recommendations")
	if len(r.Recommendations) == 0 {
		b.note("No recommendations — no action needed.")
		return
	}
	for i, rec := range r.Recommendations {
		b.line(fmt.Sprintf("%d. %s", i+1, rec))
	}
	b.blank()
}

func sectionLimitations(b *md, r *model.Report) {
	b.h2("20. Limitations & Unavailable Tests")
	if len(r.SkippedTests) == 0 && len(r.Warnings) == 0 {
		b.note("None — every planned test ran and no warnings were raised.")
		return
	}
	if len(r.SkippedTests) > 0 {
		rows := make([][]string, 0, len(r.SkippedTests))
		for _, st := range r.SkippedTests {
			rows = append(rows, []string{st.Name, st.Reason})
		}
		b.table([]string{"Test", "Reason"}, rows)
	}
	for _, w := range r.Warnings {
		b.line("- Warning: " + w)
	}
	if len(r.Warnings) > 0 {
		b.blank()
	}
}

func sectionConfiguration(b *md, r *model.Report) {
	b.h2("21. Configuration Used")
	c := r.Configuration
	if c.Role == "" && c.LocalIP == "" && c.Mode == "" {
		b.note("Not run: no configuration echo was recorded.")
		return
	}
	b.kv("Role", orUnknown(c.Role))
	b.kv("Local IP", orUnknown(c.LocalIP))
	b.kv("Peer IP", orUnknown(c.PeerIP))
	b.kv("Interface", orUnknown(c.Interface))
	b.kv("Mode", orUnknown(c.Mode))
	b.kv("Control port", fmt.Sprintf("%d", c.ControlPort))
	b.kv("iperf3 port", fmt.Sprintf("%d", c.IperfPort))
	b.kv("TCP duration", c.TCPDuration.String())
	b.kv("UDP duration", c.UDPDuration.String())
	b.kv("UDP rate", c.UDPRate.String()+"bit/s")
	b.kv("Parallel streams", fmt.Sprintf("%d", c.ParallelStreams))
	b.kv("Ping count", fmt.Sprintf("%d", c.PingCount))
	b.kv("Ping interval", c.PingInterval.String())
	b.kv("TCP repeats", fmt.Sprintf("%d", c.TCPRepeats))
	if c.SoakDuration != 0 {
		b.kv("Soak duration", c.SoakDuration.String())
		b.kv("Soak load", orUnknown(c.SoakLoad))
	}
	b.kv("Monitor interval", c.MonitorInterval.String())
	b.kv("Cable test requested", yesNo(c.CableTest))
	b.kv("Cable test TDR requested", yesNo(c.CableTestTDR))
	b.kv("Output directory", orUnknown(c.Output))
	b.kv("Verbose", yesNo(c.Verbose))
	b.kv("Non-interactive", yesNo(c.NonInteractive))
	b.kv("No sudo", yesNo(c.NoSudo))
	b.kv("No report transfer", yesNo(c.NoReportTransfer))
	b.kv("Allow virtual interface", yesNo(c.AllowVirtualInterface))
	b.kv("Token auto-generated", yesNo(c.TokenGenerated))
	b.blank()
}

func sectionToolVersions(b *md, r *model.Report) {
	b.h2("22. Tool Versions")
	if len(r.PC1.ToolVersions) == 0 && len(r.PC2.ToolVersions) == 0 {
		b.note("Not run: tool versions were not recorded.")
		return
	}
	tools := unionKeys(r.PC1.ToolVersions, r.PC2.ToolVersions)
	rows := make([][]string, 0, len(tools))
	for _, tool := range tools {
		rows = append(rows, []string{
			tool,
			orUnknown(r.PC1.ToolVersions[tool]),
			orUnknown(r.PC2.ToolVersions[tool]),
		})
	}
	b.table([]string{"Tool", "PC1", "PC2"}, rows)
}

func sectionRawIndex(b *md, r *model.Report) {
	b.h2("23. Raw Artifact Index")
	if len(r.RawFiles) == 0 {
		b.note("No raw artifacts were recorded.")
		return
	}
	rows := make([][]string, 0, len(r.RawFiles))
	for _, rf := range r.RawFiles {
		rows = append(rows, []string{rf.Name, rf.SHA256, fmt.Sprintf("%d", rf.Bytes)})
	}
	b.table([]string{"File", "SHA-256", "Bytes"}, rows)
}

// skipReason looks up the skip reason recorded for a test name, falling back
// to the given default text.
func skipReason(r *model.Report, name, fallback string) string {
	for _, st := range r.SkippedTests {
		if st.Name == name {
			return st.Reason
		}
	}
	return fallback
}
