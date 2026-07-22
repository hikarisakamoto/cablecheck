package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/model"
	"cablecheck/internal/protocol"
	"cablecheck/internal/testutil"
)

type notifyingBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes chan struct{}
}

func newNotifyingBuffer() *notifyingBuffer {
	return &notifyingBuffer{writes: make(chan struct{}, 16)}
}

func (b *notifyingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.writes <- struct{}{}:
	default:
	}
	return n, err
}

func (b *notifyingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRendererElapsedETAAndMetrics(t *testing.T) {
	start := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clk := clocktest.New(start)
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Clock: clk, Width: 120, Verbose: true}, false)
	r.Step(1, 2, "throughput")
	clk.Advance(10 * time.Second)
	r.Progress(protocol.TestProgress{
		Stage:   "fallback",
		Percent: 50,
		Text:    "running",
		Metrics: map[string]float64{"z": 2, "a": 1.5},
	})

	got := out.String()
	for _, want := range []string{
		"starting test plan: 2 steps\n",
		"[1/2] throughput",
		"running a=1.5 z=2",
		"elapsed 10s ETA 30s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fallback") {
		t.Fatalf("Text did not take precedence over Stage: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("ColorNever emitted ANSI: %q", got)
	}
}

func TestRendererSoakBudget(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Clock: clk, Width: 80, SoakBudget: time.Minute}, false)
	r.Step(1, 8, "soak")
	clk.Advance(15 * time.Second)
	r.Progress(protocol.TestProgress{Stage: "cycle", Percent: 1})
	if got := out.String(); !strings.Contains(got, "elapsed 15s ETA 45s") {
		t.Fatalf("soak timing did not override protocol progress: %q", got)
	}
}

func TestRendererLiveAndDiscrete(t *testing.T) {
	env := func(key string) string {
		if key == "TERM" {
			return "xterm"
		}
		return ""
	}
	for _, tc := range []struct {
		name        string
		terminal    bool
		verbose     bool
		color       ColorMode
		wantErase   bool
		wantColor   bool
		wantNewline bool
	}{
		{"live", true, false, ColorNever, true, false, false},
		{"live-color", true, false, ColorAlways, true, true, false},
		{"verbose-terminal", true, true, ColorNever, false, false, true},
		{"forced-color-pipe", false, false, ColorAlways, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := newRenderer(&out, Options{Color: tc.color, Verbose: tc.verbose, Env: env, Width: 40}, tc.terminal)
			r.Step(1, 2, "name")
			got := out.String()
			if strings.Contains(got, "\x1b[2K") != tc.wantErase {
				t.Fatalf("erase mismatch: %q", got)
			}
			if strings.Contains(got, "\x1b[36m") != tc.wantColor {
				t.Fatalf("color mismatch: %q", got)
			}
			if tc.wantColor {
				if tc.wantErase {
					// Live + color: erase sequence, then the SGR-wrapped
					// payload, and no terminating newline.
					if !strings.HasPrefix(got, "\r\x1b[2K\x1b[36m") || !strings.HasSuffix(got, "\x1b[0m") || strings.HasSuffix(got, "\n") {
						t.Fatalf("live color sequence mismatch: %q", got)
					}
				} else if !strings.HasPrefix(got, "\x1b[36m") || !strings.HasSuffix(got, "\x1b[0m\n") {
					t.Fatalf("forced color sequence mismatch: %q", got)
				}
			}
			if strings.HasSuffix(got, "\n") != tc.wantNewline {
				t.Fatalf("newline mismatch: %q", got)
			}
			if !strings.Contains(got, "[1/2] name") {
				t.Fatalf("plain label missing: %q", got)
			}
		})
	}
}

func TestRendererDumbTerminalDoesNotAnimate(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorAuto, Env: func(key string) string {
		if key == "TERM" {
			return "dumb"
		}
		return ""
	}}, true)
	r.Step(1, 1, "done")
	if got := out.String(); strings.Contains(got, "\x1b[") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("dumb terminal output = %q", got)
	}
}

func TestRendererForcedColorPipeIsDiscrete(t *testing.T) {
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readPipe.Close() })

	r := New(writePipe, Options{Color: ColorAlways, Width: 80})
	r.Step(1, 1, "pipe")
	if err := writePipe.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(readPipe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("\x1b[36m")) || bytes.Contains(got, []byte("\x1b[2K")) {
		t.Fatalf("forced-color pipe output = %q", got)
	}
	if !bytes.HasSuffix(got, []byte("\n")) {
		t.Fatalf("pipe output is not discrete: %q", got)
	}
}

