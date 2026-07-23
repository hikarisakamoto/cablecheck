# CableCheck control protocol

CableCheck protocol version `"1"` is a framed-JSON protocol over plain TCP. PC1 is the listener/coordinator and PC2 is the dialing worker. There's no negotiation in version 1: envelope versions must match exactly.

This document describes the wire contract implemented in `internal/protocol` and its lifecycle in `internal/peer`.

## Framing

Every message is encoded as:

```text
+--------------------------+----------------------------------+
| 4-byte big-endian uint32 | UTF-8 JSON envelope              |
| JSON-body length         | exactly the declared byte count  |
+--------------------------+----------------------------------+
```

The length excludes the four-byte prefix. `io.ReadFull` reads both prefix and body under a single absolute per-frame deadline.

| Constant | Value | Purpose |
|---|---:|---|
| `MinFrameSize` | 2 bytes | Smallest accepted body, `{}`. |
| `MaxFrameSize` | 1,048,576 bytes | Hard JSON-body cap for every message in both directions. |
| `DefaultWriteTimeout` | 10 s | Per-frame write deadline. |
| `DefaultIdleTimeout` | 20 s | Per-frame read deadline after handshake. |
| `HandshakeTimeout` | 30 s | Total handshake budget from accept/connect. |
| `HelloTimeout` | 10 s | PC1 accept-to-first-frame limit. |
| `HeartbeatInterval` | 5 s | Liveness ticker period. |

A declared length outside 2 bytes–1 MiB, or invalid envelope JSON, is fatal. The reader won't try to skip or resynchronize, since a partial header or body read leaves the stream position unknowable. After any read error, `protocol.Conn` is poisoned and later reads fail immediately. Oversized outbound envelopes are rejected rather than truncated.

Writes marshal the complete envelope, prepend the length, and issue one connection write while holding a mutex. At the session level, message-ID allocation and writing are also a single locked operation, which preserves wire order across the event loop and heartbeater.

TCP keepalive is a backup to application heartbeats: idle 5 seconds, interval 5 seconds, count 3.

## Envelope

```go
type Envelope struct {
    ProtocolVersion string          `json:"protocolVersion"`
    TestID          string          `json:"testId"`
    MessageID       string          `json:"messageId"`
    InReplyTo       string          `json:"inReplyTo,omitempty"`
    Type            MessageType     `json:"type"`
    Timestamp       time.Time       `json:"timestamp"`
    Payload         json.RawMessage `json:"payload,omitempty"`
}
```

- `protocolVersion` is the string `"1"`.
- `testId` is empty before `hello_ack`; PC1 then assigns an ID such as `ct-20260719-143205-a1b2c3d4`. Post-handshake frames normally carry it. The verifier tolerates an empty value but logs and drops a non-empty foreign value.
- `messageId` is unique per sender and monotonically increasing: `pc1-00000001`, `pc2-00000001`, and so on.
- `inReplyTo` correlates an acknowledgement, progress message, result, or completion with the message that triggered it, when the sender has such a message ID.
- `timestamp` is the sender's UTC time and marshals as RFC 3339 with fractional precision as needed. Countdown synchronization doesn't trust it.
- `payload` is decoded according to `type` into one of the fixed structs below.

Unknown JSON fields are tolerated for forward compatibility, and payloads are never decoded into arbitrary Go types. A duplicate detector remembers 128 recent IDs and the maximum sequence per role. During the strict handshake, a duplicate or non-increasing ID is a retryable protocol failure; after handshake it's logged and dropped.

Example opening envelope:

```json
{
  "protocolVersion": "1",
  "testId": "",
  "messageId": "pc2-00000001",
  "type": "hello",
  "timestamp": "2026-07-19T14:32:05Z",
  "payload": {
    "token": "example-token-1234",
    "role": "pc2",
    "cablecheckVersion": "1.0.0",
    "localIp": "192.168.50.2",
    "peerIp": "192.168.50.1"
  }
}
```

## Message catalog

