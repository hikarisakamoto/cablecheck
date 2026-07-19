# CableCheck report schema

CableCheck writes its machine-readable result to `report.json`. The current
schema version is `1.0.0`.

For a concrete document, see
[`examples/healthy/report.json`](../examples/healthy/report.json). The other
committed example reports show reduced-speed, CRC-error, and host-limited
outcomes.

## Conventions

- JSON field names are stable lower camel case unless a nested counter is
  explicitly documented as snake case.
- Timestamps are Go `time.Time` values encoded as RFC 3339/RFC 3339Nano JSON
  strings.
- Optional fields marked `omitempty` may be absent. Absence is not the same as
  a measured zero.
- A duration is an object with both an integer millisecond value and a display
  string:

  ```json
  {"ms": 30000, "text": "30s"}
  ```

- A bitrate uses the same dual machine/display representation:

  ```json
  {"bps": 800000000, "text": "800M"}
  ```

- `score` is an integer from 0 through 100 for conclusive results and `null`
  for `INCONCLUSIVE`.
- SHA-256 values are lowercase hexadecimal strings.

The decoder also accepts legacy numeric-millisecond and Go-duration-string
forms for a duration. CableCheck itself emits the object form above.

## Top-level `Report`

| JSON field | Type | Meaning |
|---|---|---|
| `schemaVersion` | string | Report schema version; currently `1.0.0`. |
| `toolVersion` | string | CableCheck version that produced the report. |
| `protocolVersion` | string | Control-protocol version used by the peers. |
| `testId` | string | Identifier assigned to this two-peer session. |
| `startedAt` | timestamp | Session start time. |
| `finishedAt` | timestamp | Session finish time. |
| `duration` | Duration | Overall elapsed session duration. |
| `configuration` | ConfigEcho | Effective, resolved configuration. The shared token is never included. |
| `pc1` | PeerReport | PC1 host, OS, NIC, and tool information. |
| `pc2` | PeerReport | PC2 host, OS, NIC, and tool information. |
| `tests` | TestsSection | Parsed ping, TCP, UDP, bidirectional, and optional cable-test results. |
| `initialCounters` | PeerCounters | Initial counter snapshots for PC1 and PC2 when available. |
| `finalCounters` | PeerCounters | Final counter snapshots for PC1 and PC2 when available. |
| `cycleCounters` | array of PeerCounters, optional | Soak-mode pre-load snapshots, one for each completed cycle when collected. |
| `counterDeltas` | PeerCounterDeltas | Final-minus-initial counter deltas, each carrying its own reliability bit. |
| `monitoringEvents` | array of MonitoringEvent | Link-monitor observations during the run. |
| `warnings` | array of string | Operational and parsing warnings. |
| `skippedTests` | array of SkippedTest | Tests that were deliberately skipped and why. |
| `udpRateAssumed` | boolean, optional | True when the link speed was unknown and the default UDP rate basis was used. |
| `classification` | string | `EXCELLENT`, `GOOD`, `WARNING`, `POOR`, `FAILED`, or `INCONCLUSIVE`. |
| `score` | integer or null | Banded 0–100 health score, or null for an inconclusive result. |
| `classificationReasons` | array of string | Finding texts that explain the classification. |
| `recommendations` | array of string | Suggested follow-up actions. |
| `partial` | boolean | True when the test did not complete normally. |
| `soakCyclesCompleted` | integer, conditional | Present for every soak report, including zero completed cycles; absent in other modes. |
| `failure` | FailureDetails, optional | Stage and error for a partial or failed orchestration. |
| `findings` | array of Finding, optional | Rule-engine findings with IDs, categories, severities, evidence, and host-sensitivity. |
| `rawFiles` | array of RawFileRef, optional | Raw-artifact names, hashes, sizes, and descriptions. |
| `link` | LinkSection, optional | Before/after link settings for both peers. |
| `machines` | MachinePair, optional | Paired PC1/PC2 machine metadata used by current reports. |

The report does not contain the authentication token. Although the evaluator
has an internal rules-version constant, `Report` has no `rulesVersion` JSON
field in schema 1.0.0.

## Configuration

`configuration` is a `ConfigEcho` containing the effective values after mode
defaults have been resolved.

