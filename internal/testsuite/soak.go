package testsuite

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/config"
	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
)

var errSoakBudgetExpired = errors.New("soak duration expired")

// soakSteps are the display names of one soak cycle's steps, in order.
var soakSteps = []string{
	"cycle counter snapshot",
	"ping stability",
	"TCP throughput PC1 → PC2",
	"TCP throughput PC2 → PC1",
	"UDP loss and jitter",
}

// SoakPlanSteps returns the ordered display names of one soak cycle's steps;
// progress lines render them per cycle. The link-settings capture happens once
// before the first cycle and is not repeated per cycle.
func SoakPlanSteps() []string {
	out := make([]string, len(soakSteps))
	copy(out, soakSteps)
	return out
}

// SoakPlan repeats test cycles until SoakDuration of wall-clock time (measured
// via Clock, never time.Now) elapses. Each cycle takes a per-cycle counter
// snapshot, runs a stability ping, TCPRepeats one-way TCP passes per direction,
// and an optional UDP pass, then records the cycle's results and a snapshot of
// the link monitor's event timeline. The periodic load profile leaves an idle
// gap (CycleGap) between cycles; the continuous profile runs them back-to-back.
// On context cancellation mid-soak, every completed cycle is retained in the
// partial Results (nothing already recorded is discarded). Its Run method
// satisfies peer.PlanFunc and drives the worker via the same rc.Call RPCs the
// quick plan uses.
type SoakPlan struct {
	// Ops bundles the local test components (the same set the worker uses).
	Ops *Ops
	// Clock measures elapsed wall-clock time and drives the inter-cycle gap;
	// must be non-nil.
	Clock clock.Clock
	// LocalIP is PC1's test-link address.
	LocalIP netip.Addr
	// PeerIP is PC2's test-link address.
	PeerIP netip.Addr
	// IperfPort is the iperf3 data port.
	IperfPort uint16
	// TCPDuration is the per-direction TCP test duration per cycle.
	TCPDuration time.Duration
	// UDPDuration is the per-direction UDP test duration per cycle; 0 skips the
	// per-cycle UDP pass.
	UDPDuration time.Duration
	// UDPRate is the explicit --udp-rate override; 0 derives 80% of the
	// negotiated link speed at run time.
	UDPRate model.Bitrate
	// TCPRepeats is how many times each one-way TCP phase runs per cycle
	// (soak preset 1); values below 1 are treated as 1.
	TCPRepeats int
	// MTU is PC1's discovered interface MTU.
	MTU int
	// Streams is the iperf3 -P parallel stream count.
	Streams int
	// PingCount is the per-cycle ping packet count.
	PingCount int
	// LocalIperfCaps are PC1's detected iperf3 capabilities.
	LocalIperfCaps model.Iperf3Caps
	// SoakDuration is the total wall-clock budget for the whole soak.
	SoakDuration time.Duration
	// SoakLoad is the load profile: periodic (idle gaps) or continuous.
	SoakLoad config.SoakLoad
	// CycleGap is the periodic profile's idle gap between cycles; unused for
	// the continuous profile.
	CycleGap time.Duration
	// EventSource, when set, returns the link monitor's current event timeline;
	// the plan snapshots it after each cycle into Results.MonitoringEvents so
	// completed cycles retain their disconnect/renegotiation events. nil means
	// the app folds the monitor history in at report assembly instead.
	EventSource func() []model.MonitoringEvent
	// Results receives everything measured; must be non-nil.
	Results *SessionResults
	// OnStep, when set, announces each step as (step, total, name); the soak
	// plan prefixes cycle context onto the cycle's step names.
	OnStep func(step, total int, name string)
	// OnProgress, when set, observes progress reported by remote operations.
	OnProgress func(protocol.TestProgress)
}

// repeats returns the effective per-cycle TCP repeat count, floored at 1.
func (p *SoakPlan) repeats() int {
	if p.TCPRepeats < 1 {
		return 1
	}
	return p.TCPRepeats
}

// gap returns the inter-cycle wait for the configured load profile.
func (p *SoakPlan) gap() time.Duration {
	if p.SoakLoad == config.SoakLoadContinuous {
		return 0
	}
	if p.CycleGap > 0 {
		return p.CycleGap
	}
	return 0
}

