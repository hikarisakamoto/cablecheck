package testutil

import "testing"

// Require fails the test fatally when err is non-nil, appending the error to
// the caller's message: Require(t, err, "open %s", path) fails as
// "open /tmp/x: <err>".
func Require(t testing.TB, err error, msg string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf(msg+": %v", append(args, err)...)
	}
}
