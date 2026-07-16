package parser

import (
	"errors"
	"strings"
	"testing"
)

func TestIperf3TCPVersions(t *testing.T) {
	// Both fixtures encode the same 10-second, 4-stream run; 3.16 adds fields
	// unknown to the wire structs (rtt, pmtu, snd_wnd, bidir, ...) which the
	// parser must silently ignore.
	for _, tc := range []struct {
		name, file, version string
	}{
		{"v3.9", "tcp_39_fwd.json", "iperf 3.9"},
		{"v3.16_unknown_fields", "tcp_316_fwd.json", "iperf 3.16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseIperf3(fixture(t, "iperf", tc.file))
			if err != nil {
				t.Fatalf("ParseIperf3: %v", err)
			}
			if res.Version != tc.version {
				t.Errorf("Version = %q, want %q", res.Version, tc.version)
			}
			if res.Protocol != "TCP" {
				t.Errorf("Protocol = %q, want TCP", res.Protocol)
			}
			if res.Streams != 4 {
				t.Errorf("Streams = %d, want 4", res.Streams)
			}
			if res.DurationSec != 10 {
				t.Errorf("DurationSec = %v, want 10", res.DurationSec)
			}
			if res.Sent == nil {
				t.Fatal("Sent = nil, want end.sum_sent")
			}
			if res.Sent.BitsPerSecond != 944e6 {
				t.Errorf("Sent.BitsPerSecond = %v, want 944e6", res.Sent.BitsPerSecond)
			}
			if res.Sent.Retransmits == nil || *res.Sent.Retransmits != 12 {
				t.Errorf("Sent.Retransmits = %v, want 12", res.Sent.Retransmits)
			}
			if res.Received == nil {
				t.Fatal("Received = nil, want end.sum_received")
			}
			if res.Received.BitsPerSecond != 943056000 {
				t.Errorf("Received.BitsPerSecond = %v, want 943056000", res.Received.BitsPerSecond)
			}
			if res.Received.Retransmits != nil {
				t.Errorf("Received.Retransmits = %d, want nil — absent is not zero", *res.Received.Retransmits)
			}
			if len(res.Intervals) != 10 {
				t.Fatalf("len(Intervals) = %d, want 10", len(res.Intervals))
			}
			if res.Intervals[0].Bps != 900e6 {
				t.Errorf("Intervals[0].Bps = %v, want 900e6 (slow start)", res.Intervals[0].Bps)
			}
			if res.Intervals[3].Retransmits == nil || *res.Intervals[3].Retransmits != 7 {
				t.Errorf("Intervals[3].Retransmits = %v, want 7", res.Intervals[3].Retransmits)
			}
			// Interval statistics exclude the first (slow-start) interval.
			if res.IntervalMinBps != 940e6 {
				t.Errorf("IntervalMinBps = %v, want 940e6 (first interval excluded)", res.IntervalMinBps)
			}
			if res.IntervalMaxBps != 955e6 {
				t.Errorf("IntervalMaxBps = %v, want 955e6", res.IntervalMaxBps)
			}
			if !near(res.IntervalAvgBps, 948888888.8888888, 1) {
				t.Errorf("IntervalAvgBps = %v, want ~948888888.9", res.IntervalAvgBps)
			}
			if !near(res.IntervalCoV, 0.0045108572094281554, 1e-9) {
				t.Errorf("IntervalCoV = %v, want ~0.00451086", res.IntervalCoV)
			}
			if len(res.Collapses) != 0 {
				t.Errorf("Collapses = %+v, want none", res.Collapses)
			}
			if res.CPU == nil || res.CPU.HostTotal != 8.42 || res.CPU.RemoteTotal != 11.27 {
				t.Errorf("CPU = %+v, want host_total 8.42 remote_total 11.27", res.CPU)
			}
			if res.CongSender != "cubic" || res.CongReceiver != "cubic" {
				t.Errorf("congestion = %q/%q, want cubic/cubic", res.CongSender, res.CongReceiver)
			}
			if res.Error != "" {
				t.Errorf("Error = %q, want empty", res.Error)
			}
		})
	}

	t.Run("collapse_and_retransmit_burst", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "tcp_collapse_retr.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if len(res.Intervals) != 12 {
			t.Fatalf("len(Intervals) = %d, want 12", len(res.Intervals))
		}
		if res.Sent == nil || res.Sent.Retransmits == nil || *res.Sent.Retransmits != 202 {
			t.Fatalf("Sent.Retransmits = %+v, want 202", res.Sent)
		}
		if len(res.Collapses) != 1 {
			t.Fatalf("Collapses = %+v, want exactly one (interval < 10%% of median)", res.Collapses)
		}
		c := res.Collapses[0]
		if c.StartSec != 5 || c.Len != 1 || c.MinBps != 4.2e6 {
			t.Errorf("Collapse = %+v, want {StartSec:5 Len:1 MinBps:4.2e6}", c)
		}
		if res.IntervalMinBps != 4.2e6 {
			t.Errorf("IntervalMinBps = %v, want 4.2e6", res.IntervalMinBps)
		}
		if !near(res.IntervalCoV, 0.3146883378170435, 1e-9) {
			t.Errorf("IntervalCoV = %v, want ~0.3147", res.IntervalCoV)
		}
	})
}

