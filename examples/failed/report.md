# CableCheck Report

## 1. Overall Result

**FAILED** — The cable failed critical tests and must be replaced.

## 2. Score & Rule Evidence

- **Score:** 25/100
- **Reason:** The link was down when testing ended.
- **Reason:** The link bounced 4 time(s) during the test.
- **Reason:** Critical evidence is missing — a clean-looking result would not be trustworthy.
- **Reason:** The run was interrupted — the report covers only the tests that completed.

| Rule | Category | Severity | Finding |
| --- | --- | --- | --- |
| PHY-01 | physical | failed | The link was down when testing ended. |
| PHY-03 | physical | failed | The link bounced 4 time(s) during the test. |
| LIM-01 | limitation | marker | Critical evidence is missing — a clean-looking result would not be trustworthy. |
| LIM-03 | limitation | marker | The run was interrupted — the report covers only the tests that completed. |

## 3. Session Info

- **Test ID:** example-failed
- **Schema version:** 1.0.0
- **Tool version:** 1.0.0
- **Protocol version:** 1
- **Started:** 2026-01-02T15:04:05Z
- **Finished:** 2026-01-02T15:05:35Z
- **Duration:** 1m30s
- **Mode:** quick
- **Partial run:** yes

## 4. Machines & Environment

| Side | Hostname | Kernel | OS | NIC | Driver | Speed | Duplex | MTU | MAC | USB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 | alpha | 6.9.1-generic | linux/amd64 | enp3s0 | e1000e | 1000 Mb/s | full | 1500 | aa:bb:cc:00:11:22 | no |
| pc2 | bravo | 6.9.1-generic | linux/amd64 | enp4s0 | e1000e | 1000 Mb/s | full | 1500 | aa:bb:cc:00:33:44 | no |

## 5. Interface & Link Negotiation

| Side | Phase | Speed | Duplex | Autoneg | Link | MDI-X | Partner modes |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 | before | 1000 Mb/s | full | on | yes | on (auto) | 1000baseT/Full |
| pc1 | after | unknown | unknown | on | no | unknown | unknown |
| pc2 | before | 1000 Mb/s | full | on | yes | on (auto) | 1000baseT/Full |
| pc2 | after | unknown | unknown | on | no | unknown | unknown |

## 6. Link Events Timeline

| At | Event | Detail |
| --- | --- | --- |
| 2026-01-02T15:04:15Z | carrier_lost | carrier lost on pc1 enp3s0 |
| 2026-01-02T15:04:17Z | carrier_restored | carrier restored on pc1 enp3s0 |
| 2026-01-02T15:04:45Z | carrier_lost | carrier lost on pc1 enp3s0 |

## 7. Counter Baseline

| Counter | PC1 | PC2 |
| --- | --- | --- |
| link_resets | 0 | 0 |
| rx_align | 0 | 0 |
| rx_crc | 0 | 0 |
| rx_fifo | 0 | 0 |
| rx_frame | 0 | 0 |
| rx_missed | 0 | 0 |

> Counters absent on a side are not exposed by that hardware — absence is not zero.

## 8. Counter Deltas

| Counter | PC1 Δ | PC2 Δ |
| --- | --- | --- |
| link_resets | +4 | +4 |
| rx_align | +0 | +0 |
| rx_crc | +0 | +0 |
| rx_fifo | +0 | +0 |
| rx_frame | +0 | +0 |
| rx_missed | +0 | +0 |

> "unreliable" marks a counter that reset or wrapped mid-run; its delta is not evidence.

## 9. Ping Stability

> Not run: no result was recorded.

## 10. Full-Size Ping

> Not run: no result was recorded.

## 11. TCP Throughput PC1→PC2

> Not run: no TCP result was recorded for this direction.

## 12. TCP Throughput PC2→PC1

> Not run: no TCP result was recorded for this direction.

## 13. Bidirectional Stress

> Not run: no bidirectional result was recorded.

## 14. UDP Loss & Jitter

> Not run: no UDP result was recorded.

## 15. CPU Utilization

> Not run: no throughput test reported CPU utilization.

## 16. Cable Diagnostics

> Not run: cable diagnostics were not requested.

## 17. Monitoring Timeline

| At | Event | Detail |
| --- | --- | --- |
| 2026-01-02T15:04:15Z | carrier_lost | carrier lost on pc1 enp3s0 |
| 2026-01-02T15:04:17Z | carrier_restored | carrier restored on pc1 enp3s0 |
| 2026-01-02T15:04:45Z | carrier_lost | carrier lost on pc1 enp3s0 |

## 18. Findings Detail

- **PHY-01** [physical/failed] The link was down when testing ended.
  - post-test link state reports no carrier on at least one side
- **PHY-03** [physical/failed] The link bounced 4 time(s) during the test.
  - carrier change counter advanced by 4 on the worse side
- **LIM-01** [limitation/marker] Critical evidence is missing — a clean-looking result would not be trustworthy.
  - no TCP throughput result in either direction
- **LIM-03** [limitation/marker] The run was interrupted — the report covers only the tests that completed.
  - partial run (interrupt or abort)

## 19. Recommendations

1. Intermittent link: check connector seating, try a different NIC port, run `--mode soak` to catch drops.
2. Install the missing tools (iperf3/ethtool) and rerun for a conclusive result.
3. Isolation test: same machines with a different cable, then the same cable between different machines.

## 20. Limitations & Unavailable Tests

| Test | Reason |
| --- | --- |
| tcp_throughput | link went down before the TCP phase completed |
| udp_loss | link went down before the UDP phase |

## 21. Configuration Used

- **Role:** pc1
- **Local IP:** 192.168.100.1
- **Peer IP:** 192.168.100.2
- **Interface:** enp3s0
- **Mode:** quick
- **Control port:** 44300
- **iperf3 port:** 44301
- **TCP duration:** 30s
- **UDP duration:** 20s
- **UDP rate:** 800Mbit/s
- **Parallel streams:** 4
- **Ping count:** 500
- **Ping interval:** 20ms
- **TCP repeats:** 1
- **Monitor interval:** 1s
- **Cable test requested:** no
- **Cable test TDR requested:** no
- **Output directory:** .
- **Verbose:** no
- **Non-interactive:** no
- **No sudo:** no
- **No report transfer:** no
- **Allow virtual interface:** no
- **Token auto-generated:** yes

## 22. Tool Versions

| Tool | PC1 | PC2 |
| --- | --- | --- |
| ethtool | 6.7 | 6.7 |
| iperf3 | 3.16 | 3.16 |
| ping | iputils-20240117 | iputils-20240117 |

## 23. Raw Artifact Index

> No raw artifacts were recorded.

