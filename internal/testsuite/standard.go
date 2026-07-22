package testsuite

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
)

// StandardPlan drives the standard test sequence on PC1. It is the quick plan
// with more depth: longer pings, each one-way TCP phase repeated TCPRepeats
// times, an extra reduced-rate (~50%) UDP pass in addition to the default-rate
// one, the bidirectional stress test, and initial/final counter snapshots
// bracketing everything. The sysfs link monitor runs for the whole phase (the
// app owns it) exactly as in quick mode. Its Run method satisfies
// peer.PlanFunc, and it drives the worker through the same rc.Call RPCs the
// quick plan uses.
type StandardPlan struct {
	// Ops bundles the local test components (the same set the worker uses).
	Ops *Ops
	// LocalIP is PC1's test-link address.
	LocalIP netip.Addr
	// PeerIP is PC2's test-link address.
	PeerIP netip.Addr
	// IperfPort is the iperf3 data port; the two-phase bidir fallback also
	// uses IperfPort+1.
	IperfPort uint16
	// TCPDuration is the per-direction TCP test duration (standard preset 60s).
	TCPDuration time.Duration
	// UDPDuration is the per-direction UDP test duration.
	UDPDuration time.Duration
	// UDPRate is the explicit --udp-rate override; 0 derives 80% of the
	// negotiated link speed at run time.
	UDPRate model.Bitrate
	// TCPRepeats is how many times each one-way TCP phase runs (standard
	// preset 2); values below 1 are treated as 1.
	TCPRepeats int
	// MTU is PC1's discovered interface MTU.
	MTU int
	// Streams is the iperf3 -P parallel stream count.
	Streams int
	// PingCount is the standard-ping packet count.
	PingCount int
	// LocalIperfCaps are PC1's detected iperf3 capabilities.
	LocalIperfCaps model.Iperf3Caps
	// Results receives everything measured; must be non-nil.
	Results *SessionResults
	// OnStep, when set, announces each step as (step, total, name).
	OnStep func(step, total int, name string)
	// OnProgress, when set, observes progress reported by remote operations.
	OnProgress func(protocol.TestProgress)
}

// standardExtraUDPRateFraction is the numerator (over 100) of the negotiated
// UDP rate used for the extra reduced-rate UDP pass: 50%.
const standardExtraUDPRateFraction = 50

// planSteps returns this plan instance's ordered step display names, expanded
// for its TCPRepeats. The TCP repeats and the extra reduced-rate UDP pass are
// named explicitly so the operator sees the real workload the mode performs.
func (p *StandardPlan) planSteps() []string {
	repeats := p.repeats()
	steps := []string{
		"link settings",
		"initial counter snapshot",
		"ping stability",
		"full-size ping",
	}
	for i := 1; i <= repeats; i++ {
		steps = append(steps, fmt.Sprintf("TCP throughput PC1 → PC2 (repeat %d/%d)", i, repeats))
	}
	for i := 1; i <= repeats; i++ {
		steps = append(steps, fmt.Sprintf("TCP throughput PC2 → PC1 (repeat %d/%d)", i, repeats))
	}
	steps = append(steps,
		"bidirectional stress",
		"UDP loss and jitter",
		"reduced-rate UDP loss and jitter",
		"final counter snapshot",
	)
	return steps
}

// StandardPlanSteps returns the ordered display names of the standard plan's
// steps for the default TCPRepeats(=2) preset; the app builds its progress
// step list from the same source the plan announces.
func StandardPlanSteps() []string {
	p := &StandardPlan{TCPRepeats: 2}
	return p.planSteps()
}

// repeats returns the effective TCP repeat count, floored at 1.
func (p *StandardPlan) repeats() int {
	if p.TCPRepeats < 1 {
		return 1
	}
	return p.TCPRepeats
}

// engine builds the QuickPlan the standard plan reuses to execute each step.
// The two share the same SessionResults accumulator and topology; the standard
// plan only orchestrates extra repeats and the reduced-rate UDP pass on top.
func (p *StandardPlan) engine() *QuickPlan {
	return &QuickPlan{
		Ops:            p.Ops,
		LocalIP:        p.LocalIP,
		PeerIP:         p.PeerIP,
		IperfPort:      p.IperfPort,
		TCPDuration:    p.TCPDuration,
		UDPDuration:    p.UDPDuration,
		UDPRate:        p.UDPRate,
		MTU:            p.MTU,
		Streams:        p.Streams,
		PingCount:      p.PingCount,
		LocalIperfCaps: p.LocalIperfCaps,
		Results:        p.Results,
		OnProgress:     p.OnProgress,
	}
}

// Run executes the standard plan; it satisfies peer.PlanFunc. On error the
// accumulated Results are preserved and marked Incomplete, exactly as the
// quick plan does — partial per-repeat and per-direction results survive.
func (p *StandardPlan) Run(ctx context.Context, rc peer.RemoteCaller) error {
	q := p.engine()
	repeats := p.repeats()
	steps := []func(context.Context, peer.RemoteCaller) error{
		q.stepLink,
		q.stepInitialCounters,
		q.stepPing,
		q.stepFullSizePing,
	}
	for range repeats {
		steps = append(steps, q.stepTCPForward)
	}
	for range repeats {
		steps = append(steps, q.stepTCPReverse)
	}
	// Derive the default UDP rate once (recording its notes/flags a single
	// time), then run the default-rate pass and the reduced-rate pass off it.
	var defaultRate model.Bitrate
	steps = append(steps,
		q.stepBidir,
		func(ctx context.Context, rc peer.RemoteCaller) error {
			defaultRate = q.udpRate()
			return q.stepUDPAtRate(ctx, rc, defaultRate)
		},
		func(ctx context.Context, rc peer.RemoteCaller) error {
			return q.stepUDPAtRate(ctx, rc, p.reducedRate(defaultRate))
		},
		q.stepFinalCounters,
	)

	names := p.planSteps()
	for i, step := range steps {
		announceStep(rc, p.OnStep, i+1, len(names), names[i])
		if err := step(ctx, rc); err != nil {
			p.Results.Incomplete = true
			return fmt.Errorf("testsuite: standard step %d (%s): %w", i+1, names[i], err)
		}
	}
	return nil
}

// reducedRate is the extra UDP pass's target: half the actual primary rate.
// It deliberately has no independent floor: a reduced diagnostic pass must
// never run faster than the primary pass it is meant to contrast with.
func (p *StandardPlan) reducedRate(defaultRate model.Bitrate) model.Bitrate {
	return defaultRate * standardExtraUDPRateFraction / 100
}
