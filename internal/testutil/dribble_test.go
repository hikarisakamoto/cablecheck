package testutil_test

import (
	"bytes"
	"io"
	"testing"

	"cablecheck/internal/testutil"
)

// TestDribbleRotatesChunkSizes: 36 bytes with maxChunk 3 splits into exactly
// six 1,2,3 cycles, so every Read size is predictable and the tail is clean.
func TestDribbleRotatesChunkSizes(t *testing.T) {
	content := []byte("abcdefghijklmnopqrstuvwxyz0123456789") // 36 bytes
	d := testutil.Dribble(bytes.NewReader(content), 3)
	buf := make([]byte, 64)
	var got []byte
	for i := 0; ; i++ {
		n, err := d.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			if n != 0 {
				t.Fatalf("EOF read returned %d bytes, want 0", n)
			}
			break
		}
		if err != nil {
			t.Fatalf("Read %d: unexpected error %v", i, err)
		}
		want := i%3 + 1
		if n != want {
			t.Fatalf("Read %d returned %d bytes, want rotating chunk size %d", i, n, want)
		}
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mangled:\n got %q\nwant %q", got, content)
	}
}

func TestDribbleTotalContentPreserved(t *testing.T) {
	content := bytes.Repeat([]byte("cablecheck!"), 13) // 143 bytes, not a multiple of any cycle
	got, err := io.ReadAll(testutil.Dribble(bytes.NewReader(content), 7))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mangled:\n got %q\nwant %q", got, content)
	}
}

// TestDribbleRespectsSmallBuffer: the rotating chunk size is capped by the
// caller's buffer, and content is still preserved.
func TestDribbleRespectsSmallBuffer(t *testing.T) {
	content := []byte("partial reads are the law")
	d := testutil.Dribble(bytes.NewReader(content), 5)
	buf := make([]byte, 2)
	var got []byte
	for {
		n, err := d.Read(buf)
		if n > 2 {
			t.Fatalf("Read returned %d bytes into a 2-byte buffer", n)
		}
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if n < 1 {
			t.Fatal("Read returned 0 bytes with nil error")
		}
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mangled:\n got %q\nwant %q", got, content)
	}
}

func TestDribblePanicsOnBadMaxChunk(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Dribble(r, 0) did not panic")
		}
	}()
	testutil.Dribble(bytes.NewReader(nil), 0)
}
