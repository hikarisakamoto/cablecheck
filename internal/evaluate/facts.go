// Package evaluate turns a completed model.Report into a flat Facts value and
// applies a fixed, ordered rule set to it, producing the health
// classification, an optional 0-100 score, findings with evidence, and
// actionable recommendations.
//
// The package is pure: FactsFromReport reads a report, Evaluate reads Facts,
// and neither touches the clock, the filesystem, or the network, so every
// rule is table-testable in isolation.
package evaluate

import (
	"strings"
	"time"

	"cablecheck/internal/model"
)

// defaultMSS is the TCP maximum segment size used to estimate segment counts
// from byte totals when computing the retransmission rate (iperf3 JSON has no
// segment count). 1448 = 1500 MTU - 40 IP/TCP headers - 12 timestamps.
const defaultMSS = 1448

// SideFacts summarizes one peer's NIC counter movement across the run.
// Aggregates only accumulate per-counter deltas whose wrap check passed;
// DeltaOK=false marks the whole side as unreliable and counter rules must not
// treat its aggregates as evidence.
type SideFacts struct {
	// CRCClassErrors is the delta of rx_crc + rx_frame + rx_align +
	// rx_symbol, wrap-safe; only counted by rules when DeltaOK.
	CRCClassErrors uint64
	// CarrierEvents is the delta of link_resets (sysfs carrier_changes)
	// during the session.
	CarrierEvents uint64
	// JabberSizeErrors is the delta of jabber + oversize + undersize +
	// rx_length.
	JabberSizeErrors uint64
	// FifoOverrun is the delta of rx_fifo.
	FifoOverrun uint64
	// DeltaOK is false on counter reset/wrap, a key missing from one
	// capture, or capture failure.
	DeltaOK bool
	// CountersAvailable reports whether both snapshots carried at least one
	// normalized counter (absence is not zero).
	CountersAvailable bool
}

// DirFacts holds the per-direction transport and performance evidence for one
// traffic direction (index 0 = pc1->pc2, index 1 = pc2->pc1).
type DirFacts struct {
	// TCPAvailable reports whether a TCP throughput result exists for this
	// direction.
	TCPAvailable bool
	// TCPBitrate is the measured TCP throughput (receiver side).
	TCPBitrate model.Bitrate
	// TCPRetransRate is retransmits / estimated segments (bytes/MSS, MSS
	// fallback 1448), as a fraction (0.001 = 0.1%).
	TCPRetransRate float64
	// TCPCoV is stdev/mean of per-interval bitrates, first interval
	// excluded, as a fraction.
	TCPCoV float64
	// TCPCollapses counts intervals below 50% of the median interval
	// bitrate (first interval excluded).
	TCPCollapses int
	// UDPAvailable reports whether a UDP result exists for this direction.
	UDPAvailable bool
	// UDPLossPct is the server-observed datagram loss percentage.
	UDPLossPct float64
	// UDPJitterMs is the RFC 1889 jitter in milliseconds.
	UDPJitterMs float64
	// UDPOutOfOrderPct is the out-of-order datagram percentage.
	UDPOutOfOrderPct float64
	// UDPTargetReached reports whether the actual send rate reached at
	// least 90% of the target rate.
	UDPTargetReached bool
	// PingLossPct is the standard ping loss percentage.
	PingLossPct float64
	// PingDuplicates counts DUP! replies.
	PingDuplicates int
	// PingSpikes counts replies with RTT far above the median.
	PingSpikes int
	// PingMaxGap is the longest gap between consecutive ping replies.
	PingMaxGap time.Duration
	// FullSizeLossPct is the loss percentage of the full-MTU ping test.
	FullSizeLossPct float64
	// FullSizeAvailable reports whether the full-size ping test ran in this
	// direction.
	FullSizeAvailable bool
	// FragErrors counts "-M do" fragmentation-needed failures (send errors
	// plus ICMP errors of the full-size ping).
	FragErrors int
}

