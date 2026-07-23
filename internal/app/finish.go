package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/testsuite"
)

// finishCoordinator maps PC1's session outcome onto files and exit codes.
// On success PrepareComplete has already rendered the capability-enriched
// report and stored its verdict. If that preparation failed, finish retries
// the finalization as a fallback. On failure a partial report with Partial=true
// and FailureDetails is always written from whatever the plan accumulated.
func (a *App) finishCoordinator(dir, rawDir string, pf *preflightInfo,
	results *testsuite.SessionResults, v *verdict, startedAt time.Time,
	outcome *peer.Outcome, runErr error, log *slog.Logger) (ExitCode, error) {
	if runErr == nil {
		if _, _, _, ok := v.get(); !ok {
			return ExitInternal, errors.New("app: session completed without a verdict")
		}
		if !v.isFinalized() {
			// PrepareComplete normally owns this final rendering. Retrying here
			// preserves the previous fallback when that callback could not write.
			if _, err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, outcome, log); err != nil {
				log.Warn("re-rendering the final report failed; the pre-complete rendering stands", "err", err)
			}
		}
		_, summary, code, _ := v.get()
		rep, rendered := v.renderedReport()
		if !rendered {
			return ExitInternal, errors.New("app: session completed without a rendered report")
		}
		if !a.cfg.Quiet && a.deps.OnSummary != nil {
			a.deps.OnSummary(rep, dir)
		} else {
			fmt.Fprintf(a.deps.Stdout, "\n%s\nReport: %s\n", summary, dir)
		}
		return code, nil
	}

	code := exitCodeForCoordinatorRunError(runErr)
	failure := &model.FailureDetails{
		Stage: failureStage(outcome),
		Error: runErr.Error(),
	}
	rep, err := a.finalize(dir, rawDir, pf, results, v, startedAt, failure, outcome, log)
	if err != nil {
		log.Warn("writing the partial report failed", "err", err)
	} else if !a.cfg.Quiet && a.deps.OnSummary != nil {
		a.deps.OnSummary(rep, dir)
	} else {
		// Compact fallback (quiet or no summary hook): keep the run-failure
		// context but still carry the same cable health: / Report: substrings
		// the success line emits, so both compact fallbacks read consistently.
		_, summary, _, _ := v.get()
		fmt.Fprintf(a.deps.Stdout, "\n%s\nrun did not complete (%v)\nReport: %s\n", summary, runErr, dir)
	}
	return code, runErr
}

// finishWorker maps PC2's session outcome onto its local summary file and
// exit code. The worker's verdict comes from PC1's complete frame; when the
// report transfer delivered PC1's full report set, those verified files are
// kept as-is and the local fallback summary is not written over them.
func (a *App) finishWorker(dir, rawDir string, outcome *peer.Outcome, runErr error,
	log *slog.Logger) (ExitCode, error) {
	// PC2 always leaves its own machine-readable record, so a run is
	// debuggable from the worker side even when PC1's report never transferred.
	a.writeWorkerDiagnostic(dir, rawDir, outcome, runErr, log)
	if runErr != nil {
		code := exitCodeForRunError(runErr)
		msg := redactSecret(runErr.Error(), a.cfg.Token)
		// The failure must reach stderr, not only the returned error: for a
		// rejected token this line is the worker operator's one actionable
		// diagnostic ("token rejected by coordinator").
		log.Error("run did not complete", "err", msg)
		if reportTransferred(dir) {
			// The full report set — including PC1's own summary.txt — already
			// arrived and was digest-verified before the session failed (a lost
			// connection or Ctrl+C during the complete exchange). Overwriting
			// summary.txt with a fallback claiming it "was not transferred"
			// would be false, so preserve the verified files and record the
			// failure under a separate name instead.
			a.writeWorkerFailureNote(dir, "run did not complete: "+msg, log)
		} else {
			a.writeWorkerSummary(dir, outcome, "run did not complete: "+msg, log)
		}
		return code, runErr
	}
	comp := outcome.PeerComplete
	if comp == nil {
		return ExitInternal, errors.New("app: session completed but PC1 sent no verdict")
	}
	code := ExitCode(comp.ExitCode)
	if code < ExitOK || code > ExitInternal {
		return ExitInternal, fmt.Errorf("app: PC1 reported an out-of-range exit code %d", comp.ExitCode)
	}
	verdictLine := fmt.Sprintf("verdict from PC1: %s", comp.Classification)
	if comp.Summary != "" {
		verdictLine = fmt.Sprintf("verdict from PC1: %s", comp.Summary)
	}
	transferred := reportTransferred(dir)
	if !transferred {
		a.writeWorkerSummary(dir, outcome, verdictLine, log)
	}
	summaryPath := filepath.Join(dir, "summary.txt")
	if a.deps.OnWorkerSummary != nil {
		path := summaryPath
		if transferred {
			path = dir
		}
		a.deps.OnWorkerSummary(model.HealthClass(comp.Classification), verdictLine, path, transferred)
	} else if transferred {
		fmt.Fprintf(a.deps.Stdout, "\n%s\nReport received from PC1: %s\n", verdictLine, dir)
	} else {
		fmt.Fprintf(a.deps.Stdout, "\n%s\nSummary: %s\n", verdictLine, summaryPath)
	}
	return code, nil
}

