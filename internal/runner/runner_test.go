package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/testutil"
)

// TestMain implements the helper-process pattern: when GO_HELPER_MODE is set
// the test binary acts as a scripted child process instead of running tests.
// Tests re-exec os.Executable() with that variable set, so no external tools
// and no shell are ever involved.
func TestMain(m *testing.M) {
	mode := os.Getenv("GO_HELPER_MODE")
	if mode == "" {
		os.Exit(m.Run())
	}
	os.Exit(runHelper(mode, os.Args[1:]))
}

// runHelper dispatches the helper behaviors used by the real-runner tests.
func runHelper(mode string, args []string) int {
	switch mode {
	case "echo-args":
		// Echo each argument verbatim on its own line (stdout) and a
		// summary including the injected locale variables (stderr).
		for _, a := range args {
			fmt.Println(a)
		}
		fmt.Fprintf(os.Stderr, "args:%d LC_ALL=%s LANG=%s\n",
			len(args), os.Getenv("LC_ALL"), os.Getenv("LANG"))
		return 0

	case "exit-n":
		n, err := strconv.Atoi(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad exit code %q: %v\n", args[0], err)
			return 125
		}
		return n

	case "emit-big-output":
		if _, err := os.Stdout.Write(bigOutput()); err != nil {
			return 125
		}
		fmt.Fprint(os.Stderr, "big done\n")
		return 0

	case "sleep-forever":
		// Not select{}: with no runnable goroutines the runtime deadlock
		// detector would exit(2) instead of blocking.
		for {
			time.Sleep(time.Hour)
		}

	case "trap-sigterm-exit-42":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		fmt.Println("ready")
		<-ch
		return 42

	case "ignore-sigterm":
		// SIGTERM is ignored outright, so only the SIGKILL escalation can
		// end this process — the coverage target for grace-expiry tests.
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}

	case "spawn-pipe-holder":
		// Spawn a grandchild that inherits our stdout/stderr pipe write
		// ends and holds them open without writing, then exit immediately:
		// the parent runner's cmd.Wait can then only be unblocked by the
		// WaitDelay pipe backstop (ErrWaitDelay).
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "os.Executable: %v\n", err)
			return 125
		}
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), "GO_HELPER_MODE=pipe-holder")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "spawn pipe holder: %v\n", err)
			return 125
		}
		return 0

	case "pipe-holder":
		// Hold the inherited pipes open, writing nothing. Bounded (~60s)
		// so a broken backstop cannot orphan it past the test run.
		for i := 0; i < 600; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		return 0

	case "spawn-grandchild-heartbeats":
		// Spawn a grandchild (same binary, heartbeat-writer mode) in
		// the SAME process group (no Setpgid here), print a sync line,
		// then block forever. The test SIGKILLs our process group and
		// asserts the grandchild died with us.
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "os.Executable: %v\n", err)
			return 125
		}
		cmd := exec.Command(exe, args[0])
		cmd.Env = append(os.Environ(), "GO_HELPER_MODE=heartbeat-writer")
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "spawn grandchild: %v\n", err)
			return 125
		}
		fmt.Println("spawned")
		for {
			time.Sleep(time.Hour)
		}

	case "heartbeat-writer":
		// Append one line every 10ms. Bounded at ~30s so a broken
		// group-kill implementation cannot orphan a writer forever.
		for i := 0; i < 3000; i++ {
			f, err := os.OpenFile(args[0], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return 125
			}
			fmt.Fprintln(f, "beat")
			f.Close()
			time.Sleep(10 * time.Millisecond)
		}
		return 0

	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		return 127
	}
}

// bigOutput returns the deterministic 1 MiB payload emitted by the
// emit-big-output helper: 16384 lines of 64 bytes each.
func bigOutput() []byte {
	var b bytes.Buffer
	for i := 0; i < 16384; i++ {
		fmt.Fprintf(&b, "%063d\n", i)
	}
	return b.Bytes()
}

// helperSpec builds a CommandSpec that re-executes this test binary in the
// given helper mode.
func helperSpec(t testing.TB, mode string, args ...string) CommandSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return CommandSpec{
		Name:  exe,
		Args:  args,
		Env:   []string{"GO_HELPER_MODE=" + mode},
		Label: "helper-" + mode,
	}
}

func newTestRunner() *Exec { return New(clock.Real{}) }

// startHelperReady starts spec via r and blocks until the helper's readiness
// line arrives on the live stdout stream — the no-sleep synchronization point
// proving the helper's signal disposition is installed.
func startHelperReady(t *testing.T, r *Exec, ctx context.Context, spec CommandSpec, want string) Process {
	t.Helper()
	p, err := r.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || line != want {
		t.Fatalf("live stdout line = %q, %v; want %q", line, err, want)
	}
	return p
}

