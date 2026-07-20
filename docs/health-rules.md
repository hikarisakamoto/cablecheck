# Health classification rules

CableCheck evaluates a fixed, deterministic rule set (`1.1.0`) after the test
plan finishes. Rules inspect physical, transport, performance, host, and
coverage evidence. The final class is not a simple average: credible physical
fault evidence deliberately dominates host-sensitive performance symptoms.

Threshold comparisons in the tables are exact. For example, “greater than
1%” does not include exactly 1%.

## Physical rules

| ID | Category | Trigger | Finding severity |
|---|---|---|---|
| `PHY-01` | physical | Link is down when testing ends. | failed |
| `PHY-02` | physical | Reliable CRC-class receive-counter delta is 1–10. | warning |
| `PHY-02` | physical | CRC-class delta is 11–1000, with no standard-ping direction above 1% loss. | poor |
| `PHY-02` | physical | CRC-class delta is greater than 1000, or is greater than 10 and any standard-ping direction has greater than 1% loss. | failed |
| `PHY-03` | physical | Worst reliable per-side carrier-event delta is 1–2. The worse side is used rather than summing both observations of the same bounce. | poor |
| `PHY-03` | physical | Worst reliable per-side carrier-event delta is at least 3. | failed |
| `PHY-04` | physical | The monitor observes at least one mid-test speed/duplex renegotiation. | poor |
| `PHY-05` | physical | Either side negotiates half duplex. | poor |
| `PHY-06` | physical | Negotiated and expected speeds are known, and negotiated speed is below expected speed. | warning |
| `PHY-07` | physical | `PHY-06`'s reduced-speed condition and at least one reliable CRC-class error occur together. | poor |
| `PHY-08` | physical | Opt-in cable diagnostics report `UNSPECIFIED`. | warning |
| `PHY-08` | physical | Opt-in cable diagnostics report `IMPEDANCE`. | poor |
| `PHY-08` | physical | Opt-in cable diagnostics report `OPEN`, `SHORT_INTRA`, or `SHORT_INTER`. | failed |
| `PHY-09` | physical | Reliable frame-size-error delta (jabber/oversize/undersize/length class) is 1–10. | warning |
| `PHY-09` | physical | Frame-size-error delta is greater than 10. | poor |
| `PHY-10` | physical | CRC-class delta is nonzero and a qualifying UDP direction loses greater than 2% at target rate. | failed |

`PHY-08` emits the worst status found across the tested pairs. A clean cable
test emits no finding. Cable-test-induced carrier events are separately
annotated and removed from ordinary carrier-event evidence.

## Transport rules

| ID | Category | Trigger | Finding severity |
|---|---|---|---|
| `TR-01` | transport | Any standard-ping direction has loss greater than 0% and at most 0.1%. | warning |
| `TR-01` | transport | Any standard-ping direction has loss greater than 0.1%. | poor |
| `TR-02` | transport | Full-size ping has any loss in a direction whose standard ping has exactly 0% loss. | poor |
| `TR-03` | transport | At least one fragmentation-needed/full-size don't-fragment error occurs. | warning |
| `TR-04` | transport | At least one duplicate ping reply occurs. | warning |
| `TR-05` | transport | A direction has more than 5 RTT spikes above 10 times its median. | warning |
| `TR-05` | transport | A direction's maximum reply gap is greater than 1 second. | poor |
| `TR-06` | transport | Estimated TCP retransmit rate is at least 0.1% and at most 1%. | warning |
| `TR-06` | transport | Estimated TCP retransmit rate is greater than 1%. | poor |
| `TR-07` | transport | CPU is at most 90%, the UDP sender reaches its target, and loss is at least 0.5% and at most 2%. | warning |
| `TR-07` | transport | Under the same gates, UDP loss is greater than 2%. | poor |
| `TR-08` | transport | UDP jitter is greater than 5 ms in any qualifying direction. | warning |
| `TR-09` | transport | More than 0.1% of UDP datagrams are out of order in any qualifying direction. | warning |

