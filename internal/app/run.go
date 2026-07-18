package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"cablecheck/internal/clock"
	"cablecheck/internal/config"
	"cablecheck/internal/logging"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/reporting"
	"cablecheck/internal/runner"
	"cablecheck/internal/testsuite"
)

// suite bundles the constructed test components of one run.
type suite struct {
	ops     *testsuite.Ops
	results *testsuite.SessionResults
	plan    *testsuite.QuickPlan
	reg     *runner.Registry
}

// run executes the prepared run to completion: preflight, report directory,
// debug log, peer session with the quick plan, report assembly and exit-code
// mapping. It is the body of the goroutine Start launches.
func (a *App) run(ctx context.Context) (ExitCode, error) {
	defer a.closeListener()
	clk := a.deps.Clock
	baseLog := logging.NewStderr(a.deps.Stderr, a.cfg.Verbose)

	pf, err := a.preflight(ctx)
	if err != nil {
		return ExitConfig, err
	}

	startedAt := clk.Now()
	dir, err := reporting.NewReportDir(a.cfg.OutputDir, startedAt)
	if err != nil {
		return ExitConfig, err
	}
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		return ExitInternal, fmt.Errorf("%w: %w", errReportWrite, err)
	}
	log, logCloser, err := logging.AttachDebugFile(baseLog,
		filepath.Join(rawDir, "cablecheck-"+string(a.cfg.Role)+".log"))
	if err != nil {
		return ExitInternal, err
	}
	defer logCloser.Close()
	for _, w := range pf.Warnings {
		log.Warn("preflight warning", "warning", w)
	}
	// RunConfig implements slog.LogValuer with the token redacted, so this
	// cannot leak the secret into either sink.
	log.Info("run configuration", slog.Any("config", *a.cfg))

	if a.cfg.Role == config.RolePC1 {
		a.printTokenBanner()
	}

	s := a.buildSuite(pf, rawDir, clkNowID(clk), log)
	v := &verdict{}
	a.watch.onState = a.deps.hooks.onState
	sessionLog := slog.New(watchHandler{Handler: log.Handler(), w: &a.watch})

	pcfg := peer.Config{
		Role:              peer.Role(a.cfg.Role),
		LocalIP:           a.cfg.LocalIP,
		PeerIP:            a.cfg.PeerIP,
		ControlPort:       a.controlPort,
		Token:             a.cfg.Token,
		NonInteractive:    a.cfg.NonInteractive,
		NoReportTransfer:  a.cfg.NoReportTransfer,
		Version:           a.deps.Build.Version,
		Caps:              pf.Caps,
		Clock:             clk,
		Logger:            sessionLog,
		Stdin:             a.deps.Stdin,
		Stdout:            a.deps.Stdout,
		Mode:              string(a.cfg.Mode),
		Steps:             testsuite.QuickPlanSteps(),
		Complete:          v.complete,
		HeartbeatInterval: a.heartbeatInterval,
		IdleTimeout:       a.idleTimeout,
	}
	var plan peer.PlanFunc
	if a.cfg.Role == config.RolePC1 {
		pcfg.Transport = &preboundListener{ln: a.ln}
		plan = func(ctx context.Context, rc peer.RemoteCaller) error {
			if err := s.plan.Run(ctx, rc); err != nil {
				return err
			}
			// Assemble, evaluate and render before returning: the session
			// sends the complete frame (whose payload v.complete supplies)
			// as soon as the plan reports success.
			return a.finalize(dir, rawDir, pf, s.results, v, startedAt, nil, nil, log)
		}
	}

	outcome, runErr := peer.Run(ctx, pcfg, plan, serverOpDetacher{ops: s.ops})
	// An aborted run must leave no one-off iperf3 server behind, whatever
	// the session managed to clean up itself.
	s.ops.StopAllServers(context.WithoutCancel(ctx))
	if s.reg != nil {
		for _, kerr := range s.reg.KillAll(context.WithoutCancel(ctx)) {
			log.Warn("session process cleanup", "err", kerr)
		}
		_ = os.RemoveAll(s.reg.StateDir())
	}

	if a.cfg.Role == config.RolePC2 {
		return a.finishWorker(dir, &outcome, runErr, log)
	}
	return a.finishCoordinator(dir, rawDir, pf, s.results, v, startedAt, &outcome, runErr, log)
}

