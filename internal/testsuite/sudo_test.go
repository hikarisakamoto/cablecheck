package testsuite

import (
	"context"
	"slices"
	"testing"
	"time"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// TestSudoProbe checks the passwordless-sudo probe: exit 0 means available,
// anything else (exit 1, sudo missing) means unavailable, and the probe runs
// "sudo -n true" under a 5 second timeout.
func TestSudoProbe(t *testing.T) {
	ctx := context.Background()

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "sudo", Match: runnertest.ArgsExact("-n", "true"),
		Result: runner.CommandResult{ExitCode: 0}})
	if !SudoProbe(ctx, fr) {
		t.Errorf("SudoProbe = false on exit 0, want true")
	}
	call := fr.CallsFor("sudo")[0]
	if !slices.Equal(call.Args, []string{"-n", "true"}) {
		t.Errorf("probe args = %q, want [-n true]", call.Args)
	}
	if call.Spec.Timeout != 5*time.Second {
		t.Errorf("probe timeout = %v, want 5s", call.Spec.Timeout)
	}

	fr2 := runnertest.New(t)
	fr2.Script(runnertest.Script{Name: "sudo", Result: runner.CommandResult{ExitCode: 1,
		Stderr: []byte("sudo: a password is required\n")}})
	if SudoProbe(ctx, fr2) {
		t.Errorf("SudoProbe = true on exit 1, want false")
	}

	fr3 := runnertest.New(t)
	fr3.Missing("sudo")
	if SudoProbe(ctx, fr3) {
		t.Errorf("SudoProbe = true with sudo missing, want false")
	}
}

// TestPrivileged checks the sudo wrapper: root passes the spec through,
// non-root with sudo prepends "sudo -n --", non-root without sudo reports
// the operation as not runnable.
func TestPrivileged(t *testing.T) {
	restore := geteuid
	defer func() { geteuid = restore }()

	spec := runner.CommandSpec{Name: "ethtool", Args: []string{"--cable-test", "eth0"}}

	geteuid = func() int { return 0 }
	got, ok := Privileged(spec, false)
	if !ok || got.Name != "ethtool" || !slices.Equal(got.Args, spec.Args) {
		t.Errorf("root Privileged = %+v ok=%v, want unchanged spec, true", got, ok)
	}

	geteuid = func() int { return 1000 }
	got, ok = Privileged(spec, true)
	if !ok || got.Name != "sudo" {
		t.Errorf("non-root+sudo Privileged name = %q ok=%v, want sudo, true", got.Name, ok)
	}
	want := []string{"-n", "--", "ethtool", "--cable-test", "eth0"}
	if !slices.Equal(got.Args, want) {
		t.Errorf("non-root+sudo args = %q, want %q", got.Args, want)
	}

	got, ok = Privileged(spec, false)
	if ok {
		t.Errorf("non-root without sudo reported runnable; want ok=false (operation UNAVAILABLE)")
	}
	if got.Name != "ethtool" {
		t.Errorf("non-runnable spec mutated: %+v", got)
	}
}
