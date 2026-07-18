package app

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cablecheck/internal/config"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// fixture returns the path of a shared testdata fixture.
func fixture(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "testdata"}, parts...)...)
}

// fakeSysfs builds a /sys/class/net stand-in containing one physical
// ethernet interface named eth0 with the given driver.
func fakeSysfs(t *testing.T, driver string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "eth0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write type: %v", err)
	}
	devDir := filepath.Join(root, "_devices", "pci0000:00", "0000:00:1f.6")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatalf("mkdir device: %v", err)
	}
	if err := os.Symlink(devDir, filepath.Join(dir, "device")); err != nil {
		t.Fatalf("symlink device: %v", err)
	}
	drvDir := filepath.Join(root, "_drivers", driver)
	if err := os.MkdirAll(drvDir, 0o755); err != nil {
		t.Fatalf("mkdir driver: %v", err)
	}
	if err := os.Symlink(drvDir, filepath.Join(devDir, "driver")); err != nil {
		t.Fatalf("symlink driver: %v", err)
	}
	return root
}

// ipAddrJSON is an `ip -j addr show` document claiming eth0 owns 127.0.0.1
// (loopback keeps the port probes hermetic; the fake sysfs classifies eth0
// as physical ether, so no virtual handling triggers).
const ipAddrJSON = `[{"ifindex":2,"ifname":"eth0","flags":["UP","LOWER_UP"],"mtu":1500,` +
	`"operstate":"UP","link_type":"ether","address":"aa:bb:cc:dd:ee:ff",` +
	`"addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host"}]}]`

// scriptPreflightTools installs the happy-path preflight scripts.
func scriptPreflightTools(fr *runnertest.FakeRunner) {
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsExact("-V"),
		Result: runner.CommandResult{Stdout: []byte("ping from iputils 20240117\n")}})
	fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsExact("-j", "addr", "show"),
		Result: runner.CommandResult{Stdout: []byte(ipAddrJSON)}})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("--version"),
		Result: runner.CommandResult{Stdout: []byte("ethtool version 6.9\n")}})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		StdoutFile: fixture("ethtool", "settings_e1000e_1g.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--version"),
		StdoutFile: fixture("iperf", "version_316.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--help"),
		StdoutFile: fixture("iperf", "help_with_bidir.txt")})
}

// newPreflightApp builds an App over a scripted FakeRunner ready for
// preflight. The returned buffer captures operator-facing stdout.
func newPreflightApp(t *testing.T, fr *runnertest.FakeRunner) (*App, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	cfg := &config.RunConfig{
		Role:        config.RolePC1,
		LocalIP:     netip.MustParseAddr("127.0.0.1"),
		PeerIP:      netip.MustParseAddr("127.0.0.2"),
		Mode:        config.ModeQuick,
		ControlPort: 45300,
		IperfPort:   45301,
		Token:       "testtoken1234",
		OutputDir:   t.TempDir(),
		NoSudo:      true,
	}
	a, err := New(cfg, Deps{
		Runner:   fr,
		Stdout:   &out,
		Stderr:   &out,
		StateDir: t.TempDir(),
		Build:    BuildInfo{Version: "test"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sysfsRoot = fakeSysfs(t, "e1000e")
	return a, &out
}

// TestPreflightHappyPath asserts the ordered checks pass and the results
// feed protocol.Capabilities.
func TestPreflightHappyPath(t *testing.T) {
	fr := runnertest.New(t)
	scriptPreflightTools(fr)
	a, _ := newPreflightApp(t, fr)

	pf, err := a.preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if pf.Iface.Name != "eth0" {
		t.Errorf("Iface.Name = %q, want eth0", pf.Iface.Name)
	}
	caps := pf.Caps
	if caps.Iperf3.Version != "3.16" || !caps.Iperf3.Bidir || !caps.Iperf3.JSON {
		t.Errorf("iperf3 caps = %+v, want 3.16 with bidir+json", caps.Iperf3)
	}
	if !strings.Contains(caps.PingVariant, "iputils") {
		t.Errorf("PingVariant = %q, want iputils", caps.PingVariant)
	}
	if caps.NIC.Name != "eth0" || caps.NIC.Driver != "e1000e" {
		t.Errorf("NIC = %+v, want eth0/e1000e", caps.NIC)
	}
	if caps.NIC.SpeedMbps != 1000 {
		t.Errorf("NIC.SpeedMbps = %d, want 1000", caps.NIC.SpeedMbps)
	}
	if caps.SudoAvailable {
		t.Errorf("SudoAvailable = true despite --no-sudo")
	}
	if caps.CablecheckVersion != "test" {
		t.Errorf("CablecheckVersion = %q, want test", caps.CablecheckVersion)
	}
	if !caps.AcceptReportTransfer {
		t.Errorf("AcceptReportTransfer = false, want true")
	}
	if v := pf.ToolVersions["ethtool"]; !strings.Contains(v, "6.9") {
		t.Errorf("ToolVersions[ethtool] = %q, want 6.9", v)
	}
}

// TestPreflightMissingTool asserts a missing dependency fails with distro
// install hints.
func TestPreflightMissingTool(t *testing.T) {
	fr := runnertest.New(t)
	fr.Missing("iperf3")
	a, _ := newPreflightApp(t, fr)

	_, err := a.preflight(context.Background())
	if err == nil {
		t.Fatalf("preflight succeeded with iperf3 missing")
	}
	for _, want := range []string{"iperf3", "pacman -S iperf3", "apt install iperf3", "dnf install iperf3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q misses %q", err, want)
		}
	}
}

// TestPreflightBusyboxPingRejected asserts a non-iputils ping fails preflight.
func TestPreflightBusyboxPingRejected(t *testing.T) {
	fr := runnertest.New(t)
	scriptPreflightTools(fr)
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsExact("-V"),
		Result: runner.CommandResult{Stdout: []byte("BusyBox v1.36.1 multi-call binary\n")}})
	a, _ := newPreflightApp(t, fr)

	_, err := a.preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "iputils") {
		t.Errorf("want iputils rejection error, got %v", err)
	}
}

// TestPreflightVirtualWarning asserts a virtual interface passes only with
// --allow-virtual-interface and prints a prominent warning.
func TestPreflightVirtualWarning(t *testing.T) {
	fr := runnertest.New(t)
	scriptPreflightTools(fr)
	a, out := newPreflightApp(t, fr)
	// Reclassify eth0 as virtual: no device symlink.
	root := t.TempDir()
	dir := filepath.Join(root, "eth0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte(strconv.Itoa(1)+"\n"), 0o644); err != nil {
		t.Fatalf("write type: %v", err)
	}
	a.sysfsRoot = root

	if _, err := a.preflight(context.Background()); err == nil {
		t.Errorf("virtual interface accepted without --allow-virtual-interface")
	}

	a.cfg.AllowVirtualInterface = true
	if _, err := a.preflight(context.Background()); err != nil {
		t.Fatalf("preflight with --allow-virtual-interface: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "virtual") {
		t.Errorf("no prominent virtual warning printed:\n%s", out.String())
	}
}
