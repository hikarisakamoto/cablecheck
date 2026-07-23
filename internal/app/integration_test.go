package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/config"
	"cablecheck/internal/model"
	"cablecheck/internal/network"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
	"cablecheck/internal/testutil"
)

// Integration-harness tunables (docs/design/testing.md §3): real clock, so
// the session timers are configured short via the App's test seams.
const (
	// intHeartbeat keeps liveness frames flowing every 100ms so the short
	// idle timeout below never fires on a healthy session.
	intHeartbeat = 100 * time.Millisecond
	// intIdleTimeout bounds every post-handshake frame read; 2s is 20
	// heartbeat intervals, far above any healthy gap, far below the test
	// budget when a side silently dies.
	intIdleTimeout = 2 * time.Second
	// intToken is the shared session token of the matched-token scenarios.
	intToken = "integration-token-1"
)

// markdownSectionHeaders are the 23 fixed report.md section headers
// (docs/design/clieval.md; pinned structurally in internal/reporting too).
var markdownSectionHeaders = []string{
	"## 1. Overall Result",
	"## 2. Score & Rule Evidence",
	"## 3. Session Info",
	"## 4. Machines & Environment",
	"## 5. Interface & Link Negotiation",
	"## 6. Link Events Timeline",
	"## 7. Counter Baseline",
	"## 8. Counter Deltas",
	"## 9. Ping Stability",
	"## 10. Full-Size Ping",
	"## 11. TCP Throughput PC1→PC2",
	"## 12. TCP Throughput PC2→PC1",
	"## 13. Bidirectional Stress",
	"## 14. UDP Loss & Jitter",
	"## 15. CPU Utilization",
	"## 16. Cable Diagnostics",
	"## 17. Monitoring Timeline",
	"## 18. Findings Detail",
	"## 19. Recommendations",
	"## 20. Limitations & Unavailable Tests",
	"## 21. Configuration Used",
	"## 22. Tool Versions",
	"## 23. Raw Artifact Index",
}

// intSide bundles one in-process peer of an integration scenario.
type intSide struct {
	app    *App
	fr     *runnertest.FakeRunner
	out    *runOutput
	states chan peer.State
	cfg    *config.RunConfig
}

// intSpec configures one side of the harness.
type intSpec struct {
	// role selects pc1 (coordinator) or pc2 (worker).
	role config.Role
	// controlPort is the control-channel port: 0 on pc1 (the bound port is
	// read back via ControlAddr), the coordinator's real port on pc2.
	controlPort uint16
	// iperfPort is this side's configured data port (probed by preflight).
	iperfPort uint16
	// mode overrides the default quick mode for this side.
	mode config.Mode
	// token overrides the shared intToken (the mismatch scenario).
	token string
	// interactive enables the stdin prompt; stdin supplies the reader.
	interactive bool
	stdin       io.Reader
	// planGate, on pc1, parks the plan's opening local link inspection
	// until the channel closes. The harness closes it only after observing
	// the worker in state testing via onState: with a real clock the two
	// GO ticks cannot be ordered any other way, and an early first RPC
	// would draw the worker's invalid-state strike.
	planGate <-chan struct{}
	// noReportTransfer sets --no-report-transfer on this side.
	noReportTransfer bool
	// mangleReportChunk, on pc1, corrupts outbound report chunks at the
	// sender boundary (the report-transfer corruption scenario).
	mangleReportChunk func([]byte) []byte
	// noBidirCaps makes this side's iperf3 report no --bidir support (its
	// `iperf3 --help` fixture lacks the flag), driving the two-phase
	// bidirectional fallback.
	noBidirCaps bool
	// stalePidfile, on the worker, is a live-looking iperf3 pidfile seeded
	// into StateDir before the run so the stale-process preflight fails.
	stalePidfile bool
}

