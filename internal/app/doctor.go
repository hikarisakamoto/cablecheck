package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"cablecheck/internal/clock"
	"cablecheck/internal/network"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
	"cablecheck/internal/testsuite"
)

// CheckStatus is the outcome of a single doctor check.
type CheckStatus string

// The three doctor outcomes (docs/design/clieval.md §6).
const (
	// CheckPass: the requirement is satisfied.
	CheckPass CheckStatus = "PASS"
	// CheckWarn: the requirement is degraded but the run can still proceed.
	CheckWarn CheckStatus = "WARN"
	// CheckFail: the requirement is unmet and a run would fail preflight.
	CheckFail CheckStatus = "FAIL"
)

// CheckResult is one line of the doctor report.
type CheckResult struct {
	// Name identifies the check, e.g. "iperf3" or "output directory".
	Name string
	// Detail explains the outcome (a version string, a failure reason, an
	// install hint).
	Detail string
	// Status is the check outcome.
	Status CheckStatus
}

// DoctorOptions configures a doctor run: it is read-only apart from a
// temp-file writability probe in the output directory.
type DoctorOptions struct {
	// Interface, when set, restricts the interface inventory to this name.
	Interface string
	// OutputDir is the parent directory whose writability is probed
	// (default "." via the cli layer).
	OutputDir string
	// NoSudo skips the passwordless-sudo probe (reported as N/A).
	NoSudo bool
	// AllowVirtual mirrors --allow-virtual-interface: it is reflected in the
	// interface inventory annotations only.
	AllowVirtual bool
	// SysfsRoot overrides /sys/class/net for interface classification; empty
	// means the real tree. Tests inject a t.TempDir tree here.
	SysfsRoot string
}

// Doctor runs the local dependency and configuration checks (docs/design/
// clieval.md §6), in order, and returns them all. It performs no peer I/O and
// is read-only except for a temp-file writability probe. When any check FAILs
// it returns an *ExitError{ExitConfig} (exit 4); otherwise it returns nil.
// Doctor never emits exit 3.
func Doctor(ctx context.Context, deps Deps, opts DoctorOptions) ([]CheckResult, error) {
	deps = withDefaults(deps)
	r := deps.Runner

	var checks []CheckResult
	add := func(c CheckResult) { checks = append(checks, c) }

	add(checkIP(ctx, r))
	add(checkPing(ctx, r))
	add(checkIperf3(ctx, r))
	add(checkEthtool(ctx, r))
	checks = append(checks, checkInterfaces(ctx, r, opts)...)
	add(checkSudo(ctx, r, opts))
	add(checkCableTestSupport())
	add(checkOutputWritable(opts.OutputDir))

	for _, c := range checks {
		if c.Status == CheckFail {
			return checks, &ExitError{Code: ExitConfig,
				Err: fmt.Errorf("doctor found %d failing check(s); fix them before running a test", countFail(checks))}
		}
	}
	return checks, nil
}

// withDefaults fills the Deps fields Doctor relies on when they are unset,
// mirroring New so Doctor can run against a bare Deps.
func withDefaults(deps Deps) Deps {
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.Runner == nil {
		deps.Runner = runner.New(deps.Clock)
	}
	if deps.Stdout == nil {
		deps.Stdout = io.Discard
	}
	if deps.Stderr == nil {
		deps.Stderr = io.Discard
	}
	return deps
}

// checkIP verifies `ip` is present and `ip -j addr` yields valid JSON.
func checkIP(ctx context.Context, r runner.Runner) CheckResult {
	if _, err := r.LookPath("ip"); err != nil {
		return failMissing("ip")
	}
	res, err := r.Run(ctx, runner.CommandSpec{
		Name: "ip", Args: []string{"-j", "addr", "show"}, Timeout: probeTimeout, Label: "doctor-ip"})
	if err != nil || res.Failed() {
		return CheckResult{Name: "ip", Status: CheckFail, Detail: "`ip -j addr show` failed to run"}
	}
	if _, perr := parser.ParseIPAddr(res.Stdout); perr != nil {
		return CheckResult{Name: "ip", Status: CheckFail,
			Detail: fmt.Sprintf("`ip -j addr show` did not produce valid JSON: %v", perr)}
	}
	return CheckResult{Name: "ip", Status: CheckPass, Detail: "present, JSON output OK"}
}