| Message type | Direction | Payload | Purpose |
|---|---|---|---|
| `hello` | PC2 → PC1 | `Hello` | Opening authentication, role, build, and IP claims. |
| `hello_ack` | PC1 → PC2 | `HelloAck` | Accepts the opening and assigns the test ID. |
| `capabilities` | Both; PC2 first | `Capabilities` | Exchanges tool, system, NIC, privilege, and transfer capabilities. |
| `ready` | Both | `Ready` | Announces local operator/automatic readiness. |
| `start_confirmation` | PC1 → PC2 | `StartConfirmation` | Announces mode/steps and the arrival-relative countdown. |
| `test_request` | PC1 → PC2 | `TestRequest` | Requests one worker operation. |
| `test_progress` | PC2 → PC1 | `TestProgress` | Optional throttled progress for a request. |
| `test_result` | PC2 → PC1 | `TestResult` | Completes one request with status and typed result data. |
| `warning` | Both | `Warning` | Non-fatal advisory. |
| `abort` | Both | `Abort` | Terminates the session; no acknowledgement follows. |
| `heartbeat` | Both | `Heartbeat` | Reports liveness, current state, and active worker operation. |
| `report` | PC1 → PC2 | `ReportManifest` | Opens report transfer. |
| `report_chunk` | PC1 → PC2 | `ReportChunk` | Carries one base64-encoded file chunk. |
| `report_ack` | PC2 → PC1 | `ReportAck` | Accepts/rejects a file or declines the manifest. |
| `complete` | PC1 → PC2, then PC2 → PC1 | `Complete` | Exchanges the final verdict and ends normally. |

### Handshake payloads

`Hello`:

| JSON field | Type | Meaning |
|---|---|---|
| `token` | string | Shared token, plaintext on the trusted link; never logged. |
| `role` | string | Must be `pc2`. |
| `cablecheckVersion` | string | Sender build version. |
| `localIp` | string | PC2's configured local IP; must match PC1's expected peer IP. |
| `peerIp` | string | PC2's configured peer IP; must match PC1's local IP. |

`HelloAck` contains `testId` and PC1's `cablecheckVersion`.

`Capabilities` contains:

- `cablecheckVersion`, `os`, and `kernel` strings.
- `iperf3`: `version`, `json`, `reverse`, `bidir`, `getServerOutput`, `udp`, and `oneOff`.
- `ethtoolVersion` and `pingVariant` strings.
- `nic`: `name`, `driver`, `speedMbps`, `duplex`, `mtu`, `mac`, and `usb`.
- `sudoAvailable`, `cableTestSupported`, and `acceptReportTransfer` booleans.

`iperf3.json` is the one required peer capability. The other flags select fallbacks or optional behavior.

Example acknowledgement and capability message:

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc1-00000001",
  "inReplyTo": "pc2-00000001",
  "type": "hello_ack",
  "timestamp": "2026-07-19T14:32:05.010Z",
  "payload": {
    "testId": "ct-20260719-143205-a1b2c3d4",
    "cablecheckVersion": "1.0.0"
  }
}
```

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc2-00000002",
  "type": "capabilities",
  "timestamp": "2026-07-19T14:32:05.020Z",
  "payload": {
    "cablecheckVersion": "1.0.0",
    "os": "linux/amd64",
    "kernel": "6.12.0",
    "iperf3": {
      "version": "3.16",
      "json": true,
      "reverse": true,
      "bidir": true,
      "getServerOutput": true,
      "udp": true,
      "oneOff": true
    },
    "ethtoolVersion": "ethtool version 6.11",
    "pingVariant": "ping from iputils 20240117",
    "nic": {
      "name": "enp3s0",
      "driver": "e1000e",
      "speedMbps": 1000,
      "duplex": "full",
      "mtu": 1500,
      "mac": "02:00:00:00:00:02",
      "usb": false
    },
    "sudoAvailable": false,
    "cableTestSupported": false,
    "acceptReportTransfer": true
  }
}
```

### Readiness payloads

`Ready` has one boolean, `nonInteractive`, indicating whether readiness was automatic.

`StartConfirmation` contains:

