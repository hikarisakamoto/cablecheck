package testsuite

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

	"cablecheck/internal/runner/runnertest"

	"cablecheck/internal/model"
)

// fullSizeArgsPayload extracts the -s payload of the last ping call.
func fullSizeArgsPayload(t *testing.T, fr *runnertest.FakeRunner) string {
	t.Helper()
	calls := fr.CallsFor("ping")
	if len(calls) == 0 {
		t.Fatal("no ping call recorded")
	}
	args := calls[len(calls)-1].Args
	i := slices.Index(args, "-s")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("ping args %q lack -s", args)
	}
	return args[i+1]
}

// TestFullSizePayloadCalc pins that the full-size ping payload is always
// MTU-28 computed from the DISCOVERED interface MTU — never a hardcoded 1472
// — in both directions: PC1's own ping uses the plan's MTU, and the worker's
// op computes the payload from its own MTU (the coordinator only sends the
// count, because the two interfaces may have different MTUs).
func TestFullSizePayloadCalc(t *testing.T) {
	ctx := context.Background()

	t.Run("plan_both_directions", func(t *testing.T) {
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
			StdoutFile: fixturePath("ping", "fullsize_ok.txt")})
		rc := newFakeCaller(t)
		rc.reply(OpPingFullSize, &PingRunResult{Ping: model.PingResult{
			Transmitted: 100, Received: 100}})

		results := &SessionResults{}
		plan := newQuickPlan(newTestOps(t, fr), results)
		plan.MTU = 9000 // jumbo local MTU: payload must track it
		if err := plan.stepFullSizePing(ctx, rc); err != nil {
			t.Fatalf("stepFullSizePing: %v", err)
		}

		if got := fullSizeArgsPayload(t, fr); got != "8972" {
			t.Errorf("local -s payload = %q, want 8972 (MTU 9000 - 28), never a hardcoded 1472", got)
		}
		var p PingFullSizeParams
		if err := json.Unmarshal(rc.params[OpPingFullSize][0], &p); err != nil {
			t.Fatalf("unmarshal full-size params: %v", err)
		}
		if p.PeerIP != "10.0.0.1" {
			t.Errorf("remote full-size params ping %q, want the coordinator's 10.0.0.1", p.PeerIP)
		}
		if p.Count <= 0 {
			t.Errorf("remote full-size params count = %d, want positive", p.Count)
		}

		if len(results.FullSizePing) != 2 {
			t.Fatalf("results.FullSizePing has %d entries, want both directions", len(results.FullSizePing))
		}
		if results.FullSizePing[0].Direction != model.DirectionPC1ToPC2 ||
			results.FullSizePing[1].Direction != model.DirectionPC2ToPC1 {
			t.Errorf("full-size directions = %q/%q, want pc1_to_pc2 then pc2_to_pc1",
				results.FullSizePing[0].Direction, results.FullSizePing[1].Direction)
		}
	})

	t.Run("worker_op_uses_own_mtu", func(t *testing.T) {
		for _, tc := range []struct {
			mtu  int
			want string
		}{
			{1500, "1472"},
			{1600, "1572"}, // proves MTU-28 is computed, not the 1472 constant
		} {
			fr := runnertest.New(t)
			fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
				StdoutFile: fixturePath("ping", "fullsize_ok.txt")})
			ops := newTestOps(t, fr)
			ops.MTU = tc.mtu
			params, _ := json.Marshal(PingFullSizeParams{PeerIP: "10.0.0.1", Count: 100})
			res, status, err := ops.HandleOp(ctx, OpPingFullSize, params, discardProgress)
			if err != nil || status != StatusOK {
				t.Fatalf("HandleOp full-size (mtu %d) = status %q err %v", tc.mtu, status, err)
			}
			if _, ok := res.(*PingRunResult); !ok {
				t.Errorf("full-size result = %T, want *PingRunResult", res)
			}
			if got := fullSizeArgsPayload(t, fr); got != tc.want {
				t.Errorf("worker -s payload for MTU %d = %q, want %q", tc.mtu, got, tc.want)
			}
		}
	})
}

// TestFullSizeFragErrors pins that -M do fragmentation failures (EMSGSIZE:
// "local error: message too long") are surfaced as SendErrors — evidence of
// an MTU/fragmentation problem — distinctly from ordinary packet loss, and
// that the run is data, not a tooling error.
func TestFullSizeFragErrors(t *testing.T) {
	ctx := context.Background()
	fr := runnertest.New(t)
	fr.Script(runnertest.Script{Name: "ping", Match: runnertest.ArgsContain("-M", "do"),
		Result: fixture(t, "ping", "fullsize_emsgsize")})

	pt := &PingTester{R: fr}
	res, _, err := pt.FullSize(ctx, netip.MustParseAddr("10.0.0.2"), 9000, 100)
	if err != nil {
		t.Fatalf("FullSize: %v — frag errors are a measurement, not a failure", err)
	}
	if res.SendErrors != 100 {
		t.Errorf("SendErrors = %d, want 100 (one EMSGSIZE per probe)", res.SendErrors)
	}
	if res.IcmpErrors != 0 {
		t.Errorf("IcmpErrors = %d, want 0 — local frag errors are not ICMP errors", res.IcmpErrors)
	}
	if res.LossPercent != 100 || res.Received != 0 {
		t.Errorf("loss = %v%% with %d received, want 100%% / 0", res.LossPercent, res.Received)
	}
	if res.Transmitted != 100 {
		t.Errorf("Transmitted = %d, want 100 from the summary", res.Transmitted)
	}
}
