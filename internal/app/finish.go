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
// On success the report was already rendered by the plan wrapper; the files
// are re-rendered once more here so the peer's machine description (known
// only after peer.Run returns) makes it into the record. On failure a
// partial report with Partial=true and FailureDetails is written from
// whatever the plan accumulated.
func (a *App) finishCoordinator(dir, rawDir string, pf *preflightInfo,
	results *testsuite.SessionResults, v *verdict, startedAt time.Time,
	outcome *peer.Outcome, runErr error, log *slog.Logger) (ExitCode, error) {
	if runErr == nil {
		if _, _, _, ok := v.get(); !ok {
			return ExitInternal, errors.New("app: session completed without a verdict")
		}
		// Enrich with the peer capabilities. finalize reuses the stored
		// verdict, so the classification that was already exchanged in the
		// complete frame — and that PC2's exit code is based on — survives
		// the re-rendering unchanged.
		if err := a.finalize(dir, rawDir, pf, results, v, startedAt, nil, outcome, log); err != nil {
			log.Warn("re-rendering the final report failed; the pre-complete rendering stands", "err", err)
		}
		_, summary, code, _ := v.get()
		fmt.Fprintf(a.deps.Stdout, "\n%s\nReport: %s\n", summary, dir)
		return code, nil
	}

	code := exitCodeForRunError(runErr)
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
// exit code. The worker's verdict comes from PC1's complete frame; the full
// report file transfer arrives in a later version.
func (a *App) finishWorker(dir string, outcome *peer.Outcome, runErr error,
	log *slog.Logger) (ExitCode, error) {
	if runErr != nil {
		code := exitCodeForRunError(runErr)
		a.writeWorkerSummary(dir, outcome, fmt.Sprintf("run did not complete: %v", runErr), log)
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
	a.writeWorkerSummary(dir, outcome, verdictLine, log)
	fmt.Fprintf(a.deps.Stdout, "\n%s\nSummary: %s\n", verdictLine, filepath.Join(dir, "summary.txt"))
	code := ExitCode(comp.ExitCode)
	if code < ExitOK || code > ExitInternal {
		return ExitInternal, fmt.Errorf("app: PC1 reported an out-of-range exit code %d", comp.ExitCode)
	}
	return code, nil
}

// writeWorkerSummary renders PC2's local summary.txt from the complete
// payload plus locally known run data.
func (a *App) writeWorkerSummary(dir string, outcome *peer.Outcome, verdictLine string, log *slog.Logger) {
	testID := a.watch.TestID()
	if outcome != nil && outcome.TestID != "" {
		testID = outcome.TestID
	}
	body := fmt.Sprintf(`CableCheck summary (pc2 / worker)
=================================
test id:   %s
mode:      %s
local ip:  %s
peer ip:   %s
%s

The full report (report.json, report.md, summary.txt) was generated on PC1.
Automatic report transfer to PC2 arrives in a later version.
`,
		testID, a.cfg.Mode, a.cfg.LocalIP.Unmap(), a.cfg.PeerIP.Unmap(), verdictLine)
	if err := os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(body), 0o600); err != nil {
		log.Warn("writing the worker summary failed", "err", err)
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
