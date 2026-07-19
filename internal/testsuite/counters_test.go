package testsuite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner/runnertest"
)

// TestCounterSnapshotsUseDistinctRawTeeFiles ensures before/after snapshots
// cannot truncate one another's forensic artifacts. Every command in the two
// snapshots must receive its own sequenced tee path.
func TestCounterSnapshotsUseDistinctRawTeeFiles(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"),
		Result: fixture(t, "ethtool", "stats_e1000e_clean")})
	fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsExact("-j", "-s", "-s", "link", "show", "dev", "eth0"),
		Result: fixture(t, "ip", "linkstats_clean")})

	root := t.TempDir()
	writeSysfs(t, root, "eth0", "carrier_changes", "2\n")
	c := &CounterCollector{
		R: fr, Clock: clocktest.New(time.Unix(1_700_000_000, 0)), IfName: "eth0",
		SysfsRoot: root, RawDir: t.TempDir(),
	}
	for _, phase := range []string{"before", "after"} {
		if _, err := c.Snapshot(context.Background()); err != nil {
			t.Errorf("%s Snapshot: %v", phase, err)
		}
	}

	paths := make(map[string]bool)
	for _, call := range fr.Calls() {
		path := call.Spec.TeeStdoutPath
		if path == "" {
			t.Errorf("%s %q has no raw tee path", call.Name, call.Args)
			continue
		}
		if paths[path] {
			t.Errorf("raw tee path %q reused; the later snapshot would overwrite the earlier artifact", path)
		}
		paths[path] = true
		if filepath.Dir(path) != c.RawDir {
			t.Errorf("raw tee path = %q, want it under %q", path, c.RawDir)
		}
	}
	if len(paths) != 4 {
		t.Errorf("two snapshots produced %d distinct raw tee paths, want 4", len(paths))
	}
}

// TestCounterSnapshotsBeforeAfter drives two full before/after snapshots off
// Times:1 scripts, then verifies the ethtool-missing fallback: Standard comes
// from ip stats, raw ethtool output is absent, and the snapshot is still valid.
func TestCounterSnapshotsBeforeAfter(t *testing.T) {
	ctx := context.Background()
	ipArgs := []string{"-j", "-s", "-s", "link", "show", "dev", "eth0"}

	t.Run("EthtoolPresent", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"),
			Result: fixture(t, "ethtool", "stats_e1000e_clean"), Times: 1})
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"),
			Result: fixture(t, "ethtool", "stats_e1000e_crc"), Times: 1})
		fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsExact(ipArgs...),
			Result: fixture(t, "ip", "linkstats_clean"), Times: 1})
		fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsExact(ipArgs...),
			Result: fixture(t, "ip", "linkstats_errors"), Times: 1})

		root := t.TempDir()
		writeSysfs(t, root, "eth0", "carrier_changes", "2\n")
		clk := clocktest.New(time.Unix(1_700_000_000, 0))
		c := &CounterCollector{R: fr, Clock: clk, IfName: "eth0", SysfsRoot: root}

		before, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatalf("before snapshot: %v", err)
		}
		writeSysfs(t, root, "eth0", "carrier_changes", "4\n")
		after, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatalf("after snapshot: %v", err)
		}

		if before.Raw == "" || len(before.Driver) == 0 {
			t.Errorf("before snapshot lost the raw ethtool view: raw %d bytes, %d driver counters",
				len(before.Raw), len(before.Driver))
		}
		if got := before.Standard["rx_crc"]; got != 0 {
			t.Errorf("before rx_crc = %d, want 0", got)
		}
		if _, ok := before.Standard["rx_crc"]; !ok {
			t.Errorf("before rx_crc missing: an ethtool-exposed zero must be present, not absent")
		}
		if got := before.Standard["link_resets"]; got != 2 {
			t.Errorf("before link_resets = %d, want 2 (sysfs carrier_changes)", got)
		}
		wantAfter := map[string]uint64{
			"rx_crc": 1543, "rx_align": 12, "rx_missed": 4,
			"rx_length": 2, "oversize": 2, "link_resets": 4,
		}
		for key, want := range wantAfter {
			if got, ok := after.Standard[key]; !ok || got != want {
				t.Errorf("after Standard[%q] = %d (present=%v), want %d", key, got, ok, want)
			}
		}
		if before.IPStats.RX.Bytes != 61250873344 {
			t.Errorf("before ipStats rx bytes = %d, want 61250873344", before.IPStats.RX.Bytes)
		}
		if after.IPStats.TX.CarrierChanges != 5 {
			t.Errorf("after ipStats tx carrier_changes = %d, want 5", after.IPStats.TX.CarrierChanges)
		}
		if !after.CapturedAt.After(time.Time{}) {
			t.Errorf("after.CapturedAt is zero")
		}
	})

	t.Run("EthtoolMissing", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Missing("ethtool")
		fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsExact(ipArgs...),
			Result: fixture(t, "ip", "linkstats_errors")})

		root := t.TempDir()
		writeSysfs(t, root, "eth0", "carrier_changes", "7\n")
		clk := clocktest.New(time.Unix(1_700_000_000, 0))
		c := &CounterCollector{R: fr, Clock: clk, IfName: "eth0", SysfsRoot: root}

		snap, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot with ethtool missing must still be valid, got error: %v", err)
		}
		if snap.Raw != "" || len(snap.Driver) != 0 {
			t.Errorf("ethtool absent: raw/driver views must be empty, got raw %d bytes, %d counters",
				len(snap.Raw), len(snap.Driver))
		}
		want := map[string]uint64{
			"rx_crc": 1543, "rx_frame": 12, "rx_missed": 9,
			"rx_fifo": 3, "tx_carrier": 2, "link_resets": 7,
		}
		for key, wantV := range want {
			if got, ok := snap.Standard[key]; !ok || got != wantV {
				t.Errorf("Standard[%q] = %d (present=%v), want %d (ip -s -s fallback)", key, got, ok, wantV)
			}
		}
		if _, ok := snap.Standard["rx_align"]; ok {
			t.Errorf("rx_align present without any source; absent counters must stay absent")
		}
		if snap.IPStats.RX.CRCErrors != 1543 {
			t.Errorf("ipStats rx crc_errors = %d, want 1543", snap.IPStats.RX.CRCErrors)
		}
	})
}

