package testsuite

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
)

// ErrPortInUse reports that the iperf3 server could not bind because the
// port is already taken (typically a foreign iperf3 the user runs manually).
var ErrPortInUse = errors.New("iperf3 port already in use")

// serverBanner is the readiness line a --forceflush iperf3 server prints as
// soon as it is listening.
const serverBanner = "Server listening on "

const (
	// serverReadyFallback is how long Ready waits for the banner before
	// declaring a still-alive server ready anyway (bind failures exit
	// immediately, so silence plus liveness means listening).
	serverReadyFallback = 1500 * time.Millisecond
	// serverStopGrace is the SIGTERM-to-SIGKILL grace inside Stop.
	serverStopGrace = 3 * time.Second
	// clientTimeoutSlack pads the client runner timeout beyond -t.
	clientTimeoutSlack = 15 * time.Second
)

// Registrar registers session-owned processes for stale-survivor cleanup;
// *runner.Registry satisfies it.
type Registrar interface {
	// Register records p and returns an unregister func removing it again.
	Register(p runner.ProcessInfo) (func(), error)
}

// IdentifyFunc snapshots a live process's identity so it can be registered;
// runner.NewProcessInfo is the production implementation.
type IdentifyFunc func(pid int, label, testID string) (runner.ProcessInfo, error)

// IperfManager starts one-off iperf3 servers and runs iperf3 clients for the
// throughput tests. The server is always on the receiving side of a phase;
// direction change means swapping the server side, never -R.
type IperfManager struct {
	// R runs iperf3.
	R runner.Runner
	// Reg registers server processes; nil disables registration.
	Reg Registrar
	// RawDir, when non-empty, receives verbatim iperf3 output tee files.
	RawDir string
	// Clock drives the readiness fallback and stop grace timers.
	Clock clock.Clock
	// TestID tags registered processes with the owning session.
	TestID string
	// Identify overrides runner.NewProcessInfo in tests; nil means real /proc.
	Identify IdentifyFunc
}

// StartServer launches `iperf3 -s -B <bind> -p <port> -1 --forceflush` and
// returns a handle; call Ready to wait until it listens. Registration
// failures are non-fatal: the one-off server may already have exited on a
// bind failure, which Ready diagnoses as ErrPortInUse.
func (m *IperfManager) StartServer(ctx context.Context, bind netip.Addr, port uint16) (*ServerHandle, error) {
	spec := teeRaw(runner.CommandSpec{
		Name: "iperf3",
		Args: []string{"-s", "-B", bind.String(), "-p", strconv.Itoa(int(port)),
			"-1", "--forceflush"},
		Label: "iperf3-server",
	}, m.RawDir)
	proc, err := m.R.Start(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("testsuite: start iperf3 server: %w", err)
	}
	unregister := func() {}
	if m.Reg != nil {
		identify := m.Identify
		if identify == nil {
			identify = runner.NewProcessInfo
		}
		if info, err := identify(proc.PID(), "iperf3-server", m.TestID); err == nil {
			if un, err := m.Reg.Register(info); err == nil {
				unregister = un
			}
		}
	}
	return &ServerHandle{
		proc:       proc,
		port:       port,
		clk:        m.Clock,
		unregister: unregister,
		banner:     make(chan struct{}),
	}, nil
}

// ServerHandle controls one running one-off iperf3 server.
type ServerHandle struct {
	proc       runner.Process
	port       uint16
	clk        clock.Clock
	unregister func()
	scanOnce   sync.Once
	banner     chan struct{}

	mu      sync.Mutex
	stopped bool
}

// Ready blocks until the server is listening: it scans the live stdout for
// the "Server listening on" banner and falls back to "alive after 1.5s"
// (bind failures exit immediately). It NEVER dial-probes the port — a
// cookie-less TCP connect can consume and abort the one-off session.
func (h *ServerHandle) Ready(ctx context.Context) error {
	h.scanOnce.Do(func() { go h.scanBanner() })
	select {
	case <-h.banner:
		return nil
	case <-h.proc.Done():
		return h.exitFailure(ctx)
	case <-h.clk.After(serverReadyFallback):
		select {
		case <-h.proc.Done():
			return h.exitFailure(ctx)
		default:
			return nil // no banner, but alive well past any bind failure
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// scanBanner reads the server's live stdout line by line and closes the
// banner channel on the readiness line, then stops reading.
func (h *ServerHandle) scanBanner() {
	sc := bufio.NewScanner(h.proc.Stdout())
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), serverBanner) {
			close(h.banner)
			return
		}
	}
}

