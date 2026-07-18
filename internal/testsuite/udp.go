package testsuite

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"cablecheck/internal/model"
	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
)

// udpOverheadBytes is the IPv4 header (20) plus UDP header (8): a datagram
// payload of MTU-28 fills one link-layer frame exactly, so UDP loss aligns
// with per-frame cable behavior instead of IP fragmentation artifacts.
const udpOverheadBytes = 28

// UDPPayloadForMTU returns the iperf3 -l datagram size for an interface MTU:
// MTU-28, pinning one datagram to one frame (no IP fragmentation). It is
// always computed from the discovered MTU — never a hardcoded 1472.
func UDPPayloadForMTU(mtu int) int {
	return mtu - udpOverheadBytes
}

// RunUDPClient runs one UDP loss/jitter phase as the iperf3 client toward the
// peer's one-off server: base client argv plus `-u -b <rate> -t <dur>
// -l <payload>` (tools.md §2 invocation matrix). rateBps is passed as a plain
// integer to sidestep suffix/decimal parsing differences across iperf3
// builds. The loss and jitter in the result are server-observed, reported
// back inside the client's end.sum. On any failure a non-nil result marked
// Incomplete is returned alongside the error, so partial data survives.
func (m *IperfManager) RunUDPClient(ctx context.Context, local, peer netip.Addr, port uint16,
	dur time.Duration, rateBps uint64, payload int) (*UDPRunResult, error) {
	args := []string{
		"-c", peer.String(),
		"-B", local.String(),
		"-p", strconv.Itoa(int(port)),
		"-J",
		"--connect-timeout", "3000",
		"-u",
		"-b", strconv.FormatUint(rateBps, 10),
		"-t", strconv.Itoa(int(dur / time.Second)),
		"-l", strconv.Itoa(payload),
	}
	spec := teeRaw(runner.CommandSpec{
		Name:    "iperf3",
		Args:    args,
		Timeout: dur + clientTimeoutSlack,
		Label:   "iperf3-udp-client",
	}, m.RawDir)

	partial := &UDPRunResult{
		Incomplete: true,
		UDP:        model.UDPResult{TargetBps: rateBps},
	}
	res, err := m.R.Run(ctx, spec)
	if err != nil {
		return partial, fmt.Errorf("testsuite: iperf3 udp client: %w", err)
	}
	parsed, err := parser.ParseIperf3(res.Stdout)
	if err != nil {
		return partial, fmt.Errorf("testsuite: iperf3 udp client: %w", err)
	}
	if parsed.UDP == nil {
		return partial, fmt.Errorf("testsuite: iperf3 udp client: output carries no UDP summary")
	}
	return &UDPRunResult{UDP: udpFromIperf(parsed, rateBps)}, nil
}

// udpFromIperf maps a parsed UDP iperf3 run onto the report model. The
// receiver-side rate is derived from the sender rate and the server-observed
// delivered fraction (same datagram size on both ends), since iperf3's UDP
// end.sum carries only one rate.
func udpFromIperf(p parser.Iperf3Result, rateBps uint64) model.UDPResult {
	u := p.UDP
	out := model.UDPResult{
		TargetBps:         rateBps,
		ActualSenderBps:   u.ActualBps,
		ActualReceiverBps: u.ActualBps * (100 - u.LostPercent) / 100,
		LostPackets:       u.Lost,
		TotalPackets:      u.Total,
		LossPercent:       u.LostPercent,
		JitterMs:          u.JitterMs,
		OutOfOrder:        u.OutOfOrder,
	}
	if p.CPU != nil {
		out.CPU = model.CPUUsage{
			HostTotal: p.CPU.HostTotal, HostUser: p.CPU.HostUser, HostSystem: p.CPU.HostSystem,
			RemoteTotal: p.CPU.RemoteTotal, RemoteUser: p.CPU.RemoteUser, RemoteSystem: p.CPU.RemoteSystem,
		}
	}
	return out
}
