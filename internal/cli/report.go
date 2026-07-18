package cli

import (
	"context"
	"errors"
	"io"

	"cablecheck/internal/app"
)

// cmdReport is a stub: offline report regeneration from report.json ships
// in a later version. It maps to the internal-error exit code (7) for now.
func cmdReport(_ context.Context, _ []string, _, _ io.Writer) error {
	return &app.ExitError{
		Code: app.ExitInternal,
		Err:  errors.New("report regeneration is not implemented yet"),
	}
}
