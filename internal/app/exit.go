package app

import (
	"context"
	"errors"
	"fmt"

	"cablecheck/internal/model"
	"cablecheck/internal/peer"
)

// ExitCode is a cablecheck process exit code (the spec's 0-7 contract).
type ExitCode int

// The complete exit-code vocabulary.
const (
	// ExitOK: run completed, classification GOOD or EXCELLENT.
	ExitOK ExitCode = 0
	// ExitWarning: run completed, classification WARNING.
	ExitWarning ExitCode = 1
	// ExitPoorFailed: run completed, classification POOR or FAILED.
	ExitPoorFailed ExitCode = 2
	// ExitInconclusive: run completed, classification INCONCLUSIVE.
	ExitInconclusive ExitCode = 3
	// ExitConfig: configuration or dependency error, including a rejected
	// token and doctor failures.
	ExitConfig ExitCode = 4
	// ExitPeer: peer or orchestration failure (peer abort, disconnect,
	// request timeout).
	ExitPeer ExitCode = 5
	// ExitInterrupt: local interrupt (Ctrl+C, quit command, stdin EOF).
	ExitInterrupt ExitCode = 6
	// ExitInternal: internal error — anything unrecognized.
	ExitInternal ExitCode = 7
)

// ExitError carries an exit code through the error return chain so that
// app/peer code can decide codes without importing the cli layer. Err may be
// nil for a pure code carrier (e.g. a classification result).
type ExitError struct {
	// Code is the process exit code to use.
	Code ExitCode
	// Err is the underlying failure; nil when the code alone is the message.
	Err error
}

// Error formats the underlying error, or "exit N" for a bare carrier.
func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit %d", e.Code)
}

// Unwrap exposes the underlying error to errors.Is/As.
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCodeFor maps a health classification to its exit code. Unknown classes
// map to ExitInternal so a corrupted verdict can never fake success.
func ExitCodeFor(c model.HealthClass) ExitCode {
	switch c {
	case model.HealthExcellent, model.HealthGood:
		return ExitOK
	case model.HealthWarning:
		return ExitWarning
	case model.HealthPoor, model.HealthFailed:
		return ExitPoorFailed
	case model.HealthInconclusive:
		return ExitInconclusive
	default:
		return ExitInternal
	}
}

// errReportWrite marks a failure to assemble or write the report files; it
// maps to ExitInternal (the measurement worked, our own persistence broke).
var errReportWrite = errors.New("app: writing report failed")

// exitCodeForRunError maps a peer-session (or plan) error to the exit code
// contract: local aborts and cancellations are 6; token and handshake
// failures are configuration errors (4) per the peer package's contract;
// report persistence failures are internal (7); every other session error —
// peer abort, disconnect, request timeout, plan failure — is an
// orchestration failure (5).
func exitCodeForRunError(err error) ExitCode {
	switch {
	case errors.Is(err, peer.ErrLocalAbort), errors.Is(err, context.Canceled):
		return ExitInterrupt
	case errors.Is(err, peer.ErrTokenRejected):
		return ExitConfig
	case errors.Is(err, peer.ErrHandshakeFailed):
		return ExitConfig
	case errors.Is(err, errReportWrite):
		return ExitInternal
	default:
		return ExitPeer
	}
}
