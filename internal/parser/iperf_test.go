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
