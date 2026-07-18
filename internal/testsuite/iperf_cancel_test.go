package testsuite

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// TestCancellationMidTCP cancels the context while the TCP client is running:
// the client returns a cancellation error with the partial result marked
// incomplete, and stopping the server under the cancelled context kills the
// server process outright.
func TestCancellationMidTCP(t *testing.T) {
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		StdoutFile: fixturePath("iperf", "server_listening.txt")})
	started := make(chan struct{})
	fr.Script(runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-c"),
		Delay: make(chan struct{}), Started: started})
	m, _ := newTestManager(t, fr)

	local := netip.MustParseAddr("10.0.0.1")
	peer := netip.MustParseAddr("10.0.0.2")

	h, err := m.StartServer(context.Background(), local, 5201)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	if err := h.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	res, err := m.RunTCPClient(ctx, local, peer, 5201, 30*time.Second, 4, false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RunTCPClient error = %v, want errors.Is context.Canceled", err)
	}
	if res == nil || !res.Incomplete {
		t.Errorf("cancelled run result = %+v, want a partial result marked incomplete", res)
	}

	if err := h.Stop(ctx); err != nil {
		t.Errorf("Stop under cancelled ctx: %v", err)
	}
	server := fr.Processes()[0]
	if !server.Killed() {
		t.Errorf("server FakeProcess not killed after cancellation")
	}
}

// TestStaleProcessPreflight seeds the pidfile tree with a live, ownership-
// verified process (this test binary itself) and a dead one: the live one
// fails preflight with remediation text, the dead one is cleaned silently.
func TestStaleProcessPreflight(t *testing.T) {
	t.Run("LiveStaleFails", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "ct-old")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		info, err := runner.NewProcessInfo(os.Getpid(), "iperf3-server", "ct-old")
		if err != nil {
			t.Fatalf("NewProcessInfo(self): %v", err)
		}
		writePidfile(t, dir, info)

		_, err = PreflightStaleProcesses(base)
		if err == nil {
			t.Fatalf("PreflightStaleProcesses passed despite a live verified stale process")
		}
		msg := err.Error()
		if !strings.Contains(msg, strconv.Itoa(os.Getpid())) {
			t.Errorf("preflight error %q does not name the stale pid %d", msg, os.Getpid())
		}
		if !strings.Contains(strings.ToLower(msg), "kill") {
			t.Errorf("preflight error %q lacks remediation text (how to kill the survivor)", msg)
		}
	})

	t.Run("DeadPidfileCleaned", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "ct-dead")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		dead := runner.ProcessInfo{PID: 1, PGID: 1, StartTicks: 1, Argv0: "iperf3",
			Label: "iperf3-server", TestID: "ct-dead"}
		path := writePidfile(t, dir, dead)

		if _, err := PreflightStaleProcesses(base); err != nil {
			t.Fatalf("PreflightStaleProcesses failed on a dead pidfile: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dead pidfile %s survived preflight; want it cleaned up", path)
		}
	})
}

// writePidfile stores info as <pid>.json in dir, mirroring the registry's
// on-disk convention, and returns the file path.
func writePidfile(t *testing.T, dir string, info runner.ProcessInfo) string {
	t.Helper()
	path := filepath.Join(dir, strconv.Itoa(info.PID)+".json")
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal pidfile: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	return path
}
