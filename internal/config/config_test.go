package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"cablecheck/internal/model"
)

// validRaw returns a RawRunFlags baseline that passes every validation rule,
// so each test can break exactly one thing.
func validRaw(t *testing.T) RawRunFlags {
	t.Helper()
	return RawRunFlags{
		Role:        "pc1",
		LocalIP:     "192.168.50.10",
		PeerIP:      "192.168.50.11",
		Mode:        "quick",
		ControlPort: 44300,
		IperfPort:   44301,
		Token:       "correct-horse-battery",
		Output:      t.TempDir(),
	}
}

// wantValidationError asserts that err is a *ValidationError for the given
// flag whose message contains the given substring (empty = any message).
func wantValidationError(t *testing.T, err error, flag, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Resolve() error = nil, want *ValidationError for %s", flag)
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Resolve() error = %v (type %T), want *ValidationError", err, err)
	}
	if verr.Flag != flag {
		t.Errorf("ValidationError.Flag = %q, want %q (msg: %q)", verr.Flag, flag, verr.Msg)
	}
	if contains != "" && !strings.Contains(verr.Msg, contains) {
		t.Errorf("ValidationError.Msg = %q, want substring %q", verr.Msg, contains)
	}
}

func TestValidationErrorFormat(t *testing.T) {
	err := &ValidationError{Flag: "--local-ip", Msg: "boom"}
	if got, want := err.Error(), "--local-ip: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestValidateIPs(t *testing.T) {
	t.Run("valid pair accepted", func(t *testing.T) {
		cfg, err := Resolve(validRaw(t), nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if want := netip.MustParseAddr("192.168.50.10"); cfg.LocalIP != want {
			t.Errorf("LocalIP = %v, want %v", cfg.LocalIP, want)
		}
		if want := netip.MustParseAddr("192.168.50.11"); cfg.PeerIP != want {
			t.Errorf("PeerIP = %v, want %v", cfg.PeerIP, want)
		}
	})

	t.Run("v4-mapped IPv6 unmapped before Is4", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "::ffff:192.168.50.10"
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil (mapped address must be unmapped)", err)
		}
		if want := netip.MustParseAddr("192.168.50.10"); cfg.LocalIP != want {
			t.Errorf("LocalIP = %v, want unmapped %v", cfg.LocalIP, want)
		}
		if !cfg.LocalIP.Is4() {
			t.Errorf("LocalIP.Is4() = false, want true after Unmap")
		}
	})

	t.Run("IPv6 local rejected with support message", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "fe80::1"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--local-ip", "IPv6 is not supported yet")
	})

	t.Run("IPv6 peer rejected with support message", func(t *testing.T) {
		raw := validRaw(t)
		raw.PeerIP = "2001:db8::1"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--peer-ip", "IPv6 is not supported yet")
	})

	t.Run("zoned address rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "fe80::1%eth0"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--local-ip", "zone")
	})

	t.Run("unspecified rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "0.0.0.0"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--local-ip", "0.0.0.0")
	})

	t.Run("multicast rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.PeerIP = "224.0.0.1"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--peer-ip", "multicast")
	})

	t.Run("leading zero octets rejected clearly", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "192.168.001.010"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--local-ip", "leading zero")
	})

	t.Run("garbage rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "not-an-ip"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--local-ip", "not a valid IPv4 address")
	})

	t.Run("empty local-ip required", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = ""
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--local-ip", "required")
	})

	t.Run("loopback rejected without allow-virtual-interface", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "127.0.0.1"
		raw.PeerIP = "127.0.0.2"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--local-ip", "--allow-virtual-interface")
	})

	t.Run("loopback allowed with allow-virtual-interface", func(t *testing.T) {
		raw := validRaw(t)
		raw.LocalIP = "127.0.0.1"
		raw.PeerIP = "127.0.0.2"
		raw.AllowVirtualInterface = true
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if !cfg.AllowVirtualInterface {
			t.Errorf("AllowVirtualInterface = false, want true")
		}
	})

	t.Run("local equals peer rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.PeerIP = raw.LocalIP
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--peer-ip", "differ")
	})
}

