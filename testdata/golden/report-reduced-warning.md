# CableCheck Report

## 1. Overall Result

**WARNING** — The cable works but shows signs that deserve attention.

## 2. Score & Rule Evidence

- **Score:** 79/100
- **Reason:** The link negotiated 100M although both NICs support 1G. Possible causes: cable wiring or a damaged pair (1000BASE-T needs all four pairs), NIC or driver configuration.

| Rule | Category | Severity | Finding |
| --- | --- | --- | --- |
| PHY-06 | physical | warning | The link negotiated 100M although both NICs support 1G. Possible causes: cable wiring or a damaged pair (1000BASE-T needs all four pairs), NIC or driver configuration. |

## 3. Session Info

- **Test ID:** ct-20260715-213005-a1b2c3d4
- **Schema version:** 1.0.0
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
| pc1 | alpha | 6.9.1-generic | linux/amd64 | enp3s0 | e1000e | 100 Mb/s | full | 1500 | aa:bb:cc:00:11:22 | no |
| pc2 | bravo | 6.9.1-generic | linux/amd64 | enp4s0 | e1000e | 100 Mb/s | full | 1500 | aa:bb:cc:00:33:44 | no |

## 5. Interface & Link Negotiation

| Side | Phase | Speed | Duplex | Autoneg | Link | MDI-X | Partner modes |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 | before | 100 Mb/s | full | on | yes | on (auto) | 100baseT/Full, 1000baseT/Full |
| pc1 | after | 100 Mb/s | full | on | yes | on (auto) | 100baseT/Full, 1000baseT/Full |
| pc2 | before | 100 Mb/s | full | on | yes | on (auto) | 100baseT/Full, 1000baseT/Full |
| pc2 | after | 100 Mb/s | full | on | yes | on (auto) | 100baseT/Full, 1000baseT/Full |

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

| Run | Duration | Streams | Sender | Receiver | Retransmits | CoV | Min interval | Max interval |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 10s | 1 | 94.1 Mbit/s | 94.1 Mbit/s | 0 | 1.20% | 93.8 Mbit/s | 94.3 Mbit/s |

## 12. TCP Throughput PC2→PC1

| Run | Duration | Streams | Sender | Receiver | Retransmits | CoV | Min interval | Max interval |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 10s | 1 | 93.9 Mbit/s | 93.9 Mbit/s | 0 | 1.20% | 93.6 Mbit/s | 94.1 Mbit/s |

## 13. Bidirectional Stress

| Direction | Sender | Receiver | Retransmits |
| --- | --- | --- | --- |
| pc1 → pc2 | 88.4 Mbit/s | 88.3 Mbit/s | 0 |
| pc2 → pc1 | 88.1 Mbit/s | 88.1 Mbit/s | 0 |

## 14. UDP Loss & Jitter

| Direction | Target | Sender | Receiver | Lost/Total | Loss | Jitter | Out-of-order |
| --- | --- | --- | --- | --- | --- | --- | --- |
| pc1 → pc2 | 80.0 Mbit/s | 80.0 Mbit/s | 80.0 Mbit/s | 0/6793 | 0.00% | 0.11 ms | 0 |
| pc2 → pc1 | 80.0 Mbit/s | 80.0 Mbit/s | 80.0 Mbit/s | 0/6793 | 0.00% | 0.11 ms | 0 |

## 15. CPU Utilization

| Test | Sender CPU | Receiver CPU |
| --- | --- | --- |
| TCP pc1 → pc2 | 12.5% | 9.8% |
| TCP pc2 → pc1 | 12.5% | 9.8% |
| Bidirectional | 12.5% | 9.8% |
| UDP pc1 → pc2 | 12.5% | 9.8% |
| UDP pc2 → pc1 | 12.5% | 9.8% |

## 16. Cable Diagnostics

> Not run: cable diagnostics were not requested.

## 17. Monitoring Timeline

> No monitoring events were recorded during the run.

## 18. Findings Detail

- **PHY-06** [physical/warning] The link negotiated 100M although both NICs support 1G. Possible causes: cable wiring or a damaged pair (1000BASE-T needs all four pairs), NIC or driver configuration.
  - negotiated 100M < expected 1G

## 19. Recommendations

1. Reduced link speed: 1000BASE-T needs all four pairs — test with another cable; verify both NICs advertise 1000 Mb/s (`ethtool <if>`).

## 20. Limitations & Unavailable Tests

> None — every planned test ran and no warnings were raised.

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
- **UDP rate:** 80Mbit/s
- **Parallel streams:** 1
- **Ping count:** 500
- **Ping interval:** 20ms
- **TCP repeats:** 1
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

