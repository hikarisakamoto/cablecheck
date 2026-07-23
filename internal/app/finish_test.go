package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/config"
	"cablecheck/internal/evaluate"
	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/testsuite"
)

// TestFinishCoordinatorRefreshesVerdictAfterPeerCapabilities pins that the
// success-path rendering merges worker facts before choosing PC1's final
// classification and exit code.
func TestFinishCoordinatorRefreshesVerdictAfterPeerCapabilities(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.RunConfig{
		Role:    config.RolePC1,
		LocalIP: netip.MustParseAddr("127.0.0.1"),
		PeerIP:  netip.MustParseAddr("127.0.0.1"),
		Mode:    config.ModeQuick,
		Token:   "testtoken1234",
	}
	a, err := New(cfg, Deps{Stdout: &out, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pf := &preflightInfo{}
	pf.Iface.Name = "eth0"
	results := &testsuite.SessionResults{}
	v := &verdict{}
	startedAt := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)

	// First pass: what the plan wrapper can render before the session outcome
	// exposes the worker capabilities.
	if _, err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, nil, log); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	exchanged := v.complete()
	if exchanged.Classification == "" {
		t.Fatalf("first finalize stored no verdict")
	}

	// The handshake caps advertise half duplex, which the plan's own link
	// step never observed: a fresh evaluation over the enriched report
	// classifies differently. Guard the fixture so the test keeps meaning.
	outcome := &peer.Outcome{
		PeerCaps: protocol.Capabilities{
			NIC: protocol.NICInfo{Name: "eth0", SpeedMbps: 1000, Duplex: "half", MTU: 1500},
		},
	}
	enriched := a.assembleReport(pf, results, startedAt, startedAt, nil, outcome)
	fresh := evaluate.Evaluate(evaluate.FactsFromReport(enriched))
	if string(fresh.Class) == exchanged.Classification {
		t.Fatalf("fixture broken: re-evaluating with the peer caps still yields %s", fresh.Class)
	}

	code, err := a.finishCoordinator(dir, rawDir, pf, results, v, startedAt, outcome, nil, log)
	if err != nil {
		t.Fatalf("finishCoordinator: %v", err)
	}
	if code != ExitCodeFor(fresh.Class) {
		t.Errorf("exit code = %d, want %d for refreshed class %s", code, ExitCodeFor(fresh.Class), fresh.Class)
	}
	if after := v.complete(); after.Classification != string(fresh.Class) || after.ExitCode != int(ExitCodeFor(fresh.Class)) {
		t.Errorf("stored verdict was not refreshed:\nprovisional: %+v\nafter:       %+v", exchanged, after)
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var got model.Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	if got.Classification != fresh.Class {
		t.Errorf("report.json classification = %s, want refreshed %s", got.Classification, fresh.Class)
	}
	// The enrichment still lands: the peer machine description made it in.
	if got.PC2.NIC.Name != "eth0" {
		t.Errorf("peer machine description missing after enrichment: PC2.NIC = %+v", got.PC2.NIC)
	}
}

func TestFinishCoordinatorReevaluatesAfterPeerCapabilities(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.RunConfig{
		Role: config.RolePC1, LocalIP: netip.MustParseAddr("127.0.0.1"),
		PeerIP: netip.MustParseAddr("127.0.0.1"), Mode: config.ModeQuick, Token: "testtoken1234",
	}
	a, err := New(cfg, Deps{Stdout: &out, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pf := &preflightInfo{Link: model.LinkSettings{SpeedMbps: 1000, Duplex: "full", LinkDetected: true}}
	pf.Iface.Name = "eth0"
	pf.Iface.Class.Driver = "e1000e"
	cleanBefore := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	cleanAfter := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	results := &testsuite.SessionResults{
		InitialCounters: model.PeerCounters{PC1: cleanBefore, PC2: cleanBefore},
		FinalCounters:   model.PeerCounters{PC1: cleanAfter, PC2: cleanAfter},
		TCP: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 941_000_000},
		},
	}
	v := &verdict{}
	startedAt := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	if _, err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, nil, log); err != nil {
		t.Fatalf("provisional finalize: %v", err)
	}
	if provisional := v.complete(); provisional.Classification != string(model.HealthExcellent) {
		t.Fatalf("provisional class = %s, want EXCELLENT", provisional.Classification)
	}

	outcome := &peer.Outcome{PeerCaps: protocol.Capabilities{
		NIC: protocol.NICInfo{Name: "veth0", Driver: "", SpeedMbps: 1000, Duplex: "full", MTU: 1500},
	}}
	code, err := a.finishCoordinator(dir, rawDir, pf, results, v, startedAt, outcome, nil, log)
	if err != nil {
		t.Fatalf("finishCoordinator: %v", err)
	}
	if code != ExitInconclusive {
		t.Errorf("exit code = %d, want %d", code, ExitInconclusive)
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var rep model.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	if rep.Classification != model.HealthInconclusive {
		t.Errorf("classification = %s, want INCONCLUSIVE (findings %v)", rep.Classification, rep.ClassificationReasons)
	}
	if !slices.Contains(findingRuleIDs(rep.Findings), "HOST-02") {
		t.Errorf("findings = %v, want HOST-02", findingRuleIDs(rep.Findings))
	}
}

func TestFinishCoordinatorQuietUsesCompactFallback(t *testing.T) {
	var out bytes.Buffer
	var summaryCalls int
	cfg := &config.RunConfig{
		Role: config.RolePC1, LocalIP: netip.MustParseAddr("127.0.0.1"),
		PeerIP: netip.MustParseAddr("127.0.0.2"), Mode: config.ModeQuick,
		Token: "testtoken1234", Quiet: true,
	}
	a, err := New(cfg, Deps{
		Stdout: &out, StateDir: t.TempDir(),
		OnSummary: func(*model.Report, string) { summaryCalls++ },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pf := &preflightInfo{}
	pf.Iface.Name = "eth0"
	results := &testsuite.SessionResults{}
	v := &verdict{}
	startedAt := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	if _, err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, nil, log); err != nil {
		t.Fatalf("provisional finalize: %v", err)
	}

	if _, err := a.finishCoordinator(dir, rawDir, pf, results, v, startedAt, &peer.Outcome{}, nil, log); err != nil {
		t.Fatalf("finishCoordinator: %v", err)
	}
	if summaryCalls != 0 {
		t.Errorf("OnSummary called %d times in quiet mode", summaryCalls)
	}
	for _, want := range []string{"cable health:", "Report: " + dir} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("quiet output missing %q: %q", want, out.String())
		}
	}
}