func TestValidatePorts(t *testing.T) {
	t.Run("valid ports accepted", func(t *testing.T) {
		raw := validRaw(t)
		raw.ControlPort = 1024
		raw.IperfPort = 65534
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.ControlPort != 1024 || cfg.IperfPort != 65534 {
			t.Errorf("ports = %d/%d, want 1024/65534", cfg.ControlPort, cfg.IperfPort)
		}
	})

	t.Run("privileged control port rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.ControlPort = 80
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--control-port", "privileged ports are not supported")
	})

	t.Run("privileged iperf port rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.IperfPort = 1023
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--iperf-port", "privileged ports are not supported")
	})

	t.Run("port zero rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.ControlPort = 0
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--control-port", "")
	})

	t.Run("port above 65535 rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.ControlPort = 70000
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--control-port", "65535")
	})

	t.Run("iperf port 65535 rejected because port+1 is used", func(t *testing.T) {
		raw := validRaw(t)
		raw.IperfPort = 65535
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--iperf-port", "65534")
	})

	t.Run("control equal to iperf rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.ControlPort = 44301
		raw.IperfPort = 44301
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--control-port", "")
	})

	t.Run("control equal to iperf plus one rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.ControlPort = 44302
		raw.IperfPort = 44301
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--control-port", "")
	})
}

func TestValidateEnums(t *testing.T) {
	t.Run("role required", func(t *testing.T) {
		raw := validRaw(t)
		raw.Role = ""
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--role", "required")
	})

	t.Run("bad role rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.Role = "pc3"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--role", "pc1")
	})

	t.Run("pc2 accepted", func(t *testing.T) {
		raw := validRaw(t)
		raw.Role = "pc2"
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.Role != RolePC2 {
			t.Errorf("Role = %q, want %q", cfg.Role, RolePC2)
		}
	})

	t.Run("bad mode rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "fast"
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--mode", "quick")
	})

	t.Run("all modes accepted", func(t *testing.T) {
		for _, mode := range []string{"quick", "standard", "soak"} {
			raw := validRaw(t)
			raw.Mode = mode
			cfg, err := Resolve(raw, nil)
			if err != nil {
				t.Errorf("Resolve(mode=%q) error = %v, want nil", mode, err)
				continue
			}
			if cfg.Mode != Mode(mode) {
				t.Errorf("Mode = %q, want %q", cfg.Mode, mode)
			}
		}
	})

	t.Run("bad soak-load rejected in soak mode", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "soak"
		raw.SoakLoad = "burst"
		_, err := Resolve(raw, map[string]bool{"soak-load": true})
		wantValidationError(t, err, "--soak-load", "periodic")
	})

	t.Run("continuous soak-load accepted", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "soak"
		raw.SoakLoad = "continuous"
		cfg, err := Resolve(raw, map[string]bool{"soak-load": true})
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.SoakLoad != SoakLoadContinuous {
			t.Errorf("SoakLoad = %q, want %q", cfg.SoakLoad, SoakLoadContinuous)
		}
	})
}

