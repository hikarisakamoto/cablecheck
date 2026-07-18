package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"cablecheck/internal/model"
	"cablecheck/internal/peer"
)

// TestExitCodeFor pins the classification → exit-code table.
func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		class model.HealthClass
		want  ExitCode
	}{
		{model.HealthExcellent, ExitOK},
		{model.HealthGood, ExitOK},
		{model.HealthWarning, ExitWarning},
		{model.HealthPoor, ExitPoorFailed},
		{model.HealthFailed, ExitPoorFailed},
		{model.HealthInconclusive, ExitInconclusive},
		{model.HealthClass("bogus"), ExitInternal},
		{model.HealthClass(""), ExitInternal},
	}
	for _, c := range cases {
		if got := ExitCodeFor(c.class); got != c.want {
			t.Errorf("ExitCodeFor(%q) = %d, want %d", c.class, got, c.want)
		}
	}
}

// TestExitError pins Error()/Unwrap() of the typed exit-code carrier.
func TestExitError(t *testing.T) {
	inner := errors.New("boom")
	e := &ExitError{Code: ExitPeer, Err: inner}
	if e.Error() != "boom" {
		t.Errorf("Error() = %q, want %q", e.Error(), "boom")
	}
	if !errors.Is(e, inner) {
		t.Errorf("errors.Is(e, inner) = false, want true")
	}
	bare := &ExitError{Code: ExitInterrupt}
	if bare.Error() != "exit 6" {
		t.Errorf("bare Error() = %q, want %q", bare.Error(), "exit 6")
	}
	var xe *ExitError
	wrapped := fmt.Errorf("outer: %w", e)
	if !errors.As(wrapped, &xe) || xe.Code != ExitPeer {
		t.Errorf("errors.As through wrapping failed: %v", wrapped)
	}
}

// TestExitCodeForRunError pins the session-error → exit-code policy: local
// aborts are 6, token/handshake failures are configuration errors (4), peer
// aborts and everything orchestration-shaped are 5.
func TestExitCodeForRunError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ExitCode
	}{
		{"local abort", fmt.Errorf("%w: ctrl-c", peer.ErrLocalAbort), ExitInterrupt},
		{"context canceled", context.Canceled, ExitInterrupt},
		{"token rejected", fmt.Errorf("%w: %w", peer.ErrHandshakeFailed, peer.ErrTokenRejected), ExitConfig},
		{"handshake failed", fmt.Errorf("%w: dial refused", peer.ErrHandshakeFailed), ExitConfig},
		{"peer aborted", fmt.Errorf("%w: peer_lost", peer.ErrPeerAborted), ExitPeer},
		{"request timeout", fmt.Errorf("%w: op ping", peer.ErrRequestTimeout), ExitPeer},
		{"report write", fmt.Errorf("%w: disk full", errReportWrite), ExitInternal},
		{"plan failure", errors.New("peer: test plan failed: ethtool exploded"), ExitPeer},
	}
	for _, c := range cases {
		if got := exitCodeForRunError(c.err); got != c.want {
			t.Errorf("%s: exitCodeForRunError(%v) = %d, want %d", c.name, c.err, got, c.want)
		}
	}
}