// serverOpDetacher adapts the worker op handler for the session: the
// iperf3_server_start op runs with its cancellation detached. The op
// executor cancels the op context the moment the op's result is sent, while
// Runner.Start ties the child's process group to the context it was started
// under — so a server started under the raw op context would be SIGTERMed
// right after the "ok" result, before the coordinator's client ever
// connects. The one-off server's lifecycle is owned explicitly instead: the
// iperf3_server_stop op, StopAllServers at session teardown, and the pidfile
// registry as the crash backstop.
type serverOpDetacher struct {
	// ops is the wrapped worker-side op handler.
	ops *testsuite.Ops
}

// HandleOp implements peer.OpHandler, detaching only iperf3_server_start
// from the per-op cancellation; every other op keeps its timeout and abort
// semantics. The detached start stays bounded by the server-readiness
// fallback inside the testsuite.
func (d serverOpDetacher) HandleOp(ctx context.Context, op string, params json.RawMessage,
	progress func(protocol.TestProgress)) (any, string, error) {
	if op == testsuite.OpIperfServerStart {
		ctx = context.WithoutCancel(ctx)
	}
	return d.ops.HandleOp(ctx, op, params, progress)
}

// closeListener releases PC1's control listener; harmless when the peer
// session already consumed and closed it.
func (a *App) closeListener() {
	if a.ln != nil {
		_ = a.ln.Close()
	}
}

// clkNowID mints a short session tag for pidfile registration: valid as a
// path element, unique enough for concurrent runs on one machine.
func clkNowID(clk clock.Clock) string {
	return fmt.Sprintf("run-%d-%d", clk.Now().Unix(), os.Getpid())
}

// buildSuite constructs the test components over the injected runner. The
// pidfile registry is only created when StateDir was not overridden: the
// registry always writes under the production state tree, and a test
// harness pointing StateDir elsewhere must not touch it. A registry that
// cannot be created does not stop the run, but the degraded safety is
// logged: unregistered iperf3 servers orphaned by a crash are invisible to
// the next run's stale-process preflight scan.
func (a *App) buildSuite(pf *preflightInfo, rawDir, tag string, log *slog.Logger) *suite {
	r := a.deps.Runner
	clk := a.deps.Clock
	var reg *runner.Registry
	var registrar testsuite.Registrar
	if a.deps.StateDir == "" {
		rg, err := runner.NewRegistry(tag, clk)
		if err != nil {
			log.Warn("pidfile registry unavailable; iperf3 servers run unregistered, so a crashed run would leave orphans the stale-process scan cannot detect", "err", err)
		} else {
			reg = rg
			registrar = rg
		}
	}
	ops := &testsuite.Ops{
		Counters:  &testsuite.CounterCollector{R: r, Clock: clk, IfName: pf.Iface.Name, SysfsRoot: a.sysfsRoot, RawDir: rawDir},
		Link:      &testsuite.LinkInspector{R: r, IfName: pf.Iface.Name, RawDir: rawDir},
		Ping:      &testsuite.PingTester{R: r, RawDir: rawDir},
		Iperf:     &testsuite.IperfManager{R: r, Reg: registrar, RawDir: rawDir, Clock: clk, TestID: tag},
		MTU:       pf.Iface.MTU,
		IperfCaps: pf.LocalIperfCaps,
	}
	results := &testsuite.SessionResults{}
	return &suite{
		ops:     ops,
		results: results,
		reg:     reg,
		plan: &testsuite.QuickPlan{
			Ops:            ops,
			LocalIP:        a.cfg.LocalIP,
			PeerIP:         a.cfg.PeerIP,
			IperfPort:      a.cfg.IperfPort,
			TCPDuration:    a.cfg.TCPDuration,
			UDPDuration:    a.cfg.UDPDuration,
			UDPRate:        a.cfg.UDPRate,
			MTU:            pf.Iface.MTU,
			Streams:        a.cfg.ParallelStreams,
			PingCount:      a.cfg.PingCount,
			LocalIperfCaps: pf.LocalIperfCaps,
			Results:        results,
			OnStep:         a.deps.OnStep,
		},
	}
}

// printTokenBanner displays the session token and the matching PC2 command
// line on stdout via fmt — deliberately never through slog, so the log file
// cannot contain the secret.
func (a *App) printTokenBanner() {
	suffix := ""
	if a.cfg.TokenGenerated {
		suffix = "  (auto-generated)"
	}
	fmt.Fprintf(a.deps.Stdout, "Session token: %s%s\nOn PC2 run:\n  cablecheck run --role pc2 --local-ip %s --peer-ip %s --token %s\n",
		a.cfg.Token, suffix, a.cfg.PeerIP.Unmap(), a.cfg.LocalIP.Unmap(), a.cfg.Token)
}
