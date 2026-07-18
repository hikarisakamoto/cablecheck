package logging

import (
	"bytes"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cablecheck/internal/config"
)

// TestLoggingTokenRedaction drives log traffic through both handlers (stderr
// text + debug JSON file) and asserts the session token can reach neither
// sink: attr-keyed token/payload values are redacted by ReplaceAttr on both
// handlers, and a RunConfig logged via slog.Any is redacted by its LogValuer.
func TestLoggingTokenRedaction(t *testing.T) {
	const token = "supersecret-token-1f2e3d4c"

	var stderrBuf bytes.Buffer
	base := NewStderr(&stderrBuf, false)

	dir := t.TempDir()
	path := filepath.Join(dir, "cablecheck-pc1.log")
	log, closer, err := AttachDebugFile(base, path)
	if err != nil {
		t.Fatalf("AttachDebugFile: %v", err)
	}

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
	log.Info("envelope", MsgAttrs("send", "hello", "pc1-00000001", 128))

	if err := closer.Close(); err != nil {
		t.Fatalf("close debug file: %v", err)
	}
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read debug file: %v", err)
	}

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

// TestNewStderrVerbose pins the level switch: verbose enables Debug.
func TestNewStderrVerbose(t *testing.T) {
	var buf bytes.Buffer
	log := NewStderr(&buf, true)
	log.Debug("verbose debug line")
	if !strings.Contains(buf.String(), "verbose debug line") {
		t.Errorf("verbose stderr logger dropped a debug record: %q", buf.String())
	}
}

// TestAttachDebugFileExclusive pins O_EXCL: attaching to an existing path
// must fail rather than truncate or append.
func TestAttachDebugFileExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.json")
	base := NewStderr(&bytes.Buffer{}, false)
	_, closer, err := AttachDebugFile(base, path)
	if err != nil {
		t.Fatalf("first AttachDebugFile: %v", err)
	}
	defer closer.Close()
	if _, _, err := AttachDebugFile(base, path); err == nil {
		t.Errorf("second AttachDebugFile on the same path succeeded; want O_EXCL failure")
	}
}
