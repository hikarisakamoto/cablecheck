package testutil

import "io"

// Dribble wraps r so successive Read calls return at most 1, 2, ..., maxChunk
// bytes, rotating deterministically (no randomness). It exercises
// partial-read handling in stream consumers: nothing downstream may assume
// one Read delivers one message. The rotation advances on every Read call;
// the effective chunk is additionally capped by the caller's buffer. Dribble
// panics if maxChunk < 1.
func Dribble(r io.Reader, maxChunk int) io.Reader {
	if maxChunk < 1 {
		panic("testutil.Dribble: maxChunk must be >= 1")
	}
	return &dribbler{r: r, max: maxChunk}
}

// dribbler implements the rotating partial reader returned by Dribble.
type dribbler struct {
	r    io.Reader
	max  int
	next int // rotation index; chunk size for the next Read is next+1
}

// Read reads at most the current rotating chunk size from the wrapped reader.
func (d *dribbler) Read(p []byte) (int, error) {
	size := d.next + 1
	d.next = (d.next + 1) % d.max
	if size > len(p) {
		size = len(p)
	}
	return d.r.Read(p[:size])
}
