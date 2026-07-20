package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"cablecheck/internal/app"
	"cablecheck/internal/config"
)

// cmdRun parses the run flags, resolves the configuration and executes the
// full two-peer run through the app layer.
func cmdRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, build app.BuildInfo) error {
	cfg, err := parseRunFlags(args, stderr)
	if err != nil {
		return err
	}
	progress := NewProgress(stdout, cfg.Verbose)
	a, err := app.New(cfg, app.Deps{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Build:  build,
		OnStep: progress.Step,
	})
	if err != nil {
		return err
	}
	if err := a.Start(ctx); err != nil {
		return err
	}
	code, werr := a.Wait()
	if code == 0 && werr == nil {
		return nil
	}
	return &app.ExitError{Code: code, Err: werr}
}

// parseRunFlags registers the 23 run flags, parses args, tracks explicitly
// set flags via flag.Visit and resolves the configuration (presets fill
// everything unset). Flag parse errors map to exit 4; -h yields
// flag.ErrHelp (exit 0).
func parseRunFlags(args []string, stderr io.Writer) (*config.RunConfig, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { runUsage(stderr) }

	var raw config.RawRunFlags
	// Connection.
	fs.StringVar(&raw.Role, "role", "", "")
	fs.StringVar(&raw.LocalIP, "local-ip", "", "")
	fs.StringVar(&raw.PeerIP, "peer-ip", "", "")
	fs.StringVar(&raw.Interface, "interface", "", "")
	fs.IntVar(&raw.ControlPort, "control-port", 44300, "")
	fs.IntVar(&raw.IperfPort, "iperf-port", 44301, "")
	fs.StringVar(&raw.Token, "token", "", "")
	// Test parameters.
	fs.StringVar(&raw.Mode, "mode", "quick", "")
	fs.DurationVar(&raw.TCPDuration, "tcp-duration", 0, "")
	fs.DurationVar(&raw.UDPDuration, "udp-duration", 0, "")
	fs.StringVar(&raw.UDPRate, "udp-rate", "", "")
	fs.IntVar(&raw.ParallelStreams, "parallel-streams", 0, "")
	fs.DurationVar(&raw.SoakDuration, "soak-duration", 0, "")
	fs.StringVar(&raw.SoakLoad, "soak-load", "", "")
	fs.DurationVar(&raw.MonitorInterval, "monitor-interval", 0, "")
	// Diagnostics.
	fs.BoolVar(&raw.CableTest, "cable-test", false, "")
	fs.BoolVar(&raw.CableTestTDR, "cable-test-tdr", false, "")
	// Behavior.
	fs.BoolVar(&raw.Verbose, "verbose", false, "")
	fs.BoolVar(&raw.NonInteractive, "non-interactive", false, "")
	fs.BoolVar(&raw.NoSudo, "no-sudo", false, "")
	fs.BoolVar(&raw.NoReportTransfer, "no-report-transfer", false, "")
	fs.BoolVar(&raw.AllowVirtualInterface, "allow-virtual-interface", false, "")
	// Output.
	fs.StringVar(&raw.Output, "output", ".", "")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, &app.ExitError{Code: app.ExitConfig, Err: err}
	}
	if fs.NArg() > 0 {
		return nil, &app.ExitError{Code: app.ExitConfig, Err: fmt.Errorf(
			"unexpected argument %q — boolean flags take the --flag=false form, not a separate value", fs.Arg(0))}
	}

	// Only flag.Visit distinguishes an explicit value from the registered
	// default; presets must fill exactly the unset flags.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return config.Resolve(raw, set)
}

// runUsage prints the grouped run flag reference (hand-ordered — the
// default alphabetic PrintDefaults is useless for 23 flags).
func runUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: cablecheck run --role pc1|pc2 --local-ip A.B.C.D --peer-ip A.B.C.D [flags]

Connection:
  --role string             pc1 (coordinator) or pc2 (worker); required
  --local-ip string         IPv4 of the test interface on this machine; required
                            unless --interface is given (then inferred from it)
  --peer-ip string          IPv4 of the test interface on the other machine; required
  --interface string        interface name (default: auto-discover by --local-ip);
                            when given, --local-ip may be omitted and is inferred
                            from this interface's sole IPv4 address
  --control-port int        control channel TCP port (default 44300)
  --iperf-port int          iperf3 data port; port+1 is reserved too (default 44301)
  --token string            shared session token; pc1 generates one when empty,
                            pc2 requires the token displayed by pc1

Test parameters:
  --mode string             quick | standard | soak (default quick). standard adds
                            repeated 60s TCP, an extra reduced-rate UDP pass and
                            longer pings; soak repeats test cycles for --soak-duration
  --tcp-duration duration   per-direction TCP test length (default 30s/60s/60s per mode)
  --udp-duration duration   UDP test length (default 20s/30s/20s per mode)
  --udp-rate string         UDP target bitrate, e.g. 800M
                            (default: 80% of the negotiated link speed)
  --parallel-streams int    iperf3 -P stream count, 1-16 (default 4)
  --soak-duration duration  soak mode only: total soak length (default 1h)
  --soak-load string        soak mode only: periodic (idle gaps) | continuous
                            (back-to-back cycles) (default periodic)
  --monitor-interval duration
                            link monitor polling interval (default 1s)

Diagnostics:
  --cable-test              run coordinated ethtool cable diagnostics
  --cable-test-tdr          request TDR cable diagnostics (implies --cable-test)

Behavior:
  --non-interactive         skip the interactive start prompt
  --no-sudo                 never probe or use sudo
  --no-report-transfer      do not send report files to pc2
  --allow-virtual-interface permit loopback/virtual interfaces (demo mode)
  --verbose                 verbose progress and debug logging

Output:
  --output string           parent directory for the report directory (default ".")

Boolean flags take no separate value: use --cable-test=false, not --cable-test false.

Exit codes: 0 GOOD/EXCELLENT · 1 WARNING · 2 POOR/FAILED · 3 INCONCLUSIVE ·
4 config/dependency · 5 peer failure · 6 interrupted · 7 internal
`)
}
