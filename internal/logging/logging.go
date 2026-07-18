// Package logging constructs CableCheck's slog loggers: a human-oriented
// text handler on stderr plus an always-Debug JSON debug file inside the
// report directory, fanned out through a multi-handler.
//
// Token redaction is layered (docs/design/clieval.md §8): protocol code logs
// envelope metadata only (MsgAttrs), BOTH handlers redact any attr keyed
// "token" or "payload" via ReplaceAttr, and config.RunConfig implements
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
)

// redactedMarker replaces the value of every secret-keyed attribute.
const redactedMarker = "[REDACTED]"

// redactSecrets is the ReplaceAttr hook installed on BOTH handlers: any
// attribute keyed "token" or "payload" (case-insensitive, at any group
// depth) has its value replaced before it can reach a sink.
func redactSecrets(_ []string, a slog.Attr) slog.Attr {
	switch strings.ToLower(a.Key) {
	case "token", "payload":
		a.Value = slog.StringValue(redactedMarker)
	}
	return a
}

// NewStderr returns the operator-facing logger: a text handler on w at
// LevelInfo, or LevelDebug when verbose. A nil w discards.
func NewStderr(w io.Writer, verbose bool) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
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

// MsgAttrs is the ONLY sanctioned way to log a protocol envelope: direction,
// type, message ID and payload size — never payload bytes.
func MsgAttrs(dir, msgType, messageID string, payloadBytes int) slog.Attr {
	return slog.Group("msg",
		slog.String("dir", dir),
		slog.String("type", msgType),
		slog.String("id", messageID),
		slog.Int("bytes", payloadBytes),
	)
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