// reportTransferred reports whether PC1's full report set was received into
// dir. Files commit per file in canonical order, so the presence of any single
// file (report.json first) is not enough: summary.txt can exhaust its retries
// after report.json already landed, leaving an incomplete set. Only when all
// three verified files are present is the transfer complete and the local
// fallback summary safe to skip. Each file was renamed from a .part solely
// after its digest verified.
func reportTransferred(dir string) bool {
	for _, name := range []string{"report.json", "report.md", "summary.txt"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// writeWorkerSummary renders PC2's local fallback summary.txt from the
// complete payload plus locally known run data. It is only used when the
// report transfer did not deliver PC1's own summary.txt (declined, corrupted,
// or --no-report-transfer).
func (a *App) writeWorkerSummary(dir string, outcome *peer.Outcome, verdictLine string, log *slog.Logger) {
	testID := a.sessionTestID()
	mode := string(a.cfg.Mode)
	if outcome != nil && outcome.TestID != "" {
		testID = outcome.TestID
	}
	if outcome != nil && outcome.Mode != "" {
		mode = outcome.Mode
	}
	body := fmt.Sprintf(`CableCheck summary (pc2 / worker)
=================================
test id:   %s
mode:      %s
local ip:  %s
peer ip:   %s
%s

The full report (report.json, report.md, summary.txt) was generated on PC1.
It was not transferred to PC2 (transfer declined, disabled, or failed).
`,
		testID, mode, a.cfg.LocalIP.Unmap(), a.cfg.PeerIP.Unmap(), verdictLine)
	if err := os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(body), 0o600); err != nil {
		log.Warn("writing the worker summary failed", "err", err)
	}
}

// writeWorkerFailureNote records a post-transfer session failure without
// touching the verified files PC1 already transferred: the fallback summary.txt
// would overwrite PC1's own verified copy with a note that is now false, so the
// diagnostic goes into a separate transfer-note.txt instead.
func (a *App) writeWorkerFailureNote(dir, note string, log *slog.Logger) {
	body := fmt.Sprintf("CableCheck (pc2 / worker)\n%s\n\nPC1's report set was transferred and verified before this failure; the transferred report.json, report.md and summary.txt are PC1's own files.\n", note)
	if err := os.WriteFile(filepath.Join(dir, "transfer-note.txt"), []byte(body), 0o600); err != nil {
		log.Warn("writing the worker failure note failed", "err", err)
	}
}

// workerDiagnostic is PC2's own record of a run. It is deliberately not a
// model.Report: the worker runs no evaluation, so it records what it observed
// (final state, any peer abort, PC1's verdict, its own raw op outputs) rather
// than fabricating a classification. It carries its own shape and no schema
// version so regeneration never mistakes it for a full report.
type workerDiagnostic struct {
	Role        string             `json:"role"`
	GeneratedAt time.Time          `json:"generatedAt"`
	TestID      string             `json:"testId"`
	Mode        string             `json:"mode"`
	LocalIP     string             `json:"localIp"`
	PeerIP      string             `json:"peerIp"`
	FinalState  string             `json:"finalState"`
	Completed   bool               `json:"completed"`
	Error       string             `json:"error,omitempty"`
	AbortReason string             `json:"abortReason,omitempty"`
	AbortStage  string             `json:"abortStage,omitempty"`
	AbortDetail string             `json:"abortDetail,omitempty"`
	PeerVerdict string             `json:"peerVerdict,omitempty"`
	RawFiles    []model.RawFileRef `json:"rawFiles,omitempty"`
}

// writeWorkerDiagnostic writes diagnostic.json from the local outcome. It never
// changes the exit code and never touches PC1's transferred files (distinct
// filename), so it is safe on every exit path.
func (a *App) writeWorkerDiagnostic(dir, rawDir string, outcome *peer.Outcome, runErr error, log *slog.Logger) {
	d := workerDiagnostic{
		Role:        string(a.cfg.Role),
		GeneratedAt: a.deps.Clock.Now(),
		TestID:      a.sessionTestID(),
		Mode:        string(a.cfg.Mode),
		LocalIP:     a.cfg.LocalIP.Unmap().String(),
		PeerIP:      a.cfg.PeerIP.Unmap().String(),
		Completed:   runErr == nil,
		RawFiles:    indexRawFiles(dir, rawDir),
	}
	if outcome != nil {
		if outcome.TestID != "" {
			d.TestID = outcome.TestID
		}
		if outcome.Mode != "" {
			d.Mode = outcome.Mode
		}
		d.FinalState = string(outcome.FinalState)
		d.AbortReason = outcome.AbortReason
		d.AbortStage = outcome.AbortStage
		// Already redacted by the sender, but redact defensively in case a
		// buggy peer echoed the shared token.
		d.AbortDetail = redactSecret(outcome.AbortDetail, a.cfg.Token)
		if outcome.PeerComplete != nil {
			d.PeerVerdict = outcome.PeerComplete.Classification
		}
	}
	if runErr != nil {
		// A peer-abort detail is already redacted by PC1; redact again so a
		// local error string can never persist the trusted-link token.
		d.Error = redactSecret(runErr.Error(), a.cfg.Token)
	}
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		log.Warn("building the worker diagnostic failed", "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "diagnostic.json"), append(body, '\n'), 0o600); err != nil {
		log.Warn("writing the worker diagnostic failed", "err", err)
	}
}

// redactSecret strips the session token from a string before it is persisted.
func redactSecret(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[REDACTED]")
}

// failureStage derives the FailureDetails stage from the session outcome.
func failureStage(outcome *peer.Outcome) string {
	if outcome == nil {
		return "session"
	}
	if outcome.AbortReason != "" {
		return outcome.AbortReason
	}
	return string(outcome.FinalState)
}
