package ui

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/protocol"
)

// Options configures a Renderer.
type Options struct {
	Color      ColorMode
	Verbose    bool
	Clock      clock.Clock
	Env        func(string) string
	Width      int
	SoakBudget time.Duration
}

// Renderer serializes progress state and output for one test run.
type Renderer struct {
	mu sync.Mutex
	// lifeMu guards the refresh goroutine lifecycle separately from rendered
	// state so Stop can join without holding mu.
	lifeMu sync.Mutex

	w          io.Writer
	clock      clock.Clock
	color      bool
	live       bool
	verbose    bool
	width      int
	soakBudget time.Duration

	started  bool
	start    time.Time
	preamble bool
	step     int
	total    int
	name     string
	progress bool
	percent  float64
	sub      string
	drawn    bool

	lifeState rendererLifeState
	ticker    clock.Ticker
	stop      chan struct{}
	done      chan struct{}
}

type rendererLifeState uint8

const (
	rendererIdle rendererLifeState = iota
	rendererRunning
	rendererStopping
	rendererStopped
)

// New returns a progress renderer writing to w.
func New(w io.Writer, opts Options) *Renderer {
	if w == nil {
		w = io.Discard
	}
	terminal := isTerminal(w)
	return newRenderer(w, opts, terminal)
}

// newRenderer is the test seam for the pre-decided live-rendering (trueTTY)
// capability; the color decision is made separately via DecideColor, so this
// terminal bool only gates the live-redraw path.
func newRenderer(w io.Writer, opts Options, terminal bool) *Renderer {
	if w == nil {
		w = io.Discard
	}
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	width := opts.Width
	if width == 0 {
		width = detectWidth(opts.Env)
	} else {
		width = clampWidth(width)
	}
	return newRendererDecided(w, opts, terminal, DecideColor(opts.Color, w, opts.Env), width)
}

// newRendererDecided isolates terminal and color capability decisions from
// stateful rendering so tests need no process-global terminal setup.
func newRendererDecided(w io.Writer, opts Options, terminal, colorEnabled bool, width int) *Renderer {
	return &Renderer{
		w:          w,
		clock:      opts.Clock,
		color:      colorEnabled,
		live:       terminal && !opts.Verbose && opts.Env("TERM") != "dumb",
		verbose:    opts.Verbose,
		width:      width,
		soakBudget: opts.SoakBudget,
	}
}

// ColorEnabled reports whether this renderer emits ANSI color sequences.
func (r *Renderer) ColorEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.color
}

// Start begins the clock-driven live refresh. Discrete renderers do not need
// a ticker: they print only real step/progress events, avoiding output floods
// in pipes, CI and verbose mode. Start is safe to call more than once.
func (r *Renderer) Start() {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	if r.lifeState != rendererIdle {
		return
	}
	if !r.live {
		r.lifeState = rendererStopped
		return
	}
	r.ticker = r.clock.NewTicker(time.Second)
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	r.lifeState = rendererRunning
	go r.refreshLoop(r.ticker, r.stop, r.done)
}

// Stop ends and joins the live refresh goroutine, then clears any unterminated
// live frame. It is idempotent and may be called after any run exit path.
func (r *Renderer) Stop() {
	r.lifeMu.Lock()
	switch r.lifeState {
	case rendererIdle:
		r.lifeState = rendererStopped
		r.lifeMu.Unlock()
		return
	case rendererStopped:
		r.lifeMu.Unlock()
		return
	case rendererRunning:
		r.lifeState = rendererStopping
		r.ticker.Stop()
		close(r.stop)
	}
	done := r.done
	r.lifeMu.Unlock()

	<-done
	r.finishLiveLine()

	r.lifeMu.Lock()
	r.lifeState = rendererStopped
	r.lifeMu.Unlock()
}

func (r *Renderer) refreshLoop(ticker clock.Ticker, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ticker.C():
			r.refresh()
		case <-stop:
			return
		}
	}
}