TCP retransmit rate is estimated as retransmissions divided by approximately
`bytes / 1448` (the evaluator's default MSS). For UDP loss to be cable
evidence, actual sender bitrate must reach at least 90% of the target. A target
above 95% of known negotiated speed is considered self-inflicted saturation
and is excluded; a standard-mode reduced-rate run can still supply qualifying
evidence. The same target/saturation qualification gates the jitter and
out-of-order facts used by `TR-08` and `TR-09`. `TR-07` is also suppressed
when maximum iperf3 CPU usage is above 90%.

When both conditions of `TR-05` occur, the rule emits one finding at the worse
severity.

## Performance rules

Every performance finding is marked `hostSensitive` because a NIC, adapter,
driver, CPU, or host can create the symptom without a bad cable.

| ID | Category | Trigger | Finding severity |
|---|---|---|---|
| `PERF-01` | performance | TCP receiver bitrate is at least 90% of negotiated speed. | no finding |
| `PERF-01` | performance | TCP receiver bitrate is at least 70% but below 90% of negotiated speed. | info |
| `PERF-01` | performance | TCP receiver bitrate is at least 40% but below 70% of negotiated speed. | warning |
| `PERF-01` | performance | TCP receiver bitrate is below 40% of negotiated speed. | poor |
| `PERF-02` | performance | TCP interval coefficient of variation is at least 15% and at most 30%. | warning |
| `PERF-02` | performance | TCP interval coefficient of variation is greater than 30%. | poor |
| `PERF-03` | performance | Across both directions, 1–2 TCP intervals after the first fall below half the median of the post-first intervals. | warning |
| `PERF-03` | performance | At least 3 such intervals fall below half the median. | poor |
| `PERF-04` | performance | Both TCP directions exist and `abs(a-b) / max(a,b)` is greater than 30%. | warning |

`PERF-01` does not run when negotiated speed is unknown. Where more than one
direction qualifies, a rule emits the worst applicable severity.

## Host markers

Host findings are markers, not health-severity ladder entries.

| ID | Category | Trigger | Severity | Effect |
|---|---|---|---|---|
| `HOST-01` | host | Maximum iperf3 CPU utilization is greater than 90%. | marker | Marks performance as potentially host-limited. |
| `HOST-02` | host | The tested interface is virtual. | marker | Forces an otherwise non-dominant result to `INCONCLUSIVE`; the run says nothing about a physical cable. |
| `HOST-03` | host | A USB-attached adapter is used and `PERF-01` emits any finding, including info. | marker | Marks the shortfall as potentially adapter/host-limited. |

Virtual interfaces are rejected during normal preflight. `HOST-02` is relevant
when `--allow-virtual-interface` explicitly permits one. A physical `poor` or
`failed` finding is folded first and therefore still dominates even in such a
run.

## Limitation rules

| ID | Category | Trigger | Severity | Effect |
|---|---|---|---|---|
| `LIM-01` | limitation | Both TCP directions are unavailable, or NIC counters are unavailable on both peers. | marker | Changes a tentative `EXCELLENT` or `GOOD` to `INCONCLUSIVE`. |
| `LIM-02` | limitation | Exactly one TCP direction is unavailable, or an unavailable test is named `ping`, `udp`, `bidir`, `bidirectional`, `full_size_ping`, `fullsize_ping`, `cable_test`, or `cable_test_tdr`. | marker | Caps tentative `EXCELLENT` at `GOOD`. |
| `LIM-03` | limitation | The report is partial because the run was interrupted or aborted. | marker | Changes a tentative `EXCELLENT` or `GOOD` to `INCONCLUSIVE`. |
| `LIM-04` | limitation | Link speed was unknown, so the UDP target rate was assumed. | info | Does not by itself change the classification. |
| `LIM-05` | limitation | A throughput test could not connect to the peer's data port (firewall/routing on the receiving side). | marker | Changes a tentative `EXCELLENT` or `GOOD` to `INCONCLUSIVE`; adds a firewall-check recommendation. |

Limitations only downgrade clean-looking outcomes. They never hide warning,
poor, or failed evidence — a real physical `POOR`/`FAILED` still wins over
`LIM-05`.

## Classification fold

Rules are evaluated in ID order: `PHY-01..10`, `TR-01..09`, `PERF-01..04`,
`HOST-01..03`, then `LIM-01..05`. The findings are folded as follows:

1. Any physical `failed` finding yields `FAILED`.
2. Otherwise, any physical `poor` finding yields `POOR`.
3. Otherwise, a `poor`-or-worse transport or performance finding normally
   yields `POOR`. It yields `INCONCLUSIVE` instead only when:
   - `HOST-01` or `HOST-03` is present;
   - physical severity is below warning; and
   - every poor-or-worse finding is marked host-sensitive.
4. Otherwise, any transport/performance warning or physical warning yields
   `WARNING`.
5. Otherwise, an informational physical, transport, or performance deviation
   yields `GOOD`; a completely clean run is `EXCELLENT`.
6. `HOST-02`, `LIM-01`, `LIM-02`, `LIM-03`, and `LIM-05` then apply their caps
   described above. `HOST-02` forces `INCONCLUSIVE` at this stage;
   `LIM-01`/`LIM-03`/`LIM-05` affect only tentative `EXCELLENT`/`GOOD`;
   `LIM-02` changes only `EXCELLENT` to `GOOD`.

This ordering is why a hot CPU can make low throughput inconclusive, but it
cannot excuse CRC errors, ping loss, or another non-host-sensitive failure.

## Score deductions and class bands

Conclusive scores start at 100. Applicable deductions are accumulated, the
result is rounded to the nearest integer, and it is then clamped into the
final classification's band. `INCONCLUSIVE` has a null score.

| Evidence | Deduction |
|---|---:|
| CRC-class errors | 2 points each, capped at 40 |
| Worst-side carrier events | 15 points each, capped at 45 |
| One or more renegotiations | 10 |
| Half duplex | 25 |
| Negotiated below expected speed | 15 |
| Standard-ping loss, per direction | `lossPercent × 20`, capped at 40 per direction |
| TCP retransmit rate 0.1%–1%, per direction | 5 |
| TCP retransmit rate greater than 1%, per direction | 15 |
| Qualifying UDP loss 0.5%–2%, per direction | 5 |
| Qualifying UDP loss greater than 2%, per direction | 15 |
| Full-size loss with clean standard ping (`TR-02`) | 20 once |
| Worst TCP coefficient of variation 15%–30% | 5 |
| Worst TCP coefficient of variation greater than 30% | 15 |
| TCP collapse intervals | 5 each, capped at 20 |
| Worst TCP ratio 40%–below 70% of negotiated speed | 10 |
| Worst TCP ratio below 40% of negotiated speed | 25 |
| TCP directional asymmetry greater than 30% | 5 |
| UDP jitter greater than 5 ms in either direction | 5 once |

The TCP-ratio deduction is omitted when `HOST-01` or `HOST-03` marks the run as
host-limited. UDP-loss deductions require CPU at most 90%, an available result,
and a qualifying target reached. A `PERF-01` info result in the 70%–below-90%
range has no direct score deduction, but its `GOOD` classification clamps the
score to that class's band.

| Classification | Score band |
|---|---:|
| `FAILED` | 0–25 |
| `POOR` | 26–50 |
| `WARNING` | 51–79 |
| `GOOD` | 80–94 |
| `EXCELLENT` | 95–100 |
| `INCONCLUSIVE` | null |

Band clamping ensures the numeric score never contradicts the rule-derived
classification. It can lower or raise the raw deducted score to the nearest
edge of the class band.

## Worked examples

These examples use the committed reports under [`examples/`](../examples/).

### Healthy: EXCELLENT

[`examples/healthy/report.json`](../examples/healthy/report.json) has a stable
1 Gbit/s full-duplex link, receiver rates of about 941 and 939.1 Mbit/s, no
loss, and no error-counter movement. No rule fires. The result is
`EXCELLENT`, score 100.

### Reduced speed: WARNING

[`examples/reduced-speed/report.json`](../examples/reduced-speed/report.json)
shows 100 Mbit/s negotiated while both NICs support 1 Gbit/s. `PHY-06` fires at
warning severity. The raw score is 85 after the 15-point reduced-speed
deduction, then the warning band clamps it to 79: `WARNING`, score 79.

### CRC errors: FAILED in the committed example

[`examples/crc-errors/report.json`](../examples/crc-errors/report.json) records
1,543 `rx_crc` plus 12 `rx_align` increments: 1,555 CRC-class errors. That is
above `PHY-02`'s 1,000-error failed threshold, so the actual committed result
is `FAILED`, score 25 after band clamping.

A lower clean-ping case demonstrates the `POOR` branch: 42 reliable CRC-class
increments would trigger `PHY-02` at poor severity (more than 10 but not more
than 1,000, with no direction above 1% ping loss). The CRC deduction is capped
at 40, producing raw score 60, then the poor band clamps it to 50.

### Host-limited: INCONCLUSIVE

[`examples/host-limited/report.json`](../examples/host-limited/report.json)
shows about 250 and 248 Mbit/s on a clean 1 Gbit/s link. `PERF-01` is poor and
host-sensitive, while peak iperf3 CPU is 98.4%, triggering `HOST-01`. With no
physical warning and no non-host-sensitive poor evidence, the fold returns
`INCONCLUSIVE` and the score is null. This result does not prove that the cable
is bad.
