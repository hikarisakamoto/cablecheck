// Package logging constructs CableCheck's slog loggers: a human-oriented
// text handler on stderr plus an always-Debug JSON debug file inside the
// report directory, fanned out through a multi-handler.
//
// Token redaction is layered (docs/design/clieval.md §8): protocol code logs
// envelope metadata only (direction, type, message ID and payload size —
// never payload bytes), ALL handlers (stderr text, verbose-TTY
// console, JSON debug file) redact any attr keyed "token" or "payload" via
// the shared redactSecrets hook, and config.RunConfig implements
// slog.LogValuer with the token pre-redacted. The one legitimate token
// display (the PC1 banner) goes to stdout via fmt, never through slog.
package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"cablecheck/internal/ui"
)

// redactedMarker replaces the value of every secret-keyed attribute.
const redactedMarker = "[REDACTED]"

// redactSecrets is the ReplaceAttr hook installed on every handler: an
// attribute is redacted when its own key — or any enclosing group in its path
// — is "token" or "payload" (case-insensitive). Checking the group path too
// closes the leak where a secret rides under a group named for the secret
// (e.g. slog.Group("token", ...) or WithGroup("payload")); the session token
// must never reach a sink (AGENTS.md non-negotiable).
func redactSecrets(groups []string, a slog.Attr) slog.Attr {
	if isSecretKey(a.Key) {
		a.Value = slog.StringValue(redactedMarker)
		return a
	}
	for _, g := range groups {
		if isSecretKey(g) {
			a.Value = slog.StringValue(redactedMarker)
			return a
		}
	}
	return a
}

// isSecretKey reports whether key names a secret-bearing attribute or group.
func isSecretKey(key string) bool {
	switch strings.ToLower(key) {
	case "token", "payload":
		return true
	}
	return false
}

// NewStderr returns the operator-facing logger at LevelInfo, or LevelDebug
// when verbose. On a TTY with verbose and color it installs the colored
// console handler ("HH:MM:SS LEVEL msg key=val"); otherwise it keeps the
// stdlib TextHandler, which stays byte-stable for pipes, files and CI (a
// buffer is never a TTY). Both paths redact token/payload. A nil w discards.
func NewStderr(w io.Writer, verbose, color bool) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	if verbose && color && ui.IsTerminal(w) {
		return slog.New(newConsoleHandler(w, level, true))
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactSecrets,
	}))
}

// AttachDebugFile tees base's handler with an always-Debug JSON handler
// writing to a fresh file at path (O_CREATE|O_WRONLY|O_EXCL, 0600, buffered).
// Each child handler keeps its own level: the stderr handler still honors
// --verbose while the file always records Debug. The returned Closer flushes
// the buffer and closes the file; call it after the last log write.
func AttachDebugFile(base *slog.Logger, path string) (*slog.Logger, io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: create debug log: %w", err)
	}
	bw := bufio.NewWriter(f)
	fileHandler := slog.NewJSONHandler(bw, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: redactSecrets,
	})
	tee := multiHandler{base.Handler(), fileHandler}
	return slog.New(tee), &flushCloser{bw: bw, f: f}, nil
}

// multiHandler fans one record out to several child handlers (stdlib has no
// tee handler). Every child keeps its own level and options.
type multiHandler []slog.Handler

// Enabled reports whether any child would accept a record at this level.
func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle delivers the record to every child enabled for its level. Each
// child gets its own clone: handlers may retain or mutate the record.
func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// WithAttrs applies attrs to every child.
func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

// WithGroup opens the group on every child.
func (m multiHandler) WithGroup(name string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}

// flushCloser flushes the buffered writer and closes the file.
type flushCloser struct {
	bw *bufio.Writer
	f  *os.File
}

// Close implements io.Closer: flush first, then close, reporting both.
func (c *flushCloser) Close() error {
	return errors.Join(c.bw.Flush(), c.f.Close())
}
