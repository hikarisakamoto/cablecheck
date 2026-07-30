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
	"slices"
	"strings"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/tcpmetrics"
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
	// UnclassifiedRXErrors is the part of the driver's own receive-error
	// aggregate (rx_errors_total) that no per-cause error counter explains.
	// Realtek NICs report corruption only through that aggregate, so without
	// this residual their receive path looks error-free. It is unexplained
	// rather than proven corrupt, so PHY-02 reports it as such and never fails
	// a run on it alone.
	UnclassifiedRXErrors uint64
	// RXErrorEvidence reports whether this side exposed any receive-error
	// counter at all (per-cause or aggregate). False means corruption was
	// never measured on this side, which is not the same as zero errors.
	RXErrorEvidence bool
	// CarrierEvents is the delta of link_resets (sysfs carrier_changes)
	// during the session.
	CarrierEvents uint64
	// JabberSizeErrors is the delta of jabber + oversize + undersize +
	// rx_length.
	JabberSizeErrors uint64
	// FifoOverrun is the delta of rx_fifo.
	FifoOverrun uint64
	// MissedErrors is the delta of rx_missed.
	MissedErrors uint64
	// FramesReceived is the rtnetlink receive-packet delta across the run: the
	// denominator that turns raw drop and error counts into rates. It is 0 when
	// no traffic was seen or the counter reset, which means "no rate can be
	// computed", not "no frames".
	FramesReceived uint64
	// CarrierPHYErrors is the delta of tx_carrier + phy_errors.
	CarrierPHYErrors uint64
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
	// TCPTrialCount is the number of completed TCP throughput trials used to
	// aggregate this direction.
	TCPTrialCount int
	// TCPThroughputDeviations counts completed trials that did not pass the
	// speed-selected throughput policy. It supports the isolated-outlier cap.
	TCPThroughputDeviations int
	// TCPBitrate is the measured TCP throughput (receiver side).
	TCPBitrate model.Bitrate
	// TCPRetransRate is the sustained retransmission rate: retransmits /
	// estimated segments (bytes/MSS, MSS fallback 1448) reduced across repeat
	// trials by the lower median. Only this drives a poor verdict.
	TCPRetransRate float64
	// TCPRetransRateWorst is the same rate from the worst repeat trial. A burst
	// confined to one trial is real evidence but not the steady state, so it
	// warns without letting trial count alone escalate the verdict.
	TCPRetransRateWorst float64
	// TCPCoV is stdev/mean of per-interval bitrates, first interval
	// excluded, as a fraction.
	TCPCoV float64
	// TCPCollapses counts carried intervals below the canonical share of median
	// interval bitrate (first interval excluded).
	TCPCollapses int
	// TCPMaxCPUPct is the maximum client/server CPU utilization among completed
	// one-way TCP trials in this direction.
	TCPMaxCPUPct float64
	// TCPSenderMaxCPUPct is the maximum sender CPU utilization among completed
	// one-way TCP trials in this direction. CableCheck always runs the iperf3
	// client on the sender, so this is the client-side host total.
	TCPSenderMaxCPUPct float64
	// UDPAvailable reports whether a UDP result exists for this direction.
	UDPAvailable bool
	// UDPLossPct is the worst server-observed datagram loss percentage among
	// runs that reached target without requesting a near-saturation rate.
	UDPLossPct float64
	// UDPJitterMs is the worst RFC 1889 jitter among qualifying runs.
	UDPJitterMs float64
	// UDPOutOfOrderPct is the worst out-of-order datagram percentage among
	// qualifying runs.
	UDPOutOfOrderPct float64
	// UDPMaxCPUPct is the maximum client/server CPU utilization among qualifying
	// UDP runs in this direction.
	UDPMaxCPUPct float64
	// UDPTargetReached reports whether any run reached the configured share of
	// its target without being near the configured saturation boundary.
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
	// UDPNearSaturation reports whether the UDP target rate exceeded the
	// configured share of link speed where loss is expected, not evidence.
	UDPNearSaturation bool
	// Unavailable lists the names of planned tests that could not run.
	Unavailable []string
	// ThroughputUnreachable reports that a throughput test was skipped
	// because iperf3 could not reach the peer's data port, so the cable was
	// never exercised — a firewall/routing problem, not a cable verdict.
	ThroughputUnreachable bool
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
	return factsFromReport(r, Default())
}