// engine builds the QuickPlan the soak plan reuses to execute each cycle's
// steps against a cycle-local SessionResults accumulator.
func (p *SoakPlan) engine(results *SessionResults) *QuickPlan {
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
		Results:        results,
		OnProgress:     p.OnProgress,
	}
}

// Run executes the soak loop; it satisfies peer.PlanFunc. It captures link
// settings once, then repeats cycles until the wall-clock budget is spent or
// the context is cancelled. A cycle error (or cancellation) stops the loop with
// the error, but every cycle already completed stays in Results and the run is
// marked Incomplete so the partial report covers exactly what ran.
func (p *SoakPlan) Run(ctx context.Context, rc peer.RemoteCaller) (runErr error) {
	q := p.engine(p.Results)
	// Whole-run boundaries are independent of per-cycle snapshots. In
	// particular, the final capture must still run after the budget or caller
	// cancels an in-flight cycle, so use a cleanup context that retains values
	// without inheriting cancellation.
	defer func() {
		if err := q.snapshotCounters(context.WithoutCancel(ctx), rc, &p.Results.FinalCounters); err != nil {
			p.Results.Incomplete = true
			finalErr := fmt.Errorf("testsuite: soak final counters: %w", err)
			if runErr == nil {
				runErr = finalErr
			} else {
				runErr = errors.Join(runErr, finalErr)
			}
		}
	}()

	// Start the wall-clock budget before any link or counter setup so the
	// configured soak duration bounds the complete plan, not only its cycles.
	deadline := p.Clock.Now().Add(p.SoakDuration)
	budgetCtx, cancelBudget := context.WithCancelCause(ctx)
	stopBudget := make(chan struct{})
	budgetStopped := make(chan struct{})
	budgetTimer := p.Clock.After(p.SoakDuration)
	go func() {
		defer close(budgetStopped)
		select {
		case <-budgetTimer:
			cancelBudget(errSoakBudgetExpired)
		case <-ctx.Done():
			cancelBudget(ctx.Err())
		case <-stopBudget:
		}
	}()
	defer func() {
		close(stopBudget)
		<-budgetStopped
		cancelBudget(context.Canceled)
	}()

	// Link settings once, up front (the negotiated speed feeds the UDP rate
	// derivation and the report's link section).
	if err := q.stepLink(budgetCtx, rc); err != nil {
		p.Results.Incomplete = true
		return fmt.Errorf("testsuite: soak link settings: %w", err)
	}
	if err := q.snapshotCounters(budgetCtx, rc, &p.Results.InitialCounters); err != nil {
		p.Results.Incomplete = true
		return fmt.Errorf("testsuite: soak initial counters: %w", err)
	}

	for cycle := 1; ; cycle++ {
		if !p.Clock.Now().Before(deadline) {
			break
		}
		if err := ctx.Err(); err != nil {
			p.Results.Incomplete = true
			return fmt.Errorf("testsuite: soak interrupted after %d cycles: %w", p.Results.CyclesCompleted, err)
		}
		cycleResults, cycleCounters, finalCounters, err := p.runCycle(budgetCtx, rc, cycle)
		if err != nil {
			if errors.Is(context.Cause(budgetCtx), errSoakBudgetExpired) {
				break
			}
			p.Results.Incomplete = true
			return fmt.Errorf("testsuite: soak cycle %d: %w", cycle, err)
		}
		p.commitCycle(cycleResults, cycleCounters, finalCounters)
		p.Results.CyclesCompleted++
		p.recordEvents()

		// Stop before the inter-cycle gap if the budget is already spent, so a
		// finished soak does not sit idle for one final gap.
		if !p.Clock.Now().Before(deadline) {
			break
		}
		if p.gap() == 0 {
			continue
		}
		remaining := deadline.Sub(p.Clock.Now())
		if err := p.waitGap(budgetCtx, remaining); err != nil {
			if errors.Is(context.Cause(budgetCtx), errSoakBudgetExpired) {
				break
			}
			p.Results.Incomplete = true
			return fmt.Errorf("testsuite: soak interrupted after %d cycles: %w", p.Results.CyclesCompleted, err)
		}
	}
	return nil
}

