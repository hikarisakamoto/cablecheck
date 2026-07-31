# Health classification rules

CableCheck runs a fixed, deterministic rule set (`1.11.0`) once the test plan
finishes. Rules inspect physical, transport, performance, host, and coverage
evidence. The final class isn't a simple average. Credible physical fault
evidence dominates host-sensitive performance symptoms.

Threshold comparisons in the tables are exact. "Greater than 1%" does not
include exactly 1%.

## Calibration and provenance

The [`Default` threshold policy](../internal/evaluate/thresholds.go) is the
executable source of truth for rule severity boundaries, host gating, and score
bands. Parser-owned measurement definitions are likewise fixed; notably,
[`tcpmetrics`](../internal/tcpmetrics/collapse.go) is the single implementation
of TCP collapse extraction. CableCheck does not offer a sensitivity preset
that could turn the same evidence into a more favorable result. The tables
below restate those policies for operators; they do not define another
implementation.

Changing a default value or its inclusive/exclusive comparison requires an
explicit `RulesVersion` decision, boundary-test review, and review of the
committed example reports. Version `1.2.0` introduced speed-scaled TCP
throughput bands and graded severe negotiated-speed reductions as poor.
Version `1.3.0` evaluates the already-collected transmit-carrier, PHY,
receive-FIFO, and receive-missed counters. Version `1.4.0` aggregates repeated
TCP bitrate, interval-variation, and retransmission facts with a lower median
instead of letting every worst pass determine the verdict, while retaining
collapse evidence from the worst pass. Version `1.5.0` consistently omits
host-sensitive TCP performance deductions when host evidence identifies a
likely bottleneck; the findings remain visible. Version `1.6.0` attributes
iperf3 CPU measurements to their one-way traffic direction for UDP evidence
and performance score deductions, while retaining the report-wide host marker
and classification safeguard. Version `1.7.0` grounds follow-up recommendations
in the concrete finding evidence collected by the same run. Version `1.8.0`
counts the unexplained remainder of a driver's own receive-error aggregate as
CRC-class evidence without letting it reach failed on count alone, reports a side
that exposes no receive-error counter as a limitation instead of as zero errors
and refuses the isolated-outlier cap without that evidence, rates receive-ring
drops against frames received before they gate the host-sensitive score, warns on
the worst repeat trial's retransmission rate while still requiring the
sustained rate to convict, marks `TR-06` host-sensitive so measured host evidence
can hold a retransmission verdict at `INCONCLUSIVE`, and lowers the retransmit
warning boundary to 0.01%. Version `1.9.0` lets `PHY-03` fall back to the link
monitor's spontaneous carrier transitions when neither side has reliable
counter deltas, with the same
thresholds and the monitor named in the evidence — counters stay authoritative
whenever any side has them, and sub-poll-interval flaps (surfaced as
renegotiation events, `PHY-04`'s evidence) are deliberately not counted, so the
fallback can only undercount; and restricts `TR-03` to classified
fragmentation failures (ICMP frag-needed/packet-too-big replies and local
EMSGSIZE send errors), so other ICMP errors — e.g. host-unreachable replies
while the link is down — no longer masquerade as an MTU mismatch and instead
surface as `TR-01` loss evidence. Version `1.10.0` makes the limitation
markers name the actual counter gap on interrupted runs: when a side captured
a counter baseline but the run was interrupted before its final snapshot,
`LIM-01` (both sides affected) and `LIM-02` (one side affected) say the final
snapshot is missing instead of claiming the NIC never exposed error counters —
the old wordings remain for completed runs and for NICs where no counters were
ever readable. Interrupted quick/standard runs also salvage a best-effort
local final counter snapshot at the abort boundary and request the peer's final
snapshot through a short, non-arming RPC when it remains responsive. The local
deltas still survive a lost peer. Version `1.11.0` makes verdict prose distinguish
link-level evidence from component isolation and describes `PHY-03` values as
carrier state changes rather than bounces. Thresholds are unchanged.

### Reference conditions and status labels

Thresholds assume two known computers on a trusted direct Ethernet link. NIC
counter evidence is used only when before/after deltas are reliable. CRC-class
evidence is the sum of the per-cause receive-error counters and the unexplained
remainder of the driver's own receive-error aggregate; a side exposing no
receive-error counter at all is treated as unmeasured, never as zero errors. TCP
throughput is receiver bitrate divided by negotiated link speed. UDP loss,
jitter, and reordering are admitted only from runs that reached the target and
whose requested rate was not near known link capacity. TCP retransmit rate is
an estimate based on transmitted bytes and the 1448-byte fallback MSS described
below. Completed repeat TCP trials are reduced independently per direction and
metric. Bitrate and interval variation use the lower median (for an even count,
the lower middle value), because they measure a steady state. Collapse interval
count keeps the worst trial. Retransmissions keep both reductions: the lower
median is the sustained rate, and only it can produce a poor verdict, while the
worst trial preserves a burst the median would smooth away and warns. Keeping
them apart is what stops a soak's larger trial count from escalating a verdict by
itself. Trials without usable retransmission data are omitted from only that
metric; an explicit zero remains a measurement. Any incomplete repeat makes its
whole direction unavailable. TCP collapse event
lengths are summed within a pass, and the resulting interval count remains the
maximum from any pass.
Count thresholds are absolute across quick, standard, and soak modes and are not
normalized by duration, with one deliberate exception: receive-ring drops are
rated against the frames that side actually received, because that marker
suppresses every host-sensitive score deduction and a handful of drops across
tens of millions of frames cannot limit throughput. A completed run always
receives frames, so the no-denominator case is defensive rather than expected;
should it arise, unrated movement counts rather than being dismissed.

The provenance status is intentionally conservative:

- **Conservative policy**: an engineering boundary chosen to avoid calling
  ordinary measurement noise a cable fault. It is not the result of a field
  calibration campaign.
- **Fixture-backed conservative policy**: additionally exercised by committed
  regression examples, but those examples are not population measurements.
- **Measurement gate**: decides whether a result is suitable evidence rather
  than whether the cable is healthy.
- **Presentation policy**: keeps a numeric score consistent with the rule-based
  class; it is not a physical measurement.

No current verdict threshold is claimed to be standards-mandated or broadly
field-calibrated. Protocol definitions explain how some metrics are measured,
but do not establish CableCheck's health boundary.

| Threshold | Default and exact comparison | Reference condition and rationale | Status |
|---|---|---|---|
| CRC warning/poor | Poor when reliable aggregate CRC-class movement is `> 10`; 1–10 warns. CRC-class movement includes the unexplained remainder of the driver's receive-error aggregate. | Any movement is anomalous on a direct link; the first band avoids escalating a small absolute count immediately. Counting the remainder is what makes NICs with no per-cause counter (Realtek) contribute evidence at all instead of a silent zero. | Conservative policy |
| CRC failed | Failed when reliable aggregate movement is `> 1000`, unless every counted error came from a driver receive-error aggregate rather than a per-cause counter. | A large absolute error count is treated as independently compelling physical evidence. An unexplained aggregate is not: those counters have driver-defined semantics and a driver could fold its own receive-ring drops into them, so on count alone such evidence never escalates past warning and reaches failed only with corroborating ping loss. That ceiling is what stops a host-side drop from being scored as a cable fault. | Conservative policy |
| CRC + ping corroboration | CRC movement `> 10` fails when any standard-ping direction is also `> 1%` loss. | Independent counter and packet-loss signals increase confidence that corruption is observable in traffic. | Conservative policy |
| Carrier events | Failed at `>= 3` state changes on the worse reliable side; 1–2 is poor. | The worse side avoids double-counting the same carrier transition observed by both peers; at least three transitions are decisive. Each down and up edge counts separately. | Conservative policy |
| Frame-size errors | Poor when reliable aggregate movement is `> 10`; 1–10 warns. | Jabber, oversize, undersize, and length movement is abnormal, with a small-count warning band. | Conservative policy |
| Transmit-carrier/PHY errors | Poor when reliable aggregate `tx_carrier` and `phy_errors` movement is `> 10`; failed when it is `> 1000`; 1–10 warns. | These counters are near-direct physical-layer evidence. They use an independent policy with the same conservative count bands as CRC-class errors, without CRC-specific ping-loss corroboration. | Conservative policy |
| Negotiated-speed reduction | Poor when negotiated speed is `<= 50%` of expected speed; a smaller reduction warns. | A large capability loss, such as 100 Mbit/s on a 1 Gbit/s-capable pair, is strong physical evidence even when traffic fills the reduced link. | Conservative policy |
| Standard-ping loss | Poor when loss is `> 0.1%`; any positive loss up to that boundary warns. | A direct cable should be lossless, while the first nonzero band avoids overstating a very small sample count. | Conservative policy |
| RTT spike count | Warning when a direction has `> 5` parser-identified spikes. | Requires repeated outliers rather than one event. The parser identifies a spike above `max(5 × median, median + 10 ms)`; this threshold counts those events. | Conservative policy |
| RTT reply gap | Poor when the longest gap is `> 1 s`. | A full-second interruption is operationally significant on a direct link. | Conservative policy |
| TCP retransmit warning | Warning at an estimated rate `>= 0.01%`. | A direct cable has no legitimate congestion source, so the expected retransmit count is zero. The previous 0.1% boundary tolerated roughly 4,800 lost segments in a 60-second gigabit trial before saying anything; 0.01% still absorbs slow-start and queue noise while a real burst is reported. | Conservative policy |
| TCP retransmit poor | Poor when the estimated rate is `> 1%`; exactly 1% remains warning. | Separates elevated retransmission from a strongly degraded stream. Deliberately not lowered with the warning boundary: retransmissions alone cannot distinguish wire corruption from local queue drops, which CableCheck does not yet measure. | Conservative policy |
| UDP target reached | Admit a run when actual sender bitrate is `>= 90%` of target. | Rejects loss from a sender that could not generate the requested load. | Measurement gate |
| UDP near saturation | Exclude a run when requested bitrate is `> 95%` of known negotiated speed. | Near-line-rate UDP can create self-inflicted queue loss rather than cable evidence. | Measurement gate |
| UDP loss warning | Warning at `>= 0.5%` qualifying loss. | Allows a small margin before treating UDP loss as a health finding. | Conservative policy |
| UDP loss poor | Poor when qualifying loss is `> 2%`; exactly 2% remains warning. | Sustained loss at this level is considered materially degraded; with CRC movement it also supplies PHY-10 correlation. | Conservative policy |
| UDP jitter | Warning when qualifying jitter is `> 5 ms`. | Multi-millisecond variation is unexpected on a direct link, but the value is not an RFC health mandate. | Conservative policy |
| UDP reordering | Warning when qualifying reordering is `> 0.1%`. | Reordering should not normally occur on a single direct path; the boundary avoids elevating one tiny fractional result. | Conservative policy |
| TCP throughput on links `<= 100 Mbit/s` | Info at `>= 90%`, warning at `>= 70%` and `< 90%`, poor below 70%; this tier never passes silently. | Low-speed links remain visible even when filled; a marginal 100 Mbit/s link must not appear clean. The committed 94 Mbit/s case is a regression reference. | Fixture-backed conservative policy |
| TCP throughput on links `> 100 Mbit/s` and `<= 1 Gbit/s` | Pass at `>= 90%`, info at `>= 70%` and `< 90%`, warning at `>= 40%` and `< 70%`, poor below 40%. | Allows ordinary protocol and host overhead. The healthy 1 Gbit/s example at about 94% is a regression reference. | Fixture-backed conservative policy |
| TCP throughput on links `> 1 Gbit/s` | Uses the 1 Gbit/s 90/70/40 bands as an explicitly uncalibrated fallback. | No real high-speed capture exists yet; these values are conservative compatibility behavior, not authoritative 2.5G/5G/10G calibration. | Conservative caveat policy |
| TCP coefficient of variation warning | Warning at `>= 15%`. | Flags repeated interval instability while allowing ordinary run-to-run variation. | Conservative policy |
| TCP coefficient of variation poor | Poor when `> 30%`; exactly 30% remains warning. | Marks strongly unstable interval throughput. | Conservative policy |
| TCP collapse interval | Count an interval when it is `< 50%` of the post-first-interval median. Consecutive intervals are stored as one event whose `len` contributes each interval. | Excludes slow start, then identifies a substantial within-run drop relative to that run. | Conservative policy |
| TCP collapse count | Poor at `>= 3` counted intervals; 1–2 warns. Repeated trials retain the largest per-trial interval count in each direction. | Repeated or sustained collapses carry more weight than an isolated interval without additionally summing evidence across repeated trials. | Conservative policy |
| TCP directional asymmetry | Warning when `abs(a-b) / max(a,b)` is `> 30%`. | A large directional difference is notable but host-sensitive. | Conservative policy |
| Isolated TCP throughput outlier | When exactly one of at least two completed trials in a direction misses the throughput policy, cap a poor `PERF-01` result at warning. Requires reliable counters and measured receive-error evidence on both peers, and no `PHY-01`–`PHY-11` finding. A peer exposing no receive-error counter reports a reliable, empty set by construction, which cannot establish a clean physical layer. | Prevents one host-sensitive pass from deciding a poor verdict without weakening physical or repeated evidence. | Conservative policy |
| Receive-ring drop rate | Count receive-ring movement as host-limitation evidence when the combined `rx_fifo` and `rx_missed` volume exceeds `0.0001%` of the frames that side received, or exceeds `100` frames outright. Any movement counts when no frames-received delta exists. | This marker suppresses every host-sensitive performance deduction, so it must describe drops frequent enough to plausibly limit throughput. Two dropped frames in eighteen million cannot, and previously silenced the whole performance score. The boundary is set low on purpose: a ring overflow drops roughly one in-flight window and then stalls the sender for a retransmission timeout, so the TCP symptoms this marker gates are produced by tens to low hundreds of dropped frames, not thousands — a higher boundary would leave the safeguard unreachable for TCP-only starvation and blame the cable for the host. The counters are rated on their combined volume because they are distinct registers on the mapped drivers, while each is still reported separately since drivers may count overlapping events. The absolute floor keeps a real burst visible in a long soak, whose counters bracket the whole run and would otherwise dilute one bad cycle below any rate. Sub-threshold movement stays visible in the counter deltas. | Conservative policy |
| Host CPU | Mark global host limitation when maximum iperf3 CPU across all throughput tests is `> 90%`. Gate one-way UDP-loss evidence and host-sensitive TCP/UDP-loss score deductions only when CPU for that traffic direction is `> 90%`; exactly 90% remains eligible. | A highly utilized endpoint can confound performance evidence; directional attribution prevents load in the opposite direction from hiding clean-direction evidence. iperf3 CPU percentage is not treated as a graded starvation measure. | Conservative policy |
| Score bands | Failed 0–25, poor 26–50, warning 51–79, good 80–94, excellent 95–100; inconclusive has no score. | Clamping prevents the secondary numeric score from contradicting the rule-derived class. | Presentation policy |

## Physical rules

| ID | Category | Trigger | Finding severity |
|---|---|---|---|
| `PHY-01` | physical | Link is down when testing ends. | failed |
| `PHY-02` | physical | Reliable CRC-class receive-counter delta is 1–10. | warning |
| `PHY-02` | physical | CRC-class delta is 11–1000, with no standard-ping direction above 1% loss. | poor |
| `PHY-02` | physical | CRC-class delta is greater than 1000 with at least one per-cause counter contributing, or is greater than 10 and any standard-ping direction has greater than 1% loss. | failed |
| `PHY-02` | physical | The delta came entirely from driver receive-error aggregates and ping is clean. Severity stays at warning whatever the count, and the finding names the errors as unexplained rather than as CRC. | warning |
| `PHY-03` | physical | Worst reliable per-side carrier-state-change delta is 1–2. The worse side is used rather than summing both observations of the same transition. When neither side has reliable deltas, the monitor's spontaneous carrier transitions are used instead (same both-edges unit; evidence names the monitor). | poor |
| `PHY-03` | physical | Worst reliable per-side carrier-state-change delta (or the monitor fallback count) is at least 3. | failed |
| `PHY-04` | physical | The monitor observes at least one mid-test speed/duplex renegotiation. | poor |
| `PHY-05` | physical | Either side negotiates half duplex. | poor |
| `PHY-06` | physical | Negotiated and expected speeds are known, and negotiated speed is below expected but above 50% of expected. | warning |
| `PHY-06` | physical | Negotiated speed is at most 50% of expected speed. | poor |
| `PHY-07` | physical | `PHY-06`'s reduced-speed condition and at least one reliable CRC-class error occur together. | poor |
| `PHY-08` | physical | Opt-in cable diagnostics report `UNSPECIFIED`. | warning |
| `PHY-08` | physical | Opt-in cable diagnostics report `IMPEDANCE`. | poor |
| `PHY-08` | physical | Opt-in cable diagnostics report `OPEN`, `SHORT_INTRA`, or `SHORT_INTER`. | failed |
| `PHY-09` | physical | Reliable frame-size-error delta (jabber/oversize/undersize/length class) is 1–10. | warning |
| `PHY-09` | physical | Frame-size-error delta is greater than 10. | poor |
| `PHY-10` | physical | CRC-class delta is nonzero and a qualifying UDP direction loses greater than 2% at target rate. | failed |
| `PHY-11` | physical | Reliable aggregate `tx_carrier` and `phy_errors` delta is 1–10. | warning |
| `PHY-11` | physical | The aggregate delta is 11–1000. | poor |
| `PHY-11` | physical | The aggregate delta is greater than 1000. | failed |

Throughout the `PHY-02`, `PHY-07` and `PHY-10` rows, the CRC-class delta is the
per-cause receive-error counters (`rx_crc`, `rx_frame`, `rx_align`, `rx_symbol`)
plus whatever the driver's own `rx_errors` aggregate reports beyond every
per-cause receive counter it could account for, including the ring-drop
counters. The remainder is clamped at zero, so no error is counted twice and a
host-side drop is never recharged as corruption. Its evidence line names it
separately from the per-cause total, and when it is the only evidence the
`PHY-02` finding says the errors are unexplained rather than asserting CRC.
Because the driver's aggregate may overlap its own per-cause counters (e1000e
reports the same oversized frame as both a length and a long-length error), the
subtraction can understate the remainder. That direction is deliberate: this
policy would rather miss ambiguous evidence than invent a cable fault.