| JSON field | Type | Meaning |
|---|---|---|
| `role` | string | `pc1` or `pc2`. |
| `localIp`, `peerIp` | string | Session IPv4 addresses. |
| `interface` | string, optional | Selected network interface. |
| `mode` | string | `quick`, `standard`, or `soak`. |
| `controlPort`, `iperfPort` | integer | Control and base iperf3 ports. |
| `tcpDuration`, `udpDuration` | Duration | Per-phase load durations. |
| `udpRate` | Bitrate | Explicit configured UDP override. A zero bitrate means automatic runtime derivation; each UDP result's `targetBps` records the rate actually used. |
| `parallelStreams` | integer | TCP parallel-stream count. |
| `pingCount` | integer | Standard-ping count selected by the mode. |
| `pingInterval` | Duration | Requested standard-ping interval. |
| `tcpRepeats` | integer | Per-direction TCP repeat count selected by the mode. |
| `soakDuration` | Duration, optional | Overall soak budget. |
| `soakLoad` | string, optional | `periodic` or `continuous`. |
| `monitorInterval` | Duration | Sysfs link-monitor interval. |
| `cableTest`, `cableTestTdr` | boolean | Requested cable diagnostics. TDR implies the base cable test. |
| `output` | string | Output base directory. |
| `verbose`, `nonInteractive`, `noSudo`, `noReportTransfer`, `allowVirtualInterface` | boolean | Effective CLI switches. |
| `tokenGenerated` | boolean | Whether PC1 generated the shared token; never the token value itself. |

## Peer and link metadata

`pc1` and `pc2` are `PeerReport` objects:

| JSON field | Type | Meaning |
|---|---|---|
| `hostname` | string | Host name. |
| `kernel` | string | Kernel release. |
| `os` | string | OS description. |
| `nic` | NICReport | Interface identity and negotiated properties. |
| `toolVersions` | object of string values | Detected external-tool versions. |

`NICReport` contains `name`, `driver`, `speedMbps`, `duplex`, `mtu`, `mac`, and
`usb`. A negative `speedMbps` means that speed could not be determined.
`MachinePair` has `pc1` and `pc2` fields containing the same `PeerReport`
shape.

`link` is a `LinkSection` with `pc1` and `pc2` endpoints. Each `LinkEndpoint`
has optional `before` and `after` `LinkSettings` objects.
`LinkSettings` contains:

- `speedMbps`, `duplex`, `linkDetected`, `autoNeg`, and `port`;
- `supportedPorts`, `supportedModes`, `advertisedModes`, and `partnerModes`;
- `mdix`; and
- `raw`, an optional string-to-string map of retained ethtool fields.

## Test results

`tests` is a `TestsSection` with these fields. Array fields are omitted when
empty; pointer fields are omitted when not run.

Directional ping, TCP, and UDP results use `pc1_to_pc2` or `pc2_to_pc1` in
their `direction` field.

| JSON field | Type | Meaning |
|---|---|---|
| `ping` | array of PingResult | Standard-payload ping results. |
| `fullSizePing` | array of PingResult | Full-size-payload ping results. |
| `tcp` | array of TCPResult | Unidirectional TCP phases. |
| `udp` | array of UDPResult | Unidirectional UDP phases. |
| `bidirectional` | BidirResult, optional | Simultaneous or two-phase bidirectional TCP result. |
| `cableTest` | CableTestResult, optional | ethtool cable-test/TDR result. |

### PingResult

`PingResult` contains:

- `direction` and `target`;
- `transmitted`, `received`, `duplicates`, `sendErrors`, and `icmpErrors`;
- `lossPercent`;
- `rttMinMs`, `rttAvgMs`, `rttMaxMs`, and `rttMdevMs`;
- `percentiles`, an object whose percentile numbers are string keys;
- `spikes`, an array of `{ "seq": N, "rttMs": N }` objects;
- `missingSeqRuns`, an array of `{ "firstSeq": N, "len": N }` objects;
- `longestSeqGap` and `longestGapMs`;
- `intervalUsedSec`, the interval actually accepted by the installed ping;
- `exitCode`; and
- `unparsedLines`, the count of parser input lines that were not recognized.

### TCPResult

`TCPResult` contains `direction`, optional `incomplete`, `duration`,
`parallelStreams`, `senderBitsPerSecond`, `receiverBitsPerSecond`, optional
`retransmissions`, `intervalResults`, `throughputVariation`,
`minimumIntervalBps`, `maximumIntervalBps`, `cpuUtilization`, and `warnings`.

Each `TCPInterval` contains `startSec`, `endSec`, `bytes`, `bitsPerSecond`, and
optional `retransmits`. `CPUUsage` contains `hostTotal`, `hostUser`,
`hostSystem`, `remoteTotal`, `remoteUser`, and `remoteSystem`.

### UDPResult

`UDPResult` contains `direction`, `targetBps`, `actualSenderBps`,
`actualReceiverBps`, `lostPackets`, `totalPackets`, `lossPercent`, `jitterMs`,
optional `outOfOrder`, and `cpu`.

### BidirResult

`BidirResult` contains `duration`, `parallelStreams`, `pc1ToPc2`,
`pc2ToPc1`, `cpuUtilization`, `twoPhaseFallback`, and `warnings`. Each
`BidirDirection` contains `senderBitsPerSecond`, `receiverBitsPerSecond`, and
optional `retransmissions`.

