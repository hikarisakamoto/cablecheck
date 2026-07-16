package protocol_test

import (
	"testing"

	"cablecheck/internal/protocol"
)

// TestTokenEqual covers the constant-time token comparison: equal tokens
// match, unequal tokens (including different lengths, which must not panic)
// do not, and two empty tokens compare equal.
func TestTokenEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"equal", "a1b2c3d4e5f60718293a4b5c6d7e8f90", "a1b2c3d4e5f60718293a4b5c6d7e8f90", true},
		{"unequal_same_length", "a1b2c3d4", "a1b2c3d5", false},
		{"different_lengths", "abc", "abcdef", false},
		{"different_lengths_reversed", "abcdef", "abc", false},
		{"empty_vs_nonempty", "", "abc", false},
		{"nonempty_vs_empty", "abc", "", false},
		{"both_empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("TokenEqual(%q, %q) panicked: %v", tc.a, tc.b, r)
				}
			}()
			if got := protocol.TokenEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("TokenEqual(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
