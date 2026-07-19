package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
)

// ErrIperfClient reports that the iperf3 client wrote a JSON document with a
// non-empty "error" field (it does so even with -J and exit code 1). The
// accompanying Iperf3Result still carries everything that decoded.
var ErrIperfClient = errors.New("iperf3 client error")

// decodePrefixLen is how many raw bytes a DecodeError preserves.
const decodePrefixLen = 256

// DecodeError reports iperf3 output that could not be decoded as JSON at
// all (truncated, corrupted, or not JSON). Prefix carries the first 256 raw
// bytes for diagnostics.
type DecodeError struct {
	// Err is the underlying JSON decoding error.
	Err error
	// Prefix is the beginning of the raw output (up to 256 bytes).
	Prefix string
}

// Error describes the decode failure and echoes the raw output prefix.
func (e *DecodeError) Error() string {
	return fmt.Sprintf("iperf3 output is not decodable JSON: %v; output begins: %s", e.Err, e.Prefix)
}

// Unwrap returns the underlying JSON error.
func (e *DecodeError) Unwrap() error { return e.Err }

// iperfSum is one summary row (per-interval or end-of-test) of iperf3 JSON.
type iperfSum struct {
	Start         float64 `json:"start"`
	End           float64 `json:"end"`
	Seconds       float64 `json:"seconds"`
	Bytes         uint64  `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	Retransmits   *uint64 `json:"retransmits"` // pointer: absent for receiver side and all UDP
	Sender        bool    `json:"sender"`
}

// iperfUDP is a UDP summary row of iperf3 JSON.
type iperfUDP struct {
	Bytes         uint64  `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	JitterMs      float64 `json:"jitter_ms"`
	LostPackets   int64   `json:"lost_packets"`
	Packets       int64   `json:"packets"`
	LostPercent   float64 `json:"lost_percent"`
	OutOfOrder    *int64  `json:"out_of_order"` // absent on some versions
	Sender        bool    `json:"sender"`
}

// iperfInterval is one per-second entry of the intervals array. In bidir
// mode 3.12+ emit sum (forward) plus sum_bidir_reverse (reverse); 3.7-3.11
// emit two duplicate "sum" keys instead (Go keeps the last, reverse one), so
// the per-stream rows are kept as the fallback source for the forward series.
type iperfInterval struct {
	Streams         []iperfSum `json:"streams"`
	Sum             *iperfSum  `json:"sum"`
	SumBidirReverse *iperfSum  `json:"sum_bidir_reverse"`
}

// iperfEndStream is one entry of end.streams: TCP rows carry sender/receiver
// summaries, UDP rows carry a single udp summary.
type iperfEndStream struct {
	Sender   *iperfSum `json:"sender"`
	Receiver *iperfSum `json:"receiver"`
	UDP      *iperfUDP `json:"udp"`
}

// iperfWire mirrors the iperf3 client -J document across versions 3.7-3.17;
// every field is optional-tolerant and unknown fields are ignored.
type iperfWire struct {
	Start struct {
		Version   string `json:"version"`
		TestStart struct {
			Protocol   string  `json:"protocol"`
			NumStreams int     `json:"num_streams"`
			Duration   float64 `json:"duration"`
			Reverse    int     `json:"reverse"`
			Blksize    int     `json:"blksize"`
		} `json:"test_start"`
	} `json:"start"`
	Intervals []iperfInterval `json:"intervals"`
	End       struct {
		Streams     []iperfEndStream `json:"streams"`
		SumSent     *iperfSum        `json:"sum_sent"`     // TCP
		SumReceived *iperfSum        `json:"sum_received"` // TCP
		Sum         *iperfUDP        `json:"sum"`          // UDP (server-observed loss on the sender side)
		CPU         *struct {
			HostTotal    float64 `json:"host_total"`
			HostUser     float64 `json:"host_user"`
			HostSystem   float64 `json:"host_system"`
			RemoteTotal  float64 `json:"remote_total"`
			RemoteUser   float64 `json:"remote_user"`
			RemoteSystem float64 `json:"remote_system"`
		} `json:"cpu_utilization_percent"`
		SenderTCPCongestion   string `json:"sender_tcp_congestion"`
		ReceiverTCPCongestion string `json:"receiver_tcp_congestion"`
	} `json:"end"`
	Error string `json:"error"` // present and non-empty on client failure even in -J mode
}

