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

// TestIperf3UDP pins the UDP normalization: jitter/lost/total/percent come
// from end.sum (server-observed, preferred over the per-stream rows, whose
// values deliberately differ in the fixture), while out_of_order — which real
// iperf3 emits only on end.streams[].udp — is aggregated from the streams as
// the fallback and stays nil when no row reports one (absent is not zero).
func TestIperf3UDP(t *testing.T) {
	t.Run("v3.9_loss_with_out_of_order", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "udp_39_loss.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.Protocol != "UDP" {
			t.Fatalf("Protocol = %q, want UDP", res.Protocol)
		}
		if res.UDP == nil {
			t.Fatal("UDP = nil, want end.sum stats")
		}
		if res.Sent != nil || res.Received != nil || res.Bidir != nil {
			t.Errorf("Sent/Received/Bidir = %v/%v/%v, want all nil for UDP", res.Sent, res.Received, res.Bidir)
		}
		// end.sum is preferred: the stream row carries 0.043/410/0.241, the
		// sum 0.041/412/0.24 — a streams-first parser fails here.
		if res.UDP.JitterMs != 0.041 {
			t.Errorf("JitterMs = %v, want 0.041 from end.sum (not 0.043 from the stream row)", res.UDP.JitterMs)
		}
		if res.UDP.Lost != 412 {
			t.Errorf("Lost = %d, want 412 from end.sum (not 410 from the stream row)", res.UDP.Lost)
		}
		if res.UDP.Total != 170000 {
			t.Errorf("Total = %d, want 170000", res.UDP.Total)
		}
		if res.UDP.LostPercent != 0.24 {
			t.Errorf("LostPercent = %v, want 0.24", res.UDP.LostPercent)
		}
		if res.UDP.ActualBps != 100096000.0 {
			t.Errorf("ActualBps = %v, want 100096000", res.UDP.ActualBps)
		}
		// out_of_order lives only on the stream rows in real iperf3 output:
		// the parser must fall back to summing them.
		if res.UDP.OutOfOrder == nil {
			t.Fatal("OutOfOrder = nil, want 3 aggregated from end.streams[].udp")
		}
		if *res.UDP.OutOfOrder != 3 {
			t.Errorf("OutOfOrder = %d, want 3", *res.UDP.OutOfOrder)
		}
		if res.CPU == nil || res.CPU.HostTotal != 5.104061 {
			t.Errorf("CPU = %+v, want host_total 5.104061", res.CPU)
		}
	})

	t.Run("v3.16_no_out_of_order_anywhere", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "udp_316.json"))
		if err != nil {
			t.Fatalf("ParseIperf3: %v", err)
		}
		if res.UDP == nil {
			t.Fatal("UDP = nil, want end.sum stats")
		}
		if res.UDP.OutOfOrder != nil {
			t.Errorf("OutOfOrder = %d, want nil — absent is not zero", *res.UDP.OutOfOrder)
		}
		if res.UDP.Lost != 0 || res.UDP.Total != 135862 || res.UDP.LostPercent != 0 {
			t.Errorf("loss = %d/%d (%v%%), want 0/135862 (0%%)", res.UDP.Lost, res.UDP.Total, res.UDP.LostPercent)
		}
		if res.UDP.JitterMs != 0.015 {
			t.Errorf("JitterMs = %v, want 0.015", res.UDP.JitterMs)
		}
	})
}

// TestIperf3BidirFromStreams pins the bidir decision across both JSON
// generations: per-direction totals always come from end.streams[] sender
// flags — never the top-level sums, which the 3.9 fixture deliberately makes
// inconsistent (the 3.7-3.11 duplicate-key bug) — and the forward interval
// series survives both the duplicate-"sum" shape (3.9, re-derived from the
// stream rows) and the sum/sum_bidir_reverse shape (3.14).
func TestIperf3BidirFromStreams(t *testing.T) {
	for _, tc := range []struct {
		name, file, version string
	}{
		{"v3.9_duplicate_sum_keys", "bidir_39.json", "iperf 3.9"},
		{"v3.14_sum_bidir_reverse", "bidir_314.json", "iperf 3.14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseIperf3(fixture(t, "iperf", tc.file))
			if err != nil {
				t.Fatalf("ParseIperf3: %v", err)
			}
			if res.Version != tc.version {
				t.Errorf("Version = %q, want %q", res.Version, tc.version)
			}
			if res.Bidir == nil {
				t.Fatal("Bidir = nil, want per-direction totals from end.streams[]")
			}
			if res.Sent != nil || res.Received != nil {
				t.Errorf("Sent/Received = %v/%v, want nil in bidir mode (top-level sums are buggy 3.7-3.11)",
					res.Sent, res.Received)
			}
			fwd, rev := res.Bidir.LocalToPeer, res.Bidir.PeerToLocal
			// The fixture's top-level sum_sent says 1828e6 — a parser reading
			// it lands far from the per-stream truth.
			if fwd.BitsPerSecond != 941e6 {
				t.Errorf("LocalToPeer.BitsPerSecond = %v, want 941e6 from the sender-flagged stream", fwd.BitsPerSecond)
			}
			if fwd.ReceiverBitsPerSecond != 940.8e6 {
				t.Errorf("LocalToPeer.ReceiverBitsPerSecond = %v, want 940.8e6 from the receiver row", fwd.ReceiverBitsPerSecond)
			}
			if fwd.Bytes != 1176250000 {
				t.Errorf("LocalToPeer.Bytes = %d, want 1176250000", fwd.Bytes)
			}
			if fwd.Retransmits == nil || *fwd.Retransmits != 17 {
				t.Errorf("LocalToPeer.Retransmits = %v, want 17", fwd.Retransmits)
			}
			if rev.BitsPerSecond != 887e6 {
				t.Errorf("PeerToLocal.BitsPerSecond = %v, want 887e6 from the reverse stream", rev.BitsPerSecond)
			}
			if rev.ReceiverBitsPerSecond != 886.4e6 {
				t.Errorf("PeerToLocal.ReceiverBitsPerSecond = %v, want 886.4e6 from the reverse receiver row", rev.ReceiverBitsPerSecond)
			}
			if rev.Bytes != 1108750000 {
				t.Errorf("PeerToLocal.Bytes = %d, want 1108750000", rev.Bytes)
			}
			if rev.Retransmits == nil || *rev.Retransmits != 4 {
				t.Errorf("PeerToLocal.Retransmits = %v, want 4", rev.Retransmits)
			}
			// Forward interval series: 10 rows at the forward rate. In the 3.9
			// shape the surviving duplicate "sum" is the reverse one, so the
			// forward row must have been re-derived from the stream rows.
			if len(res.Intervals) != 10 {
				t.Fatalf("len(Intervals) = %d, want 10", len(res.Intervals))
			}
			for i, iv := range res.Intervals {
				if iv.Bps != 941e6 {
					t.Fatalf("Intervals[%d].Bps = %v, want the forward 941e6 (not the reverse 887e6)", i, iv.Bps)
				}
			}
			if res.Intervals[2].Retransmits == nil || *res.Intervals[2].Retransmits != 1 {
				t.Errorf("Intervals[2].Retransmits = %v, want 1", res.Intervals[2].Retransmits)
			}
			if res.CongSender != "cubic" {
				t.Errorf("CongSender = %q, want cubic", res.CongSender)
			}
		})
	}
}

