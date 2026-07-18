package testsuite

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// fakeRegistry records Register/unregister calls without touching /proc or
// the filesystem.
type fakeRegistry struct {
	mu           sync.Mutex
	registered   []runner.ProcessInfo
	unregistered int
}

func (f *fakeRegistry) Register(p runner.ProcessInfo) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, p)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unregistered++
	}, nil
}

func (f *fakeRegistry) snapshot() ([]runner.ProcessInfo, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.registered), f.unregistered
}

// fakeIdentify builds a ProcessInfo without reading /proc, for fake PIDs.
func fakeIdentify(pid int, label, testID string) (runner.ProcessInfo, error) {
	return runner.ProcessInfo{PID: pid, PGID: pid, StartTicks: 1, Argv0: "iperf3", Label: label, TestID: testID}, nil
}

func newTestManager(t *testing.T, fr *runnertest.FakeRunner) (*IperfManager, *fakeRegistry) {
	t.Helper()
	reg := &fakeRegistry{}
	return &IperfManager{
		R:        fr,
		Reg:      reg,
		Clock:    clocktest.New(time.Unix(1_700_000_000, 0)),
		TestID:   "ct-test",
		Identify: fakeIdentify,
	}, reg
}

// TestTCPServerLifecycle walks the full server+client happy path: server
// start with the exact one-off argv, banner readiness from the live stdout,
// a TCP client run with the exact argv, and a graceful stop, with the
// registry observing register + unregister.
func TestTCPServerLifecycle(t *testing.T) {
	ctx := context.Background()
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		StdoutFile: fixturePath("iperf", "tcp_39_fwd.json")})
	m, reg := newTestManager(t, fr)

	local := netip.MustParseAddr("10.0.0.1")
	peer := netip.MustParseAddr("10.0.0.2")

	h, err := m.StartServer(ctx, local, 5201)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	wantServer := []string{"-s", "-B", "10.0.0.1", "-p", "5201", "-1", "--forceflush"}
	if got := fr.CallsFor("iperf3")[0].Args; !slices.Equal(got, wantServer) {
		t.Errorf("server args = %q, want %q", got, wantServer)
	}
	if err := h.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v (banner fixture was scripted)", err)
	}

	res, err := m.RunTCPClient(ctx, local, peer, 5201, 30*time.Second, 4, false)
	if err != nil {
		t.Fatalf("RunTCPClient: %v", err)
	}
	calls := fr.CallsFor("iperf3")
	wantClient := []string{"-c", "10.0.0.2", "-B", "10.0.0.1", "-p", "5201",
		"-J", "--connect-timeout", "3000", "-t", "30", "-P", "4"}
	if got := calls[len(calls)-1].Args; !slices.Equal(got, wantClient) {
		t.Errorf("client args = %q, want %q", got, wantClient)
	}
	if res.Incomplete {
		t.Errorf("clean run marked incomplete")
	}
	if res.TCP.SenderBitsPerSecond <= 0 || res.TCP.ReceiverBitsPerSecond <= 0 {
		t.Errorf("throughput not parsed: sender %v receiver %v",
			res.TCP.SenderBitsPerSecond, res.TCP.ReceiverBitsPerSecond)
	}
	if res.TCP.Retransmissions == nil {
		t.Errorf("Retransmissions = nil, fixture reports retransmits")
	}

	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	proc := fr.Processes()[0]
	if !proc.Terminated() && !proc.Killed() {
		t.Errorf("server process still running after Stop")
	}
	registered, unregistered := reg.snapshot()
	if len(registered) != 1 || registered[0].Argv0 != "iperf3" {
		t.Errorf("registry registered = %+v, want exactly one iperf3 entry", registered)
	}
	if unregistered != 1 {
		t.Errorf("registry unregistered %d times, want 1", unregistered)
	}
	if err := h.Stop(ctx); err != nil {
		t.Errorf("second Stop must be a no-op, got %v", err)
	}
	if _, unregistered := reg.snapshot(); unregistered != 1 {
		t.Errorf("second Stop unregistered again")
	}
}

// TestServerPortInUse pins the typed ErrPortInUse when the one-off server
// exits at once with the bind failure on stderr.
func TestServerPortInUse(t *testing.T) {
	ctx := context.Background()
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		Result: fixture(t, "iperf", "server_port_in_use"), Delay: closedChan()})
	m, _ := newTestManager(t, fr)

	h, err := m.StartServer(ctx, netip.MustParseAddr("10.0.0.1"), 5201)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	err = h.Ready(ctx)
	if !errors.Is(err, ErrPortInUse) {
		t.Errorf("Ready error = %v, want errors.Is ErrPortInUse", err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Errorf("Stop after failed Ready: %v", err)
	}
}
