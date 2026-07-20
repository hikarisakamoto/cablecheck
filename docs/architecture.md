# CableCheck architecture

CableCheck is one Linux binary with two runtime roles. PC1 is the coordinator: it listens, authenticates PC2, drives the test plan, evaluates the combined evidence, writes the authoritative report, and optionally transfers it. PC2 is the worker: it connects, executes one requested operation at a time, and receives the final verdict and report set.

The module is `cablecheck`, targets Go 1.24, uses only the standard library, and delegates Linux measurements to external tools through an injected command runner.

## Package map

- `internal/model` — leaf data model for reports, results, counters, bitrates, durations, findings, and classifications; imports only the standard library.
- `internal/clock` — injectable `Now`, `After`, and ticker abstraction; the production implementation is the sanctioned wall-clock source.
- `internal/clock/clocktest` — manually advanced `FakeClock` with waiter synchronization for deterministic timer tests.
- `internal/logging` — human stderr and JSON debug logging with token/payload redaction.
- `internal/config` — parses the raw run intent into validated, mode-resolved configuration; owns presets, bounds, token generation, and UDP-rate defaults.
- `internal/protocol` — framed JSON, envelopes, the fixed message catalog, message-ID/deduplication support, deadlines, keepalive, and constant-time token comparison.
- `internal/peer` — TCP transport, handshake, state machine, readiness sync, event loop, RPC correlation, abort handling, and report-transfer routing.
- `internal/runner` — shell-free process execution, bounded capture, timeouts, process-group shutdown, and stale-process registry.
- `internal/runner/runnertest` — scriptable `FakeRunner` and `FakeProcess` test doubles.
- `internal/parser` — pure parsers for `ip`, iputils `ping`, `iperf3`, `ethtool`, cable-test, and TDR output.
- `internal/network` — interface discovery/classification, sysfs link monitoring, USB/virtual detection, and port probes.
- `internal/testsuite` — link/counter/ping/iperf/cable operations, the worker op vocabulary, and quick/standard/soak coordinator plans.
- `internal/evaluate` — pure fact extraction, ordered health rules, classification fold, score, and recommendations.
- `internal/reporting` — report directory/raw-artifact management, JSON/Markdown/text rendering, and verified report-file transfer; imports only `internal/model` plus the standard library.
- `internal/app` — top-level orchestration: preflight, suite construction, monitoring, peer session, report assembly/evaluation/rendering, transfer callbacks, doctor/report commands, and exit mapping.
- `internal/cli` — subcommand dispatch, Go `flag` parsing, help/progress output, and final error-to-exit-code mapping.
- `internal/testutil` — shared hermetic test helpers for scripted stdin, dribbled reads, deadline-aware waits, and goroutine leak checks.
- `cmd/cablecheck` — process entry point, signal-aware context, build metadata, and `os.Exit(cli.Run(...))`.

## Dependency direction

Dependencies point upward from small, pure foundations toward orchestration:

```text
                         cmd/cablecheck
                               |
                              cli
                               |
                              app
       +----------+------------+-------------+-----------+
       |          |            |             |           |
     config      peer       testsuite      evaluate   reporting
       |        /   \       /   |   \          |          |
       |     clock protocol parser runner      |          |
       |                    \   /              |          |
       |                    network             |          |
       +-----------------------+----------------+----------+
                               |
                             model
```

This diagram is conceptual rather than a complete import graph, but its constraints are concrete:

- `model` is the leaf for domain data and has no internal dependencies.
- Parsers and evaluation are pure transformations over bytes or model values.
- `reporting` may import only `model`; a dependency-purity test enforces this. Offline regeneration therefore cannot accidentally rerun tests or evaluation.
- `peer` knows the protocol and session mechanics but not the health rules or external-tool details.
- `testsuite` implements operations and plans over runner/parser/peer seams.
- `app` is the composition root. It is the layer allowed to know configuration, networking, test plans, evaluation, reporting, logging, and exit policy.

## Run lifecycle

`cli` resolves a `config.RunConfig` and creates `app.App`. On PC1, `App.Start` binds the control listener before returning. The background run then performs local preflight, creates a report directory, attaches logging, builds the mode-specific suite, and enters `peer.Run`.

The peer session moves through these states:

```text
initializing -> preflight -> listening (PC1) / connecting (PC2)
             -> handshake -> waiting_for_local_start
             -> waiting_for_peer_start -> ready -> testing
             -> generating_report -> completed

Any nonterminal state can end in aborted or failed.
```

On PC1, entering `testing` starts the plan driver and the application-owned sysfs monitor. The plan performs local operations directly and remote operations through `RemoteCaller.Call`. On success, PC1 freezes monitoring data, assembles and evaluates the report, optionally transfers the rendered files, exchanges `complete`, and maps the classification to an exit code. On abnormal termination, completed measurements are retained in a partial report when possible.

PC2 runs the same preflight and peer machinery but supplies an `OpHandler` instead of a plan. It executes one operation at a time until report/complete or abort, and on every exit writes a local `diagnostic.json` — its role, test ID, mode, IPs, final state, any error, the reason and detail of a peer abort, PC1's verdict, and an index of its raw files. The diagnostic is not a `model.Report` (PC2 runs no evaluation) and is never transferred; it makes a failed run debuggable from PC2 alone.

## The session event loop

After the synchronous handshake, one event loop owns all session state: readiness flags, countdown, peer heartbeat state, invalid-state strikes, transfer routing, and the terminal finish specification. Producers never mutate those fields. They send typed values through a 64-entry `events` channel:

