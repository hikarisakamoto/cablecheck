# CableCheck — External Tool Execution & Parsing Design
Scope: `internal/runner`, `internal/network`, `internal/parser`, `internal/testsuite`, `testdata/`. Field names below marked **[verified]** were checked live on this dev machine (iproute2 6.x JSON, iputils 20250605, sysfs on kernel 7.0.9-arch2-1).

Package dependency direction (no cycles):
`parser` (pure, no exec, depends only on `model`) ← `testsuite` → `runner`, `network`, `parser`, `model`; `network` → `runner` (for `ip` exec) + direct sysfs reads; `runner` depends on nothing internal except `clock`/`logging`. Result models (`PingResult`, `Iperf3Result`, `CounterSnapshot`, `LinkSettings`, `CableTestResult`, …) live in `internal/model` so `evaluate`/`reporting` consume them without importing parsers.

---

## 1. `internal/runner` — CommandRunner

### Types

```go
package runner

type CommandSpec struct {
    Name           string        // program name; resolved via exec.LookPath (never a shell string)
    Args           []string
    Env            []string      // appended to base env; base = os.Environ() + LC_ALL=C, LANG=C (always)
    Stdin          io.Reader
    Timeout        time.Duration // 0 = bounded only by ctx
    GracePeriod    time.Duration // SIGTERM→SIGKILL escalation; default 3s
    MaxOutputBytes int64         // per-stream in-memory cap; default 4 MiB
    TeeStdoutPath  string        // "" = no tee; else full stream copied to this file (report raw/ dir)
    TeeStderrPath  string
    Label          string        // slug for logging + default raw filenames, e.g. "iperf3-tcp-fwd"
}

type CommandResult struct {
    Spec            CommandSpec
    Stdout, Stderr  []byte
    StdoutTruncated bool          // in-memory cap hit; tee file (if any) is complete
    StderrTruncated bool
    ExitCode        int           // -1 if killed by signal
    Signal          string        // "SIGKILL" etc., "" if exited normally
    Started         time.Time
    Duration        time.Duration
    TimedOut        bool
}

type Runner interface {
    Run(ctx context.Context, spec CommandSpec) (*CommandResult, error)
    Start(ctx context.Context, spec CommandSpec) (Process, error)
}

type Process interface {
    PID() int
    Stdout() io.Reader          // live, line-oriented (readiness scanning); capped copy still lands in result
    Wait() (*CommandResult, error) // idempotent; safe from multiple goroutines
    Terminate() error           // SIGTERM to process group
    Kill() error                // SIGKILL to process group
    Done() <-chan struct{}
}
```

### Error contract (decision)
`err != nil` **only** for infrastructure failures; non-zero exit is *data*, not error (ping exits 1 on any loss):
- exec not found: `Run` returns `fmt.Errorf("%s: %w", name, exec.ErrNotFound)` → callers use `errors.Is(err, exec.ErrNotFound)` (LookPath wraps it already; preserve the chain).
- timeout: result non-nil with `TimedOut=true`, err satisfies `errors.Is(err, ErrTimeout)` **and** `errors.Is(err, context.DeadlineExceeded)`.
- session cancel: err wraps `context.Canceled`; partial output preserved in result.
- non-zero exit / signaled: `err == nil`; caller inspects `ExitCode`/`Signal`. Helper `func (r *CommandResult) Failed() bool { return r.ExitCode != 0 }`.

