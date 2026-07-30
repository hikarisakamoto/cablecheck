# CableCheck Report

## 1. Overall Result

**WARNING** — The cable works but shows signs that deserve attention.

## 2. Score & Rule Evidence

- **Score:** 79/100
- **Reason:** UDP jitter above 5 ms on a direct link.
- **Reason:** TCP throughput does not meet the passing policy for this negotiated link speed.
- **Reason:** TCP throughput is unstable across intervals.
- **Reason:** TCP throughput collapsed below 50% of the median in 2 interval(s).
- **Reason:** TCP throughput is asymmetric between the two directions.
- **Reason:** CPU was saturated during throughput testing — performance results may be host-limited.

| Rule | Category | Severity | Finding |
| --- | --- | --- | --- |
| TR-08 | transport | warning | UDP jitter above 5 ms on a direct link. |
| PERF-01 | performance | warning | TCP throughput does not meet the passing policy for this negotiated link speed. |
| PERF-02 | performance | warning | TCP throughput is unstable across intervals. |
| PERF-03 | performance | warning | TCP throughput collapsed below 50% of the median in 2 interval(s). |
| PERF-04 | performance | warning | TCP throughput is asymmetric between the two directions. |
| HOST-01 | host | marker | CPU was saturated during throughput testing — performance results may be host-limited. |

## 3. Session Info

- **Test ID:** ct-20260715-213005-a1b2c3d4
- **Schema version:** 1.3.0
- **Tool version:** 1.0.0
- **Protocol version:** 1
- **Started:** 2026-07-15T21:30:05Z
- **Finished:** 2026-07-15T21:31:35Z
- **Duration:** 1m30s
- **Mode:** standard
- **Partial run:** no

## 4. Machines & Environment

| Side | Hostname | Kernel | OS | NIC | Driver | Speed | Duplex | MTU | MAC | USB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 | alpha | 6.9.1-generic | linux/amd64 | enp3s0 | e1000e | 1000 Mb/s | full | 1500 | aa:bb:cc:00:11:22 | no |
| pc2 | bravo | 6.9.1-generic | linux/amd64 | enp4s0 | e1000e | 1000 Mb/s | full | 1500 | aa:bb:cc:00:33:44 | no |

## 5. Interface & Link Negotiation

| Side | Phase | Speed | Duplex | Autoneg | Link | MDI-X | Partner modes |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 | before | 1000 Mb/s | full | on | yes | on (auto) | 1000baseT/Full |
| pc1 | after | 1000 Mb/s | full | on | yes | on (auto) | 1000baseT/Full |
| pc2 | before | 1000 Mb/s | full | on | yes | on (auto) | 1000baseT/Full |
| pc2 | after | 1000 Mb/s | full | on | yes | on (auto) | 1000baseT/Full |

## 6. Link Events Timeline

> No link events were observed during the run.

## 7. Counter Baseline

| Counter | PC1 | PC2 |
| --- | --- | --- |
| link_resets | 12 | 9 |
| rx_align | 0 | 0 |
| rx_crc | 3 | 0 |
| rx_fifo | 0 | 0 |
| rx_frame | 0 | 0 |
| rx_missed | 0 | 0 |

> Counters absent on a side are not exposed by that hardware — absence is not zero.

## 8. Counter Deltas

| Counter | PC1 Δ | PC2 Δ |
| --- | --- | --- |
| link_resets | +0 | +0 |
| rx_align | +0 | +0 |
| rx_crc | +0 | +0 |
| rx_fifo | +0 | +0 |
| rx_frame | +0 | +0 |
| rx_missed | +0 | +0 |

> "unreliable" marks a counter that reset or wrapped mid-run; its delta is not evidence.

## 9. Ping Stability

| Direction | Sent | Received | Loss | Dup | Errors | RTT min/avg/max/mdev (ms) | Longest gap |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 → pc2 | 500 | 500 | 0.00% | 0 | 0 | 0.18 / 0.21 / 0.35 / 0.02 | 0.0 ms |
| pc2 → pc1 | 500 | 500 | 0.00% | 0 | 0 | 0.18 / 0.21 / 0.35 / 0.02 | 0.0 ms |

## 10. Full-Size Ping

| Direction | Sent | Received | Loss | Dup | Errors | RTT min/avg/max/mdev (ms) | Longest gap |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 → pc2 | 500 | 500 | 0.00% | 0 | 0 | 0.18 / 0.21 / 0.35 / 0.02 | 0.0 ms |
| pc2 → pc1 | 500 | 500 | 0.00% | 0 | 0 | 0.18 / 0.21 / 0.35 / 0.02 | 0.0 ms |

## 11. TCP Throughput PC1→PC2

| Completed trials | Minimum | Lower median | Maximum | Inter-trial CoV |
| --- | --- | --- | --- | --- |
| 2 | 940.0 Mbit/s | 940.0 Mbit/s | 940.0 Mbit/s | 0.00% |

