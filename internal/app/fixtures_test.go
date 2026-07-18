package app

import (
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// scriptHealthyQuick composes the complete healthy-run FakeRunner script set
// one side of the quick plan consumes, from the shared testdata fixtures:
// the preflight probes (tool identities, iperf3 version+help capability
// detection, ip -j addr interface discovery), the ethtool link settings read
// (preflight and plan/op), exactly two counter snapshots (ethtool -S and
// ip -j -s -s, before/after — Times-limited so a third snapshot fails
// loudly), one stability ping plus one full-size -M do ping (each side pings
// the other), the iperf3 one-off servers (each side hosts the receiving side
// of its phases: TCP, bidir on pc2, UDP), one TCP client run per side, the
// native --bidir client (pc1 only; the script is harmless on pc2), and one
// UDP client run per side. side must be "pc1" or "pc2"; both sides currently
// share the same healthy fixture set, the parameter pins the call sites for
// per-side divergence.
func scriptHealthyQuick(t *testing.T, fr *runnertest.FakeRunner, side string) {
	t.Helper()
	if side != "pc1" && side != "pc2" {
		t.Fatalf("scriptHealthyQuick: side = %q, want pc1 or pc2", side)
	}

	// Preflight tool probes.
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsExact("-V"),
		Result: runner.CommandResult{Stdout: []byte("ping from iputils 20240117\n")}})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("--version"),
		Result: runner.CommandResult{Stdout: []byte("ethtool version 6.9\n")}})

	// iperf3 capability detection: version window + --help agreement.
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--version"),
		StdoutFile: fixture("iperf", "version_316.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--help"),
		StdoutFile: fixture("iperf", "help_with_bidir.txt")})

	// ip -j addr discovery: eth0 owns the loopback test address.
	fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsExact("-j", "addr", "show"),
		Result: runner.CommandResult{Stdout: []byte(ipAddrJSON)}})

	// ethtool link settings: read by preflight and by the plan's link step
	// (locally on pc1, as the link_settings op on pc2).
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		StdoutFile: fixture("ethtool", "settings_e1000e_1g.txt")})

	// Counter snapshots x2 (initial and final).
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"),
		StdoutFile: fixture("ethtool", "stats_e1000e_clean.txt"), Times: 2})
	fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsPrefix("-j", "-s", "-s"),
		StdoutFile: fixture("ip", "linkstats_clean.json"), Times: 2})

	// Ping stability toward the peer (ping both: one run per side), plus the
	// full-size -M do variant. Both matchers rank equally (Contain), so the
	// later-added -M do script wins for full-size calls and the plain one
	// serves the quick ping.
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("ping", "quick_clean_100.txt")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		StdoutFile: fixture("ping", "fullsize_ok.txt")})

	// iperf3: one-off servers for every receiving phase (launched via
	// Runner.Start, readiness from the banner fixture), one TCP client run,
	// the native --bidir client and the UDP client. The --bidir and -u
	// scripts are added after the generic -c one so they win the
	// equal-specificity tie for their phases.
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixture("iperf", "server_listening.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("iperf", "tcp_316_fwd.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("--bidir"),
		StdoutFile: fixture("iperf", "bidir_314.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-u"),
		StdoutFile: fixture("iperf", "udp_316.json")})
}
