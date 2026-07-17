package reporting

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReportDirNaming(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 7, 15, 21, 30, 5, 123456789, time.UTC)

	dir, err := NewReportDir(base, now)
	if err != nil {
		t.Fatalf("NewReportDir: %v", err)
	}
	want := filepath.Join(base, "cablecheck-report-2026-07-15_21-30-05")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat report dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("%q is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("report dir permissions = %o, want 0700", perm)
	}

	rawInfo, err := os.Stat(filepath.Join(dir, "raw"))
	if err != nil {
		t.Fatalf("stat raw dir: %v", err)
	}
	if !rawInfo.IsDir() {
		t.Errorf("raw/ is not a directory")
	}
	if perm := rawInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("raw dir permissions = %o, want 0700", perm)
	}

	// Same-second collision: suffix -2, then -3.
	dir2, err := NewReportDir(base, now)
	if err != nil {
		t.Fatalf("NewReportDir (collision): %v", err)
	}
	if want2 := want + "-2"; dir2 != want2 {
		t.Errorf("second dir = %q, want %q", dir2, want2)
	}
	dir3, err := NewReportDir(base, now)
	if err != nil {
		t.Fatalf("NewReportDir (second collision): %v", err)
	}
	if want3 := want + "-3"; dir3 != want3 {
		t.Errorf("third dir = %q, want %q", dir3, want3)
	}
}
