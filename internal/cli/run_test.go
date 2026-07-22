package cli

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"cablecheck/internal/config"
	"cablecheck/internal/ui"
)

// baseRunArgs is a minimal valid `run` command line for pc1.
func baseRunArgs(extra ...string) []string {
	args := []string{
		"--role", "pc1",
		"--local-ip", "192.168.50.10",
		"--peer-ip", "192.168.50.11",
		"--token", "testtoken1234",
	}
	return append(args, extra...)
}

// TestRunFlagPresets pins the flag.Visit plumbing into config.Resolve: mode
// presets fill everything the user did not set, and any explicitly set flag
// wins — including one set to a value that a preset would also produce.
func TestRunFlagPresets(t *testing.T) {
	t.Run("quick presets fill unset flags", func(t *testing.T) {
		cfg, err := parseRunFlags(baseRunArgs(), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("parseRunFlags: %v", err)
		}
		if cfg.TCPDuration != 30*time.Second {
			t.Errorf("TCPDuration = %v, want 30s", cfg.TCPDuration)
		}
		if cfg.UDPDuration != 20*time.Second {
			t.Errorf("UDPDuration = %v, want 20s", cfg.UDPDuration)
		}
		if cfg.ParallelStreams != 4 {
			t.Errorf("ParallelStreams = %d, want 4", cfg.ParallelStreams)
		}
		if cfg.PingCount != 500 {
			t.Errorf("PingCount = %d, want 500", cfg.PingCount)
		}
		if cfg.TCPRepeats != 1 {
			t.Errorf("TCPRepeats = %d, want 1", cfg.TCPRepeats)
		}
		if cfg.MonitorInterval != time.Second {
			t.Errorf("MonitorInterval = %v, want 1s", cfg.MonitorInterval)
		}
	})

	t.Run("explicit flag beats the preset", func(t *testing.T) {
		cfg, err := parseRunFlags(baseRunArgs("--tcp-duration", "45s"), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("parseRunFlags: %v", err)
		}
		if cfg.TCPDuration != 45*time.Second {
			t.Errorf("TCPDuration = %v, want explicit 45s", cfg.TCPDuration)
		}
		if cfg.UDPDuration != 20*time.Second {
			t.Errorf("UDPDuration = %v, want preset 20s", cfg.UDPDuration)
		}
	})

	t.Run("standard presets differ from quick", func(t *testing.T) {
		cfg, err := parseRunFlags(baseRunArgs("--mode", "standard"), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("parseRunFlags: %v", err)
		}
		if cfg.TCPDuration != 60*time.Second || cfg.UDPDuration != 30*time.Second {
			t.Errorf("standard durations = %v/%v, want 60s/30s", cfg.TCPDuration, cfg.UDPDuration)
		}
		if cfg.PingCount != 1500 || cfg.TCPRepeats != 2 {
			t.Errorf("standard ping/repeats = %d/%d, want 1500/2", cfg.PingCount, cfg.TCPRepeats)
		}
	})

	t.Run("explicit sentinel value is caught by bounds validation", func(t *testing.T) {
		// Only flag.Visit can distinguish `--tcp-duration 0` from unset;
		// the explicit zero must reach Resolve and fail bounds there.
		_, err := parseRunFlags(baseRunArgs("--tcp-duration", "0s"), &bytes.Buffer{})
		var ve *config.ValidationError
		if !errors.As(err, &ve) || ve.Flag != "--tcp-duration" {
			t.Errorf("explicit --tcp-duration 0s: err = %v, want ValidationError for --tcp-duration", err)
		}
	})

	t.Run("soak flags outside soak mode fail fast", func(t *testing.T) {
		_, err := parseRunFlags(baseRunArgs("--soak-duration", "2h"), &bytes.Buffer{})
		var ve *config.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("soak flag in quick mode: err = %v, want ValidationError", err)
		}
	})
}

func TestRunPresentationFlags(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := parseRunFlags(baseRunArgs(), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("parseRunFlags: %v", err)
		}
		if cfg.Color != "auto" || cfg.Quiet {
			t.Errorf("presentation defaults = color %q, quiet %v; want auto, false", cfg.Color, cfg.Quiet)
		}
	})

	for _, tc := range []struct {
		value string
		want  ui.ColorMode
	}{
		{"auto", ui.ColorAuto},
		{"always", ui.ColorAlways},
		{"never", ui.ColorNever},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := parseRunFlags(baseRunArgs("--color", tc.value, "--quiet"), &bytes.Buffer{})
			if err != nil {
				t.Fatalf("parseRunFlags: %v", err)
			}
			if cfg.Color != tc.value || !cfg.Quiet {
				t.Errorf("presentation flags = color %q, quiet %v; want %q, true", cfg.Color, cfg.Quiet, tc.value)
			}
			if got := mapColorMode(cfg.Color); got != tc.want {
				t.Errorf("mapColorMode(%q) = %q, want %q", cfg.Color, got, tc.want)
			}
		})
	}

	t.Run("invalid color", func(t *testing.T) {
		_, err := parseRunFlags(baseRunArgs("--color", "sometimes"), &bytes.Buffer{})
		var ve *config.ValidationError
		if !errors.As(err, &ve) || ve.Flag != "--color" {
			t.Errorf("invalid --color: err = %v, want ValidationError for --color", err)
		}
	})
}
