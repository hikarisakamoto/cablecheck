// Package cli parses the cablecheck command line, dispatches the run,
// doctor, report and version subcommands, and maps every error onto the
// 0-7 exit-code contract. All parsing uses flag.ContinueOnError — never
// ExitOnError, whose os.Exit(2) would break the contract.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"cablecheck/internal/app"
	"cablecheck/internal/config"
)

// Run is the cablecheck entry point below main: it dispatches args[0] as
// the subcommand and returns the process exit code. All I/O goes through
// the injected streams; ctx is the signal-cancelled process context.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, build app.BuildInfo) int {
	return mapError(ctx, dispatch(ctx, args, stdin, stdout, stderr, build), stderr)
}

// dispatch routes to the subcommand handler.
func dispatch(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, build app.BuildInfo) error {
	if len(args) == 0 {
		usage(stderr)
		return &app.ExitError{Code: app.ExitConfig, Err: errors.New("missing subcommand")}
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	case "run":
		return cmdRun(ctx, args[1:], stdin, stdout, stderr, build)
	case "doctor":
		return cmdDoctor(ctx, args[1:], stdout, stderr, build)
	case "report":
		return cmdReport(ctx, args[1:], stdout, stderr)
	case "version":
		return cmdVersion(stdout, build)
	default:
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(stderr, "cablecheck: flags go after the subcommand: cablecheck <command> [flags]\n\n")
			usage(stderr)
			return &app.ExitError{Code: app.ExitConfig, Err: fmt.Errorf("flag %q before subcommand", args[0])}
		}
		fmt.Fprintf(stderr, "cablecheck: unknown command %q\n\n", args[0])
		usage(stderr)
		return &app.ExitError{Code: app.ExitConfig, Err: fmt.Errorf("unknown command %q", args[0])}
	}
}

// mapError converts a dispatch error to the exit code, printing diagnoses
// to stderr. Policy order (docs/design/clieval.md §1.2): nil and explicit
// help are 0; a typed ExitError wins; a config ValidationError is 4; a
// cancellation observed while the signal context fired is the interrupt
// (6); anything unrecognized is internal (7).
func mapError(ctx context.Context, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var xe *app.ExitError
	if errors.As(err, &xe) {
		if xe.Err != nil {
			fmt.Fprintf(stderr, "cablecheck: %v\n", xe.Err)
		}
		return int(xe.Code)
	}
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		fmt.Fprintf(stderr, "cablecheck: %v\n", ve)
		return int(app.ExitConfig)
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return int(app.ExitInterrupt)
	}
	fmt.Fprintf(stderr, "cablecheck: internal error: %v\n", err)
	return int(app.ExitInternal)
}

// usage prints the top-level command summary.
func usage(w io.Writer) {
	fmt.Fprint(w, `cablecheck — Ethernet cable health tester for two directly connected Linux PCs

Usage:
  cablecheck <command> [flags]

Commands:
  run      Run a coordinated cable test (start on both PCs, pc1 first)
  doctor   Check local dependencies and configuration
  report   Re-render report.md/summary.txt from a report.json
  version  Print version, protocol and schema information

Run "cablecheck run -h" for the full flag reference.

Exit codes:
  0 GOOD/EXCELLENT   1 WARNING   2 POOR/FAILED   3 INCONCLUSIVE
  4 config/dependency error   5 peer/orchestration failure
  6 interrupted   7 internal error
`)
}