| JSON field | Type | Meaning |
|---|---|---|
| `startAt` | RFC 3339 timestamp | Informational absolute time. |
| `startInMs` | integer | Authoritative delay from frame arrival. |
| `mode` | string | `quick`, `standard`, or `soak`. |
| `steps` | string array | Ordered progress display names. |

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc1-00000004",
  "type": "start_confirmation",
  "timestamp": "2026-07-19T14:33:00Z",
  "payload": {
    "startAt": "2026-07-19T14:33:03.5Z",
    "startInMs": 3500,
    "mode": "quick",
    "steps": ["link settings", "initial counter snapshot", "ping stability"]
  }
}
```

### Orchestration payloads

`TestRequest` contains:

- `op`: operation name.
- `params`: operation-specific JSON owned by `internal/testsuite`.
- `timeoutMs`: worker-side operation budget.
- `step` and `totalSteps`: 1-based plan display metadata copied from the
  coordinator's current plan step.
- `stepName`: optional fully formatted display label. This preserves dynamic
  labels such as standard repeats and soak cycle prefixes; workers fall back
  to `start_confirmation.steps[step-1]` when it is absent.

The worker emits a step line only when an accepted `test_request` arrives and
collapses multiple requests carrying identical step metadata. It never times
or advances the coordinator's plan independently. These additive fields need
no protocol version bump: older peers ignore unknown fields, while zero-valued
metadata produces no worker-side step announcement.

Implemented operation names include `counters_snapshot`, `link_settings`, `ping_run`, `ping_fullsize`, `iperf3_server_start`, `iperf3_server_stop`, `iperf3_client_run`, `iperf3_udp_run`, `iperf3_caps`, `cancel`, `cable_test_window_start`, and `cable_test_window_end`.

Operation parameter shapes are fixed: ping uses `peerIp` and `count`; server start uses `bindIp` and `port`; server stop uses `port`; TCP client uses `localIp`, `peerIp`, `port`, `durationSec`, `streams`, and `bidir`; UDP uses `localIp`, `peerIp`, `port`, `durationSec`, and `rateBps`; cable-window start uses `idleTimeoutMs`.

`TestProgress` contains `stage`, `percent` (`-1` means indeterminate), `text`, and an optional string-to-number `metrics` map. Progress is correlated through envelope `inReplyTo` and throttled to at most one update per second.

`TestResult` contains:

- `status`: `ok`, `failed`, `timeout`, `unavailable`, or `rejected`.
- optional typed `result` JSON.
- optional `error` text.
- `startedAt` and `finishedAt` timestamps.

The operation name selects a fixed result decoder: counter snapshot, link
settings, ping, TCP, UDP, server start/stop, or iperf capability result. The
cable-window end result is `{"selfInflictedCarrierEvents":N}`, handled by the
peer layer. A second non-cancel request while an operation is active gets
`rejected` immediately.

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc1-00000005",
  "type": "test_request",
  "timestamp": "2026-07-19T14:33:10Z",
  "payload": {
    "op": "ping_run",
    "params": {"peerIp": "192.168.50.1", "count": 500},
    "timeoutMs": 655000,
    "step": 0,
    "totalSteps": 0
  }
}
```

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc2-00000009",
  "inReplyTo": "pc1-00000005",
  "type": "test_result",
  "timestamp": "2026-07-19T14:33:20Z",
  "payload": {
    "status": "ok",
    "result": {
      "ping": {
        "direction": "",
        "target": "192.168.50.1",
        "transmitted": 500,
        "received": 500,
        "duplicates": 0,
        "sendErrors": 0,
        "icmpErrors": 0,
        "lossPercent": 0,
        "rttMinMs": 0.18,
        "rttAvgMs": 0.21,
        "rttMaxMs": 0.35,
        "rttMdevMs": 0.02,
        "longestSeqGap": 0,
        "longestGapMs": 0,
        "intervalUsedSec": 0.02,
        "exitCode": 0,
        "unparsedLines": 0
      }
    },
    "startedAt": "2026-07-19T14:33:10.010Z",
    "finishedAt": "2026-07-19T14:33:20.100Z"
  }
}
```

`Warning` contains `code`, human `text`, and `stage`.

`Abort` contains `reason`, `stage`, optional `detail`, and `initiator` (`pc1` or `pc2`). Defined reasons are `user_interrupt`, `auth_failed`, `version_mismatch`, `protocol_error`, `request_timeout`, `capability_missing`, and `internal_error`. On the `internal_error` (plan-failure) and handshake paths, `detail` carries a token-redacted elaboration so the receiver can see why. It's always empty for `auth_failed`, which avoids leaking token-guessing hints.

`Heartbeat` contains monotonic `seq`, session `state`, and optional `activeOp`.

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc2-00000010",
  "type": "heartbeat",
  "timestamp": "2026-07-19T14:33:25Z",
  "payload": {"seq": 4, "state": "testing", "activeOp": "iperf3_client_run"}
}
```

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc1-00000020",
  "type": "abort",
  "timestamp": "2026-07-19T14:35:00Z",
  "payload": {
    "reason": "user_interrupt",
    "stage": "iperf3_client_run",
    "initiator": "pc1"
  }
}
```

### Report and completion payloads

`ReportManifest` has `files` and `totalSize`. Each `ReportFile` has `name`, `size`, and lowercase hexadecimal `sha256`.

`ReportChunk` has file `name`, zero-based contiguous `seq`, byte `offset`, raw `data` (automatically base64-encoded by JSON), and `last`.

`ReportAck` has `name`, `ok`, optional `declined`, and optional `error`. An empty name plus `declined:true` declines the whole manifest.

Report acknowledgements are routed by the active transfer and file order; the
adapter leaves their envelope `inReplyTo` empty.

`Complete` has `classification`, one-line `summary`, and PC1's `exitCode`.

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc1-00000030",
  "type": "report",
  "timestamp": "2026-07-19T14:40:00Z",
  "payload": {
    "files": [
      {"name": "report.json", "size": 48120, "sha256": "8b91a8c1bfb143fa4c8072280b80b069dfda4a4603b3f098725f20b4c9b78e73"},
      {"name": "report.md", "size": 12904, "sha256": "6bd2a9c77431f765021dbfcd44bad1d6fb9b103ac29cf5f32b35b10ab8765290"},
      {"name": "summary.txt", "size": 720, "sha256": "9701cf5a064fb5e994ffb374b45b39246bf62f30589908302246e2911b605370"}
    ],
    "totalSize": 61744
  }
}
```

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc2-00000031",
  "type": "report_ack",
  "timestamp": "2026-07-19T14:40:00.5Z",
  "payload": {"name": "report.json", "ok": true}
}
```

```json
{
  "protocolVersion": "1",
  "testId": "ct-20260719-143205-a1b2c3d4",
  "messageId": "pc1-00000040",
  "type": "complete",
  "timestamp": "2026-07-19T14:40:01Z",
  "payload": {
    "classification": "EXCELLENT",
    "summary": "EXCELLENT (100/100): no adverse findings",
    "exitCode": 0
  }
}
```

## Handshake

PC1 listens on exactly `--local-ip:--control-port` and never binds a wildcard address. PC2 binds the connection's source to its own `--local-ip`, uses a 5-second attempt timeout, retries every 2 seconds, and gives up after 60 seconds total.

The handshake is synchronous and precedes the event-loop reader:

```text
PC2                                                    PC1
 |---- TCP connect ------------------------------------>| verify source IP == --peer-ip
 |---- hello {token, role, build, localIp, peerIp} ---->| <= 10 s after accept
 |                                                      | validate protocol/token/role/IPs
 |<--- hello_ack {testId, build} -----------------------|
 |---- capabilities ----------------------------------->|
 |<--- capabilities ------------------------------------|
 |                                                      | both enter waiting_for_local_start
