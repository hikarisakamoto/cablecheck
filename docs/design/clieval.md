# CableCheck — Design: CLI, Config, Evaluation, Reporting, App, Logging

Scope: `internal/cli`, `internal/config`, `internal/model`, `internal/evaluate`, `internal/reporting`, `internal/app`, `internal/logging`, `cmd/cablecheck`. Stdlib only. The decisions below are final for this scope.

## 0. Package dependency graph (leaf → root)

```
internal/model      (pure data: Report, Bitrate, Duration, HealthClass, result structs — imports stdlib only)
internal/logging    (slog construction + redaction; imports stdlib only)
internal/config     (RunConfig, validation, presets; imports model)
internal/evaluate   (Facts, rules, classification; imports model)
internal/reporting  (dir layout, JSON/MD/summary rendering; imports model ONLY — enforced pure)
internal/app        (orchestration, ExitError, doctor, regenerate; imports config, model, evaluate, reporting, logging, runner, peer, ...)
internal/cli        (flag parsing, dispatch, exit mapping; imports app, config, model)
cmd/cablecheck      (main; ldflags vars; imports cli)
```

`internal/reporting` importing only `model` + stdlib is what makes `cablecheck report <json>` regeneration trivially correct. Add a test that fails if `go list -deps ./internal/reporting` contains any other internal package.

---

## 1. CLI (`internal/cli`)

### 1.1 Entry and dispatch

```go
// cmd/cablecheck/main.go
var (
    version = "dev"     // -ldflags "-X main.version=..."
    commit  = "none"    // -X main.commit=...
    date    = "unknown" // -X main.date=...
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
        app.BuildInfo{Version: version, Commit: commit, Date: date}))
}
```

```go
// internal/cli
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, build app.BuildInfo) int
```

`Run` dispatches on `args[0]` ∈ {`run`, `doctor`, `report`, `version`}. No args, or `help`/`-h`/`--help`, prints top-level usage (subcommand list + one-line descriptions) and returns 0 for explicit help, 4 for a missing or unknown subcommand. Each subcommand builds its own `flag.NewFlagSet(name, flag.ContinueOnError)` with `fs.SetOutput(stderr)` and a custom `fs.Usage`. The usage is grouped and hand-ordered because stdlib `PrintDefaults` alphabetizing is unacceptable for 23 flags. **Never** use `flag.ExitOnError`: it calls `os.Exit(2)`, which collides with our exit-code contract. Parse errors must become exit 4.

The subcommand must be `args[0]`, with flags after it. `cablecheck --verbose run` is rejected with a hint ("flags go after the subcommand"), since stdlib flag stops at the first non-flag arg and fighting that isn't worth it.

### 1.2 Exit codes and typed error hierarchy

Lives in `internal/app` (so app/peer code can return it without importing cli):

```go
// internal/app/exit.go
type ExitCode int

const (
    ExitOK           ExitCode = 0 // GOOD / EXCELLENT
    ExitWarning      ExitCode = 1
    ExitPoorFailed   ExitCode = 2
    ExitInconclusive ExitCode = 3
    ExitConfig       ExitCode = 4 // config/dependency error (also doctor FAIL)
    ExitPeer         ExitCode = 5 // peer/orchestration failure
    ExitInterrupt    ExitCode = 6
    ExitInternal     ExitCode = 7
)

type ExitError struct {
    Code ExitCode
    Err  error // may be nil (pure code carrier, e.g. classification results)
}
func (e *ExitError) Error() string { if e.Err != nil { return e.Err.Error() }; return fmt.Sprintf("exit %d", e.Code) }
func (e *ExitError) Unwrap() error { return e.Err }

func ExitCodeFor(c model.HealthClass) ExitCode {
    switch c {
    case model.ClassExcellent, model.ClassGood: return ExitOK
    case model.ClassWarning:                    return ExitWarning
    case model.ClassPoor, model.ClassFailed:    return ExitPoorFailed
    case model.ClassInconclusive:               return ExitInconclusive
    default:                                    return ExitInternal
    }
}
```

`cli.Run` unwrap policy, in order:

```go
err := dispatch(ctx, ...)
if err == nil { return 0 }
if errors.Is(err, flag.ErrHelp) { return 0 }
var xe *app.ExitError
if errors.As(err, &xe) { printIfNonNil(stderr, xe.Err); return int(xe.Code) }
var ve *config.ValidationError
if errors.As(err, &ve) { fmt.Fprintf(stderr, "cablecheck: %v\n", ve); return int(app.ExitConfig) }
if errors.Is(err, context.Canceled) && ctx.Err() != nil { return int(app.ExitInterrupt) } // signal fired
fmt.Fprintf(stderr, "cablecheck: internal error: %v\n", err)
return int(app.ExitInternal)
```

Producers: `config` returns `*ValidationError` (→4). `app` wraps peer/handshake/protocol failures in `&ExitError{Code: ExitPeer}` and interrupt-detected paths in `ExitInterrupt`. Successful runs return `&ExitError{Code: ExitCodeFor(report.Evaluation.Class)}` with `Err: nil`, so classification maps deterministically. Anything unrecognized is 7.

### 1.3 The 23 `run` flags

All registered on the `run` FlagSet. Sentinel defaults (0 / "") mean "mode preset decides". Help text states the per-mode defaults explicitly.

