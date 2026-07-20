package peer

import "testing"

func TestRedactToken(t *testing.T) {
	const token = "s3cr3t-token"
	cases := []struct {
		name  string
		in    string
		token string
		want  string
	}{
		{"replaces the token", "iperf3 client: auth " + token + " failed", token, "iperf3 client: auth [REDACTED] failed"},
		{"replaces every occurrence", token + " and " + token, token, "[REDACTED] and [REDACTED]"},
		{"empty token leaves string unchanged", "nothing to hide " + token, "", "nothing to hide " + token},
		{"token absent leaves string unchanged", "no secrets here", token, "no secrets here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactToken(tc.in, tc.token); got != tc.want {
				t.Errorf("redactToken(%q, %q) = %q, want %q", tc.in, tc.token, got, tc.want)
			}
		})
	}
}
