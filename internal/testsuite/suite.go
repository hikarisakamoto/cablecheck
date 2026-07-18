package testsuite

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/peer"
)

// SessionResults keys for per-side data.
const (
	// RolePC1Key indexes PC1's (the coordinator's) side.
	RolePC1Key = "pc1"
	// RolePC2Key indexes PC2's (the worker's) side.
	RolePC2Key = "pc2"
)

// opCallTimeout is the request timeout for quick ops (counters, link
// settings, iperf3 server start/stop): expected duration plus generous slack.
const opCallTimeout = 30 * time.Second

// SessionResults accumulates everything the plan driver measures, locally
// and via the remote worker. Direction-sensitive results carry explicit
// model.DirectionPC1ToPC2 / DirectionPC2ToPC1 keys; per-side data is keyed
// RolePC1Key / RolePC2Key.
type SessionResults struct {
	// Link holds each side's link settings and observations.
	Link map[string]*LinkSettingsResult
	// InitialCounters holds both sides' counter snapshots before testing.
	InitialCounters model.PeerCounters
	// FinalCounters holds both sides' counter snapshots after testing.
	FinalCounters model.PeerCounters
	// Ping holds the ping results, one per direction.
	Ping []model.PingResult
	// TCP holds the TCP throughput results, one per direction. An aborted
	// run's partial result is preserved here too, with Incomplete set on
	// the session results.
	TCP []model.TCPResult
	// Notes records limitations hit during the run (denied ping interval,
	// unavailable remote tools, ...).
	Notes []string
	// Incomplete marks a run that did not finish all steps.
	Incomplete bool
}

// quickSteps are the display names of the TCP-only v1 quick plan, in order.
var quickSteps = []string{
	"link settings",
	"initial counter snapshot",
	"ping stability",
	"TCP throughput PC1 → PC2",
	"TCP throughput PC2 → PC1",
	"final counter snapshot",
}

// QuickPlanSteps returns the ordered display names of the quick plan's
// steps; progress lines render them as "[n/N] name".
func QuickPlanSteps() []string {
	out := make([]string, len(quickSteps))
	copy(out, quickSteps)
	return out
}

// QuickPlan drives the TCP-only v1 quick test sequence on PC1: it executes
// the local half of every step directly through Ops' components and the
// remote half via the peer RemoteCaller, accumulating into Results. Its Run
// method is the peer.PlanFunc the session executes.
type QuickPlan struct {
	// Ops bundles the local test components (the same set the worker uses).
	Ops *Ops
	// LocalIP is PC1's test-link address.
	LocalIP netip.Addr
	// PeerIP is PC2's test-link address.
	PeerIP netip.Addr
	// IperfPort is the iperf3 data port.
	IperfPort uint16
	// TCPDuration is the per-direction TCP test duration.
	TCPDuration time.Duration
	// Streams is the iperf3 -P parallel stream count.
	Streams int
	// PingCount is the quick-ping packet count.
	PingCount int
	// Results receives everything measured; must be non-nil.
	Results *SessionResults
	// OnStep, when set, announces each step as (step, total, name).
	OnStep func(step, total int, name string)
}

// Run executes the quick plan; it satisfies peer.PlanFunc. On error the
// accumulated Results are preserved and marked Incomplete.
func (q *QuickPlan) Run(ctx context.Context, rc peer.RemoteCaller) error {
	steps := []func(context.Context, peer.RemoteCaller) error{
		q.stepLink,
		q.stepInitialCounters,
		q.stepPing,
		q.stepTCPForward,
		q.stepTCPReverse,
		q.stepFinalCounters,
	}
	for i, step := range steps {
		if q.OnStep != nil {
			q.OnStep(i+1, len(quickSteps), quickSteps[i])
		}
		if err := step(ctx, rc); err != nil {
			q.Results.Incomplete = true
			return fmt.Errorf("testsuite: step %d (%s): %w", i+1, quickSteps[i], err)
		}
	}
	return nil
}

