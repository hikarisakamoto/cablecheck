package app

import (
	"bytes"
	"fmt"
	"io"

	"cablecheck/internal/model"
	"cablecheck/internal/reporting"
)

// Compare loads two saved reports and writes a deterministic, offline A/B
// comparison. The renderer consumes the saved verdicts and measurements
// verbatim; it never re-evaluates either run.
func Compare(baselinePath, candidatePath string, stdout io.Writer) error {
	baseline, err := loadSavedReport(baselinePath)
	if err != nil {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf("load baseline report: %w", err)}
	}
	if err := validateSavedClassification(baseline.Classification); err != nil {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf("baseline report %s: %w", baselinePath, err)}
	}

	candidate, err := loadSavedReport(candidatePath)
	if err != nil {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf("load candidate report: %w", err)}
	}
	if err := validateSavedClassification(candidate.Classification); err != nil {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf("candidate report %s: %w", candidatePath, err)}
	}

	if _, err := io.Copy(stdout, bytes.NewReader(reporting.RenderComparison(baseline, candidate))); err != nil {
		return &ExitError{Code: ExitInternal, Err: fmt.Errorf("write comparison: %w", err)}
	}
	return nil
}

func validateSavedClassification(class model.HealthClass) error {
	switch class {
	case model.HealthExcellent, model.HealthGood, model.HealthWarning,
		model.HealthPoor, model.HealthFailed, model.HealthInconclusive:
		return nil
	case "":
		return fmt.Errorf("report has no saved classification")
	default:
		return fmt.Errorf("report has unknown saved classification %q", class)
	}
}
