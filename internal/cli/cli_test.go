package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"cablecheck/internal/app"
	"cablecheck/internal/config"
	"cablecheck/internal/model"
	"cablecheck/internal/reporting"
)

// testBuild is the injected build identity used across the cli tests.
var testBuild = app.BuildInfo{Version: "1.2.3", Commit: "abc1234", Date: "2026-07-15T10:22:03Z"}

// runCLI invokes Run with buffers and returns (code, stdout, stderr).
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &errOut, testBuild)
	return code, out.String(), errOut.String()
}

// TestCLIDispatch pins subcommand dispatch and its error/usage behavior.
func TestCLIDispatch(t *testing.T) {
	t.Run("no args prints usage and exits 4", func(t *testing.T) {
		code, _, errOut := runCLI(t)
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "Usage:") {
			t.Errorf("stderr misses usage:\n%s", errOut)
		}
	})

	t.Run("help flags exit 0 with usage on stdout", func(t *testing.T) {
		for _, arg := range []string{"-h", "--help", "help"} {
			code, out, _ := runCLI(t, arg)
			if code != 0 {
				t.Errorf("%s: code = %d, want 0", arg, code)
			}
			if !strings.Contains(out, "Usage:") {
				t.Errorf("%s: stdout misses usage:\n%s", arg, out)
			}
		}
	})

	t.Run("unknown subcommand exits 4 with usage", func(t *testing.T) {
		code, _, errOut := runCLI(t, "bogus")
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "unknown command") || !strings.Contains(errOut, "Usage:") {
			t.Errorf("stderr misses diagnosis + usage:\n%s", errOut)
		}
	})

	t.Run("flag before subcommand exits 4 with hint", func(t *testing.T) {
		code, _, errOut := runCLI(t, "--verbose", "run")
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "flags go after the subcommand") {
			t.Errorf("stderr misses the ordering hint:\n%s", errOut)
		}
	})

	t.Run("run flag parse error exits 4 via ContinueOnError", func(t *testing.T) {
		code, _, errOut := runCLI(t, "run", "--no-such-flag")
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "no-such-flag") {
			t.Errorf("stderr misses the flag diagnosis:\n%s", errOut)
		}
	})

	t.Run("run config validation error exits 4", func(t *testing.T) {
		code, _, errOut := runCLI(t, "run")
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "--role") {
			t.Errorf("stderr misses the --role diagnosis:\n%s", errOut)
		}
	})

	t.Run("invalid run color exits 4", func(t *testing.T) {
		args := append([]string{"run"}, baseRunArgs("--color", "sometimes")...)
		code, _, errOut := runCLI(t, args...)
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "--color") {
			t.Errorf("stderr misses the --color diagnosis:\n%s", errOut)
		}
	})

	t.Run("run help includes presentation flags", func(t *testing.T) {
		code, _, errOut := runCLI(t, "run", "-h")
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
		if !strings.Contains(errOut, "--color") || !strings.Contains(errOut, "--quiet") {
			t.Errorf("run help misses --color/--quiet:\n%s", errOut)
		}
	})

	t.Run("run rejects positional arguments", func(t *testing.T) {
		code, _, errOut := runCLI(t, "run", "positional")
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "positional") {
			t.Errorf("stderr misses the positional diagnosis:\n%s", errOut)
		}
	})

	t.Run("run -h exits 0", func(t *testing.T) {
		code, _, _ := runCLI(t, "run", "-h")
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
	})

	t.Run("version exits 0", func(t *testing.T) {
		code, out, _ := runCLI(t, "version")
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
		if !strings.Contains(out, "cablecheck 1.2.3") {
			t.Errorf("stdout misses version banner:\n%s", out)
		}
	})

	t.Run("report without a path exits 4 with usage", func(t *testing.T) {
		code, _, errOut := runCLI(t, "report")
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "report.json") {
			t.Errorf("stderr misses the missing-path hint:\n%s", errOut)
		}
	})

	t.Run("report -h exits 0", func(t *testing.T) {
		code, _, _ := runCLI(t, "report", "-h")
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
	})

	t.Run("report on a missing file exits 4", func(t *testing.T) {
		code, _, errOut := runCLI(t, "report", filepath.Join(t.TempDir(), "nope.json"))
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if errOut == "" {
			t.Errorf("stderr is empty, want a read error")
		}
	})

	t.Run("report on a non-regular file exits 4", func(t *testing.T) {
		code, _, errOut := runCLI(t, "report", t.TempDir())
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "regular file") {
			t.Errorf("stderr misses the non-regular-file diagnosis:\n%s", errOut)
		}
	})

	t.Run("report with an invalid output directory exits 4", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "report.json")
		if err := os.WriteFile(path, cliValidReportJSON(t), 0o600); err != nil {
			t.Fatalf("write report.json: %v", err)
		}
		outDir := filepath.Join(dir, "does-not-exist")
		code, _, errOut := runCLI(t, "report", "--output", outDir, path)
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "report.md") {
			t.Errorf("stderr misses the output-write diagnosis:\n%s", errOut)
		}
	})

	t.Run("report regenerates from a valid report.json exits 0", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "report.json")
		if err := os.WriteFile(path, cliValidReportJSON(t), 0o600); err != nil {
			t.Fatalf("write report.json: %v", err)
		}
		code, out, errOut := runCLI(t, "report", path)
		if code != 0 {
			t.Errorf("code = %d, want 0 (stderr: %s)", code, errOut)
		}
		if !strings.Contains(out, "report.md") {
			t.Errorf("stdout misses the wrote-file confirmation:\n%s", out)
		}
		for _, name := range []string{"report.md", "summary.txt"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s was not regenerated: %v", name, err)
			}
		}
	})

	t.Run("doctor -h exits 0", func(t *testing.T) {
		code, _, _ := runCLI(t, "doctor", "-h")
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
	})

	t.Run("doctor rejects positional arguments", func(t *testing.T) {
		code, _, errOut := runCLI(t, "doctor", "positional")
		if code != 4 {
			t.Errorf("code = %d, want 4", code)
		}
		if !strings.Contains(errOut, "positional") {
			t.Errorf("stderr misses the positional diagnosis:\n%s", errOut)
		}
	})
}

