package logging

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/slogtest"
	"time"

	"cablecheck/internal/config"
	"cablecheck/internal/testutil"
)

// TestLoggingTokenRedaction drives log traffic through both handlers (stderr
// text + debug JSON file) and asserts the session token can reach neither
// sink: attr-keyed token/payload values are redacted by ReplaceAttr on both
// handlers, and a RunConfig logged via slog.Any is redacted by its LogValuer.
func TestLoggingTokenRedaction(t *testing.T) {
	const token = "supersecret-token-1f2e3d4c"

	var stderrBuf bytes.Buffer
	base := NewStderr(&stderrBuf, false, false)

	dir := t.TempDir()
	path := filepath.Join(dir, "cablecheck-pc1.log")
	log, closer, err := AttachDebugFile(base, path)
	testutil.Require(t, err, "AttachDebugFile")

	cfg := config.RunConfig{
		Role:    config.RolePC1,
		LocalIP: netip.MustParseAddr("192.168.50.10"),
		PeerIP:  netip.MustParseAddr("192.168.50.11"),
		Token:   token,
	}

	log.Info("handshake", "token", token)
	log.Info("frame", slog.Group("hello", slog.String("payload", token)))
	log.Info("run configuration", slog.Any("config", cfg))
	log.Debug("debug-only line", "payload", token, "detail", "visible in file only")
	log.Info("envelope", slog.Group("msg",
		slog.String("dir", "send"), slog.String("id", "pc1-00000001"), slog.Int("bytes", 128)))
	// A secret riding under a group NAMED for the secret must also be redacted
	// (redactSecrets checks the group path, not only the leaf key).
	log.Info("grouped", slog.Group("token", slog.String("value", token)))
	log.WithGroup("payload").Info("withgroup", "value", token)

	if err := closer.Close(); err != nil {
		t.Fatalf("close debug file: %v", err)
	}
	fileBytes, err := os.ReadFile(path)
	testutil.Require(t, err, "read debug file")

	stderrOut := stderrBuf.String()
	fileOut := string(fileBytes)

	if strings.Contains(stderrOut, token) {
		t.Errorf("token leaked to stderr sink:\n%s", stderrOut)
	}
	if strings.Contains(fileOut, token) {
		t.Errorf("token leaked to debug file sink:\n%s", fileOut)
	}
	if !strings.Contains(stderrOut, "[REDACTED]") {
		t.Errorf("stderr sink shows no redaction marker:\n%s", stderrOut)
	}
	if !strings.Contains(fileOut, "[REDACTED]") {
		t.Errorf("file sink shows no redaction marker:\n%s", fileOut)
	}

	// Levels: the file handler is always Debug; the non-verbose stderr
	// handler is Info.
	if strings.Contains(stderrOut, "debug-only line") {
		t.Errorf("stderr sink shows debug record without --verbose:\n%s", stderrOut)
	}
	if !strings.Contains(fileOut, "debug-only line") {
		t.Errorf("file sink misses debug record:\n%s", fileOut)
	}

	// MsgAttrs logs metadata only.
	if !strings.Contains(fileOut, "pc1-00000001") {
		t.Errorf("file sink misses MsgAttrs message id:\n%s", fileOut)
	}
}

// TestNewStderrVerbose pins the level switch: verbose enables Debug. A buffer
// is never a TTY, so even verbose+color keeps the byte-stable TextHandler.
func TestNewStderrVerbose(t *testing.T) {
	var buf bytes.Buffer
	log := NewStderr(&buf, true, true)
	log.Debug("verbose debug line")
	out := buf.String()
	if !strings.Contains(out, "verbose debug line") {
		t.Errorf("verbose stderr logger dropped a debug record: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("non-TTY buffer must stay on the plain TextHandler, got ANSI: %q", out)
	}
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("non-TTY buffer should render the TextHandler format: %q", out)
	}
}

