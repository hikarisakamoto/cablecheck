package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cablecheck/internal/config"
	"cablecheck/internal/evaluate"
	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/testsuite"
)

// TestFinishCoordinatorKeepsExchangedVerdict pins the complete-frame
// contract: once the verdict has been sent to PC2 in the complete frame, the
// success-path re-rendering (which merges in the peer capabilities known
// only after peer.Run returns) must not change the classification, exit code
// or summary — otherwise PC1's exit code, stdout summary and report.json can
// disagree with the exit code PC2 exits with for the same run.
func TestFinishCoordinatorKeepsExchangedVerdict(t *testing.T) {
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

	// First pass: what the plan wrapper renders before the session sends the
	// complete frame. The verdict stored here is the one PC2 receives.
	if err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, nil, log); err != nil {
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
	if fresh := evaluate.Evaluate(evaluate.FactsFromReport(enriched)); string(fresh.Class) == exchanged.Classification {
		t.Fatalf("fixture broken: re-evaluating with the peer caps still yields %s", fresh.Class)
	}

	code, err := a.finishCoordinator(dir, rawDir, pf, results, v, startedAt, outcome, nil, log)
	if err != nil {
		t.Fatalf("finishCoordinator: %v", err)
	}
	if code != ExitCode(exchanged.ExitCode) {
		t.Errorf("exit code = %d, want the exchanged %d", code, exchanged.ExitCode)
	}
	if after := v.complete(); after != exchanged {
		t.Errorf("verdict changed after enrichment:\nexchanged: %+v\nafter:     %+v", exchanged, after)
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var got model.Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	if string(got.Classification) != exchanged.Classification {
		t.Errorf("report.json classification = %s, want the exchanged %s", got.Classification, exchanged.Classification)
	}
	// The enrichment still lands: the peer machine description made it in.
	if got.PC2.NIC.Name != "eth0" {
		t.Errorf("peer machine description missing after enrichment: PC2.NIC = %+v", got.PC2.NIC)
	}
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

	_, err := a.finishWorker(dir, &peer.Outcome{}, errPeerLost, log)
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
	code, err := a.finishWorker(dir, &peer.Outcome{PeerComplete: comp}, nil, log)
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

// errPeerLost is a stand-in run error for the worker finish tests.
var errPeerLost = errPeerLostSentinel{}

type errPeerLostSentinel struct{}

func (errPeerLostSentinel) Error() string { return "peer lost" }
