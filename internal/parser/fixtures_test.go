package parser

import (
	"os"
	"path/filepath"
	"testing"

	"cablecheck/internal/testutil"
)

// fixture reads a testdata file relative to the repository root.
func fixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", "testdata"}, parts...)...)
	b, err := os.ReadFile(p)
	testutil.Require(t, err, "reading fixture")
	return b
}