// newIntSide builds one side: healthy fixture scripts, real clock, short
// session timers, fake sysfs, an onState observation channel, and t.TempDir
// report/state dirs (docs/design/testing.md §3).
func newIntSide(t *testing.T, spec intSpec) *intSide {
	t.Helper()
	fr := runnertest.New(t)
	scriptHealthyQuick(t, fr, string(spec.role))
	if spec.noBidirCaps {
		// Override the healthy `iperf3 --help` fixture (most-recent wins) so
		// this side reports no --bidir support; the effective capability is
		// the AND of both peers, driving the two-phase fallback.
		fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--help"),
			StdoutFile: fixture("iperf", "help_no_bidir.txt")})
	}
	if spec.planGate != nil {
		// Times-limited scripts are consumed before the unlimited fixture
		// script and FIFO among themselves: the first serves preflight
		// ungated, the second blocks the plan's first local command — and
		// with it the coordinator's first RPC — on the gate.
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
			StdoutFile: fixture("ethtool", "settings_e1000e_1g.txt"), Times: 1})
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
			StdoutFile: fixture("ethtool", "settings_e1000e_1g.txt"), Times: 1,
			Delay: spec.planGate})
	}
	token := spec.token
	if token == "" {
		token = intToken
	}
	mode := spec.mode
	if mode == "" {
		mode = config.ModeQuick
	}
	cfg := &config.RunConfig{
		Role:             spec.role,
		LocalIP:          netip.MustParseAddr("127.0.0.1"),
		PeerIP:           netip.MustParseAddr("127.0.0.1"),
		Mode:             mode,
		ControlPort:      spec.controlPort,
		IperfPort:        spec.iperfPort,
		Token:            token,
		TCPDuration:      30 * time.Second,
		UDPDuration:      20 * time.Second,
		ParallelStreams:  4,
		PingCount:        100,
		TCPRepeats:       1,
		OutputDir:        t.TempDir(),
		NonInteractive:   !spec.interactive,
		NoSudo:           true,
		NoReportTransfer: spec.noReportTransfer,
	}
	out := &runOutput{}
	states := make(chan peer.State, 32) // > the longest transition sequence
	stateDir := t.TempDir()
	if spec.stalePidfile {
		seedStalePidfile(t, stateDir)
	}
	a, err := New(cfg, Deps{
		Runner:   fr,
		Stdin:    spec.stdin,
		Stdout:   &out.stdout,
		Stderr:   &out.stderr,
		StateDir: stateDir,
		Build:    BuildInfo{Version: "test"},
		hooks: testHooks{
			onState: func(st peer.State) {
				// Never block the session's event loop; Errorf only — this
				// runs on session goroutines.
				select {
				case states <- st:
				default:
					t.Errorf("onState channel overflow: dropped %q", st)
				}
			},
			mangleReportChunk: spec.mangleReportChunk,
		},
	})
	if err != nil {
		t.Fatalf("New(%s): %v", spec.role, err)
	}
	root := fakeSysfs(t, "e1000e")
	if err := os.WriteFile(filepath.Join(root, "eth0", "carrier_changes"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write carrier_changes: %v", err)
	}
	a.sysfsRoot = root
	a.heartbeatInterval = intHeartbeat
	a.idleTimeout = intIdleTimeout
	return &intSide{app: a, fr: fr, out: out, states: states, cfg: cfg}
}

// startCoordinator builds and starts PC1 on --control-port 0 and returns it
// together with the actually bound control port.
func startCoordinator(t *testing.T, ctx context.Context, spec intSpec) (*intSide, uint16) {
	t.Helper()
	spec.role = config.RolePC1
	spec.controlPort = 0
	s := newIntSide(t, spec)
	if err := s.app.Start(ctx); err != nil {
		t.Fatalf("pc1 Start: %v", err)
	}
	addr, ok := s.app.ControlAddr().(*net.TCPAddr)
	if !ok || addr.Port == 0 {
		t.Fatalf("pc1 ControlAddr = %v, want a bound *net.TCPAddr", s.app.ControlAddr())
	}
	return s, uint16(addr.Port)
}

// startWorker builds and starts PC2 against the coordinator's control port.
func startWorker(t *testing.T, ctx context.Context, spec intSpec, controlPort uint16) *intSide {
	t.Helper()
	spec.role = config.RolePC2
	spec.controlPort = controlPort
	s := newIntSide(t, spec)
	if err := s.app.Start(ctx); err != nil {
		t.Fatalf("pc2 Start: %v", err)
	}
	return s
}

// readReport loads PC1's report.json from the single report directory under
// outputDir, returning the parsed report and the directory.
func readReport(t *testing.T, outputDir string) (*model.Report, string) {
	t.Helper()
	dir := findReportDir(t, outputDir)
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var rep model.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	return &rep, dir
}

// assertQuickTrafficCalls asserts the full traffic suite's tool invocations
// were recorded on one side's runner: the full-size -M do ping with the
// MTU-derived -s 1472 payload, the UDP client with `-u -b 800000000` (80% of
// the fixture's 1 Gb/s link, plain integer) and `-l 1472` (MTU-28), and — on
// the coordinator — the native --bidir client.
func assertQuickTrafficCalls(t *testing.T, side *intSide, wantBidirClient bool) {
	t.Helper()
	var fullSize, udp, bidir bool
	contains := func(args []string, toks ...string) bool {
		for _, tok := range toks {
			if !slices.Contains(args, tok) {
				return false
			}
		}
		return true
	}
	for _, c := range side.fr.Calls() {
		switch c.Name {
		case "ping":
			if contains(c.Args, "-M", "do", "-s", "1472") {
				fullSize = true
			}
		case "iperf3":
			if contains(c.Args, "-u", "-b", "800000000", "-l", "1472") {
				udp = true
			}
			if contains(c.Args, "-c", "--bidir") {
				bidir = true
			}
		}
	}
	if !fullSize {
		t.Errorf("no full-size ping call (-M do -s 1472) recorded")
	}
	if !udp {
		t.Errorf("no UDP iperf3 client call (-u -b 800000000 -l 1472) recorded")
	}
	if bidir != wantBidirClient {
		t.Errorf("--bidir client call recorded = %v, want %v", bidir, wantBidirClient)
	}
}

