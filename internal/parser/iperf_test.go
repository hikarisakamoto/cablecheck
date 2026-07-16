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
	// iperf3 3.9 --bidir fixture: per-stream sender flags carry the stream
	// direction, intervals carry sum (fwd) + sum_bidir_reverse (rev), and the
	// top-level end sums are deliberately duplicated/misattributed (the
	// 3.7-3.11 bug) so any parser that trusts them fails these assertions.
	res, err := ParseIperf3(fixture(t, "iperf", "bidir_39.json"))
	if err != nil {
		t.Fatalf("ParseIperf3: %v", err)
	}
	if res.Version != "iperf 3.9" || res.Protocol != "TCP" {
		t.Errorf("Version/Protocol = %q/%q, want iperf 3.9/TCP", res.Version, res.Protocol)
	}
	if res.Streams != 1 || res.DurationSec != 5 {
		t.Errorf("Streams/DurationSec = %d/%v, want 1/5", res.Streams, res.DurationSec)
	}
	if res.Sent != nil || res.Received != nil {
		t.Errorf("Sent/Received = %+v/%+v, want nil/nil — bidir must ignore the misattributed top-level sums", res.Sent, res.Received)
	}
	if res.UDP != nil {
		t.Errorf("UDP = %+v, want nil", res.UDP)
	}
	if res.Bidir == nil {
		t.Fatal("Bidir = nil, want per-direction totals derived from end.streams[]")
	}
	ltp := res.Bidir.LocalToPeer
	if ltp.Bytes != 586250000 || ltp.BitsPerSecond != 938e6 {
		t.Errorf("LocalToPeer = {%d %v}, want {586250000 938e6}", ltp.Bytes, ltp.BitsPerSecond)
	}
	if ltp.Retransmits == nil || *ltp.Retransmits != 3 {
		t.Errorf("LocalToPeer.Retransmits = %v, want 3", ltp.Retransmits)
	}
	ptl := res.Bidir.PeerToLocal
	if ptl.Bytes != 422500000 || ptl.BitsPerSecond != 676e6 {
		t.Errorf("PeerToLocal = {%d %v}, want {422500000 676e6}", ptl.Bytes, ptl.BitsPerSecond)
	}
	if ptl.Retransmits != nil {
		t.Errorf("PeerToLocal.Retransmits = %d, want nil — absent is not zero", *ptl.Retransmits)
	}

	// Forward interval series comes from intervals[].sum.
	if len(res.Intervals) != 5 {
		t.Fatalf("len(Intervals) = %d, want 5", len(res.Intervals))
	}
	if res.Intervals[0].Bps != 900e6 || res.Intervals[2].Bps != 950e6 {
		t.Errorf("Intervals[0]/[2].Bps = %v/%v, want 900e6/950e6", res.Intervals[0].Bps, res.Intervals[2].Bps)
	}
	if res.Intervals[0].Retransmits == nil || *res.Intervals[0].Retransmits != 3 {
		t.Errorf("Intervals[0].Retransmits = %v, want 3", res.Intervals[0].Retransmits)
	}
	if res.IntervalMinBps != 940e6 || res.IntervalMaxBps != 955e6 || res.IntervalAvgBps != 947.5e6 {
		t.Errorf("fwd min/max/avg = %v/%v/%v, want 940e6/955e6/947.5e6",
			res.IntervalMinBps, res.IntervalMaxBps, res.IntervalAvgBps)
	}
	if !near(res.IntervalCoV, 0.0058999155079150125, 1e-12) {
		t.Errorf("IntervalCoV = %v, want ~0.00589992", res.IntervalCoV)
	}
	if len(res.Collapses) != 0 {
		t.Errorf("Collapses = %+v, want none in the forward direction", res.Collapses)
	}

	// Reverse interval series comes from intervals[].sum_bidir_reverse and
	// gets its own analysis, including collapse detection.
	if len(res.ReverseIntervals) != 5 {
		t.Fatalf("len(ReverseIntervals) = %d, want 5", len(res.ReverseIntervals))
	}
	if res.ReverseIntervals[2].Bps != 30e6 || res.ReverseIntervals[2].Bytes != 3750000 {
		t.Errorf("ReverseIntervals[2] = %+v, want Bps 30e6 Bytes 3750000", res.ReverseIntervals[2])
	}
	if res.ReverseIntervals[0].Retransmits != nil {
		t.Errorf("ReverseIntervals[0].Retransmits = %d, want nil", *res.ReverseIntervals[0].Retransmits)
	}
	if res.ReverseIntervalMinBps != 30e6 || res.ReverseIntervalMaxBps != 860e6 || res.ReverseIntervalAvgBps != 645e6 {
		t.Errorf("rev min/max/avg = %v/%v/%v, want 30e6/860e6/645e6",
			res.ReverseIntervalMinBps, res.ReverseIntervalMaxBps, res.ReverseIntervalAvgBps)
	}
	if !near(res.ReverseIntervalCoV, 0.5506059180489848, 1e-12) {
		t.Errorf("ReverseIntervalCoV = %v, want ~0.550606", res.ReverseIntervalCoV)
	}
	if len(res.ReverseCollapses) != 1 {
		t.Fatalf("ReverseCollapses = %+v, want exactly one", res.ReverseCollapses)
	}
	if c := res.ReverseCollapses[0]; c.StartSec != 2 || c.Len != 1 || c.MinBps != 3e7 {
		t.Errorf("ReverseCollapses[0] = %+v, want {StartSec:2 Len:1 MinBps:3e7}", c)
	}
	if res.CPU == nil || res.CongSender != "cubic" {
		t.Errorf("CPU/CongSender = %+v/%q, want present/cubic", res.CPU, res.CongSender)
	}

	t.Run("receiver_only_stream_reverse_attribution", func(t *testing.T) {
		// A stream that decoded with only a receiver row must be attributed
		// by that row's direction flag, not defaulted to LocalToPeer.
		const doc = `{
			"start": {"version": "iperf 3.9", "test_start": {"protocol": "TCP", "num_streams": 1, "duration": 5, "reverse": 0}},
			"intervals": [],
			"end": {"streams": [
				{"sender": {"start": 0, "end": 5, "seconds": 5, "bytes": 1000, "bits_per_second": 1600, "retransmits": 2, "sender": true},
				 "receiver": {"start": 0, "end": 5, "seconds": 5, "bytes": 990, "bits_per_second": 1584, "sender": true}},
				{"receiver": {"start": 0, "end": 5, "seconds": 5, "bytes": 500, "bits_per_second": 800, "sender": false}}
			]}
		}`
		res, err := ParseIperf3([]byte(doc))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Bidir == nil {
			t.Fatal("Bidir = nil, want bidir detected from the receiver-only reverse stream")
		}
		if res.Bidir.LocalToPeer.Bytes != 1000 {
			t.Errorf("LocalToPeer.Bytes = %d, want 1000", res.Bidir.LocalToPeer.Bytes)
		}
		if res.Bidir.PeerToLocal.Bytes != 500 || res.Bidir.PeerToLocal.BitsPerSecond != 800 {
			t.Errorf("PeerToLocal = %+v, want the receiver-only stream's 500 B / 800 bps", res.Bidir.PeerToLocal)
		}
		if res.Bidir.PeerToLocal.Retransmits != nil {
			t.Errorf("PeerToLocal.Retransmits = %d, want nil", *res.Bidir.PeerToLocal.Retransmits)
		}
	})

	t.Run("duplicate_interval_sum_keys_39", func(t *testing.T) {
		// Real iperf3 3.7-3.11 bidir intervals carry TWO "sum" keys; Go's
		// decoder keeps the last (reverse) one. The forward series must be
		// re-derived from the interval stream rows and the surviving sum
		// used as the reverse row.
		const doc = `{
			"start": {"version": "iperf 3.9", "test_start": {"protocol": "TCP", "num_streams": 1, "duration": 2, "reverse": 0}},
			"intervals": [
				{"streams": [
					{"socket": 5, "start": 0, "end": 1, "seconds": 1, "bytes": 125, "bits_per_second": 1000, "retransmits": 1, "sender": true},
					{"socket": 7, "start": 0, "end": 1, "seconds": 1, "bytes": 50, "bits_per_second": 400, "sender": false}],
				 "sum": {"start": 0, "end": 1, "seconds": 1, "bytes": 125, "bits_per_second": 1000, "retransmits": 1, "sender": true},
				 "sum": {"start": 0, "end": 1, "seconds": 1, "bytes": 50, "bits_per_second": 400, "sender": false}},
				{"streams": [
					{"socket": 5, "start": 1, "end": 2, "seconds": 1, "bytes": 250, "bits_per_second": 2000, "retransmits": 0, "sender": true},
					{"socket": 7, "start": 1, "end": 2, "seconds": 1, "bytes": 75, "bits_per_second": 600, "sender": false}],
				 "sum": {"start": 1, "end": 2, "seconds": 1, "bytes": 250, "bits_per_second": 2000, "retransmits": 0, "sender": true},
				 "sum": {"start": 1, "end": 2, "seconds": 1, "bytes": 75, "bits_per_second": 600, "sender": false}}
			],
			"end": {"streams": [
				{"sender": {"start": 0, "end": 2, "seconds": 2, "bytes": 375, "bits_per_second": 1500, "retransmits": 1, "sender": true},
				 "receiver": {"start": 0, "end": 2, "seconds": 2, "bytes": 370, "bits_per_second": 1480, "sender": true}},
				{"sender": {"start": 0, "end": 2, "seconds": 2, "bytes": 125, "bits_per_second": 500, "sender": false},
				 "receiver": {"start": 0, "end": 2, "seconds": 2, "bytes": 120, "bits_per_second": 480, "sender": false}}
			]}
		}`
		res, err := ParseIperf3([]byte(doc))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Bidir == nil {
			t.Fatal("Bidir = nil, want bidir detected")
		}
		if len(res.Intervals) != 2 {
			t.Fatalf("len(Intervals) = %d, want 2 (forward rows re-derived from streams)", len(res.Intervals))
		}
		if res.Intervals[0].Bps != 1000 || res.Intervals[0].Bytes != 125 {
			t.Errorf("Intervals[0] = %+v, want the forward stream row {Bytes:125 Bps:1000}", res.Intervals[0])
		}
		if res.Intervals[0].Retransmits == nil || *res.Intervals[0].Retransmits != 1 {
			t.Errorf("Intervals[0].Retransmits = %v, want 1 (from the forward stream row)", res.Intervals[0].Retransmits)
		}
		if res.Intervals[1].Bps != 2000 {
			t.Errorf("Intervals[1].Bps = %v, want 2000", res.Intervals[1].Bps)
		}
		if len(res.ReverseIntervals) != 2 {
			t.Fatalf("len(ReverseIntervals) = %d, want 2 (surviving reverse sum rows)", len(res.ReverseIntervals))
		}
		if res.ReverseIntervals[0].Bps != 400 || res.ReverseIntervals[1].Bps != 600 {
			t.Errorf("ReverseIntervals Bps = %v/%v, want 400/600",
				res.ReverseIntervals[0].Bps, res.ReverseIntervals[1].Bps)
		}
	})
}

