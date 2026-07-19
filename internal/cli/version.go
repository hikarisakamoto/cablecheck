package cli

import (
	"fmt"
	"io"
	"runtime"

	"cablecheck/internal/app"
)

// cmdVersion prints the build identity plus the runtime, protocol and
// report-schema versions (docs/design/clieval.md §7 format).
func cmdVersion(w io.Writer, build app.BuildInfo) error {
	_, err := fmt.Fprintf(w,
		"cablecheck %s\ncommit:   %s\nbuilt:    %s\ngo:       %s\nplatform: %s/%s\nprotocol: %s\nschema:   %s\n",
		build.Version, build.Commit, build.Date,
		runtime.Version(), runtime.GOOS, runtime.GOARCH,
		app.ProtocolVersion, app.SchemaVersion)
	return err
}