func TestValidateDurations(t *testing.T) {
	t.Run("explicit zero tcp-duration hits bounds error", func(t *testing.T) {
		raw := validRaw(t)
		raw.TCPDuration = 0
		_, err := Resolve(raw, map[string]bool{"tcp-duration": true})
		wantValidationError(t, err, "--tcp-duration", "between")
	})

	t.Run("tcp-duration below 5s rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.TCPDuration = 4 * time.Second
		_, err := Resolve(raw, map[string]bool{"tcp-duration": true})
		wantValidationError(t, err, "--tcp-duration", "5s")
	})

	t.Run("tcp-duration above 10m rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.TCPDuration = 11 * time.Minute
		_, err := Resolve(raw, map[string]bool{"tcp-duration": true})
		wantValidationError(t, err, "--tcp-duration", "10m")
	})

	t.Run("tcp-duration bounds inclusive", func(t *testing.T) {
		for _, d := range []time.Duration{5 * time.Second, 10 * time.Minute} {
			raw := validRaw(t)
			raw.TCPDuration = d
			cfg, err := Resolve(raw, map[string]bool{"tcp-duration": true})
			if err != nil {
				t.Errorf("Resolve(tcp=%v) error = %v, want nil", d, err)
				continue
			}
			if cfg.TCPDuration != d {
				t.Errorf("TCPDuration = %v, want %v", cfg.TCPDuration, d)
			}
		}
	})

	t.Run("udp-duration below 5s rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.UDPDuration = 3 * time.Second
		_, err := Resolve(raw, map[string]bool{"udp-duration": true})
		wantValidationError(t, err, "--udp-duration", "5s")
	})

	t.Run("explicit zero udp-duration hits bounds error", func(t *testing.T) {
		raw := validRaw(t)
		raw.UDPDuration = 0
		_, err := Resolve(raw, map[string]bool{"udp-duration": true})
		wantValidationError(t, err, "--udp-duration", "between")
	})

	t.Run("monitor-interval below 200ms rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.MonitorInterval = 100 * time.Millisecond
		_, err := Resolve(raw, map[string]bool{"monitor-interval": true})
		wantValidationError(t, err, "--monitor-interval", "200ms")
	})

	t.Run("monitor-interval above 30s rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.MonitorInterval = 31 * time.Second
		_, err := Resolve(raw, map[string]bool{"monitor-interval": true})
		wantValidationError(t, err, "--monitor-interval", "30s")
	})

	t.Run("soak-duration below 60s rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "soak"
		raw.SoakDuration = 30 * time.Second
		_, err := Resolve(raw, map[string]bool{"soak-duration": true})
		wantValidationError(t, err, "--soak-duration", "between")
	})

	t.Run("soak-duration above 24h rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "soak"
		raw.SoakDuration = 25 * time.Hour
		_, err := Resolve(raw, map[string]bool{"soak-duration": true})
		wantValidationError(t, err, "--soak-duration", "24h")
	})

	t.Run("soak-duration bounds inclusive", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "soak"
		raw.SoakDuration = 60 * time.Second
		cfg, err := Resolve(raw, map[string]bool{"soak-duration": true})
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.SoakDuration != 60*time.Second {
			t.Errorf("SoakDuration = %v, want 60s", cfg.SoakDuration)
		}
	})
}

func TestValidateParallelStreams(t *testing.T) {
	t.Run("explicit zero rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.ParallelStreams = 0
		_, err := Resolve(raw, map[string]bool{"parallel-streams": true})
		wantValidationError(t, err, "--parallel-streams", "between 1 and 16")
	})

	t.Run("seventeen rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.ParallelStreams = 17
		_, err := Resolve(raw, map[string]bool{"parallel-streams": true})
		wantValidationError(t, err, "--parallel-streams", "between 1 and 16")
	})

	t.Run("explicit sixteen accepted", func(t *testing.T) {
		raw := validRaw(t)
		raw.ParallelStreams = 16
		cfg, err := Resolve(raw, map[string]bool{"parallel-streams": true})
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.ParallelStreams != 16 {
			t.Errorf("ParallelStreams = %d, want 16", cfg.ParallelStreams)
		}
	})
}

func TestValidateUDPRate(t *testing.T) {
	t.Run("empty means auto", func(t *testing.T) {
		cfg, err := Resolve(validRaw(t), nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.UDPRate != 0 {
			t.Errorf("UDPRate = %v, want 0 (auto)", cfg.UDPRate)
		}
	})

	t.Run("valid rate parsed", func(t *testing.T) {
		raw := validRaw(t)
		raw.UDPRate = "800M"
		cfg, err := Resolve(raw, map[string]bool{"udp-rate": true})
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.UDPRate != model.Bitrate(800_000_000) {
			t.Errorf("UDPRate = %d, want 800000000", cfg.UDPRate)
		}
	})

	t.Run("unparseable rate rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.UDPRate = "eleventy"
		_, err := Resolve(raw, map[string]bool{"udp-rate": true})
		wantValidationError(t, err, "--udp-rate", "")
	})

	t.Run("rate below 1M rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.UDPRate = "500K"
		_, err := Resolve(raw, map[string]bool{"udp-rate": true})
		wantValidationError(t, err, "--udp-rate", "between")
	})

	t.Run("rate above 40G rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.UDPRate = "41G"
		_, err := Resolve(raw, map[string]bool{"udp-rate": true})
		wantValidationError(t, err, "--udp-rate", "between")
	})
}

