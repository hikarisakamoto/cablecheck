package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/config"
	"cablecheck/internal/logging"
	"cablecheck/internal/model"
	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/reporting"
	"cablecheck/internal/runner"
	"cablecheck/internal/testsuite"
)

// suite bundles the constructed test components of one run. plan is the
// mode-selected coordinator test plan (quick, standard or soak); steps are its
// ordered display names for the session's progress step list.
type suite struct {
	ops     *testsuite.Ops
	results *testsuite.SessionResults
	plan    peer.PlanFunc
	steps   []string
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

	// Watch the tested interface's sysfs link state for the testing phase; its
	// History becomes the report's MonitoringEvents (carrier flaps, speed and
	// duplex changes, renegotiations). The monitor is started when the session
	// enters the testing phase (see OnState below) so its clock ticker never
	// registers before the synchronized-start countdown timer. stopLinkMonitor
	// cancels and joins the poll goroutine; it is idempotent and always called
	// on teardown so no goroutine leaks.
	defer a.stopLinkMonitor()

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
		Logger:            log,
		Stdin:             a.deps.Stdin,
		Stdout:            a.deps.Stdout,
		Mode:              string(a.cfg.Mode),
		Steps:             s.steps,
		Complete:          v.complete,
		OnHandshake:       a.setSessionTestID,
		OnCableTestWindow: a.setCableTestWindow,
		OnState: func(_, to peer.State) {
			// Start the sysfs link monitor exactly when the testing phase
			// begins, so its clock ticker registers after the countdown timer.
			if to == peer.StateTesting {
				a.startLinkMonitor(ctx, pf.Iface.Name)
			}
			if h := a.deps.hooks.onState; h != nil {
				h(to)
			}
		},
		HeartbeatInterval: a.heartbeatInterval,
		IdleTimeout:       a.idleTimeout,
	}
	// Report transfer is gated inside the peer session by NoReportTransfer and
	// the peer's AcceptReportTransfer capability. PC1 always re-evaluates with
	// the worker capabilities before that decision and before complete, then
	// streams its rendered report set when enabled; PC2 receives the verified
	// files into its own report directory.
	if a.cfg.Role == config.RolePC1 {
		pcfg.PrepareComplete = func(peerCaps protocol.Capabilities) error {
			err := a.finalize(dir, rawDir, pf, s.results, v, startedAt, nil,
				&peer.Outcome{PeerCaps: peerCaps, TestID: a.sessionTestID()}, log)
			if err == nil {
				v.markFinalized()
			}
			return err
		}
		pcfg.SendReports = a.sendReportsCallback(dir)
	} else {
		pcfg.ReceiveReports = a.receiveReportsCallback(dir)
	}
	var plan peer.PlanFunc
	if a.cfg.Role == config.RolePC1 {
		pcfg.Transport = &preboundListener{ln: a.ln}
		plan = func(ctx context.Context, rc peer.RemoteCaller) error {
			if err := s.plan(ctx, rc); err != nil {
				return err
			}
			// Freeze the link timeline before assembling: every render (this
			// provisional one and the PrepareComplete enrichment) must see the
			// same complete MonitoringEvents so the transferred report stays
			// byte-identical to PC1's final copy.
			a.stopLinkMonitor()
			// Assemble, evaluate and render before returning: the session
			// sends the complete frame (whose payload v.complete supplies)
			// as soon as the plan reports success. The report files must
			// exist here because PrepareComplete replaces them with the
			// capability-enriched rendering before any optional transfer.
			return a.finalize(dir, rawDir, pf, s.results, v, startedAt, nil, nil, log)
		}
	}

	outcome, runErr := peer.Run(ctx, pcfg, plan, serverOpDetacher{ops: s.ops})
	// Freeze and join the monitor before any success or partial report can be
	// assembled. On the successful coordinator path the plan already stopped
	// it before its provisional render; this idempotent call covers interrupts,
	// peer failures, aborts, and the worker path as well.
	a.stopLinkMonitor()
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
		Cable:     &testsuite.CableTester{R: r, IfName: pf.Iface.Name, RawDir: rawDir, SudoOK: pf.SudoOK},
		MTU:       pf.Iface.MTU,
		IperfCaps: pf.LocalIperfCaps,
	}
	results := &testsuite.SessionResults{}
	plan, steps := a.buildPlan(pf, ops, results)
	return &suite{
		ops:     ops,
		results: results,
		reg:     reg,
		plan:    plan,
		steps:   steps,
	}
}

