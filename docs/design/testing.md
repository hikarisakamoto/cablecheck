# CableCheck — Testing & Verification Design

Scope: all `*_test.go`, mock infrastructure, integration harness, Makefile, CI gates, local e2e demo. Everything automated is hermetic: no iperf3, no ethtool, no real NIC, no dependence on local `ping`/`ip`. `go test ./...` and `go test -race ./...` are hard gates.

Test evidence is the project's highest engineering priority. Prefer deterministic unit tests
for pure transforms, white-box integration tests for orchestration and lifecycle ordering,
race/shuffle runs for concurrency, and the loopback demo for the built binary. A feature is
not complete merely because its happy path works manually; every practical fallback and
failure branch must be pinned without real tools, real networks, or timing sleeps.

---

## 0. Test-support package layout (decided)

```
internal/clock/               Clock interface (prod)
internal/clock/clocktest/     FakeClock
internal/runner/runnertest/   FakeRunner, FakeProcess, ArgMatchers
internal/testutil/            leakcheck, Dribbler, waitFor, stdin scripting, testTimeout
internal/app/integration_test.go   (package app — white-box, see §3)
testdata/{ip,ethtool,ping,iperf,golden,stubtools}/
tools/genexamples/            hermetic example-report generator (main pkg, run by make)
```

Test helpers live in normal internal packages, importable by every `_test.go` but never imported by the binary. `go vet` and the fact that `cmd/cablecheck` compiles without them enforce that. `testdata/` holds only fixtures, never Go code.

---

## 1. Mock infrastructure

### 1.1 Contract interfaces these mocks implement (pins the cross-package API)

```go
// internal/runner
type CommandSpec struct {
    Name      string        // bare executable name; resolved via LookPath; NO shell ever
    Args      []string
    Timeout   time.Duration // 0 = ctx only
    MaxOutput int64         // per-stream in-memory cap
    RawStdout io.Writer     // optional tee into report raw/ dir
    RawStderr io.Writer
}

type CommandResult struct {
    Stdout, Stderr []byte
    ExitCode       int
    TimedOut       bool
    Truncated      bool
    Duration       time.Duration
}

type Process interface {
    Wait(ctx context.Context) (CommandResult, error)
    Terminate() error // SIGTERM (process group)
    Kill() error      // SIGKILL (process group)
    PID() int
}

type CommandRunner interface {
    Run(ctx context.Context, spec CommandSpec) (CommandResult, error)
    Start(ctx context.Context, spec CommandSpec) (Process, error) // long-lived: iperf3 -s
    LookPath(name string) (string, error)
}

// internal/clock  — Now/After per spec, plus NewTicker: heartbeats and the
// monitor poll loop need a ticker; faking After-in-a-loop is racier than a ticker.
type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time
    NewTicker(d time.Duration) Ticker
}
type Ticker interface{ C() <-chan time.Time; Stop() }
```

**Interface requirement flagged to other design agents (load-bearing for testability):** iperf3-server readiness must be observable through the `Process` handle (started + has-not-exited) plus a `Clock.After` grace period. It must not depend on scraping server stdout mid-run or dialing the iperf port. Otherwise the FakeProcess can't express readiness and every TCP-test unit test needs a real listener.

### 1.2 FakeRunner (`internal/runner/runnertest`)

```go
type ArgMatcher func(args []string) bool
func ArgsExact(args ...string) ArgMatcher
func ArgsPrefix(prefix ...string) ArgMatcher
func ArgsContain(tokens ...string) ArgMatcher   // subsequence, order-independent tokens
func AnyArgs() ArgMatcher

type Script struct {
    Name       string
    Match      ArgMatcher
    Result     runner.CommandResult
    StdoutFile string          // load Result.Stdout from a testdata path (fixtures)
    Err        error           // e.g. simulate start failure
    Delay      <-chan struct{} // non-nil: Run/Wait blocks until closed OR ctx.Done()
    Started    chan<- struct{} // non-nil: closed when the call begins (sync point)
    Times      int             // 0 = unlimited; >0 consumed in order (before/after snapshots)
}

type RecordedCall struct {
    Name string
    Args []string
    Spec runner.CommandSpec
    Kind string // "run" | "start"
}

type FakeRunner struct{ /* mutex-guarded */ }
func New(t testing.TB) *FakeRunner
func (f *FakeRunner) Script(s Script) *FakeRunner            // chainable
func (f *FakeRunner) Missing(names ...string)                // LookPath → exec.ErrNotFound
func (f *FakeRunner) Calls() []RecordedCall
func (f *FakeRunner) CallsFor(name string) []RecordedCall
func (f *FakeRunner) Processes() []*FakeProcess              // in Start order
```