// factsFromReport is the threshold-aware implementation behind
// FactsFromReport.
func factsFromReport(r *model.Report, thresholds Thresholds) *Facts {
	f := &Facts{LinkUpAtEnd: true}
	var selfInflicted model.PeerCarrierEvents
	if r.Tests.CableTest != nil {
		selfInflicted = r.Tests.CableTest.SelfInflictedCarrierEvents
	}

	f.PC1 = sideFacts(r.InitialCounters.PC1, r.FinalCounters.PC1,
		selfInflicted.PC1)
	f.PC2 = sideFacts(r.InitialCounters.PC2, r.FinalCounters.PC2,
		selfInflicted.PC2)

	for _, p := range r.Tests.Ping {
		i := dirIndex(p.Direction)
		if i < 0 {
			continue
		}
		d := &f.Dir[i]
		d.PingLossPct = max(d.PingLossPct, p.LossPercent)
		d.PingDuplicates = max(d.PingDuplicates, p.Duplicates)
		d.PingSpikes = max(d.PingSpikes, len(p.Spikes))
		d.PingMaxGap = max(d.PingMaxGap, time.Duration(p.LongestGapMs*float64(time.Millisecond)))
	}
	for _, p := range r.Tests.FullSizePing {
		i := dirIndex(p.Direction)
		if i < 0 {
			continue
		}
		d := &f.Dir[i]
		d.FullSizeAvailable = true
		d.FullSizeLossPct = max(d.FullSizeLossPct, p.LossPercent)
		d.FragErrors = max(d.FragErrors, p.SendErrors+p.IcmpErrors)
	}

	f.NegotiatedSpeed = negotiatedSpeed(r)
	type tcpSamples struct {
		bitrates     []float64
		covs         []float64
		retrans      []float64
		collapses    int
		maxCPU       float64
		senderMaxCPU float64
	}
	var tcp [2]tcpSamples
	var tcpObserved [2]int
	var tcpIncomplete [2]bool
	for _, tr := range r.Tests.TCP {
		i := dirIndex(tr.Direction)
		if i < 0 {
			continue
		}
		tcpObserved[i]++
		if tr.Incomplete {
			tcpIncomplete[i] = true
			continue
		}
		bps := tr.ReceiverBitsPerSecond
		if bps <= 0 {
			bps = tr.SenderBitsPerSecond
		}
		s := &tcp[i]
		s.bitrates = append(s.bitrates, bps)
		s.covs = append(s.covs, tr.ThroughputVariation)
		s.collapses = max(s.collapses, collapseIntervalCount(tr))
		s.maxCPU = max(s.maxCPU, tr.CPUUtilization.HostTotal, tr.CPUUtilization.RemoteTotal)
		s.senderMaxCPU = max(s.senderMaxCPU, tr.CPUUtilization.HostTotal)
		if rate, ok := retransRate(tr); ok {
			s.retrans = append(s.retrans, rate)
		}
	}
	if expected := expectedTCPTrials(r); expected > 0 {
		for i, observed := range tcpObserved {
			if observed < expected {
				tcpIncomplete[i] = true
			}
		}
	}
	for i, samples := range tcp {
		d := &f.Dir[i]
		if len(samples.bitrates) > 0 {
			d.TCPAvailable = true
			d.TCPTrialCount = len(samples.bitrates)
			d.TCPBitrate = model.Bitrate(lowerMedian(samples.bitrates))
			d.TCPCoV = lowerMedian(samples.covs)
			d.TCPCollapses = samples.collapses
			// Retransmissions carry both reductions. The lower median is the
			// sustained rate that can convict; the worst trial preserves a burst a
			// median would smooth away, which warns. Keeping them apart stops a
			// soak's larger trial count from escalating the verdict by itself.
			d.TCPRetransRate = lowerMedian(samples.retrans)
			d.TCPRetransRateWorst = worstOf(samples.retrans)
			d.TCPMaxCPUPct = samples.maxCPU
			d.TCPSenderMaxCPUPct = samples.senderMaxCPU
			for _, bps := range samples.bitrates {
				if throughputShortfall(model.Bitrate(bps), f.NegotiatedSpeed, thresholds) {
					d.TCPThroughputDeviations++
				}
			}
		}
		incomplete := tcpIncomplete[i]
		if incomplete {
			d.TCPAvailable = false
			d.TCPTrialCount = 0
			d.TCPThroughputDeviations = 0
			d.TCPBitrate = 0
			d.TCPCoV = 0
			d.TCPCollapses = 0
			d.TCPRetransRate = 0
			d.TCPRetransRateWorst = 0
			d.TCPMaxCPUPct = 0
			d.TCPSenderMaxCPUPct = 0
		}
	}

	for _, u := range r.Tests.UDP {
		i := dirIndex(u.Direction)
		if i < 0 {
			continue
		}
		d := &f.Dir[i]
		d.UDPAvailable = true
		targetReached := u.TargetBps > 0 && u.ActualSenderBps >= thresholds.UDPTargetReachedAt*float64(u.TargetBps)
		nearSaturation := f.NegotiatedSpeed > 0 &&
			float64(u.TargetBps) > thresholds.UDPNearSaturationAbove*float64(f.NegotiatedSpeed)
		if nearSaturation {
			f.UDPNearSaturation = true
		}
		if !targetReached || nearSaturation {
			continue
		}
		d.UDPTargetReached = true
		d.UDPMaxCPUPct = max(d.UDPMaxCPUPct, u.CPU.HostTotal, u.CPU.RemoteTotal)
		d.UDPLossPct = max(d.UDPLossPct, u.LossPercent)
		d.UDPJitterMs = max(d.UDPJitterMs, u.JitterMs)
		if u.OutOfOrder != nil && u.TotalPackets > 0 {
			pct := float64(*u.OutOfOrder) / float64(u.TotalPackets) * 100
			d.UDPOutOfOrderPct = max(d.UDPOutOfOrderPct, pct)
		}
	}

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

	f.Unavailable = unavailableTests(r)
	f.ThroughputUnreachable = throughputUnreachable(r)
	return f
}