func TestModeDefaults(t *testing.T) {
	cases := []struct {
		mode         Mode
		tcpDuration  time.Duration
		udpDuration  time.Duration
		pingCount    int
		tcpRepeats   int
		soakDuration time.Duration
		soakLoad     SoakLoad
	}{
		{ModeQuick, 30 * time.Second, 20 * time.Second, 500, 1, 0, ""},
		{ModeStandard, 60 * time.Second, 30 * time.Second, 1500, 2, 0, ""},
		{ModeSoak, 60 * time.Second, 20 * time.Second, 500, 1, time.Hour, SoakLoadPeriodic},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			raw := validRaw(t)
			raw.Mode = string(tc.mode)
			cfg, err := Resolve(raw, nil)
			if err != nil {
				t.Fatalf("Resolve() error = %v, want nil", err)
			}
			if cfg.TCPDuration != tc.tcpDuration {
				t.Errorf("TCPDuration = %v, want %v", cfg.TCPDuration, tc.tcpDuration)
			}
			if cfg.UDPDuration != tc.udpDuration {
				t.Errorf("UDPDuration = %v, want %v", cfg.UDPDuration, tc.udpDuration)
			}
			if cfg.ParallelStreams != 4 {
				t.Errorf("ParallelStreams = %d, want 4", cfg.ParallelStreams)
			}
			if cfg.PingCount != tc.pingCount {
				t.Errorf("PingCount = %d, want %d", cfg.PingCount, tc.pingCount)
			}
			if cfg.PingInterval != 20*time.Millisecond {
				t.Errorf("PingInterval = %v, want 20ms", cfg.PingInterval)
			}
			if cfg.TCPRepeats != tc.tcpRepeats {
				t.Errorf("TCPRepeats = %d, want %d", cfg.TCPRepeats, tc.tcpRepeats)
			}
			if cfg.MonitorInterval != time.Second {
				t.Errorf("MonitorInterval = %v, want 1s", cfg.MonitorInterval)
			}
			if cfg.SoakDuration != tc.soakDuration {
				t.Errorf("SoakDuration = %v, want %v", cfg.SoakDuration, tc.soakDuration)
			}
			if cfg.SoakLoad != tc.soakLoad {
				t.Errorf("SoakLoad = %q, want %q", cfg.SoakLoad, tc.soakLoad)
			}
		})
	}

	t.Run("explicit values win over presets", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "standard"
		raw.TCPDuration = 45 * time.Second
		raw.ParallelStreams = 8
		cfg, err := Resolve(raw, map[string]bool{"tcp-duration": true, "parallel-streams": true})
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.TCPDuration != 45*time.Second {
			t.Errorf("TCPDuration = %v, want explicit 45s", cfg.TCPDuration)
		}
		if cfg.ParallelStreams != 8 {
			t.Errorf("ParallelStreams = %d, want explicit 8", cfg.ParallelStreams)
		}
		// Not explicitly set: preset still applies.
		if cfg.UDPDuration != 30*time.Second {
			t.Errorf("UDPDuration = %v, want preset 30s", cfg.UDPDuration)
		}
	})

	t.Run("raw value without explicitlySet entry is ignored", func(t *testing.T) {
		// flag.Visit is the source of truth: a raw value that was not
		// explicitly set on the command line is replaced by the preset.
		raw := validRaw(t)
		raw.TCPDuration = 45 * time.Second
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.TCPDuration != 30*time.Second {
			t.Errorf("TCPDuration = %v, want preset 30s (raw value not in explicitlySet)", cfg.TCPDuration)
		}
	})

	t.Run("soak-duration outside soak mode rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "quick"
		raw.SoakDuration = time.Hour
		_, err := Resolve(raw, map[string]bool{"soak-duration": true})
		wantValidationError(t, err, "--soak-duration", "only valid with --mode soak")
	})

	t.Run("soak-load outside soak mode rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.Mode = "standard"
		raw.SoakLoad = "continuous"
		_, err := Resolve(raw, map[string]bool{"soak-load": true})
		wantValidationError(t, err, "--soak-load", "only valid with --mode soak")
	})
}

