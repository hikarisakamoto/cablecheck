package parser

import (
	"errors"
	"math"
	"testing"

	"cablecheck/internal/model"
)

const pingHeader = "PING 10.0.0.2 (10.0.0.2) 56(84) bytes of data.\n"

// near reports whether got is within tol of want.
func near(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

func TestPingPerPacket(t *testing.T) {
	t.Run("quick_500_loss_dup", func(t *testing.T) {
		res, err := ParsePing(fixture(t, "ping", "quick_500_loss_dup.txt"), nil, 1)
		if err != nil {
			t.Fatalf("exit 1 with a parsed summary must be a valid result, got error: %v", err)
		}
		if res.Target != "10.0.0.2" {
			t.Errorf("Target = %q, want 10.0.0.2", res.Target)
		}
		if res.Transmitted != 500 || res.Received != 498 {
			t.Errorf("transmitted/received = %d/%d, want 500/498", res.Transmitted, res.Received)
		}
		if res.Duplicates != 1 {
			t.Errorf("Duplicates = %d, want 1 (counted separately from received)", res.Duplicates)
		}
		if !near(res.LossPercent, 0.4, 1e-9) {
			t.Errorf("LossPercent = %v, want 0.4", res.LossPercent)
		}
		wantRuns := []model.SeqRun{{FirstSeq: 137, Len: 2}}
		if len(res.MissingSeqRuns) != 1 || res.MissingSeqRuns[0] != wantRuns[0] {
			t.Errorf("MissingSeqRuns = %+v, want %+v", res.MissingSeqRuns, wantRuns)
		}
		if res.LongestMissingRunLen != 2 {
			t.Errorf("LongestMissingRunLen = %d, want 2", res.LongestMissingRunLen)
		}
		if !near(res.LongestGapMs, 59.96, 0.5) {
			t.Errorf("LongestGapMs = %v, want ~59.96 (three missed 20ms slots)", res.LongestGapMs)
		}
		// Nearest-rank percentiles over FIRST replies only: the 30.155ms DUP
		// must not shift them.
		wantPct := map[int]float64{50: 0.249, 90: 0.274, 95: 0.277, 99: 0.279}
		for p, want := range wantPct {
			if got, ok := res.Percentiles[p]; !ok || !near(got, want, 1e-9) {
				t.Errorf("Percentiles[%d] = %v (present=%v), want %v", p, got, ok, want)
			}
		}
		if len(res.Spikes) != 1 || res.Spikes[0].Seq != 350 || !near(res.Spikes[0].RTTMs, 12.412, 1e-9) {
			t.Errorf("Spikes = %+v, want exactly [{350 12.412}]", res.Spikes)
		}
		if !near(res.RTTMinMs, 0.220, 1e-9) || !near(res.RTTAvgMs, 0.274, 1e-9) ||
			!near(res.RTTMaxMs, 12.412, 1e-9) || !near(res.RTTMdevMs, 0.545, 1e-9) {
			t.Errorf("rtt min/avg/max/mdev = %v/%v/%v/%v, want 0.220/0.274/12.412/0.545",
				res.RTTMinMs, res.RTTAvgMs, res.RTTMaxMs, res.RTTMdevMs)
		}
		if res.ExitCode != 1 {
			t.Errorf("ExitCode = %d, want 1", res.ExitCode)
		}
		if res.IcmpErrors != 0 || res.SendErrors != 0 {
			t.Errorf("IcmpErrors/SendErrors = %d/%d, want 0/0", res.IcmpErrors, res.SendErrors)
		}
		if res.UnparsedLines != 0 {
			t.Errorf("UnparsedLines = %d, want 0", res.UnparsedLines)
		}
	})

	t.Run("quick_clean_100", func(t *testing.T) {
		res, err := ParsePing(fixture(t, "ping", "quick_clean_100.txt"), nil, 0)
		if err != nil {
			t.Fatalf("ParsePing: %v", err)
		}
		if res.Transmitted != 100 || res.Received != 100 {
			t.Errorf("transmitted/received = %d/%d, want 100/100", res.Transmitted, res.Received)
		}
		if res.Duplicates != 0 {
			t.Errorf("Duplicates = %d, want 0", res.Duplicates)
		}
		if res.LossPercent != 0 {
			t.Errorf("LossPercent = %v, want 0", res.LossPercent)
		}
		// RTTs are the exact ramp 0.201..0.300ms, so nearest-rank percentiles
		// are exact golden values.
		wantPct := map[int]float64{50: 0.250, 90: 0.290, 95: 0.295, 99: 0.299}
		for p, want := range wantPct {
			if got, ok := res.Percentiles[p]; !ok || !near(got, want, 1e-9) {
				t.Errorf("Percentiles[%d] = %v (present=%v), want %v", p, got, ok, want)
			}
		}
		if len(res.Spikes) != 0 {
			t.Errorf("Spikes = %+v, want none", res.Spikes)
		}
		if len(res.MissingSeqRuns) != 0 || res.LongestMissingRunLen != 0 {
			t.Errorf("MissingSeqRuns/LongestMissingRunLen = %+v/%d, want none/0",
				res.MissingSeqRuns, res.LongestMissingRunLen)
		}
		if !near(res.LongestGapMs, 20.0, 0.5) {
			t.Errorf("LongestGapMs = %v, want ~20 (steady 20ms cadence)", res.LongestGapMs)
		}
		if res.UnparsedLines != 0 {
			t.Errorf("UnparsedLines = %d, want 0", res.UnparsedLines)
		}
	})

	t.Run("unreachable_errors_summary", func(t *testing.T) {
		res, err := ParsePing(fixture(t, "ping", "unreachable.stdout"), nil, 1)
		if err != nil {
			t.Fatalf("exit 1 with a parsed summary must be a valid result, got error: %v", err)
		}
		if res.Transmitted != 20 || res.Received != 0 {
			t.Errorf("transmitted/received = %d/%d, want 20/0", res.Transmitted, res.Received)
		}
		if res.LossPercent != 100 {
			t.Errorf("LossPercent = %v, want 100", res.LossPercent)
		}
		if res.IcmpErrors != 20 {
			t.Errorf("IcmpErrors = %d, want 20 (From ... Destination Host Unreachable)", res.IcmpErrors)
		}
		if len(res.MissingSeqRuns) != 1 || res.MissingSeqRuns[0] != (model.SeqRun{FirstSeq: 1, Len: 20}) {
			t.Errorf("MissingSeqRuns = %+v, want [{1 20}]", res.MissingSeqRuns)
		}
		if res.LongestMissingRunLen != 20 {
			t.Errorf("LongestMissingRunLen = %d, want 20", res.LongestMissingRunLen)
		}
		if res.UnparsedLines != 0 {
			t.Errorf("UnparsedLines = %d, want 0", res.UnparsedLines)
		}
	})
}

func TestPingSummaryDupsReconciled(t *testing.T) {
	// Reply lines got truncated (output cap): only one DUP line survived but
	// the summary counted three. The summary is authoritative when larger.
	stdout := []byte(pingHeader +
		"[1752580000.000000] 64 bytes from 10.0.0.2: icmp_seq=1 ttl=64 time=0.250 ms\n" +
		"[1752580000.000210] 64 bytes from 10.0.0.2: icmp_seq=1 ttl=64 time=0.260 ms (DUP!)\n" +
		"[1752580000.020000] 64 bytes from 10.0.0.2: icmp_seq=2 ttl=64 time=0.251 ms\n" +
		"\n--- 10.0.0.2 ping statistics ---\n" +
		"5 packets transmitted, 3 received, +3 duplicates, 40% packet loss, time 80ms\n" +
		"rtt min/avg/max/mdev = 0.250/0.254/0.260/0.004 ms\n")
	res, err := ParsePing(stdout, nil, 1)
	if err != nil {
		t.Fatalf("ParsePing: %v", err)
	}
	if res.Duplicates != 3 {
		t.Errorf("Duplicates = %d, want 3 — the summary count wins over truncated reply lines", res.Duplicates)
	}
	if res.Transmitted != 5 || res.Received != 3 {
		t.Errorf("transmitted/received = %d/%d, want 5/3", res.Transmitted, res.Received)
	}
}

func TestPingUnrecognizedOutput(t *testing.T) {
	// No reply lines and no summary line: nothing measurable came back, and
	// silently returning a zero-valued result would fake a clean run.
	_, err := ParsePing([]byte("watchdog: something unrelated\nspeaking a different grammar entirely\n"), nil, 0)
	if err == nil {
		t.Fatal("unrecognized output accepted, want error")
	}
	if errors.Is(err, ErrUnsupportedPingFormat) || errors.Is(err, ErrIntervalRejected) {
		t.Errorf("err = %v, want a plain unrecognized-output error, not a typed sentinel", err)
	}
}

func TestPingIntervalRejected(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
	}{
		{"legacy_semicolon_wording", "interval_denied_old.stderr"},
		{"modern_comma_wording", "interval_denied_new.stderr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := fixture(t, "ping", tc.fixture)
			// The PING header still lands on stdout before the stderr error.
			_, err := ParsePing([]byte(pingHeader), stderr, 2)
			if !errors.Is(err, ErrIntervalRejected) {
				t.Errorf("err = %v, want errors.Is(err, ErrIntervalRejected)", err)
			}
		})
	}

	t.Run("other_exit2_not_interval", func(t *testing.T) {
		_, err := ParsePing(nil, []byte("ping: connect: Network is unreachable\n"), 2)
		if err == nil {
			t.Fatal("exit 2 with unrelated stderr must be an error")
		}
		if errors.Is(err, ErrIntervalRejected) {
			t.Errorf("err = %v, must not be ErrIntervalRejected", err)
		}
	})
}

func TestPingBusyboxRejected(t *testing.T) {
	_, err := ParsePing(fixture(t, "ping", "busybox_format.txt"), nil, 0)
	if !errors.Is(err, ErrUnsupportedPingFormat) {
		t.Errorf("err = %v, want errors.Is(err, ErrUnsupportedPingFormat) — busybox output must never parse as a loss figure", err)
	}
}
