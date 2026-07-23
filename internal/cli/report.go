package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"cablecheck/internal/app"
)

// cmdReport re-renders report.md, summary.txt and report.html from a saved report.json. It
// takes the report.json path as its single positional argument and an optional
// --output directory (default: alongside the JSON). All rendering happens
// offline through the pure reporting layer — the saved classification is never
// re-evaluated.
func cmdReport(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { reportUsage(stderr) }
	output := fs.String("output", "", "directory for the regenerated files (default: alongside report.json)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &app.ExitError{Code: app.ExitConfig, Err: err}
	}
	if fs.NArg() != 1 {
		reportUsage(stderr)
		return &app.ExitError{Code: app.ExitConfig,
			Err: errors.New("report takes exactly one argument: the path to a report.json")}
	}
	return app.Regenerate(fs.Arg(0), *output, stdout)
}

// reportUsage prints the report subcommand help.
func reportUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: cablecheck report [--output dir] <report.json>

Re-render report.md, summary.txt and report.html from a saved report.json. The saved
classification is reproduced verbatim (no re-evaluation).

Flags:
  --output string   directory for the regenerated files (default: alongside report.json)
`)
}