| Flag | Type | Registered default | Notes |
|---|---|---|---|
| `--role` | string | `""` | required; `pc1`\|`pc2` |
| `--local-ip` | string | `""` | required unless `--interface` is given (then inferred at preflight); IPv4 |
| `--peer-ip` | string | `""` | required; IPv4 |
| `--interface` | string | `""` | override auto-discovery |
| `--mode` | string | `"quick"` | `quick`\|`standard`\|`soak` |
| `--control-port` | int | `44300` | 1024–65535 |
| `--iperf-port` | int | `44301` | 1024–65534 (port+1 used by bidir fallback) |
| `--token` | string | `""` | PC1: auto-generate if empty; PC2: required |
| `--tcp-duration` | duration | `0` | preset: 30s/60s/60s |
| `--udp-duration` | duration | `0` | preset: 20s/30s/20s |
| `--udp-rate` | string | `""` | bitrate; empty = auto 80% of negotiated |
| `--parallel-streams` | int | `0` | preset: 4; range 1–16 |
| `--soak-duration` | duration | `0` | soak only; default 1h |
| `--soak-load` | string | `""` | soak only; `periodic`(default)\|`continuous` |
| `--monitor-interval` | duration | `0` | preset: 1s (standard/soak; quick: link-watch only) |
| `--cable-test` | bool | `false` | opt-in |
| `--cable-test-tdr` | bool | `false` | implies `--cable-test` |
| `--output` | string | `"."` | parent dir for report dir |
| `--verbose` | bool | `false` | |
| `--non-interactive` | bool | `false` | |
| `--no-sudo` | bool | `false` | |
| `--no-report-transfer` | bool | `false` | |
| `--allow-virtual-interface` | bool | `false` | |

### 1.4 Mode presets vs explicit overrides

After `fs.Parse`, collect explicitly-set flags:

```go
set := map[string]bool{}
fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
cfg, err := config.Resolve(raw, set) // raw = struct of parsed flag values
```

`config.Resolve` validates `--mode` first, then fills every field where `!set[name]` from the preset table:

| Setting | quick | standard | soak |
|---|---|---|---|
| TCPDuration | 30s | 60s | 60s (per cycle) |
| UDPDuration | 20s | 30s | 20s (per cycle) |
| ParallelStreams | 4 | 4 | 4 |
| PingCount / PingInterval (derived, not flags) | 500 @20ms | 1500 @20ms | 500 @20ms per cycle |
| MonitorInterval | 1s (link-state watch only) | 1s (full monitoring) | 1s |
| TCP repeats | 1 | 2 | per cycle |
| Extra UDP rates | – | +1 run at 50% rate | – |
| SoakDuration | invalid | invalid | 1h |
| SoakLoad | invalid | invalid | periodic |

Rule: an explicit user value always wins and is then bounds-checked. An explicit value equal to the sentinel (`--tcp-duration 0`) is caught by bounds validation with a clear message. `--soak-duration`/`--soak-load` set with `mode != soak` → `ValidationError{"--soak-duration", "only valid with --mode soak"}`. Fail fast; don't silently switch modes.

### 1.5 Usage text structure

Custom `Usage` per subcommand: a synopsis line, then groups in fixed order. **Connection** (role/local-ip/peer-ip/interface/control-port/iperf-port/token), **Test parameters** (mode/tcp-duration/udp-duration/udp-rate/parallel-streams/soak-*/monitor-interval), **Diagnostics** (cable-test/cable-test-tdr), **Behavior** (non-interactive/no-sudo/no-report-transfer/allow-virtual-interface/verbose), **Output** (output). The footer carries the exit-code table and the boolean-flag caveat (`--cable-test=false`, not `--cable-test false`).

---

## 2. Config (`internal/config`)

```go
type Role string   // "pc1" | "pc2"
type Mode string   // "quick" | "standard" | "soak"
type SoakLoad string // "periodic" | "continuous"

type RunConfig struct {
    Role            Role
    LocalIP         netip.Addr
    PeerIP          netip.Addr
    Interface       string // "" = auto-discover
    Mode            Mode
    ControlPort     uint16
    IperfPort       uint16
    Token           string
    TokenGenerated  bool
    TCPDuration     time.Duration
    UDPDuration     time.Duration
    UDPRate         model.Bitrate // 0 = auto (80% of negotiated)
    ParallelStreams int
    PingCount       int           // derived from mode
    PingInterval    time.Duration // derived from mode
    TCPRepeats      int           // derived from mode
    SoakDuration    time.Duration
    SoakLoad        SoakLoad
    MonitorInterval time.Duration
    CableTest       bool
    CableTestTDR    bool
    OutputDir       string // absolute, cleaned
    Verbose         bool
    NonInteractive  bool
    NoSudo          bool
    NoReportTransfer bool
    AllowVirtualInterface bool
}

// LogValue redacts Token — RunConfig can never leak the token through slog.
func (c RunConfig) LogValue() slog.Value

type ValidationError struct{ Flag, Msg string }
func (e *ValidationError) Error() string // `--local-ip: not a valid IPv4 address ("fe80::1"): IPv6 is not supported yet`

func Resolve(raw RawRunFlags, explicitlySet map[string]bool) (*RunConfig, error)
```

### 2.1 Validation order (strict, fail-fast, zero network I/O)

