package testutil_test

import (
	"bufio"
	"io"
	"testing"

	"cablecheck/internal/testutil"
)

// TestScriptStdinDeliversLinesThenEOFAfterCleanup runs the consumer inside a
// subtest so the parent can observe EOF after the subtest's cleanup closed
// the write end of the pipe.
func TestScriptStdinDeliversLinesThenEOFAfterCleanup(t *testing.T) {
	testutil.LeakCheck(t)
	var r io.Reader
	t.Run("consume", func(t *testing.T) {
		r = testutil.ScriptStdin(t, "start", "quit")
		br := bufio.NewReader(r)
		for _, want := range []string{"start\n", "quit\n"} {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("ReadString: %v", err)
			}
			if line != want {
				t.Fatalf("read %q, want %q", line, want)
			}
		}
	})
	// Subtest cleanup has run: the writer is closed, so the reader sees EOF.
	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("Read after cleanup = (%d, %v), want (0, io.EOF)", n, err)
	}
}

// TestScriptStdinUnreadLinesDoNotHangCleanup: nobody ever reads, yet cleanup
// must unblock the internal writer goroutine (LeakCheck enforces it exits).
func TestScriptStdinUnreadLinesDoNotHangCleanup(t *testing.T) {
	testutil.LeakCheck(t)
	_ = testutil.ScriptStdin(t, "never", "read")
}

func TestScriptStdinNoLines(t *testing.T) {
	testutil.LeakCheck(t)
	var r io.Reader
	t.Run("consume", func(t *testing.T) {
		r = testutil.ScriptStdin(t)
	})
	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("Read = (%d, %v), want (0, io.EOF)", n, err)
	}
}
