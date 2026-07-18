package cli

import (
	"context"
	"errors"
	"io"

	"cablecheck/internal/app"
)

// cmdDoctor is a stub: the local dependency checker ships in a later
// version. It maps to the internal-error exit code (7) for now.
func cmdDoctor(_ context.Context, _ []string, _, _ io.Writer) error {
	return &app.ExitError{
		Code: app.ExitInternal,
		Err:  errors.New("doctor is not implemented yet"),
	}
}
