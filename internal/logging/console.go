package logging

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"cablecheck/internal/ui"
)

// consoleHandler is the operator-facing verbose slog.Handler: one line per
// record as "HH:MM:SS LEVEL msg key=val …" with a color-coded level tag
// (DEBUG dim, INFO cyan, WARN yellow, ERROR red). It is used only on a TTY
// (see NewStderr); non-TTY sinks keep the byte-stable stdlib TextHandler.
//
// It reuses the SAME redactSecrets hook as the text and JSON handlers, applied
// to every leaf attribute with its full group path, so the token/payload
// invariant holds identically.
type consoleHandler struct {
	mu           *sync.Mutex // shared across clones so records never interleave
	w            io.Writer
	level        slog.Level
	color        bool
	groups       []string // open groups (WithGroup), used for prefix + redaction
	preformatted []byte   // " key=val" segments accumulated via WithAttrs
}

// newConsoleHandler builds a console handler writing to w at the given level.
func newConsoleHandler(w io.Writer, level slog.Level, color bool) *consoleHandler {
	return &consoleHandler{mu: &sync.Mutex{}, w: w, level: level, color: color}
}

// Enabled reports whether l meets the handler's level threshold.
func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

// Handle formats one record and writes it atomically. A zero record time is
// omitted, per the slog.Handler contract.
func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var buf []byte
	if !r.Time.IsZero() {
		buf = append(buf, r.Time.Format("15:04:05")...)
		buf = append(buf, ' ')
	}
	buf = append(buf, h.levelTag(r.Level)...)
	buf = append(buf, ' ')
	// A message carrying control bytes (peer-supplied text, wrapped errors)
	// could split the line or smuggle ANSI sequences to the TTY; quote it
	// then. Invalid UTF-8 counts too: a lone C1 byte such as 0x9B (the 8-bit
	// CSI) decodes to utf8.RuneError, which IsControl misses, so it would
	// otherwise reach the TTY raw — check validity as well. Plain spaces stay
	// readable and unquoted.
	if !utf8.ValidString(r.Message) || strings.ContainsFunc(r.Message, unicode.IsControl) {
		buf = strconv.AppendQuote(buf, r.Message)
	} else {
		buf = append(buf, r.Message...)
	}
	buf = append(buf, h.preformatted...)
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(&buf, h.groups, a)
		return true
	})
	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

// WithAttrs pre-renders attrs under the current group path so Handle need not
// re-walk them per record.
func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	nh := h.clone()
	for _, a := range attrs {
		nh.appendAttr(&nh.preformatted, h.groups, a)
	}
	return nh
}

// WithGroup nests subsequent attributes (including the record's) under name.
func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := h.clone()
	nh.groups = append(nh.groups, name)
	return nh
}

func (h *consoleHandler) clone() *consoleHandler {
	nh := *h
	// Copy the slices so appends on the clone never alias the parent's arrays.
	nh.groups = append([]string(nil), h.groups...)
	nh.preformatted = append([]byte(nil), h.preformatted...)
	return &nh
}

// appendAttr resolves a, redacts secret leaves (by key or enclosing group),
// and appends " group.key=val". Group attributes are inlined with a dotted key
// prefix; empty groups and zero attrs are dropped. It mirrors the stdlib text
// handler: Resolve any LogValuer, then apply redactSecrets with the full group
// path to every non-group attribute.
func (h *consoleHandler) appendAttr(buf *[]byte, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			return
		}
		sub := groups
		if a.Key != "" {
			// Fresh slice: sibling groups at this level must not alias each other.
			sub = append(append([]string(nil), groups...), a.Key)
		}
		for _, ga := range group {
			h.appendAttr(buf, sub, ga)
		}
		return
	}
	a = redactSecrets(groups, a)
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + a.Key
	}
	*buf = append(*buf, ' ')
	*buf = appendMaybeQuoted(*buf, key)
	*buf = append(*buf, '=')
	*buf = appendMaybeQuoted(*buf, a.Value.String())
}

// appendMaybeQuoted appends s, strconv-quoted when leaving it raw could break
// the line format or the TTY: empty, or containing an invalid UTF-8 byte, a
// non-printable rune (incl. every control byte and ESC), any Unicode
// whitespace (ASCII space, tab, NBSP, …), '"' or '='. Mirrors the stdlib
// TextHandler's quoting rule so "key=val" segments stay unambiguous.
func appendMaybeQuoted(buf []byte, s string) []byte {
	if needsQuoting(s) {
		return strconv.AppendQuote(buf, s)
	}
	return append(buf, s...)
}

// needsQuoting reports whether s must be strconv-quoted in a key=val segment.
// It mirrors slog.TextHandler: quote on invalid UTF-8, any non-printable rune,
// any Unicode whitespace, or the grammar separators '"' and '='. Using
// !IsPrint (which subsumes IsControl) and IsSpace (which subsumes the ASCII
// space) closes the gap where non-ASCII whitespace like NBSP would otherwise
// pass through and let strings.Fields split one value into two tokens.
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsPrint(r) || unicode.IsSpace(r) || r == '"' || r == '=' {
			return true
		}
	}
	return false
}

// levelTag renders the level name in its outcome color.
func (h *consoleHandler) levelTag(l slog.Level) string {
	name := l.String()
	switch {
	case l < slog.LevelInfo:
		return ui.Dim(name, h.color)
	case l < slog.LevelWarn:
		return ui.Cyan(name, h.color)
	case l < slog.LevelError:
		return ui.Yellow(name, h.color)
	default:
		return ui.Red(name, h.color)
	}
}