`PHY-08` emits the worst status found across the tested pairs. A clean cable
test emits no finding. Carrier events caused by the cable test itself are
annotated separately and removed from the ordinary carrier-event evidence.

## Transport rules

| ID | Category | Trigger | Finding severity |
|---|---|---|---|
| `TR-01` | transport | Any standard-ping direction has loss greater than 0% and at most 0.1%. | warning |
| `TR-01` | transport | Any standard-ping direction has loss greater than 0.1%. | poor |
| `TR-02` | transport | Full-size ping has any loss in a direction whose standard ping has exactly 0% loss. | poor |
| `TR-03` | transport | At least one classified fragmentation failure occurs during the full-size don't-fragment ping (ICMP frag-needed/packet-too-big reply or local EMSGSIZE send error). Other ICMP errors are not MTU evidence; they appear as `TR-01` loss evidence instead. | warning |
| `TR-04` | transport | At least one duplicate ping reply occurs. | warning |
| `TR-05` | transport | A direction has more than 5 parser-identified RTT spikes. | warning |
| `TR-05` | transport | A direction's maximum reply gap is greater than 1 second. | poor |
| `TR-06` | transport | The worst trial's estimated TCP retransmit rate is at least 0.01%, while the rate sustained across trials is at most 1%. | warning |
| `TR-06` | transport | The rate sustained across trials is greater than 1%. | poor |
| `TR-07` | transport | CPU for that UDP direction is at most 90%, the sender reaches its target, and loss is at least 0.5% and at most 2%. | warning |
| `TR-07` | transport | Under the same gates, UDP loss is greater than 2%. | poor |
| `TR-08` | transport | UDP jitter is greater than 5 ms in any qualifying direction. | warning |
| `TR-09` | transport | More than 0.1% of UDP datagrams are out of order in any qualifying direction. | warning |