// assertHealthyArtifacts asserts PC1's complete happy-path artifact set:
// report.json parses with a passing classification and the full traffic
// suite's results, report.md and report.html carry all 23 sections,
// summary.txt exists, and raw/ is non-empty.
func assertHealthyArtifacts(t *testing.T, pc1 *intSide) {
	t.Helper()
	rep, dir := readReport(t, pc1.cfg.OutputDir)
	if rep.Partial {
		t.Errorf("report.Partial = true, want false")
	}
	wantUDP := 2
	if pc1.cfg.Mode == config.ModeStandard {
		wantUDP = 4
	}
	if len(rep.Tests.FullSizePing) != 2 || len(rep.Tests.UDP) != wantUDP {
		t.Errorf("tests = %d full-size ping / %d UDP, want 2/%d",
			len(rep.Tests.FullSizePing), len(rep.Tests.UDP), wantUDP)
	}
	if rep.Tests.Bidirectional == nil {
		t.Errorf("report carries no bidirectional result")
	}
	if rep.Classification != model.HealthGood && rep.Classification != model.HealthExcellent {
		t.Errorf("classification = %s, want GOOD or EXCELLENT (reasons: %v)",
			rep.Classification, rep.ClassificationReasons)
	}
	md, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	for _, h := range markdownSectionHeaders {
		if !strings.Contains(string(md), h) {
			t.Errorf("report.md misses section header %q", h)
		}
	}
	htmlReport, err := os.ReadFile(filepath.Join(dir, "report.html"))
	if err != nil {
		t.Fatalf("read report.html: %v", err)
	}
	if got := strings.Count(string(htmlReport), "<details open>"); got != len(markdownSectionHeaders) {
		t.Errorf("report.html has %d open sections, want %d", got, len(markdownSectionHeaders))
	}
	if _, err := os.Stat(filepath.Join(dir, "summary.txt")); err != nil {
		t.Errorf("summary.txt: %v", err)
	}
	raw, err := os.ReadDir(filepath.Join(dir, "raw"))
	if err != nil {
		t.Fatalf("read raw/: %v", err)
	} else if len(raw) == 0 {
		t.Errorf("raw/ is empty, want at least the debug log")
	}
}

// assertTransferredReportsMatch asserts PC2's report directory contains the
// three allowlisted files and that each is byte-identical (SHA-256-equal) to
// PC1's copy — the end-to-end proof of the verified report transfer.
func assertTransferredReportsMatch(t *testing.T, pc1, pc2 *intSide) {
	t.Helper()
	dir1 := findReportDir(t, pc1.cfg.OutputDir)
	dir2 := findReportDir(t, pc2.cfg.OutputDir)
	for _, name := range []string{"report.json", "report.md", "summary.txt"} {
		a, err := os.ReadFile(filepath.Join(dir1, name))
		if err != nil {
			t.Fatalf("read PC1 %s: %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil {
			t.Errorf("PC2 is missing transferred %s: %v", name, err)
			continue
		}
		if sha256.Sum256(a) != sha256.Sum256(b) {
			t.Errorf("PC2 %s SHA-256 differs from PC1's copy", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir2, "report.html")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PC2 report.html should remain absent: %v", err)
	}
	assertNoPartFilesLeft(t, dir2)
}

// assertNoPartFilesLeft fails if any *.part scratch file survived in dir.
func assertNoPartFilesLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover partial file %q in %s", e.Name(), dir)
		}
	}
}

// TestIntegration runs the in-process two-peer scenarios over a real TCP
// loopback control connection with a real clock and per-side FakeRunners
// (docs/design/testing.md §3). Synchronization is exclusively Script
// Started/Delay channels and the onState hook channel — never time.Sleep.
func TestIntegration(t *testing.T) {
	t.Run("HappyPathQuickTCPOnly", testHappyPathQuickTCPOnly)
	t.Run("SIGINTSimulation", testSIGINTSimulation)
	t.Run("TokenMismatch", testTokenMismatch)
	t.Run("MalformedFrameInjection", testMalformedFrameInjection)
	t.Run("ReportTransferCorruption", testReportTransferCorruption)
	t.Run("NoReportTransferFlag", testNoReportTransferFlag)
	t.Run("NoBidirFallback", testNoBidirFallback)
	t.Run("StaleIperf3Detection", testStaleIperf3Detection)
	t.Run("PeerDisconnectMidTest", testPeerDisconnectMidTest)
}