// callRemote performs one RPC and decodes its typed payload. A status of
// "unavailable" is not fatal: it is recorded as a limitation note and
// returns (nil, nil) so the caller records the gap. Every other non-ok
// status fails the step, but when the failure result still carries a
// decodable payload — HandleOp forwards the partial result of an aborted
// iperf3 client run this way — the decoded payload is returned alongside
// the error so the caller can preserve the partial data.
func (q *QuickPlan) callRemote(ctx context.Context, rc peer.RemoteCaller, op string,
	params any, timeout time.Duration) (any, error) {
	res, err := rc.Call(ctx, op, params, timeout, nil)
	if err != nil {
		return nil, fmt.Errorf("remote %s: %w", op, err)
	}
	switch res.Status {
	case StatusOK:
		out, err := DecodeOpResult(op, res.Result)
		if err != nil {
			return nil, fmt.Errorf("remote %s: %w", op, err)
		}
		return out, nil
	case StatusUnavailable:
		q.Results.Notes = append(q.Results.Notes,
			fmt.Sprintf("peer could not run %s: %s", op, res.Error))
		return nil, nil
	default:
		var partial any
		if len(res.Result) > 0 {
			if out, decErr := DecodeOpResult(op, res.Result); decErr == nil {
				partial = out
			}
		}
		return partial, fmt.Errorf("remote %s: status %s: %s", op, res.Status, res.Error)
	}
}

// stepLink captures both sides' link settings and observations.
func (q *QuickPlan) stepLink(ctx context.Context, rc peer.RemoteCaller) error {
	if q.Results.Link == nil {
		q.Results.Link = make(map[string]*LinkSettingsResult)
	}
	settings, obs, err := q.Ops.Link.Inspect(ctx)
	if err != nil {
		return err
	}
	q.Results.Link[RolePC1Key] = &LinkSettingsResult{Settings: settings, Observations: obs}
	out, err := q.callRemote(ctx, rc, OpLinkSettings, nil, opCallTimeout)
	if err != nil {
		return err
	}
	if lr, ok := out.(*LinkSettingsResult); ok {
		q.Results.Link[RolePC2Key] = lr
	}
	return nil
}

// stepInitialCounters snapshots both sides' counters before any load.
func (q *QuickPlan) stepInitialCounters(ctx context.Context, rc peer.RemoteCaller) error {
	return q.snapshotCounters(ctx, rc, &q.Results.InitialCounters)
}

// stepFinalCounters snapshots both sides' counters after all tests.
func (q *QuickPlan) stepFinalCounters(ctx context.Context, rc peer.RemoteCaller) error {
	return q.snapshotCounters(ctx, rc, &q.Results.FinalCounters)
}

// snapshotCounters fills one PeerCounters pair, local side first so both
// captures share the same plan boundary.
func (q *QuickPlan) snapshotCounters(ctx context.Context, rc peer.RemoteCaller, into *model.PeerCounters) error {
	snap, err := q.Ops.Counters.Snapshot(ctx)
	if err != nil {
		return err
	}
	into.PC1 = &snap
	out, err := q.callRemote(ctx, rc, OpCountersSnapshot, nil, opCallTimeout)
	if err != nil {
		return err
	}
	if cs, ok := out.(*model.CounterSnapshot); ok {
		into.PC2 = cs
	}
	return nil
}

// stepPing runs the quick stability ping in both directions: locally toward
// PC2, and on the worker toward PC1.
func (q *QuickPlan) stepPing(ctx context.Context, rc peer.RemoteCaller) error {
	local, notes, err := q.Ops.Ping.Quick(ctx, q.PeerIP, q.PingCount)
	if err != nil {
		return err
	}
	local.Direction = model.DirectionPC1ToPC2
	q.Results.Ping = append(q.Results.Ping, local)
	q.Results.Notes = append(q.Results.Notes, notes...)

	out, err := q.callRemote(ctx, rc, OpPingRun,
		PingRunParams{PeerIP: q.LocalIP.String(), Count: q.PingCount}, q.pingTimeout())
	if err != nil {
		return err
	}
	if pr, ok := out.(*PingRunResult); ok {
		pr.Ping.Direction = model.DirectionPC2ToPC1
		q.Results.Ping = append(q.Results.Ping, pr.Ping)
		q.Results.Notes = append(q.Results.Notes, pr.Notes...)
	}
	return nil
}

