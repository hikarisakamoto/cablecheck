package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"cablecheck/internal/app"
	"cablecheck/internal/config"
	"cablecheck/internal/ui"
)

// cmdDoctor checks local dependencies and configuration without contacting a
// peer. It renders the aligned [PASS]/[WARN]/[FAIL] check lines and a summary
// to stdout and returns an *ExitError{ExitConfig} (exit 4) when any check
// FAILs; otherwise nil (doctor never emits exit 3).
func cmdDoctor(ctx context.Context, args []string, stdout, stderr io.Writer, build app.BuildInfo) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { doctorUsage(stderr) }
	iface := fs.String("interface", "", "restrict the interface inventory to this name")
	output := fs.String("output", ".", "parent directory whose writability is probed")
	noSudo := fs.Bool("no-sudo", false, "skip the passwordless-sudo probe")
	allowVirtual := fs.Bool("allow-virtual-interface", false, "annotate virtual interfaces as usable")
	color := fs.String("color", "auto", "auto | always | never")
	// --verbose is accepted for symmetry with run; doctor emits no slog output
	// of its own, so the flag currently has no behavioral effect.
	_ = fs.Bool("verbose", false, "verbose logging (accepted; no effect on doctor)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &app.ExitError{Code: app.ExitConfig, Err: err}
	}
	if fs.NArg() > 0 {
		doctorUsage(stderr)
		return &app.ExitError{Code: app.ExitConfig,
			Err: fmt.Errorf("doctor takes no positional arguments (got %q)", fs.Arg(0))}
	}
	// Same validation (and ValidationError => exit 4) as run's --color.
	switch *color {
	case "auto", "always", "never":
	default:
		return &config.ValidationError{Flag: "--color",
			Msg: fmt.Sprintf("invalid color mode %q: must be auto, always, or never", *color)}
	}

	deps := app.Deps{
		Stdout: stdout,
		Stderr: stderr,
		Build:  build,
	}
	checks, err := app.Doctor(ctx, deps, app.DoctorOptions{
		Interface:    *iface,
		OutputDir:    *output,
		NoSudo:       *noSudo,
		AllowVirtual: *allowVirtual,
	})
	app.RenderDoctor(stdout, checks, ui.DecideColor(mapColorMode(*color), stdout, nil))
	return err
}

// doctorUsage prints the doctor subcommand help.
func doctorUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: cablecheck doctor [flags]

Check local dependencies (ip, ping, iperf3, ethtool), enumerate interfaces,
probe sudo, and verify the output directory is writable. Exits 4 if any check
FAILs, 0 otherwise.

Flags:
  --interface string          restrict the interface inventory to this name
  --output string             parent directory whose writability is probed (default ".")
  --no-sudo                   skip the passwordless-sudo probe
  --allow-virtual-interface   annotate virtual interfaces as usable
  --color string              auto | always | never (default auto)
  --verbose                   verbose logging
`)
}
