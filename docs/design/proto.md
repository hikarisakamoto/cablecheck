# CableCheck — Design: `internal/protocol` and `internal/peer`

Protocol version constant: `protocol.Version = 1` (int). Exact match required; no negotiation in v1. The numbers below are decided constants.

---

## 1. Framing (`internal/protocol/frame.go`)

Wire format: `[4-byte big-endian uint32 length][length bytes of UTF-8 JSON envelope]`. Length counts the JSON body only, not the prefix.

**Constants**

```go
const (
    Version             = 1
    MaxFrameSize        = 1 << 20          // 1 MiB hard cap, both directions, all message types
    MinFrameSize        = 2                // "{}"
    DefaultWriteTimeout = 10 * time.Second // per frame
    DefaultIdleTimeout  = 20 * time.Second // read side; see heartbeats §6
    HandshakeTimeout    = 30 * time.Second // whole handshake budget
    HelloTimeout        = 10 * time.Second // accept → first frame
)
```

Report chunks fit inside `MaxFrameSize` by construction (§8). There's **no negotiated larger cap**: one limit everywhere means one code path and no desync risk.

**Conn wrapper**

```go
type Conn struct {
    nc          net.Conn
    br          *bufio.Reader     // 64 KiB; ONLY reader of nc after construction
    wmu         sync.Mutex        // serializes writes from event loop, heartbeater, executor
    maxFrame    uint32
    idleTimeout atomic.Int64      // nanos; mutable for cable-test link-loss windows (P4)
    writeTO     time.Duration
    lastSend    atomic.Int64      // unix nanos of last successful write (heartbeat gap check)
}

func NewConn(nc net.Conn) *Conn
func (c *Conn) ReadEnvelope() (*Envelope, error)          // blocking; enforces idle timeout
func (c *Conn) WriteEnvelope(env *Envelope) error          // thread-safe; enforces write timeout
func (c *Conn) SetIdleTimeout(d time.Duration)
func (c *Conn) LastSend() time.Time
func (c *Conn) Close() error                                // idempotent (sync.Once inside)
```

**Read contract** (never assume 1 read = 1 message):

```go
func (c *Conn) ReadEnvelope() (*Envelope, error) {
    deadline := time.Now().Add(c.getIdleTimeout())
    c.nc.SetReadDeadline(deadline)              // absolute; set fresh per FRAME, not per byte
    var hdr [4]byte
    if _, err := io.ReadFull(c.br, hdr[:]); err != nil { return nil, err }
    n := binary.BigEndian.Uint32(hdr[:])
    if n < MinFrameSize || n > c.maxFrame {
        return nil, &FrameError{Len: n}         // FATAL: caller must close conn — stream is unsyncable
    }
    buf := make([]byte, n)
    if _, err := io.ReadFull(c.br, buf); err != nil { return nil, err } // body under same deadline: a stalled body times out
    var env Envelope
    if err := json.Unmarshal(buf, &env); err != nil { return nil, &FrameError{...} } // fatal too
    return &env, nil
}
```

- **Oversized/zero frame**: fatal. Don't try to discard-and-resync; a bogus length means the stream is corrupt or hostile. Return `FrameError`, caller closes the connection and transitions to `failed`/`aborted`.
- Unknown JSON fields in the envelope are tolerated for forward compat: plain `json.Unmarshal`, no `DisallowUnknownFields` at envelope level.

**Write contract**:

```go
func (c *Conn) WriteEnvelope(env *Envelope) error {
    body, err := json.Marshal(env); ...
    if len(body) > int(c.maxFrame) { return &FrameError{...} } // caller bug; never truncate
    buf := make([]byte, 4+len(body))
    binary.BigEndian.PutUint32(buf, uint32(len(body)))
    copy(buf[4:], body)
    c.wmu.Lock(); defer c.wmu.Unlock()
    c.nc.SetWriteDeadline(time.Now().Add(c.writeTO))  // per-write, deadlines are absolute
    _, err = c.nc.Write(buf)                          // single Write; net.Conn completes or errors
    if err == nil { c.lastSend.Store(time.Now().UnixNano()) }
    return err
}
```

Header+body go out in **one buffer, one `Write`**. No bufio.Writer means no flush bugs, and nothing interleaves under the mutex. Any write error is fatal to the session. Enable TCP keepalive as backup liveness: `tcpConn.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 5s, Interval: 5s, Count: 3})` (Go 1.23+).

---

## 2. Message catalog (`internal/protocol/messages.go`)

