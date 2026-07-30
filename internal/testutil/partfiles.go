package testutil

import (
	"os"
	"strings"
	"testing"
)

// AssertNoPartFiles fails if any *.part scratch file survived in dir.
func AssertNoPartFiles(t testing.TB, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover partial file %q in %s", e.Name(), dir)
		}
	}
}
