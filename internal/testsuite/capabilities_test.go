package testsuite

import (
	"context"
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// TestCapabilityDetection pins the version+help agreement rule: a capability
// is claimed only when both the version window and the --help text agree, and
// anything that is not iperf3 3.x fails hard.
func TestCapabilityDetection(t *testing.T) {
	ctx := context.Background()

	detect := func(t *testing.T, version, help runner.CommandResult) (capsResult, error) {
		t.Helper()
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--version"), Result: version})
		fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsExact("--help"), Result: help})
		caps, err := DetectIperfCaps(ctx, fr)
		return capsResult{caps.Version, caps.JSON, caps.Bidir, caps.OneOff, caps.GetServerOutput, caps.UDP}, err
	}

	t.Run("Version39HelpWithoutBidir", func(t *testing.T) {
		got, err := detect(t, fixture(t, "iperf", "version_39"), fixture(t, "iperf", "help_no_bidir"))
		if err != nil {
			t.Fatalf("DetectIperfCaps: %v", err)
		}
		if got.Version != "3.9" {
			t.Errorf("Version = %q, want 3.9", got.Version)
		}
		if got.Bidir {
			t.Errorf("Bidir = true although --help lacks --bidir; both signals must agree")
		}
		if !got.OneOff {
			t.Errorf("OneOff = false although --help lists --one-off")
		}
		if !got.JSON || !got.UDP {
			t.Errorf("JSON/UDP = %v/%v, want unconditional true for any accepted 3.x", got.JSON, got.UDP)
		}
	})

	t.Run("Version316HelpWithBidir", func(t *testing.T) {
		got, err := detect(t, fixture(t, "iperf", "version_316"), fixture(t, "iperf", "help_with_bidir"))
		if err != nil {
			t.Fatalf("DetectIperfCaps: %v", err)
		}
		if got.Version != "3.16" {
			t.Errorf("Version = %q, want 3.16", got.Version)
		}
		if !got.Bidir {
			t.Errorf("Bidir = false although version 3.16 and --help both support --bidir")
		}
		if !got.GetServerOutput {
			t.Errorf("GetServerOutput = false although --help lists --get-server-output")
		}
	})

	t.Run("NonIperf3VersionHardError", func(t *testing.T) {
		version := runner.CommandResult{Stdout: []byte("iperf 2.0.13 (21 Jan 2019) pthreads\n")}
		if _, err := detect(t, version, fixture(t, "iperf", "help_with_bidir")); err == nil {
			t.Errorf("DetectIperfCaps accepted iperf 2.x, want hard error")
		}
		version = runner.CommandResult{Stdout: []byte("something entirely different\n")}
		if _, err := detect(t, version, fixture(t, "iperf", "help_with_bidir")); err == nil {
			t.Errorf("DetectIperfCaps accepted unrecognizable version output, want hard error")
		}
	})

	t.Run("VersionBelowWindowHardError", func(t *testing.T) {
		version := runner.CommandResult{Stdout: []byte("iperf 3.1 (cJSON 1.5.2)\n")}
		if _, err := detect(t, version, fixture(t, "iperf", "help_no_bidir")); err == nil {
			t.Errorf("DetectIperfCaps accepted iperf 3.1, want hard error (support window 3.7-3.17)")
		}
	})
}

// capsResult flattens the fields under test for compact assertions.
type capsResult struct {
	Version                                   string
	JSON, Bidir, OneOff, GetServerOutput, UDP bool
}