// testHappyPathQuickTCPOnly runs the full quick plan to double success, in
// the default non-interactive mode and in an interactive variant whose
// "start" lines are typed only after each side was observed waiting for them.
func testHappyPathQuickTCPOnly(t *testing.T) {
	t.Run("NonInteractive", func(t *testing.T) {
		testutil.LeakCheck(t)
		ctx := context.Background()
		gate := make(chan struct{})
		pc1, port := startCoordinator(t, ctx, intSpec{iperfPort: 45601, planGate: gate})
		pc2 := startWorker(t, ctx, intSpec{iperfPort: 45603}, port)

		// The coordinator's plan stays parked on the gate until the worker
		// provably reached testing; only then may the first RPC leave.
		waitForState(t, pc2.states, peer.StateTesting)
		close(gate)

		code2, err2 := pc2.app.Wait()
		code1, err1 := pc1.app.Wait()
		if code1 != ExitOK || err1 != nil {
			t.Fatalf("pc1 = (%d, %v), want (0, nil)\npc1 %s\npc2 %s", code1, err1, pc1.out.dump(), pc2.out.dump())
		}
		if code2 != ExitOK || err2 != nil {
			t.Fatalf("pc2 = (%d, %v), want (0, nil)\npc2 %s", code2, err2, pc2.out.dump())
		}
		for side, states := range map[string]chan peer.State{"pc1": pc1.states, "pc2": pc2.states} {
			if sts := drainStates(states); len(sts) == 0 || sts[len(sts)-1] != peer.StateCompleted {
				t.Errorf("%s onState observed %v, want a tail ending in %q", side, sts, peer.StateCompleted)
			}
		}
		assertHealthyArtifacts(t, pc1)
		// The full traffic suite's tool invocations reached the runners: the
		// MTU-derived full-size ping and UDP client on both sides, and the
		// native --bidir client only on the coordinator.
		assertQuickTrafficCalls(t, pc1, true)
		assertQuickTrafficCalls(t, pc2, false)
		// PC2 received PC1's verified report set (SHA-256-equal, no partials).
		assertTransferredReportsMatch(t, pc1, pc2)
	})

	t.Run("Interactive", func(t *testing.T) {
		testutil.LeakCheck(t)
		ctx := context.Background()
		gate := make(chan struct{})
		stdin1r, stdin1w := io.Pipe()
		stdin2r, stdin2w := io.Pipe()
		// Closing the write ends EOFs the session stdin pumps so they exit
		// before LeakCheck's poll; defers run ahead of all cleanups.
		defer stdin1w.Close()
		defer stdin2w.Close()

		pc1, port := startCoordinator(t, ctx, intSpec{iperfPort: 45611, planGate: gate,
			interactive: true, stdin: stdin1r})
		pc2 := startWorker(t, ctx, intSpec{iperfPort: 45613, interactive: true, stdin: stdin2r}, port)

		// Type "start" on each side only after that side was observed in
		// waiting_for_local_start — the onState channel is the sync point.
		waitForState(t, pc1.states, peer.StateWaitingForLocalStart)
		if _, err := io.WriteString(stdin1w, "start\n"); err != nil {
			t.Fatalf("write start to pc1 stdin: %v", err)
		}
		waitForState(t, pc2.states, peer.StateWaitingForLocalStart)
		if _, err := io.WriteString(stdin2w, "start\n"); err != nil {
			t.Fatalf("write start to pc2 stdin: %v", err)
		}
		waitForState(t, pc2.states, peer.StateTesting)
		close(gate)

		code2, err2 := pc2.app.Wait()
		code1, err1 := pc1.app.Wait()
		if code1 != ExitOK || err1 != nil {
			t.Fatalf("pc1 = (%d, %v), want (0, nil)\npc1 %s\npc2 %s", code1, err1, pc1.out.dump(), pc2.out.dump())
		}
		if code2 != ExitOK || err2 != nil {
			t.Fatalf("pc2 = (%d, %v), want (0, nil)\npc2 %s", code2, err2, pc2.out.dump())
		}
		assertHealthyArtifacts(t, pc1)
		assertQuickTrafficCalls(t, pc1, true)
		assertQuickTrafficCalls(t, pc2, false)
		assertTransferredReportsMatch(t, pc1, pc2)
	})
}

// testSIGINTSimulation cancels the coordinator's context mid-TCP-test (gated
// on the fake iperf3 client's Started channel — signal.NotifyContext lives
// only in main, so ctx cancellation IS the SIGINT path) and asserts the
// interrupt contract: coordinator exit 6 with a Partial=true report, worker
// observes the abort (aborted via onState) and exits 5, and the receiving
// side's one-off iperf3 server FakeProcess was terminated or killed.
func testSIGINTSimulation(t *testing.T) {
	testutil.LeakCheck(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	gate := make(chan struct{})
	pc1, port := startCoordinator(t, ctx1, intSpec{iperfPort: 45621, planGate: gate})

	// The forward-direction client on pc1 hangs on Delay (never closed);
	// Started is the mid-TCP synchronization point.
	clientStarted := make(chan struct{})
	clientHang := make(chan struct{})
	pc1.fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("iperf", "tcp_316_fwd.json"), Times: 1,
		Delay: clientHang, Started: clientStarted})

	pc2 := startWorker(t, context.Background(), intSpec{iperfPort: 45623}, port)
	waitForState(t, pc2.states, peer.StateTesting)
	close(gate)

	testutil.WaitFor(t, clientStarted, "pc1 iperf3 client to start")
	cancel1()

	code1, err1 := pc1.app.Wait()
	code2, err2 := pc2.app.Wait()
	if code1 != ExitInterrupt {
		t.Errorf("pc1 = (%d, %v), want exit 6\n%s", code1, err1, pc1.out.dump())
	}
	if code2 != ExitPeer {
		t.Errorf("pc2 = (%d, %v), want exit 5\n%s", code2, err2, pc2.out.dump())
	}

	// The worker observed the abort: its state stream ends in aborted.
	if sts := drainStates(pc2.states); len(sts) == 0 || sts[len(sts)-1] != peer.StateAborted {
		t.Errorf("pc2 onState observed %v, want a tail ending in %q", sts, peer.StateAborted)
	}

	// The interrupted phase was PC1→PC2, so the receiving side is PC2: its
	// one-off iperf3 server must have been shut down (SIGTERM or SIGKILL),
	// not left behind. The worker only ever Starts the server (clients go
	// through Run), so every started process is a server; the recorded
	// calls double-check that.
	var started int
	for _, c := range pc2.fr.Calls() {
		if c.Kind == "start" {
			started++
			if c.Name != "iperf3" || len(c.Args) == 0 || c.Args[0] != "-s" {
				t.Errorf("pc2 started %s %q, want an iperf3 -s server", c.Name, c.Args)
			}
		}
	}
	procs := pc2.fr.Processes()
	if started == 0 || len(procs) == 0 {
		t.Fatalf("no iperf3 server was started on pc2 (starts: %d, processes: %d)", started, len(procs))
	}
	for _, p := range procs {
		if !p.Terminated() && !p.Killed() {
			t.Errorf("pc2 iperf3 server (pid %d) was neither terminated nor killed", p.PID())
		}
	}

	// PC1 preserved its partial evidence.
	rep, _ := readReport(t, pc1.cfg.OutputDir)
	if !rep.Partial {
		t.Errorf("partial report has Partial = false, want true")
	}
	if rep.Failure == nil || rep.Failure.Error == "" {
		t.Errorf("partial report misses FailureDetails: %+v", rep.Failure)
	}
}

