package app

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"cablecheck/internal/config"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

func scriptInferAddr(fr *runnertest.FakeRunner, json string) {
	fr.Script(runnertest.Script{
		Name:   "ip",
		Match:  runnertest.ArgsExact("-j", "addr", "show"),
		Result: runner.CommandResult{Stdout: []byte(json)},
	})
}

func inferApp(t *testing.T, fr *runnertest.FakeRunner, localIP string) *App {
	t.Helper()
	cfg := &config.RunConfig{
		Role:        config.RolePC1,
		PeerIP:      netip.MustParseAddr("10.0.0.2"),
		Mode:        config.ModeQuick,
		Token:       "tok",
		Interface:   "enp3s0",
		ControlPort: 44300,
	}
	if localIP != "" {
		cfg.LocalIP = netip.MustParseAddr(localIP)
	}
	a, err := New(cfg, Deps{Runner: fr, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestEnsureLocalIP(t *testing.T) {
	t.Run("infers from interface", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptInferAddr(fr, `[{"ifname":"enp3s0","link_type":"ether","addr_info":[{"family":"inet","local":"10.0.0.1","prefixlen":24}]}]`)
		a := inferApp(t, fr, "")
		if err := a.ensureLocalIP(context.Background()); err != nil {
			t.Fatalf("ensureLocalIP: %v", err)
		}
		if a.cfg.LocalIP != netip.MustParseAddr("10.0.0.1") {
			t.Errorf("LocalIP = %v, want 10.0.0.1", a.cfg.LocalIP)
		}
	})

	t.Run("idempotent when local-ip already set", func(t *testing.T) {
		fr := runnertest.New(t) // no ip script: a valid LocalIP must not probe
		a := inferApp(t, fr, "10.0.0.9")
		if err := a.ensureLocalIP(context.Background()); err != nil {
			t.Fatalf("ensureLocalIP: %v", err)
		}
		if a.cfg.LocalIP != netip.MustParseAddr("10.0.0.9") {
			t.Errorf("LocalIP = %v, want it unchanged", a.cfg.LocalIP)
		}
	})

	t.Run("rejects an inferred IP equal to peer-ip", func(t *testing.T) {
		fr := runnertest.New(t)
		scriptInferAddr(fr, `[{"ifname":"enp3s0","link_type":"ether","addr_info":[{"family":"inet","local":"10.0.0.2","prefixlen":24}]}]`)
		a := inferApp(t, fr, "")
		err := a.ensureLocalIP(context.Background())
		if err == nil || !strings.Contains(err.Error(), "equals --peer-ip") {
			t.Errorf("err = %v, want a peer-collision error", err)
		}
	})
}

// TestStartInfersLocalIPBeforeBind proves the PC1 inference runs before the
// listener bind: a bogus interface makes Start fail with the inference error
// (ExitConfig), not an opaque bind error on the zero address.
func TestStartInfersLocalIPBeforeBind(t *testing.T) {
	fr := runnertest.New(t)
	scriptInferAddr(fr, `[{"ifname":"other0","link_type":"ether","addr_info":[]}]`)
	a := inferApp(t, fr, "")

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded with a bogus --interface, want an inference error")
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitConfig {
		t.Fatalf("Start err = %v, want ExitError{ExitConfig}", err)
	}
	if !strings.Contains(err.Error(), "enp3s0") {
		t.Errorf("Start err = %v, want the interface-not-found inference error", err)
	}
}