| Run | Duration | Streams | Sender | Receiver | Retransmits | Interval CoV | Min interval | Max interval |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 10s | 1 | 941.0 Mbit/s | 940.0 Mbit/s | 0 | 20.00% | 200.0 Mbit/s | 950.0 Mbit/s |
| 2 | 10s | 1 | 941.0 Mbit/s | 940.0 Mbit/s | 0 | 20.00% | 200.0 Mbit/s | 950.0 Mbit/s |

## 12. TCP Throughput PC2→PC1

| Completed trials | Minimum | Lower median | Maximum | Inter-trial CoV |
| --- | --- | --- | --- | --- |
| 2 | 650.0 Mbit/s | 650.0 Mbit/s | 650.0 Mbit/s | 0.00% |

| Run | Duration | Streams | Sender | Receiver | Retransmits | Interval CoV | Min interval | Max interval |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 10s | 1 | 651.0 Mbit/s | 650.0 Mbit/s | 0 | 20.00% | 200.0 Mbit/s | 660.0 Mbit/s |
| 2 | 10s | 1 | 651.0 Mbit/s | 650.0 Mbit/s | 0 | 20.00% | 200.0 Mbit/s | 660.0 Mbit/s |

## 13. Bidirectional Stress

| Direction | Sender | Receiver | Retransmits |
| --- | --- | --- | --- |
| pc1 → pc2 | 884.0 Mbit/s | 883.5 Mbit/s | 0 |
| pc2 → pc1 | 881.2 Mbit/s | 880.9 Mbit/s | 0 |

## 14. UDP Loss & Jitter

| Direction | Target | Sender | Receiver | Lost/Total | Loss | Jitter | Out-of-order |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 → pc2 | 800.0 Mbit/s | 799.8 Mbit/s | 799.8 Mbit/s | 0/67934 | 0.00% | 6.00 ms | 0 |
| pc2 → pc1 | 800.0 Mbit/s | 799.5 Mbit/s | 799.5 Mbit/s | 0/67934 | 0.00% | 0.11 ms | 0 |

## 15. CPU Utilization

| Test | Sender CPU | Receiver CPU |
| --- | --- | --- |
| TCP pc1 → pc2 | 96.0% | 88.0% |
| TCP pc1 → pc2 | 96.0% | 88.0% |
| TCP pc2 → pc1 | 96.0% | 88.0% |
| TCP pc2 → pc1 | 96.0% | 88.0% |
| Bidirectional | 12.5% | 9.8% |
| UDP pc1 → pc2 | 12.5% | 9.8% |
| UDP pc2 → pc1 | 12.5% | 9.8% |

## 16. Cable Diagnostics

> Not run: cable diagnostics were not requested.

## 17. Monitoring Timeline

> No monitoring events were recorded during the run.

## 18. Findings Detail

- **TR-08** [transport/warning] UDP jitter above 5 ms on a direct link.
  - pc1->pc2: 6.00 ms jitter
- **PERF-01** [performance/warning] TCP throughput does not meet the passing policy for this negotiated link speed.
  - pc2->pc1: 650M TCP = 65% of the 1G link
- **PERF-02** [performance/warning] TCP throughput is unstable across intervals.
  - pc1->pc2: throughput coefficient of variation 20%
  - pc2->pc1: throughput coefficient of variation 20%
- **PERF-03** [performance/warning] TCP throughput collapsed below 50% of the median in 2 interval(s).
  - 2 interval(s) under 50% of the median interval bitrate
- **PERF-04** [performance/warning] TCP throughput is asymmetric between the two directions.
  - pc1->pc2 940M vs pc2->pc1 650M (31% difference)
- **HOST-01** [host/marker] CPU was saturated during throughput testing — performance results may be host-limited.
  - max iperf3 CPU utilization 96.0% > 90%

## 19. Recommendations

1. Result appears host-limited: close background load, disable CPU power saving, avoid USB adapters, rerun. Evidence from this run: max iperf3 CPU utilization 96.0% > 90%.

## 20. Limitations & Unavailable Tests

> None — every planned test ran and no operational warnings were raised.

## 21. Configuration Used

- **Role:** pc1
- **Local IP:** 192.168.100.1
- **Peer IP:** 192.168.100.2
- **Interface:** enp3s0
- **Mode:** standard
- **Control port:** 51999
- **iperf3 port:** 52001
- **TCP duration:** 10s
- **UDP duration:** 10s
- **UDP rate:** 800Mbit/s
- **Parallel streams:** 1
- **Ping count:** 500
- **Ping interval:** 20ms
- **TCP repeats:** 2
- **Monitor interval:** 500ms
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

| File | SHA-256 | Bytes |
| --- | --- | --- |
| raw/01-pc1-ethtool-link-before.txt | 0f4ad9e2cf0f4a1a3b2c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809aa1 | 1832 |
| raw/02-pc1-ip-stats-before.json | 1a2b3c4d5e6f708192a3b4c5d6e7f8090f4ad9e2cf0f4a1a3b2c5d6e7f8091b2 | 2210 |
| raw/03-pc1-iperf3-tcp-pc1-to-pc2.json | 2b3c4d5e6f708192a3b4c5d6e7f8090f4ad9e2cf0f4a1a3b2c5d6e7f8091c3d4 | 9184 |

