package testsuite

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"cablecheck/internal/config"
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
	// FullSizePing holds the full-MTU non-fragmenting ping results, one per
	// direction.
	FullSizePing []model.PingResult
	// TCP holds the TCP throughput results, one per direction. An aborted
	// run's partial result is preserved here too, with Incomplete set on
	// the session results.
	TCP []model.TCPResult
	// Bidir holds the bidirectional stress result — a native --bidir run or
	// the coordinated two-phase fallback.
	Bidir *model.BidirResult
	// UDP holds the UDP loss/jitter results, one per direction.
	UDP []model.UDPResult
	// UDPRateAssumed reports that the UDP target rate was assumed (100M
	// fallback) because the negotiated link speed was unknown; the
	// evaluator soft-pedals UDP findings on it.
	UDPRateAssumed bool
	// UDPNearSaturation reports that the UDP target rate exceeded 95% of
	// the negotiated link speed (an explicit --udp-rate override): loss at
	// self-inflicted saturation must not count against the cable.
	UDPNearSaturation bool
	// Notes records limitations hit during the run (denied ping interval,
	// unavailable remote tools, ...).
	Notes []string
	// SkippedTests records planned measurements that could not run because a
	// required tool became unavailable at runtime.
	SkippedTests []model.SkippedTest
	// Incomplete marks a run that did not finish all steps.
	Incomplete bool
}

// quickSteps are the display names of the quick plan's steps, in order. The
// counters bracket every traffic step; link settings come first so the UDP
// rate derivation later in the plan can use the negotiated speed.
var quickSteps = []string{
	"link settings",
	"initial counter snapshot",
	"ping stability",
	"full-size ping",
	"TCP throughput PC1 → PC2",
	"TCP throughput PC2 → PC1",
	"bidirectional stress",
	"UDP loss and jitter",
	"final counter snapshot",
}

// fullSizePingCount is the fixed probe count of the quick-mode full-size
// ping (design tools.md §3: -c 100 -i 0.2).
const fullSizePingCount = 100

// QuickPlanSteps returns the ordered display names of the quick plan's
// steps; progress lines render them as "[n/N] name".
func QuickPlanSteps() []string {
	out := make([]string, len(quickSteps))
	copy(out, quickSteps)
	return out
}

// QuickPlan drives the quick test sequence on PC1: it executes the local
// half of every step directly through Ops' components and the remote half
// via the peer RemoteCaller, accumulating into Results. Its Run method is
// the peer.PlanFunc the session executes.
type QuickPlan struct {
	// Ops bundles the local test components (the same set the worker uses).
	Ops *Ops
	// LocalIP is PC1's test-link address.
	LocalIP netip.Addr
	// PeerIP is PC2's test-link address.
	PeerIP netip.Addr
	// IperfPort is the iperf3 data port; the two-phase bidir fallback also
	// uses IperfPort+1 (preflight probes both).
	IperfPort uint16
	// TCPDuration is the per-direction TCP test duration.
	TCPDuration time.Duration
	// UDPDuration is the per-direction UDP test duration.
	UDPDuration time.Duration
	// UDPRate is the explicit --udp-rate override; 0 derives 80% of the
	// negotiated link speed at run time (config.DefaultUDPRate).
	UDPRate model.Bitrate
	// MTU is PC1's discovered interface MTU: the full-size ping payload and
	// the UDP datagram size are MTU-28, computed, never hardcoded.
	MTU int
	// Streams is the iperf3 -P parallel stream count.
	Streams int
	// PingCount is the quick-ping packet count.
	PingCount int
	// LocalIperfCaps are PC1's detected iperf3 capabilities; the effective
	// set is the AND with the worker's OpIperfCaps answer.
	LocalIperfCaps model.Iperf3Caps
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
		q.stepFullSizePing,
		q.stepTCPForward,
		q.stepTCPReverse,
		q.stepBidir,
		q.stepUDP,
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
	return q.callRemoteForTest(ctx, rc, op, params, timeout, "")
}

