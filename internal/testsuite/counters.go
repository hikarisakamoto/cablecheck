// Package testsuite implements CableCheck's actual test procedures on top of
// the runner, parser and peer layers: NIC counter snapshots, link-settings
// inspection, ping stability runs, iperf3 server/client management, and the
// op vocabulary plus plan driver that coordinate the two peers.
package testsuite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
)

// queryToolTimeout is the runner-timeout backstop on the quick status
// queries (`ethtool -S`, `ethtool <if>`, `ip -j -s -s link show`). They
// normally finish in milliseconds, but the coordinator's local half runs
// them under the long-lived plan ctx, so without a per-spec bound a wedged
// ethtool ioctl (real with buggy NIC drivers) would hang the whole session.
const queryToolTimeout = 10 * time.Second

// stdCounterSources maps each standard counter key to its ethtool candidate
// names in resolution order. The first candidate present in the driver's
// `ethtool -S` output wins, even at value zero — a driver that exposes the
// counter measured it. Keys with no matching candidate fall back to the
// `ip -s -s` field (nonzero only; see NormalizeCounters), else stay absent.
var stdCounterSources = map[string][]string{
	"rx_crc":     {"rx_crc_errors", "rx_fcs_errors"},
	"rx_frame":   {"rx_frame_errors"},
	"rx_align":   {"rx_align_errors", "align_errors"},
	"rx_symbol":  {"rx_symbol_errors", "symbol_errors"},
	"rx_missed":  {"rx_missed_errors", "rx_missed"},
	"rx_fifo":    {"rx_fifo_errors", "rx_over_errors", "rx_no_buffer_count"},
	"rx_length":  {"rx_length_errors"},
	"undersize":  {"rx_undersize_packets", "rx_short_length_errors"},
	"oversize":   {"rx_oversize_packets", "rx_long_length_errors"},
	"jabber":     {"rx_jabbers", "rx_jabber_errors", "jabber"},
	"tx_carrier": {"tx_carrier_errors"},
	"phy_errors": {"phy_errors", "rx_phy_errors"},
}

// ipFallback extracts the `ip -j -s -s` value backing a standard key, when
// rtnetlink carries one at all; ok=false means this key has no ip fallback.
func ipFallback(key string, ip model.IPStats64) (uint64, bool) {
	switch key {
	case "rx_crc":
		return ip.RX.CRCErrors, true
	case "rx_frame":
		return ip.RX.FrameErrors, true
	case "rx_missed":
		return ip.RX.MissedErrors, true
	case "rx_fifo":
		return ip.RX.FifoErrors, true
	case "rx_length":
		return ip.RX.LengthErrors, true
	case "tx_carrier":
		return ip.TX.CarrierErrors, true
	default:
		return 0, false
	}
}

// NormalizeCounters builds the standard counter view from the driver-specific
// `ethtool -S` counters, the `ip -j -s -s` stats and the sysfs carrier_changes
// reading (nil when unreadable).
//
// Resolution order per key: first present ethtool candidate (zero included:
// the driver measured it) → nonzero `ip -s -s` field → absent. Zero rtnetlink
// values are deliberately NOT surfaced: `ip` reports every field as 0 even for
// drivers that never track it, so a zero there is ambiguous, while an absent
// key correctly tells the evaluator "no data" (which is not the same as "0
// errors"). link_resets comes only from sysfs carrier_changes.
func NormalizeCounters(ethtool map[string]uint64, ip model.IPStats64, carrierChanges *uint64) map[string]uint64 {
	std := make(map[string]uint64)
	for key, candidates := range stdCounterSources {
		resolved := false
		for _, name := range candidates {
			if v, ok := ethtool[name]; ok {
				std[key] = v
				resolved = true
				break
			}
		}
		if resolved {
			continue
		}
		if v, ok := ipFallback(key, ip); ok && v != 0 {
			std[key] = v
		}
	}
	if carrierChanges != nil {
		std["link_resets"] = *carrierChanges
	}
	return std
}

