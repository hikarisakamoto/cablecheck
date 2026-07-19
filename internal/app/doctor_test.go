package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// doctorDeps builds a Deps for a doctor run over the given FakeRunner, with a
// writable output dir and the fake sysfs tree that classifies eth0 physical.
func doctorDeps(t *testing.T, fr *runnertest.FakeRunner) (Deps, string) {
	t.Helper()
	out := &runOutput{}
	return Deps{
		Runner:   fr,
		Stdout:   &out.stdout,
		Stderr:   &out.stderr,
		StateDir: t.TempDir(),
		Build:    BuildInfo{Version: "test"},
	}, t.TempDir()
}

// scriptDoctorAllPresent scripts every probe of an all-green doctor run: the
// four tool versions, iperf3 capability detection, `ip -j addr show` for the
// interface inventory, ethtool link read, and a passwordless-sudo success.
func scriptDoctorAllPresent(fr *runnertest.FakeRunner) {
	fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsExact("-j", "addr", "show"),
		Result: runner.CommandResult{Stdout: []byte(ipAddrJSON)}})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsExact("-V"),
		Result: runner.CommandResult{Stdout: []byte("ping from iputils 20240117\n")}})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--version"),
		StdoutFile: fixture("iperf", "version_316.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--help"),
		StdoutFile: fixture("iperf", "help_with_bidir.txt")})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("--version"),
		Result: runner.CommandResult{Stdout: []byte("ethtool version 6.9\n")}})
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
		StdoutFile: fixture("ethtool", "settings_e1000e_1g.txt")})
	fr.Script(runnertest.Script{Name: "sudo", Match: runnertest.ArgsExact("-n", "true"),
		Result: runner.CommandResult{ExitCode: 0}})
}

// checkByPrefix returns the first check whose name starts with prefix.
func checkByPrefix(checks []CheckResult, prefix string) (CheckResult, bool) {
	for _, c := range checks {
		if strings.HasPrefix(c.Name, prefix) {
			return c, true
		}
	}
	return CheckResult{}, false
}

// countStatus counts checks with the given status.
func countStatus(checks []CheckResult, status CheckStatus) int {
	n := 0
	for _, c := range checks {
		if c.Status == status {
			n++
		}
	}
	return n
}

func TestDoctorChecks(t *testing.T) {
	root := fakeSysfs(t, "e1000e")

	t.Run("AllPresentPass", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		deps, outDir := doctorDeps(t, fr)
		checks, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: outDir, SysfsRoot: root, AllowVirtual: true})
		if err != nil {
			t.Fatalf("Doctor returned error on an all-present run: %v", err)
		}
		if n := countStatus(checks, CheckFail); n != 0 {
			t.Errorf("all-present run has %d FAIL checks, want 0:\n%s", n, dumpChecks(checks))
		}
	})

	t.Run("MissingIperf3FailsWithHints", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		fr.Missing("iperf3")
		deps, outDir := doctorDeps(t, fr)
		checks, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: outDir, SysfsRoot: root, AllowVirtual: true})
		assertExitConfig(t, err)
		c, ok := checkByPrefix(checks, "iperf3")
		if !ok {
			t.Fatalf("no iperf3 check present:\n%s", dumpChecks(checks))
		}
		if c.Status != CheckFail {
			t.Errorf("iperf3 check = %s, want FAIL", c.Status)
		}
		for _, hint := range []string{"pacman", "apt", "dnf"} {
			if !strings.Contains(c.Detail, hint) {
				t.Errorf("iperf3 FAIL detail %q lacks the %s install hint", c.Detail, hint)
			}
		}
	})

	t.Run("MissingEthtoolFails", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		fr.Missing("ethtool")
		deps, outDir := doctorDeps(t, fr)
		checks, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: outDir, SysfsRoot: root, AllowVirtual: true})
		assertExitConfig(t, err)
		c, ok := checkByPrefix(checks, "ethtool")
		if !ok || c.Status != CheckFail {
			t.Errorf("ethtool check = %+v, want a FAIL", c)
		}
	})

	t.Run("BusyboxPingFails", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		// Override the ping version with a busybox banner (no "iputils").
		fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsExact("-V"),
			Result: runner.CommandResult{
				Stderr:   []byte("BusyBox v1.36.1 (2024-01-01) multi-call binary.\n"),
				ExitCode: 1}})
		deps, outDir := doctorDeps(t, fr)
		checks, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: outDir, SysfsRoot: root, AllowVirtual: true})
		assertExitConfig(t, err)
		c, ok := checkByPrefix(checks, "ping")
		if !ok || c.Status != CheckFail {
			t.Errorf("ping check = %+v, want a FAIL for busybox", c)
		}
	})

	t.Run("SudoDeniedWarns", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		// sudo -n true is denied.
		fr.Script(runnertest.Script{Name: "sudo", Match: runnertest.ArgsExact("-n", "true"),
			Result: runner.CommandResult{ExitCode: 1,
				Stderr: []byte("sudo: a password is required\n")}})
		deps, outDir := doctorDeps(t, fr)
		checks, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: outDir, SysfsRoot: root, AllowVirtual: true})
		if err != nil {
			t.Fatalf("a denied sudo probe must WARN, not fail the run: %v", err)
		}
		c, ok := checkByPrefix(checks, "sudo")
		if !ok || c.Status != CheckWarn {
			t.Errorf("sudo check = %+v, want a WARN", c)
		}
	})

	t.Run("NoPhysicalInterfacesWarns", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		deps, outDir := doctorDeps(t, fr)
		// An empty sysfs tree: eth0 has no device symlink → virtual.
		emptyRoot := t.TempDir()
		checks, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: outDir, SysfsRoot: emptyRoot, AllowVirtual: true})
		if err != nil {
			t.Fatalf("no physical interfaces must WARN, not fail: %v", err)
		}
		c, ok := checkByPrefix(checks, "interface")
		if !ok || c.Status != CheckWarn {
			t.Errorf("interface check = %+v, want a WARN when no physical NIC is found", c)
		}
	})

	t.Run("OutputDirUnwritableFails", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		deps, _ := doctorDeps(t, fr)
		checks, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: filepath.Join(t.TempDir(), "does-not-exist"),
			SysfsRoot: root, AllowVirtual: true})
		assertExitConfig(t, err)
		c, ok := checkByPrefix(checks, "output")
		if !ok || c.Status != CheckFail {
			t.Errorf("output-dir check = %+v, want a FAIL for an unwritable dir", c)
		}
	})

	t.Run("ExitCodeIsFourOnAnyFailElseZero", func(t *testing.T) {
		// A single FAIL → the returned error carries ExitConfig (4).
		fr := runnertest.New(t)
		scriptDoctorAllPresent(fr)
		fr.Missing("ethtool")
		deps, outDir := doctorDeps(t, fr)
		_, err := Doctor(context.Background(), deps, DoctorOptions{
			OutputDir: outDir, SysfsRoot: root, AllowVirtual: true})
		assertExitConfig(t, err)

		// No FAIL → nil error (doctor never emits exit 3).
		fr2 := runnertest.New(t)
		scriptDoctorAllPresent(fr2)
		deps2, outDir2 := doctorDeps(t, fr2)
		if _, err := Doctor(context.Background(), deps2, DoctorOptions{
			OutputDir: outDir2, SysfsRoot: root, AllowVirtual: true}); err != nil {
			t.Errorf("clean doctor returned %v, want nil", err)
		}
	})
}