1. `--mode` valid (needed before presets).
2. `--role` present and valid.
3. `--local-ip`: may be empty when `--interface` is given. In that case preflight infers it from that interface's sole IPv4 address (zero or several IPv4 addresses is an error asking for an explicit `--local-ip`), and on PC1 this inference runs before the listener bind. When present: `netip.ParseAddr`, then `addr = addr.Unmap()`, then require `addr.Is4()`. Reject IPv6 with `"IPv6 is not supported yet; use the interface's IPv4 address"`. Reject `IsUnspecified()` (0.0.0.0) and `IsMulticast()`. Reject `IsLoopback()` **unless** `--allow-virtual-interface` (which enables the 127.0.0.1/127.0.0.2 single-machine demo). Reject addrs with zones.
4. `--peer-ip`: same rules.
5. `local != peer`.
6. Ports: each in 1024–65535 (below 1024 rejected: "privileged ports are not supported"), `iperf-port <= 65534` (bidir fallback uses port+1), `control-port ∉ {iperf-port, iperf-port+1}`.
7. Token: if provided, length 8–128, printable ASCII, no whitespace. If empty and role=pc1, generate (§2.2). If empty and role=pc2, `ValidationError{"--token", "required on pc2; copy the token displayed by pc1"}`.
8. Apply mode presets (per §1.4), then bounds: TCPDuration 5s–10m; UDPDuration 5s–10m; MonitorInterval 200ms–30s; SoakDuration 60s–24h; ParallelStreams 1–16.
9. `--udp-rate` if set: `model.ParseBitrate`, then bounds 1M–40G.
10. `--cable-test-tdr` sets `CableTest = true` (implied, no error).
11. `--output`: reject if any raw path element is `..` (traversal); `filepath.Abs` + `filepath.Clean`; `os.Stat` must exist and be a directory. (The writability probe, a temp-file create/delete, belongs to preflight, not config. Stat is local FS, so it's acceptable pre-network.)

### 2.2 Token generation and distribution

PC1 with empty `--token`: 16 bytes from `crypto/rand` → `hex.EncodeToString` → 32 hex chars, and `TokenGenerated = true`. **The display path is stdout via `fmt.Fprintf`, never slog**, since the log file must not contain it:

```
Session token: 3f9c62d1a8e04b77c5d2910f4a6b8e33  (auto-generated)
On PC2 run:
  cablecheck run --role pc2 --local-ip <PC2-IP> --peer-ip 192.168.50.10 --token 3f9c62d1a8e04b77c5d2910f4a6b8e33
```

PC2 receives it only via `--token`. Constant-time comparison (`crypto/subtle.ConstantTimeCompare`) happens in the protocol layer, out of scope here; config just carries the string.

---

## 3. Bitrate (`internal/model`)

```go
type Bitrate uint64 // bits per second

func ParseBitrate(s string) (Bitrate, error)
func (b Bitrate) String() string          // "1G", "2.5G", "800M", "10M", "1500"
func (b Bitrate) MarshalJSON() ([]byte, error)   // {"bps":1000000000,"text":"1G"}
func (b *Bitrate) UnmarshalJSON([]byte) error    // accepts object, number, or "1G" string
```

**Parsing**: grammar `INT[.FRAC][K|M|G]`, case-insensitive suffix, decimal multipliers 1e3/1e6/1e9. Implemented with integer math, **not** `strconv.ParseFloat`, to avoid 0.1-style float artifacts. Parse the integer part and fraction digits separately, and require `len(frac) <= 9` and that `10^len(frac)` divides the multiplier (so `2.5G` = 2×1e9 + 5×1e8; `1.5K` ok; `1.2345K` → error "fraction finer than unit"). Errors: empty string; zero; bare fractional without suffix (`"2.5"`); unknown suffix, with a special-case hint for `KB/MB/GB/Kb/...`: "bytes suffixes not accepted; use decimal bits: K, M, G"; negative sign; overflow past `math.MaxUint64` (checked before multiply). A bare integer is bits/sec (allowed, used in tests).

**String()**: largest suffix that renders with ≤1 decimal: `b%1e9==0 → "%dG"`, `b≥1e9 && b%1e8==0 → "%d.%dG"`, same pattern for M and K, else bare digits. This round-trips for every value CableCheck itself produces.

**UDP default-rate derivation** (in `config`, since it uses run context and is called after link negotiation is known):

```go
// negotiated == 0 means unknown.
func DefaultUDPRate(negotiated model.Bitrate) (rate model.Bitrate, warnings []string)
```

- Known speed: `rate = negotiated * 80 / 100`, then `rate = max(rate, 10M)`, then `rate = min(rate, negotiated*95/100)`. The cap wins over the floor: on a 10M link, 80%→8M, floor→10M, cap→9.5M.
- Unknown speed (ethtool unavailable or virtual interface): `rate = 100M` + warning `"link speed unknown; defaulting UDP rate to 100M — UDP loss results may be unreliable"`. This warning also lands in `Facts.UDPRateAssumed` so the evaluator soft-pedals UDP findings.
- Explicit `--udp-rate` above 95% of a known negotiated speed: honored, but emits a warning and sets `Facts.UDPNearSaturation`. Loss at self-inflicted saturation must not fail the cable.

---

## 4. Evaluation engine (`internal/evaluate`)

### 4.1 Facts — flat evidence model

Assembled from the pre-evaluation Report. Every field is plain data, so rules stay pure and table-testable:

```go
type SideFacts struct {
    CRCClassErrors   uint64 // Δ(rx_crc + frame + alignment + symbol), wrap-safe; only counted when DeltaOK
    CarrierEvents    uint64 // Δcarrier_changes / link resets during session
    JabberSizeErrors uint64 // Δ(jabber + oversize + undersize + length)
    FifoOverrun      uint64
    DeltaOK          bool   // false on counter reset/wrap or capture failure
    CountersAvailable bool
}

type DirFacts struct { // one per traffic direction (pc1→pc2, pc2→pc1)
    // TCP
    TCPAvailable   bool
    TCPBitrate     model.Bitrate
    TCPRetransRate float64 // retransmits / est. segments (bytes/MSS, MSS fallback 1448)
    TCPCoV         float64 // stdev/mean of per-interval bitrates, first interval excluded
    TCPCollapses   int     // intervals < 50% of median interval bitrate
    // UDP
    UDPAvailable   bool
    UDPLossPct     float64
    UDPJitterMs    float64
    UDPOutOfOrderPct float64
    UDPTargetReached bool // actual send rate >= 90% of target
    // Ping
    PingLossPct    float64
    PingDuplicates int
    PingSpikes     int           // RTT > 10× median
    PingMaxGap     time.Duration // longest inter-response gap
    FullSizeLossPct float64
    FullSizeAvailable bool
    FragErrors     int // "-M do" frag-needed failures
}

type Facts struct {
    PC1, PC2         SideFacts
    Dir              [2]DirFacts // [0]=pc1→pc2, [1]=pc2→pc1
    NegotiatedSpeed  model.Bitrate // 0 = unknown
    ExpectedSpeed    model.Bitrate // min(local supported max, peer advertised max); 0 = unknown
    HalfDuplex       bool
    LinkUpAtEnd      bool
    Renegotiations   int  // mid-test speed/duplex changes (monitoring)
    CableTestRan     bool
    CableTestPairs   []model.CablePairResult // status: ok|open|short|impedance|unspecified (+ distance m)
    MaxCPUPct        float64 // max of iperf3 host/remote cpu_utilization_percent across tests
    USBAdapter       bool
    VirtualInterface bool
    Partial          bool
    UDPRateAssumed   bool
    UDPNearSaturation bool
    Unavailable      []string // test names that could not run
}

func FactsFromReport(r *model.Report) *Facts
```

### 4.2 Rule and Finding types

```go
type Category string // "physical" | "transport" | "performance" | "host" | "limitation"
type Severity int    // SevInfo < SevWarning < SevPoor < SevFailed; SevMarker for host/limitation markers

type Finding struct {
    RuleID   string   `json:"ruleId"`
    Category Category `json:"category"`
    Severity Severity `json:"severity"` // marshals as string
    Text     string   `json:"text"`
    Evidence []string `json:"evidence"`      // e.g. "pc2 enp3s0: rx_crc_errors +42 during test"
    HostSensitive bool `json:"hostSensitive"` // eligible for host-limited override
}

type Rule struct {
    ID       string
    Category Category
    Evaluate func(f *Facts) *Finding // nil = passed / not applicable
}

func Rules() []Rule // fixed, deterministic order — the order below
type Result struct {
    Class           model.HealthClass
    Score           *int // nil for INCONCLUSIVE
    Findings        []Finding
    Recommendations []string
    RulesVersion    string // "1.0.0"
}
func Evaluate(f *Facts) Result
```

### 4.3 Concrete rule list (deterministic order)

Physical (dominant):

| ID | Condition | Severity |
|---|---|---|
| PHY-01 link-lost | `!LinkUpAtEnd` | FAILED |
| PHY-02 crc-class | total CRC-class Δ (both sides): 1–10 → WARNING; 11–1000 → POOR; >1000, or >10 **and** any ping loss >1% | WARNING/POOR/FAILED |
| PHY-03 carrier | CarrierEvents: 1–2 → POOR; ≥3 → FAILED | POOR/FAILED |
| PHY-04 renegotiation | Renegotiations ≥ 1 mid-test | POOR |
| PHY-05 half-duplex | HalfDuplex | POOR |
| PHY-06 reduced-speed | NegotiatedSpeed < ExpectedSpeed (both known), e.g. 100M on 1G-capable pair — conservative text listing cable/pairs/NIC/config causes | WARNING |
| PHY-07 reduced-speed+errors | PHY-06 condition **and** CRC-class Δ > 0 | POOR |
| PHY-08 cable-test | any pair open/short (→FAILED, with fault distance); impedance mismatch (→POOR); unspecified fault (→WARNING) | per pair |
| PHY-09 frame-size-errors | JabberSize Δ: 1–10 → WARNING; >10 → POOR | WARNING/POOR |
| PHY-10 loss+errors correlation | any direction UDP loss > 2% (target reached) **and** CRC-class Δ > 0 | FAILED |

Transport:

| ID | Condition | Severity |
|---|---|---|
| TR-01 ping-loss | per direction: 0 < loss ≤ 0.1% → WARNING; 0.1–1% → POOR; >1% → POOR (FAILED is reserved for physical corroboration via PHY-10/PHY-02) | WARNING/POOR |
| TR-02 fullsize-loss | full-size loss > 0 while standard ping loss == 0 (frame-size-dependent corruption) | POOR |
| TR-03 frag-errors | FragErrors > 0 (MTU mismatch, config not cable) | WARNING |
| TR-04 duplicates | PingDuplicates > 0 | WARNING |
| TR-05 rtt-instability | PingSpikes > 5 → WARNING; PingMaxGap > 1s → POOR | WARNING/POOR |
| TR-06 tcp-retrans | per direction: 0.1–1% → WARNING; >1% → POOR. `< 0.1%` passes. | WARNING/POOR |
| TR-07 udp-loss | only if `UDPTargetReached && MaxCPUPct ≤ 90 && !UDPNearSaturation`: 0.5–2% → WARNING; >2% → POOR. `< 0.5%` passes. If preconditions unmet → no finding (host/limitation rules speak instead). | WARNING/POOR |
| TR-08 udp-jitter | jitter > 5ms on a direct link | WARNING |
| TR-09 udp-reorder | out-of-order > 0.1% (should be zero on a direct cable) | WARNING |

Performance (all `HostSensitive: true`):

| ID | Condition | Severity |
|---|---|---|
| PERF-01 throughput | ratio = TCPBitrate/NegotiatedSpeed: 0.4–0.7 → WARNING; <0.4 → POOR. ≥0.9 passes silently; 0.7–0.9 → Info note. Skipped when speed unknown. | WARNING/POOR |
| PERF-02 cov | TCPCoV 15–30% → WARNING; >30% → POOR | WARNING/POOR |
| PERF-03 collapses | 1–2 → WARNING; ≥3 → POOR | WARNING/POOR |
| PERF-04 asymmetry | \|dir0−dir1\|/max > 30% | WARNING |

Host (markers, SevMarker/SevInfo — never directly degrade class):

| ID | Condition | Effect |
|---|---|---|
| HOST-01 cpu | MaxCPUPct > 90 during any throughput test | sets hostLimited |
| HOST-02 virtual | VirtualInterface | forces final class INCONCLUSIVE |
| HOST-03 usb | USBAdapter **and** PERF-01 fired | sets hostLimited (weaker corroboration) |

Limitation (markers):

| ID | Condition | Effect |
|---|---|---|
| LIM-01 critical-unavailable | no TCP result at all, or counters unavailable on both sides | tentative EXCELLENT/GOOD → INCONCLUSIVE |
| LIM-02 noncritical-unavailable | UDP, bidir, full-size ping, or requested cable-test unavailable | tentative EXCELLENT → GOOD (note in findings) |
| LIM-03 partial | Partial run (interrupt/abort) | tentative EXCELLENT/GOOD → INCONCLUSIVE; POOR/FAILED evidence stands |
| LIM-04 udp-rate-assumed | UDPRateAssumed | Info note; UDP findings labeled "at assumed rate" |

### 4.4 Classification fold

```go
func classify(findings []Finding, f *Facts) model.HealthClass {
    worstPhys  := worst(findings, CatPhysical)
    hostLimited := hasMarker(findings, "HOST-01") || hasMarker(findings, "HOST-03")
    if worstPhys == SevFailed { return model.ClassFailed }
    if worstPhys == SevPoor   { return model.ClassPoor }
    worstTP := worst(findings, CatTransport, CatPerformance)
    tentative := model.ClassExcellent
    switch {
    case worstTP >= SevPoor:
        if hostLimited && worstPhys < SevWarning && allPoorAreHostSensitive(findings) {
            return model.ClassInconclusive // host-limited, cable NOT proven bad
        }
        tentative = model.ClassPoor
    case worstTP == SevWarning || worstPhys == SevWarning:
        tentative = model.ClassWarning
    case anyInfoDeviation(findings):
        tentative = model.ClassGood
    }
    // limitation caps (only ever downgrade toward INCONCLUSIVE/GOOD, never upgrade)
    if hasMarker(findings, "HOST-02") { return model.ClassInconclusive }
    if hasMarker(findings, "LIM-01") || hasMarker(findings, "LIM-03") {
        if tentative == model.ClassExcellent || tentative == model.ClassGood { return model.ClassInconclusive }
    }
    if hasMarker(findings, "LIM-02") && tentative == model.ClassExcellent { tentative = model.ClassGood }
    return tentative
}
```

The key asymmetry, per spec: physical POOR/FAILED is **never** softened by host evidence, but performance POOR **is** (→ INCONCLUSIVE) when hostLimited and the physical layer is clean. Note `allPoorAreHostSensitive`: a transport POOR (e.g. ping loss) isn't host-sensitive and keeps POOR even with hot CPUs.

### 4.5 Score (0–100)

Computed only when class ≠ INCONCLUSIVE; otherwise `Score == nil`, since a number would imply confidence we don't have. Start at 100, apply deductions, and clamp to the class band so score and class can never contradict:

| Deduction | Amount |
|---|---|
| each CRC-class error | −2 (cap −40) |
| each carrier event | −15 (cap −45) |
| renegotiation | −10 |
| half duplex | −25 |
| reduced speed | −15 |
| ping loss | −min(40, lossPct×20) per direction |
| full-size loss w/ clean ping | −20 |
| TCP retrans 0.1–1% / >1% | −5 / −15 per direction |
| CoV 15–30% / >30% | −5 / −15 |
| each collapse | −5 (cap −20) |
| UDP loss 0.5–2% / >2% | −5 / −15 per direction |
| throughput ratio 0.4–0.7 / <0.4 (not host-limited) | −10 / −25 |
| asymmetry >30% | −5 |
| jitter >5ms | −5 |

Bands (clamp after deductions): FAILED ≤25, POOR 26–50, WARNING 51–79, GOOD 80–94, EXCELLENT 95–100. EXCELLENT also requires zero findings above Info, all critical tests ran, ping loss 0 both dirs, retrans <0.1%, CoV <15%, UDP loss <0.5%, and throughput ≥90% of negotiated.

### 4.6 Recommendations

`var recommendations = map[string]string{...}` keyed by RuleID. The generator walks findings in order, appends the mapped strings, and de-dupes while preserving order. It always appends the isolation-test line when class ∈ {POOR, FAILED, INCONCLUSIVE}. Entries (abridged):

- `PHY-02/09/10`: "Reseat both connectors and inspect for damage; replace the cable with a known-good Cat5e/Cat6 and rerun."
- `PHY-06/07`: "Reduced link speed: 1000BASE-T needs all four pairs — test with another cable; verify both NICs advertise 1000 Mb/s (`ethtool <if>`)."
- `PHY-03/04`: "Intermittent link: check connector seating, try a different NIC port, run `--mode soak` to catch drops."
- `PHY-05`: "Half duplex usually means autonegotiation failure: enable autoneg on both sides; replace the cable."
- `PHY-08`: "Cable test reports open/short at ~{distance}m — replace or re-terminate the cable."
- `TR-06/07`: "Retest with `--parallel-streams 1`; correlate with counter deltas and CPU before blaming the cable."
- `HOST-01/03`: "Result appears host-limited: close background load, disable CPU power saving, avoid USB adapters, rerun."
- `HOST-02`: "Rerun on the physical interface — a virtual interface cannot exercise the cable."
- `LIM-01`: "Install the missing tools (iperf3/ethtool) and rerun for a conclusive result."
- isolation: "Isolation test: same machines with a different cable, then the same cable between different machines."

---

## 5. Reporting (`internal/reporting`)

### 5.1 Directory + raw store

```go
// Creates <base>/cablecheck-report-YYYY-MM-DD_HH-MM-SS (0700) and raw/ (0700).
// Collision: -2, -3 ... -99 (os.Mkdir, not MkdirAll — EEXIST detection must be atomic), then error.
func NewReportDir(base string, now time.Time) (dir string, err error)

type RawStore struct { /* dir string; mu sync.Mutex; seq int */ }
func NewRawStore(reportDir string) (*RawStore, error)
func (s *RawStore) Create(side, tool, purpose, ext string) (*os.File, string, error) // returns file + relative name
func (s *RawStore) Index() []model.RawFileRef // name, sha256, bytes, description — computed at close
```

Naming: `NN-side-tool-purpose.ext`, where NN is the two-digit creation sequence. Examples: `01-pc1-ethtool-link-before.txt`, `02-pc1-ip-stats-before.json`, `04-pc1-ping-stability.txt`, `06-pc1-iperf3-tcp-pc1-to-pc2.json`, `09-pc1-iperf3-udp-pc2-to-pc1.json`, `12-pc1-cablecheck-monitoring.jsonl`. A command's stderr, when non-empty, goes to `NN-...-stderr.txt` with the same NN. The protocol log has a fixed name outside the NN scheme, `raw/cablecheck-pc1.log`, created before any command runs.

### 5.2 Duration marshaling — decision

Custom `model.Duration` marshals to a two-field object. Raw `time.Duration` is banned from `model.Report`:

```go
type Duration time.Duration
func (d Duration) MarshalJSON() ([]byte, error)  // {"ms":30000,"text":"30s"}
func (d *Duration) UnmarshalJSON(b []byte) error // accepts {"ms":...}, bare number (ms), or "30s"
```

Justification: bare nanosecond integers (encoding/json's default for time.Duration) are a classic silent trap for JSON consumers and unreadable in diffs. A string alone forces every consumer to implement Go duration parsing. The `ms` integer plus `text` is machine-unambiguous and human-greppable at once. A reflection test walks `model.Report` and fails on any `time.Duration` field.

### 5.3 Report struct (`internal/model`)

```go
type HealthClass string // "EXCELLENT","GOOD","WARNING","POOR","FAILED","INCONCLUSIVE"

type Report struct {
    SchemaVersion string        `json:"schemaVersion"` // "1.0.0"
    ToolVersion   string        `json:"toolVersion"`
    TestID        string        `json:"testId"`
    GeneratedAt   time.Time     `json:"generatedAt"`
    StartedAt     time.Time     `json:"startedAt"`
    EndedAt       time.Time     `json:"endedAt"`
    Mode          string        `json:"mode"`
    Partial       bool          `json:"partial"`
    Config        ConfigEcho    `json:"config"`   // flags echo; token EXCLUDED by construction (no field)
    Machines      MachinePair   `json:"machines"` // hostname, kernel, NIC model/driver, tool versions per side
    Link          LinkSection   `json:"link"`     // before/after per side + events
    Counters      CountersSection `json:"counters"` // before/after/delta per side (Standard/Driver/Raw)
    Ping          []PingResult  `json:"ping,omitempty"`
    FullSizePing  []PingResult  `json:"fullSizePing,omitempty"`
    TCP           []TCPResult   `json:"tcp,omitempty"`
    UDP           []UDPResult   `json:"udp,omitempty"`
    Bidirectional *BidirResult  `json:"bidirectional,omitempty"`
    CableTest     *CableTestResult `json:"cableTest,omitempty"`
    Monitoring    []MonitoringEvent `json:"monitoring,omitempty"`
    Unavailable   []UnavailableTest `json:"unavailable,omitempty"` // {name, reason}
    Evaluation    Evaluation    `json:"evaluation"` // class, score *int, findings, recommendations, rulesVersion
    RawFiles      []RawFileRef  `json:"rawFiles"`
}
```

### 5.4 Rendering — builders for text, html/template for HTML

Markdown and the compact text summary use builders and shared formatting helpers. The self-contained browser report uses `html/template` so peer-derived strings are contextually escaped; its map-backed and filtered rows are prepared as deterministic view-model slices before template execution.

```go
func RenderJSON(r *model.Report) ([]byte, error)   // json.MarshalIndent, stable key order
func RenderMarkdown(r *model.Report) []byte         // pure function of Report — nothing else
func RenderSummary(r *model.Report) []byte          // ~30 lines plain text
func RenderHTML(r *model.Report) []byte             // inline CSS/SVG, no JS or external assets
```

One private `func sectionN(b *md, r *model.Report)` per section. `md` wraps `strings.Builder` with helpers `h2`, `kv`, `table(headers, rows)`, `note`. When data is absent, the section renders a one-line "Not run: <reason>" note rather than being silently omitted, so section numbering stays stable. Each section function gets its own golden test.

Section order (the 23): 1 Overall Result (class banner + one-line verdict) · 2 Score & Rule Evidence · 3 Session Info (testId, times, mode, partial) · 4 Machines & Environment · 5 Interface & Link Negotiation · 6 Link Events Timeline · 7 Counter Baseline · 8 Counter Deltas (table, per side, wrap/reset annotations) · 9 Ping Stability (both directions) · 10 Full-Size Ping · 11 TCP Throughput PC1→PC2 · 12 TCP Throughput PC2→PC1 · 13 Bidirectional Stress · 14 UDP Loss & Jitter · 15 CPU Utilization · 16 Cable Diagnostics · 17 Monitoring Timeline · 18 Findings Detail (every finding w/ evidence) · 19 Recommendations · 20 Limitations & Unavailable Tests · 21 Configuration Used · 22 Tool Versions · 23 Raw Artifact Index (name, sha256, size).

summary.txt: class + score, cable/link line (speed/duplex), 4-6 headline metrics (ping loss, retrans, UDP loss, throughput per dir), top 3 findings, top 3 recommendations, report dir path — hard cap ~30 lines, plain ASCII.

### 5.5 `cablecheck report <report.json>`

```go
// internal/app
func Regenerate(path string, outDir string, stdout io.Writer) error
```

Read the file (size cap 64 MiB), `json.Unmarshal` into `model.Report`, validate `schemaVersion` major == 1 (else ExitConfig with a message), and re-render report.md + summary.txt + report.html next to the JSON (or `--output`). **No re-evaluation**: Evaluation is part of the record. The purity of the reporting renderers (only `*model.Report` in, bytes out) is what makes this correct, and the dependency test from §0 enforces it structurally.

---

## 6. `doctor` command

Flags: `--interface`, `--output` (default "."), `--no-sudo`, `--verbose`. It runs locally, with no peer, read-only except for a temp-file writability probe.

```go
// internal/app
type CheckStatus string // "PASS" | "WARN" | "FAIL"
type CheckResult struct{ Name, Detail string; Status CheckStatus }
func Doctor(ctx context.Context, deps Deps, opts DoctorOptions) ([]CheckResult, error)
```

Checks, in order: (1) `ip` present + `ip -j addr` produces valid JSON; (2) `ping` present + version string; (3) `iperf3` present + version ≥3.1, capability probes (JSON always for ≥3.1; `--bidir` gated on version ≥3.7; reverse/UDP/get-server-output from a `--help` scan), missing = FAIL with distro install hints (pacman/apt/dnf); (4) `ethtool` present + version, missing = **FAIL** since preflight requires it; (5) interface enumeration: each interface with state, speed/duplex if readable, IPv4 addrs, physical/virtual classification (virtual ones annotated "requires --allow-virtual-interface"), no physical candidates = WARN; (6) `sudo -n true` probe (skipped/N-A with `--no-sudo`), failure = WARN; (7) cable-test support: version gate only (ethtool ≥5.4 + kernel note), reported as WARN "cannot verify without running — driver support varies" (doctor never actually runs `--cable-test`); (8) output dir writability (create+remove temp file), failure = FAIL.

Output: aligned `[PASS]/[WARN]/[FAIL] name: detail` lines plus a summary line `N PASS, N WARN, N FAIL`. Exit: any FAIL → `&ExitError{Code: ExitConfig}` (4), else 0. Doctor never emits exit 3.

## 7. `version` command

```
cablecheck 1.0.0
commit:   4f2a1c9
built:    2026-07-15T10:22:03Z
go:       go1.26.3
platform: linux/amd64
protocol: 1
schema:   1.0.0
```

`go`/`platform` come from `runtime.Version()` and `runtime.GOOS/GOARCH`; protocol and schema constants come from their packages. Makefile:

```make
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(shell git rev-parse --short HEAD) -X main.date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
build: ; CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o cablecheck ./cmd/cablecheck
```

`BuildInfo{Version, Commit, Date string}` flows main → cli → app; `Report.ToolVersion = build.Version`.

## 8. Logging (`internal/logging`)

```go
func NewStderr(w io.Writer, verbose bool) *slog.Logger
// text handler, LevelInfo (LevelDebug when verbose), no source

func AttachDebugFile(base *slog.Logger, path string) (*slog.Logger, io.Closer, error)
// wraps base's handler + a JSON handler (LevelDebug, ReplaceAttr: redact) over a bufio.Writer
// on os.OpenFile(path, O_CREATE|O_WRONLY|O_EXCL, 0600); Closer flushes+closes.

func MsgAttrs(dir, msgType, messageID string, payloadBytes int) slog.Attr
// slog.Group("msg","dir",dir,"type",msgType,"id",messageID,"bytes",payloadBytes)
```

Lifecycle: `cli` creates the stderr logger immediately. `app.Run` creates the report dir right after config validation (before preflight, so preflight raw output has a home), then calls `AttachDebugFile(logger, filepath.Join(dir, "raw", "cablecheck-"+role+".log"))` and uses the tee logger for everything after. The returned Closer is deferred and closed **after** the last log write, before report-dir checksumming. The tee is a small `multiHandler` fan-out (stdlib has none); each child keeps its own level, so the file always gets Debug regardless of `--verbose`.

Token-redaction guarantees come in three independent layers. (1) **Construction**: protocol code logs envelopes only via `MsgAttrs` (type/id/size, never payload bytes, never the hello raw), and a lint-style test greps `internal/protocol` for `Payload` appearing inside slog calls. (2) **Handler**: `ReplaceAttr` on *both* handlers replaces the value of any attr keyed `token`/`payload` with `"[REDACTED]"`, and it fires for attrs inside groups too. (3) **Types**: `config.RunConfig` implements `slog.LogValuer` with Token redacted, so even a naive `slog.Any("cfg", cfg)` is safe. Regression test: run a mock handshake, capture both sinks, assert the token substring is absent. The one legitimate token display (the PC1 banner) goes to stdout via `fmt`, never through slog.

## 9. Example-report harness

Decision: **generated by a committed hermetic generator + a drift test**, not hand-committed artifacts and not generated at build time.

- `internal/tools/genexamples/main.go` — a `go run`-able command; Makefile target `examples:` runs `go run ./internal/tools/genexamples -out examples`, plus a `//go:generate` directive in `internal/reporting/doc.go`.
- Each scenario (`healthy`, `reduced-speed`, `crc-errors`, `host-limited`, `failed`) is a seed: a pre-Evaluation `model.Report` built in Go code from the same parsed-result structs the parsers produce. Values are traceable to `testdata/` fixtures; the crc-errors seed, for example, uses the deltas from the Realtek CRC fixture. The generator runs the *real* pipeline slice in scope: `evaluate.FactsFromReport` → `evaluate.Evaluate` → set `Evaluation` → all reporting renderers → write `examples/<name>/{report.json,report.md,summary.txt,report.html}`.
- Determinism: fixed timestamps in seeds (`2026-01-02T15:04:05Z`), fixed testId (`example-<name>`), token never present (ConfigEcho has no token field), no clock, no rand.
- Drift guard: `TestExamplesUpToDate` in `internal/tools/genexamples` regenerates into `t.TempDir()` and byte-compares against `examples/`, with a failure message of `run: make examples`. This makes it impossible for evaluate/reporting changes to land without regenerated examples.

Expected outcomes baked into the seeds: healthy → EXCELLENT/exit 0; reduced-speed → WARNING (PHY-06); crc-errors → POOR or FAILED (PHY-02 band); host-limited → INCONCLUSIVE (PERF-01 + HOST-01, clean counters); failed → FAILED (PHY-01 + PHY-03).

## Pitfalls

1. **`flag.ExitOnError` calls `os.Exit(2)`** — collides with our exit contract. Use `ContinueOnError` everywhere; map parse errors to 4, `flag.ErrHelp` to 0.
2. **Boolean flags in stdlib take no separate argument**: `--cable-test false` parses `--cable-test=true` and treats `false` as a positional. Reject unexpected positionals after parse and document `--flag=false` in usage.
3. **`flag.Visit` only reports flags set on the command line** — presets must be applied *after* Parse; and an explicit `--tcp-duration 0` is distinguishable from unset only via Visit, then killed by bounds validation (good).
4. **`netip.ParseAddr` accepts 4-in-6 (`::ffff:192.0.2.1`) and zones** — call `Unmap()` before `Is4()` or the demo docs' addresses behave inconsistently. It also rejects leading-zero octets (`192.168.001.001`), so give a clear error; users paste these.
5. **`encoding/json` marshals `time.Duration` as bare nanoseconds** silently — ban raw `time.Duration` in `model.Report` via the reflection test; use `model.Duration` everywhere.
6. **Report-dir collision** on same-second runs (PC1+PC2 demo on one machine, tests): use `os.Mkdir` (not MkdirAll) so EEXIST is atomic; suffix `-2..-99`.
7. **iperf3 JSON has no segment count** — the retransmit *rate* must be estimated as `retransmits / (bytes/MSS)` with MSS from `tcp_mss_default` (fallback 1448). Label it "estimated" in evidence or the 0.1% threshold looks falsely precise.
8. **TCP slow-start poisons CoV/collapse stats** — exclude the first interval from CoV/median or every run "collapses" at t=0.
9. **UDP loss when the sender never reached target rate** (CPU-bound or `--udp-rate` too high) isn't cable loss. TR-07 must gate on `UDPTargetReached && CPU ≤ 90 && !UDPNearSaturation`, else host-limited runs misclassify as POOR.
10. **Score/class contradiction** — independent score arithmetic can produce "FAILED, score 88" on sparse evidence. Always clamp score into the class band, and emit no score for INCONCLUSIVE.
11. **Log file inside the report dir**: create the dir before preflight or early raw output has nowhere to go. Flush/close the bufio'd JSON handler on interrupt *before* computing raw-file checksums, or hashes won't match transferred files.
12. **Token leakage paths are plural**: slog attrs, envelope payload dumps, RunConfig `%+v` in error messages, and the PC1 banner. The three-layer redaction (§8) plus banner-via-stdout covers all four. Keep the grep-test so refactors can't reintroduce payload logging.
13. **Ports**: iperf-port+1 is consumed by the bidir fallback. Validate `≤65534` and that port+1 ≠ control-port, or the fallback fails mid-run after preflight passed.
14. **Counter wrap/reset**: deltas are `(after-before, ok)`. A false `ok` must remove that side's counters from Facts (`DeltaOK=false`) rather than report a bogus 2^32-ish delta that triggers PHY-02 FAILED.
15. **Half-open cap semantics**: limitation caps must only ever *downgrade* (EXCELLENT/GOOD → INCONCLUSIVE); applying them to POOR/FAILED would hide real failure evidence. Order the fold exactly as §4.4.
16. **Go 1.24+ `GOEXPERIMENT=jsonv2`** can alter marshaling details, and the drift test pins output. Run CI without the experiment and note it in Makefile comments.
