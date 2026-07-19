package app

import (
	"log/slog"
	"net/netip"
	"slices"
	"testing"
	"time"

	"cablecheck/internal/config"
	"cablecheck/internal/model"
	"cablecheck/internal/testsuite"
)

// TestBuildSuiteSelectsPlanByMode pins that the run selects a real plan per
// mode: quick, standard and soak each install their own ordered step list on
// the session, so --mode standard and --mode soak no longer silently run the
// quick plan. The step lists are the observable proof — a StandardPlan
// announces repeated TCP and the extra reduced-rate UDP pass, a SoakPlan
// announces per-cycle steps — and they differ from the quick list.
func TestBuildSuiteSelectsPlanByMode(t *testing.T) {
	cases := []struct {
		mode      config.Mode
		wantSteps []string
	}{
		{config.ModeQuick, testsuite.QuickPlanSteps()},
		{config.ModeStandard, testsuite.StandardPlanSteps()},
		{config.ModeSoak, testsuite.SoakPlanSteps()},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := &config.RunConfig{
				Role:            config.RolePC1,
				LocalIP:         netip.MustParseAddr("127.0.0.1"),
				PeerIP:          netip.MustParseAddr("127.0.0.2"),
				Mode:            tc.mode,
				Token:           "testtoken1234",
				TCPDuration:     60 * time.Second,
				UDPDuration:     30 * time.Second,
				ParallelStreams: 4,
				PingCount:       1500,
				TCPRepeats:      2,
				SoakDuration:    time.Hour,
				SoakLoad:        config.SoakLoadPeriodic,
			}
			a, err := New(cfg, Deps{StateDir: t.TempDir()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
			pf := &preflightInfo{}
			pf.Iface.Name = "eth0"

			s := a.buildSuite(pf, t.TempDir(), "run-mode-test", log)
			if s.plan == nil {
				t.Fatalf("%s: buildSuite installed no plan", tc.mode)
			}
			if !slices.Equal(s.steps, tc.wantSteps) {
				t.Errorf("%s steps = %q, want the %s plan's step list %q", tc.mode, s.steps, tc.mode, tc.wantSteps)
			}
		})
	}

	// The three modes' step lists must be mutually distinct, so the report's
	// [n/N] progress genuinely reflects the executed workload.
	quick := testsuite.QuickPlanSteps()
	standard := testsuite.StandardPlanSteps()
	soak := testsuite.SoakPlanSteps()
	if slices.Equal(quick, standard) || slices.Equal(quick, soak) || slices.Equal(standard, soak) {
		t.Errorf("mode step lists are not distinct:\nquick=%q\nstandard=%q\nsoak=%q", quick, standard, soak)
	}
}

// TestCableTestPlanOptIn verifies the disruptive step is absent by default
// and appended exactly once for every mode when explicitly requested.
func TestCableTestPlanOptIn(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeQuick, config.ModeStandard, config.ModeSoak} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := &config.RunConfig{
				Role: config.RolePC1, LocalIP: netip.MustParseAddr("127.0.0.1"),
				PeerIP: netip.MustParseAddr("127.0.0.2"), Mode: mode,
				Token: "testtoken1234", CableTest: true,
				TCPDuration: time.Minute, UDPDuration: 30 * time.Second,
				ParallelStreams: 4, PingCount: 100, TCPRepeats: 2,
				SoakDuration: time.Hour, SoakLoad: config.SoakLoadPeriodic,
			}
			a, err := New(cfg, Deps{StateDir: t.TempDir()})
			if err != nil {
				t.Errorf("New: %v", err)
				return
			}
			pf := &preflightInfo{}
			pf.Iface.Name = "eth0"
			s := a.buildSuite(pf, t.TempDir(), "cable-plan", slog.Default())
			if got := s.steps[len(s.steps)-1]; got != "cable diagnostics" {
				t.Errorf("last step = %q, want cable diagnostics", got)
			}
			var base int
			switch mode {
			case config.ModeStandard:
				base = len(testsuite.StandardPlanSteps())
			case config.ModeSoak:
				base = len(testsuite.SoakPlanSteps())
			default:
				base = len(testsuite.QuickPlanSteps())
			}
			if len(s.steps) != base+1 {
				t.Errorf("steps = %q, want base length %d plus one", s.steps, base)
			}
		})
	}
}

// TestSoakReportReflectsModeAndCycles pins that a soak run's report echoes the
// soak mode and configuration and reports the number of completed cycles, so
// the report reflects what actually ran rather than only the requested mode.
func TestSoakReportReflectsModeAndCycles(t *testing.T) {
	cfg := &config.RunConfig{
		Role:         config.RolePC1,
		LocalIP:      netip.MustParseAddr("127.0.0.1"),
		PeerIP:       netip.MustParseAddr("127.0.0.2"),
		Mode:         config.ModeSoak,
		Token:        "testtoken1234",
		SoakDuration: 2 * time.Hour,
		SoakLoad:     config.SoakLoadContinuous,
	}
	a, err := New(cfg, Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	when := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	cycleCounters := []model.PeerCounters{{
		PC1: &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 3}},
		PC2: &model.CounterSnapshot{Standard: map[string]uint64{"rx_crc": 4}},
	}}
	results := &testsuite.SessionResults{CyclesCompleted: 7, CycleCounters: cycleCounters}
	rep := a.assembleReport(&preflightInfo{}, results, when, when.Add(time.Hour), nil, nil)

	if rep.Configuration.Mode != string(config.ModeSoak) {
		t.Errorf("report mode = %q, want %q", rep.Configuration.Mode, config.ModeSoak)
	}
	if rep.Configuration.SoakLoad != string(config.SoakLoadContinuous) {
		t.Errorf("report soakLoad = %q, want %q", rep.Configuration.SoakLoad, config.SoakLoadContinuous)
	}
	if rep.SoakCyclesCompleted != 7 {
		t.Errorf("report SoakCyclesCompleted = %d, want 7", rep.SoakCyclesCompleted)
	}
	if len(rep.CycleCounters) != 1 || rep.CycleCounters[0].PC2.Standard["rx_crc"] != 4 {
		t.Errorf("report CycleCounters = %+v, want the soak plan's per-cycle snapshots", rep.CycleCounters)
	}
}

// discardWriter drops all writes; used for a throwaway slog logger.
type discardWriter struct{}

// Write implements io.Writer, discarding everything.
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
