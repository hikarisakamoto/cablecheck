package testutil

import (
	"io"
	"testing"
)

// ScriptStdin returns a reader that yields the given lines (a "\n" is
// appended to each) and then blocks, emulating an interactive stdin that
// stays open after the scripted input. It is io.Pipe-backed so arrival timing
// is real: a consumer pump goroutine blocks between lines instead of seeing
// one pre-buffered blob. A t.Cleanup closes the write end, which unblocks any
// pending internal write, delivers EOF to the reader so consumer pumps exit,
// and waits for the internal writer goroutine to finish — keeping LeakCheck
// happy.
func ScriptStdin(t testing.TB, lines ...string) io.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, line := range lines {
			if _, err := io.WriteString(pw, line+"\n"); err != nil {
				return // pipe closed before the script was consumed
			}
		}
	}()
	t.Cleanup(func() {
		pw.Close() // idempotent; unblocks writes and EOFs the reader
		<-done
	})
	return pr
}