func TestIperf3Bidir(t *testing.T) {
	t.Run("v3.14_sum_bidir_reverse", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "bidir_314.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Version != "iperf 3.14" {
			t.Errorf("Version = %q, want iperf 3.14", res.Version)
		}
		if res.Protocol != "TCP" {
			t.Errorf("Protocol = %q, want TCP", res.Protocol)
		}
		if res.Bidir == nil {
			t.Fatal("Bidir = nil, want per-direction totals derived from end.streams[]")
		}
		l2p, p2l := res.Bidir.LocalToPeer, res.Bidir.PeerToLocal
		if l2p.Bytes != 580625000 || l2p.BitsPerSecond != 929000000 {
			t.Errorf("LocalToPeer = {Bytes:%d Bps:%v}, want {580625000 929000000} (the sender:true stream)",
				l2p.Bytes, l2p.BitsPerSecond)
		}
		if l2p.Retransmits == nil || *l2p.Retransmits != 1 {
			t.Errorf("LocalToPeer.Retransmits = %v, want 1", l2p.Retransmits)
		}
		if p2l.Bytes != 575750000 || p2l.BitsPerSecond != 921200000 {
			t.Errorf("PeerToLocal = {Bytes:%d Bps:%v}, want {575750000 921200000} (the sender:false stream)",
				p2l.Bytes, p2l.BitsPerSecond)
		}
		if p2l.Retransmits == nil || *p2l.Retransmits != 2 {
			t.Errorf("PeerToLocal.Retransmits = %v, want 2", p2l.Retransmits)
		}
		if res.Sent != nil || res.Received != nil {
			t.Errorf("Sent/Received = %+v/%+v, want nil/nil — bidir must ignore top-level end sums",
				res.Sent, res.Received)
		}
		if len(res.Intervals) != 5 {
			t.Fatalf("len(Intervals) = %d, want 5 forward rows", len(res.Intervals))
		}
		if res.Intervals[0].Bps != 880e6 || res.Intervals[2].Bps != 941e6 {
			t.Errorf("Intervals[0]/[2].Bps = %v/%v, want 880e6/941e6 (forward sum rows)",
				res.Intervals[0].Bps, res.Intervals[2].Bps)
		}
		if res.Intervals[0].Retransmits == nil || *res.Intervals[0].Retransmits != 1 {
			t.Errorf("Intervals[0].Retransmits = %v, want 1", res.Intervals[0].Retransmits)
		}
		if len(res.IntervalsReverse) != 5 {
			t.Fatalf("len(IntervalsReverse) = %d, want 5 rows from sum_bidir_reverse", len(res.IntervalsReverse))
		}
		if res.IntervalsReverse[0].Bps != 850e6 || res.IntervalsReverse[3].Bps != 940e6 {
			t.Errorf("IntervalsReverse[0]/[3].Bps = %v/%v, want 850e6/940e6",
				res.IntervalsReverse[0].Bps, res.IntervalsReverse[3].Bps)
		}
		if res.IntervalsReverse[0].Retransmits != nil {
			t.Errorf("IntervalsReverse[0].Retransmits = %d, want nil — the receiver view has none",
				*res.IntervalsReverse[0].Retransmits)
		}
		// Interval statistics cover the forward series only, first row excluded.
		if res.IntervalMinBps != 941e6 || res.IntervalMaxBps != 942e6 {
			t.Errorf("IntervalMin/MaxBps = %v/%v, want 941e6/942e6", res.IntervalMinBps, res.IntervalMaxBps)
		}
		if len(res.Collapses) != 0 {
			t.Errorf("Collapses = %+v, want none", res.Collapses)
		}
	})

	t.Run("v3.9_duplicate_sum_keys", func(t *testing.T) {
		// 3.7-3.11 emit the bidir sums as DUPLICATE JSON keys (two "sum" per
		// interval, two "sum_sent"/"sum_received" in end); Go keeps the last —
		// reverse — one. The fixture's surviving top-level sums carry the
		// reverse direction's values, so any code path that trusts them
		// instead of end.streams[] swaps the directions and fails below.
		res, err := ParseIperf3(fixture(t, "iperf", "bidir_39.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Version != "iperf 3.9" {
			t.Errorf("Version = %q, want iperf 3.9", res.Version)
		}
		if res.Streams != 2 {
			t.Errorf("Streams = %d, want 2", res.Streams)
		}
		if res.Bidir == nil {
			t.Fatal("Bidir = nil, want per-direction totals derived from end.streams[]")
		}
		l2p, p2l := res.Bidir.LocalToPeer, res.Bidir.PeerToLocal
		if l2p.Bytes != 287500000 || l2p.BitsPerSecond != 460000000 {
			t.Errorf("LocalToPeer = {Bytes:%d Bps:%v}, want {287500000 460000000} — surviving sum_sent holds the reverse totals",
				l2p.Bytes, l2p.BitsPerSecond)
		}
		if l2p.Retransmits == nil || *l2p.Retransmits != 2 {
			t.Errorf("LocalToPeer.Retransmits = %v, want 2 (summed over sender:true streams)", l2p.Retransmits)
		}
		if p2l.Bytes != 286000000 || p2l.BitsPerSecond != 457600000 {
			t.Errorf("PeerToLocal = {Bytes:%d Bps:%v}, want {286000000 457600000}", p2l.Bytes, p2l.BitsPerSecond)
		}
		if p2l.Retransmits == nil || *p2l.Retransmits != 5 {
			t.Errorf("PeerToLocal.Retransmits = %v, want 5 (4+1 over sender:false streams)", p2l.Retransmits)
		}
		if res.Sent != nil || res.Received != nil {
			t.Errorf("Sent/Received = %+v/%+v, want nil/nil — bidir must ignore top-level end sums",
				res.Sent, res.Received)
		}
		// The surviving interval "sum" is the reverse row, so the forward
		// series must be re-derived from the per-stream rows.
		if len(res.Intervals) != 5 {
			t.Fatalf("len(Intervals) = %d, want 5 forward rows derived from streams", len(res.Intervals))
		}
		if res.Intervals[0].Bps != 420e6 || res.Intervals[0].Bytes != 52500000 {
			t.Errorf("Intervals[0] = {Bps:%v Bytes:%d}, want {420e6 52500000} (2×210e6 sender:true rows)",
				res.Intervals[0].Bps, res.Intervals[0].Bytes)
		}
		if res.Intervals[0].Retransmits == nil || *res.Intervals[0].Retransmits != 2 {
			t.Errorf("Intervals[0].Retransmits = %v, want 2", res.Intervals[0].Retransmits)
		}
		if res.Intervals[1].Bps != 470e6 {
			t.Errorf("Intervals[1].Bps = %v, want 470e6", res.Intervals[1].Bps)
		}
		if res.Intervals[1].Retransmits == nil || *res.Intervals[1].Retransmits != 0 {
			t.Errorf("Intervals[1].Retransmits = %v, want 0 (present on sender rows)", res.Intervals[1].Retransmits)
		}
		if len(res.IntervalsReverse) != 5 {
			t.Fatalf("len(IntervalsReverse) = %d, want 5 rows derived from sender:false streams", len(res.IntervalsReverse))
		}
		if res.IntervalsReverse[0].Bps != 416e6 || res.IntervalsReverse[0].Bytes != 52000000 {
			t.Errorf("IntervalsReverse[0] = {Bps:%v Bytes:%d}, want {416e6 52000000}",
				res.IntervalsReverse[0].Bps, res.IntervalsReverse[0].Bytes)
		}
		if res.IntervalsReverse[1].Bps != 468e6 {
			t.Errorf("IntervalsReverse[1].Bps = %v, want 468e6", res.IntervalsReverse[1].Bps)
		}
		if res.IntervalsReverse[0].Retransmits != nil {
			t.Errorf("IntervalsReverse[0].Retransmits = %d, want nil — receive-side rows have none",
				*res.IntervalsReverse[0].Retransmits)
		}
		if res.IntervalMinBps != 470e6 || res.IntervalMaxBps != 470e6 {
			t.Errorf("IntervalMin/MaxBps = %v/%v, want 470e6/470e6", res.IntervalMinBps, res.IntervalMaxBps)
		}
	})
}