func TestRendererWidthOverrideIsClamped(t *testing.T) {
	for _, tc := range []struct {
		width int
		want  int
	}{
		{-1, 40},
		{20, 40},
		{120, 120},
		{300, 200},
	} {
		r := newRenderer(nil, Options{Width: tc.width}, false)
		if r.width != tc.want {
			t.Fatalf("width override %d became %d, want %d", tc.width, r.width, tc.want)
		}
	}
}

func TestRendererConcurrentCalls(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Width: 80}, false)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(2)
		go func(step int) {
			defer wg.Done()
			r.Step(step, 20, "step")
		}(i)
		go func(percent float64) {
			defer wg.Done()
			r.Progress(protocol.TestProgress{Stage: "running", Percent: percent})
		}(float64(i * 5))
	}
	wg.Wait()
	if out.Len() == 0 {
		t.Fatal("concurrent renderer produced no output")
	}
}

func TestRendererClockDrivenLiveRefresh(t *testing.T) {
	defer testutil.LeakCheck(t)
	clk := clocktest.New(time.Unix(0, 0))
	out := newNotifyingBuffer()
	r := newRenderer(out, Options{
		Color: ColorNever,
		Clock: clk,
		Env: func(key string) string {
			if key == "TERM" {
				return "xterm"
			}
			return ""
		},
		Width: 120,
	}, true)
	r.Start()
	r.Step(1, 2, "throughput")
	<-out.writes // initial Step write
	r.Progress(protocol.TestProgress{Stage: "iperf3_client_run", Percent: -1, Text: "running iperf3_client_run"})
	<-out.writes // initial Progress write

	clk.BlockUntilWaiters(1)
	clk.Advance(2 * time.Second)
	testutil.WaitFor(t, out.writes, "live elapsed refresh")
	if got := out.String(); !strings.Contains(got, "elapsed 2s") || !strings.Contains(got, "running iperf3_client_run") {
		t.Fatalf("clock refresh did not advance elapsed time: %q", got)
	}

	r.Stop()
	afterStop := out.String()
	clk.Advance(5 * time.Second)
	if got := out.String(); got != afterStop {
		t.Fatalf("stopped renderer wrote after a later clock advance:\nbefore %q\nafter  %q", afterStop, got)
	}
	if !strings.HasSuffix(afterStop, "\r\x1b[2K\n") {
		t.Fatalf("Stop did not clear and terminate the live line: %q", afterStop)
	}
	r.Stop() // idempotent
}

func TestRendererDiscreteModesDoNotStartTicker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		verbose  bool
		term     string
	}{
		{name: "pipe", terminal: false, term: "xterm"},
		{name: "verbose", terminal: true, verbose: true, term: "xterm"},
		{name: "dumb", terminal: true, term: "dumb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.New(time.Unix(0, 0))
			r := newRenderer(io.Discard, Options{
				Clock: clk, Verbose: tc.verbose,
				Env: func(string) string { return tc.term },
			}, tc.terminal)
			r.Start()
			r.lifeMu.Lock()
			state, ticker := r.lifeState, r.ticker
			r.lifeMu.Unlock()
			if state != rendererStopped || ticker != nil {
				t.Fatalf("discrete renderer lifecycle = (%v, %v), want stopped with no ticker", state, ticker)
			}
			r.Stop()
		})
	}
}

func TestRendererProgressBeforeFirstStepIsSuppressed(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Clock: clk, Width: 120, SoakBudget: time.Minute}, false)
	r.Progress(protocol.TestProgress{Stage: "setup", Percent: -1})
	if got := out.String(); got != "" {
		t.Fatalf("pre-step progress rendered invalid step metadata: %q", got)
	}
	clk.Advance(5 * time.Second)
	r.Step(1, 5, "cycle 1: counters")
	got := out.String()
	if strings.Contains(got, "[0/0]") || !strings.Contains(got, "[1/5] cycle 1: counters") {
		t.Fatalf("first valid soak step output = %q", got)
	}
	if !strings.Contains(got, "elapsed 5s ETA 55s") {
		t.Fatalf("pre-step progress did not preserve soak elapsed origin: %q", got)
	}
}

func TestRendererPreservesLegacyStepSubstrings(t *testing.T) {
	steps := []string{"link settings", "initial counter snapshot", "TCP throughput PC1 → PC2"}
	for _, verbose := range []bool{false, true} {
		var out bytes.Buffer
		r := newRenderer(&out, Options{Color: ColorNever, Width: 120, Verbose: verbose}, false)
		for i, name := range steps {
			r.Step(i+1, len(steps), name)
		}
		for i, name := range steps {
			want := fmt.Sprintf("[%d/%d] %s", i+1, len(steps), name)
			if !strings.Contains(out.String(), want) {
				t.Errorf("verbose=%v output missing %q:\n%s", verbose, want, out.String())
			}
		}
		if verbose && !strings.Contains(out.String(), "starting test plan: 3 steps\n") {
			t.Errorf("verbose output missing plan preamble:\n%s", out.String())
		}
	}
}