**Envelope** (spec fields + `inReplyTo` for RPC correlation):

```go
type Envelope struct {
    ProtocolVersion int             `json:"protocolVersion"`
    TestID          string          `json:"testId"`              // "" until hello_ack assigns it
    MessageID       string          `json:"messageId"`           // "<role>-<8-digit seq>", e.g. "pc2-00000042"
    InReplyTo       string          `json:"inReplyTo,omitempty"` // messageId being answered
    Type            MessageType     `json:"type"`
    Timestamp       time.Time       `json:"timestamp"`           // time.Now().UTC(); RFC3339Nano
    Payload         json.RawMessage `json:"payload,omitempty"`
}

type MessageType string
const (
    TypeHello        MessageType = "hello"
    TypeHelloAck     MessageType = "hello_ack"
    TypeCapabilities MessageType = "capabilities"
    TypeReady        MessageType = "ready"
    TypeStartConfirm MessageType = "start_confirmation"
    TypeTestRequest  MessageType = "test_request"
    TypeTestProgress MessageType = "test_progress"
    TypeTestResult   MessageType = "test_result"
    TypeWarning      MessageType = "warning"
    TypeAbort        MessageType = "abort"
    TypeHeartbeat    MessageType = "heartbeat"
    TypeReport       MessageType = "report"        // transfer manifest
    TypeReportChunk  MessageType = "report_chunk"  // added
    TypeReportAck    MessageType = "report_ack"    // added
    TypeComplete     MessageType = "complete"
)

func NewEnvelope(t MessageType, testID, msgID string, payload any) (*Envelope, error)
func DecodePayload[T any](env *Envelope) (*T, error) // errors on nil/absent payload; DisallowUnknownFields OFF
```

MessageIDs are role-prefixed monotonic counters (atomic uint64 per session): cheap, sortable in logs, and enough for duplicate detection (§6). Payload decoding always goes **into these explicit structs**, never `map[string]any` or arbitrary types.

**Payload structs** (all in `protocol`, all with json tags):

