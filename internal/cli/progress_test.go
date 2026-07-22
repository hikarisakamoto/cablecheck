package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestProgressLines pins the progress renderer: non-verbose mode emits exactly
// the "[n/N] name" step lines; --verbose emits a strict superset of them.
func TestProgressLines(t *testing.T) {
	steps := []struct {
		n    int
		name string
	}{
		{1, "link settings"},
		{2, "initial counter snapshot"},
		{3, "TCP throughput PC1 → PC2"},
	}
	feed := func(p *Progress) {
		for _, s := range steps {
			p.Step(s.n, len(steps), s.name)
		}
	}

	var plain bytes.Buffer
	feed(NewProgress(&plain, false))
	want := "[1/3] link settings\n" +
		"[2/3] initial counter snapshot\n" +
		"[3/3] TCP throughput PC1 → PC2\n"
	if plain.String() != want {
		t.Errorf("plain output = %q, want exactly %q", plain.String(), want)
	}

	var verbose bytes.Buffer
	feed(NewProgress(&verbose, true))
	got := verbose.String()
	for line := range strings.Lines(want) {
		if !strings.Contains(got, line) {
			t.Errorf("verbose output misses quiet line %q:\n%s", line, got)
		}
	}
	if len(strings.Split(strings.TrimRight(got, "\n"), "\n")) <= len(steps) {
		t.Errorf("verbose output is not a strict superset of the quiet lines:\n%s", got)
	}
}