```

PC1 validates PC2's claimed `peerIp` against PC1's local address and its `localIp` against PC1's configured peer. It compares the token by hashing both inputs with SHA-256 and running `subtle.ConstantTimeCompare`. The test ID comes from a UTC timestamp plus four random bytes. A foreign `testId` in the handshake capabilities exchange is a protocol error; after handshake, a frame with a non-empty foreign ID is logged and dropped.

### Handshake failures

The coordinator and worker map failed establishment differently. PC1 treats a connecting side that can't complete a valid handshake as peer/orchestration exit 5. PC2 treats its own connection or handshake failure as configuration/dependency exit 4.

| Failure | PC1 behavior | PC2 behavior / exit |
|---|---|---|
| Source IP differs from PC1 `--peer-ip` | Close silently, no response or strike; keep listening. | Connection closes; a CableCheck worker eventually exits 4 if it cannot establish. |
| First frame is not `hello`, malformed framing/JSON, or hello timeout | Best-effort `abort(protocol_error)`, close, retry once; second retryable failure ends PC1 with exit 5. | Opening side sees abort/EOF. A CableCheck worker's establishment failure maps to exit 4. |
| Hello role/IP claim, payload, message order, or assigned test ID is invalid | Best-effort `abort(protocol_error)`, close, retry once; second retryable failure ends PC1 with exit 5. | Sends or observes a protocol abort and exits 4. |
| Wrong token | Send `abort(auth_failed)` with no detail, close, and keep listening; the third auth strike ends PC1 with exit 5. | `ErrTokenRejected`; exit 4, with no automatic handshake retry. |
| Protocol version mismatch | Send `abort(version_mismatch)` with `ours=1 theirs=N`, close, and stop accepting; PC1 exits 5. | Reports mismatch and exits 4. |
| Required iperf3 JSON capability missing | Send `abort(capability_missing)` and stop; PC1 exits 5 if the connecting peer lacks it. | Exits 4 when either side's required capability is absent. |
| Handshake exceeds 30 seconds | Close and allow one retry; the second retryable failure ends PC1 with exit 5. | Post-connect handshake timeout exits 4. |

After success PC1 closes the listener. A process serves exactly one session.
The `cablecheckVersion` values are recorded but need not match; compatibility
is decided by the exact control-protocol version and the required capabilities.

## Readiness and synchronized start

After handshake, both sides enter `waiting_for_local_start`. Interactive commands are read by a stdin pump:

- `start` sends `ready`; duplicate starts are idempotent.
- `status` prints local state plus the most recent peer heartbeat.
- `quit` starts a local abort.
- stdin EOF is treated as `quit`.

Under `--non-interactive`, each side sends `ready` automatically. A peer's early `ready` is remembered while the local side still waits, but it doesn't bypass local confirmation.

Once PC1 knows both sides are ready, it sends `start_confirmation` with `startInMs:3500`. PC1 starts its own countdown from sending; PC2 starts from frame arrival. Each prints `3… 2… 1… GO` and transitions from `ready` to `testing` at the end. `startAt` is informational only, since the hosts' clocks may differ.

## RPC, heartbeats, and idle timeout

PC1's operation pattern is:

```text
test_request -> (test_progress | heartbeat)* -> test_result
```

`inReplyTo` correlates progress and results with the request ID. The worker applies `timeoutMs` through an operation context, and PC1 waits the same timeout plus 10 seconds of call grace. If no result arrives, even while heartbeats continue, PC1 emits `abort(request_timeout)` and ends the session. Late results without a pending call are logged and dropped.

Both sides tick every 5 seconds but send a heartbeat only when no successful frame write has occurred for at least 80% of that interval, which is 4 seconds at defaults. Ordinary traffic thus suppresses redundant heartbeats. A heartbeat carries the sender's state and active operation.

`ReadEnvelope` gets a fresh 20-second deadline for every inbound frame. A timeout, EOF, reset, malformed frame, or truncation is terminal; protocol v1 has no post-handshake reconnect. A connection loss becomes a `peer_lost` session outcome and exit 5, with PC1 preserving partial measurements when it can.

## Abort flow

For local Ctrl+C/SIGTERM, `quit`, or interactive stdin EOF, the session:

1. Cancels its context and active operation.
2. Waits for that operation to stop.
3. Sends a best-effort `abort` under a two-second farewell bound.
4. Closes the connection, failing pending RPCs and unblocking the reader.
5. Joins session goroutines and enters `aborted`.

The local side exits 6. PC1 attempts to write a partial report from completed measurements. PC2 always writes its local `diagnostic.json`, plus a failure summary unless a verified transferred report set already exists.

On a received `abort`, the peer prints the reason, stage, and `detail` when present, logs the same, folds `detail` into its returned error and `diagnostic.json`, cancels work, and exits 5. There's no abort acknowledgement and no wait for one. A raw TCP drop follows the peer-lost path and doesn't attempt a farewell on the dead connection.

## Report-transfer sub-protocol

Transfer runs PC1 → PC2 during `generating_report`, only when PC1 hasn't disabled it and PC2 advertised `acceptReportTransfer:true`.

| Limit | Value |
|---|---:|
| Allowed names | `report.json`, `report.md`, `summary.txt` only |
| Raw chunk size | 256 KiB |
| Per-file cap | 8 MiB |
| Total manifest cap | 16 MiB |
| Retry | One extra attempt per rejected file |

The self-contained `report.html` is a PC1-local projection and is deliberately
not part of protocol v1 report transfer.

The flow is:

1. PC1 hashes the existing allowed files and sends `report` with name, size, digest, and total size.
2. PC2 validates the exact allowlist, rejects `/`, `\`, or `..`, enforces both caps, and checks that its report directory is writable. It can decline the whole manifest with an empty-name `report_ack`.
3. PC1 streams one file at a time with contiguous zero-based `seq` values and
   byte offsets. PC2 reads `seq:0, offset:0` as the start or restart of an
   attempt and enforces the expected name, exact running offset, chunk cap,
   and declared file-size boundary.
4. PC2 writes `<name>.part`, hashes incrementally, and on `last:true` compares both the final byte count and the SHA-256. A match is fsynced, closed, renamed, and acknowledged `ok:true`.
5. An offset or digest failure deletes the partial file and sends `ok:false`. PC1 retries that entire file once. A second rejection ends the transfer callback, and the session emits `warning{code:"report_transfer_failed"}` and continues to `complete`.
6. Unexpected names, oversize data, local filesystem failure, or other non-retryable faults decline the manifest so the sender stops cleanly. No unverified partial file is kept.
7. PC1 sends `complete`; PC2 answers with its own `complete`, and both enter `completed`.

Acknowledgements are per file, not per chunk, and `raw/` artifacts are never part of the manifest. A transfer failure is advisory: it never changes the health verdict or its exit code.

## Cable-test window coordination

The opt-in ethtool cable diagnostic can drop the same link that carries the control channel. CableCheck appends it after the normal plan, so the ordinary before/after counters exclude the disruptive window.

PC1 first warns the peer and calls `SetIdleTimeout(4m)` locally. It then sends a normal `test_request` with op `cable_test_window_start` and `idleTimeoutMs:240000`. Before acknowledging, PC2 updates its active read deadline to four minutes, marks its monitor window active, and starts labeling observed carrier transitions as self-inflicted.

After the acknowledgement, PC1 marks its own monitor window active and runs `ethtool --cable-test` plus the independently requested TDR command. Each command has a 90-second execution timeout. PC1 then requests `cable_test_window_end`. Before returning that result, PC2 closes its annotation window, reports its local self-inflicted carrier count, and restores the normal idle timeout. PC1 records both peers' counts, closes its own window, and restores its normal timeout.

Self-inflicted monitoring events carry `selfInflicted:true`, and their carrier counts are subtracted from physical carrier evidence. Recovering the peer after a locally completed diagnostic is best-effort: if it fails, the local cable-test result is kept with a limitation note.

## Security model

- The protocol is for a trusted direct cable or trusted isolated LAN. It provides no TLS, encryption, forward secrecy, or protection from an on-path observer.
- The supported participants are two known computers. The token prevents accidental peer mismatch; it is not intended to establish trust between unknown machines.
- The token is plaintext on the wire. To compare, both strings are SHA-256 hashed and the fixed-size digests are compared with `subtle.ConstantTimeCompare`, which avoids content- and length-dependent timing.
- Tokens and raw hello payloads are never logged, and no report model field can contain the token.
- PC1 binds only the configured local IP and accepts only the configured peer source IP.
- Frames are size-capped before allocation; report names and bytes are constrained separately.
- Envelope types, payload structs, operation names, parameter decoders, and result decoders are all fixed. There's no reflection-based arbitrary deserialization and no remote shell command string.
- Unknown JSON fields and unknown message types are tolerated for forward compatibility, but they don't grant new operations.

Authentication isn't confidentiality. Anyone who can observe the trusted link can read the token and results, so don't expose the control port on an untrusted network.

Within that trust model, tested correctness, usability, compatibility, and maintainability take
priority over optional hostile-network hardening. The bounded-frame, token non-persistence,
fixed-operation, and verified-transfer rules remain correctness and local-safety invariants.
