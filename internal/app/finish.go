package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
			if err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, outcome, log); err != nil {
				log.Warn("re-rendering the final report failed; the pre-complete rendering stands", "err", err)
			}
		}
		_, summary, code, _ := v.get()
		fmt.Fprintf(a.deps.Stdout, "\n%s\nReport: %s\n", summary, dir)
		return code, nil
	}

	code := exitCodeForCoordinatorRunError(runErr)
	failure := &model.FailureDetails{
		Stage: failureStage(outcome),
		Error: runErr.Error(),
	}
	if err := a.finalize(dir, rawDir, pf, results, v, startedAt, failure, outcome, log); err != nil {
		log.Warn("writing the partial report failed", "err", err)
	} else {
		fmt.Fprintf(a.deps.Stdout, "\nrun did not complete (%v)\nPartial report: %s\n", runErr, dir)
	}
	return code, runErr
}

// finishWorker maps PC2's session outcome onto its local summary file and
// exit code. The worker's verdict comes from PC1's complete frame; when the
// report transfer delivered PC1's full report set, those verified files are
// kept as-is and the local fallback summary is not written over them.
func (a *App) finishWorker(dir string, outcome *peer.Outcome, runErr error,
	log *slog.Logger) (ExitCode, error) {
	if runErr != nil {
		code := exitCodeForRunError(runErr)
		// The failure must reach stderr, not only the returned error: for a
		// rejected token this line is the worker operator's one actionable
		// diagnostic ("token rejected by coordinator").
		log.Error("run did not complete", "err", runErr)
		if reportTransferred(dir) {
			// The full report set — including PC1's own summary.txt — already
			// arrived and was digest-verified before the session failed (a lost
			// connection or Ctrl+C during the complete exchange). Overwriting
			// summary.txt with a fallback claiming it "was not transferred"
			// would be false, so preserve the verified files and record the
			// failure under a separate name instead.
			a.writeWorkerFailureNote(dir, fmt.Sprintf("run did not complete: %v", runErr), log)
		} else {
			a.writeWorkerSummary(dir, outcome, fmt.Sprintf("run did not complete: %v", runErr), log)
		}
		return code, runErr
	}
	comp := outcome.PeerComplete
	if comp == nil {
		return ExitInternal, errors.New("app: session completed but PC1 sent no verdict")
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
	if transferred {
		fmt.Fprintf(a.deps.Stdout, "\n%s\nReport received from PC1: %s\n", verdictLine, dir)
	} else {
		fmt.Fprintf(a.deps.Stdout, "\n%s\nSummary: %s\n", verdictLine, summaryPath)
	}
	code := ExitCode(comp.ExitCode)
	if code < ExitOK || code > ExitInternal {
		return ExitInternal, fmt.Errorf("app: PC1 reported an out-of-range exit code %d", comp.ExitCode)
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