// Facts is the flat evidence model the rules evaluate. Every field is plain
// data assembled from the pre-evaluation report; fields the current pipeline
// cannot derive stay at their zero value (rules treat zero as "no evidence").
type Facts struct {
	// PC1 and PC2 are the per-side counter facts.
	PC1, PC2 SideFacts
	// Dir holds the per-direction facts: [0] = pc1->pc2, [1] = pc2->pc1.
	Dir [2]DirFacts
	// NegotiatedSpeed is the link speed during the test; 0 = unknown.
	NegotiatedSpeed model.Bitrate
	// ExpectedSpeed is min(local supported max, peer advertised max);
	// 0 = unknown.
	ExpectedSpeed model.Bitrate
	// HalfDuplex reports whether either side negotiated half duplex.
	HalfDuplex bool
	// LinkUpAtEnd is false only when a captured post-test link state shows
	// the link down (unknown states count as up).
	LinkUpAtEnd bool
	// Renegotiations counts mid-test speed/duplex changes seen by the link
	// monitor.
	Renegotiations int
	// CableTestRan reports whether ethtool cable diagnostics produced pair
	// results.
	CableTestRan bool
	// CableTestPairs holds the per-pair cable test outcomes.
	CableTestPairs []model.CablePairResult
	// MaxCPUPct is the maximum iperf3 host/remote CPU utilization observed
	// across all throughput tests.
	MaxCPUPct float64
	// USBAdapter reports whether either side tested through a USB adapter.
	USBAdapter bool
	// VirtualInterface reports whether either side tested a virtual
	// interface (a named NIC without a kernel driver binding).
	VirtualInterface bool
	// Partial reports whether the run was interrupted.
	Partial bool
	// UDPRateAssumed reports whether the UDP target rate was assumed rather
	// than derived from a known link speed.
	UDPRateAssumed bool
	// UDPNearSaturation reports whether the UDP target rate exceeded 95% of
	// the negotiated link speed (loss there is expected, not evidence).
	UDPNearSaturation bool
	// Unavailable lists the names of planned tests that could not run.
	Unavailable []string
}

// CounterDelta returns the change of a single counter across the run. A
// counter that went backwards (reset or wrap) yields (0, false) — never
// negative math and never a bogus huge delta.
func CounterDelta(before, after uint64) (uint64, bool) {
	if after < before {
		return 0, false
	}
	return after - before, true
}

// DeltaSet computes the per-key counter deltas between two snapshots.
//
// A key present in only one snapshot is absent from the result (no data is
// not zero) and marks the set as unreliable. The boolean result is true only
// when both snapshots exist, every key matched, and no counter wrapped.
func DeltaSet(before, after *model.CounterSnapshot) (model.CounterDeltaSet, bool) {
	set := model.CounterDeltaSet{}
	if before == nil || after == nil {
		return set, false
	}
	ok := true
	for key, b := range before.Standard {
		a, present := after.Standard[key]
		if !present {
			ok = false
			continue
		}
		delta, dok := CounterDelta(b, a)
		if !dok {
			ok = false
		}
		set[key] = model.CounterDelta{Delta: delta, OK: dok}
	}
	for key := range after.Standard {
		if _, present := before.Standard[key]; !present {
			ok = false
		}
	}
	return set, ok
}

