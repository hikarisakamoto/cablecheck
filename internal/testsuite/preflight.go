package testsuite

import (
	"fmt"
	"strconv"
	"strings"

	"cablecheck/internal/runner"
)

// PreflightStaleProcesses scans the pidfile tree under base for survivors of
// earlier cablecheck runs. Pidfiles whose process is dead (or no longer
// ownership-verifies) are cleaned silently; a live, ownership-verified
// survivor fails preflight with remediation text naming the PID, because a
// leftover iperf3 would occupy the test port and corrupt measurements. It
// returns the live survivors alongside the error describing them.
func PreflightStaleProcesses(base string) ([]runner.ProcessInfo, error) {
	stale, _, err := runner.ScanStale(base)
	if err != nil {
		return nil, fmt.Errorf("testsuite: stale-process preflight: %w", err)
	}
	if len(stale) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("stale cablecheck-owned processes from an earlier run are still alive:")
	for _, p := range stale {
		fmt.Fprintf(&b, "\n  %s (pid %d, session %s)", p.Argv0, p.PID, p.TestID)
	}
	b.WriteString("\nterminate them before retrying, e.g.: kill ")
	pids := make([]string, len(stale))
	for i, p := range stale {
		pids[i] = strconv.Itoa(p.PID)
	}
	b.WriteString(strings.Join(pids, " "))
	return stale, fmt.Errorf("testsuite: stale-process preflight: %s", b.String())
}
