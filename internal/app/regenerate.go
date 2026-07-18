package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cablecheck/internal/model"
	"cablecheck/internal/reporting"
)

// regenSizeCap bounds the report.json a regeneration will read into memory: a
// well-formed CableCheck report is a few hundred KiB, so 64 MiB is a generous
// ceiling that still refuses a hostile or truncated multi-gigabyte file before
// allocating for it.
const regenSizeCap = 64 << 20

// Regenerate re-renders report.md and summary.txt from a saved report.json
// (docs/design/clieval.md §5.5). It re-parses the record and renders through
// the same pure reporting functions a live run uses — it never re-evaluates,
// because the classification is part of the saved record and must not drift.
//
// path is the report.json to read; outDir is where the two files are written,
// defaulting to the JSON's own directory when empty. Every user-facing failure
// (missing file, oversize file, malformed JSON, unsupported schema major) is
// wrapped in an *ExitError{ExitConfig} so the cli maps it to exit 4.
func Regenerate(path, outDir string, stdout io.Writer) error {
	fi, err := os.Stat(path)
	if err != nil {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf("cannot read %s: %w", path, err)}
	}
	if fi.Size() > regenSizeCap {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf(
			"%s is %d bytes, larger than the %d-byte (64 MiB) report size cap", path, fi.Size(), int64(regenSizeCap))}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf("cannot read %s: %w", path, err)}
	}
	// A regular file can grow between Stat and ReadFile; guard the in-memory
	// size too, so the cap is not merely advisory.
	if int64(len(data)) > regenSizeCap {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf(
			"%s exceeds the %d-byte (64 MiB) report size cap", path, int64(regenSizeCap))}
	}

	var rep model.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return &ExitError{Code: ExitConfig, Err: fmt.Errorf("%s is not a valid cablecheck report: %w", path, err)}
	}
	if err := checkSchemaMajor(rep.SchemaVersion); err != nil {
		return &ExitError{Code: ExitConfig, Err: err}
	}

	if outDir == "" {
		outDir = filepath.Dir(path)
	}
	files := []struct {
		name string
		data []byte
	}{
		{"report.md", reporting.RenderMarkdown(&rep)},
		{"summary.txt", reporting.RenderSummary(&rep)},
	}
	for _, f := range files {
		dst := filepath.Join(outDir, f.name)
		if err := os.WriteFile(dst, f.data, 0o600); err != nil {
			return &ExitError{Code: ExitInternal, Err: fmt.Errorf("write %s: %w", dst, err)}
		}
		fmt.Fprintf(stdout, "wrote %s\n", dst)
	}
	fmt.Fprintf(stdout, "regenerated report for %s (classification %s) from %s\n",
		orUnknownStr(rep.TestID), orUnknownStr(string(rep.Classification)), path)
	return nil
}

// checkSchemaMajor accepts any schemaVersion sharing the current major
// (forward-compatible additive changes) and rejects a different major, whose
// field layout this build cannot be trusted to render.
func checkSchemaMajor(version string) error {
	if version == "" {
		return fmt.Errorf("report has no schemaVersion; it does not look like a cablecheck report")
	}
	major, _, _ := strings.Cut(version, ".")
	got, err := strconv.Atoi(major)
	if err != nil {
		return fmt.Errorf("report schemaVersion %q is malformed", version)
	}
	want, _, _ := strings.Cut(model.SchemaVersion, ".")
	wantMajor, _ := strconv.Atoi(want)
	if got != wantMajor {
		return fmt.Errorf("report schemaVersion %q has major %d; this build renders schema major %d only — upgrade cablecheck to read it",
			version, got, wantMajor)
	}
	return nil
}