Semantics (decided):
- **Unmatched call ⇒ test failure**, via `t.Errorf` + returned `error`. Never `t.Fatalf`: FakeRunner is called from app goroutines, and `Fatal` off the test goroutine is undefined behavior.
- Matching order: most-recently-added `Script` wins for equal specificity; `Times`-limited scripts are consumed FIFO. This is how before/after counter snapshots are scripted: two `Times:1` scripts for `ethtool -S eth0` returning different fixtures.
- All state is mutex-guarded, since the runner is hit from multiple app goroutines under `-race`.
- `Delay` + `Started` channels are the cancellation-test mechanism. The test waits on `Started` (knows we're mid-iperf3), cancels ctx, and asserts the kill. **No sleeps.**

### 1.3 FakeProcess

```go
type FakeProcess struct{ /* internal sync */ }
func (p *FakeProcess) Wait(ctx context.Context) (runner.CommandResult, error)
func (p *FakeProcess) Terminate() error
func (p *FakeProcess) Kill() error
func (p *FakeProcess) PID() int

// Test-side observation & control:
func (p *FakeProcess) Terminated() bool
func (p *FakeProcess) Killed() bool
func (p *FakeProcess) TermCh() <-chan struct{}     // closed on first Terminate
func (p *FakeProcess) KillCh() <-chan struct{}
func (p *FakeProcess) Exit(res runner.CommandResult) // makes Wait return
```

Default behavior: `Wait` blocks until `Exit()` or `Terminate`/`Kill` (which synthesize `ExitCode:-1` results) or ctx cancel. Scripted via `Script{Kind: start}` fields (`ExitOnTerminate bool`, default true).

### 1.4 FakeClock (`internal/clock/clocktest`)

```go
type FakeClock struct{ /* ... */ }
func New(start time.Time) *FakeClock
func (c *FakeClock) Now() time.Time
func (c *FakeClock) After(d time.Duration) <-chan time.Time
func (c *FakeClock) NewTicker(d time.Duration) clock.Ticker
func (c *FakeClock) Advance(d time.Duration)      // fires all due waiters/ticks
func (c *FakeClock) BlockUntilWaiters(n int)      // parks until ≥n goroutines wait on After/tick
```

`BlockUntilWaiters` is mandatory before every `Advance` that's supposed to wake a goroutine. It eliminates the classic lost-wakeup race, where the advance runs before the goroutine calls `After`. Implemented with a condvar over the waiter count.

**Where real time is tolerated (decided rule):**
- `net.Conn` Set{Read,Write}Deadline are kernel/runtime-enforced absolute times, so they're unfakeable. Deadlines exist to catch genuine hangs; tests set them generously (≥30s or derived from `t.Deadline`), and no test *waits* for one to fire. The one test that verifies deadline behavior (`protocol` read-deadline test) uses a real 50ms deadline on `net.Pipe`, which supports deadlines, and asserts a timeout error. That's a bounded wait, not a sync sleep.
- `exec` timeouts (real runner) use real `context.WithTimeout`. The real-runner timeout test uses a helper process that blocks and a 100ms timeout, bounded by design and isolated to one test.
- Everything else goes through `Clock` and is faked: heartbeat scheduling, monitor polling, countdown, report timestamps, "longest gap" math.

### 1.5 Scripted stdin / Prompt

The prompt component **must** take `io.Reader`, never touch `os.Stdin`:

```go
// internal/cli
type PromptCmd int // CmdStart, CmdQuit, CmdStatus
type Prompt struct{ /* ... */ }
func NewPrompt(in io.Reader, out io.Writer) *Prompt
func (p *Prompt) ReadCommand(ctx context.Context) (PromptCmd, error) // EOF ⇒ CmdQuit
func (p *Prompt) Close() error
```

Implementation note that tests depend on: a blocked `Read` isn't ctx-cancellable, so `Prompt` runs one pump goroutine feeding a line channel and `ReadCommand` selects on ctx + channel. Tests inject either `strings.NewReader("start\n")` (EOF ends the pump, so no leak) or an `io.Pipe` when arrival *timing* matters (the test closes the pipe writer in cleanup so the pump exits and leakcheck passes). `testutil.ScriptStdin(t, lines ...string) io.Reader` wraps the pipe pattern with automatic cleanup.

### 1.6 testutil

```go
func LeakCheck(t testing.TB)                       // defer at test start; see §4
func Dribble(r io.Reader, maxChunk int) io.Reader  // returns 1..maxChunk bytes per Read
func WaitFor(t testing.TB, ch <-chan struct{}, msg string) // select ch vs testTimeout
func TestTimeout(t testing.TB) time.Duration       // from t.Deadline minus 2s grace, floor 10s
```

---

## 2. Unit test matrix (per package; names are the actual test functions)

### internal/config
- `TestParseBitrate` — table: `80M→80e6`, `800M`, `1G→1e9`, `2.5G→2.5e9`, `100K`, bare `5000000`; rejects: empty, `-1G`, `0`, `1.5X`, `G`, overflow (`999999999G`), whitespace. Decimal units (1M=1e6) per spec.
- `TestValidateIPs` — bad syntax, IPv6 rejected with "IPv4 only" message (netip.Addr.Is4), local==peer, unspecified `0.0.0.0`, multicast.
- `TestValidatePorts` — 0/65536/negative, control==iperf conflict.
- `TestValidateDurations` — soak `30s|10m|1h|4h` accepted, `0s` rejected, garbage rejected; tcp/udp duration bounds.
- `TestValidateEnums` — role, mode, soak-load.
- `TestModeDefaults` — quick/standard/soak fill correct default durations; explicit flags override.
- `TestTokenLimits` — empty→"generate later" sentinel, >128 bytes rejected, control chars rejected.
- `TestFlagSetIsolated` — parse twice in one process (must use `flag.NewFlagSet`, not `flag.CommandLine`; catches global-flag registration panic).
- `TestSubcommandDispatch` — `run|doctor|report|version|bogus`; bogus → usage error mapping to exit 4.

### internal/protocol
- `TestFrameRoundTrip` — encode/decode over `bytes.Buffer`.
- `TestFramePartialReads` — `net.Pipe` + `testutil.Dribble(conn, 3)`; also a split exactly after the 4-byte header. **Never assume 1 Read = 1 message** is proven here.
- `TestFrameMaxSize` — length == max OK; max+1 → typed `ErrFrameTooLarge` and reader refuses further use; header `0xFFFFFFFF` rejected *before* allocation (no 4GiB alloc — assert via a length check, not memory).
- `TestFrameZeroLength` — rejected (decided: zero-length frames invalid).
- `TestFrameTruncatedStream` — writer closes mid-body → `io.ErrUnexpectedEOF`.
- `TestEnvelopeVersionMismatch` — typed `ErrProtocolVersion{Got,Want}`.
- `TestEnvelopeUnknownType` — tolerated: returned with raw payload, caller policy = log-and-ignore (asserted at peer level).
- `TestEnvelopeDuplicateMessageID` — dedup window drops repeat.
- `TestPayloadDecodeStrictEnvelopeLenientPayload` — envelope unknown fields rejected, payload unknown fields tolerated (forward compat, decided).
- `TestTokenCompare` — equal, unequal, different lengths (no panic, false), empty both; implementation must route through `crypto/subtle.ConstantTimeCompare` (constant-timeness itself is a code-review gate, noted in test comment).
- `TestReadDeadline` — real 50ms deadline on `net.Pipe`, blocked read → `os.ErrDeadlineExceeded` mapped to typed idle-timeout error.
- `TestHeartbeatScheduling` — FakeClock ticker; advance 3 intervals ⇒ 3 heartbeats written (reader on other pipe end).

### internal/peer (state machine + handshake)
- `TestTransitionTable` — table-driven full matrix: every (state, event) pair asserted as either (nextState) or `ErrInvalidTransition`; includes: `start_confirmation` during handshake ✗, `test_result` outside testing ✗, duplicate `ready` ✗/idempotent (decided: error), any event in `completed|aborted|failed` ✗ except no-ops.
- `TestAbortFromEveryState` — abort legal from all non-terminal states → aborted.
- `TestHandshakeHappyPath` — two peer FSMs over `net.Pipe`: hello/hello_ack/capabilities; asserts capability struct exchanged.
- `TestHandshakeTokenMismatch` — rejection message + connection close + coordinator returns to listening (or terminal, per peer design — test pins whichever the peer agent decided; scenario also covered in integration §3).
- `TestHeartbeatLossDetection` — FakeClock; no heartbeat for idle-timeout ⇒ peer-lost event emitted exactly once.
- `TestUnknownMessageTolerated` — unknown type mid-session does not abort.

### internal/runner (the REAL exec implementation — hermetic via self-re-exec)
Uses the standard helper-process pattern: `os.Executable()` re-exec with `GO_HELPER_MODE=<case>` env; `TestMain` intercepts. No `/bin/sh`, no external tools.
- `TestRunCapturesOutput` — helper writes to stdout+stderr; separation asserted.
- `TestRunExitCode` — helper exits 3.
- `TestRunMissingExecutable` — `LookPath("definitely-not-here-xyz")` → typed `ErrToolNotFound` (distinct from failure).
- `TestRunTimeoutVsFailure` — helper sleeps; 100ms timeout ⇒ `TimedOut:true`, and exit-code-nonzero case ⇒ `TimedOut:false`.
- `TestRunMaxOutputTruncation` — helper emits 1MiB; cap 4KiB ⇒ `Truncated:true`, len ≤ cap.
- `TestRunNoShellInterpretation` — args `["a b", "$(reboot)", ";", "&&"]`; helper echoes `os.Args` verbatim; asserted literal.
- `TestStartTerminateDeliversSIGTERM` — helper traps SIGTERM → exits 42; `Terminate()` then `Wait` sees 42.
- `TestStartKill` — SIGKILL path, `Wait` returns signal-death result.
- `TestContextCancelKillsChild` — cancel ctx; helper (which would run 10min) is dead: `Wait` returns promptly (bounded by TestTimeout, no sleep).
- `TestProcessGroupKill` — helper spawns a grand-child helper writing heartbeat lines to a temp file; kill parent; assert file stops growing within bounded poll (verifies `Setpgid` + negative-pid kill). Marked as the one “systems” test; generous bounds.
- `TestRawStreamTee` — RawStdout writer receives bytes even when in-memory cap truncates.

### internal/network (interface discovery)
All from `testdata/ip/*.json` fixtures through FakeRunner — never the real `ip`.
- `TestDiscoverByIP` — single-eth fixture, IP owned → iface, MTU, operstate populated.
- `TestIPNotOnMachine` — error message lists candidate addresses.
- `TestLoopbackRejectedWithoutFlag` / `TestLoopbackAllowedWithFlag` — with `--allow-virtual-interface`, loopback accepted **including prefix-contained match** (127.0.0.2 inside 127.0.0.1/8) — this rule is what makes the sudo-free demo work (§5); warn-flag asserted.
- `TestVirtualPatternsRejected` — docker0/veth/br-/tun/wg fixtures rejected; allowed+warned with flag.
- `TestExplicitInterfaceOverride` — `--interface eth1` honored; error if it doesn't own the IP.
- `TestOperstateDown` — preflight-failing condition surfaced.
- `TestMalformedIPJSON` / empty array → typed error.

### internal/parser
- `TestEthtoolLink` — table over fixtures: intel-1g, realtek-2500m, usb-100m-half, no-link; asserts speed, duplex, autoneg, advertised/peer-advertised mode lists, port, `Speed: Unknown!` → unset-not-zero.
- `TestEthtoolStatsNormalization` — driver-specific names (`rx_crc_errors`, `rx_crc_errors_phy`, `CrcErrs`) map to `Standard` keys; unknown names preserved in `Raw`; **absent counter ⇒ absent, never 0** (load-bearing for delta semantics).
- `TestIPLinkStats` — `ip -j -s -s link show` fixture → CounterSnapshot.Standard.
- `TestPingPerPacket` — clean run; loss/dup/unreachable variants; `(DUP!)` lines; icmp_seq gaps → longest-response-gap; RTT list → min/avg/max/mdev + p50/p95/p99 computed from per-packet data (not summary); summary-line variants (`+1 duplicates`, no mdev at 100% loss).
- `TestPingIntervalRejected` — stderr fixture "minimal interval allowed" → typed `ErrIntervalRejected` (drives graceful fallback retry).
- `TestFullSizePingFragErrors` — "message too long"/"Frag needed" fixture → frag-error count, distinct from loss.
- `TestIperf3TCPVersions` — fixtures for 3.7 and 3.9+ layouts: sum_sent/sum_received, retransmits (sender only), per-interval series, min/max/avg interval bitrate, coefficient of variation, CPU util; collapse detection (interval < 10% of median flagged).
- `TestIperf3Bidir` — 3.14 bidir fixture: per-direction extraction.
- `TestIperf3UDP` — jitter_ms, lost/total, lost_percent, out_of_order.
- `TestIperf3ErrorObject` — `{"error":"unable to connect..."}` fixture → typed error, not parse failure.
- `TestIperf3Truncated` — malformed JSON → error carrying first 256 raw bytes for the report.
- `TestCableTestParse` — ok / open-pair-at-distance / "Operation not supported" → UNAVAILABLE (never FAILED).

### internal/testsuite (orchestration, all via FakeRunner + FakeClock)
- `TestLinkTestFlags` — no-link fixture → finding "no link"; half-duplex; 100M-vs-advertised-1G → conservative "possible causes" text present.
- `TestCounterSnapshotsBeforeAfter` — two `Times:1` scripts; delta computed; recorded calls prove `ethtool -S` and `ip -j -s -s` each called twice.
- `TestTCPTestServerLifecycle` — receiver-side `Start("iperf3", ["-s",...])` → FakeProcess; sender `Run` client; assert server `Terminated()` after test; assert recorded client args contain `--json`, `-t 30`, `-P 4`.
- `TestUDPDefaultRate` — link fixture 1G ⇒ recorded `-b 800M` (80% rule); `--udp-rate` override respected.
- `TestBidirFallback` — capabilities without bidir ⇒ two one-way runs on distinct ports; report notes "limitation, not cable failure".
- `TestCancellationMidTCP` — `Delay` script + `Started` sync; cancel ctx; assert `FakeProcess.Killed()` (server) and partial TCPResult marked incomplete.
- `TestStaleProcessPreflight` — pre-seeded pidfile in temp state dir + fake liveness ⇒ preflight failure with remediation text; dead-pid pidfile ⇒ cleaned, preflight passes.
- `TestCapabilityDetection` — scripted `iperf3 --version` / probe outputs ⇒ Capabilities{JSON, Reverse, Bidir, GetServerOutput, UDP} correct across old/new fixtures.
- `TestMonitorEvents` (P3) — FakeClock ticker; scripted sequence of `ip`/`ethtool` fixtures (link up → down → up at 100M) ⇒ MonitoringEvents: carrier-loss, renegotiation-with-speed-change, ordered, timestamped from FakeClock.

### internal/evaluate
- `TestCounterDelta` — table: normal increase; after<before (wrap/reset) ⇒ `(0, ok=false)` conservative; missing-in-after; missing-in-before; zero-delta.
- `TestRuleXxx` (one per rule) — CRC delta>0 under load ⇒ physical/POOR-tier finding; zero phys errors + low throughput + CPU≥90% ⇒ host-limited ⇒ INCONCLUSIVE override; UDP loss near saturation alone ⇒ note, not POOR (must correlate); half duplex ⇒ WARNING; cable-test open ⇒ FAILED-tier; UNAVAILABLE never downgrades.
- `TestClassifyGoldenScenarios` — 5 golden inputs mirroring the example reports (healthy-gigabit ⇒ EXCELLENT/GOOD, 100M-negotiation ⇒ WARNING, crc-errors ⇒ POOR, host-limited ⇒ INCONCLUSIVE, disconnected ⇒ FAILED); asserts classification, exit-code mapping (0/1/2/3/2), and that every classification carries ≥1 Finding with evidence strings.
- `TestScoreDeterministic` — same input twice ⇒ identical score + rule trace.

### internal/model
- `TestReportJSONRoundTrip` — marshal→unmarshal→reflect.DeepEqual; `schemaVersion` present; timestamps RFC3339.

### internal/reporting
- `TestMarkdownGolden` — golden files `testdata/golden/report-{healthy,reduced-speed,crc,host-limited,failed}.md`; deterministic input (FakeClock timestamps, fixed testId); `-update` flag (`var update = flag.Bool("update", false, ...)`); byte-exact compare; asserts all 23 mandated section headers present via a separate structural test so a golden regen can't silently drop sections.
- `TestSummaryTxtGolden` — same pattern.
- `TestRegenerateFromJSON` — write report.json → `reporting.Regenerate(jsonPath, outDir)` (the `cablecheck report` engine) ⇒ report.md + summary.txt byte-identical to direct generation. Pins the CLI subcommand's core.
- `TestReportDirNaming` — FakeClock ⇒ `cablecheck-report-2026-07-15_10-30-00`.
- `TestRawArtifactPaths` — raw/ tree written; every CommandResult teed.
- `TestTransferSHA256` — chunked transfer over `net.Pipe`: happy path hash match; **corrupted chunk** (test flips a byte via a wrapping `io.Writer`) ⇒ verification error, receiver keeps nothing partial; oversize file ⇒ rejected against negotiated cap before transfer.

### internal/cli
- `TestPromptCommands` — "start\n" ⇒ CmdStart; "status\nstart\n" ⇒ status callback then start; "quit" ⇒ CmdQuit; garbage ⇒ reprompt; EOF ⇒ CmdQuit; ctx cancel while blocked (io.Pipe) ⇒ ctx.Err, no goroutine leak.
- `TestProgressLines` — quiet mode emits exactly `[n/8] ...` lines; `--verbose` superset.
- `TestVersionOutput` — injected version string printed.

---

## 3. Integration harness

**Location/decision:** `internal/app/integration_test.go`, **`package app` (white-box)**. White-box is required so tests can set unexported fault-injection hooks and read the bound control address. Runs under plain `go test ./...` (no build tag); total suite budget < 10s, since all external durations are FakeRunner-instant.

App seam (pins the app package API):

```go
// internal/app
type Deps struct {
    Runner runner.CommandRunner
    Clock  clock.Clock
    Stdin  io.Reader
    Stdout, Stderr io.Writer
    StateDir string                 // pidfiles / stale detection
    hooks    testHooks              // unexported; zero in production
}
type testHooks struct {
    onState           func(peer.State)     // pushed to a channel by tests: THE sync primitive
    mangleReportChunk func([]byte) []byte  // corruption injection
}

type App struct{ /* ... */ }
func New(cfg config.Config, deps Deps) (*App, error)
func (a *App) Start(ctx context.Context) error        // binds listener (pc1) before returning
func (a *App) ControlAddr() net.Addr                  // real bound addr (port 0 support)
func (a *App) Wait() (ExitCode, error)
```

**Harness skeleton per scenario:** coordinator `New` with `--control-port 0` → `Start` → read `ControlAddr()` → worker `New` with that port → both `Wait` in goroutines → assert exit codes + report dirs in each side's `t.TempDir()`. Each side gets its **own FakeRunner** scripted from a named fixture set (`fixtures.Healthy(t, fr)` helper composing Scripts). Integration uses a **real clock** (decided): golden/timestamp determinism is a unit-level concern, and sharing one FakeClock across two concurrently-running apps deadlocks in practice. Heartbeat/monitor intervals are configured short (100ms) via config for integration only. Non-interactive mode everywhere except one scripted-stdin scenario. `testutil.LeakCheck(t)` in every subtest.

Scenarios (each a subtest of `TestIntegration`):

1. **HappyPathQuick** — full quick run. Asserts: both exit 0; PC1 dir contains report.json (unmarshals into model.Report, classification ∈ {GOOD, EXCELLENT}), report.md with all 23 headers, summary.txt, raw/ populated; PC2 dir contains transferred report.json/report.md/summary.txt whose SHA-256 equal PC1's files. Interactive variant: stdin `io.Pipe`, test writes "start\n" to both after observing state `waiting_for_local_start` via `onState` channel. This proves start-sync ordering without sleeps.
2. **PeerDisconnectMidTest** — worker's iperf3 client Script has `Delay`+`Started`; on `Started`, test hard-cancels the worker's ctx (simulates death). Coordinator: partial report written (report.json has `"partial": true` / incomplete test entries), exit **5**.
3. **SIGINTSimulation** — coordinator ctx cancelled mid-TCP-test (gated on `Started`). Asserts: abort message received by worker (worker `onState` reaches aborted; worker exits 5), coordinator's receiving-side FakeProcess `Killed()` true, partial report present, coordinator exit **6**. Tests don't send real signals; `signal.NotifyContext` lives only in `main.go` (thin, review-gated).
4. **MalformedFrameInjection** — raw `net.Dial` to control port sends header `0xFFFFFFFF` + garbage; coordinator closes that conn without panic and, still pre-handshake, keeps listening; then a legit worker completes normally. Mid-session malformed frames are covered at protocol unit level.
5. **TokenMismatch** — worker wrong token ⇒ handshake rejected; worker exits 5 with "token" in stderr; coordinator behavior per peer design asserted (rejects and continues listening).
6. **NoBidirFallback** — worker capability fixture lacks bidir ⇒ two one-way stress runs; asserted via both sides' recorded iperf3 args (no `--bidir`, two port-distinct sessions) and report limitation note.
7. **ReportTransferCorruption** — coordinator's `mangleReportChunk` flips one byte in chunk 2 ⇒ PC2 detects SHA mismatch, logs warning, run still *completes* on PC1 side with exit driven by health. Transfer failure is a warning, not an orchestration failure (decided). PC2 has no partial corrupt files.
8. **StaleIperf3Detection** — pre-write live-looking pidfile into worker `StateDir`; preflight fails, exit **4**, remediation hint in output; coordinator times out its wait gracefully (bounded by short heartbeat config).

---

## 4. Determinism rules (enforced conventions)

1. **No `time.Sleep` for synchronization** — sync via `Script.Started`/`Delay` channels, `onState` channel, `FakeClock.BlockUntilWaiters`+`Advance`, and `testutil.WaitFor`. The only loops with sleeps are bounded *convergence polls* inside `LeakCheck` and `TestProcessGroupKill` (10ms polls capped by `TestTimeout`). Those wait for shutdown, not for scheduling luck.
2. **Base context** = `t.Context()` (Go 1.24) everywhere; long operations bounded by `testutil.TestTimeout(t)`, derived from `t.Deadline()` minus 2s grace. Tests degrade gracefully under `-timeout` overrides and slow `-race` runs.
3. **Goroutine leak check — hand-rolled (goleak rejected: stdlib-only mandate).** `testutil.LeakCheck(t)`: capture `runtime.Stack(buf, true)` at defer time, count goroutines whose stack contains the module path (`cablecheck/internal`), retry-poll up to 2s for the count to reach the entry snapshot, else `t.Errorf` with the offending stacks. Filtering by module path sidesteps testing-framework and `os/signal` background goroutines. Applied in every integration subtest and every peer/protocol/testsuite test that starts goroutines.
4. `go test -shuffle=on` in the gate (catches order coupling); `t.Parallel()` on independent unit tests; zero mutable package-level state in production packages (report-gen map iteration sorted, enforced by byte-exact goldens).
5. Randomness injected: token via config in tests (crypto/rand only in prod path), testId injectable through `Deps`/config.

---

## 5. Local e2e demo (manual verification, this machine)

**Decision: stub-tools on PATH. `demo` build tag REJECTED.** A build tag forks product behavior, so the demo would no longer exercise the real `LookPath → exec → parse` pipeline. PATH stubs run the *production binary byte-for-byte* through its real command plumbing. Stubs are sh scripts: fixtures, not product code (the product never uses a shell).

`testdata/stubtools/` (executable, committed):

```
testdata/stubtools/iperf3     # sh: "-s" in args → trap TERM/INT, loop sleep (killable server)
                              # client: pick fixture by args (-u? --reverse? --bidir?), sleep 0.3, cat it
testdata/stubtools/ethtool    # "-S" → stats fixture; "--cable-test" → ok fixture; else link fixture (1Gb/s full)
testdata/stubtools/fixtures/  # copies of the same testdata/iperf + testdata/ethtool files
```

Stubs resolve fixtures relative to `$0` (`dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)`), so they work from any cwd. `ping` and `ip` are the real binaries (loopback ping to 127.0.0.2 works on Linux; `ip -j addr` is real). Unit and integration tests never touch stubtools.

Demo (no sudo needed — relies on the §2 network rule: loopback prefix-match under `--allow-virtual-interface`; if the network design ends up requiring exact match, fallback documented: `sudo ip addr add 127.0.0.2/8 dev lo` + `ip addr del` cleanup):

```sh
make build
export PATH="$PWD/testdata/stubtools:$PATH"

# terminal 1 (PC1/coordinator):
./cablecheck run --role pc1 --local-ip 127.0.0.1 --peer-ip 127.0.0.2 \
  --allow-virtual-interface --mode quick --token demo-token
# terminal 2 (PC2/worker):
./cablecheck run --role pc2 --local-ip 127.0.0.2 --peer-ip 127.0.0.1 \
  --allow-virtual-interface --mode quick --token demo-token
# type "start" in both when prompted
```

**Success proof:** virtual-interface warning printed; 3-2-1 countdown on both; `[n/8]` progress lines; both exit 0 (`echo $?`); `cablecheck-report-*/` on both sides with summary.txt/report.md/report.json/raw/; `sha256sum` of PC1 vs PC2 report.json identical; `./cablecheck report <pc1-dir>/report.json` regenerates md+summary (proves `report` subcommand); `./cablecheck doctor --local-ip 127.0.0.1 --allow-virtual-interface` all-green with stub PATH, and *without* stub PATH shows iperf3/ethtool missing with `pacman -S iperf3 ethtool` hint (negative demo). Interrupt demo: Ctrl-C in terminal 1 mid-test ⇒ exit 6, partial report, terminal 2 reports peer abort.

Because `--non-interactive` exists, this is also scriptable. `scripts/demo-e2e.sh` runs PC1 in background with port 0→fixed demo port, PC2 foreground, and asserts exit codes + report artifacts + hash equality. It's wired as `make demo-e2e` so the "demo" gate is push-button.

---

## 6. Makefile + verification gates

```make
BIN      := cablecheck
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"
export CGO_ENABLED=0

build:      ; go build $(LDFLAGS) -o $(BIN) ./cmd/cablecheck
test:       ; go test ./...
test-race:  ; go test -race -shuffle=on ./...
vet:        ; go vet ./...
fmt-check:  ; @out=$$(gofmt -l .); [ -z "$$out" ] || { echo "$$out"; exit 1; }
fmt:        ; gofmt -w .
lint: vet fmt-check   ## staticcheck absent on dev/CI: run if present, never required
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed (skipped)"
tidy-check: ; go mod tidy -diff
dist:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BIN)-linux-amd64 ./cmd/cablecheck
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BIN)-linux-arm64 ./cmd/cablecheck
examples:   ; go run ./tools/genexamples -out examples/   # hermetic: FakeRunner scenarios → real reporting code
demo-e2e: build ; ./scripts/demo-e2e.sh                    # stubtools PATH, loopback, --non-interactive
clean:      ; rm -rf $(BIN) dist/ cablecheck-report-*
check: fmt-check vet tidy-check test test-race build       # fast inner loop / CI PR gate
verify: check dist examples demo-e2e                       # release gate
```

**Exact final gate sequence before declaring done (in order, all must pass):**

```
gofmt -l .                                   # empty output
go vet ./...
go mod tidy -diff
go test ./...
go test -race -shuffle=on ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./...
make examples && for d in examples/*/; do ./cablecheck report "$d/report.json" >/dev/null; done
make demo-e2e                                # scripted loopback run, exit 0 both sides
# manual once per release: two-terminal interactive demo (§5) incl. Ctrl-C partial-report check
```

Examples are regenerated rather than hand-written, so they can never drift from reporting code. The `cablecheck report` loop proves forward-consumability of every shipped report.json.

---

## 7. TDD anchors per phase (write these RED first)

**P1 (vertical slice)** — order matters; each item unblocks the next:
1. `config.TestParseBitrate` + `TestValidateIPs` (pure, zero deps — proves module builds).
2. `protocol.TestFramePartialReads` + `TestFrameMaxSize` (net.Pipe + Dribbler) — framing before anything networked.
3. `peer.TestTransitionTable` (pure FSM), then `TestHandshakeHappyPath`/`TestHandshakeTokenMismatch` over net.Pipe.
4. `runnertest` fakes + `runner.TestRunNoShellInterpretation`/`TestContextCancelKillsChild` (helper-process pattern) — the exec substrate.
5. `parser.TestPingPerPacket`, `TestIperf3TCPVersions`, `TestIPLinkStats` — fixtures first, parsers second.
6. `evaluate.TestCounterDelta`.
7. `testsuite.TestTCPTestServerLifecycle` + `TestCancellationMidTCP`.
8. `model.TestReportJSONRoundTrip` + minimal `reporting.TestMarkdownGolden` (healthy only).
9. **Phase exit anchor:** `integration/HappyPathQuick` (TCP-only variant) + `SIGINTSimulation` — written failing at phase start, green = P1 done.

**P2:** anchors first: `integration/NoBidirFallback`, `integration/ReportTransferCorruption`. Then `parser.TestFullSizePingFragErrors`, `TestIperf3UDP`, `testsuite.TestCapabilityDetection`, `TestUDPDefaultRate`, `reporting.TestTransferSHA256`, `evaluate.TestClassifyGoldenScenarios` (all 5), full 23-section goldens, `tools/genexamples` + examples gate.

**P3:** anchor: `integration/PeerDisconnectMidTest` (partial report + exit 5). Then `clocktest` ticker semantics, `testsuite.TestMonitorEvents`, soak partial-result retention tests, renegotiation fixture parsing.

**P4:** anchor: `parser.TestCableTestParse` (ok/open/unsupported→UNAVAILABLE), then testsuite coordination test with scripted link-loss→recovery fixture sequence (FakeClock-driven poll observing down→up fixtures).

Rule for the executing engineer: never write a fixture-consuming parser before committing its fixture, and never write orchestration before its FakeRunner script compiles. The phase's integration anchor stays red until the phase's last unit lands.

---

## Pitfalls (things that break in practice)

1. **`t.Fatal` off the test goroutine** — FakeRunner/FakeProcess/leakcheck are called from app goroutines, so they must use `t.Errorf` + returned errors. `Fatalf` there silently corrupts the test run.
2. **FakeClock lost wakeup** — `Advance` before the code-under-test calls `After` hangs forever. `BlockUntilWaiters(n)` before every Advance is non-negotiable; bake it into test helpers.
3. **`net.Pipe` is synchronous** — a frame write blocks until the peer reads. Any test writing then reading on one goroutine deadlocks, so always run the counterpart in a goroutine (helper: `testutil.PipePeer`). The same property is *useful* for backpressure tests.
4. **4-byte length header DoS in tests and prod** — decode must validate length against max *before* `make([]byte, n)`. The `0xFFFFFFFF` test exists to catch an OOM-crash, so don't drop it.
5. **Stdin pump goroutine leaks** — `io.Pipe`-backed prompts leak the pump unless the test closes the writer, so `testutil.ScriptStdin` registers `t.Cleanup(w.Close)`. The real `os.Stdin` pump can't be unblocked; that's acceptable only in `main.go`, which the leakchecker never sees.
6. **Ephemeral-port race** — never "find free port then listen later". Coordinator binds `:0`, tests read `ControlAddr()`. Same rule inside the product for the iperf-port-free preflight: check by binding, then release immediately before handing to iperf3 (window documented).
7. **Shared FakeClock across two in-process apps deadlocks** — one app waits for an advance the other's assertion path never triggers. So integration uses real time with short configured intervals, and FakeClock is unit-scope only.
8. **`flag.CommandLine` reuse panics** on second parse in one test binary. Config must use `flag.NewFlagSet` per invocation (test `TestFlagSetIsolated` pins it).
9. **Golden-file mangling** — editors strip trailing whitespace and convert line endings in `testdata/golden/*.md`. Add `.gitattributes` (`testdata/** -text`) and `.editorconfig` exclusion, compare exact bytes, regenerate only via `-update`.
10. **Map iteration + `time.Now()` in report generation** — non-deterministic goldens. Sort every ranged map; route all timestamps through the injected `Clock`. Guard: goldens fail loudly, plus a grep in review for `time.Now()` outside `internal/clock` and `main.go`.
11. **Sh stubs spawn `sleep` children** — `Terminate` on the stub kills `sh`, orphaning a `sleep`. Runner's process-group kill (`Setpgid` + `kill(-pgid)`) covers it, and stubs also `trap ... TERM INT`. Without pgid-kill the demo leaves zombies, which is why `TestProcessGroupKill` exists.
12. **Absent counters ≠ zero counters** — treating a missing `rx_crc_errors` as 0 in the *before* snapshot fabricates a huge delta when the driver reports it *after* (or vice versa). Delta requires presence in both; otherwise `(0, ok=false)`. The parser tests pin absence semantics.
13. **`-race` slows everything ~5–20×** — any hardcoded 1–2s test timeout that passes plain will flake under race in CI. All waits go through `testutil.TestTimeout(t)`.
14. **Real signals in tests** — sending SIGINT to the test process interferes with `go test` itself. Signal handling stays in `main.go` (`signal.NotifyContext`) and tests cancel contexts; don't "improve" this with an os.Process.Signal self-test.
15. **iperf3 server readiness by stdout-scrape or port-dial** breaks hermetic tests (FakeProcess has no live socket). Readiness must be Process-handle + Clock-grace + protocol-level ready ack (§1.1 requirement to the runner/testsuite designers).
