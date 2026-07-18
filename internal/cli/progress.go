package cli

import (
	"fmt"
	"io"
)

// Progress renders test plan step announcements. Quiet mode (the default)
// emits exactly one "[n/N] name" line per step; verbose mode additionally
// announces the plan once before the first step.
type Progress struct {
	w        io.Writer
	verbose  bool
	preamble bool
}

// NewProgress returns a Progress writing to w.
func NewProgress(w io.Writer, verbose bool) *Progress {
	return &Progress{w: w, verbose: verbose}
}

// Step announces one plan step; it matches the app.Deps.OnStep signature.
func (p *Progress) Step(step, total int, name string) {
	if p.verbose && !p.preamble {
		p.preamble = true
		fmt.Fprintf(p.w, "starting test plan: %d steps\n", total)
	}
	fmt.Fprintf(p.w, "[%d/%d] %s\n", step, total, name)
}
