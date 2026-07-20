package testsuite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"sync"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/runner"
)

// Op names of the test-request vocabulary the coordinator sends the worker.
const (
	// OpCountersSnapshot captures a NIC counter snapshot (no params).
	OpCountersSnapshot = "counters_snapshot"
	// OpLinkSettings inspects the negotiated link settings (no params).
	OpLinkSettings = "link_settings"
	// OpPingRun runs the quick stability ping (PingRunParams).
	OpPingRun = "ping_run"
	// OpIperfServerStart starts a one-off iperf3 server and waits until it
	// listens (IperfServerStartParams).
	OpIperfServerStart = "iperf3_server_start"
	// OpIperfServerStop stops a previously started server (IperfServerStopParams).
	OpIperfServerStop = "iperf3_server_stop"
	// OpIperfClientRun runs a TCP iperf3 client phase (IperfClientRunParams).
	OpIperfClientRun = "iperf3_client_run"
	// OpPingFullSize runs the full-size non-fragmenting ping; the worker
	// computes the -s payload from its OWN interface MTU, so the params
	// carry only the target and count (PingFullSizeParams).
	OpPingFullSize = "ping_fullsize"
	// OpIperfUDPRun runs a UDP iperf3 client phase (UDPRunParams); the -l
	// datagram size is computed from the worker's own MTU.
	OpIperfUDPRun = "iperf3_udp_run"
	// OpIperfCaps reports the worker's detected iperf3 capabilities (no
	// params) so the coordinator can compute the effective set (local AND
	// peer) before choosing the native --bidir path or the two-phase
	// fallback.
	OpIperfCaps = "iperf3_caps"
	// OpCableTestWindowStart opens the coordinated link-loss window before
	// the local cable diagnostic runs.
	OpCableTestWindowStart = protocol.OpCableTestWindowStart
	// OpCableTestWindowEnd closes the coordinated window after link recovery.
	OpCableTestWindowEnd = protocol.OpCableTestWindowEnd
	// OpCancel aborts the in-flight op; it is intercepted by the peer
	// session layer and never reaches HandleOp.
	OpCancel = "cancel"
)

// Result statuses of protocol.TestResult as produced by HandleOp.
const (
	// StatusOK marks a completed op with a result payload.
	StatusOK = "ok"
	// StatusFailed marks an op that ran but failed.
	StatusFailed = "failed"
	// StatusTimeout marks an op that exceeded its time budget.
	StatusTimeout = "timeout"
	// StatusUnavailable marks an op whose tool is not usable on this host;
	// the evaluator, not the peer layer, decides what that means.
	StatusUnavailable = "unavailable"
	// StatusRejected marks a request the worker refused to run at all.
	StatusRejected = "rejected"
)

// PingRunParams are the parameters of OpPingRun.
type PingRunParams struct {
	// PeerIP is the address the worker pings (the coordinator's test IP).
	PeerIP string `json:"peerIp"`
	// Count is the number of echo requests.
	Count int `json:"count"`
}

// IperfServerStartParams are the parameters of OpIperfServerStart.
type IperfServerStartParams struct {
	// BindIP is the worker-local address the server binds to.
	BindIP string `json:"bindIp"`
	// Port is the iperf3 port.
	Port uint16 `json:"port"`
}

// IperfServerStopParams are the parameters of OpIperfServerStop.
type IperfServerStopParams struct {
	// Port identifies which server to stop.
	Port uint16 `json:"port"`
}

// IperfClientRunParams are the parameters of OpIperfClientRun.
type IperfClientRunParams struct {
	// LocalIP is the worker-local address the client binds to (-B).
	LocalIP string `json:"localIp"`
	// PeerIP is the server address the client connects to (-c).
	PeerIP string `json:"peerIp"`
	// Port is the iperf3 port.
	Port uint16 `json:"port"`
	// DurationSec is the -t test duration in seconds.
	DurationSec int `json:"durationSec"`
	// Streams is the -P parallel stream count.
	Streams int `json:"streams"`
	// Bidir requests a --bidir run.
	Bidir bool `json:"bidir"`
}

// PingFullSizeParams are the parameters of OpPingFullSize. The payload size
// is deliberately absent: each side derives MTU-28 from its own discovered
// interface MTU (the two interfaces may be configured differently).
type PingFullSizeParams struct {
	// PeerIP is the address the worker pings (the coordinator's test IP).
	PeerIP string `json:"peerIp"`
	// Count is the number of echo requests.
	Count int `json:"count"`
}