// FactsFromReport assembles the flat evidence model from a pre-evaluation
// report. It never mutates the report.
func FactsFromReport(r *model.Report) *Facts {
	f := &Facts{LinkUpAtEnd: true}

	f.PC1 = sideFacts(r.InitialCounters.PC1, r.FinalCounters.PC1)
	f.PC2 = sideFacts(r.InitialCounters.PC2, r.FinalCounters.PC2)

	for _, p := range r.Tests.Ping {
		i := dirIndex(p.Direction)
		if i < 0 {
			continue
		}
		d := &f.Dir[i]
		d.PingLossPct = p.LossPercent
		d.PingDuplicates = p.Duplicates
		d.PingSpikes = len(p.Spikes)
		d.PingMaxGap = time.Duration(p.LongestGapMs * float64(time.Millisecond))
	}
	for _, p := range r.Tests.FullSizePing {
		i := dirIndex(p.Direction)
		if i < 0 {
			continue
		}
		d := &f.Dir[i]
		d.FullSizeAvailable = true
		d.FullSizeLossPct = p.LossPercent
		d.FragErrors += p.SendErrors + p.IcmpErrors
	}

	var tcpSeen [2]bool
	for _, tr := range r.Tests.TCP {
		i := dirIndex(tr.Direction)
		if i < 0 || tcpSeen[i] {
			continue // evaluate the first run per direction
		}
		tcpSeen[i] = true
		if tr.Incomplete {
			continue
		}
		d := &f.Dir[i]
		d.TCPAvailable = true
		bps := tr.ReceiverBitsPerSecond
		if bps <= 0 {
			bps = tr.SenderBitsPerSecond
		}
		if bps > 0 {
			d.TCPBitrate = model.Bitrate(bps)
		}
		d.TCPCoV = tr.ThroughputVariation
		d.TCPCollapses = collapses(tr.IntervalResults)
		d.TCPRetransRate = retransRate(tr)
	}

	for _, u := range r.Tests.UDP {
		i := dirIndex(u.Direction)
		if i < 0 {
			continue
		}
		d := &f.Dir[i]
		d.UDPAvailable = true
		d.UDPLossPct = u.LossPercent
		d.UDPJitterMs = u.JitterMs
		if u.OutOfOrder != nil && u.TotalPackets > 0 {
			d.UDPOutOfOrderPct = float64(*u.OutOfOrder) / float64(u.TotalPackets) * 100
		}
		d.UDPTargetReached = u.TargetBps > 0 && u.ActualSenderBps >= 0.9*float64(u.TargetBps)
	}

	f.NegotiatedSpeed = negotiatedSpeed(r)
	f.ExpectedSpeed = expectedSpeed(r)
	f.HalfDuplex = halfDuplex(r)
	f.LinkUpAtEnd = linkUpAtEnd(r)
	f.Renegotiations = renegotiations(r)

	if ct := r.Tests.CableTest; ct != nil && ct.Available {
		f.CableTestRan = true
		f.CableTestPairs = ct.Pairs
	}

	f.MaxCPUPct = maxCPUPct(r)
	f.USBAdapter = r.PC1.NIC.USB || r.PC2.NIC.USB
	f.VirtualInterface = virtualNIC(r.PC1.NIC) || virtualNIC(r.PC2.NIC)
	f.Partial = r.Partial
	f.UDPRateAssumed = r.UDPRateAssumed

	if f.NegotiatedSpeed > 0 {
		for _, u := range r.Tests.UDP {
			if float64(u.TargetBps) > 0.95*float64(f.NegotiatedSpeed) {
				f.UDPNearSaturation = true
			}
		}
	}

	f.Unavailable = unavailableTests(r)
	return f
}

// sideFacts folds a snapshot pair into per-side counter facts.
func sideFacts(before, after *model.CounterSnapshot) SideFacts {
	set, ok := DeltaSet(before, after)
	available := before != nil && after != nil &&
		len(before.Standard) > 0 && len(after.Standard) > 0
	sum := func(keys ...string) uint64 {
		var total uint64
		for _, k := range keys {
			if d, present := set[k]; present && d.OK {
				total += d.Delta
			}
		}
		return total
	}
	return SideFacts{
		CRCClassErrors:    sum("rx_crc", "rx_frame", "rx_align", "rx_symbol"),
		CarrierEvents:     sum("link_resets"),
		JabberSizeErrors:  sum("jabber", "oversize", "undersize", "rx_length"),
		FifoOverrun:       sum("rx_fifo"),
		DeltaOK:           ok && available,
		CountersAvailable: available,
	}
}