// TestConsoleHandlerFormat pins the colored console handler used on a verbose
// TTY: the "HH:MM:SS LEVEL msg key=val" layout, the level tag color, dotted
// group prefixes (including a resolved RunConfig LogValuer), and that the
// token/payload redaction invariant holds identically to the stdlib handlers.
func TestConsoleHandlerFormat(t *testing.T) {
	const token = "supersecret-token-1f2e3d4c"
	ts := time.Date(2026, 7, 24, 13, 45, 7, 0, time.UTC)

	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelDebug, true)
	// A group opened via WithGroup nests both WithAttrs and record attrs.
	h = h.WithAttrs([]slog.Attr{slog.String("role", "pc1")}).(*consoleHandler)

	rec := slog.NewRecord(ts, slog.LevelInfo, "handshake", 0)
	rec.Add("token", token, "peer", "pc2")
	rec.AddAttrs(slog.Group("msg",
		slog.String("dir", "send"), slog.String("id", "pc1-00000001"), slog.Int("bytes", 128)))
	rec.AddAttrs(slog.Any("config", config.RunConfig{
		Role:    config.RolePC1,
		LocalIP: netip.MustParseAddr("192.168.50.10"),
		PeerIP:  netip.MustParseAddr("192.168.50.11"),
		Token:   token,
	}))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"13:45:07 ",               // timestamp
		"\x1b[36mINFO\x1b[0m",     // INFO tag in cyan
		"handshake",               // message
		"role=pc1",                // WithAttrs
		"peer=pc2",                // record attr
		"token=[REDACTED]",        // secret leaf redacted
		"msg.dir=send",            // dotted group prefix
		"msg.id=pc1-00000001",     // MsgAttrs group
		"config.token=[REDACTED]", // LogValuer resolved + redacted
	} {
		if !strings.Contains(got, want) {
			t.Errorf("console line missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, token) {
		t.Errorf("token leaked through console handler:\n%s", got)
	}

	// Level colors for the other severities and the plain (color off) path.
	for _, tc := range []struct {
		level slog.Level
		sgr   string
	}{
		{slog.LevelDebug, "\x1b[2m"},
		{slog.LevelWarn, "\x1b[33m"},
		{slog.LevelError, "\x1b[31m"},
	} {
		var b bytes.Buffer
		ch := newConsoleHandler(&b, slog.LevelDebug, true)
		if err := ch.Handle(context.Background(), slog.NewRecord(ts, tc.level, "m", 0)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if !strings.Contains(b.String(), tc.sgr) {
			t.Errorf("level %v tag missing SGR %q: %q", tc.level, tc.sgr, b.String())
		}
	}

	var plain bytes.Buffer
	ch := newConsoleHandler(&plain, slog.LevelDebug, false)
	if err := ch.Handle(context.Background(), slog.NewRecord(ts, slog.LevelInfo, "m", 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(plain.String(), "\x1b[") {
		t.Errorf("color=false console handler emitted ANSI: %q", plain.String())
	}
	if got := plain.String(); got != "13:45:07 INFO m\n" {
		t.Errorf("plain console line = %q, want %q", got, "13:45:07 INFO m\n")
	}
}

// TestConsoleHandlerRedactsGroupPath pins the console handler's group-path
// redaction (a secret under a group named token/payload) and its omission of a
// zero record time per the slog.Handler contract.
func TestConsoleHandlerRedactsGroupPath(t *testing.T) {
	const token = "supersecret-token-1f2e3d4c"

	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelDebug, false)
	// Zero time => no leading "HH:MM:SS ".
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "grouped", 0)
	rec.AddAttrs(slog.Group("token", slog.String("value", token)))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// A group opened via WithGroup named for the secret redacts its leaves too.
	wg := h.WithGroup("payload")
	rec2 := slog.NewRecord(time.Time{}, slog.LevelWarn, "withgroup", 0)
	rec2.Add("value", token)
	if err := wg.Handle(context.Background(), rec2); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := buf.String()
	if strings.Contains(got, token) {
		t.Errorf("token leaked under a secret-named group:\n%s", got)
	}
	for _, want := range []string{"token.value=[REDACTED]", "payload.value=[REDACTED]"} {
		if !strings.Contains(got, want) {
			t.Errorf("console output missing %q:\n%s", want, got)
		}
	}
	if strings.HasPrefix(got, "00:00:00") {
		t.Errorf("zero record time should be omitted, got:\n%s", got)
	}
}

// TestConsoleHandlerContract runs the stdlib slog.Handler conformance suite
// (testing/slogtest) against the console handler, parsing its line format back
// into the nested-map shape the harness expects.
func TestConsoleHandlerContract(t *testing.T) {
	var buf bytes.Buffer
	newHandler := func(*testing.T) slog.Handler {
		buf.Reset()
		return newConsoleHandler(&buf, slog.LevelDebug, false)
	}
	result := func(t *testing.T) map[string]any {
		return parseConsoleLine(t, strings.TrimSuffix(buf.String(), "\n"))
	}
	slogtest.Run(t, newHandler, result)
}

// parseConsoleLine decodes "HH:MM:SS LEVEL msg k=v g.k=v" into nested maps for
// slogtest. It only supports the simple unquoted tokens the conformance cases
// emit (no spaces, quotes or '=' inside keys, values or messages).
func parseConsoleLine(t *testing.T, line string) map[string]any {
	t.Helper()
	m := map[string]any{}
	fields := strings.Fields(line)
	i := 0
	if len(fields) > 0 && len(fields[0]) == 8 && fields[0][2] == ':' && fields[0][5] == ':' {
		m[slog.TimeKey] = fields[0]
		i++
	}
	if i < len(fields) {
		m[slog.LevelKey] = fields[i]
		i++
	}
	var msg []string
	for ; i < len(fields) && !strings.Contains(fields[i], "="); i++ {
		msg = append(msg, fields[i])
	}
	m[slog.MessageKey] = strings.Join(msg, " ")
	for ; i < len(fields); i++ {
		key, val, _ := strings.Cut(fields[i], "=")
		target := m
		parts := strings.Split(key, ".")
		for _, p := range parts[:len(parts)-1] {
			sub, ok := target[p].(map[string]any)
			if !ok {
				sub = map[string]any{}
				target[p] = sub
			}
			target = sub
		}
		target[parts[len(parts)-1]] = val
	}
	return m
}

// TestConsoleHandlerQuoting pins the injection guards: control bytes in the
// message are quoted, and keys/values that could break the "key=val" grammar
// (spaces, '=', '"', ANSI escapes, empties) are strconv-quoted.
func TestConsoleHandlerQuoting(t *testing.T) {
	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelDebug, false)
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "evil\nmsg \x1b[31m", 0)
	rec.Add("spaced", "two words", "esc", "\x1b[2Jwipe", "eq", "a=b",
		"empty", "", "plain", "ok")
	rec.AddAttrs(slog.String("bad key", "v"))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		`"evil\nmsg \x1b[31m"`, // control bytes in the message => quoted
		`spaced="two words"`,
		`esc="\x1b[2Jwipe"`,
		`eq="a=b"`,
		`empty=""`,
		`"bad key"=v`,
		" plain=ok", // nothing special => raw
	} {
		if !strings.Contains(got, want) {
			t.Errorf("quoted line missing %s:\n%q", want, got)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("record must stay one line, got %q", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("raw ESC byte leaked to the sink: %q", got)
	}
}

// TestConsoleHandlerC1AndUnicodeWhitespace guards the two escape/grammar holes
// the original rune-based checks missed: an invalid-UTF-8 C1 byte (0x9B, the
// 8-bit CSI) in the message — which decodes to utf8.RuneError, not a control
// rune — and non-ASCII whitespace (NBSP) in an attribute value. Both must be
// strconv-quoted so no raw C1 byte ever reaches the TTY and every key=val
// segment stays a single strings.Fields token.
func TestConsoleHandlerC1AndUnicodeWhitespace(t *testing.T) {
	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelDebug, false)
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "x\x9by", 0)
	rec.Add("nbsp", "a\u00a0b")
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()
	if strings.ContainsRune(got, 0x9b) {
		t.Errorf("raw C1 byte 0x9b leaked to the sink: %q", got)
	}
	if strings.ContainsRune(got, '\u00a0') {
		t.Errorf("raw NBSP leaked to the sink: %q", got)
	}
	for _, want := range []string{
		`"x\x9by"`,        // C1-byte message => quoted, byte escaped
		`nbsp="a\u00a0b"`, // NBSP value => quoted, whitespace escaped
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %s:\n%q", want, got)
		}
	}
	// With both escaped, the value stays one whitespace-split token.
	nbspFields := 0
	for _, f := range strings.Fields(strings.TrimSuffix(got, "\n")) {
		if strings.HasPrefix(f, "nbsp=") {
			nbspFields++
		}
	}
	if nbspFields != 1 {
		t.Errorf("nbsp value split across %d fields, want 1: %q", nbspFields, got)
	}
}

// TestAttachDebugFileExclusive pins O_EXCL: attaching to an existing path
// must fail rather than truncate or append.
func TestAttachDebugFileExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.json")
	base := NewStderr(&bytes.Buffer{}, false, false)
	_, closer, err := AttachDebugFile(base, path)
	testutil.Require(t, err, "first AttachDebugFile")
	defer closer.Close()
	if _, _, err := AttachDebugFile(base, path); err == nil {
		t.Errorf("second AttachDebugFile on the same path succeeded; want O_EXCL failure")
	}
}
