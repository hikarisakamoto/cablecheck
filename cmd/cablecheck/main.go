// Command cablecheck tests Ethernet cable health between two directly
// connected Linux PCs. main is deliberately thin: everything testable lives
// in internal/cli and below.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"cablecheck/internal/app"
	"cablecheck/internal/cli"
)

// Build identity, injected via -ldflags "-X main.version=... ...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
		app.BuildInfo{Version: version, Commit: commit, Date: date}))
}