// buildPlan selects the coordinator test plan by mode and returns it as a
// peer.PlanFunc together with its ordered step display names. quick uses
// QuickPlan, standard StandardPlan (longer pings, repeated TCP, an extra
// reduced-rate UDP pass, bidirectional stress), and soak SoakPlan (test cycles
// repeated for the soak wall-clock budget). All three satisfy peer.PlanFunc and
// drive the worker over the same RPCs.
func (a *App) buildPlan(pf *preflightInfo, ops *testsuite.Ops, results *testsuite.SessionResults) (peer.PlanFunc, []string) {
	switch a.cfg.Mode {
	case config.ModeStandard:
		p := &testsuite.StandardPlan{
			Ops:            ops,
			LocalIP:        a.cfg.LocalIP,
			PeerIP:         a.cfg.PeerIP,
			IperfPort:      a.cfg.IperfPort,
			TCPDuration:    a.cfg.TCPDuration,
			UDPDuration:    a.cfg.UDPDuration,
			UDPRate:        a.cfg.UDPRate,
			TCPRepeats:     a.cfg.TCPRepeats,
			MTU:            pf.Iface.MTU,
			Streams:        a.cfg.ParallelStreams,
			PingCount:      a.cfg.PingCount,
			LocalIperfCaps: pf.LocalIperfCaps,
			Results:        results,
			OnStep:         a.baseStepObserver(),
		}
		return a.wrapCablePlan(p.Run, testsuite.StandardPlanSteps(), ops, results)
	case config.ModeSoak:
		p := &testsuite.SoakPlan{
			Ops:            ops,
			Clock:          a.deps.Clock,
			LocalIP:        a.cfg.LocalIP,
			PeerIP:         a.cfg.PeerIP,
			IperfPort:      a.cfg.IperfPort,
			TCPDuration:    a.cfg.TCPDuration,
			UDPDuration:    a.cfg.UDPDuration,
			UDPRate:        a.cfg.UDPRate,
			TCPRepeats:     a.cfg.TCPRepeats,
			MTU:            pf.Iface.MTU,
			Streams:        a.cfg.ParallelStreams,
			PingCount:      a.cfg.PingCount,
			LocalIperfCaps: pf.LocalIperfCaps,
			SoakDuration:   a.cfg.SoakDuration,
			SoakLoad:       a.cfg.SoakLoad,
			CycleGap:       a.soakCycleGap(),
			EventSource:    a.monitorEventSnapshot,
			Results:        results,
			OnStep:         a.baseStepObserver(),
		}
		return a.wrapCablePlan(p.Run, testsuite.SoakPlanSteps(), ops, results)
	default:
		p := &testsuite.QuickPlan{
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
			OnStep:         a.baseStepObserver(),
		}
		return a.wrapCablePlan(p.Run, testsuite.QuickPlanSteps(), ops, results)
	}
}

// baseStepObserver adjusts base-mode progress totals when the opt-in cable
// diagnostic appends one final plan step.
func (a *App) baseStepObserver() func(step, total int, name string) {
	if a.deps.OnStep == nil {
		return nil
	}
	if !a.cfg.CableTest {
		return a.deps.OnStep
	}
	return func(step, total int, name string) {
		a.deps.OnStep(step, total+1, name)
	}
}

// wrapCablePlan leaves every mode untouched when diagnostics are disabled;
// when enabled it appends the coordinated disruptive step and display name.
func (a *App) wrapCablePlan(base peer.PlanFunc, steps []string, ops *testsuite.Ops,
	results *testsuite.SessionResults) (peer.PlanFunc, []string) {
	if !a.cfg.CableTest {
		return base, steps
	}
	steps = append(append([]string(nil), steps...), "cable diagnostics")
	plan := &testsuite.CablePlan{
		Base: base, Tester: ops.Cable, TDR: a.cfg.CableTestTDR, Results: results,
		NormalIdleTimeout: a.idleTimeout,
		BeginWindow:       func() { a.setCableTestWindow(true) },
		EndWindow:         func() uint64 { return a.setCableTestWindow(false) },
		OnStep:            a.deps.OnStep,
		Step:              len(steps),
		Total:             len(steps),
	}
	return plan.Run, steps
}

// soakCycleGap is the periodic soak load profile's idle window between test
// cycles: the link is left quiet for as long as one TCP phase stressed it (a
// conservative rest that still keeps the soak progressing), floored so a tiny
// configured TCP duration cannot collapse the gap to nothing. The continuous
// profile ignores this and runs cycles back-to-back.
func (a *App) soakCycleGap() time.Duration {
	gap := a.cfg.TCPDuration
	if gap < time.Second {
		gap = time.Second
	}
	return gap
}

// monitorEventSnapshot returns the sysfs link monitor's event timeline mapped
// onto the report model, or nil when no monitor is running. The soak plan calls
// it after each cycle so completed cycles retain their disconnect/renegotiation
// events even if a later cycle is interrupted.
func (a *App) monitorEventSnapshot() []model.MonitoringEvent {
	return a.monitoringEvents()
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