func TestTokenRules(t *testing.T) {
	sixDigit := regexp.MustCompile(`^[0-9]{6}$`)

	t.Run("pc1 empty token generates a 6-digit code", func(t *testing.T) {
		raw := validRaw(t)
		raw.Token = ""
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if !sixDigit.MatchString(cfg.Token) {
			t.Errorf("Token = %q, want 6 digits", cfg.Token)
		}
		if !cfg.TokenGenerated {
			t.Errorf("TokenGenerated = false, want true")
		}

		// Sample several tokens and require more than one distinct value.
		// (A single pair could collide 1-in-a-million; not-all-identical
		// across a handful is effectively certain and never flakes.)
		seen := map[string]struct{}{cfg.Token: {}}
		for i := 0; i < 8; i++ {
			r := validRaw(t)
			r.Token = ""
			c, err := Resolve(r, nil)
			if err != nil {
				t.Fatalf("Resolve() error = %v, want nil", err)
			}
			if !sixDigit.MatchString(c.Token) {
				t.Errorf("Token = %q, want 6 digits", c.Token)
			}
			seen[c.Token] = struct{}{}
		}
		if len(seen) < 2 {
			t.Errorf("all generated tokens were identical (%v); want random tokens", seen)
		}
	})

	t.Run("pc2 empty token rejected with copy hint", func(t *testing.T) {
		raw := validRaw(t)
		raw.Role = "pc2"
		raw.Token = ""
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--token", "copy the token")
	})

	t.Run("provided token not marked generated", func(t *testing.T) {
		raw := validRaw(t)
		raw.Token = "demo-token-1234"
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.Token != "demo-token-1234" {
			t.Errorf("Token = %q, want %q", cfg.Token, "demo-token-1234")
		}
		if cfg.TokenGenerated {
			t.Errorf("TokenGenerated = true, want false for user-supplied token")
		}
	})

	t.Run("token length bounds", func(t *testing.T) {
		for _, tc := range []struct {
			token string
			ok    bool
		}{
			{"12345", false},  // 5 chars, below the 6 minimum
			{"123456", true},  // 6 chars, matches a generated code
			{"seven77", true}, // 7 chars, valid
			{strings.Repeat("a", 128), true},
			{strings.Repeat("a", 129), false},
		} {
			raw := validRaw(t)
			raw.Token = tc.token
			_, err := Resolve(raw, nil)
			if tc.ok && err != nil {
				t.Errorf("Resolve(token len %d) error = %v, want nil", len(tc.token), err)
			}
			if !tc.ok {
				wantValidationError(t, err, "--token", "6-128")
			}
		}
	})

	t.Run("whitespace and non-printable rejected", func(t *testing.T) {
		for _, token := range []string{
			"has a space",
			"has\ttab99",
			"has\nnewline",
			"t\x01control",
			"tökén-chars",
		} {
			raw := validRaw(t)
			raw.Token = token
			_, err := Resolve(raw, nil)
			wantValidationError(t, err, "--token", "")
		}
	})
}

func TestOutputPathValidation(t *testing.T) {
	t.Run("dotdot element rejected", func(t *testing.T) {
		for _, out := range []string{"../reports", "/tmp/foo/../bar", "a/../b", ".."} {
			raw := validRaw(t)
			raw.Output = out
			_, err := Resolve(raw, nil)
			wantValidationError(t, err, "--output", "..")
		}
	})

	t.Run("dotdot-prefixed name allowed", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "..reports")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		raw := validRaw(t)
		raw.Output = sub
		if _, err := Resolve(raw, nil); err != nil {
			t.Errorf("Resolve() error = %v, want nil (%q is not a traversal element)", err, "..reports")
		}
	})

	t.Run("result is absolute and clean", func(t *testing.T) {
		dir := t.TempDir()
		raw := validRaw(t)
		raw.Output = dir + string(filepath.Separator) + "." + string(filepath.Separator)
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if cfg.OutputDir != dir {
			t.Errorf("OutputDir = %q, want cleaned %q", cfg.OutputDir, dir)
		}
		if !filepath.IsAbs(cfg.OutputDir) {
			t.Errorf("OutputDir = %q, want absolute path", cfg.OutputDir)
		}
	})

	t.Run("relative path made absolute", func(t *testing.T) {
		raw := validRaw(t)
		raw.Output = "."
		cfg, err := Resolve(raw, nil)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		want, err := filepath.Abs(".")
		if err != nil {
			t.Fatalf("filepath.Abs: %v", err)
		}
		if cfg.OutputDir != want {
			t.Errorf("OutputDir = %q, want %q", cfg.OutputDir, want)
		}
	})

	t.Run("missing directory rejected", func(t *testing.T) {
		raw := validRaw(t)
		raw.Output = filepath.Join(t.TempDir(), "does-not-exist")
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--output", "exist")
	})

	t.Run("file instead of directory rejected", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		raw := validRaw(t)
		raw.Output = file
		_, err := Resolve(raw, nil)
		wantValidationError(t, err, "--output", "not a directory")
	})
}

