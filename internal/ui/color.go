// Package ui renders terminal-facing progress and presentation output.
package ui

import (
	"io"
	"os"

	"cablecheck/internal/model"
)

// ColorMode controls ANSI color output.
type ColorMode string

const (
	ColorAuto   ColorMode = "auto"
	ColorAlways ColorMode = "always"
	ColorNever  ColorMode = "never"
)

const ansiReset = "\x1b[0m"

// DecideColor reports whether ANSI colors should be emitted.
func DecideColor(mode ColorMode, w io.Writer, env func(string) string) bool {
	switch mode {
	case ColorNever:
		return false
	case ColorAlways:
		return true
	case ColorAuto:
		if env == nil {
			env = os.Getenv
		}
		if env("NO_COLOR") != "" || env("TERM") == "dumb" {
			return false
		}
		return isTerminal(w)
	default:
		return false
	}
}

// IsTerminal reports whether w is a character device (a TTY). It is the
// exported form of the internal probe DecideColor uses, so other packages
// (doctor, logging) can gate colored output on the same rule.
func IsTerminal(w io.Writer) bool { return isTerminal(w) }

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func colorize(text, sgr string, enabled bool) string {
	if !enabled || sgr == "" {
		return text
	}
	return sgr + text + ansiReset
}

// Green, Yellow, Red, Cyan and Dim wrap text in the corresponding SGR when
// enabled, returning it unchanged otherwise. They are the shared primitives
// behind the doctor status tags and the verbose slog console handler; the
// codes match the palette used by HealthColor/SeverityColor.
func Green(text string, enabled bool) string  { return colorize(text, "\x1b[32m", enabled) }
func Yellow(text string, enabled bool) string { return colorize(text, "\x1b[33m", enabled) }
func Red(text string, enabled bool) string    { return colorize(text, "\x1b[31m", enabled) }
func Cyan(text string, enabled bool) string   { return colorize(text, "\x1b[36m", enabled) }
func Dim(text string, enabled bool) string    { return colorize(text, "\x1b[2m", enabled) }

// HealthColor colors text according to a report health classification.
func HealthColor(class model.HealthClass, text string, enabled bool) string {
	var sgr string
	switch class {
	case model.HealthExcellent, model.HealthGood:
		sgr = "\x1b[32m"
	case model.HealthWarning:
		sgr = "\x1b[33m"
	case model.HealthPoor:
		sgr = "\x1b[31m"
	case model.HealthFailed:
		sgr = "\x1b[1;31m"
	case model.HealthInconclusive:
		sgr = "\x1b[36m"
	}
	return colorize(text, sgr, enabled)
}

// SeverityColor colors text according to a finding severity.
func SeverityColor(severity model.Severity, text string, enabled bool) string {
	var sgr string
	switch severity {
	case model.SevInfo:
		sgr = "\x1b[36m"
	case model.SevWarning:
		sgr = "\x1b[33m"
	case model.SevPoor:
		sgr = "\x1b[31m"
	case model.SevFailed:
		sgr = "\x1b[1;31m"
	case model.SevMarker:
		sgr = "\x1b[35m"
	}
	return colorize(text, sgr, enabled)
}