// sendWrongTokenHello performs one raw wrong-token handshake attempt against
// the coordinator and asserts it is answered with abort(auth_failed) — which
// also proves the coordinator was still listening. Detail must be empty:
// nothing that could help guess the token.
func sendWrongTokenHello(t *testing.T, addr string) {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial coordinator: %v", err)
	}
	defer nc.Close()
	conn := protocol.NewConn(nc)
	env, err := protocol.NewEnvelope(protocol.TypeHello, "", "pc2-00000001", protocol.Hello{
		Token:             "still-the-wrong-token",
		Role:              "pc2",
		CablecheckVersion: "test",
		LocalIP:           "127.0.0.1",
		PeerIP:            "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("build hello: %v", err)
	}
	if err := conn.WriteEnvelope(env); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	reply, err := conn.ReadEnvelope()
	if err != nil {
		t.Fatalf("read handshake reply: %v", err)
	}
	if reply.Type != protocol.TypeAbort {
		t.Fatalf("handshake reply type = %s, want abort", reply.Type)
	}
	ab, err := protocol.DecodePayload[protocol.Abort](reply)
	if err != nil {
		t.Fatalf("decode abort: %v", err)
	}
	if ab.Reason != "auth_failed" || ab.Detail != "" {
		t.Errorf("abort = reason %q detail %q, want auth_failed with no detail", ab.Reason, ab.Detail)
	}
}

// testTokenMismatch runs a worker with the wrong token: the worker exits 4
// naming the token in its stderr logging; the coordinator rejects the
// attempt, keeps listening, and exits 5 after its three wrong-token strikes
// are used up (the implemented retry policy).
func testTokenMismatch(t *testing.T) {
	testutil.LeakCheck(t)
	ctx := context.Background()
	pc1, port := startCoordinator(t, ctx, intSpec{iperfPort: 45631})
	pc2 := startWorker(t, ctx, intSpec{iperfPort: 45633, token: "totally-wrong-token"}, port)

	code2, err2 := pc2.app.Wait()
	if code2 != ExitConfig {
		t.Errorf("pc2 = (%d, %v), want exit 4\n%s", code2, err2, pc2.out.dump())
	}
	if !errors.Is(err2, peer.ErrTokenRejected) {
		t.Errorf("pc2 error = %v, want peer.ErrTokenRejected in the chain", err2)
	}
	// "token" must appear in stderr as the actual rejection diagnostic —
	// the redacted `config.token=[REDACTED]` echo alone does not count.
	if got := pc2.out.stderr.String(); !strings.Contains(got, "token rejected") {
		t.Errorf("pc2 stderr does not mention the token rejection:\n%s", got)
	}

	// The coordinator survived strike one and is still listening: two more
	// wrong-token attempts each draw an abort(auth_failed) answer, and the
	// third strike makes it give up.
	addr := pc1.app.ControlAddr().String()
	sendWrongTokenHello(t, addr)
	sendWrongTokenHello(t, addr)

	code1, err1 := pc1.app.Wait()
	if code1 != ExitPeer {
		t.Errorf("pc1 = (%d, %v), want exit 5 after three auth strikes\n%s", code1, err1, pc1.out.dump())
	}
	if !errors.Is(err1, peer.ErrHandshakeFailed) {
		t.Errorf("pc1 error = %v, want peer.ErrHandshakeFailed in the chain", err1)
	}
}

// testMalformedFrameInjection dials the control port raw and writes an
// oversized frame header (0xFFFFFFFF) plus garbage: the coordinator must
// drop that connection without panicking, keep listening, and then complete
// a subsequent legitimate handshake and full run.
func testMalformedFrameInjection(t *testing.T) {
	testutil.LeakCheck(t)
	ctx := context.Background()
	gate := make(chan struct{})
	pc1, port := startCoordinator(t, ctx, intSpec{iperfPort: 45641, planGate: gate})

	nc, err := net.Dial("tcp", pc1.app.ControlAddr().String())
	if err != nil {
		t.Fatalf("dial coordinator: %v", err)
	}
	if err := nc.SetDeadline(time.Now().Add(testutil.TestTimeout(t))); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := nc.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'g', 'a', 'r', 'b', 'a', 'g', 'e'}); err != nil {
		t.Fatalf("write malformed frame: %v", err)
	}
	// Reading to EOF proves the coordinator processed and dropped the
	// connection (a best-effort abort frame may arrive first) before the
	// legitimate worker connects.
	if _, err := io.Copy(io.Discard, nc); err != nil {
		t.Fatalf("coordinator never closed the malformed connection: %v", err)
	}
	nc.Close()

	pc2 := startWorker(t, ctx, intSpec{iperfPort: 45643}, port)
	waitForState(t, pc2.states, peer.StateTesting)
	close(gate)

	code2, err2 := pc2.app.Wait()
	code1, err1 := pc1.app.Wait()
	if code1 != ExitOK || err1 != nil {
		t.Fatalf("pc1 = (%d, %v), want (0, nil)\npc1 %s\npc2 %s", code1, err1, pc1.out.dump(), pc2.out.dump())
	}
	if code2 != ExitOK || err2 != nil {
		t.Fatalf("pc2 = (%d, %v), want (0, nil)\npc2 %s", code2, err2, pc2.out.dump())
	}
	assertHealthyArtifacts(t, pc1)
}

