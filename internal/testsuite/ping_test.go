package testsuite

import (
	"context"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/runner/runnertest"
)

// TestPingLadder exercises the interval fallback ladder (0.02 denied → 0.2
// succeeds), the recorded IntervalUsed plus limitation note, and the exact
// argument shapes of the quick and full-size invocations.
func TestPingLadder(t *testing.T) {
	ctx := context.Background()
	peer := netip.MustParseAddr("10.0.0.2")

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-i", "0.02"),
		Result: fixture(t, "ping", "interval_denied_new"), Times: 1})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-i", "0.2"),
		Result: fixture(t, "ping", "quick_clean_100"), Times: 1})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		Result: fixture(t, "ping", "quick_clean_100")})

	pt := &PingTester{R: fr}

	res, notes, err := pt.Quick(ctx, peer, 100)
	if err != nil {
		t.Fatalf("Quick: %v", err)
	}
	if res.IntervalUsedSec != 0.2 {
		t.Errorf("IntervalUsedSec = %v, want 0.2 after the denied 0.02 rung", res.IntervalUsedSec)
	}
	if len(notes) == 0 || !strings.Contains(strings.ToLower(strings.Join(notes, "\n")), "interval") {
		t.Errorf("limitation notes = %q, want a note about the degraded interval", notes)
	}
	if res.Received != 100 {
		t.Errorf("Received = %d, want 100 from the clean fixture", res.Received)
	}

	calls := fr.CallsFor("ping")
	if len(calls) != 2 {
		t.Fatalf("got %d ping calls after Quick, want 2 (denied + retry)", len(calls))
	}
	for i, call := range calls {
		for _, tok := range []string{"-n", "-D"} {
			if !slices.Contains(call.Args, tok) {
				t.Errorf("quick call %d args %q lack %s", i, call.Args, tok)
			}
		}
		if slices.Contains(call.Args, "-M") {
			t.Errorf("quick call %d args %q must not use -M do", i, call.Args)
		}
		if call.Args[len(call.Args)-1] != peer.String() {
			t.Errorf("quick call %d args %q must end with the peer address", i, call.Args)
		}
	}
	if !slices.Contains(calls[0].Args, "0.02") {
		t.Errorf("first quick attempt args %q must request -i 0.02", calls[0].Args)
	}
	if !slices.Contains(calls[1].Args, "0.2") {
		t.Errorf("retry args %q must request -i 0.2", calls[1].Args)
	}

	fres, _, err := pt.FullSize(ctx, peer, 1500, 100)
	if err != nil {
		t.Fatalf("FullSize: %v", err)
	}
	if fres.Transmitted != 100 {
		t.Errorf("FullSize Transmitted = %d, want 100", fres.Transmitted)
	}
	calls = fr.CallsFor("ping")
	full := calls[len(calls)-1].Args
	for _, tok := range []string{"-n", "-D", "-M", "do", "-s", "1472"} {
		if !slices.Contains(full, tok) {
			t.Errorf("full-size args %q lack %s", full, tok)
		}
	}
	if i := slices.Index(full, "-s"); i < 0 || i+1 >= len(full) || full[i+1] != "1472" {
		t.Errorf("full-size args %q: -s payload must be MTU-28 = 1472, computed not hardcoded", full)
	}
	if i := slices.Index(full, "-M"); i < 0 || i+1 >= len(full) || full[i+1] != "do" {
		t.Errorf("full-size args %q: want -M do (don't fragment)", full)
	}

	// The MTU-28 payload must track the MTU, never a constant 1472.
	fr2 := runnertest.New(t)
	fr2.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		Result: fixture(t, "ping", "quick_clean_100")})
	pt2 := &PingTester{R: fr2}
	if _, _, err := pt2.FullSize(ctx, peer, 9000, 100); err != nil {
		t.Fatalf("FullSize mtu 9000: %v", err)
	}
	jumbo := fr2.CallsFor("ping")[0].Args
	if !slices.Contains(jumbo, "8972") {
		t.Errorf("full-size args %q for MTU 9000 must carry -s 8972", jumbo)
	}
}

func TestFullSizePayloadClampsIPv4Maximum(t *testing.T) {
	peer := netip.MustParseAddr("10.0.0.2")
	for _, tc := range []struct {
		name        string
		mtu         int
		wantPayload string
	}{
		{name: "standard MTU", mtu: 1500, wantPayload: "1472"},
		{name: "loopback MTU", mtu: 65536, wantPayload: "65507"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := runnertest.New(t)
			fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
				Result: fixture(t, "ping", "quick_clean_100")})
			pt := &PingTester{R: fr}

			if _, _, err := pt.FullSize(context.Background(), peer, tc.mtu, 100); err != nil {
				t.Fatalf("FullSize MTU %d: %v", tc.mtu, err)
			}
			args := fr.CallsFor("ping")[0].Args
			i := slices.Index(args, "-s")
			if i < 0 || i+1 >= len(args) {
				t.Errorf("FullSize MTU %d args %q lack a -s payload", tc.mtu, args)
				return
			}
			if args[i+1] != tc.wantPayload {
				t.Errorf("FullSize MTU %d payload = %s, want %s", tc.mtu, args[i+1], tc.wantPayload)
			}
		})
	}
}

// TestPingRunnerTimeout pins the runner-timeout backstop: every ping
// invocation (each quick-ladder rung and the full-size run) bounds its
// runtime to its own -w wall-clock budget plus 10 seconds of slack, so a
// wedged ping cannot hang a session that runs under a long-lived context.
func TestPingRunnerTimeout(t *testing.T) {
	ctx := context.Background()
	peer := netip.MustParseAddr("10.0.0.2")

	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-i", "0.02"),
		Result: fixture(t, "ping", "interval_denied_new"), Times: 1})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		Result: fixture(t, "ping", "quick_clean_100")})
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-i", "0.2"),
		Result: fixture(t, "ping", "quick_clean_100")})

	pt := &PingTester{R: fr}
	if _, _, err := pt.Quick(ctx, peer, 100); err != nil {
		t.Fatalf("Quick: %v", err)
	}
	if _, _, err := pt.FullSize(ctx, peer, 1500, 100); err != nil {
		t.Fatalf("FullSize: %v", err)
	}

	calls := fr.CallsFor("ping")
	if len(calls) != 3 {
		t.Fatalf("got %d ping calls, want 3 (denied rung + retry + full-size)", len(calls))
	}
	for i, call := range calls {
		wi := slices.Index(call.Args, "-w")
		if wi < 0 || wi+1 >= len(call.Args) {
			t.Fatalf("call %d args %q lack a -w wall-clock bound", i, call.Args)
		}
		wSec, err := strconv.Atoi(call.Args[wi+1])
		if err != nil {
			t.Fatalf("call %d args %q: -w value not an integer: %v", i, call.Args, err)
		}
		want := time.Duration(wSec+10) * time.Second
		if call.Spec.Timeout != want {
			t.Errorf("call %d (%q): Spec.Timeout = %v, want %v (-w %ds + 10s backstop)",
				i, call.Args, call.Spec.Timeout, want, wSec)
		}
	}
}