// dirIndex maps a direction label to its Facts.Dir index, -1 when unknown.
func dirIndex(direction string) int {
	switch direction {
	case model.DirectionPC1ToPC2:
		return 0
	case model.DirectionPC2ToPC1:
		return 1
	}
	return -1
}

// retransRate estimates the TCP retransmission rate as retransmits divided by
// the estimated segment count (total bytes / MSS 1448). It returns 0 when the
// running iperf3 did not report retransmissions or no byte total is known.
func retransRate(tr model.TCPResult) float64 {
	if tr.Retransmissions == nil {
		return 0
	}
	var bytes float64
	for _, iv := range tr.IntervalResults {
		bytes += float64(iv.Bytes)
	}
	if bytes == 0 {
		if secs := tr.Duration.Std().Seconds(); secs > 0 {
			bytes = tr.SenderBitsPerSecond * secs / 8
		}
	}
	if bytes <= 0 {
		return 0
	}
	segments := bytes / defaultMSS
	if segments <= 0 {
		return 0
	}
	return float64(*tr.Retransmissions) / segments
}

// collapses counts intervals whose bitrate fell below 50% of the median
// interval bitrate. The first interval is excluded from both the median and
// the count (TCP slow-start would otherwise flag every run at t=0).
func collapses(ivs []model.TCPInterval) int {
	if len(ivs) < 2 {
		return 0
	}
	rates := make([]float64, 0, len(ivs)-1)
	for _, iv := range ivs[1:] {
		rates = append(rates, iv.BitsPerSecond)
	}
	med := median(rates)
	if med <= 0 {
		return 0
	}
	n := 0
	for _, rate := range rates {
		if rate < med/2 {
			n++
		}
	}
	return n
}

// median returns the median of the values (mean of the middle pair for even
// counts) without mutating the input.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	for i := 1; i < len(sorted); i++ { // insertion sort: n is tiny
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// settingsSpeed converts a captured link state to a Bitrate, 0 when unknown.
func settingsSpeed(ls *model.LinkSettings) model.Bitrate {
	if ls == nil || ls.SpeedMbps <= 0 {
		return 0
	}
	return model.Bitrate(uint64(ls.SpeedMbps) * 1_000_000)
}

// nicSpeed converts a NIC description's speed to a Bitrate, 0 when unknown.
func nicSpeed(nic model.NICReport) model.Bitrate {
	if nic.SpeedMbps <= 0 {
		return 0
	}
	return model.Bitrate(uint64(nic.SpeedMbps) * 1_000_000)
}

// negotiatedSpeed picks the link speed during the test: captured link state
// first (before, then after, either side), then the NIC descriptions.
func negotiatedSpeed(r *model.Report) model.Bitrate {
	if r.Link != nil {
		for _, ls := range []*model.LinkSettings{
			r.Link.PC1.Before, r.Link.PC2.Before, r.Link.PC1.After, r.Link.PC2.After,
		} {
			if s := settingsSpeed(ls); s > 0 {
				return s
			}
		}
	}
	if s := nicSpeed(r.PC1.NIC); s > 0 {
		return s
	}
	return nicSpeed(r.PC2.NIC)
}

// expectedSpeed computes min(local supported max, peer advertised max) from
// the captured link modes; 0 when either side is unknown.
func expectedSpeed(r *model.Report) model.Bitrate {
	if r.Link == nil {
		return 0
	}
	local := maxModeSpeed(linkModes(r.Link.PC1, func(ls *model.LinkSettings) []string { return ls.SupportedModes }))
	peer := maxModeSpeed(linkModes(r.Link.PC1, func(ls *model.LinkSettings) []string { return ls.PartnerModes }))
	if peer == 0 {
		peer = maxModeSpeed(linkModes(r.Link.PC2, func(ls *model.LinkSettings) []string { return ls.AdvertisedModes }))
	}
	if peer == 0 {
		peer = maxModeSpeed(linkModes(r.Link.PC2, func(ls *model.LinkSettings) []string { return ls.SupportedModes }))
	}
	if local == 0 || peer == 0 {
		return 0
	}
	return min(local, peer)
}