// testReportTransferCorruption flips a byte in every report chunk PC1 sends
// via the mangleReportChunk hook. The transfer therefore fails every SHA-256
// check and both retries; by contract that is a warning, never an
// orchestration failure — so both sides still exit 0 (health-driven), PC1's
// report is intact, and PC2 keeps NOTHING corrupt: no verified files, and no
// leftover .part scratch files.
func testReportTransferCorruption(t *testing.T) {
	testutil.LeakCheck(t)
	ctx := context.Background()
	gate := make(chan struct{})
	// Corrupt every chunk so no file ever verifies and PC2 abandons them all.
	mangle := func(data []byte) []byte {
		out := append([]byte(nil), data...)
		if len(out) > 0 {
			out[0] ^= 0xFF
		}
		return out
	}
	pc1, port := startCoordinator(t, ctx, intSpec{iperfPort: 45651, planGate: gate,
		mangleReportChunk: mangle})
	pc2 := startWorker(t, ctx, intSpec{iperfPort: 45653}, port)

	waitForState(t, pc2.states, peer.StateTesting)
	close(gate)

	code2, err2 := pc2.app.Wait()
	code1, err1 := pc1.app.Wait()
	// Transfer failure is a warning: the run completes, exit is health-driven.
	if code1 != ExitOK || err1 != nil {
		t.Fatalf("pc1 = (%d, %v), want (0, nil) — transfer failure must not fail the run\npc1 %s\npc2 %s",
			code1, err1, pc1.out.dump(), pc2.out.dump())
	}
	if code2 != ExitOK || err2 != nil {
		t.Fatalf("pc2 = (%d, %v), want (0, nil)\npc2 %s", code2, err2, pc2.out.dump())
	}
	// PC1's own report is intact and complete regardless of the transfer.
	assertHealthyArtifacts(t, pc1)

	// PC2 kept nothing corrupt: no .part scratch files, and every corrupt file
	// was abandoned rather than committed. The one file PC2 legitimately has
	// is its own locally-written fallback summary.txt.
	dir2 := findReportDir(t, pc2.cfg.OutputDir)
	assertNoPartFilesLeft(t, dir2)
	if _, err := os.Stat(filepath.Join(dir2, "report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PC2 has a report.json after every chunk was corrupted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "report.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PC2 has a report.md after every chunk was corrupted: %v", err)
	}
	// The fallback summary must be PC2's own (not PC1's transferred one), since
	// no transfer landed.
	sum, err := os.ReadFile(filepath.Join(dir2, "summary.txt"))
	if err != nil {
		t.Fatalf("read PC2 fallback summary.txt: %v", err)
	}
	if !strings.Contains(string(sum), "worker") {
		t.Errorf("PC2 summary.txt is not the local worker fallback:\n%s", sum)
	}
}

// testNoReportTransferFlag sets --no-report-transfer on the coordinator: the
// transfer is gated off, so PC1 sends no manifest and PC2 receives no files,
// yet both sides still complete the run and exit 0. PC2 falls back to its
// locally-written worker summary.
func testNoReportTransferFlag(t *testing.T) {
	testutil.LeakCheck(t)
	ctx := context.Background()
	gate := make(chan struct{})
	pc1, port := startCoordinator(t, ctx, intSpec{iperfPort: 45661, planGate: gate,
		noReportTransfer: true, mode: config.ModeStandard})
	pc2 := startWorker(t, ctx, intSpec{iperfPort: 45663}, port)

	waitForState(t, pc2.states, peer.StateTesting)
	close(gate)

	code2, err2 := pc2.app.Wait()
	code1, err1 := pc1.app.Wait()
	if code1 != ExitOK || err1 != nil {
		t.Fatalf("pc1 = (%d, %v), want (0, nil)\npc1 %s\npc2 %s", code1, err1, pc1.out.dump(), pc2.out.dump())
	}
	if code2 != ExitOK || err2 != nil {
		t.Fatalf("pc2 = (%d, %v), want (0, nil)\npc2 %s", code2, err2, pc2.out.dump())
	}
	assertHealthyArtifacts(t, pc1)

	// No transfer: PC2 has no report.json/report.md, only its own fallback.
	dir2 := findReportDir(t, pc2.cfg.OutputDir)
	assertNoPartFilesLeft(t, dir2)
	for _, name := range []string{"report.json", "report.md"} {
		if _, err := os.Stat(filepath.Join(dir2, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("PC2 has %s despite --no-report-transfer: %v", name, err)
		}
	}
	sum, err := os.ReadFile(filepath.Join(dir2, "summary.txt"))
	if err != nil {
		t.Fatalf("read PC2 summary.txt: %v", err)
	}
	if !strings.Contains(string(sum), "worker") {
		t.Errorf("PC2 summary.txt is not the local worker fallback:\n%s", sum)
	}
	if !strings.Contains(string(sum), "mode:      standard") {
		t.Errorf("PC2 fallback summary does not use PC1's announced mode:\n%s", sum)
	}
	if strings.Contains(string(sum), "mode:      quick") {
		t.Errorf("PC2 fallback summary uses PC2's local mode:\n%s", sum)
	}
}

// testNoBidirFallback drives the two-phase bidirectional fallback: the worker
// reports no iperf3 --bidir support, so the effective (ANDed) capability lacks
// --bidir and the coordinator runs two coordinated one-way phases on distinct
// ports instead of one native --bidir run. Both sides therefore record no
// --bidir client, the coordinator's fallback used IperfPort+1 for the reverse
// phase, and the report carries the limitation note plus TwoPhaseFallback.
func testNoBidirFallback(t *testing.T) {
	testutil.LeakCheck(t)
	ctx := context.Background()
	gate := make(chan struct{})
	pc1, port := startCoordinator(t, ctx, intSpec{iperfPort: 45671, planGate: gate})
	pc2 := startWorker(t, ctx, intSpec{iperfPort: 45673, noBidirCaps: true}, port)

	waitForState(t, pc2.states, peer.StateTesting)
	close(gate)

	code2, err2 := pc2.app.Wait()
	code1, err1 := pc1.app.Wait()
	if code1 != ExitOK || err1 != nil {
		t.Fatalf("pc1 = (%d, %v), want (0, nil)\npc1 %s\npc2 %s", code1, err1, pc1.out.dump(), pc2.out.dump())
	}
	if code2 != ExitOK || err2 != nil {
		t.Fatalf("pc2 = (%d, %v), want (0, nil)\npc2 %s", code2, err2, pc2.out.dump())
	}

	// Neither side ran a --bidir client: the fallback is two one-way phases.
	assertNoBidirClient(t, pc1)
	assertNoBidirClient(t, pc2)

	// The coordinator's fallback ran the reverse phase on IperfPort+1 (a
	// distinct port), proving the two phases were coordinated on separate
	// ports rather than a single simultaneous --bidir session.
	if !usedIperfPort(pc1, 45672) {
		t.Errorf("coordinator did not use the fallback reverse port %d", 45672)
	}

	rep, _ := readReport(t, pc1.cfg.OutputDir)
	if rep.Tests.Bidirectional == nil {
		t.Fatalf("report carries no bidirectional result")
	}
	if !rep.Tests.Bidirectional.TwoPhaseFallback {
		t.Errorf("bidirectional result TwoPhaseFallback = false, want true")
	}
	if !warningsMention(rep, "one-way") {
		t.Errorf("report warnings lack the two-phase limitation note: %v", rep.Warnings)
	}
}

// assertNoBidirClient fails if the side ran any iperf3 client with --bidir.
func assertNoBidirClient(t *testing.T, side *intSide) {
	t.Helper()
	for _, c := range side.fr.Calls() {
		if c.Name == "iperf3" && slices.Contains(c.Args, "-c") && slices.Contains(c.Args, "--bidir") {
			t.Errorf("iperf3 --bidir client recorded despite the fallback: %q", c.Args)
		}
	}
}

// usedIperfPort reports whether the side ran any iperf3 command against the
// given port (via -p).
func usedIperfPort(side *intSide, port int) bool {
	want := strconv.Itoa(port)
	for _, c := range side.fr.Calls() {
		if c.Name != "iperf3" {
			continue
		}
		for i := 0; i+1 < len(c.Args); i++ {
			if c.Args[i] == "-p" && c.Args[i+1] == want {
				return true
			}
		}
	}
	return false
}

// probedFreeIperfPort asks the kernel for an ephemeral loopback TCP port,
// releases it, then verifies that both it and its successor satisfy the same
// TCP+UDP availability checks as iperf preflight.
func probedFreeIperfPort(t *testing.T) uint16 {
	t.Helper()
	loopback := netip.MustParseAddr("127.0.0.1")
	for range 100 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("allocate ephemeral iperf port: %v", err)
		}
		port := uint16(listener.Addr().(*net.TCPAddr).Port)
		if err := listener.Close(); err != nil {
			t.Errorf("close ephemeral iperf port probe: %v", err)
			continue
		}
		if port == ^uint16(0) {
			continue
		}
		if network.ProbePortFree(loopback, port) != nil {
			continue
		}
		if network.ProbePortFree(loopback, port+1) != nil {
			continue
		}
		return port
	}
	t.Fatalf("could not find two consecutive free loopback ports for iperf")
	return 0
}