// checkPing verifies `ping` is present and is the iputils implementation
// (busybox ping lacks the flags and grammar CableCheck parses).
func checkPing(ctx context.Context, r runner.Runner) CheckResult {
	if _, err := r.LookPath("ping"); err != nil {
		return failMissing("ping")
	}
	res, err := r.Run(ctx, runner.CommandSpec{
		Name: "ping", Args: []string{"-V"}, Timeout: probeTimeout, Label: "doctor-ping"})
	if err != nil {
		return CheckResult{Name: "ping", Status: CheckFail, Detail: "could not probe ping version"}
	}
	variant := firstLine(res.Stdout)
	if variant == "" {
		variant = firstLine(res.Stderr)
	}
	if !strings.Contains(variant, "iputils") {
		return CheckResult{Name: "ping", Status: CheckFail,
			Detail: fmt.Sprintf("unsupported ping %q: cablecheck requires iputils ping (busybox is not supported)", variant)}
	}
	return CheckResult{Name: "ping", Status: CheckPass, Detail: variant}
}

// checkIperf3 verifies iperf3 is present, within the supported version window,
// and reports its detected capabilities. Missing = FAIL with distro hints.
func checkIperf3(ctx context.Context, r runner.Runner) CheckResult {
	if _, err := r.LookPath("iperf3"); err != nil {
		return failMissing("iperf3")
	}
	caps, err := testsuite.DetectIperfCaps(ctx, r)
	if err != nil {
		return CheckResult{Name: "iperf3", Status: CheckFail, Detail: err.Error()}
	}
	feats := []string{"JSON"}
	if caps.Bidir {
		feats = append(feats, "bidir")
	}
	if caps.Reverse {
		feats = append(feats, "reverse")
	}
	if caps.UDP {
		feats = append(feats, "UDP")
	}
	if caps.GetServerOutput {
		feats = append(feats, "get-server-output")
	}
	return CheckResult{Name: "iperf3", Status: CheckPass,
		Detail: fmt.Sprintf("version %s (%s)", caps.Version, strings.Join(feats, ", "))}
}

// checkEthtool verifies ethtool is present. Preflight requires ethtool for the
// counter and link-state reads, so its absence is a FAIL (docs/design/
// clieval.md §6 decision).
func checkEthtool(ctx context.Context, r runner.Runner) CheckResult {
	if _, err := r.LookPath("ethtool"); err != nil {
		return failMissing("ethtool")
	}
	res, err := r.Run(ctx, runner.CommandSpec{
		Name: "ethtool", Args: []string{"--version"}, Timeout: probeTimeout, Label: "doctor-ethtool"})
	if err != nil {
		return CheckResult{Name: "ethtool", Status: CheckFail, Detail: "could not probe ethtool version"}
	}
	return CheckResult{Name: "ethtool", Status: CheckPass, Detail: firstLine(res.Stdout)}
}

// checkInterfaces enumerates the machine's interfaces via `ip -j addr show`,
// classifies each against sysfs, and reports one PASS line per physical NIC
// plus annotations for virtual ones. No physical candidate (optionally after
// the --interface filter) → a single WARN.
func checkInterfaces(ctx context.Context, r runner.Runner, opts DoctorOptions) []CheckResult {
	res, err := r.Run(ctx, runner.CommandSpec{
		Name: "ip", Args: []string{"-j", "addr", "show"}, Timeout: probeTimeout, Label: "doctor-ip-inventory"})
	if err != nil || res.Failed() {
		return []CheckResult{{Name: "interfaces", Status: CheckWarn,
			Detail: "could not enumerate interfaces (`ip -j addr show` failed)"}}
	}
	links, perr := parser.ParseIPAddr(res.Stdout)
	if perr != nil {
		return []CheckResult{{Name: "interfaces", Status: CheckWarn,
			Detail: "could not parse the interface inventory"}}
	}

	var out []CheckResult
	physical := 0
	for _, l := range links {
		if opts.Interface != "" && l.Ifname != opts.Interface {
			continue
		}
		cls := network.Classify(opts.SysfsRoot, l.Ifname)
		name := "interface " + l.Ifname
		detail := interfaceDetail(l, cls)
		switch {
		case cls.Loopback, cls.Virtual, cls.Wireless:
			annotation := " (virtual; requires --allow-virtual-interface)"
			if opts.AllowVirtual {
				annotation = " (virtual; allowed by --allow-virtual-interface)"
			}
			out = append(out, CheckResult{Name: name, Status: CheckWarn,
				Detail: detail + annotation})
		default:
			physical++
			out = append(out, CheckResult{Name: name, Status: CheckPass, Detail: detail})
		}
	}
	if physical == 0 {
		out = append(out, CheckResult{Name: "interface (physical)", Status: CheckWarn,
			Detail: "no physical Ethernet interface found; a cable test needs a wired NIC"})
	}
	// Stable order: physical PASS lines first (already in ip order), then the
	// summary WARN if any. ip already yields a deterministic order.
	return out
}