// expectedTCPTrials returns the number of report entries expected in each
// direction. Soak commits trials only with a completed cycle, so interrupted
// cycle-local work is deliberately excluded from this count.
func expectedTCPTrials(r *model.Report) int {
	repeats := r.Configuration.TCPRepeats
	if repeats <= 0 {
		return 0
	}
	if r.Configuration.Mode == "soak" {
		return repeats * r.SoakCyclesCompleted
	}
	return repeats
}

// sideFacts folds a snapshot pair into per-side counter facts.
func sideFacts(before, after *model.CounterSnapshot, selfInflictedCarrier uint64) SideFacts {
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
	present := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := set[k]; ok {
				return true
			}
		}
		return false
	}
	carrierEvents := sum("link_resets")
	if selfInflictedCarrier >= carrierEvents {
		carrierEvents = 0
	} else {
		carrierEvents -= selfInflictedCarrier
	}
	return SideFacts{
		CRCClassErrors:       sum("rx_crc", "rx_frame", "rx_align", "rx_symbol"),
		UnclassifiedRXErrors: unclassifiedRXErrors(sum),
		CarrierEvents:        carrierEvents,
		JabberSizeErrors:     sum("jabber", "oversize", "undersize", "rx_length"),
		FifoOverrun:          sum("rx_fifo"),
		MissedErrors:         sum("rx_missed"),
		FramesReceived:       framesReceived(before, after),
		CarrierPHYErrors:     sum("tx_carrier", "phy_errors"),
		DeltaOK:              ok && available,
		CountersAvailable:    available,
		RXErrorEvidence:      present(rxErrorEvidenceKeys...),
	}
}

// framesReceived returns how many frames this side took in during the run, from
// the rtnetlink packet counter that every driver maintains. A missing snapshot,
// an idle link or a counter reset all yield 0: no denominator, so callers must
// fall back to absolute counts rather than divide.
func framesReceived(before, after *model.CounterSnapshot) uint64 {
	if before == nil || after == nil {
		return 0
	}
	delta, ok := CounterDelta(before.IPStats.RX.Packets, after.IPStats.RX.Packets)
	if !ok {
		return 0
	}
	return delta
}

// rxErrorEvidenceKeys are the counters whose presence proves a side actually
// measured receive corruption. Carrier and receive-ring drop counters are
// deliberately excluded: they say nothing about frame integrity.
var rxErrorEvidenceKeys = []string{"rx_crc", "rx_frame", "rx_align", "rx_symbol", "rx_errors_total"}