// warningsMention reports whether any report warning contains sub.
func warningsMention(rep *model.Report, sub string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// seedStalePidfile writes a live-looking, ownership-verified iperf3 pidfile
// (the running test binary itself, which passes VerifyOwnership) into a fake
// prior-session directory under stateDir. There is no session marker, so
// ScanStale does not treat the directory as owned by a live session and does
// examine the pidfile — which then verifies as a live stale survivor.
func seedStalePidfile(t *testing.T, stateDir string) {
	t.Helper()
	sessionDir := filepath.Join(stateDir, "stale-prior-session")
	if err := os.Mkdir(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir stale session dir: %v", err)
	}
	info, err := runner.NewProcessInfo(os.Getpid(), "iperf3-server", "stale-prior-session")
	if err != nil {
		t.Fatalf("NewProcessInfo(self): %v", err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal pidfile: %v", err)
	}
	path := filepath.Join(sessionDir, strconv.Itoa(info.PID)+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write stale pidfile: %v", err)
	}
}

// testStaleIperf3Detection pre-seeds a live-looking iperf3 pidfile in the
// worker's StateDir. The worker's stale-process preflight must fail with
// remediation text and exit 4 without ever dialing the coordinator. The
// coordinator, whose worker never connects, blocks on Accept; the operator
// then cancels it (Ctrl+C — cancelling the coordinator's context is the
// SIGINT path since signal.NotifyContext lives only in main), and it must
// shut its listener down gracefully rather than deadlock. Cancellation is
// applied only after the worker has provably failed, so the assertion that
// the worker never connected is race-free.
func testStaleIperf3Detection(t *testing.T) {
	testutil.LeakCheck(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	pc1, port := startCoordinator(t, ctx1, intSpec{iperfPort: 45681})
	pc2 := startWorker(t, context.Background(), intSpec{
		iperfPort: probedFreeIperfPort(t), stalePidfile: true,
	}, port)

	code2, err2 := pc2.app.Wait()
	if code2 != ExitConfig {
		t.Errorf("pc2 = (%d, %v), want exit 4 for a stale iperf3 pidfile\n%s", code2, err2, pc2.out.dump())
	}
	// The preflight diagnostic must reach the operator with remediation text.
	msg := pc2.out.stderr.String() + errString(err2)
	if !strings.Contains(strings.ToLower(msg), "stale") {
		t.Errorf("pc2 diagnostics do not mention the stale process:\n%s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "kill") {
		t.Errorf("pc2 diagnostics lack remediation text (how to kill the survivor):\n%s", msg)
	}
	// The failing worker never opened the control connection.
	if pc2.app.sessionTestID() != "" {
		t.Errorf("worker completed a handshake despite failing preflight")
	}

	// The coordinator was still waiting for a peer that never dialed; the
	// operator cancels it. The one thing that must NOT happen is a hang: Wait
	// has to return promptly (bounded below by a watchdog) with a
	// non-success code — whether the cancellation landed during preflight
	// (config, 4), while blocked on Accept (interrupt, 6), or as an
	// orchestration failure (5) does not matter for "shuts down gracefully".
	cancel1()
	done := make(chan struct{})
	var code1 ExitCode
	go func() {
		code1, _ = pc1.app.Wait()
		close(done)
	}()
	testutil.WaitFor(t, done, "coordinator deadlocked instead of shutting down after cancellation")
	if code1 == ExitOK {
		t.Errorf("pc1 = %d, want a non-success shutdown code after the worker never connected\n%s",
			code1, pc1.out.dump())
	}
}

func TestProbedFreeIperfPort(t *testing.T) {
	port := probedFreeIperfPort(t)
	if port == 0 || port == ^uint16(0) {
		t.Errorf("probed iperf port = %d, want a nonzero base with room for port+1", port)
	}
	for _, candidate := range []uint16{port, port + 1} {
		if err := network.ProbePortFree(netip.MustParseAddr("127.0.0.1"), candidate); err != nil {
			t.Errorf("probed iperf port %d is not free: %v", candidate, err)
		}
	}
}

// errString renders err for diagnostics, "" for nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// testPeerDisconnectMidTest hard-cancels the worker's context mid-TCP-test
// (gated on the worker's fake iperf3 client Started channel, simulating a hard
// death). The coordinator must write a partial report (partial:true) and exit
// 5; the worker, killed mid-op, exits 5 too.
func testPeerDisconnectMidTest(t *testing.T) {
	testutil.LeakCheck(t)
	ctx1 := context.Background()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	gate := make(chan struct{})
	pc1, port := startCoordinator(t, ctx1, intSpec{iperfPort: 45691, planGate: gate})

	// The reverse-direction TCP phase runs the worker's iperf3 client; hang it
	// on Delay and use Started as the mid-test synchronization point.
	workerClientStarted := make(chan struct{})
	workerClientHang := make(chan struct{})
	pc2 := startWorker(t, ctx2, intSpec{iperfPort: 45693}, port)
	pc2.fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixture("iperf", "tcp_316_fwd.json"), Times: 1,
		Delay: workerClientHang, Started: workerClientStarted})

	waitForState(t, pc2.states, peer.StateTesting)
	close(gate)

	// Wait until the worker is provably mid-client, then hard-cancel it.
	testutil.WaitFor(t, workerClientStarted, "pc2 iperf3 client to start")
	cancel2()

	code2, err2 := pc2.app.Wait()
	code1, err1 := pc1.app.Wait()
	if code2 != ExitInterrupt && code2 != ExitPeer {
		t.Errorf("pc2 = (%d, %v), want exit 5 or 6 after a hard cancel\n%s", code2, err2, pc2.out.dump())
	}
	if code1 != ExitPeer {
		t.Errorf("pc1 = (%d, %v), want exit 5 (peer disconnected)\n%s", code1, err1, pc1.out.dump())
	}

	// The coordinator preserved a partial report.
	rep, _ := readReport(t, pc1.cfg.OutputDir)
	if !rep.Partial {
		t.Errorf("coordinator partial report has Partial = false, want true")
	}
	if rep.Failure == nil || rep.Failure.Error == "" {
		t.Errorf("partial report misses FailureDetails: %+v", rep.Failure)
	}
}