// callRemoteForTest performs callRemote and associates an unavailable RPC
// with the planned test whose measurement that RPC was serving.
func (q *QuickPlan) callRemoteForTest(ctx context.Context, rc peer.RemoteCaller, op string,
	params any, timeout time.Duration, testName string) (any, error) {
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
		reason := fmt.Sprintf("peer could not run %s: %s", op, res.Error)
		q.Results.Notes = append(q.Results.Notes, reason)
		q.recordSkippedTest(testName, reason)
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

// recordSkippedTest records one semantic test name, preserving the first
// runtime reason when multiple RPCs belonging to that test are unavailable.
func (q *QuickPlan) recordSkippedTest(name, reason string) {
	if name == "" {
		return
	}
	for _, skipped := range q.Results.SkippedTests {
		if skipped.Name == name {
			return
		}
	}
	q.Results.SkippedTests = append(q.Results.SkippedTests, model.SkippedTest{Name: name, Reason: reason})
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
	out, err := q.callRemoteForTest(ctx, rc, OpLinkSettings, nil, opCallTimeout, "link_settings")
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
	out, err := q.callRemoteForTest(ctx, rc, OpCountersSnapshot, nil, opCallTimeout, "counters")
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

	out, err := q.callRemoteForTest(ctx, rc, OpPingRun,
		PingRunParams{PeerIP: q.LocalIP.String(), Count: q.PingCount}, q.pingTimeout(), "ping")
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

// stepFullSizePing runs the full-size non-fragmenting ping in both
// directions. Each side computes its own MTU-28 payload from its own
// discovered MTU: PC1 from q.MTU here, the worker from its Ops.MTU inside
// OpPingFullSize — the two interfaces may be configured differently.
func (q *QuickPlan) stepFullSizePing(ctx context.Context, rc peer.RemoteCaller) error {
	local, notes, err := q.Ops.Ping.FullSize(ctx, q.PeerIP, q.MTU, fullSizePingCount)
	if err != nil {
		return err
	}
	local.Direction = model.DirectionPC1ToPC2
	q.Results.FullSizePing = append(q.Results.FullSizePing, local)
	q.Results.Notes = append(q.Results.Notes, notes...)

	out, err := q.callRemoteForTest(ctx, rc, OpPingFullSize,
		PingFullSizeParams{PeerIP: q.LocalIP.String(), Count: fullSizePingCount}, q.fullSizeTimeout(), "full_size_ping")
	if err != nil {
		return err
	}
	if pr, ok := out.(*PingRunResult); ok {
		pr.Ping.Direction = model.DirectionPC2ToPC1
		q.Results.FullSizePing = append(q.Results.FullSizePing, pr.Ping)
		q.Results.Notes = append(q.Results.Notes, pr.Notes...)
	}
	return nil
}

// pingTimeout bounds the remote ping op: the worst ladder rung is -i 1.0
// bounded by -w, plus slack for the earlier denied rungs and the RPC itself.
func (q *QuickPlan) pingTimeout() time.Duration {
	return time.Duration(q.PingCount)*1250*time.Millisecond + 30*time.Second
}

// fullSizeTimeout bounds the remote full-size ping op: the fixed 0.2s
// interval bounded by -w plus the runner backstop and RPC grace.
func (q *QuickPlan) fullSizeTimeout() time.Duration {
	return time.Duration(fullSizePingCount)*250*time.Millisecond + 60*time.Second
}

// tcpTimeout bounds the remote iperf3 client op: -t plus the client's own
// runner slack plus RPC grace.
func (q *QuickPlan) tcpTimeout() time.Duration {
	return q.TCPDuration + clientTimeoutSlack + 20*time.Second
}

// withRemoteServer runs one client phase against a one-off server on the
// worker, preserving unavailable-tool skips and always stopping a server that
// was started. The bool reports whether the client phase ran.
func (q *QuickPlan) withRemoteServer(ctx context.Context, rc peer.RemoteCaller, stage, skipNote string,
	runClient func() error) (bool, error) {
	out, err := q.callRemoteForTest(ctx, rc, OpIperfServerStart,
		IperfServerStartParams{BindIP: q.PeerIP.String(), Port: q.IperfPort}, opCallTimeout, stage)
	if err != nil {
		return false, err
	}
	if out == nil {
		q.Results.Notes = append(q.Results.Notes, skipNote)
		return false, nil
	}
	runErr := runClient()
	_, stopErr := q.callRemote(ctx, rc, OpIperfServerStop,
		IperfServerStopParams{Port: q.IperfPort}, opCallTimeout)
	if runErr == nil && stopErr != nil {
		return true, stopErr
	}
	return true, runErr
}

// stepTCPForward measures PC1 → PC2: the worker hosts the one-off server
// (server always on the receiving side), the local client sends. A failed or
// aborted client run still contributes its partial result to Results (marked
// via SessionResults.Incomplete) before the step reports the error.
func (q *QuickPlan) stepTCPForward(ctx context.Context, rc peer.RemoteCaller) error {
	_, err := q.withRemoteServer(ctx, rc, "tcp",
		"TCP throughput PC1 → PC2 skipped: peer iperf3 unavailable", func() error {
			res, runErr := q.Ops.Iperf.RunTCPClient(ctx, q.LocalIP, q.PeerIP, q.IperfPort,
				q.TCPDuration, q.Streams, false)
			q.recordTCP(res, model.DirectionPC1ToPC2)
			return runErr
		})
	return err
}

// recordTCP appends one TCP run outcome (complete or partial) to Results
// under the given direction; a partial run additionally marks the session
// results incomplete. A nil res records nothing.
func (q *QuickPlan) recordTCP(res *TCPRunResult, direction string) {
	if res == nil {
		return
	}
	res.TCP.Direction = direction
	res.TCP.Incomplete = res.Incomplete
	q.Results.TCP = append(q.Results.TCP, res.TCP)
	if res.Incomplete {
		q.Results.Incomplete = true
	}
}

// stepBidir runs the bidirectional stress test. It first asks the worker for
// its iperf3 capabilities: with --bidir in the effective set (local AND peer)
// it runs one native --bidir client on PC1 against a single one-off server on
// PC2; otherwise it degrades to two coordinated one-way phases on IperfPort
// and IperfPort+1 — recorded as a limitation note, never a failure.
func (q *QuickPlan) stepBidir(ctx context.Context, rc peer.RemoteCaller) error {
	out, err := q.callRemote(ctx, rc, OpIperfCaps, nil, opCallTimeout)
	if err != nil {
		return err
	}
	peerCaps, _ := out.(*model.Iperf3Caps)
	if q.LocalIperfCaps.Bidir && peerCaps != nil && peerCaps.Bidir {
		return q.bidirNative(ctx, rc)
	}
	note := fmt.Sprintf("iperf3 --bidir is unavailable on at least one peer; "+
		"the bidirectional stress test ran as two coordinated one-way phases "+
		"on ports %d and %d instead of a single simultaneous run",
		q.IperfPort, q.IperfPort+1)
	q.Results.Notes = append(q.Results.Notes, note)
	return q.bidirFallback(ctx, rc, note)
}

// bidirNative runs the single-server --bidir path: server on PC2 (the design
// exception — bidir is the one phase whose server side is fixed), client with
// --bidir on PC1. A failed or aborted client still contributes its partial
// result before the step reports the error.
func (q *QuickPlan) bidirNative(ctx context.Context, rc peer.RemoteCaller) error {
	_, err := q.withRemoteServer(ctx, rc, "bidirectional",
		"bidirectional stress skipped: peer iperf3 unavailable", func() error {
			res, runErr := q.Ops.Iperf.RunBidirClient(ctx, q.LocalIP, q.PeerIP, q.IperfPort,
				q.TCPDuration, q.Streams)
			if res != nil {
				bidir := res.Bidir
				q.Results.Bidir = &bidir
				if res.Incomplete {
					q.Results.Incomplete = true
				}
			}
			return runErr
		})
	return err
}

// bidirFallback runs the two-phase bidirectional fallback: both one-way
// phases run SIMULTANEOUSLY (that is the stress), each on its own port —
// PC1→PC2 toward the worker's server on IperfPort, PC2→PC1 toward a local
// server on IperfPort+1 (both ports were preflight-probed). The local client
// runs in its own goroutine while the remote client RPC blocks this one.
func (q *QuickPlan) bidirFallback(ctx context.Context, rc peer.RemoteCaller, note string) error {
	out, err := q.callRemoteForTest(ctx, rc, OpIperfServerStart,
		IperfServerStartParams{BindIP: q.PeerIP.String(), Port: q.IperfPort}, opCallTimeout, "bidirectional")
	if err != nil {
		return err
	}
	if out == nil {
		q.Results.Notes = append(q.Results.Notes,
			"bidirectional stress skipped: peer iperf3 unavailable")
		return nil
	}
	remoteStopped := false
	defer func() {
		if !remoteStopped {
			_, _ = q.callRemote(ctx, rc, OpIperfServerStop,
				IperfServerStopParams{Port: q.IperfPort}, opCallTimeout)
		}
	}()
	h, err := q.Ops.Iperf.StartServer(ctx, q.LocalIP, q.IperfPort+1)
	if err != nil {
		return err
	}
	defer func() { _ = h.Stop(ctx) }()
	if err := h.Ready(ctx); err != nil {
		return err
	}

	var localRes *TCPRunResult
	var localErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		localRes, localErr = q.Ops.Iperf.RunTCPClient(ctx, q.LocalIP, q.PeerIP, q.IperfPort,
			q.TCPDuration, q.Streams, false)
	}()
	remoteOut, remoteErr := q.callRemoteForTest(ctx, rc, OpIperfClientRun, IperfClientRunParams{
		LocalIP:     q.PeerIP.String(),
		PeerIP:      q.LocalIP.String(),
		Port:        q.IperfPort + 1,
		DurationSec: int(q.TCPDuration / time.Second),
		Streams:     q.Streams,
	}, q.tcpTimeout(), "bidirectional")
	<-done

	b := &model.BidirResult{
		Duration:         model.Duration(q.TCPDuration),
		ParallelStreams:  q.Streams,
		TwoPhaseFallback: true,
		Warnings:         []string{note},
	}
	if localRes != nil {
		b.PC1ToPC2 = bidirDirFromTCP(localRes.TCP)
		b.CPUUtilization = localRes.TCP.CPUUtilization
		if localRes.Incomplete {
			q.Results.Incomplete = true
		}
	}
	if tr, ok := remoteOut.(*TCPRunResult); ok {
		b.PC2ToPC1 = bidirDirFromTCP(tr.TCP)
		if tr.Incomplete {
			q.Results.Incomplete = true
		}
	}
	q.Results.Bidir = b

	_, stopErr := q.callRemote(ctx, rc, OpIperfServerStop,
		IperfServerStopParams{Port: q.IperfPort}, opCallTimeout)
	remoteStopped = true
	if stopErr != nil &&
		localErr == nil && remoteErr == nil {
		return stopErr
	}
	if localErr != nil {
		return localErr
	}
	return remoteErr
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
	out, err := q.callRemoteForTest(ctx, rc, OpIperfClientRun, IperfClientRunParams{
		LocalIP:     q.PeerIP.String(),
		PeerIP:      q.LocalIP.String(),
		Port:        q.IperfPort,
		DurationSec: int(q.TCPDuration / time.Second),
		Streams:     q.Streams,
	}, q.tcpTimeout(), "tcp")
	if tr, ok := out.(*TCPRunResult); ok {
		q.recordTCP(tr, model.DirectionPC2ToPC1)
	}
	return err
}

// stepUDP measures UDP loss and jitter in both directions at one shared
// target rate derived from the negotiated link speed (or the explicit
// --udp-rate override). Each phase's server sits on the receiving side; each
// client computes its -l datagram size from its own MTU.
func (q *QuickPlan) stepUDP(ctx context.Context, rc peer.RemoteCaller) error {
	rate := q.udpRate()

	// Forward: server on PC2, local client sends.
	ran, err := q.withRemoteServer(ctx, rc, "udp",
		"UDP loss/jitter skipped: peer iperf3 unavailable", func() error {
			res, runErr := q.Ops.Iperf.RunUDPClient(ctx, q.LocalIP, q.PeerIP, q.IperfPort,
				q.UDPDuration, uint64(rate), UDPPayloadForMTU(q.MTU))
			q.recordUDP(res, model.DirectionPC1ToPC2)
			return runErr
		})
	if err != nil {
		return err
	}
	if !ran {
		return nil
	}

	// Reverse: local server, the worker's client sends toward PC1.
	h, err := q.Ops.Iperf.StartServer(ctx, q.LocalIP, q.IperfPort)
	if err != nil {
		return err
	}
	defer func() { _ = h.Stop(ctx) }()
	if err := h.Ready(ctx); err != nil {
		return err
	}
	out, err := q.callRemoteForTest(ctx, rc, OpIperfUDPRun, UDPRunParams{
		LocalIP:     q.PeerIP.String(),
		PeerIP:      q.LocalIP.String(),
		Port:        q.IperfPort,
		DurationSec: int(q.UDPDuration / time.Second),
		RateBps:     uint64(rate),
	}, q.udpTimeout(), "udp")
	if ur, ok := out.(*UDPRunResult); ok {
		q.recordUDP(ur, model.DirectionPC2ToPC1)
	}
	return err
}

// recordUDP appends one UDP run outcome (complete or partial) to Results
// under the given direction; a partial run additionally marks the session
// results incomplete. A nil res records nothing.
func (q *QuickPlan) recordUDP(res *UDPRunResult, direction string) {
	if res == nil {
		return
	}
	res.UDP.Direction = direction
	q.Results.UDP = append(q.Results.UDP, res.UDP)
	if res.Incomplete {
		q.Results.Incomplete = true
	}
}

// udpTimeout bounds the remote UDP client op: -t plus the client's own
// runner slack plus RPC grace.
func (q *QuickPlan) udpTimeout() time.Duration {
	return q.UDPDuration + clientTimeoutSlack + 20*time.Second
}

// udpRate resolves the UDP target rate once per run and records its
// side-effects into Results: an explicit --udp-rate is honored but flagged
// (note + UDPNearSaturation) above 95% of the negotiated speed; otherwise
// the rate is 80% of the negotiated speed via config.DefaultUDPRate, falling
// back to 100M with a warning note and UDPRateAssumed when the speed is
// unknown.
func (q *QuickPlan) udpRate() model.Bitrate {
	negotiated := q.negotiatedBitrate()
	if q.UDPRate > 0 {
		near, warns := config.CheckExplicitUDPRate(q.UDPRate, negotiated)
		q.Results.Notes = append(q.Results.Notes, warns...)
		if near {
			q.Results.UDPNearSaturation = true
		}
		return q.UDPRate
	}
	rate, warns := config.DefaultUDPRate(negotiated)
	q.Results.Notes = append(q.Results.Notes, warns...)
	if negotiated == 0 {
		q.Results.UDPRateAssumed = true
	}
	return rate
}

// negotiatedBitrate derives the negotiated link speed from the link-settings
// step: the lowest positive speed either side reported (they should agree on
// a healthy link; the minimum is the conservative choice), 0 when unknown.
func (q *QuickPlan) negotiatedBitrate() model.Bitrate {
	best := 0
	for _, key := range []string{RolePC1Key, RolePC2Key} {
		lr := q.Results.Link[key]
		if lr == nil || lr.Settings.SpeedMbps <= 0 {
			continue
		}
		if best == 0 || lr.Settings.SpeedMbps < best {
			best = lr.Settings.SpeedMbps
		}
	}
	return model.Bitrate(best) * 1_000_000
}
