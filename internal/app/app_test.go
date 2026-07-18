package app

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"cablecheck/internal/config"
	"cablecheck/internal/testutil"
)

// minimalConfig returns the smallest RunConfig the lifecycle tests need.
func minimalConfig(role config.Role, controlPort uint16) *config.RunConfig {
	return &config.RunConfig{
		Role:        role,
		LocalIP:     netip.MustParseAddr("127.0.0.1"),
		PeerIP:      netip.MustParseAddr("127.0.0.1"),
		Mode:        config.ModeQuick,
		ControlPort: controlPort,
		Token:       "testtoken1234",
	}
}

// TestWaitAfterFailedStart pins the App lifecycle contract other packages
// build on: when Start fails (here: the control port is already taken), Wait
// must not deadlock — it reports the failure, mirroring os/exec.Cmd.Wait.
func TestWaitAfterFailedStart(t *testing.T) {
	defer testutil.LeakCheck(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	a, err := New(minimalConfig(config.RolePC1, port), Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Start(context.Background()); err == nil {
		t.Fatalf("Start succeeded on an occupied port")
	}

	done := make(chan struct{})
	var code ExitCode
	var werr error
	go func() {
		code, werr = a.Wait()
		close(done)
	}()
	testutil.WaitFor(t, done, "Wait deadlocked after a failed Start")
	if code != ExitConfig || werr == nil {
		t.Errorf("Wait = (%d, %v), want (4, the Start error)", code, werr)
	}
	var ee *ExitError
	if !errors.As(werr, &ee) || ee.Code != ExitConfig {
		t.Errorf("Wait error = %v, want an *ExitError with code 4", werr)
	}
}

// TestWaitBeforeStart: Wait without Start must fail fast instead of blocking
// on a channel nothing will ever close.
func TestWaitBeforeStart(t *testing.T) {
	defer testutil.LeakCheck(t)
	a, err := New(minimalConfig(config.RolePC1, 0), Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	var code ExitCode
	var werr error
	go func() {
		code, werr = a.Wait()
		close(done)
	}()
	testutil.WaitFor(t, done, "Wait deadlocked without Start")
	if werr == nil || code != ExitInternal {
		t.Errorf("Wait = (%d, %v), want (7, an error)", code, werr)
	}
}
