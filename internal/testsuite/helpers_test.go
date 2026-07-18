package testsuite

import (
	"os"
	"path/filepath"
	"testing"

	"cablecheck/internal/runner"
	"cablecheck/internal/runner/runnertest"
)

// testdataDir resolves a fixture directory relative to this package.
func testdataDir(dir string) string {
	return filepath.Join("..", "..", "testdata", dir)
}

// fixture loads a canned CommandResult from testdata/<dir>/<name> using the
// runnertest fixture conventions (name.txt or the name.{stdout,stderr,exit}
// triplet), additionally accepting stdout-only name.json fixtures, and fails
// the test on any load problem.
func fixture(t *testing.T, dir, name string) runner.CommandResult {
	t.Helper()
	res, err := runnertest.FromFixture(testdataDir(dir), name)
	if err != nil {
		if data, jerr := os.ReadFile(fixturePath(dir, name+".json")); jerr == nil {
			return runner.CommandResult{Stdout: data}
		}
		t.Fatalf("load fixture %s/%s: %v", dir, name, err)
	}
	return res
}

// fixturePath returns the path of a stdout-only fixture file.
func fixturePath(dir, name string) string {
	return filepath.Join(testdataDir(dir), name)
}

// readFixture returns the raw bytes of a fixture file, trying the bare name
// and then the .txt and .json stdout-only conventions.
func readFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	for _, candidate := range []string{name, name + ".txt", name + ".json"} {
		data, err := os.ReadFile(fixturePath(dir, candidate))
		if err == nil {
			return data
		}
	}
	t.Fatalf("read fixture %s/%s: no such file (tried bare, .txt, .json)", dir, name)
	return nil
}

// writeSysfs writes one sysfs attribute file under root/<ifName>/<attr>.
func writeSysfs(t *testing.T, root, ifName, attr, content string) {
	t.Helper()
	dir := filepath.Join(root, ifName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, attr), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s/%s: %v", dir, attr, err)
	}
}

// closedChan returns an already-closed channel, for Scripts that should
// complete (or exit) immediately.
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