### CableTestResult

`CableTestResult` contains:

- `available` and `unavailableReason`;
- `pairs`, whose entries contain `pair`, `status`, `rawCode`, `faultMeters`,
  and `hasFault`;
- `samples`, whose entries contain `pair`, `distanceM`, and `amplitude`;
- `tdrUnavailableReason`;
- `selfInflictedCarrierEvents`, with `pc1` and `pc2` counts; and
- `unparsedLines`.

Pair statuses are `OK`, `OPEN`, `SHORT_INTRA`, `SHORT_INTER`, `IMPEDANCE`, or
`UNSPECIFIED`.

## Counters

`initialCounters` and `finalCounters` are `PeerCounters` objects with optional
`pc1` and `pc2` `CounterSnapshot` values. A snapshot contains:

| JSON field | Type | Meaning |
|---|---|---|
| `capturedAt` | timestamp | Snapshot time. |
| `standard` | object of unsigned integers | Flat normalized counters with stable names such as `rx_crc`, `rx_missed`, and `link_resets`. |
| `driver` | object of unsigned integers | Driver-specific ethtool counters, retaining their driver names. |
| `raw` | string | Raw ethtool counter output. |
| `ipStats` | IPStats64 | Full `stats64` receive/transmit data from `ip -j -s -s link show`. |

`ipStats` has `rx` and `tx` objects. RX uses these iproute2 snake-case names:

```text
bytes, packets, errors, dropped, over_errors, multicast, length_errors,
crc_errors, frame_errors, fifo_errors, missed_errors, nohandler
```

TX uses:

```text
bytes, packets, errors, dropped, carrier_errors, collisions, aborted_errors,
fifo_errors, window_errors, heartbeat_errors, carrier_changes
```

These typed RX/TX objects are separate from the flat normalized `standard`
map and the untouched driver-specific `driver` map.

`counterDeltas` is a `PeerCounterDeltas` with optional `pc1` and `pc2`
`CounterDeltaSet` values. Each set is one flat object keyed by normalized
counter name. A `CounterDelta` is:

```json
{
  "delta": 42,
  "ok": true
}
```

`ok: true` means the delta was reliable. `ok: false` means that counter went
backwards because of a reset or wrap; its `delta` is then zero and must not be
used as evidence. A key missing from either capture is omitted rather than
represented as zero, and the report carries a counter-reliability warning.

In soak mode, `cycleCounters` contains the pre-load snapshots retained for
completed cycles. It is not a replacement for the overall initial/final
snapshots and deltas.

## Monitoring, failure, and raw-file references

A `MonitoringEvent` contains `at`, `type`, `detail`, and `selfInflicted`.
Events caused by the coordinated cable-test window are annotated with
`selfInflicted: true` so they are not treated as ordinary link instability.

A `SkippedTest` contains `name` and `reason`. `FailureDetails` contains
`stage` and `error`.

A `RawFileRef` contains `name`, `sha256`, `bytes`, and `description`. It is a
reference to an artifact under the report's `raw/` directory; raw artifacts
are not embedded in `report.json`.

## Classification block

The result block is spread across these top-level fields:

- `classification` and nullable `score` give the final outcome;
- `classificationReasons` contains the human-readable texts of the findings;
- `recommendations` contains follow-up advice; and
- `findings` preserves the structured rule results.

Each `Finding` contains:

| JSON field | Values or type | Meaning |
|---|---|---|
| `ruleId` | string | Stable rule ID such as `PHY-02` or `PERF-01`. |
| `category` | `physical`, `transport`, `performance`, `host`, `limitation` | Evidence family. |
| `severity` | `info`, `warning`, `poor`, `failed`, `marker` | Rule result before classification folding. |
| `text` | string | Human-readable explanation. |
| `evidence` | array of string, optional | Concrete observations that triggered the rule. |
| `hostSensitive` | boolean | Whether host limits can plausibly explain the finding. |

The complete rule definitions and score bands are in
[`health-rules.md`](health-rules.md).

## Forward compatibility

Schema 1.x follows these rules:

1. Existing JSON field names and meanings remain stable.
2. New optional fields may be added. Consumers should ignore unknown fields.
3. An omitted optional field means unavailable/not applicable; it must not be
   silently interpreted as a measured zero.
4. Consumers should inspect `schemaVersion` before interpreting a report.
   CableCheck's `report` command accepts the current major schema and rejects
   a different major version.
5. Existing enum meanings are part of the stable contract. A change that old
   1.x readers cannot safely interpret requires a schema-major change rather
   than silently redefining an existing value.

Use `cablecheck report path/to/report.json` to render a compatible JSON report
into `report.md` and `summary.txt` without rerunning any tests.