TCP retransmit rate is estimated as retransmissions divided by approximately
`bytes / 1448` (the evaluator's default MSS). For UDP loss to count as cable
evidence, the actual sender bitrate must reach at least 90% of the target. A
target above 95% of known negotiated speed counts as self-inflicted saturation
and is excluded, though a standard-mode reduced-rate run can still supply
qualifying evidence. The same target and saturation checks gate the jitter and
out-of-order facts used by `TR-08` and `TR-09`. `TR-07` independently excludes
each direction whose qualifying UDP runs report endpoint CPU above 90%. CPU
load in another direction or in the bidirectional stress test does not exclude
otherwise qualifying loss.

When both conditions of `TR-05` occur, the rule emits a single finding at the
worse severity.

## Performance rules

Every performance finding is marked `hostSensitive`. A NIC, adapter, driver,
CPU, or host can produce these symptoms without a bad cable.

| ID | Category | Trigger | Finding severity |
|---|---|---|---|
| `PERF-01` | performance | On links at or below 100 Mbit/s, TCP receiver bitrate is at least 90% of negotiated speed. | info |
| `PERF-01` | performance | On links at or below 100 Mbit/s, TCP receiver bitrate is at least 70% but below 90%. | warning |
| `PERF-01` | performance | On links at or below 100 Mbit/s, TCP receiver bitrate is below 70%. | poor |
| `PERF-01` | performance | On links above 100 Mbit/s, TCP receiver bitrate is at least 90% of negotiated speed. | no finding |
| `PERF-01` | performance | On links above 100 Mbit/s, TCP receiver bitrate is at least 70% but below 90%. | info |
| `PERF-01` | performance | On links above 100 Mbit/s, TCP receiver bitrate is at least 40% but below 70%. | warning |
| `PERF-01` | performance | On links above 100 Mbit/s, TCP receiver bitrate is below 40%. | poor |
| `PERF-02` | performance | TCP interval coefficient of variation is at least 15% and at most 30%. | warning |
| `PERF-02` | performance | TCP interval coefficient of variation is greater than 30%. | poor |
| `PERF-03` | performance | Across both directions, carried event lengths total 1–2 TCP intervals after the first below 50% of the median of the post-first intervals. | warning |
| `PERF-03` | performance | Carried event lengths total at least 3 such intervals. | poor |
| `PERF-04` | performance | Both TCP directions exist and `abs(a-b) / max(a,b)` is greater than 30%. The finding remains visible under host load; its score deduction applies only when sender CPU in both directions is at most 90%. | warning |

`PERF-01` doesn't run when negotiated speed is unknown. Links above 1 Gbit/s
currently use the explicitly uncalibrated 1 Gbit/s fallback bands pending real
high-speed captures. When more than one direction qualifies, a rule emits the
worst applicable severity. The isolated-outlier cap applies independently by
direction and only to `PERF-01`: informational and warning results are unchanged,
while poor becomes warning. An uncapped poor result in the other direction still
wins. Collapse, variation, retransmission, transport, and physical findings are
never softened by this cap.

Current results carry grouped collapse events directly from parsing. A
non-nil empty event array is authoritative clean evidence. Protocol v1 allows
different CableCheck build versions, so a missing/null event array from an
older peer is analyzed from retained intervals by the same canonical
`tcpmetrics` implementation; there is no second threshold or detector.

## Host markers

Host findings are markers, not health-severity ladder entries.

| ID | Category | Trigger | Severity | Effect |
|---|---|---|---|---|
| `HOST-01` | host | Maximum iperf3 CPU utilization is greater than 90%. | marker | Marks performance as potentially host-limited. |
| `HOST-02` | host | The tested interface is virtual. | marker | Forces an otherwise non-dominant result to `INCONCLUSIVE`; the run says nothing about a physical cable. |
| `HOST-03` | host | A USB-attached adapter is used and `PERF-01` emits any finding, including info. | marker | Marks the shortfall as potentially adapter/host-limited. |
| `HOST-04` | host | A reliable `rx_fifo` or `rx_missed` delta exceeds 0.001% of the frames that peer received, or is nonzero when no frames-received delta exists. | marker | Marks performance as potentially limited by the host draining the NIC receive ring. The two counters remain separate evidence because drivers may count overlapping drops, and each is rated against the same frame count. |

Virtual interfaces are rejected during normal preflight. `HOST-02` only matters
when `--allow-virtual-interface` explicitly permits one. Even then, a physical
`poor` or `failed` finding is folded first, so it still dominates.

## Limitation rules

| ID | Category | Trigger | Severity | Effect |
|---|---|---|---|---|
| `LIM-01` | limitation | Both TCP directions are unavailable, NIC counters are unavailable on both peers, or neither peer exposes any receive-error counter. | marker | Changes a tentative `EXCELLENT` or `GOOD` to `INCONCLUSIVE`. |
| `LIM-02` | limitation | Exactly one TCP direction is unavailable, exactly one peer exposes no receive-error counter, or an unavailable test is named `ping`, `udp`, `bidir`, `bidirectional`, `full_size_ping`, `fullsize_ping`, `cable_test`, or `cable_test_tdr`. | marker | Caps tentative `EXCELLENT` at `GOOD`. |
| `LIM-03` | limitation | The report is partial because the run was interrupted or aborted. | marker | Changes a tentative `EXCELLENT` or `GOOD` to `INCONCLUSIVE`. |
| `LIM-04` | limitation | Link speed was unknown, so the UDP target rate was assumed. | info | Does not by itself change the classification. |
| `LIM-05` | limitation | A throughput test could not connect to the peer's data port (firewall/routing on the receiving side). | marker | Changes a tentative `EXCELLENT` or `GOOD` to `INCONCLUSIVE`; adds a firewall-check recommendation. |

Limitations only downgrade clean-looking outcomes. They never hide warning,
poor, or failed evidence. A real physical `POOR`/`FAILED` still wins over
`LIM-05`.

A peer counts as exposing a receive-error counter when its normalized counters
carry any of `rx_crc`, `rx_frame`, `rx_align`, `rx_symbol`, or the driver's
`rx_errors` aggregate. Carrier changes and receive-ring drop counters do not
qualify: they say nothing about frame integrity, and carrier changes are always
readable, which previously made every run look as though it had counter
evidence. A NIC whose driver exposes no receive-error counter, or a run where
`ethtool -S` was unavailable and rtnetlink reported nothing, therefore reports a
limitation rather than certifying a receive path it never measured.

## Classification fold

Rules are evaluated in ID order: `PHY-01..11`, `TR-01..09`, `PERF-01..04`,
`HOST-01..04`, then `LIM-01..05`. The findings are folded as follows:

1. Any physical `failed` finding yields `FAILED`.
2. Otherwise, any physical `poor` finding yields `POOR`.
3. Otherwise, a `poor`-or-worse transport or performance finding normally
   yields `POOR`. It yields `INCONCLUSIVE` instead only when:
   - `HOST-01`, `HOST-03`, or `HOST-04` is present;
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

This ordering is why a hot CPU can make low throughput inconclusive, but can't
excuse CRC errors, ping loss, or any other non-host-sensitive failure.
`HOST-01` deliberately remains report-wide for this classification safeguard,
even though score and UDP-evidence gating use directional CPU attribution.

## Score deductions and class bands

Conclusive scores start at 100. The evaluator sums the applicable deductions,
rounds to the nearest integer, then clamps the result into the final
classification's band. `INCONCLUSIVE` has a null score.

| Evidence | Deduction |
|---|---:|
| CRC-class errors | 2 points each, capped at 40 |
| Worst-side carrier events | 15 points each, capped at 45 |
| One or more renegotiations | 10 |
| Half duplex | 25 |
| Negotiated below expected speed | 15 |
| Standard-ping loss, per direction | `lossPercent × 20`, capped at 40 per direction |
| Worst-trial TCP retransmit rate at least 0.01%, sustained rate at most 1%, per direction | 5 |
| Sustained TCP retransmit rate greater than 1%, per direction | 15 |
| Qualifying UDP loss 0.5%–2%, per direction | 5 |
| Qualifying UDP loss greater than 2%, per direction | 15 |
| Full-size loss with clean standard ping (`TR-02`) | 20 once |
| Worst eligible-direction TCP coefficient of variation 15%–30% | 5 |
| Worst eligible-direction TCP coefficient of variation greater than 30% | 15 |
| Eligible-direction TCP collapse intervals | 5 each, capped at 20 |
| Worst eligible-direction TCP ratio in the warning tier (70%–below 90% at `<= 100 Mbit/s`; 40%–below 70% otherwise) | 10 |
| Worst eligible-direction TCP ratio in the poor tier (below 70% at `<= 100 Mbit/s`; below 40% otherwise) | 25 |
| TCP directional asymmetry greater than 30%, when both directional senders are eligible | 5 |
| UDP jitter greater than 5 ms in either direction | 5 once |

TCP-ratio, coefficient-of-variation, and collapse deductions omit only
directions whose one-way TCP result reports endpoint CPU above 90%.
The asymmetry deduction is omitted when either direction's sender CPU exceeds
90%; receiver-only load does not invoke this sender-specific caveat.
`HOST-03` and `HOST-04` still omit all host-sensitive performance deductions
because their USB and receive-ring evidence has no reliable directional
attribution. Findings remain in the report; only score arithmetic is gated.
UDP-loss deductions independently require directional CPU at most 90%, an
available result, and a qualifying target reached. A `PERF-01` info result has
no direct score deduction, but its `GOOD` classification still clamps the score
to that band. A poor ratio reduced by the
isolated-outlier cap uses the 10-point warning deduction, keeping the score and
finding severity aligned. `PHY-11` has no separate arithmetic deduction; its
severity sets the classification and the score is clamped into that class's
band.

Native `--bidir` and the coordinated two-client fallback both expose one shared
CPU block in the bidirectional result. The fallback block is intentionally
coarser than the ordinary one-way results: it can contribute to global
`HOST-01`, but it is not used for directional TCP or UDP score gates.

| Classification | Score band |
|---|---:|
| `FAILED` | 0–25 |
| `POOR` | 26–50 |
| `WARNING` | 51–79 |
| `GOOD` | 80–94 |
| `EXCELLENT` | 95–100 |
| `INCONCLUSIVE` | null |

Band clamping keeps the numeric score from ever contradicting the rule-derived
classification. It can pull the raw deducted score up or down to the nearest
edge of the class band.

## Worked examples

These examples use the committed reports under [`examples/`](../examples/).

### Healthy: EXCELLENT

[`examples/healthy/report.json`](../examples/healthy/report.json) has a stable
1 Gbit/s full-duplex link, receiver rates of about 941 and 939.1 Mbit/s, no
loss, and no error-counter movement. Nothing fires, so the result is
`EXCELLENT`, score 100.

### Reduced speed: POOR

[`examples/reduced-speed/report.json`](../examples/reduced-speed/report.json)
shows 100 Mbit/s negotiated while both NICs support 1 Gbit/s. Because 100
Mbit/s is only 10% of the expected speed, `PHY-06` fires at poor severity.
Throughput around 94 Mbit/s also produces the low-speed tier's informational
`PERF-01` finding. The 15-point reduced-speed deduction leaves a raw score of
85, which the poor band clamps down to 50. Result: `POOR`, score 50.

### CRC errors: FAILED in the committed example

[`examples/crc-errors/report.json`](../examples/crc-errors/report.json) records
1,543 `rx_crc` plus 12 `rx_align` increments, for 1,555 CRC-class errors. That
clears `PHY-02`'s 1,000-error failed threshold, so the committed result is
`FAILED`, score 25 after band clamping.

For a look at the `POOR` branch, consider a lower clean-ping case. 42 reliable
CRC-class increments would trigger `PHY-02` at poor severity: more than 10 but
not more than 1,000, with no direction above 1% ping loss. The CRC deduction
caps at 40, giving a raw score of 60, which the poor band clamps to 50.

### Host-limited: INCONCLUSIVE

[`examples/host-limited/report.json`](../examples/host-limited/report.json)
shows about 250 and 248 Mbit/s on a clean 1 Gbit/s link. `PERF-01` is poor and
host-sensitive, and peak iperf3 CPU is 98.4%, which triggers `HOST-01`. There's
no physical warning and no non-host-sensitive poor evidence, so the fold returns
`INCONCLUSIVE` with a null score. That result doesn't prove the cable is bad.