// DirStats is the aggregate of one traffic direction of a TCP test.
type DirStats struct {
	// Bytes is the total bytes carried in this direction.
	Bytes uint64
	// BitsPerSecond is the sender-side average throughput in this direction.
	BitsPerSecond float64
	// ReceiverBitsPerSecond is the receiver-side average throughput in this
	// direction. It is populated from the distinct end.streams[].receiver
	// rows emitted by native --bidir runs.
	ReceiverBitsPerSecond float64
	// Retransmits is the TCP retransmission count; nil when the running
	// iperf3 did not report it (absent is not zero — receiver-side sums and
	// all UDP runs have no retransmit counter).
	Retransmits *uint64
}

// BidirStats holds the two directions of a --bidir run, always derived from
// end.streams[] sender flags because iperf3 3.7-3.11 emit duplicate,
// misattributed top-level bidir sums.
type BidirStats struct {
	// LocalToPeer is the client-to-server direction.
	LocalToPeer DirStats
	// PeerToLocal is the server-to-client direction.
	PeerToLocal DirStats
}

// UDPStats is the result of a UDP test as observed by the receiving server
// and reported back inside the sending client's end.sum.
type UDPStats struct {
	// TargetBps is the requested send rate; the parser leaves it 0 because
	// it only appears on the command line, and the caller fills it in.
	TargetBps uint64
	// ActualBps is the achieved rate.
	ActualBps float64
	// JitterMs is the RFC 1889 jitter in milliseconds.
	JitterMs float64
	// Lost is the number of datagrams lost.
	Lost int64
	// Total is the number of datagrams sent.
	Total int64
	// LostPercent is the datagram loss percentage.
	LostPercent float64
	// OutOfOrder counts datagrams received out of order; nil when the
	// running iperf3 does not report it.
	OutOfOrder *int64
}

// IntervalStat is one per-second interval summary ("sum") row.
type IntervalStat struct {
	// StartSec is the interval start offset in seconds.
	StartSec float64
	// EndSec is the interval end offset in seconds.
	EndSec float64
	// Bytes carried during the interval.
	Bytes uint64
	// Bps is the interval throughput in bits per second.
	Bps float64
	// Retransmits during the interval; nil when not reported.
	Retransmits *uint64
}

// CollapseEvent is a run of consecutive intervals whose throughput fell below
// 10% of the median interval throughput — the signature of a link stall or
// retransmission storm.
type CollapseEvent struct {
	// StartSec is the start offset of the first collapsed interval.
	StartSec float64
	// Len is the number of consecutive collapsed intervals.
	Len int
	// MinBps is the slowest interval throughput inside the run.
	MinBps float64
}

// CPUUtil is iperf3's cpu_utilization_percent block.
type CPUUtil struct {
	// HostTotal is total CPU % on the side that ran the client.
	HostTotal float64
	// HostUser is user-space CPU % on the client side.
	HostUser float64
	// HostSystem is kernel CPU % on the client side.
	HostSystem float64
	// RemoteTotal is total CPU % on the server side.
	RemoteTotal float64
	// RemoteUser is user-space CPU % on the server side.
	RemoteUser float64
	// RemoteSystem is kernel CPU % on the server side.
	RemoteSystem float64
}

