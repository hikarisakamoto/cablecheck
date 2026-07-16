package runnertest_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
	"cablecheck/internal/testutil"
)

// startProcess starts one scripted process and returns its FakeProcess handle.
func startProcess(t *testing.T, f *runnertest.FakeRunner, s runnertest.Script, spec runner.CommandSpec) *runnertest.FakeProcess {
	t.Helper()
	f.Script(s)
	if _, err := f.Start(t.Context(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	procs := f.Processes()
	return procs[len(procs)-1]
}

func TestFakeProcessWaitBlocksUntilExit(t *testing.T) {
	testutil.LeakCheck(t)
	f := runnertest.New(t)
	p := startProcess(t, f, runnertest.Script{Name: "iperf3", Match: runnertest.AnyArgs()},
		runSpec("iperf3", "-s"))

	if p.PID() <= 0 {
		t.Errorf("PID = %d, want > 0", p.PID())
	}
	type outcome struct {
		res *runner.CommandResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := p.Wait(t.Context())
		done <- outcome{res, err}
	}()
	select {
	case out := <-done:
		t.Fatalf("Wait returned (%+v, %v) before Exit", out.res, out.err)
	case <-p.Done():
		t.Fatal("Done() closed before Exit")
	default:
	}

	p.Exit(runner.CommandResult{ExitCode: 7, Stdout: []byte("done\n")})
	out := <-done
	if out.err != nil {
		t.Fatalf("Wait: %v", out.err)
	}
	if out.res.ExitCode != 7 || string(out.res.Stdout) != "done\n" {
		t.Errorf("Wait result = %+v, want scripted exit", out.res)
	}
	testutil.WaitFor(t, p.Done(), "Done() to close after Exit")

	// Idempotent: a second Wait returns the same outcome immediately.
	res2, err2 := p.Wait(t.Context())
	if err2 != nil || res2.ExitCode != 7 {
		t.Errorf("second Wait = (%+v, %v), want same result", res2, err2)
	}
}

func TestFakeProcessWaitCtxCancel(t *testing.T) {
	testutil.LeakCheck(t)
	f := runnertest.New(t)
	p := startProcess(t, f, runnertest.Script{Name: "iperf3", Match: runnertest.AnyArgs()},
		runSpec("iperf3", "-s"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait err = %v, want errors.Is(err, context.Canceled)", err)
	}
	// Process is still alive; a later Exit still resolves Wait.
	p.Exit(runner.CommandResult{ExitCode: 0})
	if res, err := p.Wait(t.Context()); err != nil || res.ExitCode != 0 {
		t.Errorf("Wait after Exit = (%+v, %v), want exit 0", res, err)
	}
}

func TestFakeProcessTerminateAndKill(t *testing.T) {
	testutil.LeakCheck(t)
	f := runnertest.New(t)
	banner := []byte("Server listening on 5201\n")

	p1 := startProcess(t, f, runnertest.Script{Name: "iperf3", Match: runnertest.ArgsContain("-s"),
		Result: runner.CommandResult{Stdout: banner}}, runSpec("iperf3", "-s", "-1"))
	select {
	case <-p1.TermCh():
		t.Fatal("TermCh closed before Terminate")
	default:
	}
	if p1.Terminated() {
		t.Error("Terminated() = true before Terminate")
	}
	if err := p1.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	testutil.WaitFor(t, p1.TermCh(), "TermCh to close")
	if !p1.Terminated() {
		t.Error("Terminated() = false after Terminate")
	}
	res, err := p1.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.ExitCode != -1 || res.Signal != "SIGTERM" {
		t.Errorf("Terminate result = exit %d signal %q, want -1/SIGTERM", res.ExitCode, res.Signal)
	}
	if string(res.Stdout) != string(banner) {
		t.Errorf("result stdout = %q, want the scripted stream %q", res.Stdout, banner)
	}
	// Kill after exit observably fires KillCh but cannot change the result.
	if err := p1.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	testutil.WaitFor(t, p1.KillCh(), "KillCh to close")
	if res2, _ := p1.Wait(t.Context()); res2.Signal != "SIGTERM" {
		t.Errorf("result changed after post-exit Kill: signal %q, want SIGTERM", res2.Signal)
	}

	p2 := startProcess(t, f, runnertest.Script{Name: "iperf3", Match: runnertest.AnyArgs()},
		runSpec("iperf3", "-s"))
	if err := p2.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !p2.Killed() {
		t.Error("Killed() = false after Kill")
	}
	res, err = p2.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.ExitCode != -1 || res.Signal != "SIGKILL" {
		t.Errorf("Kill result = exit %d signal %q, want -1/SIGKILL", res.ExitCode, res.Signal)
	}
}

func TestFakeProcessScriptedStdoutStream(t *testing.T) {
	testutil.LeakCheck(t)
	f := runnertest.New(t)
	banner := "Server listening on 5201\n"
	p := startProcess(t, f, runnertest.Script{Name: "iperf3", Match: runnertest.AnyArgs(),
		Result: runner.CommandResult{Stdout: []byte(banner)}}, runSpec("iperf3", "-s", "-1"))

	// Readiness scan: the scripted content is readable line by line while
	// the process is still "running".
	br := bufio.NewReader(p.Stdout())
	line, err := br.ReadString('\n')
	if err != nil || line != banner {
		t.Fatalf("live stdout line = %q, %v; want the banner", line, err)
	}

	// After Exit the stream drains to EOF instead of blocking forever.
	p.Exit(runner.CommandResult{ExitCode: 0})
	if _, err := br.ReadString('\n'); !errors.Is(err, io.EOF) {
		t.Errorf("read after exit = %v, want io.EOF", err)
	}
}

func TestFakeProcessDelayAutoExit(t *testing.T) {
	testutil.LeakCheck(t)
	f := runnertest.New(t)
	started := make(chan struct{})
	delay := make(chan struct{})
	p := startProcess(t, f, runnertest.Script{Name: "iperf3", Match: runnertest.AnyArgs(),
		Started: started, Delay: delay,
		Result: runner.CommandResult{ExitCode: 0, Stdout: []byte("server output\n")}},
		runSpec("iperf3", "-s", "-1"))

	testutil.WaitFor(t, started, "Started to close on Start call")
	select {
	case <-p.Done():
		t.Fatal("process exited while Delay channel still open")
	default:
	}
	close(delay)
	res, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "server output\n" {
		t.Errorf("auto-exit result = %+v, want the scripted result", res)
	}
}
