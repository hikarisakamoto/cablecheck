package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/config"
	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/runner/runnertest"
	"cablecheck/internal/testutil"
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
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixture("iperf", "server_listening.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("iperf", "tcp_316_fwd.json")})
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
	out *runOutput, clk clock.Clock, onStep func(int, int, string)) (*App, *runnertest.FakeRunner, <-chan peer.State) {
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
	a, err := New(cfg, Deps{
		Runner:   fr,
		Clock:    clk,
		Stdout:   &out.stdout,
		Stderr:   &out.stderr,
		StateDir: t.TempDir(),
		Build:    BuildInfo{Version: "test"},
		OnStep:   onStep,
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
	var steps []string
	pc1, _, states1 := newRunApp(t, config.RolePC1, 0, 45301, &out1, clk1, func(n, total int, name string) {
		steps = append(steps, name)
		_ = total
	})
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

	pc2, _, states2 := newRunApp(t, config.RolePC2, uint16(ctrl.Port), 45311, &out2, clk2, nil)
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

	// PC1's step callbacks fired once per plan step, in order.
	wantSteps := []string{
		"link settings", "initial counter snapshot", "ping stability",
		"TCP throughput PC1 → PC2", "TCP throughput PC2 → PC1", "final counter snapshot",
	}
	if len(steps) != len(wantSteps) {
		t.Fatalf("steps = %q, want %q", steps, wantSteps)
	}
	for i := range wantSteps {
		if steps[i] != wantSteps[i] {
			t.Errorf("step %d = %q, want %q", i, steps[i], wantSteps[i])
		}
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
	if rep.TestID == "" {
		t.Errorf("report.TestID is empty")
	}
	// The sessionWatch captured the test ID from the session's "handshake
	// complete" log record on both sides: PC1's copy feeds partial reports
	// (assembleReport falls back to it when no Outcome exists yet), PC2's
	// the worker summary. rep.TestID alone cannot pin this — the happy-path
	// re-render takes outcome.TestID — so the watch is asserted directly.
	if got := pc1.watch.TestID(); got == "" || got != rep.TestID {
		t.Errorf("pc1 watch.TestID() = %q, want the report's %q captured from the session log", got, rep.TestID)
	}
	if got := pc2.watch.TestID(); got == "" {
		t.Errorf("pc2 watch.TestID() is empty; the log-watch seam captured nothing")
	}
	if len(rep.Tests.TCP) != 2 || len(rep.Tests.Ping) != 2 {
		t.Errorf("tests = %d TCP / %d ping, want 2/2", len(rep.Tests.TCP), len(rep.Tests.Ping))
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

	// PC2 wrote its local summary from the complete payload.
	dir2 := findReportDir(t, pc2.cfg.OutputDir)
	sum, err := os.ReadFile(filepath.Join(dir2, "summary.txt"))
	if err != nil {
		t.Fatalf("read pc2 summary.txt: %v", err)
	}
	if !strings.Contains(string(sum), "verdict from PC1") {
		t.Errorf("pc2 summary misses the PC1 verdict:\n%s", sum)
	}
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
	pc1, fr1, states1 := newRunApp(t, config.RolePC1, 0, 45321, &out1, clk1, nil)
	started := make(chan struct{})
	delay := make(chan struct{}) // never closed: the client hangs until cancel
	fr1.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("iperf", "tcp_316_fwd.json"),
		Delay:      delay, Started: started})

	if err := pc1.Start(pc1Ctx); err != nil {
		t.Fatalf("pc1 Start: %v", err)
	}
	ctrl := pc1.ControlAddr().(*net.TCPAddr)
	pc2, _, states2 := newRunApp(t, config.RolePC2, uint16(ctrl.Port), 45331, &out2, clk2, nil)
	if err := pc2.Start(context.Background()); err != nil {
		t.Fatalf("pc2 Start: %v", err)
	}

	// Same countdown choreography as the happy path: worker first.
	fireCountdown(clk2)
	waitForState(t, states2, peer.StateTesting)
	fireCountdown(clk1)

	testutil.WaitFor(t, started, "iperf3 client never started")
	cancel()

	code1, err1 := pc1.Wait()
	code2, err2 := pc2.Wait()
	if code1 != ExitInterrupt {
		t.Errorf("pc1 = (%d, %v), want exit 6\n%s", code1, err1, out1.dump())
	}
	if code2 != ExitPeer {
		t.Errorf("pc2 = (%d, %v), want exit 5\n%s", code2, err2, out2.dump())
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