// UDPRunParams are the parameters of OpIperfUDPRun.
type UDPRunParams struct {
	// LocalIP is the worker-local address the client binds to (-B).
	LocalIP string `json:"localIp"`
	// PeerIP is the server address the client connects to (-c).
	PeerIP string `json:"peerIp"`
	// Port is the iperf3 port.
	Port uint16 `json:"port"`
	// DurationSec is the -t test duration in seconds.
	DurationSec int `json:"durationSec"`
	// RateBps is the -b target rate in bits per second (plain integer).
	RateBps uint64 `json:"rateBps"`
}

// LinkSettingsResult is the payload of a completed OpLinkSettings.
type LinkSettingsResult struct {
	// Settings is the parsed ethtool link state.
	Settings model.LinkSettings `json:"settings"`
	// Observations are the ObserveLink findings, already worded for humans.
	Observations []string `json:"observations,omitempty"`
}

// PingRunResult is the payload of a completed OpPingRun.
type PingRunResult struct {
	// Ping is the parsed ping outcome.
	Ping model.PingResult `json:"ping"`
	// Notes records limitations such as a denied ping interval.
	Notes []string `json:"notes,omitempty"`
}

// ServerStartResult is the payload of a completed OpIperfServerStart; it is
// sent only after the server is confirmed listening.
type ServerStartResult struct {
	// Port echoes the listening port.
	Port uint16 `json:"port"`
}

// ServerStopResult is the payload of a completed OpIperfServerStop.
type ServerStopResult struct {
	// Stopped confirms the server is gone.
	Stopped bool `json:"stopped"`
}

// TCPRunResult is the payload of a completed OpIperfClientRun.
type TCPRunResult struct {
	// TCP is the throughput result (Direction is filled by the coordinator).
	TCP model.TCPResult `json:"tcp"`
	// Incomplete marks a partial result from a canceled or failed run.
	Incomplete bool `json:"incomplete,omitempty"`
}

// UDPRunResult is the payload of a completed OpIperfUDPRun.
type UDPRunResult struct {
	// UDP is the loss/jitter result (Direction is filled by the coordinator).
	UDP model.UDPResult `json:"udp"`
	// Incomplete marks a partial result from a canceled or failed run.
	Incomplete bool `json:"incomplete,omitempty"`
}

// BidirRunResult is the outcome of a native --bidir client run; it stays
// local to the coordinator (the client always runs on PC1), but shares the
// run-result shape of the other iperf3 phases.
type BidirRunResult struct {
	// Bidir is the bidirectional stress result.
	Bidir model.BidirResult `json:"bidir"`
	// Incomplete marks a partial result from a canceled or failed run.
	Incomplete bool `json:"incomplete,omitempty"`
}

// opDecoders is the fixed op-to-result-decoder table: the op name alone
// determines the concrete payload type — never reflection on foreign input.
var opDecoders = map[string]func(json.RawMessage) (any, error){
	OpCountersSnapshot: decodeAs[model.CounterSnapshot],
	OpLinkSettings:     decodeAs[LinkSettingsResult],
	OpPingRun:          decodeAs[PingRunResult],
	OpIperfServerStart: decodeAs[ServerStartResult],
	OpIperfServerStop:  decodeAs[ServerStopResult],
	OpIperfClientRun:   decodeAs[TCPRunResult],
	OpPingFullSize:     decodeAs[PingRunResult],
	OpIperfUDPRun:      decodeAs[UDPRunResult],
	OpIperfCaps:        decodeAs[model.Iperf3Caps],
}

// decodeAs unmarshals raw into a fresh *T.
func decodeAs[T any](raw json.RawMessage) (any, error) {
	out := new(T)
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	return out, nil
}

func parseLocalPeer(op, localIP, peerIP string) (local, remote netip.Addr, err error) {
	local, err = netip.ParseAddr(localIP)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("testsuite: %s: bad localIp: %w", op, err)
	}
	remote, err = parsePeer(op, peerIP)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	return local, remote, nil
}

func parsePeer(op, peerIP string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(peerIP)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("testsuite: %s: bad peerIp: %w", op, err)
	}
	return addr, nil
}