// Iperf3Result is the normalized outcome of one iperf3 client run, shaped
// identically across iperf3 3.7-3.17.
type Iperf3Result struct {
	// Version is iperf3's own version string, e.g. "iperf 3.16".
	Version string
	// Protocol is "TCP" or "UDP".
	Protocol string
	// Streams is the number of parallel streams (-P).
	Streams int
	// DurationSec is the configured test duration in seconds.
	DurationSec float64
	// Sent is the sender-side total (end.sum_sent); nil in bidir/UDP runs.
	Sent *DirStats
	// Received is the receiver-side total (end.sum_received); nil in
	// bidir/UDP runs.
	Received *DirStats
	// Bidir carries per-direction totals of a --bidir run, derived from
	// end.streams[]; nil otherwise.
	Bidir *BidirStats
	// UDP carries the server-observed loss/jitter of a UDP run; nil for TCP.
	UDP *UDPStats
	// Intervals are the per-second interval sum rows (forward direction).
	Intervals []IntervalStat
	// IntervalMinBps is the slowest interval, excluding the first
	// (slow-start) interval.
	IntervalMinBps float64
	// IntervalMaxBps is the fastest interval, excluding the first interval.
	IntervalMaxBps float64
	// IntervalAvgBps is the mean interval throughput, excluding the first
	// interval.
	IntervalAvgBps float64
	// IntervalCoV is the coefficient of variation (stdev/mean) of interval
	// throughput, excluding the first interval.
	IntervalCoV float64
	// Collapses lists runs of intervals below 10% of the median interval
	// throughput (first interval excluded from both median and detection).
	Collapses []CollapseEvent
	// CPU is iperf3's CPU utilization report; nil when absent.
	CPU *CPUUtil
	// CongSender is the sender-side TCP congestion algorithm, when reported.
	CongSender string
	// CongReceiver is the receiver-side TCP congestion algorithm, when
	// reported.
	CongReceiver string
	// Error is iperf3's own error message, when the run failed.
	Error string
}

// ParseIperf3 decodes and normalizes an iperf3 client -J document.
//
// Behavior encoded here (see docs/design/tools.md §2):
//   - Undecodable input returns a *DecodeError carrying the first 256 raw
//     bytes; a document with a non-empty "error" field returns
//     ErrIperfClient alongside whatever decoded (exit-1 client output is
//     still JSON — decode first, then check).
//   - TCP totals prefer end.sum_sent / end.sum_received; retransmit counts
//     stay pointers so "absent" never turns into 0.
//   - Bidir runs (detected via end.streams[] rows whose direction flag is
//     false, since -R is never used) ignore the top-level end sums entirely
//     and aggregate per direction from the streams — 3.7-3.11 emit duplicate
//     misattributed bidir sum keys. The interval series keeps the forward
//     direction: intervals[].sum, re-derived from the stream rows when the
//     surviving duplicate is the reverse one.
//   - UDP out_of_order is read from end.sum when present and otherwise
//     summed from end.streams[].udp (where iperf3 actually emits it); it
//     stays nil when no row reports one — absent is not zero.
//   - Interval statistics (min/max/avg/CoV, collapse detection) exclude the
//     first interval to keep TCP slow-start out of the variation signal.
func ParseIperf3(out []byte) (Iperf3Result, error) {
	var w iperfWire
	if err := json.Unmarshal(out, &w); err != nil {
		prefix := out
		if len(prefix) > decodePrefixLen {
			prefix = prefix[:decodePrefixLen]
		}
		return Iperf3Result{}, &DecodeError{Err: err, Prefix: string(prefix)}
	}

	res := Iperf3Result{
		Version:      w.Start.Version,
		Protocol:     w.Start.TestStart.Protocol,
		Streams:      w.Start.TestStart.NumStreams,
		DurationSec:  w.Start.TestStart.Duration,
		CongSender:   w.End.SenderTCPCongestion,
		CongReceiver: w.End.ReceiverTCPCongestion,
		Error:        w.Error,
	}
	if w.Error != "" {
		return res, fmt.Errorf("%w: %s", ErrIperfClient, w.Error)
	}

	if c := w.End.CPU; c != nil {
		res.CPU = &CPUUtil{
			HostTotal: c.HostTotal, HostUser: c.HostUser, HostSystem: c.HostSystem,
			RemoteTotal: c.RemoteTotal, RemoteUser: c.RemoteUser, RemoteSystem: c.RemoteSystem,
		}
	}

	// Bidir detection first (it shapes interval handling): -R is never used,
	// so an end stream whose direction flag is false can only come from
	// --bidir. Every row of a stream carries the stream's direction, so the
	// sender row is preferred and the receiver row is the fallback.
	bidir := false
	for _, st := range w.End.Streams {
		if row := streamDirRow(st); row != nil && !row.Sender {
			bidir = true
			break
		}
	}

	for _, iv := range w.Intervals {
		if !bidir {
			if iv.Sum != nil {
				res.Intervals = append(res.Intervals, intervalRow(iv.Sum))
			}
			continue
		}
		// Forward: the interval sum when it really is the forward row;
		// otherwise re-derived from the per-stream rows (3.7-3.11 emit two
		// "sum" keys per interval and Go keeps the last, reverse one).
		if iv.Sum != nil && iv.Sum.Sender {
			res.Intervals = append(res.Intervals, intervalRow(iv.Sum))
		} else if row := deriveIntervalRow(iv.Streams, true); row != nil {
			res.Intervals = append(res.Intervals, *row)
		} else {
			res.Intervals = append(res.Intervals, missingIntervalRow(iv))
		}
	}
	res.IntervalMinBps, res.IntervalMaxBps, res.IntervalAvgBps, res.IntervalCoV, res.Collapses =
		analyzeIntervals(res.Intervals)

	switch {
	case bidir:
		b := &BidirStats{}
		for _, st := range w.End.Streams {
			direction := streamDirRow(st)
			if direction == nil {
				continue
			}
			dir := &b.LocalToPeer
			if !direction.Sender {
				dir = &b.PeerToLocal
			}
			if st.Sender != nil {
				dir.Bytes += st.Sender.Bytes
				dir.BitsPerSecond += st.Sender.BitsPerSecond
				if st.Sender.Retransmits != nil {
					addRetransmits(&dir.Retransmits, *st.Sender.Retransmits)
				}
			} else {
				// Keep receiver-only output useful on iperf3 variants that omit
				// the sender row entirely.
				dir.Bytes += st.Receiver.Bytes
				dir.BitsPerSecond += st.Receiver.BitsPerSecond
			}
			if st.Receiver != nil {
				dir.ReceiverBitsPerSecond += st.Receiver.BitsPerSecond
			}
		}
		res.Bidir = b
	case res.Protocol == "UDP":
		if s := w.End.Sum; s != nil {
			udp := &UDPStats{
				ActualBps:   s.BitsPerSecond,
				JitterMs:    s.JitterMs,
				Lost:        s.LostPackets,
				Total:       s.Packets,
				LostPercent: s.LostPercent,
				OutOfOrder:  copyI64(s.OutOfOrder),
			}
			if udp.OutOfOrder == nil {
				udp.OutOfOrder = sumOutOfOrder(w.End.Streams)
			}
			res.UDP = udp
		}
	default: // one-way TCP
		res.Sent = dirStats(w.End.SumSent)
		res.Received = dirStats(w.End.SumReceived)
	}

	return res, nil
}

