package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/testutil"
)

// deadPID is guaranteed unused: the Linux PID_MAX_LIMIT is 2^22, far below
// this value, so /proc/<deadPID> can never exist.
const deadPID = 1 << 30

// writeJSON marshals v into path, failing the test on any error.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRegistryOwnershipVerification(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)

	if got, want := DefaultBaseDir(), filepath.Join(base, "cablecheck"); got != want {
		t.Errorf("DefaultBaseDir() = %q, want %q", got, want)
	}

	reg, err := NewRegistry("t-123", clock.Real{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	wantDir := filepath.Join(base, "cablecheck", "t-123")
	if reg.StateDir() != wantDir {
		t.Errorf("StateDir() = %q, want %q", reg.StateDir(), wantDir)
	}
	marker := filepath.Join(wantDir, sessionFileName)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("session-liveness marker not written by NewRegistry: %v", err)
	}

	self := os.Getpid()
	info, err := NewProcessInfo(self, "self-label", "t-123")
	if err != nil {
		t.Fatalf("NewProcessInfo(self): %v", err)
	}
	if info.PID != self {
		t.Errorf("PID = %d, want %d", info.PID, self)
	}
	if info.PGID != syscall.Getpgrp() {
		t.Errorf("PGID = %d, want %d", info.PGID, syscall.Getpgrp())
	}
	if info.StartTicks == 0 {
		t.Error("StartTicks = 0, want the /proc/<pid>/stat starttime (field 22)")
	}
	if want := filepath.Base(os.Args[0]); info.Argv0 != want {
		t.Errorf("Argv0 = %q, want %q", info.Argv0, want)
	}

	unregister, err := reg.Register(info)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	pidfile := filepath.Join(wantDir, fmt.Sprintf("%d.json", self))
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("pidfile not written: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("pidfile is not JSON: %v", err)
	}
	for _, k := range []string{"pid", "pgid", "startTicks", "argv0", "label", "testID"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("pidfile missing key %q; got %v", k, fields)
		}
	}

	if !VerifyOwnership(info) {
		t.Error("VerifyOwnership(self) = false, want true")
	}
	tampered := info
	tampered.StartTicks++
	if VerifyOwnership(tampered) {
		t.Error("VerifyOwnership with wrong starttime = true, want false (PID reuse defense)")
	}
	wrongName := info
	wrongName.Argv0 = "iperf3"
	if VerifyOwnership(wrongName) {
		t.Error("VerifyOwnership with wrong argv0 = true, want false")
	}
	wrongGroup := info
	wrongGroup.PGID++
	if VerifyOwnership(wrongGroup) {
		t.Error("VerifyOwnership with wrong pgid = true, want false (kill paths signal -PGID, so the group must verify too)")
	}
	zeroGroup := info
	zeroGroup.PGID = 0
	if VerifyOwnership(zeroGroup) {
		t.Error("VerifyOwnership with pgid 0 = true, want false (kill(-0) would signal the caller's own group)")
	}
	dead := info
	dead.PID = deadPID
	if VerifyOwnership(dead) {
		t.Error("VerifyOwnership(dead pid) = true, want false")
	}

	// ScanStale with session markers: t-123 belongs to THIS live process, so
	// it is skipped wholesale — a concurrent cablecheck session's children
	// are never stale. t-old has no marker (dead session): its live-verified
	// survivor must be reported, and its dead pidfile and corrupt pidfile
	// must be cleaned silently.
	oldDir := filepath.Join(base, "cablecheck", "t-old")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	survivor := info
	survivor.TestID = "t-old"
	survivor.Label = "survivor"
	survivorFile := filepath.Join(oldDir, fmt.Sprintf("%d.json", self))
	writeJSON(t, survivorFile, survivor)
	deadInfo := ProcessInfo{PID: deadPID, PGID: deadPID, StartTicks: 1, Argv0: "ghost", Label: "x", TestID: "t-old"}
	deadFile := filepath.Join(oldDir, fmt.Sprintf("%d.json", deadPID))
	writeJSON(t, deadFile, deadInfo)
	corruptFile := filepath.Join(oldDir, "999.json")
	if err := os.WriteFile(corruptFile, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt pidfile: %v", err)
	}

	stale, err := ScanStale(filepath.Join(base, "cablecheck"))
	if err != nil {
		t.Fatalf("ScanStale: %v", err)
	}
	if len(stale) != 1 || stale[0].PID != self || stale[0].TestID != "t-old" {
		t.Errorf("ScanStale = %+v, want exactly the live t-old survivor", stale)
	}
	if _, err := os.Stat(deadFile); !os.IsNotExist(err) {
		t.Errorf("dead pidfile not cleaned up: stat err = %v", err)
	}
	if _, err := os.Stat(corruptFile); !os.IsNotExist(err) {
		t.Errorf("corrupt pidfile not cleaned up: stat err = %v", err)
	}
	// The live session's dir is untouched: marker and pidfile both intact.
	if _, err := os.Stat(pidfile); err != nil {
		t.Errorf("live session's pidfile was disturbed by ScanStale: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("live session's marker was disturbed by ScanStale: %v", err)
	}

	// ScanStale on a missing base dir is not an error.
	if got, err := ScanStale(filepath.Join(base, "nope")); err != nil || len(got) != 0 {
		t.Errorf("ScanStale(missing dir) = (%v, %v), want (empty, nil)", got, err)
	}

	unregister()
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Errorf("pidfile still present after unregister: stat err = %v", err)
	}
	unregister() // idempotent
}

func TestScanStaleDeadSessionCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cablecheck")
	goneDir := filepath.Join(root, "t-gone")
	if err := os.MkdirAll(goneDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A dead session marker plus a dead pidfile: everything must be removed
	// and the emptied directory pruned.
	deadSession := ProcessInfo{PID: deadPID, PGID: deadPID, StartTicks: 1, Argv0: "cablecheck", Label: "session", TestID: "t-gone"}
	writeJSON(t, filepath.Join(goneDir, sessionFileName), deadSession)
	deadProc := ProcessInfo{PID: deadPID, PGID: deadPID, StartTicks: 1, Argv0: "iperf3", Label: "x", TestID: "t-gone"}
	writeJSON(t, filepath.Join(goneDir, fmt.Sprintf("%d.json", deadPID)), deadProc)

	stale, err := ScanStale(root)
	if err != nil {
		t.Fatalf("ScanStale: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("ScanStale = %+v, want empty (everything in the dead session is dead)", stale)
	}
	if _, err := os.Stat(goneDir); !os.IsNotExist(err) {
		t.Errorf("dead session dir not pruned: stat err = %v", err)
	}
}

func TestRegisterSurvivesPrunedStateDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	reg, err := NewRegistry("t-pruned", clock.Real{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Simulate a sibling session's ScanStale pruning the directory in the
	// window between NewRegistry and Register — the race the re-create
	// defends against.
	if err := os.RemoveAll(reg.StateDir()); err != nil {
		t.Fatalf("remove state dir: %v", err)
	}
	info, err := NewProcessInfo(os.Getpid(), "self", "t-pruned")
	if err != nil {
		t.Fatalf("NewProcessInfo: %v", err)
	}
	unregister, err := reg.Register(info)
	if err != nil {
		t.Fatalf("Register after prune: %v", err)
	}
	if _, err := os.Stat(pidfilePath(reg.StateDir(), info.PID)); err != nil {
		t.Errorf("pidfile not written after prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reg.StateDir(), sessionFileName)); err != nil {
		t.Errorf("session marker not restored after prune: %v", err)
	}
	unregister()
}

func TestRegistryKillAll(t *testing.T) {
	testutil.LeakCheck(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	r := newTestRunner()
	p, err := r.Start(t.Context(), helperSpec(t, "sleep-forever"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	reg, err := NewRegistry("kill-test", clock.Real{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	info, err := NewProcessInfo(p.PID(), "sleeper", "kill-test")
	if err != nil {
		t.Fatalf("NewProcessInfo: %v", err)
	}
	if _, err := reg.Register(info); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if errs := reg.KillAll(t.Context()); len(errs) != 0 {
		t.Fatalf("KillAll errors: %v", errs)
	}
	res, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Signal != "SIGTERM" {
		t.Errorf("Signal = %q, want SIGTERM (verified group terminate)", res.Signal)
	}
	pidfile := filepath.Join(reg.StateDir(), fmt.Sprintf("%d.json", info.PID))
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Errorf("pidfile still present after KillAll: stat err = %v", err)
	}
}

func TestRegistryKillAllEscalatesToSIGKILL(t *testing.T) {
	testutil.LeakCheck(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r := newTestRunner()

	t.Run("SIGKILL after grace", func(t *testing.T) {
		p := startHelperReady(t, r, t.Context(), helperSpec(t, "ignore-sigterm"), "ready\n")
		fc := clocktest.New(time.Unix(1_700_000_000, 0))
		reg, err := NewRegistry("kill-esc", fc)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		info, err := NewProcessInfo(p.PID(), "trapper", "kill-esc")
		if err != nil {
			t.Fatalf("NewProcessInfo: %v", err)
		}
		if _, err := reg.Register(info); err != nil {
			t.Fatalf("Register: %v", err)
		}

		errsCh := make(chan []error, 1)
		go func() { errsCh <- reg.KillAll(t.Context()) }()
		// KillAll SIGTERMs the group (ignored), then enters the grace wait:
		// one After timer plus one poll ticker on the fake clock.
		fc.BlockUntilWaiters(2)
		fc.Advance(DefaultGracePeriod)
		if errs := <-errsCh; len(errs) != 0 {
			t.Fatalf("KillAll errors: %v", errs)
		}
		res, err := p.Wait(t.Context())
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if res.Signal != "SIGKILL" {
			t.Errorf("Signal = %q, want SIGKILL (survivor of the ignored SIGTERM must be escalated)", res.Signal)
		}
		if _, err := os.Stat(pidfilePath(reg.StateDir(), info.PID)); !os.IsNotExist(err) {
			t.Errorf("pidfile still present after KillAll: stat err = %v", err)
		}
	})

	t.Run("poll detects exit during grace", func(t *testing.T) {
		p := startHelperReady(t, r, t.Context(), helperSpec(t, "ignore-sigterm"), "ready\n")
		fc := clocktest.New(time.Unix(1_700_000_000, 0))
		reg, err := NewRegistry("kill-poll", fc)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		info, err := NewProcessInfo(p.PID(), "trapper", "kill-poll")
		if err != nil {
			t.Fatalf("NewProcessInfo: %v", err)
		}
		if _, err := reg.Register(info); err != nil {
			t.Fatalf("Register: %v", err)
		}

		errsCh := make(chan []error, 1)
		go func() { errsCh <- reg.KillAll(t.Context()) }()
		fc.BlockUntilWaiters(2)
		// The child dies mid-grace to an external SIGKILL; the next poll
		// tick must notice the exit and let KillAll return without the
		// grace timer ever firing.
		if err := p.Kill(); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		if _, err := p.Wait(t.Context()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		fc.Advance(killPollInterval)
		if errs := <-errsCh; len(errs) != 0 {
			t.Fatalf("KillAll errors: %v", errs)
		}
	})
}
