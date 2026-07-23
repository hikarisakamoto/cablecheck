package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/config"
	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner/runnertest"
	"cablecheck/internal/testsuite"
	"cablecheck/internal/testutil"
	"cablecheck/internal/ui"
)

// scriptQuickRun adds the test-plan scripts a full quick run needs on top of
// the preflight set.
func scriptQuickRun(fr *runnertest.FakeRunner) {
	fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("-S", "eth0"),
		StdoutFile: fixture("ethtool", "stats_e1000e_clean.txt")})
	fr.Script(runnertest.Script{Name: "ip", Match: runnertest.ArgsPrefix("-j", "-s", "-s"),
		StdoutFile: fixture("ip", "linkstats_clean.json")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("ping", "quick_clean_100.txt")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		StdoutFile: fixture("ping", "fullsize_ok.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixture("iperf", "server_listening.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("iperf", "tcp_316_fwd.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("--bidir"),
		StdoutFile: fixture("iperf", "bidir_314.json")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-u"),
		StdoutFile: fixture("iperf", "udp_316.json")})
}

// syncBuffer is a mutex-guarded bytes.Buffer safe for concurrent writers.
// The run writes to Stdout via raw fmt from several goroutines (token banner
// and summary on the app goroutine, countdown/status lines on the peer event
// loop) while slog writes to Stderr under its own handler mutex — so the two
// sinks must be independently synchronized and must never share a bare
// bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer under the buffer's lock.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the accumulated contents under the buffer's lock.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runOutput collects one side's operator output and logging in separate,
// concurrency-safe sinks.
type runOutput struct {
	stdout, stderr syncBuffer
}

// dump renders both sinks for failure messages.
func (o *runOutput) dump() string {
	return "stdout:\n" + o.stdout.String() + "\nstderr:\n" + o.stderr.String()
}

// countdownAdvance comfortably covers the peer session's 3.5s synchronized
// start countdown while staying below the 5s heartbeat interval, so one
// Advance fires every countdown tick and nothing else.
const countdownAdvance = 4 * time.Second

// fireCountdown releases one side's synchronized-start countdown on its fake
// clock: it parks until the session's two pre-testing clock waiters exist —
// the heartbeat ticker and the armed countdown timer, the only registrations
// before the testing phase — then advances past the whole countdown. Ticks
// re-armed in the past fire immediately, so a single Advance drives
// 3-2-1-GO without any wall-clock wait.
func fireCountdown(clk *clocktest.FakeClock) {
	clk.BlockUntilWaiters(2)
	clk.Advance(countdownAdvance)
}

// waitForState consumes states until want is observed, failing the test when
// the budget expires first. Test-goroutine use only.
func waitForState(t *testing.T, states <-chan peer.State, want peer.State) {
	t.Helper()
	timer := time.NewTimer(testutil.TestTimeout(t))
	defer timer.Stop()
	for {
		select {
		case st := <-states:
			if st == want {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for peer state %q", want)
		}
	}
}

// drainStates returns every state currently buffered on ch. After Wait
// returned, every transition has been pushed (the onState hook fires
// synchronously inside the session before Run returns), so this is the
// complete remaining sequence.
func drainStates(ch <-chan peer.State) []peer.State {
	var out []peer.State
	for {
		select {
		case st := <-ch:
			out = append(out, st)
		default:
			return out
		}
	}
}

// newRunApp builds one side of the in-process two-peer run on its own fake
// clock. The returned FakeRunner accepts additional per-test scripts (most
// recent wins); the returned channel observes every peer session state
// transition through the Deps.hooks.onState seam — THE sync primitive of the
// integration harness (docs/design/testing.md §3).
func newRunApp(t *testing.T, role config.Role, controlPort, iperfPort uint16,
	out *runOutput, clk clock.Clock, onStep func(int, int, string), onRunEnd func(),
	onProgress ...func(protocol.TestProgress)) (*App, *runnertest.FakeRunner, <-chan peer.State) {
	t.Helper()
	fr := runnertest.New(t)
	scriptPreflightTools(fr)
	scriptQuickRun(fr)
	cfg := &config.RunConfig{
		Role:            role,
		LocalIP:         netip.MustParseAddr("127.0.0.1"),
		PeerIP:          netip.MustParseAddr("127.0.0.1"),
		Mode:            config.ModeQuick,
		ControlPort:     controlPort,
		IperfPort:       iperfPort,
		Token:           "testtoken1234",
		TCPDuration:     30_000_000_000,
		UDPDuration:     20_000_000_000,
		ParallelStreams: 4,
		PingCount:       100,
		TCPRepeats:      1,
		OutputDir:       t.TempDir(),
		NonInteractive:  true,
		NoSudo:          true,
	}
	states := make(chan peer.State, 32) // > the longest possible transition sequence
	var progressObserver func(protocol.TestProgress)
	if len(onProgress) > 0 {
		progressObserver = onProgress[0]
	}
	a, err := New(cfg, Deps{
		Runner:     fr,
		Clock:      clk,
		Stdout:     &out.stdout,
		Stderr:     &out.stderr,
		StateDir:   t.TempDir(),
		Build:      BuildInfo{Version: "test"},
		OnStep:     onStep,
		OnProgress: progressObserver,
		OnRunEnd:   onRunEnd,
		hooks: testHooks{onState: func(st peer.State) {
			// Never block the session's event loop: overflow is loud, not
			// a deadlock. Errorf only — this runs on session goroutines.
			select {
			case states <- st:
			default:
				t.Errorf("onState channel overflow: dropped %q", st)
			}
		}},
	})
	if err != nil {
		t.Fatalf("New(%s): %v", role, err)
	}
	root := fakeSysfs(t, "e1000e")
	if err := os.WriteFile(filepath.Join(root, "eth0", "carrier_changes"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write carrier_changes: %v", err)
	}
	a.sysfsRoot = root
	return a, fr, states
}

// TestRunQuickHappyPath wires two Apps together in-process over a real TCP
// loopback control connection with fake runners on both sides and asserts
// the full vertical slice: both exit 0, PC1 renders a complete report with
// the [n/N] step callbacks fired in order, PC2 renders its local summary
// from the complete payload.
func TestRunQuickHappyPath(t *testing.T) {
	defer testutil.LeakCheck(t)
	ctx := context.Background()

	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	clk1, clk2 := clocktest.New(start), clocktest.New(start)
	var out1, out2 runOutput
	var steps1, steps2 []string
	var progressMu sync.Mutex
	var progress1 []protocol.TestProgress
	var runEndCalls int
	var summaryWrittenAtRunEnd bool
	var runEnded bool
	var summaryCalls int
	var summaryAfterRunEnd bool
	var presentedReport *model.Report
	onRunEnd := func() {
		// OnRunEnd must fire after the session ends but before the end-of-run
		// summary is written to Stdout: only then can the cli stop live progress
		// rendering without the summary landing under a stale bar. Runs on the
		// run goroutine, so reading out1 here is unraced.
		runEndCalls++
		summaryWrittenAtRunEnd = strings.Contains(out1.stdout.String(), "Report:")
		runEnded = true
	}
	pc1, _, states1 := newRunApp(t, config.RolePC1, 0, 45301, &out1, clk1, func(n, total int, name string) {
		steps1 = append(steps1, fmt.Sprintf("[%d/%d] %s", n, total, name))
	}, onRunEnd, func(p protocol.TestProgress) {
		progressMu.Lock()
		progress1 = append(progress1, p)
		progressMu.Unlock()
	})
	pc1Presenter := ui.New(&out1.stdout, ui.Options{Color: ui.ColorAuto, Width: 80})
	pc1.deps.OnSummary = func(rep *model.Report, dir string) {
		summaryCalls++
		summaryAfterRunEnd = runEnded
		presentedReport = rep
		pc1Presenter.Summary(rep, dir)
	}
	pc1.deps.OnTokenBanner = pc1Presenter.TokenBanner
	if err := pc1.Start(ctx); err != nil {
		t.Fatalf("pc1 Start: %v", err)
	}
	ctrl, ok := pc1.ControlAddr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("pc1 ControlAddr = %v, want *net.TCPAddr", pc1.ControlAddr())
	}
	if ctrl.Port == 0 {
		t.Fatalf("pc1 bound port is 0; --control-port 0 support broken")
	}

	pc2, _, states2 := newRunApp(t, config.RolePC2, uint16(ctrl.Port), 45311, &out2, clk2, func(n, total int, name string) {
		steps2 = append(steps2, fmt.Sprintf("[%d/%d] %s", n, total, name))
	}, nil)
	pc2Presenter := ui.New(&out2.stdout, ui.Options{Color: ui.ColorAuto, Width: 80})
	pc2.deps.OnWorkerSummary = pc2Presenter.WorkerSummary
	if err := pc2.Start(ctx); err != nil {
		t.Fatalf("pc2 Start: %v", err)
	}

	// The worker must be in testing before the coordinator's plan fires its
	// first RPC (a test_request in state ready draws an invalid-state
	// strike), so PC2's countdown fires first and the onState channel proves
	// the transition landed before PC1 GOes — no sleeps, no wall-clock.
	fireCountdown(clk2)
	waitForState(t, states2, peer.StateTesting)
	fireCountdown(clk1)

	code2, err2 := pc2.Wait()
	code1, err1 := pc1.Wait()
	if err1 != nil || code1 != ExitOK {
		t.Fatalf("pc1 = (%d, %v), want (0, nil)\npc1 %s\npc2 %s", code1, err1, out1.dump(), out2.dump())
	}
	if err2 != nil || code2 != ExitOK {
		t.Fatalf("pc2 = (%d, %v), want (0, nil)\npc2 %s", code2, err2, out2.dump())
	}

	// Both onState hooks observed the lifecycle through to completed — the
	// hook is the sync primitive the integration suite builds on, so its
	// end-to-end delivery is pinned here. states2 was partially consumed
	// above; the remainder must still end in completed.
	for side, sts := range map[string][]peer.State{
		"pc1": drainStates(states1), "pc2": drainStates(states2),
	} {
		if len(sts) == 0 || sts[len(sts)-1] != peer.StateCompleted {
			t.Errorf("%s onState observed %v, want a sequence ending in %q", side, sts, peer.StateCompleted)
		}
	}

	// Both consoles receive identical step callbacks once per plan step.
	wantSteps := []string{
		"link settings", "initial counter snapshot",
		"ping stability", "full-size ping",
		"TCP throughput PC1 → PC2", "TCP throughput PC2 → PC1",
		"bidirectional stress", "UDP loss and jitter",
		"final counter snapshot",
	}
	if len(steps1) != len(wantSteps) || len(steps2) != len(wantSteps) {
		t.Fatalf("PC1 steps = %q, PC2 steps = %q, want %q", steps1, steps2, wantSteps)
	}
	for i := range wantSteps {
		want := fmt.Sprintf("[%d/%d] %s", i+1, len(wantSteps), wantSteps[i])
		if steps1[i] != want || steps2[i] != want {
			t.Errorf("step %d = PC1 %q, PC2 %q, want %q", i, steps1[i], steps2[i], want)
		}
	}
	progressMu.Lock()
	progressSnapshot := append([]protocol.TestProgress(nil), progress1...)
	progressMu.Unlock()
	if len(progressSnapshot) == 0 {
		t.Fatal("PC1 observed no worker TestProgress updates")
	}
	foundLinkProgress := false
	for _, p := range progressSnapshot {
		if p.Stage == testsuite.OpLinkSettings && p.Percent == -1 && p.Text == "running "+testsuite.OpLinkSettings {
			foundLinkProgress = true
			break
		}
	}
	if !foundLinkProgress {
		t.Errorf("PC1 progress = %+v, want the worker link-settings update", progressSnapshot)
	}

	// OnRunEnd fires exactly once, after the session ends and before the
	// end-of-run summary reaches Stdout — the ordering the cli relies on to
	// clear the live progress bar before the summary prints over it.
	if runEndCalls != 1 {
		t.Errorf("OnRunEnd called %d times, want exactly 1", runEndCalls)
	}
	if summaryWrittenAtRunEnd {
		t.Error("OnRunEnd fired after the summary was written; a live bar would be left above it")
	}
	if summaryCalls != 1 || !summaryAfterRunEnd {
		t.Errorf("OnSummary calls/ordering = %d/%v, want exactly once after OnRunEnd", summaryCalls, summaryAfterRunEnd)
	}

	// PC1 report files exist; report.json unmarshals and is complete.
	dir1 := findReportDir(t, pc1.cfg.OutputDir)
	data, err := os.ReadFile(filepath.Join(dir1, "report.json"))
	if err != nil {
		t.Fatalf("read pc1 report.json: %v", err)
	}
	var rep model.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	if rep.Partial {
		t.Errorf("report.Partial = true, want false")
	}
	if rep.Classification != model.HealthGood && rep.Classification != model.HealthExcellent {
		t.Errorf("classification = %s, want GOOD or EXCELLENT (findings: %v)", rep.Classification, rep.ClassificationReasons)
	}
	if presentedReport == nil || presentedReport.Classification != rep.Classification || presentedReport.PC2.NIC.Name != rep.PC2.NIC.Name {
		t.Errorf("OnSummary report = %+v, want final enriched report class %s with PC2 NIC %q", presentedReport, rep.Classification, rep.PC2.NIC.Name)
	}
	for side, output := range map[string]string{"pc1": out1.stdout.String(), "pc2": out2.stdout.String()} {
		if strings.Contains(output, "\x1b[") {
			t.Errorf("%s non-TTY output contains ANSI: %q", side, output)
		}
	}
	for _, want := range []string{"Session token:", "CableCheck result", "cable health:", "Report: "} {
		if !strings.Contains(out1.stdout.String(), want) {
			t.Errorf("pc1 rendered output missing %q:\n%s", want, out1.stdout.String())
		}
	}
	for _, want := range []string{"verdict from PC1:", "Report received from PC1:"} {
		if !strings.Contains(out2.stdout.String(), want) {
			t.Errorf("pc2 rendered output missing %q:\n%s", want, out2.stdout.String())
		}
	}
	if rep.TestID == "" {
		t.Errorf("report.TestID is empty")
	}
	// The handshake callback captured the test ID on both sides: PC1's copy feeds partial reports
	// (assembleReport falls back to it when no Outcome exists yet), PC2's
	// and the worker summary. rep.TestID alone cannot pin this — the happy-path
	// re-render takes outcome.TestID — so the callback value is asserted directly.
	if got := pc1.sessionTestID(); got == "" || got != rep.TestID {
		t.Errorf("pc1 sessionTestID() = %q, want the report's %q captured by OnHandshake", got, rep.TestID)
	}
	if got := pc2.sessionTestID(); got == "" {
		t.Errorf("pc2 sessionTestID() is empty; OnHandshake captured nothing")
	}
	if len(rep.Tests.TCP) != 2 || len(rep.Tests.Ping) != 2 {
		t.Errorf("tests = %d TCP / %d ping, want 2/2", len(rep.Tests.TCP), len(rep.Tests.Ping))
	}
	if len(rep.Tests.FullSizePing) != 2 {
		t.Errorf("tests carry %d full-size ping results, want both directions", len(rep.Tests.FullSizePing))
	}
	if len(rep.Tests.UDP) != 2 {
		t.Errorf("tests carry %d UDP results, want both directions", len(rep.Tests.UDP))
	} else {
		for _, u := range rep.Tests.UDP {
			// 1G negotiated (the ethtool fixture) → 80% = 800000000.
			if u.TargetBps != 800000000 {
				t.Errorf("UDP %s TargetBps = %d, want the derived 800000000", u.Direction, u.TargetBps)
			}
		}
	}
	if rep.Tests.Bidirectional == nil {
		t.Errorf("tests carry no bidirectional result")
	} else if rep.Tests.Bidirectional.TwoPhaseFallback {
		t.Errorf("bidirectional ran as the two-phase fallback despite --bidir on both peers")
	}
	if rep.PC2.NIC.Name != "eth0" {
		t.Errorf("peer machine info missing: PC2.NIC = %+v", rep.PC2.NIC)
	}
	for _, name := range []string{"report.md", "summary.txt"} {
		if _, err := os.Stat(filepath.Join(dir1, name)); err != nil {
			t.Errorf("pc1 %s: %v", name, err)
		}
	}

	// The token appears in PC1's stdout banner but never in the debug log.
	if !strings.Contains(out1.stdout.String(), "testtoken1234") {
		t.Errorf("pc1 stdout misses the token banner:\n%s", out1.stdout.String())
	}
	logBytes, err := os.ReadFile(filepath.Join(dir1, "raw", "cablecheck-pc1.log"))
	if err != nil {
		t.Fatalf("read pc1 debug log: %v", err)
	}
	if strings.Contains(string(logBytes), "testtoken1234") {
		t.Errorf("token leaked into the debug log")
	}

	// PC2 received PC1's verified report set via the transfer: report.json,
	// report.md and summary.txt, each byte-identical (SHA-256-equal) to PC1's
	// final copy, with no .part scratch files left behind.
	dir2 := findReportDir(t, pc2.cfg.OutputDir)
	for _, name := range []string{"report.json", "report.md", "summary.txt"} {
		a, err := os.ReadFile(filepath.Join(dir1, name))
		if err != nil {
			t.Fatalf("read pc1 %s: %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil {
			t.Errorf("pc2 is missing transferred %s: %v", name, err)
			continue
		}
		if sha256.Sum256(a) != sha256.Sum256(b) {
			t.Errorf("pc2 %s SHA-256 differs from pc1's copy", name)
		}
	}
	for _, e := range mustReadDir(t, dir2) {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("pc2 left a partial file %q", e.Name())
		}
	}
}

func TestTokenBannerUsesEffectivePC2Command(t *testing.T) {
	var out bytes.Buffer
	var gotToken, gotCommand string
	var gotGenerated bool
	cfg := &config.RunConfig{
		Role:                  config.RolePC1,
		LocalIP:               netip.MustParseAddr("192.168.50.1"),
		PeerIP:                netip.MustParseAddr("192.168.50.2"),
		ControlPort:           0,
		IperfPort:             45401,
		Token:                 "123456",
		TokenGenerated:        true,
		AllowVirtualInterface: true,
	}
	a, err := New(cfg, Deps{
		Stdout: &out,
		OnTokenBanner: func(token, command string, generated bool) {
			gotToken, gotCommand, gotGenerated = token, command, generated
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.controlPort = 46234
	a.printTokenBanner()

	if out.Len() != 0 {
		t.Errorf("token hook did not own output: %q", out.String())
	}
	if gotToken != "123456" || !gotGenerated {
		t.Errorf("token callback = %q generated=%v", gotToken, gotGenerated)
	}
	for _, want := range []string{
		"--local-ip 192.168.50.2",
		"--peer-ip 192.168.50.1",
		"--control-port 46234",
		"--iperf-port 45401",
		"--allow-virtual-interface",
		"--token 123456",
	} {
		if !strings.Contains(gotCommand, want) {
			t.Errorf("PC2 command missing %q: %q", want, gotCommand)
		}
	}
}

func TestTokenBannerPlainFallbackAndShellQuoting(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.RunConfig{
		Role: config.RolePC1, LocalIP: netip.MustParseAddr("192.168.50.1"),
		PeerIP: netip.MustParseAddr("192.168.50.2"), ControlPort: 44300,
		IperfPort: 44301, Token: "odd'$;token",
	}
	a, err := New(cfg, Deps{Stdout: &out})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.printTokenBanner()
	got := out.String()
	for _, want := range []string{
		"Session token: odd'$;token",
		"On PC2 run:",
		`--token 'odd'"'"'$;token'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain token banner missing %q: %q", want, got)
		}
	}
}

// TestRunNoReportTransferFinalizesPostCapsReportOnce pins that PC1 merges the
// worker's handshake capabilities and finalizes exactly once before complete,
// even when report transfer is disabled. A virtual PC2 must make both PC1's
// local report and the complete payload received by PC2 INCONCLUSIVE.
func TestRunNoReportTransferFinalizesPostCapsReportOnce(t *testing.T) {
	defer testutil.LeakCheck(t)
	ctx := context.Background()

	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	clk1, clk2 := clocktest.New(start), clocktest.New(start)
	var out1, out2 runOutput
	pc1, _, _ := newRunApp(t, config.RolePC1, 0, 45341, &out1, clk1, nil, nil)
	var finalizations, postCapabilitiesFinalizations int
	pc1.deps.hooks.onFinalize = func(_, hasOutcome bool) {
		finalizations++
		if hasOutcome {
			postCapabilitiesFinalizations++
		}
	}
	pc1.cfg.NoReportTransfer = true
	if err := pc1.Start(ctx); err != nil {
		t.Fatalf("pc1 Start: %v", err)
	}

	ctrl := pc1.ControlAddr().(*net.TCPAddr)
	pc2, _, states2 := newRunApp(t, config.RolePC2, uint16(ctrl.Port), 45351, &out2, clk2, nil, nil)
	pc2.cfg.NoReportTransfer = true
	pc2.cfg.AllowVirtualInterface = true
	virtualRoot := t.TempDir()
	virtualNIC := filepath.Join(virtualRoot, "eth0")
	if err := os.MkdirAll(virtualNIC, 0o755); err != nil {
		t.Fatalf("mkdir virtual NIC: %v", err)
	}
	if err := os.WriteFile(filepath.Join(virtualNIC, "type"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write virtual NIC type: %v", err)
	}
	if err := os.WriteFile(filepath.Join(virtualNIC, "carrier_changes"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write virtual NIC carrier_changes: %v", err)
	}
	pc2.sysfsRoot = virtualRoot
	if err := pc2.Start(ctx); err != nil {
		t.Fatalf("pc2 Start: %v", err)
	}

	fireCountdown(clk2)
	waitForState(t, states2, peer.StateTesting)
	fireCountdown(clk1)

	code2, err2 := pc2.Wait()
	code1, err1 := pc1.Wait()
	if err1 != nil || code1 != ExitInconclusive {
		t.Errorf("pc1 = (%d, %v), want (%d, nil)\npc1 %s\npc2 %s", code1, err1, ExitInconclusive, out1.dump(), out2.dump())
	}
	if err2 != nil || code2 != ExitInconclusive {
		t.Errorf("pc2 = (%d, %v), want (%d, nil); complete payload was not post-caps\npc1 %s\npc2 %s", code2, err2, ExitInconclusive, out1.dump(), out2.dump())
	}
	if postCapabilitiesFinalizations != 1 {
		t.Errorf("post-capabilities finalizations = %d, want exactly 1", postCapabilitiesFinalizations)
	}
	if finalizations != 2 {
		t.Errorf("report finalizations = %d, want 2 (provisional plus one post-capabilities finalization)", finalizations)
	}

	dir1 := findReportDir(t, pc1.cfg.OutputDir)
	data, err := os.ReadFile(filepath.Join(dir1, "report.json"))
	if err != nil {
		t.Fatalf("read pc1 report.json: %v", err)
	}
	var rep model.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal pc1 report.json: %v", err)
	}
	if rep.Classification != model.HealthInconclusive {
		t.Errorf("pc1 report classification = %s, want INCONCLUSIVE", rep.Classification)
	}
	if !slices.Contains(findingRuleIDs(rep.Findings), "HOST-02") {
		t.Errorf("pc1 report findings = %v, want HOST-02", findingRuleIDs(rep.Findings))
	}

	dir2 := findReportDir(t, pc2.cfg.OutputDir)
	summary, err := os.ReadFile(filepath.Join(dir2, "summary.txt"))
	if err != nil {
		t.Fatalf("read pc2 summary.txt: %v", err)
	}
	if !strings.Contains(string(summary), "INCONCLUSIVE") {
		t.Errorf("pc2 summary does not contain the post-caps complete verdict:\n%s", summary)
	}
	if _, err := os.Stat(filepath.Join(dir2, "report.json")); !os.IsNotExist(err) {
		t.Errorf("pc2 report.json exists despite disabled transfer: %v", err)
	}
}

// mustReadDir reads dir or fails the test.
func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	return entries
}

// TestRunInterruptProducesPartialReport cancels PC1's context mid-TCP-test
// (gated on the fake client's Started channel — no sleeps) and asserts the
// interrupt contract: PC1 exits 6 with a partial report carrying
// Partial=true and FailureDetails, PC2 sees the abort and exits 5.
func TestRunInterruptProducesPartialReport(t *testing.T) {
	defer testutil.LeakCheck(t)
	pc1Ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	clk1, clk2 := clocktest.New(start), clocktest.New(start)
	var out1, out2 runOutput
	pc1, fr1, states1 := newRunApp(t, config.RolePC1, 0, 45321, &out1, clk1, nil, nil)
	var finalizations, failureFinalizations int
	var monitorStateMu sync.Mutex
	monitorStopped := false
	pc1.deps.hooks.onFinalize = func(hasFailure, _ bool) {
		finalizations++
		if hasFailure {
			failureFinalizations++
		}
		monitorStateMu.Lock()
		stopped := monitorStopped
		monitorStateMu.Unlock()
		if !stopped {
			t.Errorf("partial report finalized before the link monitor stopped")
		}
	}
	started := make(chan struct{})
	delay := make(chan struct{}) // never closed: the client hangs until cancel
	fr1.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("iperf", "tcp_316_fwd.json"),
		Delay:      delay, Started: started})

	if err := pc1.Start(pc1Ctx); err != nil {
		t.Fatalf("pc1 Start: %v", err)
	}
	ctrl := pc1.ControlAddr().(*net.TCPAddr)
	pc2, _, states2 := newRunApp(t, config.RolePC2, uint16(ctrl.Port), 45331, &out2, clk2, nil, nil)
	if err := pc2.Start(context.Background()); err != nil {
		t.Fatalf("pc2 Start: %v", err)
	}

	// Same countdown choreography as the happy path: worker first.
	fireCountdown(clk2)
	waitForState(t, states2, peer.StateTesting)
	fireCountdown(clk1)

	testutil.WaitFor(t, started, "iperf3 client never started")
	pc1.monitorMu.Lock()
	originalStop := pc1.monitorStop
	if originalStop == nil {
		pc1.monitorMu.Unlock()
		t.Fatal("link monitor did not start before the TCP test")
	}
	pc1.monitorStop = func() {
		originalStop()
		monitorStateMu.Lock()
		monitorStopped = true
		monitorStateMu.Unlock()
	}
	pc1.monitorMu.Unlock()
	cancel()

	code1, err1 := pc1.Wait()
	code2, err2 := pc2.Wait()
	if code1 != ExitInterrupt {
		t.Errorf("pc1 = (%d, %v), want exit 6\n%s", code1, err1, out1.dump())
	}
	if code2 != ExitPeer {
		t.Errorf("pc2 = (%d, %v), want exit 5\n%s", code2, err2, out2.dump())
	}
	if finalizations != 1 || failureFinalizations != 1 {
		t.Errorf("finalizations = %d (%d with failure), want one failure finalization", finalizations, failureFinalizations)
	}

	// The abort reached both onState hooks: PC1 aborts locally, the abort
	// frame drives PC2 to aborted — the observation T12's SIGINT scenario
	// synchronizes on.
	if sts := drainStates(states1); len(sts) == 0 || sts[len(sts)-1] != peer.StateAborted {
		t.Errorf("pc1 onState observed %v, want a sequence ending in %q", sts, peer.StateAborted)
	}
	if sts := drainStates(states2); len(sts) == 0 || sts[len(sts)-1] != peer.StateAborted {
		t.Errorf("pc2 onState observed %v, want a sequence ending in %q", sts, peer.StateAborted)
	}

	dir1 := findReportDir(t, pc1.cfg.OutputDir)
	data, err := os.ReadFile(filepath.Join(dir1, "report.json"))
	if err != nil {
		t.Fatalf("read partial report.json: %v", err)
	}
	var rep model.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal partial report: %v", err)
	}
	if !rep.Partial {
		t.Errorf("partial report has Partial = false")
	}
	if rep.Failure == nil || rep.Failure.Error == "" {
		t.Errorf("partial report misses FailureDetails: %+v", rep.Failure)
	}
}

// TestBuildSuiteRegistryFailureWarns: when the pidfile registry cannot be
// created the run continues, but the degradation must be logged — otherwise
// iperf3 servers run unregistered and a crashed run leaves orphans the next
// run's stale-process preflight scan cannot detect, with no trace of why.
func TestBuildSuiteRegistryFailureWarns(t *testing.T) {
	base := t.TempDir()
	notADir := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", notADir) // registry base dir cannot be created

	cfg := &config.RunConfig{
		Role:    config.RolePC1,
		LocalIP: netip.MustParseAddr("127.0.0.1"),
		PeerIP:  netip.MustParseAddr("127.0.0.1"),
		Mode:    config.ModeQuick,
		Token:   "testtoken1234",
	}
	// StateDir stays empty: that is the production branch that creates the
	// pidfile registry.
	a, err := New(cfg, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	pf := &preflightInfo{}
	pf.Iface.Name = "eth0"

	s := a.buildSuite(pf, t.TempDir(), "run-test-1", log)
	if s.reg != nil {
		t.Fatalf("registry created despite an unwritable base dir")
	}
	if !strings.Contains(logBuf.String(), "registry") {
		t.Errorf("registry creation failure left no log trace; log:\n%s", logBuf.String())
	}
}

// findReportDir returns the single cablecheck-report-* directory under base.
func findReportDir(t *testing.T, base string) string {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read %s: %v", base, err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "cablecheck-report-") {
			return filepath.Join(base, e.Name())
		}
	}
	t.Fatalf("no report directory under %s", base)
	return ""
}