// linkModes extracts a mode list from an endpoint's before (falling back to
// after) capture.
func linkModes(ep model.LinkEndpoint, pick func(*model.LinkSettings) []string) []string {
	if ep.Before != nil {
		if modes := pick(ep.Before); len(modes) > 0 {
			return modes
		}
	}
	if ep.After != nil {
		return pick(ep.After)
	}
	return nil
}

// maxModeSpeed parses link mode names like "1000baseT/Full" and returns the
// highest speed as a Bitrate, 0 when nothing parses.
func maxModeSpeed(modes []string) model.Bitrate {
	var best uint64
	for _, mode := range modes {
		var mbps uint64
		i := 0
		for i < len(mode) && mode[i] >= '0' && mode[i] <= '9' {
			mbps = mbps*10 + uint64(mode[i]-'0')
			i++
		}
		if i == 0 || !strings.Contains(strings.ToLower(mode[i:]), "base") {
			continue
		}
		if mbps > best {
			best = mbps
		}
	}
	return model.Bitrate(best * 1_000_000)
}

// halfDuplex reports whether any captured link state or NIC description shows
// half duplex.
func halfDuplex(r *model.Report) bool {
	if r.Link != nil {
		for _, ls := range []*model.LinkSettings{
			r.Link.PC1.Before, r.Link.PC1.After, r.Link.PC2.Before, r.Link.PC2.After,
		} {
			if ls != nil && strings.EqualFold(ls.Duplex, "half") {
				return true
			}
		}
	}
	return strings.EqualFold(r.PC1.NIC.Duplex, "half") || strings.EqualFold(r.PC2.NIC.Duplex, "half")
}

// linkUpAtEnd is false only when a captured after-test link state reports the
// link down; missing captures count as up (absence of evidence is not link
// loss).
func linkUpAtEnd(r *model.Report) bool {
	if r.Link == nil {
		return true
	}
	if a := r.Link.PC1.After; a != nil && !a.LinkDetected {
		return false
	}
	if a := r.Link.PC2.After; a != nil && !a.LinkDetected {
		return false
	}
	return true
}

// renegotiations counts monitoring events that indicate a mid-test link
// parameter change.
func renegotiations(r *model.Report) int {
	n := 0
	for _, ev := range r.MonitoringEvents {
		switch ev.Type {
		case "renegotiation", "speed_changed", "duplex_changed":
			n++
		}
	}
	return n
}

// maxCPUPct returns the maximum host/remote CPU utilization across every
// throughput test in the report.
func maxCPUPct(r *model.Report) float64 {
	var best float64
	consider := func(cpu model.CPUUsage) {
		if cpu.HostTotal > best {
			best = cpu.HostTotal
		}
		if cpu.RemoteTotal > best {
			best = cpu.RemoteTotal
		}
	}
	for _, tr := range r.Tests.TCP {
		consider(tr.CPUUtilization)
	}
	for _, u := range r.Tests.UDP {
		consider(u.CPU)
	}
	if b := r.Tests.Bidirectional; b != nil {
		consider(b.CPUUtilization)
	}
	return best
}

// virtualNIC reports whether a described interface is virtual: a physical NIC
// always has a kernel driver binding (device/driver symlink), so a named
// interface without one is virtual (veth, bridge, loopback, ...).
func virtualNIC(nic model.NICReport) bool {
	return nic.Name != "" && nic.Driver == ""
}

// unavailableTests collects the names of planned tests that could not run.
func unavailableTests(r *model.Report) []string {
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, st := range r.SkippedTests {
		add(st.Name)
	}
	if ct := r.Tests.CableTest; ct != nil && !ct.Available {
		add("cable_test")
	}
	return names
}