// DecodeOpResult decodes a remote test_result payload into the typed result
// struct for op, using the fixed decoder table. Unknown ops are an error.
func DecodeOpResult(op string, raw json.RawMessage) (any, error) {
	dec, ok := opDecoders[op]
	if !ok {
		return nil, fmt.Errorf("testsuite: no result decoder for op %q", op)
	}
	out, err := dec(raw)
	if err != nil {
		return nil, fmt.Errorf("testsuite: decode %s result: %w", op, err)
	}
	return out, nil
}

// Ops is the worker-side peer.OpHandler: it dispatches incoming test
// requests to the local test components and tracks the iperf3 servers
// started on behalf of the coordinator.
type Ops struct {
	// Counters serves OpCountersSnapshot.
	Counters *CounterCollector
	// Link serves OpLinkSettings.
	Link *LinkInspector
	// Ping serves OpPingRun.
	Ping *PingTester
	// Iperf serves the iperf3 server and client ops.
	Iperf *IperfManager
	// Cable runs the coordinator-local opt-in ethtool cable diagnostic.
	Cable *CableTester
	// MTU is the discovered MTU of the interface under test; the full-size
	// ping payload and the UDP datagram size are computed from it (MTU-28,
	// never hardcoded).
	MTU int
	// IperfCaps are the locally detected iperf3 capabilities, served by
	// OpIperfCaps.
	IperfCaps model.Iperf3Caps

	mu      sync.Mutex
	servers map[uint16]*ServerHandle
}

var _ peer.OpHandler = (*Ops)(nil)

// HandleOp implements peer.OpHandler for the quick-mode op vocabulary. It
// announces the op via one progress frame naming its stage, runs it, and
// maps errors onto the test-result statuses (a missing tool is
// StatusUnavailable, a blown deadline StatusTimeout, unknown or malformed
// requests StatusRejected, everything else StatusFailed).
func (o *Ops) HandleOp(ctx context.Context, op string, params json.RawMessage,
	progress func(protocol.TestProgress)) (any, string, error) {
	if progress != nil {
		progress(protocol.TestProgress{Stage: op, Percent: -1, Text: "running " + op})
	}
	switch op {
	case OpCountersSnapshot:
		snap, err := o.Counters.Snapshot(ctx)
		if err != nil {
			return nil, statusFor(err), err
		}
		return &snap, StatusOK, nil

	case OpLinkSettings:
		settings, obs, err := o.Link.Inspect(ctx)
		if err != nil {
			return nil, statusFor(err), err
		}
		return &LinkSettingsResult{Settings: settings, Observations: obs}, StatusOK, nil

	case OpPingRun:
		var p PingRunParams
		if err := decodeParams(params, &p); err != nil {
			return nil, StatusRejected, err
		}
		addr, err := parsePeer(op, p.PeerIP)
		if err != nil {
			return nil, StatusRejected, err
		}
		res, notes, err := o.Ping.Quick(ctx, addr, p.Count)
		if err != nil {
			return nil, statusFor(err), err
		}
		return &PingRunResult{Ping: res, Notes: notes}, StatusOK, nil

	case OpPingFullSize:
		var p PingFullSizeParams
		if err := decodeParams(params, &p); err != nil {
			return nil, StatusRejected, err
		}
		addr, err := parsePeer(op, p.PeerIP)
		if err != nil {
			return nil, StatusRejected, err
		}
		res, notes, err := o.Ping.FullSize(ctx, addr, o.MTU, p.Count)
		if err != nil {
			return nil, statusFor(err), err
		}
		return &PingRunResult{Ping: res, Notes: notes}, StatusOK, nil

	case OpIperfCaps:
		caps := o.IperfCaps
		return &caps, StatusOK, nil

	case OpCableTestWindowStart:
		var p protocol.CableTestWindowParams
		if err := decodeParams(params, &p); err != nil || p.IdleTimeoutMs <= 0 {
			return nil, StatusRejected, fmt.Errorf("testsuite: invalid cable-test window timeout")
		}
		return struct{}{}, StatusOK, nil

	case OpCableTestWindowEnd:
		return struct{}{}, StatusOK, nil

	case OpIperfServerStart:
		return o.handleServerStart(ctx, params)

	case OpIperfServerStop:
		return o.handleServerStop(ctx, params)

	case OpIperfClientRun:
		var p IperfClientRunParams
		if err := decodeParams(params, &p); err != nil {
			return nil, StatusRejected, err
		}
		local, remote, err := parseLocalPeer(op, p.LocalIP, p.PeerIP)
		if err != nil {
			return nil, StatusRejected, err
		}
		res, err := o.Iperf.RunTCPClient(ctx, local, remote, p.Port,
			time.Duration(p.DurationSec)*time.Second, p.Streams, p.Bidir)
		if err != nil {
			return res, statusFor(err), err
		}
		return res, StatusOK, nil

	case OpIperfUDPRun:
		var p UDPRunParams
		if err := decodeParams(params, &p); err != nil {
			return nil, StatusRejected, err
		}
		local, remote, err := parseLocalPeer(op, p.LocalIP, p.PeerIP)
		if err != nil {
			return nil, StatusRejected, err
		}
		res, err := o.Iperf.RunUDPClient(ctx, local, remote, p.Port,
			time.Duration(p.DurationSec)*time.Second, p.RateBps, UDPPayloadForMTU(o.MTU))
		if err != nil {
			return res, statusFor(err), err
		}
		return res, StatusOK, nil

	default:
		// Includes OpCancel, which the peer session layer intercepts and
		// must never forward here.
		return nil, StatusRejected, fmt.Errorf("testsuite: unknown op %q", op)
	}
}

