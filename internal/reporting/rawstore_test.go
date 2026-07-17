package reporting

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRawStorePaths(t *testing.T) {
	reportDir := t.TempDir()
	store, err := NewRawStore(reportDir)
	if err != nil {
		t.Fatalf("NewRawStore: %v", err)
	}

	f1, name1, err := store.Create("pc1", "ethtool", "link-before", "txt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := filepath.Join("raw", "01-pc1-ethtool-link-before.txt"); name1 != want {
		t.Errorf("first name = %q, want %q", name1, want)
	}
	if _, err := io.WriteString(f1, "hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f2, name2, err := store.Create("pc2", "iperf3", "tcp-pc1-to-pc2", "json")
	if err != nil {
		t.Fatalf("Create (second): %v", err)
	}
	if want := filepath.Join("raw", "02-pc2-iperf3-tcp-pc1-to-pc2.json"); name2 != want {
		t.Errorf("second name = %q, want %q", name2, want)
	}
	if _, err := io.WriteString(f2, `{"ok":true}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Files must live under <reportDir>/<relative name>.
	if _, err := os.Stat(filepath.Join(reportDir, name1)); err != nil {
		t.Errorf("first file not on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reportDir, name2)); err != nil {
		t.Errorf("second file not on disk: %v", err)
	}

	idx := store.Index()
	if len(idx) != 2 {
		t.Fatalf("Index() returned %d entries, want 2", len(idx))
	}
	sum1 := sha256.Sum256([]byte("hello\n"))
	if idx[0].Name != name1 {
		t.Errorf("Index[0].Name = %q, want %q", idx[0].Name, name1)
	}
	if idx[0].Bytes != 6 {
		t.Errorf("Index[0].Bytes = %d, want 6", idx[0].Bytes)
	}
	if want := hex.EncodeToString(sum1[:]); idx[0].SHA256 != want {
		t.Errorf("Index[0].SHA256 = %q, want %q", idx[0].SHA256, want)
	}
	sum2 := sha256.Sum256([]byte(`{"ok":true}`))
	if idx[1].Name != name2 {
		t.Errorf("Index[1].Name = %q, want %q", idx[1].Name, name2)
	}
	if idx[1].Bytes != int64(len(`{"ok":true}`)) {
		t.Errorf("Index[1].Bytes = %d, want %d", idx[1].Bytes, len(`{"ok":true}`))
	}
	if want := hex.EncodeToString(sum2[:]); idx[1].SHA256 != want {
		t.Errorf("Index[1].SHA256 = %q, want %q", idx[1].SHA256, want)
	}

	t.Run("path metacharacters are rejected", func(t *testing.T) {
		if _, _, err := store.Create("pc1", "../evil", "x", "txt"); err == nil {
			t.Errorf("Create with path traversal in tool succeeded, want error")
		}
		if _, _, err := store.Create("pc1", "tool", "a/b", "txt"); err == nil {
			t.Errorf("Create with slash in purpose succeeded, want error")
		}
	})
}