func TestIperf3UDP(t *testing.T) {
	t.Run("v3.9_loss_out_of_order", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "udp_39_loss.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Version != "iperf 3.9" || res.Protocol != "UDP" {
			t.Errorf("Version/Protocol = %q/%q, want iperf 3.9/UDP", res.Version, res.Protocol)
		}
		if res.Sent != nil || res.Received != nil || res.Bidir != nil {
			t.Errorf("Sent/Received/Bidir = %+v/%+v/%+v, want all nil for UDP", res.Sent, res.Received, res.Bidir)
		}
		if res.UDP == nil {
			t.Fatal("UDP = nil, want end.sum stats")
		}
		if res.UDP.ActualBps != 200192000 {
			t.Errorf("ActualBps = %v, want 200192000", res.UDP.ActualBps)
		}
		if res.UDP.JitterMs != 0.041 {
			t.Errorf("JitterMs = %v, want 0.041", res.UDP.JitterMs)
		}
		if res.UDP.Lost != 412 || res.UDP.Total != 170000 {
			t.Errorf("Lost/Total = %d/%d, want 412/170000", res.UDP.Lost, res.UDP.Total)
		}
		if !near(res.UDP.LostPercent, 0.24235294117647058, 1e-12) {
			t.Errorf("LostPercent = %v, want ~0.2424", res.UDP.LostPercent)
		}
		if res.UDP.OutOfOrder == nil || *res.UDP.OutOfOrder != 3 {
			t.Errorf("OutOfOrder = %v, want 3 (iperf3 reports it only on end.streams[].udp)", res.UDP.OutOfOrder)
		}
		if res.UDP.TargetBps != 0 {
			t.Errorf("TargetBps = %d, want 0 (the caller fills it in)", res.UDP.TargetBps)
		}
		if len(res.Intervals) != 10 {
			t.Fatalf("len(Intervals) = %d, want 10", len(res.Intervals))
		}
		for i, iv := range res.Intervals {
			if iv.Bps != 200192000 {
				t.Errorf("Intervals[%d].Bps = %v, want 200192000", i, iv.Bps)
			}
			if iv.Retransmits != nil {
				t.Errorf("Intervals[%d].Retransmits = %d, want nil — UDP never has retransmits", i, *iv.Retransmits)
			}
		}
		if res.IntervalMinBps != 200192000 || res.IntervalMaxBps != 200192000 || res.IntervalCoV != 0 {
			t.Errorf("interval min/max/CoV = %v/%v/%v, want 200192000/200192000/0",
				res.IntervalMinBps, res.IntervalMaxBps, res.IntervalCoV)
		}
		if len(res.Collapses) != 0 {
			t.Errorf("Collapses = %+v, want none", res.Collapses)
		}
	})

	t.Run("v3.16_no_out_of_order", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "udp_316.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Version != "iperf 3.16" || res.Protocol != "UDP" {
			t.Errorf("Version/Protocol = %q/%q, want iperf 3.16/UDP", res.Version, res.Protocol)
		}
		if res.UDP == nil {
			t.Fatal("UDP = nil, want end.sum stats")
		}
		if res.UDP.OutOfOrder != nil {
			t.Errorf("OutOfOrder = %d, want nil — absent is not zero", *res.UDP.OutOfOrder)
		}
		if res.UDP.JitterMs != 0.012 || res.UDP.Lost != 0 || res.UDP.Total != 85000 || res.UDP.LostPercent != 0 {
			t.Errorf("UDP = %+v, want jitter 0.012, 0/85000 lost, 0%%", res.UDP)
		}
		if len(res.Intervals) != 5 {
			t.Errorf("len(Intervals) = %d, want 5", len(res.Intervals))
		}
	})

	t.Run("end_sum_out_of_order_preferred", func(t *testing.T) {
		// When end.sum itself carries out_of_order, it wins over the
		// per-stream counters.
		const doc = `{
			"start": {"version": "iperf 3.9", "test_start": {"protocol": "UDP", "num_streams": 1, "duration": 1, "reverse": 0}},
			"intervals": [],
			"end": {
				"streams": [{"udp": {"socket": 5, "start": 0, "end": 1, "seconds": 1, "bytes": 1472, "bits_per_second": 11776, "jitter_ms": 0.1, "lost_packets": 0, "packets": 1, "lost_percent": 0, "out_of_order": 1, "sender": true}}],
				"sum": {"start": 0, "end": 1, "seconds": 1, "bytes": 1472, "bits_per_second": 11776, "jitter_ms": 0.1, "lost_packets": 0, "packets": 1, "lost_percent": 0, "out_of_order": 7, "sender": true}
			}
		}`
		res, err := ParseIperf3([]byte(doc))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.UDP == nil || res.UDP.OutOfOrder == nil || *res.UDP.OutOfOrder != 7 {
			t.Errorf("UDP = %+v, want OutOfOrder 7 from end.sum", res.UDP)
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