// handleServerStart starts a one-off iperf3 server and waits for readiness;
// the ok result is sent only once the server is confirmed listening.
func (o *Ops) handleServerStart(ctx context.Context, params json.RawMessage) (any, string, error) {
	var p IperfServerStartParams
	if err := decodeParams(params, &p); err != nil {
		return nil, StatusRejected, err
	}
	bind, err := netip.ParseAddr(p.BindIP)
	if err != nil {
		return nil, StatusRejected, fmt.Errorf("testsuite: %s: bad bindIp: %w", OpIperfServerStart, err)
	}
	o.mu.Lock()
	if _, exists := o.servers[p.Port]; exists {
		o.mu.Unlock()
		return nil, StatusRejected, fmt.Errorf("testsuite: an iperf3 server on port %d is already running", p.Port)
	}
	o.mu.Unlock()

	h, err := o.Iperf.StartServer(ctx, bind, p.Port)
	if err != nil {
		return nil, statusFor(err), err
	}
	if err := h.Ready(ctx); err != nil {
		_ = h.Stop(ctx)
		return nil, statusFor(err), err
	}
	o.mu.Lock()
	if o.servers == nil {
		o.servers = make(map[uint16]*ServerHandle)
	}
	o.servers[p.Port] = h
	o.mu.Unlock()
	return &ServerStartResult{Port: p.Port}, StatusOK, nil
}

// handleServerStop stops the server previously started on the given port.
func (o *Ops) handleServerStop(ctx context.Context, params json.RawMessage) (any, string, error) {
	var p IperfServerStopParams
	if err := decodeParams(params, &p); err != nil {
		return nil, StatusRejected, err
	}
	o.mu.Lock()
	h := o.servers[p.Port]
	delete(o.servers, p.Port)
	o.mu.Unlock()
	if h == nil {
		return nil, StatusRejected, fmt.Errorf("testsuite: no iperf3 server running on port %d", p.Port)
	}
	if err := h.Stop(ctx); err != nil {
		return nil, statusFor(err), err
	}
	return &ServerStopResult{Stopped: true}, StatusOK, nil
}

// StopAllServers force-stops every server this handler still tracks; the
// session teardown calls it so an aborted run leaves no iperf3 behind.
func (o *Ops) StopAllServers(ctx context.Context) {
	o.mu.Lock()
	handles := make([]*ServerHandle, 0, len(o.servers))
	for port, h := range o.servers {
		handles = append(handles, h)
		delete(o.servers, port)
	}
	o.mu.Unlock()
	for _, h := range handles {
		_ = h.Stop(ctx)
	}
}

// decodeParams unmarshals op params, treating malformed input as a protocol
// misuse (the coordinator always sends well-formed typed params).
func decodeParams(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return errors.New("testsuite: missing op params")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("testsuite: malformed op params: %w", err)
	}
	return nil
}

// statusFor maps an op error onto the test-result status vocabulary.
func statusFor(err error) string {
	switch {
	case err == nil:
		return StatusOK
	case errors.Is(err, exec.ErrNotFound):
		return StatusUnavailable
	case errors.Is(err, parser.ErrIperfUnreachable):
		// Not a cable fault; unavailable keeps the run alive to finalize as a
		// SkippedTest + INCONCLUSIVE instead of a fatal abort.
		return StatusUnavailable
	case errors.Is(err, runner.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return StatusTimeout
	default:
		return StatusFailed
	}
}