func TestIperf3UDP(t *testing.T) {
	t.Run("v3.9_loss_out_of_order", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "udp_39_loss.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Version != "iperf 3.9" {
			t.Errorf("Version = %q, want iperf 3.9", res.Version)
		}
		if res.Protocol != "UDP" {
			t.Errorf("Protocol = %q, want UDP", res.Protocol)
		}
		if res.UDP == nil {
			t.Fatal("UDP = nil, want end.sum stats")
		}
		if res.UDP.ActualBps != 200192000 {
			t.Errorf("UDP.ActualBps = %v, want 200192000", res.UDP.ActualBps)
		}
		if res.UDP.JitterMs != 0.041 {
			t.Errorf("UDP.JitterMs = %v, want 0.041", res.UDP.JitterMs)
		}
		if res.UDP.Lost != 412 || res.UDP.Total != 170000 {
			t.Errorf("UDP.Lost/Total = %d/%d, want 412/170000", res.UDP.Lost, res.UDP.Total)
		}
		if !near(res.UDP.LostPercent, 0.24235294117647058, 1e-12) {
			t.Errorf("UDP.LostPercent = %v, want ~0.2424", res.UDP.LostPercent)
		}
		if res.UDP.OutOfOrder == nil || *res.UDP.OutOfOrder != 3 {
			t.Errorf("UDP.OutOfOrder = %v, want 3 (from end.sum)", res.UDP.OutOfOrder)
		}
		if res.UDP.TargetBps != 0 {
			t.Errorf("UDP.TargetBps = %d, want 0 (caller-filled, not parsed)", res.UDP.TargetBps)
		}
		if res.Sent != nil || res.Received != nil || res.Bidir != nil {
			t.Errorf("Sent/Received/Bidir = %+v/%+v/%+v, want all nil for UDP", res.Sent, res.Received, res.Bidir)
		}
		if len(res.Intervals) != 10 {
			t.Fatalf("len(Intervals) = %d, want 10", len(res.Intervals))
		}
		if res.Intervals[4].Bps != 200192000 {
			t.Errorf("Intervals[4].Bps = %v, want 200192000", res.Intervals[4].Bps)
		}
		if res.Intervals[4].Retransmits != nil {
			t.Errorf("Intervals[4].Retransmits = %d, want nil — UDP never has retransmits", *res.Intervals[4].Retransmits)
		}
	})

	t.Run("v3.16_no_out_of_order", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "udp_316.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Version != "iperf 3.16" {
			t.Errorf("Version = %q, want iperf 3.16", res.Version)
		}
		if res.UDP == nil {
			t.Fatal("UDP = nil, want end.sum stats")
		}
		if res.UDP.OutOfOrder != nil {
			t.Errorf("UDP.OutOfOrder = %d, want nil — absent is not zero", *res.UDP.OutOfOrder)
		}
		if res.UDP.JitterMs != 0.018 {
			t.Errorf("UDP.JitterMs = %v, want 0.018", res.UDP.JitterMs)
		}
		if res.UDP.Lost != 0 || res.UDP.Total != 85000 || res.UDP.LostPercent != 0 {
			t.Errorf("UDP loss = %d/%d (%v%%), want 0/85000 (0%%)",
				res.UDP.Lost, res.UDP.Total, res.UDP.LostPercent)
		}
		if res.UDP.ActualBps != 100096000 {
			t.Errorf("UDP.ActualBps = %v, want 100096000", res.UDP.ActualBps)
		}
	})

	t.Run("out_of_order_summed_from_streams", func(t *testing.T) {
		// Some versions emit out_of_order only on end.streams[].udp, not on
		// end.sum: the parser must total the stream counters, not report nil.
		doc := []byte(`{
			"start": {"version": "iperf 3.9", "test_start": {"protocol": "UDP", "num_streams": 2, "duration": 10}},
			"intervals": [],
			"end": {
				"streams": [
					{"udp": {"bytes": 125120000, "bits_per_second": 100096000, "jitter_ms": 0.021, "lost_packets": 3, "packets": 85000, "lost_percent": 0.0035, "out_of_order": 2, "sender": true}},
					{"udp": {"bytes": 125120000, "bits_per_second": 100096000, "jitter_ms": 0.033, "lost_packets": 4, "packets": 85000, "lost_percent": 0.0047, "out_of_order": 1, "sender": true}}
				],
				"sum": {"bytes": 250240000, "bits_per_second": 200192000, "jitter_ms": 0.033, "lost_packets": 7, "packets": 170000, "lost_percent": 0.0041, "sender": true}
			}
		}`)
		res, err := ParseIperf3(doc)
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.UDP == nil {
			t.Fatal("UDP = nil, want end.sum stats")
		}
		if res.UDP.OutOfOrder == nil || *res.UDP.OutOfOrder != 3 {
			t.Errorf("UDP.OutOfOrder = %v, want 3 (2+1 summed from end.streams[].udp)", res.UDP.OutOfOrder)
		}
		if res.UDP.Lost != 7 || res.UDP.Total != 170000 {
			t.Errorf("UDP.Lost/Total = %d/%d, want 7/170000 (from end.sum)", res.UDP.Lost, res.UDP.Total)
		}
	})
}