// cliValidReportJSON renders a minimal valid report.json via the reporting
// layer, so the CLI report test drives the real Regenerate engine end to end.
func cliValidReportJSON(t *testing.T) []byte {
	t.Helper()
	score := 96
	rep := &model.Report{
		SchemaVersion:         model.SchemaVersion,
		ToolVersion:           "1.0.0",
		ProtocolVersion:       "1",
		TestID:                "cli-report-test",
		Configuration:         model.ConfigEcho{Role: "pc1", Mode: "quick"},
		Classification:        model.HealthExcellent,
		Score:                 &score,
		ClassificationReasons: []string{"all measured tests passed"},
	}
	data, err := reporting.RenderJSON(rep)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	return data
}

// TestExitCodeMapping pins the Run error-unwrap policy: ExitError wins, then
// ValidationError (4), then signal-context cancellation (6), then internal (7).
func TestExitCodeMapping(t *testing.T) {
	liveCtx := context.Background()
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	classCases := []struct {
		class model.HealthClass
		want  int
	}{
		{model.HealthExcellent, 0},
		{model.HealthGood, 0},
		{model.HealthWarning, 1},
		{model.HealthPoor, 2},
		{model.HealthFailed, 2},
		{model.HealthInconclusive, 3},
	}
	for _, c := range classCases {
		err := &app.ExitError{Code: app.ExitCodeFor(c.class)}
		if got := mapError(liveCtx, err, &bytes.Buffer{}); got != c.want {
			t.Errorf("classification %s: mapError = %d, want %d", c.class, got, c.want)
		}
	}

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"nil error", liveCtx, nil, 0},
		{"flag.ErrHelp", liveCtx, flag.ErrHelp, 0},
		{"wrapped flag.ErrHelp", liveCtx, fmt.Errorf("parse: %w", flag.ErrHelp), 0},
		{"ValidationError", liveCtx, &config.ValidationError{Flag: "--local-ip", Msg: "bad"}, 4},
		{"wrapped ValidationError", liveCtx, fmt.Errorf("resolve: %w", &config.ValidationError{Flag: "--mode", Msg: "bad"}), 4},
		{"ExitError beats ValidationError", liveCtx, &app.ExitError{Code: app.ExitPeer, Err: &config.ValidationError{Flag: "--x", Msg: "y"}}, 5},
		{"context.Canceled with signal ctx", canceledCtx, context.Canceled, 6},
		{"wrapped context.Canceled with signal ctx", canceledCtx, fmt.Errorf("dial: %w", context.Canceled), 6},
		{"context.Canceled without signal", liveCtx, context.Canceled, 7},
		{"unknown error", liveCtx, errors.New("boom"), 7},
	}
	for _, c := range cases {
		if got := mapError(c.ctx, c.err, &bytes.Buffer{}); got != c.want {
			t.Errorf("%s: mapError = %d, want %d", c.name, got, c.want)
		}
	}

	t.Run("ExitError message reaches stderr", func(t *testing.T) {
		var buf bytes.Buffer
		mapError(liveCtx, &app.ExitError{Code: app.ExitConfig, Err: errors.New("bad port")}, &buf)
		if !strings.Contains(buf.String(), "bad port") {
			t.Errorf("stderr misses the wrapped message: %q", buf.String())
		}
	})
}

// TestVersionOutput pins the version command rendering: injected BuildInfo
// plus go runtime version, platform, protocol "1" and schema "1.0.0".
func TestVersionOutput(t *testing.T) {
	code, out, _ := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("version exited %d", code)
	}
	for _, want := range []string{
		"cablecheck 1.2.3",
		"commit:   abc1234",
		"built:    2026-07-15T10:22:03Z",
		"go:       " + runtime.Version(),
		"platform: " + runtime.GOOS + "/" + runtime.GOARCH,
		"protocol: 1",
		"schema:   1.0.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("version output misses %q:\n%s", want, out)
		}
	}
}
