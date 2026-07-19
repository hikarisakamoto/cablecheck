package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cablecheck/internal/evaluate"
	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/reporting"
	"cablecheck/internal/testsuite"
)

// verdict is the evaluated outcome shared between the plan goroutine (which
// stores it after rendering the report) and the session's Complete callback
// (which reads it to fill the complete frame). A later rendering replaces the
// stored result after merging newly available peer capabilities, ensuring the
// final report and coordinator exit code use the complete evidence set.
type verdict struct {
	mu      sync.Mutex
	set     bool
	res     evaluate.Result
	summary string
	code    ExitCode
	// finishedAt is captured on the first render so every later re-render
	// (the pre-transfer enrichment, the post-run enrichment) produces
	// byte-identical report files: PC2's transferred copy must SHA-256-equal
	// PC1's final copy, which a fresh clock read on each render would break.
	finishedAt time.Time
}

// store records the evaluated verdict and the render's finish timestamp.
func (v *verdict) store(res evaluate.Result, summary string, code ExitCode, finishedAt time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.set, v.res, v.summary, v.code, v.finishedAt = true, res, summary, code, finishedAt
}

// get returns the stored verdict and whether one was stored.
func (v *verdict) get() (evaluate.Result, string, ExitCode, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.res, v.summary, v.code, v.set
}

// finishTime returns the captured finish timestamp and whether one was stored.
func (v *verdict) finishTime() (time.Time, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.finishedAt, v.set
}

// complete builds the complete-frame payload; zero before a verdict exists.
func (v *verdict) complete() protocol.Complete {
	res, summary, code, ok := v.get()
	if !ok {
		return protocol.Complete{}
	}
	return protocol.Complete{
		Classification: string(res.Class),
		Summary:        summary,
		ExitCode:       int(code),
	}
}

// finalize assembles the report from everything measured, evaluates it,
// renders all three outputs into dir and stores the verdict. Re-rendering with
// a session outcome intentionally re-evaluates after peer capabilities have
// been merged: virtual interfaces, USB attachment and peer NIC properties are
// classification facts, not presentation-only enrichment. failure is nil for
// a completed plan; outcome enriches the peer side when available. Failures
// are wrapped in errReportWrite (exit 7).
func (a *App) finalize(dir, rawDir string, pf *preflightInfo, results *testsuite.SessionResults,
	v *verdict, startedAt time.Time, failure *model.FailureDetails, outcome *peer.Outcome,
	log *slog.Logger) error {
	// Reuse the finish timestamp captured on the first render so re-renders are
	// byte-stable; the first render stamps it from the clock.
	finishedAt, stored := v.finishTime()
	if !stored {
		finishedAt = a.deps.Clock.Now()
	}
	rep := a.assembleReport(pf, results, startedAt, finishedAt, failure, outcome)
	res := evaluate.Evaluate(evaluate.FactsFromReport(rep))
	rep.Classification = res.Class
	rep.Score = res.Score
	rep.Findings = res.Findings
	rep.Recommendations = res.Recommendations
	for _, f := range res.Findings {
		rep.ClassificationReasons = append(rep.ClassificationReasons, f.Text)
	}
	rep.RawFiles = indexRawFiles(dir, rawDir)
	if err := writeReportFiles(dir, rep); err != nil {
		return fmt.Errorf("%w: %w", errReportWrite, err)
	}
	summary := fmt.Sprintf("cable health: %s", res.Class)
	if len(res.Findings) > 0 {
		summary += " — " + res.Findings[0].Text
	}
	v.store(res, summary, ExitCodeFor(res.Class), finishedAt)
	log.Info("report rendered", "dir", dir, "classification", string(res.Class))
	return nil
}

