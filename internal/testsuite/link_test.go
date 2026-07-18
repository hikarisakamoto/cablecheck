package testsuite

import (
	"context"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/parser"
	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// reducedSpeedSettings is an ethtool output where both ends advertise
// 1000baseT/Full but the link negotiated only 100 Mb/s full duplex.
const reducedSpeedSettings = `Settings for eth0:
	Supported ports: [ TP ]
	Supported link modes:   10baseT/Half 10baseT/Full
	                        100baseT/Half 100baseT/Full
	                        1000baseT/Full
	Supports auto-negotiation: Yes
	Advertised link modes:  10baseT/Half 10baseT/Full
	                        100baseT/Half 100baseT/Full
	                        1000baseT/Full
	Advertised auto-negotiation: Yes
	Link partner advertised link modes:  10baseT/Half 10baseT/Full
	                                     100baseT/Half 100baseT/Full
	                                     1000baseT/Full
	Link partner advertised auto-negotiation: Yes
	Speed: 100Mb/s
	Duplex: Full
	Auto-negotiation: on
	Port: Twisted Pair
	Link detected: yes
`

// TestLinkTestFlags checks the link inspector's observations: a downed link,
// a half-duplex negotiation with conservative possible-cause wording, and a
// negotiated speed below what both ends advertise.
func TestLinkTestFlags(t *testing.T) {
	ctx := context.Background()

	inspect := func(t *testing.T, res runner.CommandResult, ifName string) ([]string, string) {
		t.Helper()
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact(ifName), Result: res})
		li := &LinkInspector{R: fr, IfName: ifName}
		settings, obs, err := li.Inspect(ctx)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if settings.Raw == nil {
			t.Errorf("Inspect returned settings without raw key/value map")
		}
		return obs, strings.ToLower(strings.Join(obs, "\n"))
	}

	t.Run("NoLink", func(t *testing.T) {
		obs, joined := inspect(t, fixture(t, "ethtool", "settings_no_link"), "enp0s31f6")
		if !strings.Contains(joined, "no link") {
			t.Errorf("observations %q lack a 'no link' observation", obs)
		}
	})

	t.Run("HalfDuplex", func(t *testing.T) {
		obs, joined := inspect(t, fixture(t, "ethtool", "settings_r8152_100m_half"), "enp0s20f0u3")
		if !strings.Contains(joined, "half duplex") {
			t.Errorf("observations %q lack a half-duplex observation", obs)
		}
		if !strings.Contains(joined, "possible causes") {
			t.Errorf("observations %q must hedge with conservative 'possible causes' wording", obs)
		}
	})

	t.Run("ReducedSpeed", func(t *testing.T) {
		res := runner.CommandResult{Stdout: []byte(reducedSpeedSettings)}
		obs, joined := inspect(t, res, "eth0")
		if !strings.Contains(joined, "100 mb/s") || !strings.Contains(joined, "1000 mb/s") {
			t.Errorf("observations %q must cite negotiated 100 Mb/s vs mutually advertised 1000 Mb/s", obs)
		}
		if !strings.Contains(joined, "reduced") {
			t.Errorf("observations %q lack reduced-speed evidence wording", obs)
		}
	})

	t.Run("CleanGigabitHasNoAlarms", func(t *testing.T) {
		_, joined := inspect(t, fixture(t, "ethtool", "settings_e1000e_1g"), "enp0s31f6")
		for _, bad := range []string{"no link", "half duplex", "reduced"} {
			if strings.Contains(joined, bad) {
				t.Errorf("clean 1G link produced observation containing %q: %q", bad, joined)
			}
		}
	})

	t.Run("InspectHasTimeoutBackstop", func(t *testing.T) {
		// The coordinator's local half runs Inspect under the long-lived
		// plan ctx, so the spec needs its own bound against wedged ethtool
		// ioctls (real with buggy NIC drivers).
		fr := runnertest.New(t)
		fr.Script(runnertest.Script{Name: "ethtool", Match: runnertest.ArgsExact("eth0"),
			Result: fixture(t, "ethtool", "settings_e1000e_1g")})
		li := &LinkInspector{R: fr, IfName: "eth0"}
		if _, _, err := li.Inspect(ctx); err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if got := fr.Calls()[0].Spec.Timeout; got != 10*time.Second {
			t.Errorf("ethtool eth0 Spec.Timeout = %v, want the 10s backstop", got)
		}
	})

	t.Run("ObserveLinkIsPure", func(t *testing.T) {
		ls := parser.ParseEthtoolSettings([]byte(reducedSpeedSettings))
		if got := ObserveLink(ls); len(got) == 0 {
			t.Errorf("ObserveLink on reduced-speed settings returned no observations")
		}
	})
}