// TestSnapshotToolTimeouts pins the explicit runner-timeout backstop on the
// snapshot's external commands: the coordinator's local half runs under the
// long-lived plan ctx, so without a per-spec Timeout a wedged ethtool ioctl
// (real with buggy NIC drivers) would hang the whole session.
func TestSnapshotToolTimeouts(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsPrefix("-S"),
		Result: fixture(t, "ethtool", "stats_e1000e_clean")})
	fr.Script(runnertest.Script{Name: "ip", Result: fixture(t, "ip", "linkstats_clean")})
	root := t.TempDir()
	writeSysfs(t, root, "eth0", "carrier_changes", "2\n")
	c := &CounterCollector{R: fr, Clock: clocktest.New(time.Unix(1_700_000_000, 0)),
		IfName: "eth0", SysfsRoot: root}
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	calls := fr.Calls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls %+v, want 2 (ethtool -S, ip)", len(calls), calls)
	}
	for _, call := range calls {
		if call.Spec.Timeout != 10*time.Second {
			t.Errorf("%s %q Spec.Timeout = %v, want the 10s backstop (a wedged tool must not hang the session)",
				call.Name, call.Args, call.Spec.Timeout)
		}
	}
}

// TestCounterNormalization pins the tools.md §6 candidate table: e1000e and
// r8169 naming, the virtio near-empty view, and that an absent key is never
// rendered as zero.
func TestCounterNormalization(t *testing.T) {
	zeroIP := model.IPStats64{}

	t.Run("E1000E", func(t *testing.T) {
		stats := parser.ParseEthtoolStats(readFixture(t, "ethtool", "stats_e1000e_crc"))
		std := NormalizeCounters(stats, zeroIP, nil)
		want := map[string]uint64{
			"rx_crc":     1543, // rx_crc_errors
			"rx_frame":   0,    // rx_frame_errors, present at zero
			"rx_align":   12,   // rx_align_errors
			"rx_missed":  4,    // rx_missed_errors
			"rx_fifo":    0,    // rx_over_errors candidate
			"rx_length":  2,    // rx_length_errors
			"undersize":  0,    // rx_short_length_errors
			"oversize":   2,    // rx_long_length_errors
			"tx_carrier": 0,    // tx_carrier_errors
		}
		for key, wantV := range want {
			if got, ok := std[key]; !ok || got != wantV {
				t.Errorf("e1000e Standard[%q] = %d (present=%v), want %d", key, got, ok, wantV)
			}
		}
		for _, key := range []string{"rx_symbol", "jabber", "phy_errors", "link_resets"} {
			if got, ok := std[key]; ok {
				t.Errorf("e1000e Standard[%q] = %d, want absent (no data is not zero)", key, got)
			}
		}
	})

	t.Run("R8169", func(t *testing.T) {
		stats := parser.ParseEthtoolStats(readFixture(t, "ethtool", "stats_r8169"))
		std := NormalizeCounters(stats, zeroIP, nil)
		if got, ok := std["rx_align"]; !ok || got != 41 {
			t.Errorf("r8169 rx_align = %d (present=%v), want 41 from align_errors", got, ok)
		}
		if got, ok := std["rx_missed"]; !ok || got != 9 {
			t.Errorf("r8169 rx_missed = %d (present=%v), want 9 from rx_missed", got, ok)
		}
		if got, ok := std["rx_crc"]; ok {
			t.Errorf("r8169 rx_crc = %d, want ABSENT: Realtek exposes no CRC counter and zero ip stats prove nothing", got)
		}
	})

	t.Run("Virtio", func(t *testing.T) {
		stats := parser.ParseEthtoolStats(readFixture(t, "ethtool", "stats_virtio"))
		carrier := uint64(3)
		std := NormalizeCounters(stats, zeroIP, &carrier)
		if len(std) != 1 {
			t.Errorf("virtio Standard = %v, want only link_resets (nearly empty)", std)
		}
		if got, ok := std["link_resets"]; !ok || got != 3 {
			t.Errorf("virtio link_resets = %d (present=%v), want 3", got, ok)
		}
	})

	t.Run("IPFallbackNonzeroOnly", func(t *testing.T) {
		ip := model.IPStats64{}
		ip.RX.CRCErrors = 17
		std := NormalizeCounters(nil, ip, nil)
		if got, ok := std["rx_crc"]; !ok || got != 17 {
			t.Errorf("rx_crc = %d (present=%v), want 17 from ip fallback", got, ok)
		}
		if _, ok := std["rx_length"]; ok {
			t.Errorf("rx_length present from a zero ip counter; rtnetlink zeros are ambiguous and must stay absent")
		}
	})
}