func TestRendererPlaceholdersAndDefaults(t *testing.T) {
	r := New(nil, Options{})
	if r.ColorEnabled() {
		t.Fatal("discard writer unexpectedly enabled color")
	}
	r.Summary((*model.Report)(nil), "")
	r.TokenBanner("secret-token", "pc2 command", true)
	r.Step(1, 1, "safe")
}

// TestOverallFraction pins the overall-progress math, including the
// indeterminate (percent < 0) branch, which must contribute nothing.
func TestOverallFraction(t *testing.T) {
	for _, tc := range []struct {
		step, total int
		percent     float64
		want        float64
	}{
		{1, 2, -1, 0.0},   // step 1, indeterminate: completed 0, no add
		{2, 4, -1, 0.25},  // step 2, indeterminate: (2-1)/4, no add
		{2, 4, 50, 0.375}, // (1 + 0.5) / 4
		{1, 0, 50, 0.0},   // zero total
		{5, 4, 100, 1.0},  // clamped to 1
	} {
		if got := overallFraction(tc.step, tc.total, tc.percent); got != tc.want {
			t.Errorf("overallFraction(%d, %d, %v) = %v, want %v", tc.step, tc.total, tc.percent, got, tc.want)
		}
	}
}

// TestRendererIndeterminateProgress checks that a Percent == -1 update (the
// documented indeterminate value) does not advance the bar: the fraction stays
// at the completed-steps value, so the ETA is derived from that alone.
func TestRendererIndeterminateProgress(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Clock: clk, Width: 120}, false)
	r.Step(2, 4, "phase")
	out.Reset()
	clk.Advance(5 * time.Second)
	r.Progress(protocol.TestProgress{Stage: "working", Percent: -1})
	got := out.String()
	// complete stays at (2-1)/4 = 0.25, so ETA = 5s * (0.75/0.25) = 15s.
	for _, want := range []string{"working", "elapsed 5s ETA 15s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("indeterminate progress missing %q: %q", want, got)
		}
	}
}

// TestRendererTimingBranches covers the elapsed-only (no ETA) branch and the
// over-budget soak clamp that renders "ETA 0s".
func TestRendererTimingBranches(t *testing.T) {
	t.Run("start frame is elapsed-only", func(t *testing.T) {
		clk := clocktest.New(time.Unix(0, 0))
		var out bytes.Buffer
		r := newRenderer(&out, Options{Color: ColorNever, Clock: clk, Width: 120}, false)
		r.Step(1, 2, "throughput")
		if got := out.String(); !strings.Contains(got, "elapsed 0s") || strings.Contains(got, " ETA ") {
			t.Fatalf("start frame should carry elapsed but no ETA: %q", got)
		}
	})

	t.Run("over-budget soak clamps ETA to zero", func(t *testing.T) {
		clk := clocktest.New(time.Unix(0, 0))
		var out bytes.Buffer
		r := newRenderer(&out, Options{Color: ColorNever, Clock: clk, Width: 120, SoakBudget: time.Minute}, false)
		r.Step(1, 4, "soak")
		clk.Advance(90 * time.Second)
		r.Progress(protocol.TestProgress{Stage: "cycle", Percent: 50})
		if got := out.String(); !strings.Contains(got, "ETA 0s") {
			t.Fatalf("over-budget soak should clamp ETA to 0s: %q", got)
		}
	})
}

// TestRendererTinyPercentDoesNotOverflowETA guards the ETA projection against a
// degenerate near-zero completion fraction: the int64-overflowing projection
// must be omitted, not wrapped into a misleading "ETA 0s".
func TestRendererTinyPercentDoesNotOverflowETA(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	var out bytes.Buffer
	r := newRenderer(&out, Options{Color: ColorNever, Clock: clk, Width: 120}, false)
	r.Step(1, 1, "phase")
	clk.Advance(3 * time.Second)
	r.Progress(protocol.TestProgress{Stage: "x", Percent: 1e-9})
	got := out.String()
	if !strings.Contains(got, "elapsed 3s") {
		t.Fatalf("missing elapsed: %q", got)
	}
	if strings.Contains(got, " ETA ") {
		t.Fatalf("unrepresentable ETA must be omitted, not wrapped: %q", got)
	}
}