func TestIperf3ErrorObject(t *testing.T) {
	res, err := ParseIperf3(fixture(t, "iperf", "client_error_refused.stdout"))
	if !errors.Is(err, ErrIperfClient) {
		t.Fatalf("err = %v, want errors.Is(err, ErrIperfClient)", err)
	}
	const wantMsg = "unable to connect to server: Connection refused"
	if res.Error != wantMsg {
		t.Errorf("Error = %q, want %q", res.Error, wantMsg)
	}
	if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("err.Error() = %q, want it to carry the client message", err)
	}
	// The JSON itself decoded fine: fields around the error are preserved.
	if res.Version != "iperf 3.16" {
		t.Errorf("Version = %q, want iperf 3.16 (decode success despite error field)", res.Version)
	}
}

func TestIperf3Truncated(t *testing.T) {
	raw := fixture(t, "iperf", "tcp_39_fwd.json")[:300]
	_, err := ParseIperf3(raw)
	if err == nil {
		t.Fatal("truncated JSON accepted, want error")
	}
	var dec *DecodeError
	if !errors.As(err, &dec) {
		t.Fatalf("err = %T %v, want *DecodeError", err, err)
	}
	if dec.Prefix != string(raw[:256]) {
		t.Errorf("Prefix = %q, want the first 256 raw bytes", dec.Prefix)
	}
	if !strings.Contains(err.Error(), string(raw[:32])) {
		t.Errorf("err.Error() = %q, want it to carry the raw prefix", err)
	}

	t.Run("short_input", func(t *testing.T) {
		_, err := ParseIperf3([]byte("oops"))
		var dec *DecodeError
		if !errors.As(err, &dec) {
			t.Fatalf("err = %T %v, want *DecodeError", err, err)
		}
		if dec.Prefix != "oops" {
			t.Errorf("Prefix = %q, want the whole short input", dec.Prefix)
		}
	})
}
