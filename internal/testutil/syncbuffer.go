package testutil

import (
	"bytes"
	"sync"
)

// SyncBuffer is a mutex-guarded bytes.Buffer safe for concurrent writers,
// for capturing output that tests assert on after goroutines finish.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer under the buffer's lock.
func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the accumulated contents under the buffer's lock.
func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