func (r *Renderer) refresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.validStep() {
		return
	}
	r.render(r.clock.Now())
}

// Step announces a test-plan step.
func (r *Renderer) Step(step, total int, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock.Now()
	if !r.started {
		r.started = true
		r.start = now
	}
	if r.verbose && !r.preamble {
		r.preamble = true
		fmt.Fprintf(r.w, "starting test plan: %d steps\n", total)
	}
	r.step, r.total, r.name = step, total, name
	r.progress, r.percent, r.sub = false, -1, ""
	r.render(now)
}

// Progress renders an in-flight operation update.
func (r *Renderer) Progress(p protocol.TestProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock.Now()
	if !r.started {
		r.started = true
		r.start = now
	}
	percent := p.Percent
	if percent >= 0 {
		if percent > 100 {
			percent = 100
		}
	}
	text := p.Text
	if text == "" {
		text = p.Stage
	}
	if metrics := formatMetrics(p.Metrics); metrics != "" {
		if text != "" {
			text += " "
		}
		text += metrics
	}
	r.progress, r.percent, r.sub = true, percent, text
	if !r.validStep() {
		return
	}
	r.render(now)
}

func (r *Renderer) validStep() bool {
	return r.step >= 1 && r.total >= 1 && r.step <= r.total
}

func (r *Renderer) render(now time.Time) {
	percent := float64(0)
	sub := ""
	if r.progress {
		percent = r.percent
		sub = r.sub
	}
	complete := overallFraction(r.step, r.total, percent)
	if r.soakBudget > 0 {
		complete = clampFraction(now.Sub(r.start).Seconds() / r.soakBudget.Seconds())
	}
	r.writeLine(renderBarFraction(r.step, r.total, r.width, r.name, sub, r.timing(now, complete), complete))
}

func (r *Renderer) timing(now time.Time, complete float64) string {
	elapsed := now.Sub(r.start)
	if elapsed < 0 {
		elapsed = 0
	}
	result := "elapsed " + formatDuration(elapsed)
	var eta time.Duration
	if r.soakBudget > 0 {
		eta = r.soakBudget - elapsed
	} else if complete > 0 && complete < 1 {
		// A tiny-but-positive completion fraction (e.g. a degenerate peer
		// percent) can push the projection past the int64 nanosecond range,
		// where the float64->Duration conversion wraps to a huge negative
		// value the eta<0 guard would then misreport as "ETA 0s". Omit the
		// ETA entirely when it is not representable rather than lie.
		raw := float64(elapsed) * (1 - complete) / complete
		if raw >= float64(math.MaxInt64) {
			return result
		}
		eta = time.Duration(raw)
	} else {
		return result
	}
	if eta < 0 {
		eta = 0
	}
	return result + " ETA " + formatDuration(eta)
}

func (r *Renderer) writeLine(line string) {
	if r.live {
		fmt.Fprint(r.w, "\r\x1b[2K", colorize(line, "\x1b[36m", r.color))
		r.drawn = true
		return
	}
	line = strings.TrimRight(line, " ")
	fmt.Fprintln(r.w, colorize(line, "\x1b[36m", r.color))
}

func (r *Renderer) finishLiveLine() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.live || !r.drawn {
		return
	}
	fmt.Fprint(r.w, "\r\x1b[2K\n")
	r.drawn = false
}

func overallFraction(step, total int, percent float64) float64 {
	if total <= 0 {
		return 0
	}
	completed := float64(step - 1)
	if completed < 0 {
		completed = 0
	}
	if percent >= 0 {
		completed += clampFraction(percent / 100)
	}
	return clampFraction(completed / float64(total))
}

func formatMetrics(metrics map[string]float64) string {
	if len(metrics) == 0 {
		return ""
	}
	keys := make([]string, 0, len(metrics))
	for key := range metrics {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%g", key, metrics[key]))
	}
	return strings.Join(parts, " ")
}

func formatDuration(value time.Duration) string {
	return value.Round(time.Second).String()
}