func TestDefaultUDPRate(t *testing.T) {
	t.Run("gigabit gives 800M", func(t *testing.T) {
		rate, warnings := DefaultUDPRate(model.Bitrate(1_000_000_000))
		if rate != model.Bitrate(800_000_000) {
			t.Errorf("DefaultUDPRate(1G) = %d, want 800000000", rate)
		}
		if len(warnings) != 0 {
			t.Errorf("warnings = %q, want none", warnings)
		}
	})

	t.Run("ten megabit floor then cap wins", func(t *testing.T) {
		// 80% of 10M = 8M, floor raises to 10M, 95% cap lowers to 9.5M.
		rate, warnings := DefaultUDPRate(model.Bitrate(10_000_000))
		if rate != model.Bitrate(9_500_000) {
			t.Errorf("DefaultUDPRate(10M) = %d, want 9500000", rate)
		}
		if len(warnings) != 0 {
			t.Errorf("warnings = %q, want none", warnings)
		}
	})

	t.Run("hundred megabit plain 80 percent", func(t *testing.T) {
		rate, _ := DefaultUDPRate(model.Bitrate(100_000_000))
		if rate != model.Bitrate(80_000_000) {
			t.Errorf("DefaultUDPRate(100M) = %d, want 80000000", rate)
		}
	})

	t.Run("unknown speed falls back to 100M with warning", func(t *testing.T) {
		rate, warnings := DefaultUDPRate(0)
		if rate != model.Bitrate(100_000_000) {
			t.Errorf("DefaultUDPRate(0) = %d, want 100000000", rate)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "unknown") {
			t.Errorf("warnings = %q, want one warning mentioning unknown link speed", warnings)
		}
	})

	t.Run("explicit rate above 95 percent flags near saturation", func(t *testing.T) {
		near, warnings := CheckExplicitUDPRate(model.Bitrate(990_000_000), model.Bitrate(1_000_000_000))
		if !near {
			t.Errorf("CheckExplicitUDPRate(990M, 1G) nearSaturation = false, want true")
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "95%") {
			t.Errorf("warnings = %q, want one warning mentioning 95%%", warnings)
		}
	})

	t.Run("explicit rate at 95 percent not flagged", func(t *testing.T) {
		near, warnings := CheckExplicitUDPRate(model.Bitrate(950_000_000), model.Bitrate(1_000_000_000))
		if near || len(warnings) != 0 {
			t.Errorf("CheckExplicitUDPRate(950M, 1G) = (%v, %q), want (false, none)", near, warnings)
		}
	})

	t.Run("unknown negotiated speed yields no saturation verdict", func(t *testing.T) {
		near, warnings := CheckExplicitUDPRate(model.Bitrate(990_000_000), 0)
		if near || len(warnings) != 0 {
			t.Errorf("CheckExplicitUDPRate(990M, unknown) = (%v, %q), want (false, none)", near, warnings)
		}
	})
}

func TestCableTestTDRImpliesCableTest(t *testing.T) {
	raw := validRaw(t)
	raw.CableTestTDR = true
	cfg, err := Resolve(raw, nil)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if !cfg.CableTest {
		t.Errorf("CableTest = false, want true (implied by --cable-test-tdr)")
	}
	if !cfg.CableTestTDR {
		t.Errorf("CableTestTDR = false, want true")
	}
}

func TestRunConfigLogRedactsToken(t *testing.T) {
	const secret = "hunter2-super-secret-token"
	raw := validRaw(t)
	raw.Token = secret
	cfg, err := Resolve(raw, nil)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}

	t.Run("text handler", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		logger.Info("resolved configuration", "config", cfg)
		out := buf.String()
		if strings.Contains(out, secret) {
			t.Errorf("log output contains the token: %s", out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("log output missing REDACTED marker: %s", out)
		}
		if !strings.Contains(out, "pc1") {
			t.Errorf("log output missing role (LogValue should still carry config): %s", out)
		}
	})

	t.Run("json handler", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		logger.Info("resolved configuration", "config", cfg)
		out := buf.String()
		if strings.Contains(out, secret) {
			t.Errorf("log output contains the token: %s", out)
		}
		if !json.Valid(buf.Bytes()) {
			t.Errorf("log output is not valid JSON: %s", out)
		}
	})

	t.Run("value receiver also redacts", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		logger.Info("resolved configuration", "config", *cfg)
		if strings.Contains(buf.String(), secret) {
			t.Errorf("log output contains the token: %s", buf.String())
		}
	})
}