```go
type Hello struct { // PC2 → PC1
    Token             string `json:"token"`             // plaintext on the wire; trusted-link-only, documented
    Role              string `json:"role"`              // must be "pc2"
    CablecheckVersion string `json:"cablecheckVersion"`
    LocalIP           string `json:"localIp"`           // PC2's --local-ip (sanity)
    PeerIP            string `json:"peerIp"`            // PC2's --peer-ip; must equal PC1's local IP
}

type HelloAck struct { // PC1 → PC2
    TestID            string `json:"testId"`            // "ct-20260715-143205-a1b2c3d4"
    CablecheckVersion string `json:"cablecheckVersion"`
}

type Iperf3Caps struct {
    Version         string `json:"version"`
    JSON            bool   `json:"json"`
    Reverse         bool   `json:"reverse"`
    Bidir           bool   `json:"bidir"`
    GetServerOutput bool   `json:"getServerOutput"`
    UDP             bool   `json:"udp"`
    OneOff          bool   `json:"oneOff"` // supports -1
}
type NICInfo struct {
    Name string `json:"name"`; Driver string `json:"driver"`
    SpeedMbps int `json:"speedMbps"`; Duplex string `json:"duplex"`
    MTU int `json:"mtu"`; MAC string `json:"mac"`
}
type Capabilities struct { // both directions, PC2 first
    CablecheckVersion    string     `json:"cablecheckVersion"`
    OS                   string     `json:"os"`     // "linux/amd64"
    Kernel               string     `json:"kernel"` // uname -r
    Iperf3               Iperf3Caps `json:"iperf3"`
    EthtoolVersion       string     `json:"ethtoolVersion"`
    PingVariant          string     `json:"pingVariant"` // "iputils-20240117" etc.
    NIC                  NICInfo    `json:"nic"`
    SudoAvailable        bool       `json:"sudoAvailable"`
    CableTestSupported   bool       `json:"cableTestSupported"`
    AcceptReportTransfer bool       `json:"acceptReportTransfer"` // false when --no-report-transfer
}

type Ready struct { NonInteractive bool `json:"nonInteractive"` }

type StartConfirmation struct { // PC1 → PC2
    StartAt   time.Time `json:"startAt"`   // absolute, for the report only
    StartInMs int       `json:"startInMs"` // authoritative: countdown anchor = frame ARRIVAL + StartInMs (immune to clock skew)
    Mode      string    `json:"mode"`      // quick|standard|soak
    Steps     []string  `json:"steps"`     // display names, drives "[1/8]" numbering on PC2
}

type TestRequest struct { // PC1 → PC2
    Op         string          `json:"op"`     // e.g. "iperf3_server_start","iperf3_client_run","ping_run","counters_snapshot","iperf3_server_stop","cancel"
    Params     json.RawMessage `json:"params"` // op-specific struct owned by internal/testsuite
    TimeoutMs  int             `json:"timeoutMs"`  // worker-side budget (coordinator waits this + grace)
    Step       int             `json:"step"`
    TotalSteps int             `json:"totalSteps"`
}

type TestProgress struct { // PC2 → PC1; InReplyTo = request messageId
    Stage   string             `json:"stage"`
    Percent float64            `json:"percent"` // -1 = indeterminate
    Text    string             `json:"text"`
    Metrics map[string]float64 `json:"metrics,omitempty"` // e.g. {"bitrateMbps": 941.2}
}

type TestResult struct { // PC2 → PC1; InReplyTo = request messageId
    Status     string          `json:"status"` // "ok"|"failed"|"timeout"|"unavailable"|"rejected"
    Result     json.RawMessage `json:"result,omitempty"` // typed per-op model struct (internal/model)
    Error      string          `json:"error,omitempty"`
    StartedAt  time.Time       `json:"startedAt"`
    FinishedAt time.Time       `json:"finishedAt"`
}

type Warning struct { Code string `json:"code"`; Text string `json:"text"`; Stage string `json:"stage"` }

type Abort struct {
    Reason    string `json:"reason"` // "user_interrupt","auth_failed","version_mismatch","protocol_error","request_timeout","capability_missing","internal_error"
    Stage     string `json:"stage"`  // state or op name at abort time
    Detail    string `json:"detail,omitempty"` // token-redacted; populated on internal_error/handshake, empty for auth_failed
    Initiator string `json:"initiator"` // "pc1"|"pc2"
}

type Heartbeat struct {
    Seq      uint64 `json:"seq"`
    State    string `json:"state"`              // sender's state-machine state (feeds `status` command)
    ActiveOp string `json:"activeOp,omitempty"`
}

type ReportFile struct { Name string `json:"name"`; Size int64 `json:"size"`; SHA256 string `json:"sha256"` } // hex
type ReportManifest struct { Files []ReportFile `json:"files"`; TotalSize int64 `json:"totalSize"` }          // type "report"
type ReportChunk struct {
    Name   string `json:"name"`
    Seq    int    `json:"seq"`    // 0-based per file, must be contiguous
    Offset int64  `json:"offset"` // must equal bytes received so far
    Data   []byte `json:"data"`   // encoding/json base64s automatically; ≤ ChunkSize raw
    Last   bool   `json:"last"`
}
type ReportAck struct { // per FILE (and one for the manifest when declining)
    Name string `json:"name"`; OK bool `json:"ok"`; Declined bool `json:"declined,omitempty"`; Error string `json:"error,omitempty"`
}

type Complete struct {
    Classification string `json:"classification"` // EXCELLENT..INCONCLUSIVE
    Summary        string `json:"summary"`        // one-line verdict
    ExitCode       int    `json:"exitCode"`
}
```

---

## 3. Handshake

PC1 binds `net.Listen("tcp", net.JoinHostPort(localIP, port))` on **only** the supplied local IP, never `0.0.0.0`. PC2 dials with a 5 s per-attempt timeout, retrying every 2 s for up to 60 s total (covers "PC1 started second").

Sequence (all within `HandshakeTimeout` = 30 s, measured from accept/connect):

```
PC2                                   PC1
 |--- TCP connect ------------------->| accept; verify RemoteAddr IP == --peer-ip
 |--- hello {token,role,ips,ver} ---->| (must arrive ≤ 10s after accept)
 |                                    | validate: type, protocolVersion, token, role, peerIP==my localIP
 |<-- hello_ack {testId} -------------| testId assigned here; ALL later envelopes carry it
 |--- capabilities ------------------>|
 |<-- capabilities -------------------| PC1 checks required caps (iperf3 JSON) 
 |                                    | both → waiting_for_local_start
```

**Token check** (constant-time, length-safe):

```go
func TokenEqual(a, b string) bool {
    ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
    return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
```

Hash first because `subtle.ConstantTimeCompare` short-circuits (returns 0) on unequal lengths, which leaks token length. Never log the token; log its SHA-256 prefix (8 hex) for correlation.