// TestIperf3BidirMissingForwardIntervalKeepsSeriesContiguous covers iperf3
// 3.7-3.11 output where duplicate sum keys leave only the reverse row and no
// forward stream rows survive. The missing forward sample remains visible as
// a zero-rate placeholder instead of shortening and time-shifting the series.
func TestIperf3BidirMissingForwardIntervalKeepsSeriesContiguous(t *testing.T) {
	raw := []byte(`{
		"start":{"version":"iperf 3.9","test_start":{"protocol":"TCP","num_streams":1,"duration":3}},
		"intervals":[
			{"sum":{"start":0,"end":1,"seconds":1,"bytes":100,"bits_per_second":800,"sender":true}},
			{"sum":{"start":1,"end":2,"seconds":1,"bytes":90,"bits_per_second":720,"sender":false}},
			{"sum":{"start":2,"end":3,"seconds":1,"bytes":100,"bits_per_second":800,"sender":true}}
		],
		"end":{"streams":[
			{"sender":{"bytes":300,"bits_per_second":800,"sender":true}},
			{"sender":{"bytes":270,"bits_per_second":720,"sender":false}}
		]}
	}`)
	res, err := ParseIperf3(raw)
	if err != nil {
		t.Fatalf("ParseIperf3: %v", err)
	}
	if len(res.Intervals) != 3 {
		t.Fatalf("len(Intervals) = %d, want 3 contiguous samples", len(res.Intervals))
	}
	gap := res.Intervals[1]
	if gap.StartSec != 1 || gap.EndSec != 2 {
		t.Errorf("missing-forward placeholder bounds = %v-%v, want 1-2", gap.StartSec, gap.EndSec)
	}
	if gap.Bytes != 0 || gap.Bps != 0 || gap.Retransmits != nil {
		t.Errorf("missing-forward placeholder = %+v, want a zero sample with absent retransmits", gap)
	}
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

// TestIperf3Unreachable pins the connect/reachability classification: a
// "unable to connect to server" error wraps BOTH ErrIperfClient and the
// ErrIperfUnreachable subclass (so it can be handled non-fatally), while a
// genuine mid-test client error wraps ErrIperfClient only and must keep
// failing the run.
func TestIperf3Unreachable(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		res, err := ParseIperf3(fixture(t, "iperf", "client_error_refused.stdout"))
		if !errors.Is(err, ErrIperfClient) || !errors.Is(err, ErrIperfUnreachable) {
			t.Fatalf("err = %v, want it to wrap both ErrIperfClient and ErrIperfUnreachable", err)
		}
		if res.Error != "unable to connect to server: Connection refused" {
			t.Errorf("Error = %q, want the full client message preserved", res.Error)
		}
	})
	t.Run("timed_out_proves_firewall_drop", func(t *testing.T) {
		_, err := ParseIperf3(fixture(t, "iperf", "client_error_timedout.stdout"))
		if !errors.Is(err, ErrIperfUnreachable) {
			t.Errorf("err = %v, want errors.Is(err, ErrIperfUnreachable) for a timed-out connect", err)
		}
	})
	t.Run("midtest_error_is_not_unreachable", func(t *testing.T) {
		raw := []byte(`{"start":{"version":"iperf 3.16"},"intervals":[],"end":{},"error":"error - control socket has closed unexpectedly, is the other side up?"}`)
		err := func() error { _, e := ParseIperf3(raw); return e }()
		if !errors.Is(err, ErrIperfClient) {
			t.Fatalf("err = %v, want errors.Is(err, ErrIperfClient)", err)
		}
		if errors.Is(err, ErrIperfUnreachable) {
			t.Errorf("mid-test error wrongly classified as unreachable: %v", err)
		}
	})
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