// streamDirRow returns the row that speaks for an end stream: the sender row
// when present, else the receiver row. Both carry the stream's direction in
// the "sender" boolean (true = local-to-peer), so the survivor is usable for
// both direction attribution and byte counts.
func streamDirRow(st iperfEndStream) *iperfSum {
	if st.Sender != nil {
		return st.Sender
	}
	return st.Receiver
}

// intervalRow converts a wire interval sum row to an IntervalStat.
func intervalRow(s *iperfSum) IntervalStat {
	return IntervalStat{
		StartSec:    s.Start,
		EndSec:      s.End,
		Bytes:       s.Bytes,
		Bps:         s.BitsPerSecond,
		Retransmits: copyU64(s.Retransmits),
	}
}

// deriveIntervalRow aggregates the per-stream rows of one direction of a
// bidir interval into a synthetic sum row; nil when the interval has no rows
// for that direction (sender true = forward, the only direction the result
// carries). Retransmits stay nil unless at least one stream row reports
// them — absent is not zero.
func deriveIntervalRow(streams []iperfSum, sender bool) *IntervalStat {
	var out *IntervalStat
	for i := range streams {
		s := &streams[i]
		if s.Sender != sender {
			continue
		}
		if out == nil {
			out = &IntervalStat{StartSec: s.Start, EndSec: s.End}
		}
		out.Bytes += s.Bytes
		out.Bps += s.BitsPerSecond
		if s.Retransmits != nil {
			addRetransmits(&out.Retransmits, *s.Retransmits)
		}
	}
	return out
}