```text
reader ------------ evFrame / evConnErr --------+
stdin ------------- evStdin / evStdinEOF -------+
op executor -------- evOpProgress / evOpDone ----+--> session event loop
plan driver -------- evPlanDone -----------------+        |
RPC timer ---------- evCallExpired --------------+        +--> state transitions
transfer callback -- evTransferDone -------------+        +--> protocol responses
```

Every producer send selects on the session context, so a producer cannot remain blocked after teardown. Network reads are the exception to context cancellation: a blocked `net.Conn` read is released by closing the connection.

Inbound envelopes first pass message-ID duplicate/order checks, exact protocol-version checks, and assigned-test-ID checks. The loop then dispatches by message type. State-inappropriate frames receive `warning{code:"invalid_state"}`; three such frames abort the session. Unknown message types and late unmatched results are logged and dropped.

### State ownership and the wire-write rule

There are two related invariants:

1. The event loop is the single writer of loop-owned session state. The executor, stdin pump, reader, plan, and transfer callback communicate only through events or narrow synchronized RPC channels.
2. Every post-handshake frame uses `session.mintAndWrite`, which holds `writeMu` across message-ID allocation and the physical write. This guarantees that monotonically increasing IDs reach the wire in the same order.

The event loop performs ordinary response writes. Other sanctioned initiators
are the heartbeater, the plan driver's synchronous RPC calls, the report
callback's narrow `ReportChannel`, and the bounded farewell helper. None can
write the TCP connection directly: all converge on the same serialized
`mintAndWrite` path. Executor progress/results are routed back through the
event loop, so the executor never touches the connection. `protocol.Conn`
also has its own write mutex so a 4-byte prefix and JSON body can never
interleave with another frame.

## Goroutine inventory and shutdown

| Goroutine | Where and when | Stop mechanism | Joined? |
|---|---|---|---|
| Session event loop | Both roles; runs in `peer.Run` after handshake | A terminal `finishSpec` or parent cancellation leads to ordered shutdown | It is the owning goroutine |
| Connection reader | Both roles; post-handshake | `conn.Close()` unblocks `ReadEnvelope`; sends one terminal `evConnErr` unless context already ended | Yes |
| Heartbeater | Both roles; post-handshake | Session context cancellation; ticker is stopped on return | Yes |
| Stdin pump | Interactive runs only | Context stops event sends; an `*os.File` read deadline unblocks supported Linux tty/pipe reads | No; a regular-file reader without deadline support is the one sanctioned harmless orphan until process exit |
| Plan driver | PC1, at countdown `GO` | Session context cancellation; pending calls are closed during teardown | Yes |
| Op executor | PC2, one per accepted request and at most one active | Per-op timeout/cancel; session abort cancels and explicitly waits for the active op | Yes |
| Report-transfer callback | When sending or receiving a manifest | Session context cancellation or callback completion, reported as `evTransferDone` | Yes |

Short-lived helpers also exist around this core: PC1 uses a listener watcher during blocking `Accept`; a farewell-abort helper is bounded by two seconds; soak mode uses a budget watcher inside the plan; and `internal/app` owns a joinable sysfs monitor during `testing`.

The session teardown order is deliberate:

1. Cancel the session context.
2. Cancel and wait for the active worker operation.
3. Send a best-effort farewell `abort` when appropriate, bounded by two seconds.
4. Close the connection to unblock the reader.
5. Close pending RPC calls and wait for session-owned goroutines.
6. Unblock deadline-capable stdin.
7. Enter the terminal state and return the outcome to `app`, which stops the monitor/processes and writes final or partial output.

Waiting before closing the connection would deadlock a reader blocked in `ReadEnvelope`, so this order is covered by shutdown and leak tests.

## External-command boundary

No test operation invokes a shell. `runner.Runner` accepts a program name and argument vector, resolves it with `exec.LookPath`, injects `LC_ALL=C` and `LANG=C`, and captures bounded stdout/stderr. Child processes run in their own process groups. Cancellation or timeout sends `SIGTERM` to the group, waits a grace period, then uses `SIGKILL`; registry entries identify only CableCheck-owned processes.

Nonzero command exit is data, not automatically an infrastructure error—`ping` exit 1, for example, means loss. Parsers decide how to interpret the result. Raw output is preserved independently of parsed model values.

## Test and mock strategy

The production graph is assembled from interfaces so tests remain hermetic:

- `runner/runnertest.FakeRunner` matches ordered command scripts, returns canned fixture results, records calls, and exposes `FakeProcess` lifecycle channels. It never starts `ping`, `iperf3`, `ip`, or `ethtool` unless a test specifically exercises the real runner.
- `clock/clocktest.FakeClock` advances only when the test requests it. `BlockUntilWaiters` prevents timer-registration races before `Advance`.
- Network discovery and link monitoring accept an injectable sysfs root. Tests build a temporary `/sys/class/net`-shaped tree and mutate attributes such as `carrier`, `speed`, and `carrier_changes`.
- Parser tests consume committed byte fixtures and perform no I/O beyond reading testdata.
- Peer unit tests use `net.Pipe` or injected transports, scripted stdin, fake clocks, and state hooks.
- Runner process-lifecycle tests re-execute the test binary as a helper process, avoiding dependence on installed network tools.
- Application integration tests run PC1 and PC2 in-process over loopback TCP with fake runners and temporary directories. They exercise handshake, orchestration, transfer, interruption, malformed frames, and partial reports without touching a real NIC.
- Leak checks inspect module-owned goroutine stacks after cancellation. Race tests cover concurrent session, runner, and monitor paths.

The manual `scripts/demo-e2e.sh` test is intentionally separate: it uses loopback plus repository stub tools to exercise the built binary. Because loopback is virtual, its expected health result is `INCONCLUSIVE`.
