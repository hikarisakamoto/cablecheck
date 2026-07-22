package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"cablecheck/internal/model"
)

func TestDecideColor(t *testing.T) {
	regular, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { regular.Close() })
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readPipe.Close() })
	t.Cleanup(func() { writePipe.Close() })
	device, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { device.Close() })

	writers := []struct {
		name string
		w    interface{ Write([]byte) (int, error) }
		tty  bool
	}{
		{"buffer", &bytes.Buffer{}, false},
		{"regular", regular, false},
		{"pipe", writePipe, false},
		{"character-device", device, true},
	}
	for _, writer := range writers {
		for _, mode := range []ColorMode{ColorAuto, ColorAlways, ColorNever} {
			for _, environment := range []struct {
				name    string
				noColor string
				term    string
			}{
				{"color-xterm", "", "xterm"},
				{"color-dumb", "", "dumb"},
				{"no-color-xterm", "1", "xterm"},
				{"no-color-dumb", "1", "dumb"},
			} {
				t.Run(writer.name+"/"+string(mode)+"/"+environment.name, func(t *testing.T) {
					want := false
					switch mode {
					case ColorAlways:
						want = true
					case ColorAuto:
						want = writer.tty && environment.noColor == "" && environment.term != "dumb"
					}
					env := func(key string) string {
						if key == "NO_COLOR" {
							return environment.noColor
						}
						if key == "TERM" {
							return environment.term
						}
						return ""
					}
					if got := DecideColor(mode, writer.w, env); got != want {
						t.Fatalf("DecideColor() = %v, want %v", got, want)
					}
				})
			}
		}
	}
	if DecideColor(ColorMode("sometimes"), device, func(string) string { return "" }) {
		t.Fatal("unknown color mode enabled color")
	}
}

func TestColorHelpers(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"health-green", HealthColor(model.HealthGood, "GOOD", true), "\x1b[32mGOOD\x1b[0m"},
		{"health-excellent", HealthColor(model.HealthExcellent, "EXCELLENT", true), "\x1b[32mEXCELLENT\x1b[0m"},
		{"health-yellow", HealthColor(model.HealthWarning, "WARNING", true), "\x1b[33mWARNING\x1b[0m"},
		{"health-red", HealthColor(model.HealthPoor, "POOR", true), "\x1b[31mPOOR\x1b[0m"},
		{"health-bold-red", HealthColor(model.HealthFailed, "FAILED", true), "\x1b[1;31mFAILED\x1b[0m"},
		{"health-cyan", HealthColor(model.HealthInconclusive, "INCONCLUSIVE", true), "\x1b[36mINCONCLUSIVE\x1b[0m"},
		{"severity-info", SeverityColor(model.SevInfo, "info", true), "\x1b[36minfo\x1b[0m"},
		{"severity-warning", SeverityColor(model.SevWarning, "warning", true), "\x1b[33mwarning\x1b[0m"},
		{"severity-poor", SeverityColor(model.SevPoor, "poor", true), "\x1b[31mpoor\x1b[0m"},
		{"severity-failed", SeverityColor(model.SevFailed, "failed", true), "\x1b[1;31mfailed\x1b[0m"},
		{"severity-marker", SeverityColor(model.SevMarker, "marker", true), "\x1b[35mmarker\x1b[0m"},
		{"disabled", HealthColor(model.HealthGood, "plain\x00text", false), "plain\x00text"},
		{"unknown-health", HealthColor(model.HealthClass("UNKNOWN"), "unknown", true), "unknown"},
		{"unknown-severity", SeverityColor(model.Severity(99), "unknown", true), "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
	if got := HealthColor(model.HealthExcellent, "plain", false); strings.Contains(got, "\x1b[") {
		t.Fatalf("disabled output contains ANSI: %q", got)
	}
}

func TestIsTerminalTypedNilFile(t *testing.T) {
	var file *os.File
	if isTerminal(file) {
		t.Fatal("typed nil *os.File reported as a terminal")
	}
}