func TestFinishCoordinatorPartialUsesSummaryHook(t *testing.T) {
	var out bytes.Buffer
	var presented *model.Report
	var presentedDir string
	cfg := &config.RunConfig{
		Role: config.RolePC1, LocalIP: netip.MustParseAddr("127.0.0.1"),
		PeerIP: netip.MustParseAddr("127.0.0.2"), Mode: config.ModeQuick, Token: "testtoken1234",
	}
	a, err := New(cfg, Deps{
		Stdout: &out, StateDir: t.TempDir(),
		OnSummary: func(rep *model.Report, dir string) {
			presented, presentedDir = rep, dir
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pf := &preflightInfo{}
	pf.Iface.Name = "eth0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := errors.New("peer disconnected")

	_, gotErr := a.finishCoordinator(dir, rawDir, pf, &testsuite.SessionResults{}, &verdict{},
		time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC), &peer.Outcome{FinalState: peer.StateTesting}, runErr, log)
	if !errors.Is(gotErr, runErr) {
		t.Fatalf("finishCoordinator error = %v, want %v", gotErr, runErr)
	}
	if presented == nil || !presented.Partial || presented.Failure == nil {
		t.Fatalf("presented report = %+v, want partial report with failure", presented)
	}
	if presentedDir != dir {
		t.Errorf("presented dir = %q, want %q", presentedDir, dir)
	}
	if out.Len() != 0 {
		t.Errorf("summary hook did not own partial output: %q", out.String())
	}
}

func TestFinishCoordinatorPartialQuietUsesCompactFallback(t *testing.T) {
	var out bytes.Buffer
	var summaryCalls int
	cfg := &config.RunConfig{
		Role: config.RolePC1, LocalIP: netip.MustParseAddr("127.0.0.1"),
		PeerIP: netip.MustParseAddr("127.0.0.2"), Mode: config.ModeQuick,
		Token: "testtoken1234", Quiet: true,
	}
	a, err := New(cfg, Deps{
		Stdout: &out, StateDir: t.TempDir(),
		OnSummary: func(*model.Report, string) { summaryCalls++ },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pf := &preflightInfo{}
	pf.Iface.Name = "eth0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := errors.New("peer disconnected")

	if _, gotErr := a.finishCoordinator(dir, rawDir, pf, &testsuite.SessionResults{}, &verdict{},
		time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC), &peer.Outcome{FinalState: peer.StateTesting}, runErr, log); !errors.Is(gotErr, runErr) {
		t.Fatalf("finishCoordinator error = %v, want %v", gotErr, runErr)
	}
	if summaryCalls != 0 {
		t.Errorf("OnSummary called %d times in quiet mode", summaryCalls)
	}
	// The compact partial fallback keeps the failure context yet still carries
	// the cable health: / Report: substrings the success line emits.
	for _, want := range []string{"cable health:", "run did not complete (peer disconnected)", "Report: " + dir} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("quiet partial output missing %q: %q", want, out.String())
		}
	}
}

func TestFinishCoordinatorPresentsLastSuccessfulReportAfterEnrichmentWriteFailure(t *testing.T) {
	var presented *model.Report
	cfg := &config.RunConfig{
		Role: config.RolePC1, LocalIP: netip.MustParseAddr("127.0.0.1"),
		PeerIP: netip.MustParseAddr("127.0.0.2"), Mode: config.ModeQuick, Token: "testtoken1234",
	}
	a, err := New(cfg, Deps{
		StateDir: t.TempDir(),
		OnSummary: func(rep *model.Report, _ string) {
			presented = rep
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pf := &preflightInfo{}
	pf.Iface.Name = "eth0"
	results := &testsuite.SessionResults{}
	v := &verdict{}
	startedAt := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provisional, err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, nil, log)
	if err != nil {
		t.Fatalf("provisional finalize: %v", err)
	}
	_, _, provisionalCode, _ := v.get()

	// Turn report.md into a directory so both enrichment attempts fail after
	// the provisional report was already published successfully.
	if err := os.Remove(filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "report.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	outcome := &peer.Outcome{PeerCaps: protocol.Capabilities{
		NIC: protocol.NICInfo{Name: "eth0", SpeedMbps: 1000, Duplex: "half"},
	}}
	code, err := a.finishCoordinator(dir, rawDir, pf, results, v, startedAt, outcome, nil, log)
	if err != nil {
		t.Fatalf("finishCoordinator: %v", err)
	}
	if code != provisionalCode {
		t.Errorf("exit code = %d, want provisional %d", code, provisionalCode)
	}
	if presented != provisional {
		t.Errorf("presented report pointer = %p, want last successful provisional %p", presented, provisional)
	}
	if latest, ok := v.renderedReport(); !ok || latest != provisional {
		t.Errorf("stored report = %p/%v, want provisional %p", latest, ok, provisional)
	}
}

func findingRuleIDs(findings []model.Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		ids = append(ids, finding.RuleID)
	}
	return ids
}

func TestCapabilitiesExchangePropagatesPeerUSB(t *testing.T) {
	cfg := &config.RunConfig{Role: config.RolePC1}
	a, err := New(cfg, Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pf := &preflightInfo{}
	results := &testsuite.SessionResults{}
	when := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	workerCaps := protocol.Capabilities{
		NIC: protocol.NICInfo{Name: "enx001122334455", Driver: "r8152", USB: true},
	}
	wire, err := json.Marshal(workerCaps)
	if err != nil {
		t.Fatalf("marshal worker capabilities: %v", err)
	}
	var exchanged protocol.Capabilities
	if err := json.Unmarshal(wire, &exchanged); err != nil {
		t.Fatalf("unmarshal exchanged capabilities: %v", err)
	}
	outcome := &peer.Outcome{PeerCaps: exchanged}

	rep := a.assembleReport(pf, results, when, when, nil, outcome)
	if !rep.PC2.NIC.USB {
		t.Errorf("PC2.NIC.USB = false, want true from worker capabilities")
	}
	if rep.Machines == nil || !rep.Machines.PC2.NIC.USB {
		t.Errorf("Machines.PC2.NIC.USB was not propagated: %+v", rep.Machines)
	}
}

func TestRuntimeSkippedPingCapsEvaluationAtGood(t *testing.T) {
	cfg := &config.RunConfig{Role: config.RolePC1}
	a, err := New(cfg, Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pf := &preflightInfo{}
	pf.Iface.Name = "eth0"
	pf.Iface.Class.Driver = "e1000e"
	pf.Link = model.LinkSettings{SpeedMbps: 1000, Duplex: "full", LinkDetected: true}
	before := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	after := &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 0}}
	results := &testsuite.SessionResults{
		InitialCounters: model.PeerCounters{PC1: before, PC2: before},
		FinalCounters:   model.PeerCounters{PC1: after, PC2: after},
		TCP: []model.TCPResult{
			{Direction: model.DirectionPC1ToPC2, ReceiverBitsPerSecond: 941_000_000},
			{Direction: model.DirectionPC2ToPC1, ReceiverBitsPerSecond: 941_000_000},
		},
		SkippedTests: []model.SkippedTest{{
			Name: "ping", Reason: "peer could not run ping_run: ping disappeared after preflight",
		}},
	}
	when := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	outcome := &peer.Outcome{PeerCaps: protocol.Capabilities{
		NIC: protocol.NICInfo{Name: "eth0", Driver: "e1000e", SpeedMbps: 1000, Duplex: "full"},
	}}
	rep := a.assembleReport(pf, results, when, when, nil, outcome)
	if !slices.Contains(rep.SkippedTests, results.SkippedTests[0]) {
		t.Errorf("report.SkippedTests = %+v, want runtime skip %+v", rep.SkippedTests, results.SkippedTests[0])
	}
	res := evaluate.Evaluate(evaluate.FactsFromReport(rep))
	if res.Class != model.HealthGood {
		t.Errorf("class = %s, want GOOD when remote ping is unavailable (findings %v)", res.Class, findingIDsForApp(res))
	}
	if !slices.Contains(findingIDsForApp(res), "LIM-02") {
		t.Errorf("findings = %v, want LIM-02", findingIDsForApp(res))
	}
}

func TestAssembleReportPropagatesAssumedUDPRate(t *testing.T) {
	cfg := &config.RunConfig{Role: config.RolePC1}
	a, err := New(cfg, Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	when := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rep := a.assembleReport(&preflightInfo{}, &testsuite.SessionResults{UDPRateAssumed: true},
		when, when, nil, nil)
	if !rep.UDPRateAssumed {
		t.Errorf("report.UDPRateAssumed = false, want true")
	}
	facts := evaluate.FactsFromReport(rep)
	if !facts.UDPRateAssumed {
		t.Errorf("facts.UDPRateAssumed = false, want true")
	}
	res := evaluate.Evaluate(facts)
	if !slices.Contains(findingIDsForApp(res), "LIM-04") {
		t.Errorf("findings = %v, want LIM-04", findingIDsForApp(res))
	}
}

func findingIDsForApp(res evaluate.Result) []string {
	ids := make([]string, 0, len(res.Findings))
	for _, finding := range res.Findings {
		ids = append(ids, finding.RuleID)
	}
	return ids
}

// newWorkerApp builds a minimal PC2 App for finishWorker tests.
func newWorkerApp(t *testing.T, out *bytes.Buffer) *App {
	t.Helper()
	cfg := &config.RunConfig{
		Role:    config.RolePC2,
		LocalIP: netip.MustParseAddr("127.0.0.2"),
		PeerIP:  netip.MustParseAddr("127.0.0.1"),
		Mode:    config.ModeQuick,
		Token:   "testtoken1234",
	}
	a, err := New(cfg, Deps{Stdout: out, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// writeTransferredSet drops the given transferred report file names into dir
// with recognizable bodies, modeling a completed PC1->PC2 report transfer.
func writeTransferredSet(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("PC1 "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// TestFinishWorkerPreservesTransferredSummaryOnRunErr covers edge case (1): a
// transfer that completed before the session failed afterward (connection lost
// or Ctrl+C during the complete exchange) must not have its verified copy of
// PC1's summary.txt clobbered by the fallback whose text would then be false.
func TestFinishWorkerPreservesTransferredSummaryOnRunErr(t *testing.T) {
	var out bytes.Buffer
	a := newWorkerApp(t, &out)
	dir := t.TempDir()
	writeTransferredSet(t, dir, "report.json", "report.md", "summary.txt")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := a.finishWorker(dir, filepath.Join(dir, "raw"), &peer.Outcome{}, errPeerLost, log)
	if err == nil {
		t.Fatalf("finishWorker err = nil, want the run error propagated")
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "summary.txt"))
	if rerr != nil {
		t.Fatalf("read summary.txt: %v", rerr)
	}
	if string(got) != "PC1 summary.txt\n" {
		t.Errorf("transferred summary.txt was clobbered on the runErr path:\ngot: %q", got)
	}
}

// TestFinishWorkerFallbackWhenSummaryMissing covers edge case (2): report.json
// committed but summary.txt exhausted its retries, so the transferred set is
// incomplete. finishWorker must still write the fallback summary.txt rather
// than claim "Report received from PC1" while no summary.txt exists at all.
func TestFinishWorkerFallbackWhenSummaryMissing(t *testing.T) {
	var out bytes.Buffer
	a := newWorkerApp(t, &out)
	dir := t.TempDir()
	// Only report.json and report.md landed; summary.txt never verified.
	writeTransferredSet(t, dir, "report.json", "report.md")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	comp := &protocol.Complete{Classification: "EXCELLENT", Summary: "cable healthy", ExitCode: 0}
	code, err := a.finishWorker(dir, filepath.Join(dir, "raw"), &peer.Outcome{PeerComplete: comp}, nil, log)
	if err != nil {
		t.Fatalf("finishWorker: %v", err)
	}
	if code != ExitOK {
		t.Errorf("exit code = %d, want %d", code, ExitOK)
	}
	if _, serr := os.Stat(filepath.Join(dir, "summary.txt")); serr != nil {
		t.Errorf("summary.txt missing after an incomplete transfer: %v", serr)
	}
}

// TestFinishWorkerFallbackUsesCoordinatorMode pins that PC2's local fallback
// reports the mode announced by PC1, not PC2's usually-defaulted local mode.
func TestFinishWorkerFallbackUsesCoordinatorMode(t *testing.T) {
	var out bytes.Buffer
	a := newWorkerApp(t, &out)
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	comp := &protocol.Complete{Classification: "EXCELLENT", Summary: "cable healthy", ExitCode: 0}

	_, err := a.finishWorker(dir, filepath.Join(dir, "raw"), &peer.Outcome{PeerComplete: comp, Mode: "soak"}, nil, log)
	if err != nil {
		t.Fatalf("finishWorker: %v", err)
	}
	summary, err := os.ReadFile(filepath.Join(dir, "summary.txt"))
	if err != nil {
		t.Fatalf("read summary.txt: %v", err)
	}
	if !bytes.Contains(summary, []byte("mode:      soak")) {
		t.Errorf("PC2 fallback summary does not use PC1's announced mode:\n%s", summary)
	}
	if bytes.Contains(summary, []byte("mode:      quick")) {
		t.Errorf("PC2 fallback summary uses PC2's local mode:\n%s", summary)
	}
}

func TestFinishWorkerSummaryHookOwnsOutput(t *testing.T) {
	var out bytes.Buffer
	a := newWorkerApp(t, &out)
	var calls int
	var gotClass model.HealthClass
	var gotLine, gotPath string
	var gotTransferred bool
	a.deps.OnWorkerSummary = func(class model.HealthClass, line, path string, transferred bool) {
		calls++
		gotClass, gotLine, gotPath, gotTransferred = class, line, path, transferred
	}
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	comp := &protocol.Complete{Classification: "EXCELLENT", Summary: "cable health: EXCELLENT", ExitCode: 0}

	if _, err := a.finishWorker(dir, filepath.Join(dir, "raw"), &peer.Outcome{PeerComplete: comp}, nil, log); err != nil {
		t.Fatalf("finishWorker: %v", err)
	}
	if calls != 1 || gotClass != model.HealthExcellent || !strings.Contains(gotLine, "verdict from PC1: cable health: EXCELLENT") || gotPath != filepath.Join(dir, "summary.txt") || gotTransferred {
		t.Errorf("worker hook = calls %d class %q line %q path %q transferred %v", calls, gotClass, gotLine, gotPath, gotTransferred)
	}
	if out.Len() != 0 {
		t.Errorf("worker hook did not own output: %q", out.String())
	}

	comp.ExitCode = 99
	if _, err := a.finishWorker(t.TempDir(), t.TempDir(), &peer.Outcome{PeerComplete: comp}, nil, log); err == nil {
		t.Fatal("invalid completion exit code was accepted")
	}
	if calls != 1 {
		t.Errorf("worker hook called for invalid completion; calls = %d", calls)
	}
}

// TestWorkerDiagnosticAlwaysWritten pins requirement (2): PC2 always leaves a
// parseable diagnostic.json — carrying the peer's abort detail on failure —
// without clobbering PC1's transferred files on success.
func TestWorkerDiagnosticAlwaysWritten(t *testing.T) {
	readDiag := func(t *testing.T, dir string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "diagnostic.json"))
		if err != nil {
			t.Fatalf("diagnostic.json missing: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("diagnostic.json is not valid JSON: %v", err)
		}
		return m
	}

	t.Run("on abort it surfaces the peer detail", func(t *testing.T) {
		var out bytes.Buffer
		a := newWorkerApp(t, &out)
		dir := t.TempDir()
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		outcome := &peer.Outcome{
			FinalState:  peer.StateAborted,
			AbortReason: "internal_error",
			AbortStage:  "testing",
			AbortDetail: "peer: test plan failed: iperf3 could not reach the server",
			TestID:      "ct-diag-1",
		}
		if _, err := a.finishWorker(dir, filepath.Join(dir, "raw"), outcome, errPeerLost, log); err == nil {
			t.Fatal("finishWorker err = nil, want the run error propagated")
		}
		m := readDiag(t, dir)
		if m["role"] != "pc2" || m["finalState"] != "aborted" || m["abortReason"] != "internal_error" {
			t.Errorf("diagnostic core fields = %+v, want pc2/aborted/internal_error", m)
		}
		if m["completed"] != false {
			t.Errorf("completed = %v, want false", m["completed"])
		}
		if d, _ := m["abortDetail"].(string); !strings.Contains(d, "could not reach the server") {
			t.Errorf("abortDetail = %q, want the peer's detail surfaced", d)
		}
	})

	t.Run("on success it does not clobber the transferred set", func(t *testing.T) {
		var out bytes.Buffer
		a := newWorkerApp(t, &out)
		dir := t.TempDir()
		writeTransferredSet(t, dir, "report.json", "report.md", "summary.txt")
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		comp := &protocol.Complete{Classification: "EXCELLENT", ExitCode: 0}
		if _, err := a.finishWorker(dir, filepath.Join(dir, "raw"), &peer.Outcome{PeerComplete: comp}, nil, log); err != nil {
			t.Fatalf("finishWorker: %v", err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "summary.txt")); string(got) != "PC1 summary.txt\n" {
			t.Errorf("transferred summary.txt was clobbered: %q", got)
		}
		m := readDiag(t, dir)
		if m["completed"] != true || m["peerVerdict"] != "EXCELLENT" {
			t.Errorf("diagnostic = %+v, want completed=true, peerVerdict=EXCELLENT", m)
		}
	})
}

// errPeerLost is a stand-in run error for the worker finish tests.
var errPeerLost = errPeerLostSentinel{}

type errPeerLostSentinel struct{}

func (errPeerLostSentinel) Error() string { return "peer lost" }
