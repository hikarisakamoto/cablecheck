// Package reporting owns the on-disk report layout and the three rendered
// outputs of a run: report.json, report.md and summary.txt.
//
// The package depends on internal/model and the standard library ONLY (a test
// enforces it structurally). Rendering is a pure function of *model.Report,
// which is what makes offline regeneration (`cablecheck report <json>`)
// trivially correct.
package reporting

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// NewReportDir creates <base>/cablecheck-report-YYYY-MM-DD_HH-MM-SS (mode
// 0700) with a raw/ subdirectory (0700) and returns the report directory
// path.
//
// Same-second collisions get a -2..-99 suffix; os.Mkdir (never MkdirAll) is
// used so EEXIST detection is atomic. The timestamp comes from the caller —
// this package never reads the clock.
func NewReportDir(base string, now time.Time) (string, error) {
	name := "cablecheck-report-" + now.Format("2006-01-02_15-04-05")
	for i := 1; i <= 99; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", name, i)
		}
		dir := filepath.Join(base, candidate)
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create report directory: %w", err)
		}
		if err := os.Mkdir(filepath.Join(dir, "raw"), 0o700); err != nil {
			return "", fmt.Errorf("create raw directory: %w", err)
		}
		return dir, nil
	}
	return "", fmt.Errorf("create report directory: %s-2..-99 all exist under %s", name, base)
}