// pingTimeout bounds the remote ping op: the worst ladder rung is -i 1.0
// bounded by -w, plus slack for the earlier denied rungs and the RPC itself.
func (q *QuickPlan) pingTimeout() time.Duration {
	return time.Duration(q.PingCount)*1250*time.Millisecond + 30*time.Second
}

// tcpTimeout bounds the remote iperf3 client op: -t plus the client's own
// runner slack plus RPC grace.
func (q *QuickPlan) tcpTimeout() time.Duration {
	return q.TCPDuration + clientTimeoutSlack + 20*time.Second
}

// stepTCPForward measures PC1 → PC2: the worker hosts the one-off server
// (server always on the receiving side), the local client sends. A failed or
// aborted client run still contributes its partial result to Results (marked
// via SessionResults.Incomplete) before the step reports the error.
func (q *QuickPlan) stepTCPForward(ctx context.Context, rc peer.RemoteCaller) error {
	out, err := q.callRemote(ctx, rc, OpIperfServerStart,
		IperfServerStartParams{BindIP: q.PeerIP.String(), Port: q.IperfPort}, opCallTimeout)
	if err != nil {
		return err
	}
	if out == nil {
		q.Results.Notes = append(q.Results.Notes,
			"TCP throughput PC1 → PC2 skipped: peer iperf3 unavailable")
		return nil
	}
	res, runErr := q.Ops.Iperf.RunTCPClient(ctx, q.LocalIP, q.PeerIP, q.IperfPort,
		q.TCPDuration, q.Streams, false)
	q.recordTCP(res, model.DirectionPC1ToPC2)
	// Stop the remote server even after a failed client run, so no one-off
	// server lingers waiting for a client that will never come.
	if _, stopErr := q.callRemote(ctx, rc, OpIperfServerStop,
		IperfServerStopParams{Port: q.IperfPort}, opCallTimeout); runErr == nil && stopErr != nil {
		return stopErr
	}
	return runErr
}

// recordTCP appends one TCP run outcome (complete or partial) to Results
// under the given direction; a partial run additionally marks the session
// results incomplete. A nil res records nothing.
func (q *QuickPlan) recordTCP(res *TCPRunResult, direction string) {
	if res == nil {
		return
	}
	res.TCP.Direction = direction
	q.Results.TCP = append(q.Results.TCP, res.TCP)
	if res.Incomplete {
		q.Results.Incomplete = true
	}
}

// stepTCPReverse measures PC2 → PC1: the local side hosts the one-off
// server, the worker's client sends toward PC1. When the remote run fails
// but its failed status still carries a partial payload, that partial is
// recorded before the step reports the error.
func (q *QuickPlan) stepTCPReverse(ctx context.Context, rc peer.RemoteCaller) error {
	h, err := q.Ops.Iperf.StartServer(ctx, q.LocalIP, q.IperfPort)
	if err != nil {
		return err
	}
	defer func() { _ = h.Stop(ctx) }()
	if err := h.Ready(ctx); err != nil {
		return err
	}
	out, err := q.callRemote(ctx, rc, OpIperfClientRun, IperfClientRunParams{
		LocalIP:     q.PeerIP.String(),
		PeerIP:      q.LocalIP.String(),
		Port:        q.IperfPort,
		DurationSec: int(q.TCPDuration / time.Second),
		Streams:     q.Streams,
	}, q.tcpTimeout())
	if tr, ok := out.(*TCPRunResult); ok {
		q.recordTCP(tr, model.DirectionPC2ToPC1)
	}
	return err
}
