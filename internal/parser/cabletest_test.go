package parser

import (
	"strings"
	"testing"

	"cablecheck/internal/model"
)

// TestCableTestParse covers the supported pair-code grammar, unavailable
// outcomes, and tolerant TDR sample extraction.
func TestCableTestParse(t *testing.T) {
	t.Run("ok fixture", func(t *testing.T) {
		got := ParseCableTest(fixture(t, "ethtool", "cabletest_ok.txt"), nil, 0)
		if !got.Available || len(got.Pairs) != 4 {
			t.Errorf("ParseCableTest(ok) = %+v, want four available pairs", got)
		}
		for _, pair := range got.Pairs {
			if pair.Status != model.PairOK || pair.HasFault {
				t.Errorf("pair %+v, want OK without fault", pair)
			}
		}
	})

	t.Run("open with distance fixture", func(t *testing.T) {
		got := ParseCableTest(
			fixture(t, "ethtool", "cabletest_open_32m.stdout"), nil, 0)
		if !got.Available || len(got.Pairs) != 4 {
			t.Errorf("ParseCableTest(open) = %+v, want four available pairs", got)
			return
		}
		if pair := got.Pairs[2]; pair.Pair != "C" || pair.Status != model.PairOpen ||
			!pair.HasFault || pair.FaultMeters != 32 {
			t.Errorf("pair C = %+v, want OPEN at 32m", pair)
		}
		if pair := got.Pairs[3]; pair.Pair != "D" || pair.Status != model.PairOpen ||
			!pair.HasFault || pair.FaultMeters != 32.4 {
			t.Errorf("pair D = %+v, want OPEN at 32.4m", pair)
		}
	})

	t.Run("short impedance unspecified", func(t *testing.T) {
		stdout := []byte(strings.Join([]string{
			"Pair A code Short within Pair",
			"Pair B code Short to another pair",
			"Pair C code Impedance mismatch",
			"Pair D code Crossed mystery",
		}, "\n"))
		got := ParseCableTest(stdout, nil, 0)
		want := []model.PairStatus{
			model.PairShortIntra, model.PairShortInter,
			model.PairImpedance, model.PairUnspecified,
		}
		if len(got.Pairs) != len(want) {
			t.Errorf("pairs = %+v, want %d", got.Pairs, len(want))
			return
		}
		for i, status := range want {
			if got.Pairs[i].Status != status {
				t.Errorf("pair %d status = %s, want %s", i, got.Pairs[i].Status, status)
			}
		}
		if got.Pairs[3].RawCode != "Crossed mystery" {
			t.Errorf("unspecified RawCode = %q, want preserved", got.Pairs[3].RawCode)
		}
	})

	t.Run("unsupported never faults", func(t *testing.T) {
		got := ParseCableTest(
			[]byte("Pair C code Open Circuit\nPair C, fault length: 8.00m\n"),
			fixture(t, "ethtool", "cabletest_unsupported.stderr"), 1)
		if got.Available || got.UnavailableReason != "driver does not support cable test" {
			t.Errorf("unsupported = %+v, want unavailable driver reason", got)
		}
		if len(got.Pairs) != 0 {
			t.Errorf("unsupported pairs = %+v, want none (never a fault)", got.Pairs)
		}
	})

	t.Run("requires root", func(t *testing.T) {
		got := ParseCableTest(nil, fixture(t, "ethtool", "cabletest_noperm.stderr"), 1)
		if got.Available || got.UnavailableReason != "requires root" || len(got.Pairs) != 0 {
			t.Errorf("not permitted = %+v, want unavailable requires root", got)
		}
	})

	t.Run("old ethtool", func(t *testing.T) {
		got := ParseCableTest(nil, []byte("ethtool: bad command line argument(s)\nFor more information run ethtool -h\n"), 0)
		if got.Available || got.UnavailableReason != "ethtool too old" {
			t.Errorf("old ethtool = %+v, want unavailable ethtool too old", got)
		}
	})

	t.Run("tdr samples", func(t *testing.T) {
		got := ParseCableTestTDR(fixture(t, "ethtool", "cabletest_tdr.txt"), nil, 0)
		if !got.Available || len(got.Samples) != 5 {
			t.Errorf("ParseCableTestTDR = %+v, want five available samples", got)
			return
		}
		if sample := got.Samples[1]; sample.Pair != "A" || sample.DistanceM != 12.5 || sample.Amplitude != -14 {
			t.Errorf("negative sample = %+v, want pair A 12.5m amplitude -14", sample)
		}
	})
}
