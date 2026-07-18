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
	enriched := a.assembleReport(pf, results, startedAt, nil, outcome)
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