// CounterCollector captures point-in-time NIC counter snapshots for one
// interface by combining `ethtool -S`, `ip -j -s -s link show dev` and the
// sysfs carrier_changes attribute.
type CounterCollector struct {
	// R runs the external tools.
	R runner.Runner
	// Clock timestamps the snapshots.
	Clock clock.Clock
	// IfName is the interface under test.
	IfName string
	// SysfsRoot overrides /sys/class/net for tests; empty means the real tree.
	SysfsRoot string
	// RawDir, when non-empty, receives verbatim tool output tee files.
	RawDir string
}

// Snapshot captures the interface's counters right now. `ethtool -S` being
// unavailable (tool missing, or statistics unsupported) is not an error: the
// snapshot is still valid with Raw/Driver empty and Standard filled from the
// `ip -s -s` fallback. A failing `ip` invocation is fatal — without it there
// is no reliable counter source at all.
func (c *CounterCollector) Snapshot(ctx context.Context) (model.CounterSnapshot, error) {
	snap := model.CounterSnapshot{CapturedAt: c.Clock.Now()}

	var driver map[string]uint64
	ethRes, err := c.R.Run(ctx, teeRaw(runner.CommandSpec{
		Name:    "ethtool",
		Args:    []string{"-S", c.IfName},
		Timeout: queryToolTimeout,
		Label:   "ethtool-stats",
	}, c.RawDir))
	if err == nil && ethRes.ExitCode == 0 {
		snap.Raw = string(ethRes.Stdout)
		driver = parser.ParseEthtoolStats(ethRes.Stdout)
		snap.Driver = driver
	} else if err != nil && ctx.Err() != nil {
		return snap, fmt.Errorf("testsuite: counter snapshot: %w", err)
	}

	ipRes, err := c.R.Run(ctx, teeRaw(runner.CommandSpec{
		Name:    "ip",
		Args:    []string{"-j", "-s", "-s", "link", "show", "dev", c.IfName},
		Timeout: queryToolTimeout,
		Label:   "ip-linkstats",
	}, c.RawDir))
	if err != nil {
		return snap, fmt.Errorf("testsuite: counter snapshot: ip -j -s -s: %w", err)
	}
	if ipRes.ExitCode != 0 {
		return snap, fmt.Errorf("testsuite: counter snapshot: ip -j -s -s exited %d: %s",
			ipRes.ExitCode, strings.TrimSpace(string(ipRes.Stderr)))
	}
	link, err := parser.ParseIPLinkStats(ipRes.Stdout)
	if err != nil {
		return snap, fmt.Errorf("testsuite: counter snapshot: %w", err)
	}
	if link.Stats64 != nil {
		snap.IPStats = *link.Stats64
	}

	snap.Standard = NormalizeCounters(driver, snap.IPStats, c.carrierChanges())
	return snap, nil
}

// carrierChanges reads the sysfs carrier_changes attribute; nil when the
// attribute is unreadable or unparseable (link_resets then stays absent).
func (c *CounterCollector) carrierChanges() *uint64 {
	root := c.SysfsRoot
	if root == "" {
		root = "/sys/class/net"
	}
	data, err := os.ReadFile(filepath.Join(root, c.IfName, "carrier_changes"))
	if err != nil {
		return nil
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

var rawTeeSequence = struct {
	sync.Mutex
	nextByDir map[string]int
}{nextByDir: make(map[string]int)}

// teeRaw points the spec's stdout tee at a unique NN-<label>.txt path when a
// raw directory is configured. The per-directory sequence mirrors the report
// RawStore naming convention and keeps repeated phases (including concurrent
// bidirectional clients) from truncating an earlier artifact with O_TRUNC.
func teeRaw(spec runner.CommandSpec, rawDir string) runner.CommandSpec {
	if rawDir != "" && spec.Label != "" {
		rawTeeSequence.Lock()
		rawTeeSequence.nextByDir[rawDir]++
		seq := rawTeeSequence.nextByDir[rawDir]
		rawTeeSequence.Unlock()
		spec.TeeStdoutPath = filepath.Join(rawDir, fmt.Sprintf("%02d-%s.txt", seq, spec.Label))
	}
	return spec
}