// runCycle runs one soak cycle: a per-cycle counter snapshot, a stability
// ping, TCPRepeats one-way TCP passes per direction, and (when UDPDuration is
// set) a UDP pass. Measurements stay cycle-local until the post-load counter
// snapshot succeeds, so an interrupted cycle cannot leak into the report.
func (p *SoakPlan) runCycle(ctx context.Context, rc peer.RemoteCaller, cycle int) (*SessionResults, model.PeerCounters, model.PeerCounters, error) {
	cycleResults := &SessionResults{Link: p.Results.Link}
	q := p.engine(cycleResults)
	total := len(soakSteps)
	step := 0
	announce := func(name string) {
		step++
		announceStep(rc, p.OnStep, step, total, fmt.Sprintf("cycle %d: %s", cycle, name))
	}

	var cycleCounters model.PeerCounters
	announce(soakSteps[0])
	if err := q.snapshotCounters(ctx, rc, &cycleCounters); err != nil {
		return nil, model.PeerCounters{}, model.PeerCounters{}, err
	}

	announce(soakSteps[1])
	if err := q.stepPing(ctx, rc); err != nil {
		return nil, model.PeerCounters{}, model.PeerCounters{}, err
	}
	announce(soakSteps[2])
	for range p.repeats() {
		if err := q.stepTCPForward(ctx, rc); err != nil {
			return nil, model.PeerCounters{}, model.PeerCounters{}, err
		}
	}
	announce(soakSteps[3])
	for range p.repeats() {
		if err := q.stepTCPReverse(ctx, rc); err != nil {
			return nil, model.PeerCounters{}, model.PeerCounters{}, err
		}
	}
	if p.UDPDuration > 0 {
		announce(soakSteps[4])
		if err := q.stepUDP(ctx, rc); err != nil {
			return nil, model.PeerCounters{}, model.PeerCounters{}, err
		}
	}

	var finalCounters model.PeerCounters
	if err := q.snapshotCounters(ctx, rc, &finalCounters); err != nil {
		return nil, model.PeerCounters{}, model.PeerCounters{}, err
	}
	return cycleResults, cycleCounters, finalCounters, nil
}

// commitCycle atomically folds one fully completed cycle into the session.
func (p *SoakPlan) commitCycle(cycle *SessionResults, counters, finalCounters model.PeerCounters) {
	p.Results.Ping = append(p.Results.Ping, cycle.Ping...)
	p.Results.FullSizePing = append(p.Results.FullSizePing, cycle.FullSizePing...)
	p.Results.TCP = append(p.Results.TCP, cycle.TCP...)
	p.Results.UDP = append(p.Results.UDP, cycle.UDP...)
	p.Results.Notes = append(p.Results.Notes, cycle.Notes...)
	p.Results.SkippedTests = append(p.Results.SkippedTests, cycle.SkippedTests...)
	p.Results.UDPRateAssumed = p.Results.UDPRateAssumed || cycle.UDPRateAssumed
	p.Results.UDPNearSaturation = p.Results.UDPNearSaturation || cycle.UDPNearSaturation
	p.Results.CycleCounters = append(p.Results.CycleCounters, counters)
}

// recordEvents snapshots the link monitor's current event timeline into the
// results, so a mid-soak disconnect or renegotiation is retained even if the
// run is later interrupted. It replaces the slice each cycle (the source is
// the monitor's full, growing history), never appends duplicates.
func (p *SoakPlan) recordEvents() {
	if p.EventSource == nil {
		return
	}
	events := p.EventSource()
	if len(events) == 0 {
		return
	}
	p.Results.MonitoringEvents = append(p.Results.MonitoringEvents[:0:0], events...)
}

// waitGap waits the inter-cycle gap on the clock, returning ctx.Err() if the
// context is cancelled during the wait.
func (p *SoakPlan) waitGap(ctx context.Context, remaining time.Duration) error {
	gap := min(p.gap(), remaining)
	if gap <= 0 {
		return nil
	}
	select {
	case <-p.Clock.After(gap):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