// pollUntil is a bounded convergence poll (10ms steps) — sanctioned for
// process-shutdown observation only, never for scheduling synchronization.
func pollUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(testutil.TestTimeout(t))
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func TestRunCapturesOutput(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	res, err := r.Run(t.Context(), helperSpec(t, "echo-args", "hello", "world"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(res.Stdout), "hello\nworld\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	// Locale injection (LC_ALL=C, LANG=C, always) is asserted here too.
	if got, want := string(res.Stderr), "args:2 LC_ALL=C LANG=C\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.TimedOut || res.Signal != "" {
		t.Errorf("TimedOut=%v Signal=%q, want false/empty", res.TimedOut, res.Signal)
	}
	if res.Started.IsZero() {
		t.Error("Started is zero")
	}
	if res.Duration < 0 {
		t.Errorf("Duration = %v, want >= 0", res.Duration)
	}
}

func TestRunExitCode(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	res, err := r.Run(t.Context(), helperSpec(t, "exit-n", "3"))
	if err != nil {
		t.Fatalf("Run: %v (non-zero exit must be data, not error)", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !res.Failed() {
		t.Error("Failed() = false, want true")
	}
	if res.TimedOut {
		t.Error("TimedOut = true, want false")
	}
}

func TestRunMissingExecutable(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	res, err := r.Run(t.Context(), CommandSpec{Name: "cablecheck-definitely-not-here-xyz"})
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is(err, exec.ErrNotFound)", err)
	}
	if _, err := r.LookPath("cablecheck-definitely-not-here-xyz"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("LookPath err = %v, want errors.Is(err, exec.ErrNotFound)", err)
	}
}

func TestRunTimeoutVsFailure(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()

	t.Run("timeout", func(t *testing.T) {
		spec := helperSpec(t, "sleep-forever")
		spec.Timeout = 100 * time.Millisecond
		res, err := r.Run(t.Context(), spec)
		if res == nil {
			t.Fatal("result = nil, want non-nil result alongside timeout error")
		}
		if !res.TimedOut {
			t.Error("TimedOut = false, want true")
		}
		if !errors.Is(err, ErrTimeout) {
			t.Errorf("err = %v, want errors.Is(err, ErrTimeout)", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
		}
		if res.Signal != "SIGTERM" {
			t.Errorf("Signal = %q, want SIGTERM", res.Signal)
		}
		if res.ExitCode != -1 {
			t.Errorf("ExitCode = %d, want -1", res.ExitCode)
		}
	})

	t.Run("non-zero exit is not a timeout", func(t *testing.T) {
		res, err := r.Run(t.Context(), helperSpec(t, "exit-n", "3"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.TimedOut {
			t.Error("TimedOut = true, want false")
		}
		if res.ExitCode != 3 {
			t.Errorf("ExitCode = %d, want 3", res.ExitCode)
		}
	})
}

func TestRunMaxOutputTruncation(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	tee := filepath.Join(t.TempDir(), "raw-stdout.txt")
	spec := helperSpec(t, "emit-big-output")
	spec.MaxOutputBytes = 4096
	spec.TeeStdoutPath = tee
	res, err := r.Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.StdoutTruncated {
		t.Error("StdoutTruncated = false, want true")
	}
	if res.StderrTruncated {
		t.Error("StderrTruncated = true, want false")
	}
	full := bigOutput()
	if !bytes.HasPrefix(res.Stdout, full[:4096]) {
		t.Error("in-memory stdout does not start with the emitted stream")
	}
	if !strings.Contains(string(res.Stdout), "output truncated at 4096 bytes") {
		t.Errorf("stdout missing truncation marker; tail: %q", tail(res.Stdout, 120))
	}
	if int64(len(res.Stdout)) > 4096+256 {
		t.Errorf("in-memory stdout length = %d, want <= cap + marker", len(res.Stdout))
	}
	got, err := os.ReadFile(tee)
	if err != nil {
		t.Fatalf("read tee file: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("tee file has %d bytes, want the complete %d-byte stream", len(got), len(full))
	}
}

func TestRunNoShellInterpretation(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	args := []string{"a b", "$(reboot)", ";", "&&"}
	res, err := r.Run(t.Context(), helperSpec(t, "echo-args", args...))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "a b\n$(reboot)\n;\n&&\n"
	if got := string(res.Stdout); got != want {
		t.Errorf("stdout = %q, want args echoed verbatim %q", got, want)
	}
}

func TestStartTerminateDeliversSIGTERM(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	p, err := r.Start(t.Context(), helperSpec(t, "trap-sigterm-exit-42"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.PID() <= 0 {
		t.Errorf("PID = %d, want > 0", p.PID())
	}
	// The helper prints "ready" only after installing its SIGTERM handler;
	// reading it from the live stream is the synchronization point.
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("live stdout line = %q, %v; want \"ready\\n\"", line, err)
	}
	select {
	case <-p.Done():
		t.Fatal("Done() closed before Terminate")
	default:
	}
	if err := p.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	res, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42 (helper traps SIGTERM)", res.ExitCode)
	}
	if res.Signal != "" {
		t.Errorf("Signal = %q, want empty (normal exit)", res.Signal)
	}
	select {
	case <-p.Done():
	default:
		t.Error("Done() not closed after Wait returned")
	}
	// Wait is idempotent.
	res2, err2 := p.Wait(t.Context())
	if err2 != nil || res2.ExitCode != 42 {
		t.Errorf("second Wait = (%+v, %v), want same result", res2, err2)
	}
}

func TestContextCancelKillsChild(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	ctx, cancel := context.WithCancel(t.Context())
	p, err := r.Start(ctx, helperSpec(t, "sleep-forever"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	// Bounded by t.Context (test deadline), no sleeps: the runner must kill
	// the child and Wait must return promptly.
	res, err := p.Wait(t.Context())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, must NOT be ErrTimeout (parent cancel is not a timeout)", err)
	}
	if res == nil {
		t.Fatal("result = nil, want partial result on cancel")
	}
	if res.TimedOut {
		t.Error("TimedOut = true, want false (attribution must not come from ctx.Err alone)")
	}
	if res.Signal != "SIGTERM" {
		t.Errorf("Signal = %q, want SIGTERM", res.Signal)
	}
}

func TestEscalationSIGKILLAfterGrace(t *testing.T) {
	t.Run("parent cancel", func(t *testing.T) {
		testutil.LeakCheck(t)
		fc := clocktest.New(time.Unix(1_700_000_000, 0))
		r := New(fc)
		ctx, cancel := context.WithCancel(t.Context())
		spec := helperSpec(t, "ignore-sigterm")
		// The real-time WaitDelay backstop (GracePeriod + 2s of wall time)
		// must stay far away so only the escalation goroutine can end this
		// child; the elapsed guard below proves the backstop never ran.
		spec.GracePeriod = 30 * time.Second
		begin := time.Now()
		p := startHelperReady(t, r, ctx, spec, "ready\n")
		cancel()
		// cmd.Cancel SIGTERMs the group (ignored) and arms the escalation
		// timer on the fake clock; park until it exists, then expire it.
		fc.BlockUntilWaiters(1)
		fc.Advance(spec.GracePeriod)
		res, err := p.Wait(t.Context())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, ErrTimeout) {
			t.Errorf("err = %v, must NOT be ErrTimeout (parent cancel is not a timeout)", err)
		}
		if res == nil {
			t.Fatal("result = nil, want partial result on cancel")
		}
		if res.TimedOut {
			t.Error("TimedOut = true, want false")
		}
		if res.Signal != "SIGKILL" {
			t.Errorf("Signal = %q, want SIGKILL (SIGTERM is ignored; only the escalation can kill)", res.Signal)
		}
		if res.ExitCode != -1 {
			t.Errorf("ExitCode = %d, want -1", res.ExitCode)
		}
		if elapsed := time.Since(begin); elapsed >= spec.GracePeriod {
			t.Errorf("Wait took %v, want far under %v: the kill must come from the escalation timer, not the WaitDelay backstop", elapsed, spec.GracePeriod)
		}
	})

	t.Run("spec timeout", func(t *testing.T) {
		testutil.LeakCheck(t)
		fc := clocktest.New(time.Unix(1_700_000_000, 0))
		r := New(fc)
		spec := helperSpec(t, "ignore-sigterm")
		// Real-time timeout: the ready-line handshake below completes long
		// before it fires, so the SIGTERM is guaranteed to be ignored.
		spec.Timeout = 2 * time.Second
		spec.GracePeriod = 30 * time.Second
		begin := time.Now()
		p := startHelperReady(t, r, t.Context(), spec, "ready\n")
		// Parks until the timeout fired and cmd.Cancel armed the timer.
		fc.BlockUntilWaiters(1)
		fc.Advance(spec.GracePeriod)
		res, err := p.Wait(t.Context())
		if !errors.Is(err, ErrTimeout) {
			t.Errorf("err = %v, want errors.Is(err, ErrTimeout)", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
		}
		if res == nil {
			t.Fatal("result = nil, want partial result on timeout")
		}
		if !res.TimedOut {
			t.Error("TimedOut = false, want true (attribution must survive the SIGKILL escalation)")
		}
		if res.Signal != "SIGKILL" {
			t.Errorf("Signal = %q, want SIGKILL (SIGTERM is ignored; only the escalation can kill)", res.Signal)
		}
		if elapsed := time.Since(begin); elapsed >= spec.GracePeriod {
			t.Errorf("Wait took %v, want far under %v: the kill must come from the escalation timer, not the WaitDelay backstop", elapsed, spec.GracePeriod)
		}
	})
}

func TestWaitDelayBackstopGrandchildHoldsPipe(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner() // real clock: WaitDelay is a real-time os/exec mechanism
	spec := helperSpec(t, "spawn-pipe-holder")
	spec.GracePeriod = 50 * time.Millisecond // WaitDelay = GracePeriod + 2s of wall time
	p, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The grandchild inherits the pipes and never writes; the parent exits 0
	// immediately. Only the WaitDelay backstop can unblock the reap, and per
	// the contract exec.ErrWaitDelay is data: the exit status is still valid.
	res, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v (ErrWaitDelay must be treated as data, not an error)", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (the parent exited normally)", res.ExitCode)
	}
	if res.Signal != "" {
		t.Errorf("Signal = %q, want empty", res.Signal)
	}
	if res.TimedOut {
		t.Error("TimedOut = true, want false")
	}
	// Reap the still-sleeping grandchild: it anchors the process group, so
	// the negative-pgid SIGKILL cannot hit a reused id. ESRCH is fine.
	_ = syscall.Kill(-p.PID(), syscall.SIGKILL)
}

func TestProcessGroupKill(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	hb := filepath.Join(t.TempDir(), "heartbeats")
	spec := helperSpec(t, "spawn-grandchild-heartbeats", hb)
	spec.GracePeriod = 500 * time.Millisecond
	p, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || line != "spawned\n" {
		t.Fatalf("live stdout line = %q, %v; want \"spawned\\n\"", line, err)
	}
	// Wait until the grandchild demonstrably writes heartbeats.
	pollUntil(t, func() bool { return fileSize(hb) >= 10 }, "grandchild heartbeats")

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	res, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Signal != "SIGKILL" {
		t.Errorf("Signal = %q, want SIGKILL", res.Signal)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}

	// The grandchild shares the process group (Setpgid on the parent, no
	// new group for the grandchild), so kill(-pgid, SIGKILL) must have
	// taken it down too: the heartbeat file must stop growing. Bounded
	// 10ms polling; 50 consecutive stable samples (~500ms >> the 10ms
	// heartbeat interval) means the writer is dead.
	deadline := time.Now().Add(testutil.TestTimeout(t))
	var prev int64 = -1
	stable := 0
	for stable < 50 {
		if !time.Now().Before(deadline) {
			t.Fatal("heartbeat file still growing after group kill (grandchild survived)")
		}
		cur := fileSize(hb)
		if cur == prev {
			stable++
		} else {
			stable = 0
			prev = cur
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRawStreamTee(t *testing.T) {
	testutil.LeakCheck(t)
	r := newTestRunner()
	dir := t.TempDir()
	outTee := filepath.Join(dir, "out.raw")
	errTee := filepath.Join(dir, "err.raw")
	spec := helperSpec(t, "emit-big-output")
	spec.MaxOutputBytes = 1024
	spec.TeeStdoutPath = outTee
	spec.TeeStderrPath = errTee
	res, err := r.Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.StdoutTruncated {
		t.Error("StdoutTruncated = false, want true")
	}
	gotOut, err := os.ReadFile(outTee)
	if err != nil {
		t.Fatalf("read stdout tee: %v", err)
	}
	if full := bigOutput(); !bytes.Equal(gotOut, full) {
		t.Errorf("stdout tee has %d bytes, want complete %d-byte stream despite in-memory cap", len(gotOut), len(full))
	}
	gotErr, err := os.ReadFile(errTee)
	if err != nil {
		t.Fatalf("read stderr tee: %v", err)
	}
	if want := "big done\n"; string(gotErr) != want {
		t.Errorf("stderr tee = %q, want %q", gotErr, want)
	}
	if res.StderrTruncated {
		t.Error("StderrTruncated = true, want false")
	}
}

// tail returns the last n bytes of b for error messages.
func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