**TestID**: `"ct-" + time.Now().UTC().Format("20060102-150405") + "-" + hex(4 random bytes)` from `crypto/rand`. After `hello_ack`, any envelope with a non-matching, non-empty `testId` is a protocol error → `abort(protocol_error)`.

**Failure handling** (exact behavior):

| Failure | PC1 action | PC2 action | Exit |
|---|---|---|---|
| Connecting IP ≠ `--peer-ip` | Close conn immediately, **no response**, log warn, keep listening | n/a | — |
| First frame not `hello` / frame error / hello timeout | `abort(protocol_error)` best-effort, close, keep listening | sees abort or EOF → exit | PC2: 5 |
| `protocolVersion` mismatch | `abort(version_mismatch, detail="ours=1 theirs=N")`, close, **exit** | print both versions, exit | 4 both |
| Wrong token | `abort(auth_failed)` (no detail), close; keep listening; after **3** auth failures exit | print "token rejected", exit | PC1: 5 after 3; PC2: 4 |
| Required capability missing (no iperf3 JSON) | `abort(capability_missing, detail)`, exit | exit | 4 both |
| Handshake exceeds 30 s | close, keep listening (once), then exit | retry window applies pre-connect only; post-connect stall → exit | 5 |

After a successful handshake PC1 **closes the listener**: exactly one session per process, no second-connection ambiguity.

---

## 4. State machine (`internal/peer/state.go`)

```go
type State string
const (
    StateInitializing         State = "initializing"
    StatePreflight            State = "preflight"
    StateListening            State = "listening"   // PC1 only
    StateConnecting           State = "connecting"  // PC2 only
    StateHandshake            State = "handshake"
    StateWaitingForLocalStart State = "waiting_for_local_start"
    StateWaitingForPeerStart  State = "waiting_for_peer_start"
    StateReady                State = "ready"
    StateTesting              State = "testing"
    StateGeneratingReport     State = "generating_report"
    StateCompleted            State = "completed"
    StateAborted              State = "aborted"
    StateFailed               State = "failed"
)

var validTransitions = map[State][]State{
    StateInitializing:         {StatePreflight, StateFailed},
    StatePreflight:            {StateListening, StateConnecting, StateFailed, StateAborted},
    StateListening:            {StateHandshake, StateAborted, StateFailed},
    StateConnecting:           {StateHandshake, StateAborted, StateFailed},
    StateHandshake:            {StateWaitingForLocalStart, StateAborted, StateFailed},
    StateWaitingForLocalStart: {StateWaitingForPeerStart, StateReady, StateAborted, StateFailed}, // → Ready directly when peer's ready already arrived
    StateWaitingForPeerStart:  {StateReady, StateAborted, StateFailed},
    StateReady:                {StateTesting, StateAborted, StateFailed},
    StateTesting:              {StateGeneratingReport, StateAborted, StateFailed},
    StateGeneratingReport:     {StateCompleted, StateAborted, StateFailed},
    StateCompleted: {}, StateAborted: {}, StateFailed: {},
}

type StateMachine struct {
    mu    sync.Mutex
    cur   State
    onChange func(from, to State) // logging hook; called under lock, must not re-enter
}
func NewStateMachine(initial State, onChange func(from, to State)) *StateMachine
func (m *StateMachine) Current() State
func (m *StateMachine) Transition(to State) error       // ErrInvalidTransition{From,To} if not allowed
func (m *StateMachine) Require(states ...State) error   // ErrWrongState{Cur, Want} — op guards
```

Unit-testable in isolation: a table-driven test walks every `(from,to)` pair asserting allowed/denied. **Invalid operation rejection**: every inbound-message handler and stdin command starts with `Require(...)`. A network-triggered violation (say `test_request` before `ready`) sends `warning{code:"invalid_state"}` and drops the frame; 3 in a row triggers `abort(protocol_error)`. Stdin violations just print (`"cannot start: still in handshake"`). A peer's `ready` arriving while we're in `waiting_for_local_start` does **not** transition; it sets a `peerReady bool` flag in the session, keeping the transition table honest.

---

## 5. Readiness sync + stdin integration

**Stdin** (interactive mode only; skipped entirely under `--non-interactive`):

```go
func stdinLoop(ctx context.Context, events chan<- event) {
    sc := bufio.NewScanner(os.Stdin) // default 64 KiB line cap is fine
    for sc.Scan() {
        select {
        case events <- evStdin{line: strings.TrimSpace(strings.ToLower(sc.Text()))}:
        case <-ctx.Done(): return
        }
    }
    select { case events <- evStdinEOF{}: case <-ctx.Done(): } // EOF/err → treated as "quit"
}
```