// assembleReport builds the pre-evaluation report from preflight data, the
// accumulated session results and (when available) the session outcome.
// It only ever runs on PC1, so the local side is pc1 and the peer pc2.
func (a *App) assembleReport(pf *preflightInfo, results *testsuite.SessionResults,
	startedAt, finishedAt time.Time, failure *model.FailureDetails, outcome *peer.Outcome) *model.Report {
	testID := a.sessionTestID()
	var peerCaps protocol.Capabilities
	if outcome != nil {
		if outcome.TestID != "" {
			testID = outcome.TestID
		}
		peerCaps = outcome.PeerCaps
	}

	hostname, _ := os.Hostname()
	local := model.PeerReport{
		Hostname: hostname,
		Kernel:   pf.Caps.Kernel,
		OS:       pf.Caps.OS,
		NIC: model.NICReport{
			Name:      pf.Iface.Name,
			Driver:    pf.Iface.Class.Driver,
			SpeedMbps: pf.Link.SpeedMbps,
			Duplex:    pf.Link.Duplex,
			MTU:       pf.Iface.MTU,
			MAC:       pf.Iface.MAC,
			USB:       pf.Iface.Class.USB,
		},
		ToolVersions: pf.ToolVersions,
	}
	remote := peerReportFromCaps(peerCaps)

	link := &model.LinkSection{}
	var warnings []string
	warnings = append(warnings, pf.Warnings...)
	if lr := results.Link[testsuite.RolePC1Key]; lr != nil {
		settings := lr.Settings
		link.PC1.Before = &settings
		warnings = appendPrefixed(warnings, "pc1: ", lr.Observations)
	} else if pf.Link.SpeedMbps != 0 || pf.Link.LinkDetected {
		settings := pf.Link
		link.PC1.Before = &settings
		warnings = appendPrefixed(warnings, "pc1: ", pf.LinkObservations)
	}
	if lr := results.Link[testsuite.RolePC2Key]; lr != nil {
		settings := lr.Settings
		link.PC2.Before = &settings
		warnings = appendPrefixed(warnings, "pc2: ", lr.Observations)
		if remote.NIC.SpeedMbps == 0 {
			remote.NIC.SpeedMbps = settings.SpeedMbps
			remote.NIC.Duplex = settings.Duplex
		}
	}
	warnings = append(warnings, results.Notes...)

	var deltas model.PeerCounterDeltas
	var ok bool
	deltas.PC1, ok = evaluate.DeltaSet(results.InitialCounters.PC1, results.FinalCounters.PC1)
	if !ok && results.InitialCounters.PC1 != nil && results.FinalCounters.PC1 != nil {
		warnings = append(warnings, "pc1: counter deltas unreliable (reset, wrap or missing counters)")
	}
	deltas.PC2, ok = evaluate.DeltaSet(results.InitialCounters.PC2, results.FinalCounters.PC2)
	if !ok && results.InitialCounters.PC2 != nil && results.FinalCounters.PC2 != nil {
		warnings = append(warnings, "pc2: counter deltas unreliable (reset, wrap or missing counters)")
	}

	partial := results.Incomplete || failure != nil
	if failure != nil {
		warnings = append(warnings, fmt.Sprintf("run did not complete: %s (stage %s)", failure.Error, failure.Stage))
	}

	rep := &model.Report{
		SchemaVersion:   model.SchemaVersion,
		ToolVersion:     a.deps.Build.Version,
		ProtocolVersion: protocol.Version,
		TestID:          testID,
		StartedAt:       startedAt.UTC(),
		FinishedAt:      finishedAt.UTC(),
		Duration:        model.Duration(finishedAt.Sub(startedAt)),
		Configuration:   echoConfig(a.cfg, a.controlPort, pf.Iface.Name),
		PC1:             local,
		PC2:             remote,
		Tests: model.TestsSection{
			Ping:          results.Ping,
			FullSizePing:  results.FullSizePing,
			TCP:           results.TCP,
			UDP:           results.UDP,
			Bidirectional: results.Bidir,
		},
		InitialCounters: results.InitialCounters,
		FinalCounters:   results.FinalCounters,
		CounterDeltas:   deltas,
		Warnings:        warnings,
		SkippedTests:    append(a.skippedTests(), results.SkippedTests...),
		UDPRateAssumed:  results.UDPRateAssumed,
		Partial:         partial,
		Failure:         failure,
		Link:            link,
		Machines:        &model.MachinePair{PC1: local, PC2: remote},
	}
	return rep
}

// skippedTests names the planned tests this version cannot run yet: the quick
// plan now runs the full traffic suite (full-size ping, UDP, bidirectional
// stress), so only the opt-in cable diagnostics remain unimplemented.
func (a *App) skippedTests() []model.SkippedTest {
	var skipped []model.SkippedTest
	if a.cfg.CableTest {
		skipped = append(skipped, model.SkippedTest{
			Name: "cable_test", Reason: "cable diagnostics are not implemented in this version",
		})
	}
	if a.cfg.CableTestTDR {
		skipped = append(skipped, model.SkippedTest{
			Name: "cable_test_tdr", Reason: "cable diagnostics are not implemented in this version",
		})
	}
	return skipped
}

// peerReportFromCaps converts the handshake capabilities into the peer's
// machine description; hostname is unknown (not part of the exchange).
func peerReportFromCaps(caps protocol.Capabilities) model.PeerReport {
	tools := map[string]string{}
	if caps.Iperf3.Version != "" {
		tools["iperf3"] = caps.Iperf3.Version
	}
	if caps.EthtoolVersion != "" {
		tools["ethtool"] = caps.EthtoolVersion
	}
	if caps.PingVariant != "" {
		tools["ping"] = caps.PingVariant
	}
	return model.PeerReport{
		Kernel: caps.Kernel,
		OS:     caps.OS,
		NIC: model.NICReport{
			Name:      caps.NIC.Name,
			Driver:    caps.NIC.Driver,
			SpeedMbps: caps.NIC.SpeedMbps,
			Duplex:    caps.NIC.Duplex,
			MTU:       caps.NIC.MTU,
			MAC:       caps.NIC.MAC,
			USB:       caps.NIC.USB,
		},
		ToolVersions: tools,
	}
}

// appendPrefixed appends each line of extra to dst with the given prefix.
func appendPrefixed(dst []string, prefix string, extra []string) []string {
	for _, s := range extra {
		dst = append(dst, prefix+s)
	}
	return dst
}

// writeReportFiles renders and writes report.json, report.md and summary.txt.
func writeReportFiles(dir string, rep *model.Report) error {
	jsonBytes, err := reporting.RenderJSON(rep)
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"report.json", jsonBytes},
		{"report.md", reporting.RenderMarkdown(rep)},
		{"summary.txt", reporting.RenderSummary(rep)},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// indexRawFiles indexes the sequenced raw artifacts (name, sha256, size).
// The live debug log is excluded: it is still being appended to when the
// index is computed, so its hash could not be trusted.
func indexRawFiles(dir, rawDir string) []model.RawFileRef {
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return nil
	}
	var refs []model.RawFileRef
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), "cablecheck-") {
			continue
		}
		path := filepath.Join(rawDir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			continue
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = e.Name()
		}
		refs = append(refs, model.RawFileRef{
			Name:   rel,
			SHA256: hex.EncodeToString(h.Sum(nil)),
			Bytes:  n,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}
