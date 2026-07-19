package testsuite

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
)

// icmpOverheadBytes is the IPv4 header (20) plus ICMP header (8): before
// applying IPv4's packet-size limit, a full-MTU payload is MTU minus this.
const icmpOverheadBytes = 28

// maxIPv4ICMPPayload is the largest ICMP payload representable in an IPv4
// packet: 65535 bytes minus the 20-byte IPv4 and 8-byte ICMP headers.
const maxIPv4ICMPPayload = 65507

// pingTimeoutSlack pads the runner timeout beyond the -w wall-clock bound:
// ping itself gets first chance to enforce the deadline, and the runner kills
// a ping that outlives it so a wedged run cannot hang the session.
const pingTimeoutSlack = 10 * time.Second

// pingIntervalLadder is the -i fallback ladder: 20 ms works unprivileged on
// modern iputils (>= 2 ms rule); older builds deny it (exit 2, "cannot flood"
// + "minimal interval") and we climb to the next rung, trading gap-analysis
// granularity for a run that completes at all.
var pingIntervalLadder = []float64{0.02, 0.2, 1.0}

// PingTester runs iputils ping against the peer and parses the per-packet
// output into model.PingResult.
type PingTester struct {
	// R runs ping.
	R runner.Runner
	// RawDir, when non-empty, receives the verbatim ping output.
	RawDir string
}

// Quick runs the quick-mode stability ping (-n -D, count packets), owning the
// interval fallback ladder. The returned notes record limitations such as a
// denied interval; the result's IntervalUsedSec is the rung that actually ran.
func (p *PingTester) Quick(ctx context.Context, peer netip.Addr, count int) (model.PingResult, []string, error) {
	var notes []string
	for i, interval := range pingIntervalLadder {
		wallSec := int(math.Ceil(float64(count)*interval)) + 10
		args := []string{
			"-n", "-D",
			"-c", strconv.Itoa(count),
			"-i", formatInterval(interval),
			"-W", "1",
			"-w", strconv.Itoa(wallSec),
			peer.String(),
		}
		res, notes2, err := p.run(ctx, args, "ping-quick", interval, wallSec)
		if err != nil && errors.Is(err, parser.ErrIntervalRejected) && i < len(pingIntervalLadder)-1 {
			notes = append(notes, fmt.Sprintf(
				"ping interval %ss was denied for this user; fell back to %ss "+
					"(response-gap analysis granularity is reduced)",
				formatInterval(interval), formatInterval(pingIntervalLadder[i+1])))
			continue
		}
		return res, append(notes, notes2...), err
	}
	// Unreachable: the last rung never returns ErrIntervalRejected above.
	return model.PingResult{}, notes, errors.New("testsuite: ping interval ladder exhausted")
}

// FullSize runs the largest valid non-fragmenting IPv4 ping for the interface:
// min(MTU-28, 65507). It fills one link-layer frame when the MTU is representable
// by IPv4; larger MTUs use IPv4's maximum packet size. Loss here that quick-mode
// ping does not show points at frame-size-dependent cable problems.
func (p *PingTester) FullSize(ctx context.Context, peer netip.Addr, mtu, count int) (model.PingResult, []string, error) {
	const interval = 0.2
	wallSec := int(math.Ceil(float64(count)*interval)) + 20
	payload := min(mtu-icmpOverheadBytes, maxIPv4ICMPPayload)
	args := []string{
		"-n", "-D",
		"-M", "do",
		"-s", strconv.Itoa(payload),
		"-c", strconv.Itoa(count),
		"-i", formatInterval(interval),
		"-W", "2",
		"-w", strconv.Itoa(wallSec),
		peer.String(),
	}
	return p.run(ctx, args, "ping-fullsize", interval, wallSec)
}

// run executes one ping invocation and parses it. wallSec is the -w
// wall-clock bound already present in args; the runner timeout backs it with
// pingTimeoutSlack so a ping that outlives its own deadline is killed instead
// of hanging the session.
func (p *PingTester) run(ctx context.Context, args []string, label string, interval float64, wallSec int) (model.PingResult, []string, error) {
	res, err := p.R.Run(ctx, teeRaw(runner.CommandSpec{
		Name:    "ping",
		Args:    args,
		Timeout: time.Duration(wallSec)*time.Second + pingTimeoutSlack,
		Label:   label,
	}, p.RawDir))
	if err != nil {
		return model.PingResult{}, nil, fmt.Errorf("testsuite: ping: %w", err)
	}
	parsed, err := parser.ParsePing(res.Stdout, res.Stderr, res.ExitCode)
	if err != nil {
		return parsed, nil, fmt.Errorf("testsuite: ping: %w", err)
	}
	parsed.IntervalUsedSec = interval
	return parsed, nil, nil
}

// formatInterval renders a ladder interval the way ping expects it ("0.02",
// "0.2", "1").
func formatInterval(interval float64) string {
	return strconv.FormatFloat(interval, 'f', -1, 64)
}
