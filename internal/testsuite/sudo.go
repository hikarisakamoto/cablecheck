package testsuite

import (
	"context"
	"os"
	"time"

	"cablecheck/internal/runner"
)

// sudoProbeTimeout bounds the passwordless-sudo probe.
const sudoProbeTimeout = 5 * time.Second

// geteuid is swappable in tests for the root/non-root branches of Privileged.
var geteuid = os.Geteuid

// SudoProbe reports whether passwordless sudo works, by running
// `sudo -n true` under a 5-second timeout. Exit 0 means available; anything
// else — exit 1, "a password is required", or sudo missing entirely — means
// unavailable. Sudo is never invoked without -n anywhere: no mid-test
// password prompts, ever.
func SudoProbe(ctx context.Context, r runner.Runner) bool {
	res, err := r.Run(ctx, runner.CommandSpec{
		Name:    "sudo",
		Args:    []string{"-n", "true"},
		Timeout: sudoProbeTimeout,
		Label:   "sudo-probe",
	})
	return err == nil && res.ExitCode == 0
}

// Privileged wraps spec for root-only operations (ethtool --cable-test):
// running as root passes it through unchanged; otherwise, when the sudo
// probe succeeded, it prepends "sudo -n --". ok=false means the operation
// cannot run at all and must be reported UNAVAILABLE (never FAILED), with
// spec returned unchanged.
func Privileged(spec runner.CommandSpec, sudoOK bool) (_ runner.CommandSpec, ok bool) {
	if geteuid() == 0 {
		return spec, true
	}
	if !sudoOK {
		return spec, false
	}
	wrapped := spec
	wrapped.Name = "sudo"
	wrapped.Args = append([]string{"-n", "--", spec.Name}, spec.Args...)
	return wrapped, true
}