// unclassifiedRXErrors returns the part of the driver's receive-error aggregate
// that no per-cause error counter accounts for. Drivers exposing no aggregate
// yield 0.
//
// The receive-ring drop counters are deliberately NOT subtracted: on both
// drivers whose semantics are established here they sit outside the aggregate
// (e1000e's netdev rx_errors never includes the missed-packet register, and
// Realtek's RxErr tally is separate from RxMissed), so subtracting them would
// cancel real corruption evidence one drop for one error. A driver that does
// fold its drops in cannot escalate past a warning on its own: PHY-02 refuses to
// fail a run on unclassified evidence alone.
func unclassifiedRXErrors(sum func(keys ...string) uint64) uint64 {
	total := sum("rx_errors_total")
	explained := sum("rx_crc", "rx_frame", "rx_align", "rx_symbol", "rx_length",
		"undersize", "oversize", "jabber")
	if total <= explained {
		return 0
	}
	return total - explained
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
// the estimated segment count (total bytes / MSS 1448). The boolean is false
// when iperf3 did not report retransmissions or no byte total is known; absent
// data must not be folded into a repeated-trial median as a measured zero.
func retransRate(tr model.TCPResult) (float64, bool) {
	if tr.Retransmissions == nil {
		return 0, false
	}
	var bytes float64
	for _, iv := range tr.IntervalResults {
		bytes += float64(iv.Bytes)
	}
	if bytes == 0 {
		if secs := time.Duration(tr.Duration).Seconds(); secs > 0 {
			bytes = tr.SenderBitsPerSecond * secs / 8
		}
	}
	if bytes <= 0 {
		return 0, false
	}
	segments := bytes / defaultMSS
	if segments <= 0 {
		return 0, false
	}
	return float64(*tr.Retransmissions) / segments, true
}

// collapseIntervalCount consumes carried parser evidence. A nil slice marks
// analysis unavailable (for example, a protocol-v1 result from an older peer),
// so only that compatibility path invokes the same canonical analyzer over the
// retained intervals. A non-nil empty slice is authoritative clean evidence.
func collapseIntervalCount(tr model.TCPResult) int {
	events := tr.Collapses
	if events == nil {
		samples := make([]tcpmetrics.Sample, len(tr.IntervalResults))
		for i, interval := range tr.IntervalResults {
			samples[i] = tcpmetrics.Sample{
				StartSec:      interval.StartSec,
				BitsPerSecond: interval.BitsPerSecond,
			}
		}
		events = tcpmetrics.CollapseEvents(samples)
	}

	const maxInt = int(^uint(0) >> 1)
	total := 0
	for _, event := range events {
		if event.Len <= 0 {
			continue
		}
		if event.Len > maxInt-total {
			return maxInt
		}
		total += event.Len
	}
	return total
}

// tcpCollapseTotal adds the per-direction counts without allowing malformed
// externally constructed facts to wrap a positive total negative.
func tcpCollapseTotal(f *Facts) int {
	return saturatingPositiveTotal(f.Dir[0].TCPCollapses, f.Dir[1].TCPCollapses)
}

// saturatingPositiveTotal adds two non-negative counts while treating
// malformed negatives as zero and preventing integer overflow.
func saturatingPositiveTotal(a, b int) int {
	const maxInt = int(^uint(0) >> 1)
	a = max(0, a)
	b = max(0, b)
	if a > maxInt-b {
		return maxInt
	}
	return a + b
}

// lowerMedian returns the middle value for odd counts and the lower middle
// value for even counts, without mutating the input. Repeat-trial aggregation
// deliberately uses this conservative policy for standard mode's two trials.
func lowerMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := sortedValues(values)
	return sorted[(len(sorted)-1)/2]
}

// worstOf returns the largest sample, the reduction for metrics that count
// discrete anomalies rather than measure a steady state: an event that happened
// in one trial happened, and no other trial disproves it. Empty means no
// samples, which reads as no evidence.
func worstOf(values []float64) float64 {
	var worst float64
	for _, v := range values {
		worst = max(worst, v)
	}
	return worst
}

func sortedValues(values []float64) []float64 {
	sorted := make([]float64, len(values))
	copy(sorted, values)
	slices.Sort(sorted)
	return sorted
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
		if ev.SelfInflicted {
			continue
		}
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
		if tr.Incomplete {
			continue
		}
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

// throughputUnreachable reports whether a throughput test was skipped because
// iperf3 could not reach the peer's data port, keyed off the marker the
// testsuite shares via model.SkipReasonUnreachable.
func throughputUnreachable(r *model.Report) bool {
	for _, st := range r.SkippedTests {
		if strings.HasPrefix(st.Reason, model.SkipReasonUnreachable) {
			return true
		}
	}
	return false
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
	} else if ct != nil && ct.TDRUnavailableReason != "" {
		add("cable_test_tdr")
	}
	return names
}
