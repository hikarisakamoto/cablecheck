package testsuite

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/runner"
)

// Iperf3 support window: versions below 3.7 predate --bidir and several JSON
// shape fixes CableCheck relies on; versions above 3.17 are untested.
const (
	iperfMinMinor = 7
	iperfMaxMinor = 17
)

// iperfVersionRe matches the first line of `iperf3 --version`,
// e.g. "iperf 3.16 (cJSON 1.7.15)".
var iperfVersionRe = regexp.MustCompile(`^iperf (\d+)\.(\d+)`)

// capProbeTimeout bounds the --version/--help probes.
const capProbeTimeout = 10 * time.Second

// DetectIperfCaps probes the local iperf3 binary via `--version` and `--help`
// and reports its capabilities. A flag-dependent capability is claimed only
// when BOTH signals agree: the version window says the upstream feature
// exists AND the --help usage text lists the flag (which catches distro
// patches and backports, since the usage text is generated from the accepted
// option table). Anything that is not iperf3 within the 3.7-3.17 support
// window is a hard error.
func DetectIperfCaps(ctx context.Context, r runner.Runner) (model.Iperf3Caps, error) {
	var caps model.Iperf3Caps

	verRes, err := r.Run(ctx, runner.CommandSpec{
		Name: "iperf3", Args: []string{"--version"},
		Timeout: capProbeTimeout, Label: "iperf3-version",
	})
	if err != nil {
		return caps, fmt.Errorf("testsuite: iperf3 capability probe: %w", err)
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(verRes.Stdout)), "\n")
	m := iperfVersionRe.FindStringSubmatch(firstLine)
	if m == nil {
		return caps, fmt.Errorf("testsuite: %q does not look like iperf3 (iperf3 3.%d-3.%d required)",
			firstLine, iperfMinMinor, iperfMaxMinor)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major != 3 || minor < iperfMinMinor || minor > iperfMaxMinor {
		return caps, fmt.Errorf("testsuite: iperf %d.%d is outside the supported window 3.%d-3.%d",
			major, minor, iperfMinMinor, iperfMaxMinor)
	}
	caps.Version = fmt.Sprintf("%d.%d", major, minor)

	helpRes, err := r.Run(ctx, runner.CommandSpec{
		Name: "iperf3", Args: []string{"--help"},
		Timeout: capProbeTimeout, Label: "iperf3-help",
	})
	if err != nil {
		return caps, fmt.Errorf("testsuite: iperf3 capability probe: %w", err)
	}
	help := string(helpRes.Stdout) + string(helpRes.Stderr)
	helpHas := func(flag string) bool { return strings.Contains(help, flag) }

	// Unconditional for any accepted 3.x: -J, -R and -u have been present
	// since 3.0/3.1, well below the support window's floor.
	caps.JSON = true
	caps.Reverse = true
	caps.UDP = true

	// Version window and --help must both agree per flag.
	caps.Bidir = minor >= 7 && helpHas("--bidir")
	caps.OneOff = helpHas("--one-off")
	caps.GetServerOutput = helpHas("--get-server-output")

	return caps, nil
}
