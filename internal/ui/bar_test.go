package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDetectWidth(t *testing.T) {
	for _, tc := range []struct {
		columns string
		want    int
	}{
		{"120", 120},
		{"", 80},
		{"bad", 80},
		{"0", 80},
		{"-3", 80},
		{"20", 40},
		{"999", 200},
	} {
		t.Run(tc.columns, func(t *testing.T) {
			if got := detectWidth(func(string) string { return tc.columns }); got != tc.want {
				t.Fatalf("detectWidth() = %d, want %d", got, tc.want)
			}
		})
	}
	if got := detectWidth(nil); got != defaultWidth {
		t.Fatalf("detectWidth(nil) = %d, want %d", got, defaultWidth)
	}
}

// TestRenderBarFractionRounding pins the round-half-up fill math
// (filled = int(complete*barWidth + 0.5)); plain truncation would drop a hash.
func TestRenderBarFractionRounding(t *testing.T) {
	// width 40, empty name/detail -> barWidth 31.
	for _, tc := range []struct {
		complete float64
		want     int
	}{
		{0.25, 8}, // 0.25*31=7.75 -> round 8 (truncate 7)
		{0.5, 16}, // 0.5*31=15.5  -> round 16 (truncate 15)
	} {
		got := renderBarFraction(1, 2, 40, "", "", "", tc.complete)
		if n := strings.Count(got, "#"); n != tc.want {
			t.Fatalf("complete %v: hashes = %d, want %d: %q", tc.complete, n, tc.want, got)
		}
	}
}

func TestRenderBar(t *testing.T) {
	for _, tc := range []struct {
		name          string
		step, total   int
		width         int
		label         string
		sub, elapsed  string
		wantSubstring string
		wantHashes    int
	}{
		{"half", 1, 2, 40, "load", "running", "elapsed 5s", "[1/2] load", 5},
		{"clamp-high", 9, 2, 40, "load", "", "", "[9/2] load", 27},
		{"clamp-low", -2, 2, 40, "load", "", "", "[-2/2] load", 0},
		{"zero-total", 1, 0, 40, "load", "", "", "[1/0] load", 0},
		{"unicode", 1, 3, 40, "測試測試測試測試測試測試測試", "running", "", "[1/3] 測試", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderBar(tc.step, tc.total, tc.width, tc.label, tc.sub, tc.elapsed)
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if runeLen(got) != clampWidth(tc.width) {
				t.Fatalf("width = %d, want %d: %q", runeLen(got), clampWidth(tc.width), got)
			}
			if !strings.Contains(got, tc.wantSubstring) {
				t.Fatalf("missing %q in %q", tc.wantSubstring, got)
			}
			if gotHashes := strings.Count(got, "#"); gotHashes != tc.wantHashes {
				t.Fatalf("hashes = %d, want %d: %q", gotHashes, tc.wantHashes, got)
			}
			if strings.Contains(got, "\x1b[") {
				t.Fatalf("renderBar emitted ANSI: %q", got)
			}
		})
	}
}

func TestRenderBarFormat(t *testing.T) {
	want := "[1/2] load [#####-----] running elaps..."
	if got := renderBar(1, 2, 40, "load", "running", "elapsed 5s"); got != want {
		t.Fatalf("renderBar() = %q, want %q", got, want)
	}
}