func TestCheckPingPreLaunchFailureReturnsFail(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{
		Name:  "ping",
		Match: runnertest.ArgsExact("-V"),
		Err:   errors.New("start ping: executable disappeared"),
	})

	got := checkPing(context.Background(), fr)
	if got.Status != CheckFail {
		t.Errorf("checkPing status = %s, want FAIL", got.Status)
	}
}

func TestCheckInterfacesAllowVirtualDropsRequiresFlagNote(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{
		Name:   "ip",
		Match:  runnertest.ArgsExact("-j", "addr", "show"),
		Result: runner.CommandResult{Stdout: []byte(ipAddrJSON)},
	})

	checks := checkInterfaces(context.Background(), fr, DoctorOptions{
		AllowVirtual: true,
		SysfsRoot:    t.TempDir(),
	})
	got, ok := checkByPrefix(checks, "interface eth0")
	if !ok {
		t.Errorf("no eth0 interface check present:\n%s", dumpChecks(checks))
		return
	}
	if strings.Contains(got.Detail, "requires --allow-virtual-interface") {
		t.Errorf("allowed virtual interface detail still requires the flag: %q", got.Detail)
	}
}

// assertExitConfig fails unless err carries ExitConfig (4).
func assertExitConfig(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want an *ExitError with code 4")
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitConfig {
		t.Fatalf("error = %v, want an *ExitError{ExitConfig}", err)
	}
}

// dumpChecks renders checks for failure messages.
func dumpChecks(checks []CheckResult) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString("[" + string(c.Status) + "] " + c.Name + ": " + c.Detail + "\n")
	}
	return b.String()
}

// TestDoctorOutputFormat asserts the operator-facing rendering: aligned
// [PASS]/[WARN]/[FAIL] lines plus a summary line counting each status.
func TestDoctorOutputFormat(t *testing.T) {
	root := fakeSysfs(t, "e1000e")
	fr := runnertest.New(t)
	scriptDoctorAllPresent(fr)
	fr.Missing("ethtool") // one FAIL, so all three statuses can appear
	out := &runOutput{}
	deps := Deps{
		Runner:   fr,
		Stdout:   &out.stdout,
		Stderr:   &out.stderr,
		StateDir: t.TempDir(),
		Build:    BuildInfo{Version: "test"},
	}
	outDir := t.TempDir()
	checks, _ := Doctor(context.Background(), deps, DoctorOptions{
		OutputDir: outDir, SysfsRoot: root, AllowVirtual: true})
	RenderDoctor(&out.stdout, checks)

	text := out.stdout.String()
	if !strings.Contains(text, "[FAIL]") {
		t.Errorf("rendered output lacks a [FAIL] line:\n%s", text)
	}
	if !strings.Contains(text, "[PASS]") {
		t.Errorf("rendered output lacks a [PASS] line:\n%s", text)
	}
	// Summary line "N PASS, N WARN, N FAIL".
	pass := countStatus(checks, CheckPass)
	warn := countStatus(checks, CheckWarn)
	fail := countStatus(checks, CheckFail)
	summary := formatDoctorSummary(pass, warn, fail)
	if !strings.Contains(text, summary) {
		t.Errorf("rendered output lacks the summary line %q:\n%s", summary, text)
	}
}