// exitFailure diagnoses a server that exited before becoming ready.
func (h *ServerHandle) exitFailure(ctx context.Context) error {
	res, err := h.proc.Wait(ctx)
	if err != nil {
		return fmt.Errorf("testsuite: iperf3 server on port %d exited before ready: %w", h.port, err)
	}
	detail := strings.TrimSpace(string(res.Stderr))
	if strings.Contains(detail, "unable to start listener") ||
		strings.Contains(detail, "Address already in use") {
		return fmt.Errorf("testsuite: iperf3 server: port %d: %w (%s)", h.port, ErrPortInUse, detail)
	}
	return fmt.Errorf("testsuite: iperf3 server on port %d exited %d before ready: %s",
		h.port, res.ExitCode, detail)
}

// Stop shuts the server down: SIGTERM, a 3s grace, then SIGKILL — or SIGKILL
// straight away when ctx is already canceled (abort path). It is idempotent,
// a no-op when the one-off server already exited, and always releases the
// registry entry. Stop never fails: every path ends with the process dead.
func (h *ServerHandle) Stop(ctx context.Context) error {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	h.mu.Unlock()
	defer h.unregister()

	select {
	case <-h.proc.Done():
		return nil // one-off server already finished (or failed to bind)
	default:
	}
	if ctx.Err() != nil {
		_ = h.proc.Kill()
		<-h.proc.Done()
		return nil
	}
	_ = h.proc.Terminate()
	select {
	case <-h.proc.Done():
	case <-h.clk.After(serverStopGrace):
		_ = h.proc.Kill()
		<-h.proc.Done()
	case <-ctx.Done():
		_ = h.proc.Kill()
		<-h.proc.Done()
	}
	return nil
}

// RunTCPClient runs one TCP throughput phase as the iperf3 client toward the
// peer's server (`-c peer -B local -p port -J --connect-timeout 3000 -t dur
// -P streams`, plus --bidir when bidir is set). On any failure — including
// cancellation mid-test — it returns a non-nil result marked Incomplete
// alongside the error, so partial data survives an abort.
func (m *IperfManager) RunTCPClient(ctx context.Context, local, peer netip.Addr, port uint16,
	dur time.Duration, streams int, bidir bool) (*TCPRunResult, error) {
	args := []string{
		"-c", peer.String(),
		"-B", local.String(),
		"-p", strconv.Itoa(int(port)),
		"-J",
		"--connect-timeout", "3000",
		"-t", strconv.Itoa(int(dur / time.Second)),
		"-P", strconv.Itoa(streams),
	}
	if bidir {
		args = append(args, "--bidir")
	}
	spec := teeRaw(runner.CommandSpec{
		Name:    "iperf3",
		Args:    args,
		Timeout: dur + clientTimeoutSlack,
		Label:   "iperf3-client",
	}, m.RawDir)

	partial := &TCPRunResult{
		Incomplete: true,
		TCP:        model.TCPResult{Duration: model.Duration(dur), ParallelStreams: streams},
	}
	res, err := m.R.Run(ctx, spec)
	if err != nil {
		return partial, fmt.Errorf("testsuite: iperf3 client: %w", err)
	}
	parsed, err := parser.ParseIperf3(res.Stdout)
	if err != nil {
		return partial, fmt.Errorf("testsuite: iperf3 client: %w", err)
	}
	return &TCPRunResult{TCP: tcpFromIperf(parsed, dur, streams)}, nil
}