stdin reads aren't portably interruptible. Design around it, in order:
1. On shutdown, call `os.Stdin.SetReadDeadline(time.Now())`. This works on Linux ttys and pipes (os.File deadline support), unblocking the pending `Read` with `os.ErrDeadlineExceeded` so the goroutine exits cleanly.
2. If `SetReadDeadline` returns `os.ErrNoDeadline` (stdin is a regular file), the goroutine is orphaned. It can't block the session, since its only sends select on `ctx.Done()`; it's excluded from the session WaitGroup, and process exit reaps it. This is the one sanctioned "leak".
3. `evStdinEOF` maps to `quit`, which prevents an infinite hang when someone runs interactive mode with `< /dev/null`.

Commands: `start` → if `Require(waiting_for_local_start)` ok: send `ready`, transition (`→ ready` if `peerReady`, else `→ waiting_for_peer_start`); duplicate `start` is idempotent (prints "already ready"). `status` → print own state, peer's last-heartbeat state, and time since last frame. `quit` → abort flow (§7). Anything else → one help line.

**Synchronized start**: once PC1 knows both readies (its own `ready` plus the peer's), it sends `start_confirmation{StartAt: now+3500ms, StartInMs: 3500, Mode, Steps}` and both sides transition `ready → testing` at countdown end. **Countdown anchor = local receipt time (`StartInMs`)**, not `StartAt`. Wall clocks on two air-gapped PCs aren't trusted, and one-way latency on a direct cable is sub-ms, so both countdowns align within display precision. Each side prints `3… 2… 1… GO` on 1 s ticks from `Clock.After`. `StartAt` exists only to be recorded in the report. Under `--non-interactive`, `ready` is sent automatically on entering `waiting_for_local_start`.

---

## 6. Orchestration model (coordinator RPC)

PC1 drives the whole suite as a sequence of RPCs; the pattern per step is
`test_request → (test_progress | heartbeat)* → test_result`, correlated by `InReplyTo == request.MessageID`.

```go
// internal/peer — what internal/testsuite programs against on PC1
type RemoteCaller interface {
    // Call blocks until test_result, timeout, or session death.
    // onProgress may be nil; called from the event loop goroutine — must not block.
    Call(ctx context.Context, op string, params any, timeout time.Duration,
         onProgress func(protocol.TestProgress)) (*protocol.TestResult, error)
    Warn(code, text string)
}
```

Internally, `Call` allocates a messageID, registers `pending[msgID] = &call{done: make(chan *protocol.TestResult, 1), onProgress: ...}`, writes the frame, and waits on `done`/`ctx`/timer. The pending map is owned by the event loop; registration could go through a command channel or a mutex, and with only two goroutines a mutex wins.

**Timeout budget**: `request.TimeoutMs` = expected op duration + 20 s. The worker enforces it via the op ctx and reports `status:"timeout"`, which is a *result*, not a protocol failure. The coordinator waits `TimeoutMs + 10s` grace. If even that expires, the worker hasn't reported at all despite heartbeats (a wedged executor), so the coordinator sends `abort(request_timeout, stage=op)` and fails the session. A worker that heartbeats but can't answer isn't trustworthy for the remaining steps. Partial results are preserved (§7).

**Duplicate protection**: messageIDs are `"<role>-%08d"`; the receiver keeps `maxSeq[role]` plus a ring of the last 128 seen IDs. A duplicate or non-increasing ID gets logged as a warning and dropped. TCP already guarantees ordering, but the check catches application bugs and costs nothing.

**Unknown message types**: log at `slog.Warn` with type + messageId, ignore, continue. Same for a `test_result` whose `InReplyTo` matches no pending call, which happens when a late result loses the timeout-abort race: log and drop.

**Heartbeats**: **both** sides run a heartbeat goroutine from handshake completion until session end. The ticker fires at `HeartbeatInterval = 5s`; on each tick, if `time.Since(conn.LastSend()) >= 4s`, it sends `heartbeat{seq++, state, activeOp}`. Any recent frame counts as liveness, so busy periods don't double-send. The read side enforces `IdleTimeout = 20s` (≈4 missed intervals) via the per-frame read deadline in §1. Heartbeats are valid in every post-handshake state, including `generating_report`: report generation on PC1 can take seconds, and transfer keeps the line busy anyway.

**Disconnect detection**: the reader goroutine returns an error (EOF, RST, `FrameError`, deadline) and emits `evConnErr`. If the state is `testing` or `generating_report`, the event loop treats it as a peer-lost abort: preserve partials, write the partial report, transition `→ aborted`, exit 5. If `completed` was already reached, it's benign and ignored.

---

## 7. Abort flow

**Local Ctrl+C / `quit`**: the app uses `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`, so cancellation reaches the session event loop as `<-ctx.Done()`. Sequence, strictly ordered inside the loop:
1. Cancel any active op ctx (kills child processes via CommandRunner; stops iperf3 servers).
2. Best-effort `abort{reason:"user_interrupt", stage: current state or activeOp, initiator: role}`. The frame write already has a 10 s deadline, but the farewell uses a shorter dedicated 2 s deadline. A write failure is logged and never blocks shutdown.
3. `conn.Close()` (unblocks reader), wait WaitGroup, unblock stdin (§5).
4. Transition `→ aborted`; write partial report (whatever results exist, marked `"aborted": true`); exit 6.

**Peer-initiated abort** (received `abort` frame): cancel active ops, print `"peer aborted: <reason> at <stage>"`, transition `→ aborted`, write own partial report from local data, exit 5. There's no abort-ack message: after sending abort each side closes, and the closing side doesn't wait for a reply.

**TCP drop mid-test** (no abort frame): identical to peer abort but with reason `peer_lost`. Each side independently preserves partials and produces a partial report. PC2 also stops any iperf3 server it was hosting. Neither side retries the connection in v1. Reconnect/recover logic is scoped only to the P4 cable-test feature, which pre-arranges a link-loss window by calling `conn.SetIdleTimeout(recoveryWindow)` on both sides **before** the link goes down.

---

## 8. Report transfer sub-protocol

Direction PC1 → PC2, only in `generating_report`, only if **both** `!cfg.NoReportTransfer` and peer's `Capabilities.AcceptReportTransfer`.

**Numbers**: `ChunkSize = 256 KiB` raw (base64 → ~342 KiB, envelope ≤ ~350 KiB ≪ 1 MiB frame cap). `MaxFileSize = 8 MiB` per file, `MaxTotalSize = 16 MiB`. A soak-mode report.json with interval series stays well under that, and `raw/` is never transferred. Names use the exact allowlist `{"report.json","report.md","summary.txt"}`; PC2 rejects anything else, and independently rejects any name containing `/`, `\`, or `..` (path traversal).

Flow, per session:
1. PC1 → `report` manifest (names, sizes, sha256). PC2 validates names/caps; if it can't accept (disk, caps exceeded) replies `report_ack{Name:"", Declined:true, Error}` → PC1 logs warning, skips to `complete`.
2. Per file, in manifest order: PC1 streams chunks `seq=0..n` (`Offset` must equal bytes-received; any gap/mismatch → `report_ack{OK:false}`); PC2 writes to `<name>.part` in its report dir, feeding an incremental `sha256.New()`.
3. On `Last:true`: PC2 compares digest to manifest; match → rename `.part` → final name, send `report_ack{Name, OK:true}`; mismatch → delete `.part`, `report_ack{OK:false, Error:"sha256 mismatch"}`.
4. On a failed ack PC1 retries that file **once**; a second failure sends `warning{code:"report_transfer_failed"}` and continues. Transfer failure never changes health classification or exit code, since PC2 already has its own locally-generated summary of its side.
5. PC1 → `complete{classification, summary, exitCode-class}`; PC2 replies `complete` (its own view); PC1 closes conn; both → `completed`.

Acking is **per file, not per chunk**. TCP provides flow control, and the per-frame write deadline (10 s) bounds a stalled receiver. 8 MiB at even 10 Mbps is ~7 s/file worst case per chunk budget, which is fine.

---

## 9. Concurrency architecture

Event loop shape is mandatory; implementers must not invent alternate topologies.

```go
type event interface{ isEvent() }
type evFrame     struct{ env *protocol.Envelope }         // from reader
type evConnErr   struct{ err error }                       // from reader (terminal)
type evStdin     struct{ line string }                     // from stdin loop
type evStdinEOF  struct{}
type evOpDone    struct{ reqID string; res protocol.TestResult; resultPayload any } // PC2 executor
type evOpProgress struct{ reqID string; p protocol.TestProgress }                   // PC2 executor
```

`events := make(chan event, 64)`. **Every** producer send selects on `ctx.Done()` so no producer can block after the loop exits.

**Goroutine inventory** (owner is `Session.Run`), with ownership tree and stop mechanism:

| # | Goroutine | Role | Started | Stopped by | In WaitGroup |
|---|---|---|---|---|---|
| 0 | Session event loop | both | `Run` itself (not spawned) | ctx cancel / terminal state | n/a |
| 1 | Reader: `for { env := conn.ReadEnvelope(); events <- evFrame{env} }` | both | post-handshake (handshake reads happen synchronously in `Run` with explicit deadlines, pre-loop) | **`conn.Close()`** — a blocked `Read` ignores ctx; the loop must close the conn *before* `wg.Wait()` | yes |
| 2 | Heartbeater: ticker 5 s, `defer ticker.Stop()` | both | post-handshake | `ctx.Done()` | yes |
| 3 | Stdin loop | both, interactive only | after preflight | `os.Stdin.SetReadDeadline(now)`; else orphaned-harmless (§5) | **no** |
| 4 | Plan driver: runs `PlanFunc(ctx, remoteCaller)` — the test sequence, including PC1's local ops | PC1 | on `testing` entry | ctx cancel; every pending `Call` chan is closed by the loop on teardown so `Call` returns `ErrSessionClosed` | yes |
| 5 | Op executor: **at most one** — spawned per accepted `test_request`, runs `OpHandler.HandleOp` with per-op ctx (`context.WithTimeout(sessionCtx, TimeoutMs)`) | PC2 | per request | op ctx cancel (session abort, or `op:"cancel"` request) | yes (tracked via `activeOp` handle) |

A second `test_request` while one is active (except `op:"cancel"`) gets an immediate `test_result{Status:"rejected"}`. The coordinator never legitimately overlaps requests, so this only fires on bugs.

**Writes**: `Conn.WriteEnvelope` is mutex-guarded. The sanctioned writers are the event loop, the heartbeater, and (PC2) the executor's progress path. Executor progress and results route through `events` (`evOpProgress`/`evOpDone`) so the **loop** performs those writes and the executor itself never touches the conn. So the net writers are loop and heartbeater only, which keeps `pending`/state mutation single-threaded.

**Shutdown order** (one function, one order, always): cancel session ctx → cancel/wait active op → farewell abort frame if appropriate (2 s deadline) → `conn.Close()` → `wg.Wait()` → unblock stdin → final state transition → report writing. `go test -race` plus a goroutine-count assertion in integration tests (before/after `Run`, tolerating the one documented stdin orphan) enforces this.

**Session surface** (`internal/peer/session.go`):

```go
type Clock interface { Now() time.Time; After(d time.Duration) <-chan time.Time }

type Config struct {
    Role             Role          // RolePC1 | RolePC2
    LocalIP, PeerIP  netip.Addr
    ControlPort      uint16
    Token            string
    NonInteractive   bool
    NoReportTransfer bool
    Version          string
    Caps             protocol.Capabilities // from preflight
    Clock            Clock
    Logger           *slog.Logger
    Stdin            io.Reader             // os.Stdin in prod; injectable for tests
    Transport        Transport             // see below; nil = real TCP
}

type Transport interface { // testability seam (net.Pipe in unit tests)
    Listen(ctx context.Context, addr string) (net.Listener, error)
    Dial(ctx context.Context, addr string) (net.Conn, error)
}

type PlanFunc func(ctx context.Context, rc RemoteCaller) error // PC1's suite driver (internal/testsuite)
type OpHandler interface {                                     // PC2's op executor (internal/testsuite)
    HandleOp(ctx context.Context, op string, params json.RawMessage,
             progress func(protocol.TestProgress)) (result any, status string, err error)
}

type Outcome struct {
    FinalState     State
    PeerComplete   *protocol.Complete // nil if never received
    AbortReason    string
    PeerCaps       protocol.Capabilities
    TestID         string
}

func Run(ctx context.Context, cfg Config, plan PlanFunc, ops OpHandler) (Outcome, error)
```

PC1 passes both `plan` and `ops`, though it runs its own local steps directly inside `plan`, so its `ops` is unused/nil. PC2 passes `ops` only: `plan == nil` means worker mode, where the loop services `test_request`s until `complete`/`abort`.

---

## 10. PC2 concurrent execution & PC1 aggregation

**PC2**: the executor goroutine runs exactly one op via `OpHandler` (which wraps `internal/runner.CommandRunner`). Progress callbacks are throttled inside the executor to ≥ 1/s before emitting `evOpProgress`. The heartbeater is independent of the executor, so a silent 60 s iperf3 run still shows liveness (`heartbeat.activeOp = "iperf3_client_run"`). Long-lived server ops split into paired requests: `iperf3_server_start` returns `ok` only after the worker confirms the server is listening (it polls the port), and `iperf3_server_stop` collects server-side JSON if `--get-server-output` was unavailable. The worker keeps a per-testID process registry so `stop`, `cancel`, and abort teardown target only CableCheck-owned PIDs.

**PC1 aggregation**: the plan driver owns a `model.SessionResults` accumulator. Every direction-sensitive result is stored under an explicit direction key `"pc1_to_pc2"` / `"pc2_to_pc1"`. Local steps append directly; remote steps decode `TestResult.Result` into the same `internal/model` structs (`DecodePayload[model.PingResult]` etc.). The op name picks the concrete type via a fixed `map[op]decoder` table, never reflection on arbitrary input. Counter snapshots are requested from both sides at the same plan points (`counters_snapshot` op remote, direct call local) so deltas are computed per side with aligned boundaries. `TestResult.Status == "unavailable"` propagates into the model as UNAVAILABLE, and the evaluator, not the peer layer, decides what that means. Raw command output **never** crosses the control channel: each side writes its own `raw/` dir, and PC2's raw artifacts stay on PC2 (referenced in its local summary).

---

## Pitfalls

1. **`subtle.ConstantTimeCompare` length short-circuit**: comparing raw token bytes leaks token length via timing and returns 0 instantly on length mismatch. Always compare SHA-256 digests (§3).
2. **Blocked `net.Conn.Read` ignores context**: the reader goroutine can only be freed by `conn.Close()`. Closing must happen *before* `wg.Wait()` or shutdown deadlocks. Forgetting `ticker.Stop()` is the same class of bug.
3. **Never read the raw conn after wrapping in `bufio.Reader`**: buffered bytes vanish and the stream desyncs. All reads (including handshake) go through `Conn.ReadEnvelope`.
4. **Deadlines are absolute**: `SetWriteDeadline`/`SetReadDeadline` must be re-armed per frame. Setting them once at startup silently disables timeouts after the first period.
5. **Clock skew on synchronized start**: anchoring the countdown to the absolute `startAt` timestamp breaks on machines with unsynced clocks, common on bench PCs. Anchor to frame arrival + `StartInMs`.
6. **base64 expansion vs frame cap**: chunk data grows ×4/3 plus envelope overhead. 256 KiB raw is safe under 1 MiB; anyone "optimizing" chunk size above ~700 KiB will produce frames the peer fatally rejects.
7. **Partial reads/writes**: one `Read` never equals one message (Nagle, GRO, scheduling). Use `io.ReadFull` for header and body, and a single buffered `Write` per frame under a mutex so heartbeats can't interleave into a chunk frame.
8. **stdin is not interruptible portably**: a naive `wg.Add(1)` around the stdin goroutine hangs shutdown forever when the user never presses Enter. Use `os.Stdin.SetReadDeadline(time.Now())` (works for ttys/pipes on Linux), exclude it from the WaitGroup, and treat EOF as `quit` to survive `< /dev/null` without `--non-interactive`.
9. **Late `test_result` after RPC timeout**: the coordinator may abort a call whose result arrives one frame later. The loop must tolerate `InReplyTo` with no pending entry (log+drop), or it will misroute/panic.
10. **Farewell abort on a dead conn**: after Ctrl+C following a peer crash, the abort write blocks until the write deadline. Use the short 2 s farewell deadline and ignore its error, or Ctrl+C appears to "hang".
11. **Path traversal in report transfer**: PC2 must allowlist exact filenames. Writing `manifest.Name` verbatim lets a malicious or buggy PC1 write outside the report dir.
12. **Read deadline must cover the frame body, not just the header**: extending the deadline after reading the header lets a stalled mid-body peer wedge the reader for another full idle period. Keep one deadline per frame.
13. **Heartbeat storms during chunk streaming**: the heartbeater must check `LastSend()` before sending, or it queues redundant frames behind large chunk writes and inflates write latency measurements in verbose logs.
14. **Envelope `testId` on early frames**: `hello` and PC1's first `abort` legitimately carry `testId:""`, so validators must special-case pre-`hello_ack` frames or the handshake rejects itself.
