package app

import (
	"context"
	"sync"

	"cablecheck/internal/model"
	"cablecheck/internal/network"
)

// defaultMonitorInterval is the link-watch poll interval used when the config
// leaves MonitorInterval unset; quick mode wants a light 1 s watch.
const defaultMonitorInterval = 1_000_000_000 // 1s in nanoseconds

// startLinkMonitor begins polling the named interface's sysfs link state for
// the testing phase and attaches the monitor to the App so assembleReport can
// fold its History into the report's MonitoringEvents. It launches the poll
// loop on a joinable goroutine and records an idempotent stop function on the
// App; stopLinkMonitor cancels and joins it. Calling startLinkMonitor twice is
// a no-op after the first (the session may re-enter testing on a retry).
//
// It is deliberately started only once the session reaches the testing phase,
// so the monitor's clock ticker never registers before the synchronized-start
// countdown timer — keeping the FakeClock waiter accounting the countdown
// synchronization relies on unchanged.
func (a *App) startLinkMonitor(ctx context.Context, ifName string) {
	a.monitorMu.Lock()
	defer a.monitorMu.Unlock()
	if a.monitor != nil {
		return
	}
	interval := a.cfg.MonitorInterval
	if interval <= 0 {
		interval = defaultMonitorInterval
	}
	m := network.NewMonitor(ifName, interval, a.deps.Clock, network.WithSysfsRoot(a.sysfsRoot))
	a.monitor = m

	watchCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Run returns ctx.Err() on cancel; that is the expected exit, not a
		// failure to surface.
		_ = m.Run(watchCtx)
	}()

	var once sync.Once
	a.monitorStop = func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
}

// stopLinkMonitor cancels the link-watch poll loop and joins its goroutine. It
// is idempotent and safe to call when no monitor was started (a no-op), so
// both the PC1 plan closure — which stops it to freeze the timeline before
// assembling — and the run's deferred teardown can call it.
func (a *App) stopLinkMonitor() {
	a.monitorMu.Lock()
	stop := a.monitorStop
	a.monitorMu.Unlock()
	if stop != nil {
		stop()
	}
}

// monitoringEvents maps the link monitor's full event History onto the
// report's MonitoringEvent timeline. The LinkEventType string values are
// chosen to equal model.MonitoringEvent.Type (snake_case, e.g. "renegotiation",
// "speed_changed"), which is exactly what evaluate.renegotiations looks for, so
// the mapping is a direct field copy. It returns nil when no monitor ran.
func (a *App) monitoringEvents() []model.MonitoringEvent {
	a.monitorMu.Lock()
	m := a.monitor
	a.monitorMu.Unlock()
	if m == nil {
		return nil
	}
	hist := m.History()
	if len(hist) == 0 {
		return nil
	}
	events := make([]model.MonitoringEvent, 0, len(hist))
	for _, e := range hist {
		events = append(events, model.MonitoringEvent{
			At:     e.At.UTC(),
			Type:   string(e.Type),
			Detail: e.Detail,
		})
	}
	return events
}