// missingIntervalRow records a gap in the forward bidirectional series. Its
// bounds come from any surviving reverse row so variation and collapse
// analysis sees a zero sample at the right point in time instead of silently
// shifting later samples earlier.
func missingIntervalRow(iv iperfInterval) IntervalStat {
	var timing *iperfSum
	switch {
	case iv.Sum != nil:
		timing = iv.Sum
	case iv.SumBidirReverse != nil:
		timing = iv.SumBidirReverse
	case len(iv.Streams) > 0:
		timing = &iv.Streams[0]
	}
	if timing == nil {
		return IntervalStat{}
	}
	return IntervalStat{StartSec: timing.Start, EndSec: timing.End}
}

// sumOutOfOrder totals the per-stream UDP out_of_order counters — iperf3
// emits the counter only on end.streams[].udp, not on end.sum. It returns
// nil when no stream reports one, keeping absent distinguishable from zero.
func sumOutOfOrder(streams []iperfEndStream) *int64 {
	var out *int64
	for _, st := range streams {
		if st.UDP == nil || st.UDP.OutOfOrder == nil {
			continue
		}
		if out == nil {
			out = new(int64)
		}
		*out += *st.UDP.OutOfOrder
	}
	return out
}

// dirStats converts a wire sum row to DirStats; nil in, nil out.
func dirStats(s *iperfSum) *DirStats {
	if s == nil {
		return nil
	}
	return &DirStats{
		Bytes:         s.Bytes,
		BitsPerSecond: s.BitsPerSecond,
		Retransmits:   copyU64(s.Retransmits),
	}
}

// analyzeIntervals computes min/max/avg/CoV and collapse events over the
// interval rows, excluding the first (slow-start) interval whenever at least
// two rows exist.
func analyzeIntervals(rows []IntervalStat) (minBps, maxBps, avgBps, cov float64, collapses []CollapseEvent) {
	set := rows
	if len(rows) >= 2 {
		set = rows[1:]
	}
	if len(set) == 0 {
		return 0, 0, 0, 0, nil
	}

	minBps, maxBps = set[0].Bps, set[0].Bps
	sum := 0.0
	for _, r := range set {
		minBps = math.Min(minBps, r.Bps)
		maxBps = math.Max(maxBps, r.Bps)
		sum += r.Bps
	}
	avgBps = sum / float64(len(set))
	if avgBps > 0 {
		varSum := 0.0
		for _, r := range set {
			d := r.Bps - avgBps
			varSum += d * d
		}
		cov = math.Sqrt(varSum/float64(len(set))) / avgBps
	}

	med := medianBps(set)
	if med <= 0 {
		return minBps, maxBps, avgBps, cov, nil
	}
	threshold := 0.10 * med
	var cur *CollapseEvent
	for i, r := range set {
		if r.Bps >= threshold {
			cur = nil
			continue
		}
		if cur == nil || set[i-1].Bps >= threshold {
			collapses = append(collapses, CollapseEvent{StartSec: r.StartSec, Len: 0, MinBps: r.Bps})
			cur = &collapses[len(collapses)-1]
		}
		cur.Len++
		cur.MinBps = math.Min(cur.MinBps, r.Bps)
	}
	return minBps, maxBps, avgBps, cov, collapses
}

// medianBps returns the median interval throughput (mean of the middle pair
// for even counts).
func medianBps(rows []IntervalStat) float64 {
	vals := make([]float64, len(rows))
	for i, r := range rows {
		vals[i] = r.Bps
	}
	slices.Sort(vals)
	n := len(vals)
	if n%2 == 1 {
		return vals[n/2]
	}
	return (vals[n/2-1] + vals[n/2]) / 2
}

// addRetransmits accumulates into a possibly-nil retransmit counter,
// materializing it on first use so absent stays distinguishable from zero.
func addRetransmits(dst **uint64, v uint64) {
	if *dst == nil {
		*dst = new(uint64)
	}
	**dst += v
}

// copyU64 clones an optional uint64 so results do not alias wire memory.
func copyU64(p *uint64) *uint64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// copyI64 clones an optional int64 so results do not alias wire memory.
func copyI64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
