package app

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"cablecheck/internal/model"
)

func TestCompareSavedReports(t *testing.T) {
	baseline := regenReport()
	baseline.Classification = model.HealthExcellent
	candidate := regenReport()
	candidate.TestID = "candidate-report"
	candidate.Classification = model.HealthFailed
	baseDir, candidateDir := t.TempDir(), t.TempDir()
	basePath := writeReportJSON(t, baseDir, baseline)
	candidatePath := writeReportJSON(t, candidateDir, candidate)

	var stdout bytes.Buffer
	if err := Compare(basePath, candidatePath, &stdout); err != nil {
		t.Fatalf("Compare: %v", err)
	}
	for _, want := range []string{"CableCheck comparison", "EXCELLENT -> FAILED", "Assessment: WORSE"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("comparison misses %q:\n%s", want, stdout.String())
		}
	}
}

func TestCompareInputErrorsAreExitConfig(t *testing.T) {
	validDir := t.TempDir()
	validPath := writeReportJSON(t, validDir, regenReport())
	missing := filepath.Join(t.TempDir(), "missing.json")

	for _, tc := range []struct {
		name      string
		baseline  string
		candidate string
		wantText  string
	}{
		{"missing baseline", missing, validPath, "baseline"},
		{"missing candidate", validPath, missing, "candidate"},
		{"non-regular baseline", t.TempDir(), validPath, "regular file"},
		{"non-regular candidate", validPath, t.TempDir(), "regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Compare(tc.baseline, tc.candidate, io.Discard)
			assertExitConfig(t, err)
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not contain %q", err, tc.wantText)
			}
		})
	}
}

func TestCompareRejectsInvalidSavedClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class model.HealthClass
	}{
		{"missing", ""},
		{"unknown", "PERFECT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline := regenReport()
			baseline.Classification = tc.class
			basePath := writeReportJSON(t, t.TempDir(), baseline)
			candidatePath := writeReportJSON(t, t.TempDir(), regenReport())
			err := Compare(basePath, candidatePath, io.Discard)
			assertExitConfig(t, err)
			if !strings.Contains(err.Error(), "classification") {
				t.Errorf("error %q does not identify the invalid classification", err)
			}
		})
	}
}

func TestCompareAcceptsSameMajorSchemaDifference(t *testing.T) {
	baseline := regenReport()
	candidate := regenReport()
	candidate.SchemaVersion = "1.9.0"
	basePath := writeReportJSON(t, t.TempDir(), baseline)
	candidatePath := writeReportJSON(t, t.TempDir(), candidate)

	var stdout bytes.Buffer
	if err := Compare(basePath, candidatePath, &stdout); err != nil {
		t.Fatalf("Compare rejected same-major reports: %v", err)
	}
	if !strings.Contains(stdout.String(), "schema versions differ") {
		t.Errorf("comparison omitted schema warning:\n%s", stdout.String())
	}
}

func TestCompareRejectsUnsupportedCandidateSchema(t *testing.T) {
	baseline := regenReport()
	candidate := regenReport()
	candidate.SchemaVersion = "2.0.0"
	basePath := writeReportJSON(t, t.TempDir(), baseline)
	candidatePath := writeReportJSON(t, t.TempDir(), candidate)

	err := Compare(basePath, candidatePath, io.Discard)
	assertExitConfig(t, err)
	if !strings.Contains(err.Error(), "candidate") || !strings.Contains(err.Error(), "schema") {
		t.Errorf("error %q does not identify the candidate schema mismatch", err)
	}
}

func TestCompareOutputFailureIsExitInternal(t *testing.T) {
	basePath := writeReportJSON(t, t.TempDir(), regenReport())
	candidatePath := writeReportJSON(t, t.TempDir(), regenReport())
	err := Compare(basePath, candidatePath, failingCompareWriter{})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitInternal {
		t.Fatalf("Compare output error = %v, want ExitInternal", err)
	}
}

type failingCompareWriter struct{}

func (failingCompareWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
