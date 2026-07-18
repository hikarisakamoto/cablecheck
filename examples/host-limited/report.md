# CableCheck Report

## 1. Overall Result

**INCONCLUSIVE** — The collected evidence does not support a verdict about the cable.

## 2. Score & Rule Evidence

- **Score:** n/a (INCONCLUSIVE runs carry no score)
- **Reason:** TCP throughput is below the negotiated link speed.
- **Reason:** CPU was saturated during throughput testing — performance results may be host-limited.

| Rule | Category | Severity | Finding |
| --- | --- | --- | --- |
| PERF-01 | performance | poor | TCP throughput is below the negotiated link speed. |
| HOST-01 | host | marker | CPU was saturated during throughput testing — performance results may be host-limited. |

## 3. Session Info

- **Test ID:** example-host-limited
- **Schema version:** 1.0.0
- **Tool version:** 1.0.0
- **Protocol version:** 1
- **Started:** 2026-01-02T15:04:05Z
- **Finished:** 2026-01-02T15:05:35Z
- **Duration:** 1m30s
- **Mode:** quick
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

> Not run: no result was recorded.

## 11. TCP Throughput PC1→PC2

| Run | Duration | Streams | Sender | Receiver | Retransmits | CoV | Min interval | Max interval |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 30s | 4 | 251.0 Mbit/s | 250.0 Mbit/s | 0 | 1.20% | 247.0 Mbit/s | 252.0 Mbit/s |

## 12. TCP Throughput PC2→PC1

| Run | Duration | Streams | Sender | Receiver | Retransmits | CoV | Min interval | Max interval |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 30s | 4 | 249.0 Mbit/s | 248.0 Mbit/s | 0 | 1.20% | 245.0 Mbit/s | 250.0 Mbit/s |

## 13. Bidirectional Stress

> Not run: no bidirectional result was recorded.

## 14. UDP Loss & Jitter

| Direction | Target | Sender | Receiver | Lost/Total | Loss | Jitter | Out-of-order |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 → pc2 | 800.0 Mbit/s | 799.4 Mbit/s | 799.4 Mbit/s | 0/67926 | 0.00% | 0.11 ms | 0 |
| pc2 → pc1 | 800.0 Mbit/s | 799.1 Mbit/s | 799.1 Mbit/s | 0/67924 | 0.00% | 0.11 ms | 0 |

## 15. CPU Utilization

| Test | Sender CPU | Receiver CPU |
| --- | --- | --- |
| TCP pc1 → pc2 | 98.4% | 42.0% |
| TCP pc2 → pc1 | 98.4% | 42.0% |
| UDP pc1 → pc2 | 12.5% | 9.8% |
| UDP pc2 → pc1 | 12.5% | 9.8% |

## 16. Cable Diagnostics

> Not run: cable diagnostics were not requested.

## 17. Monitoring Timeline

> No monitoring events were recorded during the run.

## 18. Findings Detail

- **PERF-01** [performance/poor] TCP throughput is below the negotiated link speed.
  - pc1->pc2: 250M TCP = 25% of the 1G link
  - pc2->pc1: 248M TCP = 25% of the 1G link
- **HOST-01** [host/marker] CPU was saturated during throughput testing — performance results may be host-limited.
  - max iperf3 CPU utilization 98.4% > 90%

## 19. Recommendations

1. Result appears host-limited: close background load, disable CPU power saving, avoid USB adapters, rerun.
2. Isolation test: same machines with a different cable, then the same cable between different machines.

## 20. Limitations & Unavailable Tests

> None — every planned test ran and no warnings were raised.

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