### Process-group and escalation (exact mechanism)
- `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` → child pgid == child pid; grandchildren (sudo→ethtool, iperf3 threads) inherit it.
- `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }` (ignore `ESRCH`).
- Own escalation timer: when ctx (or per-spec `context.WithTimeout`) fires, after `GracePeriod` send `syscall.Kill(-pgid, SIGKILL)`.
- `cmd.WaitDelay = GracePeriod + 2*time.Second` as **backstop only** — see Pitfalls: WaitDelay's kill hits only the direct child, but it also un-hangs `Wait()` when a grandchild holds the stdout pipe open. Both mechanisms are required.
- Timeout attribution: set a `timedOut atomic.Bool` in the timer callback before signaling; after `Wait`, `TimedOut = timedOut.Load()` (don't infer from `ctx.Err()` alone — parent cancel ≠ timeout).

### Output capture
`cappedWriter{max int64}`: stores up to `max` bytes, then discards and sets `Truncated`; on finalize appends marker line `"\n[cablecheck: output truncated at %d bytes; full stream in %s]\n"`. Wiring: `cmd.Stdout = io.MultiWriter(teeFile, capped)` — tee files get the **complete** stream (that's the point of `raw/`), memory is capped. For `Start()`, add a third leg: an `io.Pipe` for live readiness scanning (reader must be drained; the manager always drains it in a goroutine).

### Cleanup registry (session-owned PIDs)

```go
type ProcessInfo struct {
    PID, PGID  int
    StartTicks uint64 // /proc/<pid>/stat field 22 (parse after the last ')')
    Argv0      string // expected basename, e.g. "iperf3"
    Label      string
    TestID     string
}

type Registry struct { /* mutex, map[int]ProcessInfo, stateDir string */ }
func NewRegistry(testID string) (*Registry, error) // stateDir = ${XDG_RUNTIME_DIR:-/tmp}/cablecheck/<testID>/
func (r *Registry) Register(p ProcessInfo) (unregister func(), err error) // also writes <pid>.json pidfile
func (r *Registry) KillAll(ctx context.Context) []error // verified SIGTERM group → grace → SIGKILL group
func VerifyOwnership(p ProcessInfo) bool // re-reads /proc/<pid>/stat starttime + /proc/<pid>/cmdline argv[0]
func ScanStale(baseDir string) ([]ProcessInfo, error) // preflight: old testID dirs → validate pidfiles
```

Rules: never `pkill`/`killall`; never signal a PID unless `VerifyOwnership` passes (starttime match defeats PID reuse). `Wait()` unregisters and removes the pidfile. Preflight stale check: `ScanStale` over `${XDG_RUNTIME_DIR:-/tmp}/cablecheck/*`; verified-live stale iperf3 → report + offer kill; dead pidfiles → clean up silently.

### Test double
`internal/runner/runnertest.FakeRunner`: ordered script of `Stub{MatchArgv []string /* prefix match */, Result CommandResult, Err error, Delay time.Duration}` + call recording; `runnertest.FromFixture(dir, name)` loads the triplet convention (`name.stdout`, `name.stderr`, `name.exit` — missing stderr/exit ⇒ empty/0; stdout-only fixtures are plain `name.txt`).

---

## 2. iperf3 management (`internal/testsuite/iperf.go`)

### Placement & lifecycle (decisions)
- Server **always on the receiving side**, one-shot (`-1`) per phase. Rationale: clean lifecycle (server exits when the test ends — no stale listeners between phases), sender-side client JSON carries everything including `sum_received` and remote CPU. `-R` is **never used**: direction change = swap server side. This kills a whole class of `-R` JSON asymmetries.
- Exception: `--bidir` (single server on PC2, client on PC1 runs `--bidir`).
- Server argv: `iperf3 -s -B <localIP> -p <port> -1 --forceflush` (bind to the tested link only — mirrors control-plane rule and keeps multi-homed hosts honest).
- Client argv base: `iperf3 -c <peerIP> -B <localIP> -p <port> -J --connect-timeout 3000`.

### Invocation matrix

| Phase | Server host | Client argv additions |
|---|---|---|
| TCP PC1→PC2 | PC2 | `-t <tcpDur> -P <streams>` |
| TCP PC2→PC1 | PC1 | same, run from PC2 |
| TCP bidir | PC2 | `-t <tcpDur> -P <streams> --bidir` (PC1 client) |
| UDP A→B | B | `-u -b <int bits/s> -t <udpDur> -l <mtu-28>` |

`-b` is passed as a **plain integer** (e.g. `800000000`), computed as 80% of negotiated speed — avoids suffix/decimal parsing differences across iperf3 builds. `-l mtu-28` pins one datagram = one frame (no IP fragmentation), aligning UDP loss with cable behavior. Runner `Timeout = testDuration + 15s`; server `Timeout = testDuration + 30s`.

### Readiness detection (decision: stdout banner, NOT port probe)
`StartServer` scans live stdout for prefix `"Server listening on "` (requires `--forceflush`; see Pitfalls). Fallback: if no banner but process still alive after 1.5 s → ready (bind failures exit immediately with stderr `"unable to start listener for connections: Address already in use"` → typed `ErrPortInUse`). **Never** TCP-connect-probe a `-1` server: a cookie-less connect can consume/abort the one-off session. Readiness is then acked to the peer over the control protocol; only after the ack does the sender launch the client.

```go
type IperfManager struct { R runner.Runner; Reg *runner.Registry; RawDir string }
func (m *IperfManager) DetectCapabilities(ctx context.Context) (model.IperfCaps, error)
func (m *IperfManager) StartServer(ctx context.Context, bind netip.Addr, port uint16) (*ServerHandle, error)
func (h *ServerHandle) Ready(ctx context.Context) error         // banner-or-1.5s logic
func (h *ServerHandle) Stop(ctx context.Context) error          // graceful; no-op if already exited
func (m *IperfManager) RunTCPClient(ctx context.Context, local, peer netip.Addr, port uint16, dur time.Duration, streams int, bidir bool) (model.Iperf3Result, error)
func (m *IperfManager) RunUDPClient(ctx context.Context, local, peer netip.Addr, port uint16, dur time.Duration, rateBps uint64, payload int) (model.Iperf3Result, error)
```

### Capability detection (what is reliable)
```go
type IperfCaps struct {
    RawVersion   string // "iperf 3.16 (cJSON 1.7.15)"
    Major, Minor int
    JSON, Reverse, Bidir, OneOff, GetServerOutput, UDP bool
}
```
- `iperf3 --version` first line, regex `^iperf (\d+)\.(\d+)` → reliable for upstream feature *semantics* (bidir JSON shape changed over time).
- `iperf3 --help` usage text greps: `--bidir`, `--one-off`, `--json`, `--get-server-output` → reliable for *flag acceptance* (usage text is generated from the accepted option table; catches distro patches/backports).
- Rule: a capability is claimed only if **both** signals agree (`Bidir = ver>=3.7 && helpHas("--bidir")`). `JSON`, `Reverse`, `UDP` are unconditional for any 3.x (present since 3.0/3.1; if `--version` doesn't say `iperf 3`, preflight fails with "iperf3 3.7+ required"). Support window: 3.7–3.17.
- Both peers exchange `IperfCaps` in the capabilities message; effective caps = AND. No `--bidir` on either side ⇒ two coordinated one-way phases (report as limitation, never cable failure).

### JSON parsing across 3.7–3.17 (`internal/parser/iperf.go`)
Wire structs (all fields optional-tolerant; `json.Unmarshal` ignores unknowns):

```go
type iperfWire struct {
    Start struct {
        Version   string `json:"version"`
        TestStart struct {
            Protocol string `json:"protocol"`; NumStreams int `json:"num_streams"`
            Duration float64 `json:"duration"`; Reverse int `json:"reverse"`; Blksize int `json:"blksize"`
        } `json:"test_start"`
    } `json:"start"`
    Intervals []struct {
        Sum             *iperfSum `json:"sum"`
        SumBidirReverse *iperfSum `json:"sum_bidir_reverse"` // bidir intervals, reverse direction
    } `json:"intervals"`
    End struct {
        Streams []struct {
            Sender   *iperfSum `json:"sender"`   // TCP
            Receiver *iperfSum `json:"receiver"` // TCP
            UDP      *iperfUDP `json:"udp"`      // UDP
        } `json:"streams"`
        SumSent     *iperfSum `json:"sum_sent"`     // TCP
        SumReceived *iperfSum `json:"sum_received"` // TCP
        Sum         *iperfUDP `json:"sum"`          // UDP (server-reported loss on sender side)
        CPU *struct {
            HostTotal float64 `json:"host_total"`; HostUser float64 `json:"host_user"`; HostSystem float64 `json:"host_system"`
            RemoteTotal float64 `json:"remote_total"`; RemoteUser float64 `json:"remote_user"`; RemoteSystem float64 `json:"remote_system"`
        } `json:"cpu_utilization_percent"`
        SenderTCPCongestion   string `json:"sender_tcp_congestion"`
        ReceiverTCPCongestion string `json:"receiver_tcp_congestion"`
    } `json:"end"`
    Error string `json:"error"` // present+non-empty on client failure even in -J mode
}
type iperfSum struct {
    Start, End, Seconds float64
    Bytes         uint64  `json:"bytes"`
    BitsPerSecond float64 `json:"bits_per_second"`
    Retransmits   *uint64 `json:"retransmits"` // pointer: absent for receiver side and all UDP
    Sender        bool    `json:"sender"`
}
type iperfUDP struct {
    Bytes uint64 `json:"bytes"`; BitsPerSecond float64 `json:"bits_per_second"`
    JitterMs float64 `json:"jitter_ms"`; LostPackets int64 `json:"lost_packets"`
    Packets int64 `json:"packets"`; LostPercent float64 `json:"lost_percent"`
    OutOfOrder *int64 `json:"out_of_order"` // absent on some versions
    Sender bool `json:"sender"`
}
```

Version-difference handling (all encoded in `ParseIperf3`):
- **TCP**: prefer `end.sum_sent`/`end.sum_received` (present across 3.7–3.17). `retransmits` only on sender-side sums/streams — model it as `*uint64`, propagate absence (absent ≠ 0).
- **UDP**: only `end.sum` exists; on the sending client it carries **server-observed** loss/jitter (that's what we want). No retransmits ever.
- **Bidir**: 3.7–3.11 have a known bug emitting duplicate/misattributed `sum_sent`/`sum_received` keys (Go's decoder silently keeps the last). Decision: in bidir mode **ignore top-level end sums entirely** and aggregate from `end.streams[]`, partitioning by each stream's `sender.sender` boolean (`true` = client→server). Same for intervals: use `intervals[].sum` (fwd) + `intervals[].sum_bidir_reverse` (rev), tolerating absence of the latter by re-deriving from interval streams.
- **3.16/3.17** (multithreaded): identical top-level shape; per-interval stream timestamps may stagger a few ms — interval analysis uses `sum` rows only, so it's immune.
- `error` field non-empty ⇒ return `model.Iperf3Result{Error: ...}` plus typed `ErrIperfClient`; never treat exit-1 client output as unparseable before attempting JSON decode.

Normalized model:

```go
type Iperf3Result struct {
    Version, Protocol string
    Streams           int
    DurationSec       float64
    Sent, Received    *DirStats      // TCP one-way; Sent.Retransmits may be nil
    Bidir             *BidirStats    // {LocalToPeer, PeerToLocal DirStats}, derived from streams
    UDP               *UDPStats      // TargetBps, ActualBps, JitterMs, Lost, Total, LostPercent, OutOfOrder *int64
    Intervals         []IntervalStat // per-second sum rows: Bps, Retransmits *uint64
    IntervalMinBps, IntervalMaxBps, IntervalAvgBps, IntervalCoV float64
    Collapses         []CollapseEvent // interval Bps < 10% of median → {StartSec, Len, MinBps}
    CPU               *CPUUtil
    CongSender, CongReceiver string
    Error             string
}
```

### Port availability probe (`internal/network`)
`ProbePortFree(ip netip.Addr, port uint16) error`: `net.Listen("tcp", ip:port)` **and** `net.ListenPacket("udp", ip:port)`, close both. Both families matter — iperf3 UDP data uses the same port number over UDP. `EADDRINUSE` on a specific-IP bind also catches wildcard binds by other processes.

### Stale-process identification
A CableCheck-owned iperf3 is identified **only** by Registry pidfiles: `{pid, startTicks, argv0:"iperf3", testID}`. Before any signal: re-read `/proc/PID/stat` (starttime equal) and `/proc/PID/cmdline` (NUL-split argv[0] basename == `iperf3`, args contain our `-p <port>`). Anything else — including an iperf3 the user runs themselves — is reported in preflight as "port busy / foreign iperf3" and never killed.

---

## 3. ping (`internal/testsuite/ping.go` + `internal/parser/ping.go`)

### Invocation
- Quick: `ping -n -D -c 500 -i 0.02 -W 1 -w <ceil(500*i)+10> <peer>` (LC_ALL=C via runner base env). `-n` avoids rDNS stalls, `-D` gives per-reply epoch timestamps for real time-gap math, `-w` bounds wall time (runner timeout is the backstop at `-w`+10 s).
- Full-size: `ping -n -D -M do -s <MTU-28> -c 100 -i 0.2 -W 2 -w 40 <peer>` (1472 for MTU 1500; IPv4: 20 IP + 8 ICMP).

### Interval fallback ladder (0.02 → 0.2 → 1.0)
Trigger: exit code 2 **and** stderr contains both `"cannot flood"` and `"minimal interval"`. **[verified]** modern iputils 20250605 emits `ping: cannot flood, minimal interval for user must be >= 2 ms, use -i 0.002 (or higher)` (comma); legacy (<2021) emits `ping: cannot flood; minimal interval allowed for user is 200ms` (semicolon) — the two shared substrings above match both, and LC_ALL=C defeats NLS translation. On trigger, retry next rung; record `IntervalUsed` in the result and a limitation note (quick-mode gap analysis granularity degrades). 0.02 works unprivileged on iputils ≥2021 (≥2 ms rule) — verified `-i 0.002` exit 0 unprivileged on this machine.

### Line grammar (iputils only — busybox detected and rejected)
Decision: busybox ping is **rejected at preflight**, not supported: `ping -V` must contain `iputils` (**[verified]** `ping from iputils 20250605`); otherwise (or if `exec.LookPath("ping")` resolves through a symlink whose target basename is `busybox`) preflight fails with an install hint. Justification: busybox uses `seq=` not `icmp_seq=`, has no `mdev`, no `-D`, unreliable fractional `-i` — supporting it doubles the grammar for a target audience (desktop/server distros) that universally ships iputils. The parser still recognizes busybox-shaped lines just enough to return `ErrUnsupportedPingFormat` (fixture-tested) instead of silently reporting 100% loss.

Regexes (LC_ALL=C, iputils):
- Reply: `^\[(\d+\.\d+)\] (\d+) bytes from ([0-9a-fA-F:.]+): icmp_seq=(\d+) ttl=(\d+) time=([\d.]+) ms( \(DUP!\))?$` **[verified shape]**
- ICMP error: `^\[?[\d.]*\]? ?From (\S+):? icmp_seq=(\d+) (.+)$` (Destination Host Unreachable, Frag needed and DF set (mtu = 1500), …)
- Local send error: `^(\[[\d.]+\] )?ping: (local error: .+|sendmsg: .+)$` (EMSGSIZE from `-M do` payload > MTU)
- Summary: `^(\d+) packets transmitted, (\d+) received(?:, \+(\d+) duplicates)?(?:, \+(\d+) errors)?, ([\d.]+)% packet loss, time (\d+)ms$` **[verified shape]**
- RTT: `^rtt min/avg/max/mdev = ([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+) ms(, pipe \d+)?$` **[verified shape]**

Unknown lines: counted (`UnparsedLines int`), never fatal — raw output is always in `raw/` anyway.

### Analysis
```go
type PingResult struct {
    Target                      string
    Transmitted, Received       int
    Duplicates, SendErrors, IcmpErrors int
    LossPercent                 float64
    RTTMinMs, RTTAvgMs, RTTMaxMs, RTTMdevMs float64
    Percentiles                 map[int]float64 // 50,90,95,99 nearest-rank over first-reply RTTs
    Spikes                      []PingSpike     // rtt > max(5*p50, p50+10ms)
    MissingSeqRuns              []SeqRun        // {FirstSeq,Len}
    LongestSeqGap               int
    LongestGapMs                float64         // max Δ between consecutive reply timestamps (-D)
    IntervalUsedSec             float64
    ExitCode                    int
    UnparsedLines               int
}
```
Semantics: percentiles/spikes computed over the **first** reply per seq (DUPs tracked separately — duplicates on a direct cable are themselves evidence). `LongestGapMs` from `-D` timestamps is the "longest response gap" (captures burst loss *and* stalls); seq-run analysis localizes which packets vanished. Exit 1 with a parsed summary = valid result with loss, not an error.

### Testsuite API
```go
type PingTester struct { R runner.Runner; RawDir string }
func (t *PingTester) Quick(ctx context.Context, peer netip.Addr, count int) (model.PingResult, error)      // owns the ladder
func (t *PingTester) FullSize(ctx context.Context, peer netip.Addr, mtu, count int) (model.PingResult, error)
```
Both directions = each peer runs its own ping against the other (coordinated by the protocol layer); no remote execution.

---

## 4. ethtool parsing (`internal/parser/ethtool.go`)

### Base settings (`ethtool <if>`)
Line-oriented state machine: a trimmed line containing `": "` (or ending `:`) starts key/value; the three link-mode keys (`Supported link modes`, `Advertised link modes`, `Link partner advertised link modes`) enter list mode where subsequent deeper-indented colon-free lines are whitespace-split into mode tokens (`1000baseT/Full`). `Not reported` ⇒ empty list.

```go
type LinkSettings struct {
    SpeedMbps    int      // -1 for "Unknown!"
    Duplex       string   // "full"|"half"|"unknown"  (from "Full"/"Half"/"Unknown! (255)")
    LinkDetected bool     // "Link detected: yes"
    AutoNeg      string   // "on"|"off"|"unknown"
    Port         string   // "Twisted Pair", "MII", ...
    SupportedPorts, SupportedModes, AdvertisedModes, PartnerModes []string
    MDIX         string
    Raw          map[string]string // every simple key:value line
}
```
Speed regex `^(\d+)Mb/s$`; `Unknown!` → -1 (no-link or virtual). `PartnerModes` empty + autoneg on + link up ⇒ "partner did not report" (finding, not failure). Multiline `Current message level` continuation lines are colon-free but we're not in list mode there — they're appended to `Raw["Current message level"]`.

### `ethtool -S` (`ParseEthtoolStats`)
Skip banner line (`NIC statistics:`); accept only `^\s*([A-Za-z0-9_\[\]. -]+?):\s+(\d+)\s*$` → `map[string]uint64`. Anything else ignored (some drivers emit section headers/hex). Duplicate names (per-queue collisions don't happen; identical names would be driver bugs): last wins, log once.

### `--cable-test` (`ParseCableTest(stdout, stderr []byte, exitCode int)`)
Netlink-era grammar:
```
Cable test started for device eth0.
Cable test completed for device eth0.
Pair A code OK
Pair C code Open Circuit
Pair C, fault length: 32.00m
```
- `^Pair ([A-D]) code (.+)$` → code map: `OK`→OK, `Open Circuit`→OPEN, `Short within Pair`→SHORT_INTRA, `Short to another pair`→SHORT_INTER, `Impedance mismatch`→IMPEDANCE, anything else→UNSPECIFIED with `RawCode` preserved.
- `^Pair ([A-D]), fault length: ([\d.]+)m$` attaches distance.
- Unavailability: exit≠0 with stderr containing `Operation not supported` (or `netlink error` + EOPNOTSUPP text) → `Available=false, Reason="driver does not support cable test"`. `Operation not permitted` → retry once via sudo path if allowed, else UNAVAILABLE("requires root"). Pre-netlink ethtool (no such flag): stderr `bad command line argument` / usage dump → UNAVAILABLE("ethtool too old (netlink cable-test requires ethtool ≥5.4 + kernel ≥5.4)"). Never FAILED from unavailability.

```go
type CableTestResult struct {
    Available bool; UnavailableReason string
    Pairs []PairResult // {Pair string; Status PairStatus; RawCode string; FaultMeters float64; HasFault bool}
}
```

### `--cable-test-tdr` (`ParseCableTestTDR`)
Shape: header `Cable test TDR data for device X.` then per-sample lines pairing `Pair <A-D>`, a distance, and a signed amplitude. Because ethtool's TDR text has shifted between 5.x/6.x releases, the parser is tolerant: within any line starting `Pair `, independently extract `Pair ([A-D])`, `distance[:\s]+([\d.]+)\s*c?m`, `amplitude[:\s]+(-?\d+)`; store `TDRSample{Pair, DistanceM, Amplitude}`; unmatched lines only raise `UnparsedLines`. Result mirrors `CableTestResult` (`Available/Reason/Samples/UnparsedLines`). Evaluator uses TDR only as supporting evidence, so lossy parsing is acceptable.

---

## 5. `ip -j` parsing (`internal/parser/iplink.go`) — field names **[verified live]**

`ip -j addr show` element (verified: `ifindex,ifname,flags,mtu,qdisc,operstate,group,txqlen,link_type,address,broadcast,altnames,addr_info[]`, addr_info: `family,local,prefixlen,metric,broadcast,scope,dynamic,noprefixroute,label,valid_life_time,preferred_life_time,protocol`; **operstate is UPPERCASE** (`"UP"/"DOWN"/"UNKNOWN"`); WireGuard shows `link_type:"none"`, wifi/ether `"ether"`, loopback `"loopback"`):

```go
type IPAddrInfo struct {
    Family    string `json:"family"`    // "inet" | "inet6"
    Local     string `json:"local"`
    Prefixlen int    `json:"prefixlen"`
    Scope     string `json:"scope"`     // "global" | "host" | "link"
    Label     string `json:"label"`
    Dynamic   bool   `json:"dynamic"`
}
type IPLink struct {
    Ifindex   int          `json:"ifindex"`
    Ifname    string       `json:"ifname"`
    Flags     []string     `json:"flags"`     // "LOOPBACK","BROADCAST","UP","LOWER_UP","NO-CARRIER","POINTOPOINT","NOARP"
    MTU       int          `json:"mtu"`
    Operstate string       `json:"operstate"` // UPPERCASE
    LinkType  string       `json:"link_type"` // "ether" | "loopback" | "none"
    Address   string       `json:"address"`
    Altnames  []string     `json:"altnames"`
    AddrInfo  []IPAddrInfo `json:"addr_info"`
    Stats64   *IPStats64   `json:"stats64"`
}
```

`ip -j -s -s link show dev X` — **the detailed error counters are FLAT inside `rx`/`tx`** (not a nested `rx_errors` object; verified against live iproute2):

```go
type IPStats64 struct {
    RX IPRxStats `json:"rx"`
    TX IPTxStats `json:"tx"`
}
type IPRxStats struct {
    Bytes, Packets, Errors, Dropped uint64 `json:"bytes","packets","errors","dropped"` // (one tag per field in real code)
    OverErrors   uint64 `json:"over_errors"`
    Multicast    uint64 `json:"multicast"`
    // below present only with -s -s:
    LengthErrors uint64 `json:"length_errors"`
    CRCErrors    uint64 `json:"crc_errors"`
    FrameErrors  uint64 `json:"frame_errors"`
    FifoErrors   uint64 `json:"fifo_errors"`
    MissedErrors uint64 `json:"missed_errors"`
    Nohandler    uint64 `json:"nohandler"` // emitted only when nonzero
}
type IPTxStats struct {
    Bytes, Packets, Errors, Dropped uint64
    CarrierErrors   uint64 `json:"carrier_errors"`
    Collisions      uint64 `json:"collisions"`
    // -s -s only:
    AbortedErrors   uint64 `json:"aborted_errors"`
    FifoErrors      uint64 `json:"fifo_errors"`
    WindowErrors    uint64 `json:"window_errors"`
    HeartbeatErrors uint64 `json:"heartbeat_errors"`
    CarrierChanges  uint64 `json:"carrier_changes"` // yes, under tx — verified
}
```
Both commands return a JSON **array**; `ParseIPLinkStats` requires exactly one element for `show dev X`. `Stats64` nil ⇒ typed error (all supported kernels emit stats64).

---

## 6. Counter normalization (`internal/testsuite/counters.go`)

```go
type CounterSnapshot struct {
    CapturedAt time.Time
    Driver     string                     // from sysfs readlink, e.g. "e1000e","r8169","r8152","virtio_net"
    Standard   map[StdKey]StdCounter      // normalized view
    Raw        map[string]uint64          // full ethtool -S, untouched
    IPStats    model.IPStats64            // full ip -j -s -s snapshot
}
type StdCounter struct { Value uint64; Source string /* "ethtool:rx_crc_errors" | "iplink:rx.crc_errors" | "sysfs:carrier_changes" */ }
// absent key in Standard means "no data", which is NOT zero.
```

Resolution order per key: driver-specific ethtool name → generic ethtool candidates (first present wins) → `ip -s -s` field → absent. Table:

| StdKey | e1000e / igb (ethtool) | r8169 / r8152 (ethtool) | virtio_net | generic ethtool candidates | ip -s -s fallback |
|---|---|---|---|---|---|
| `rx_crc` | `rx_crc_errors` | — (folded into `rx_errors`) | — | `rx_crc_errors`, `rx_fcs_errors` | `rx.crc_errors` |
| `rx_frame` | `rx_frame_errors` | — | — | `rx_frame_errors` | `rx.frame_errors` |
| `rx_align` | `rx_align_errors` | `align_errors` | — | `rx_align_errors`, `align_errors` | — |
| `rx_symbol` | — | — | — | `rx_symbol_errors`, `symbol_errors` | — |
| `rx_missed` | `rx_missed_errors` | `rx_missed` | — | `rx_missed_errors`, `rx_missed` | `rx.missed_errors` |
| `rx_fifo` | `rx_fifo_errors` (igb), `rx_no_buffer_count` | — | — | `rx_fifo_errors`, `rx_over_errors`, `rx_no_buffer_count` | `rx.fifo_errors` |
| `rx_length` | `rx_length_errors` | — | — | `rx_length_errors` | `rx.length_errors` |
| `undersize` | `rx_short_length_errors` | — | — | `rx_undersize_packets`, `rx_short_length_errors` | — |
| `oversize` | `rx_long_length_errors` | — | — | `rx_oversize_packets`, `rx_long_length_errors` | — |
| `jabber` | — | — | — | `rx_jabbers`, `rx_jabber_errors`, `jabber` | — |
| `tx_carrier` | `tx_carrier_errors` | — | — | `tx_carrier_errors` | `tx.carrier_errors` |
| `phy_errors` | — | — | — | `phy_errors`, `rx_phy_errors` | — |
| `link_resets` | n/a | n/a | n/a | n/a | **sysfs** `carrier_changes` (documented exception) |

Notes baked into the design: r8169/r8152 expose almost nothing named per-cause via `ethtool -S` (`tx_packets,rx_packets,tx_errors,rx_errors,rx_missed,align_errors,tx_single_collisions,tx_multi_collisions,tx_aborted,tx_underrun,…`), so on Realtek most physical evidence comes from the `ip -s -s` fallback and `align_errors`. virtio exposes only per-queue counters → `Standard` nearly empty → evaluator correctly lands on "no physical-layer counters available" instead of "0 errors".

```go
type CounterCollector struct { R runner.Runner; IfName, Driver string; RawDir string }
func (c *CounterCollector) Snapshot(ctx context.Context) (model.CounterSnapshot, error) // ethtool -S may be UNAVAILABLE → Raw empty, IPStats still filled
func Delta(before, after model.CounterSnapshot) model.CounterDelta
// per key: {Delta uint64, OK bool}; after<before ⇒ OK=false ("reset/wrap; delta unreliable"), never negative math.
```

---

## 7. Interface discovery & virtual detection (`internal/network/discover.go`)

```go
func Discover(ctx context.Context, r runner.Runner, localIP netip.Addr) (Iface, error)
type Iface struct {
    Name string; Index, MTU int; Operstate string; MAC string
    PrefixLen int
    Class Class
}
type Class struct {
    Loopback, Virtual, Wireless, USB bool
    Driver string   // "" if no device symlink
    Reason string   // human-readable classification evidence
}
func Classify(name string) Class // pure sysfs, injectable root for tests (sysfs fixture tree under testdata is overkill; use a fs.FS/root-dir parameter)
```

- **Matching: exact address equality** of `localIP.String()` against `addr_info[].local` with `family=="inet"`. Prefix containment is deliberately NOT used for selection — an IP inside an interface's subnet but not assigned is a config error; guessing hides typos. However, on no-match, the error message computes containment purely as a *hint*: `"10.0.0.7 is not assigned to any interface (enp9s0 has 10.0.0.5/24 in the same subnet — did you mean that?)"`.
- Classification order (first hit wins for `Reason`):
  1. `link_type=="loopback"` or `LOOPBACK` flag → Loopback (rejected without `--allow-virtual-interface`; with the flag it is allowed — this powers the single-machine 127.0.0.1 demo).
  2. `link_type != "ether"` → Virtual (catches WireGuard/tun, which report `"none"` **[verified]**).
  3. `/sys/class/net/<if>/wireless` dir exists or uevent `DEVTYPE=wlan` → Wireless **[verified]** (rejected unless `--allow-virtual-interface`; a wifi NIC has a `device` symlink, so this check must precede #4-as-pass).
  4. `/sys/class/net/<if>/device` symlink **absent** → Virtual (veth, bridge, bond, vlan, tun, wg all lack it **[verified for wg]**).
  5. uevent `DEVTYPE` ∈ {bridge, vlan, bond, vxlan, wireguard, geneve, macvlan, macsec} → Virtual **[verified: `DEVTYPE=wireguard`]**.
  6. Name-prefix heuristic (belt-and-braces, matches digest): `veth`, `br-`, `docker`, `tun`, `tap`, `wg`, `virbr`, `vmnet`, `vnet`, `zt`, `tailscale` → Virtual.
- `Driver` = `filepath.Base(os.Readlink("/sys/class/net/<if>/device/driver"))` **[verified: → "r8169"]**; fallback `ethtool -i` parse (`driver: r8169`) if sysfs unreadable. `USB = strings.Contains(realpath(device), "/usb")` → recorded as host-limitation evidence for the evaluator (r8152 dongles).
- `--interface` override skips IP-based selection but still runs Classify + IP-ownership validation.

---

## 8. Link monitoring (`internal/network/monitor.go`)

Pure sysfs polling — zero exec cost, safe at 1 s (or faster) intervals:

```go
type LinkSnapshot struct {
    At             time.Time
    Operstate      string // sysfs lowercase: "up","down","unknown","lowerlayerdown","dormant"
    Carrier        int    // 1,0; -1 = unknown (EINVAL when admin-down)
    SpeedMbps      int    // -1 unknown (EINVAL or "-1" when no carrier — both observed)
    Duplex         string // "full","half","unknown" ("" on EINVAL)
    CarrierChanges uint64 // up+down transitions combined
    CarrierUp, CarrierDown uint64 // carrier_up_count / carrier_down_count [verified present]
}
func ReadLinkSnapshot(ifName string) LinkSnapshot // every field independently error-tolerant

type LinkEventType string // CarrierLost, CarrierRestored, SpeedChanged, DuplexChanged, OperstateChanged, Renegotiation
type LinkEvent struct { At time.Time; Type LinkEventType; Detail string; Before, After LinkSnapshot }

type Monitor struct{ /* ifName, interval, clk clock.Clock, ch chan LinkEvent, mu+last+history, dropped atomic.Uint64 */ }
func NewMonitor(ifName string, interval time.Duration, clk clock.Clock) *Monitor
func (m *Monitor) Run(ctx context.Context) error     // blocking loop; returns on ctx cancel
func (m *Monitor) Events() <-chan LinkEvent          // buffered 64; on full: drop + count (never blocks the poller)
func (m *Monitor) History() []LinkEvent              // full record for the report
func (m *Monitor) Current() LinkSnapshot
```

Key detection trick: **`Renegotiation` fires when `CarrierChanges` advanced by ≥2 between polls even if carrier/speed look identical** — catches sub-interval link flaps that per-field comparison misses (a full down/up cycle = +2). Speed/duplex changes with carrier stable = renegotiation-without-drop (also flagged). Reads use `os.ReadFile` with EINVAL tolerance (see Pitfalls). The `-1` speed sentinel from sysfs **[verified on NO-CARRIER port]** maps to unknown, never "speed changed to -1".

---

## 9. sudo handling

- Preflight probe: `sudo -n true` (5 s timeout). Exit 0 ⇒ passwordless sudo available; anything else (exit 1, stderr "a password is required", or sudo missing → `exec.ErrNotFound`) ⇒ unavailable. Never run sudo without `-n` anywhere (hard rule: no mid-test prompts).
- Privilege matrix: `ethtool <if>`, `ethtool -S`, `ip -j …`, `ping` (incl. 20 ms interval on modern iputils **[verified unprivileged]**), `iperf3` — **no root needed**. Root (CAP_NET_ADMIN) needed only for `ethtool --cable-test` / `--cable-test-tdr`.
- `runner.Privileged(spec CommandSpec) CommandSpec` helper in testsuite: if `euid==0` → unchanged; else if sudo probed OK and `--no-sudo` not set → prepend `["sudo","-n","--"]` (note: registry then tracks the sudo PID; group-kill via Setpgid still reaps the real ethtool because sudo shares the group). Else → the operation is marked UNAVAILABLE with reason `"requires root; passwordless sudo not available (or --no-sudo)"` at preflight — the run proceeds, cable-test section reports UNAVAILABLE, never FAILED.
- `--no-sudo`: probe skipped entirely, matrix collapses to "root ops unavailable unless euid==0".

---

## 10. Fixture inventory (`testdata/`)

Convention: stdout-only, exit-0 fixtures are `name.txt`; otherwise triplets `name.stdout` / `name.stderr` / `name.exit`. Loaded via `runnertest.FromFixture`.

**testdata/ethtool/**
| File | Content |
|---|---|
| `settings_e1000e_1g.txt` | Intel 82574L full `ethtool` output: 10/100/1000 supported+advertised, partner advertises same, `Speed: 1000Mb/s`, `Duplex: Full`, autoneg on, `Port: Twisted Pair`, MDI-X, `Link detected: yes` |
| `settings_r8169_2g5.txt` | RTL8125: modes incl `2500baseT/Full`, `Speed: 2500Mb/s`, partner advertises 2.5G |
| `settings_r8152_100m_half.txt` | USB dongle degraded: partner advertises only `100baseT/Half`, `Speed: 100Mb/s`, `Duplex: Half` (bad-cable scenario) |
| `settings_no_link.txt` | `Speed: Unknown!`, `Duplex: Unknown! (255)`, `Link partner advertised link modes: Not reported`, `Link detected: no` |
| `stats_e1000e_clean.txt` | `ethtool -S`, full e1000e counter list, all error counters 0 |
| `stats_e1000e_crc.txt` | same list with `rx_crc_errors: 1543`, `rx_align_errors: 12`, `rx_missed_errors: 4`, `rx_long_length_errors: 2` |
| `stats_r8169.txt` | Realtek naming: `align_errors`, `rx_missed`, `tx_aborted`, `rx_errors` nonzero |
| `stats_virtio.txt` | only `rx_queue_0_*`/`tx_queue_0_*` counters (no physical error names) |
| `cabletest_ok.txt` | `Cable test started/completed…` + 4× `Pair X code OK` |
| `cabletest_open_32m.stdout/.exit(0)` | Pairs A/B OK; `Pair C code Open Circuit` + `Pair C, fault length: 32.00m`; Pair D open at 32.40m |
| `cabletest_unsupported.stdout/.stderr/.exit(1)` | stderr `netlink error: Operation not supported` |
| `cabletest_noperm.stderr/.exit(1)` | stderr `netlink error: Operation not permitted` (drives sudo-retry path) |
| `cabletest_tdr.txt` | TDR header + per-pair distance/amplitude sample lines incl. negative amplitudes |
| `driverinfo_e1000e.txt` | `ethtool -i`: `driver: e1000e`, `bus-info: 0000:00:1f.6` (sysfs-fallback path) |

**testdata/ip/**
| File | Content |
|---|---|
| `addr_multi.json` | array: `lo` (loopback), `enp3s0` ether 10.0.0.1/24 UP, `wlan0` (DEVTYPE wlan sibling test), `docker0` bridge, `veth1a2b`, `wg0` link_type "none" — exercises discovery + every rejection branch |
| `addr_single_25g.json` | one RTL8125 iface, 192.168.50.2/24, altnames present |
| `addr_ip_not_found.json` | interfaces whose subnets contain—but don't own—the target IP (hint-message test) |
| `linkstats_clean.json` | `ip -j -s -s link show dev` single-element array, flat rx/tx detail fields all 0 (mirrors verified real shape incl. `tx.carrier_changes`) |
| `linkstats_errors.json` | `rx.crc_errors:1543`, `frame_errors:12`, `missed_errors:9`, `fifo_errors:3`, `tx.carrier_errors:2`, `collisions:7` |

**testdata/ping/**
| File | Content |
|---|---|
| `quick_500_loss_dup.txt` | 500-packet `-D` run: seqs 137–138 missing (gap), seq 201 has ` (DUP!)`, summary `500 packets transmitted, 498 received, +1 duplicates, 0.4% packet loss`, rtt line with mdev |
| `quick_clean_100.txt` | clean 100-packet run, tight RTTs (percentile math golden test) |
| `fullsize_ok.txt` | `-s 1472 -M do`: `1480 bytes from …` replies, 0% loss |
| `fullsize_emsgsize.stdout/.stderr/.exit(1)` | `ping: local error: message too long, mtu=1500` per probe, summary `+100 errors, 100% packet loss` |
| `interval_denied_old.stderr/.exit(2)` | `ping: cannot flood; minimal interval allowed for user is 200ms` (legacy iputils) |
| `interval_denied_new.stderr/.exit(2)` | `ping: cannot flood, minimal interval for user must be >= 2 ms, use -i 0.002 (or higher)` **[verbatim from live run]** |
| `unreachable.stdout/.exit(1)` | `From 10.0.0.1 icmp_seq=N Destination Host Unreachable` lines, 100% loss |
| `busybox_format.txt` | busybox `seq=`/`round-trip min/avg/max` shape → must yield `ErrUnsupportedPingFormat` |

**testdata/iperf/**
| File | Content |
|---|---|
| `tcp_39_fwd.json` | iperf 3.9 client, TCP 4×30 s: intervals, `end.sum_sent/sum_received`, retransmits 12, cpu_utilization_percent, congestion "cubic" |
| `tcp_316_fwd.json` | iperf 3.16 same test (version string + extra unknown fields; parser-ignores-unknowns test) |
| `tcp_collapse_retr.json` | interval series with one ~0 bps interval + retransmit burst (collapse/CoV analysis golden) |
| `udp_39_loss.json` | UDP `end.sum` with `jitter_ms:0.041`, `lost_packets:412`, `packets:170000`, `lost_percent:0.24`, `out_of_order:3` |
| `udp_316.json` | 3.16 UDP variant (no `out_of_order` → nil-pointer path) |
| `bidir_39.json` | `--bidir`: per-stream sender flags, `intervals[].sum`+`sum_bidir_reverse`, deliberately inconsistent top-level sums (locks in derive-from-streams behavior) |
| `client_error_refused.stdout/.exit(1)` | valid JSON `{"start":{...},"error":"unable to connect to server: Connection refused"}` |
| `server_listening.txt` | server banner block `Server listening on 5201` (readiness scanner) |
| `server_port_in_use.stderr/.exit(1)` | `iperf3: error - unable to start listener for connections: Address already in use` |
| `version_39.txt` / `version_316.txt` | `iperf3 --version` first-line variants |
| `help_with_bidir.txt` / `help_no_bidir.txt` | usage text with/without `--bidir` and `--one-off` (capability probe) |

---

## Pitfalls (things that break in practice)

1. **`cmd.WaitDelay` kills only the direct child, not the process group** — a sudo'd ethtool or forked helper survives. And without `WaitDelay`, `cmd.Wait` blocks forever if any grandchild inherited the stdout pipe. You need *both*: manual SIGTERM→SIGKILL to `-pgid` **and** `WaitDelay` as the pipe-unblock backstop.
2. **iperf3 stdout is block-buffered on pipes** — the `Server listening on` banner never arrives without `--forceflush`. Readiness detection silently degrades to the 1.5 s fallback if you forget it.
3. **Never TCP-probe an `iperf3 -1` server for readiness** — a connect that doesn't send the iperf cookie can consume/abort the one-off session; the real client then gets connection refused. Use the stdout banner.
4. **iperf3 bidir JSON (3.7–3.11) emits duplicate/misattributed `sum_sent`/`sum_received` keys**; Go's decoder keeps the last one silently. Always derive bidir per-direction totals from `end.streams[]` sender flags.
5. **iperf3 `-J` failures still print JSON** (`{"error": "..."}`, exit 1). Decode first, check `.error`; don't treat exit≠0 as "no output".
6. **`retransmits` is absent (not 0) for UDP and for receiver-side sums** — model as `*uint64`; rendering absent as 0 corrupts health evidence. Same for UDP `out_of_order`.
7. **sysfs reads return EINVAL, not empty**: `speed`/`duplex` when no carrier or on virtual devices, `carrier` when the interface is admin-down **[verified: `cat speed` → "Invalid argument"; also returns literal `-1` on NO-CARRIER ether]**. Every sysfs read must tolerate errors and the `-1` sentinel independently.
8. **The ping interval-denied message changed wording** between iputils generations (`"cannot flood; minimal interval allowed for user is 200ms"` vs `"cannot flood, minimal interval for user must be >= 2 ms, use -i 0.002 (or higher)"` **[verified]**). Match on the shared substrings `cannot flood` + `minimal interval`, exit code 2 — and note the `PING …` header still lands on stdout before the stderr error.
9. **Locale breaks every text parser**: iputils is built with NLS **[verified]** and translates messages. The runner must inject `LC_ALL=C`+`LANG=C` unconditionally; don't leave it to call sites.
10. **`ping` exit 1 means "some loss", not failure**; treating it as an error turns a lossy-cable measurement into a tooling error. Only exit 2 (+stderr) is an invocation problem.
11. **`ip -j -s -s` detail counters are flat inside `rx`/`tx`** (`rx.crc_errors`, `tx.carrier_changes`) — not a nested `rx_errors` object as the C source structure suggests. Structs above match verified live output. Also `operstate` is uppercase in `ip` JSON but lowercase in sysfs — normalize at parse time.
12. **Missing counter ≠ zero**: r8169/r8152 expose no `rx_crc_errors` via `ethtool -S`; virtio exposes no physical counters at all. `Standard` map must distinguish absent from 0 or the evaluator will certify a Realtek NIC as "0 CRC errors" it never measured.
13. **PID reuse on kill paths**: always re-verify `/proc/PID/stat` starttime + `/proc/PID/cmdline` before signaling registry/stale PIDs; ignore `ESRCH` from group kills (child may have exited between check and signal).
14. **Duplicate ICMP replies skew percentiles** — compute RTT stats over the first reply per seq; count DUPs as their own signal (on a two-host direct cable, DUPs are strong physical-layer evidence).
15. **ethtool `-S` names are driver-version dependent** (Intel added/renamed counters across kernel releases) — normalization must be candidate-list based, never a 1:1 map, and always preserve `Raw` for the report.
16. **`--cable-test` takes the link down** — the monitor will fire CarrierLost/Renegotiation during it; testsuite must suppress/annotate monitor events during the cable-test window or the evaluator double-counts self-inflicted link resets (`carrier_changes` jumps by ≥2).