// interfaceDetail formats an interface inventory line: state, IPv4 addresses,
// driver, MTU.
func interfaceDetail(l parser.IPLink, cls network.Class) string {
	var addrs []string
	for _, a := range l.AddrInfo {
		if a.Family == "inet" {
			addrs = append(addrs, a.Local)
		}
	}
	sort.Strings(addrs)
	parts := []string{"state " + orUnknownStr(l.Operstate)}
	if len(addrs) > 0 {
		parts = append(parts, "ipv4 "+strings.Join(addrs, ","))
	}
	if cls.Driver != "" {
		parts = append(parts, "driver "+cls.Driver)
	}
	if l.MTU > 0 {
		parts = append(parts, fmt.Sprintf("mtu %d", l.MTU))
	}
	return strings.Join(parts, ", ")
}

// checkSudo probes passwordless sudo; a failure is a WARN (root is only needed
// for the opt-in cable tests). Skipped with --no-sudo.
func checkSudo(ctx context.Context, r runner.Runner, opts DoctorOptions) CheckResult {
	if opts.NoSudo {
		return CheckResult{Name: "sudo", Status: CheckPass, Detail: "skipped (--no-sudo)"}
	}
	if testsuite.SudoProbe(ctx, r) {
		return CheckResult{Name: "sudo", Status: CheckPass, Detail: "passwordless sudo available"}
	}
	return CheckResult{Name: "sudo", Status: CheckWarn,
		Detail: "passwordless sudo unavailable; cable diagnostics (--cable-test) would be skipped"}
}

// checkCableTestSupport reports cable-test support conservatively: doctor
// never runs `ethtool --cable-test`, and driver support varies, so this is
// always a WARN advising the operator that only a live run can confirm it.
func checkCableTestSupport() CheckResult {
	return CheckResult{Name: "cable-test support", Status: CheckWarn,
		Detail: "cannot verify without running — driver and NIC support varies (ethtool >= 5.4 and a supporting driver)"}
}

// checkOutputWritable probes the output directory by creating and removing a
// temp file; failure is a FAIL (a run could not persist its report).
func checkOutputWritable(dir string) CheckResult {
	if dir == "" {
		dir = "."
	}
	probe, err := os.CreateTemp(dir, ".cablecheck-writable-*")
	if err != nil {
		return CheckResult{Name: "output directory", Status: CheckFail,
			Detail: fmt.Sprintf("%s is not writable: %v", dir, err)}
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	return CheckResult{Name: "output directory", Status: CheckPass, Detail: dir + " is writable"}
}

// failMissing builds the FAIL result for a required tool that is not on PATH,
// with the distro install hints.
func failMissing(name string) CheckResult {
	for _, t := range requiredTools {
		if t.name == name {
			return CheckResult{Name: name, Status: CheckFail, Detail: fmt.Sprintf(
				"not found in PATH; install it: pacman -S %s | apt install %s | dnf install %s",
				t.pacman, t.apt, t.dnf)}
		}
	}
	return CheckResult{Name: name, Status: CheckFail, Detail: "not found in PATH"}
}

// countFail counts FAIL checks.
func countFail(checks []CheckResult) int {
	n := 0
	for _, c := range checks {
		if c.Status == CheckFail {
			n++
		}
	}
	return n
}

// orUnknownStr returns s, or "unknown" when empty.
func orUnknownStr(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// RenderDoctor writes the aligned check lines and the summary to w.
func RenderDoctor(w io.Writer, checks []CheckResult) {
	width := 0
	for _, c := range checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	var pass, warn, fail int
	for _, c := range checks {
		switch c.Status {
		case CheckPass:
			pass++
		case CheckWarn:
			warn++
		case CheckFail:
			fail++
		}
		fmt.Fprintf(w, "[%s] %-*s  %s\n", c.Status, width, c.Name, c.Detail)
	}
	fmt.Fprintf(w, "\n%s\n", formatDoctorSummary(pass, warn, fail))
}

// formatDoctorSummary renders the "N PASS, N WARN, N FAIL" tally line.
func formatDoctorSummary(pass, warn, fail int) string {
	return fmt.Sprintf("%d PASS, %d WARN, %d FAIL", pass, warn, fail)
}
