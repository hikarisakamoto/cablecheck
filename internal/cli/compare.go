package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"cablecheck/internal/app"
)

// cmdCompare renders an offline A/B comparison of two saved reports. The
// first path is the baseline and the second is the candidate under test.
func cmdCompare(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { compareUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &app.ExitError{Code: app.ExitConfig, Err: err}
	}
	if fs.NArg() != 2 {
		compareUsage(stderr)
		return &app.ExitError{Code: app.ExitConfig,
			Err: errors.New("compare takes exactly two arguments: baseline.json and candidate.json")}
	}
	return app.Compare(fs.Arg(0), fs.Arg(1), stdout)
}

func compareUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: cablecheck compare <baseline.json> <candidate.json>

Compare a saved baseline report with a candidate report. The saved classifications
remain authoritative; metrics and comparability warnings are presented without
re-evaluating either run.
`)
}