// RunBidirClient runs one native bidirectional stress phase as the iperf3
// client (`--bidir`) toward the peer's single one-off server: the client side
// is PC1, so the parser's client-to-server direction is PC1→PC2. Per-direction
// totals always come from the per-stream sender flags (iperf3 3.7-3.11 emit
// duplicate, misattributed top-level bidir sums). On any failure a non-nil
// result marked Incomplete is returned alongside the error.
func (m *IperfManager) RunBidirClient(ctx context.Context, local, peer netip.Addr, port uint16,
	dur time.Duration, streams int) (*BidirRunResult, error) {
	args := []string{
		"-c", peer.String(),
		"-B", local.String(),
		"-p", strconv.Itoa(int(port)),
		"-J",
		"--connect-timeout", "3000",
		"-t", strconv.Itoa(int(dur / time.Second)),
		"-P", strconv.Itoa(streams),
		"--bidir",
	}
	spec := teeRaw(runner.CommandSpec{
		Name:    "iperf3",
		Args:    args,
		Timeout: dur + clientTimeoutSlack,
		Label:   "iperf3-bidir-client",
	}, m.RawDir)

	partial := &BidirRunResult{
		Incomplete: true,
		Bidir:      model.BidirResult{Duration: model.Duration(dur), ParallelStreams: streams},
	}
	res, err := m.R.Run(ctx, spec)
	if err != nil {
		return partial, fmt.Errorf("testsuite: iperf3 bidir client: %w", err)
	}
	parsed, err := parser.ParseIperf3(res.Stdout)
	if err != nil {
		return partial, fmt.Errorf("testsuite: iperf3 bidir client: %w", err)
	}
	if parsed.Bidir == nil {
		return partial, fmt.Errorf("testsuite: iperf3 bidir client: output carries no bidirectional streams")
	}
	out := model.BidirResult{
		Duration:        model.Duration(dur),
		ParallelStreams: streams,
		PC1ToPC2:        bidirDirFromStats(parsed.Bidir.LocalToPeer),
		PC2ToPC1:        bidirDirFromStats(parsed.Bidir.PeerToLocal),
	}
	if parsed.CPU != nil {
		out.CPUUtilization = model.CPUUsage{
			HostTotal: parsed.CPU.HostTotal, HostUser: parsed.CPU.HostUser, HostSystem: parsed.CPU.HostSystem,
			RemoteTotal: parsed.CPU.RemoteTotal, RemoteUser: parsed.CPU.RemoteUser, RemoteSystem: parsed.CPU.RemoteSystem,
		}
	}
	return &BidirRunResult{Bidir: out}, nil
}

// bidirDirFromStats maps one stream-derived direction onto the report model.
// The stream rows carry a single rate per direction, recorded as the sender
// side; the receiver rate is not reported separately in bidir mode.
func bidirDirFromStats(d parser.DirStats) model.BidirDirection {
	return model.BidirDirection{
		SenderBitsPerSecond: d.BitsPerSecond,
		Retransmissions:     d.Retransmits,
	}
}

// bidirDirFromTCP maps one completed one-way TCP phase of the two-phase
// fallback onto a bidir direction.
func bidirDirFromTCP(tr model.TCPResult) model.BidirDirection {
	return model.BidirDirection{
		SenderBitsPerSecond:   tr.SenderBitsPerSecond,
		ReceiverBitsPerSecond: tr.ReceiverBitsPerSecond,
		Retransmissions:       tr.Retransmissions,
	}
}

// tcpFromIperf maps a parsed one-way iperf3 TCP run onto the report model.
func tcpFromIperf(p parser.Iperf3Result, dur time.Duration, streams int) model.TCPResult {
	out := model.TCPResult{
		Duration:            model.Duration(dur),
		ParallelStreams:     streams,
		ThroughputVariation: p.IntervalCoV,
		MinimumIntervalBps:  p.IntervalMinBps,
		MaximumIntervalBps:  p.IntervalMaxBps,
	}
	if p.Sent != nil {
		out.SenderBitsPerSecond = p.Sent.BitsPerSecond
		out.Retransmissions = p.Sent.Retransmits
	}
	if p.Received != nil {
		out.ReceiverBitsPerSecond = p.Received.BitsPerSecond
	}
	for _, iv := range p.Intervals {
		out.IntervalResults = append(out.IntervalResults, model.TCPInterval{
			StartSec:      iv.StartSec,
			EndSec:        iv.EndSec,
			Bytes:         iv.Bytes,
			BitsPerSecond: iv.Bps,
			Retransmits:   iv.Retransmits,
		})
	}
	if p.CPU != nil {
		out.CPUUtilization = model.CPUUsage{
			HostTotal: p.CPU.HostTotal, HostUser: p.CPU.HostUser, HostSystem: p.CPU.HostSystem,
			RemoteTotal: p.CPU.RemoteTotal, RemoteUser: p.CPU.RemoteUser, RemoteSystem: p.CPU.RemoteSystem,
		}
	}
	return out
}

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
