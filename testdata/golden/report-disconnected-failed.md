# CableCheck Report

## 1. Overall Result

**FAILED** — The cable failed critical tests and must be replaced.

## 2. Score & Rule Evidence

- **Score:** 25/100
- **Reason:** The link was down when testing ended.
- **Reason:** The link bounced 4 time(s) during the test.
- **Reason:** Critical evidence is missing — a clean-looking result would not be trustworthy.
- **Reason:** Some planned measurements could not run — coverage is reduced.
- **Reason:** The run was interrupted — the report covers only the tests that completed.

| Rule | Category | Severity | Finding |
| --- | --- | --- | --- |
| PHY-01 | physical | failed | The link was down when testing ended. |
| PHY-03 | physical | failed | The link bounced 4 time(s) during the test. |
| LIM-01 | limitation | marker | Critical evidence is missing — a clean-looking result would not be trustworthy. |
| LIM-02 | limitation | marker | Some planned measurements could not run — coverage is reduced. |
| LIM-03 | limitation | marker | The run was interrupted — the report covers only the tests that completed. |

## 3. Session Info

- **Test ID:** ct-20260715-213005-a1b2c3d4
- **Schema version:** 1.2.0
- **Tool version:** 1.0.0
- **Protocol version:** 1
- **Started:** 2026-07-15T21:30:05Z
- **Finished:** 2026-07-15T21:31:35Z
- **Duration:** 1m30s
- **Mode:** standard
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
| 2026-07-15T21:30:15Z | carrier_lost | carrier lost on pc1 enp3s0 |
| 2026-07-15T21:30:17Z | carrier_restored | carrier restored on pc1 enp3s0 |
| 2026-07-15T21:30:45Z | carrier_lost | carrier lost on pc1 enp3s0 |

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
| link_resets | +4 | +0 |
| rx_align | +0 | +0 |
| rx_crc | +0 | +0 |
| rx_fifo | +0 | +0 |
| rx_frame | +0 | +0 |
| rx_missed | +0 | +0 |

> "unreliable" marks a counter that reset or wrapped mid-run; its delta is not evidence.

## 9. Ping Stability

> Not run: link went down before ping completed

## 10. Full-Size Ping

> Not run: link went down before full-size ping

## 11. TCP Throughput PC1→PC2

> Not run: link went down before TCP throughput

## 12. TCP Throughput PC2→PC1

> Not run: link went down before TCP throughput

## 13. Bidirectional Stress

> Not run: link went down before bidirectional stress

## 14. UDP Loss & Jitter

> Not run: link went down before UDP testing

## 15. CPU Utilization

> Not run: no throughput test reported CPU utilization.

## 16. Cable Diagnostics

> Not run: cable diagnostics were not requested.

## 17. Monitoring Timeline

| At | Event | Detail |
| --- | --- | --- |
| 2026-07-15T21:30:15Z | carrier_lost | carrier lost on pc1 enp3s0 |
| 2026-07-15T21:30:17Z | carrier_restored | carrier restored on pc1 enp3s0 |
| 2026-07-15T21:30:45Z | carrier_lost | carrier lost on pc1 enp3s0 |

## 18. Findings Detail

- **PHY-01** [physical/failed] The link was down when testing ended.
  - post-test link state reports no carrier on at least one side
- **PHY-03** [physical/failed] The link bounced 4 time(s) during the test.
  - carrier change counter advanced by 4 on the worse side
- **LIM-01** [limitation/marker] Critical evidence is missing — a clean-looking result would not be trustworthy.
  - no TCP throughput result in either direction
- **LIM-02** [limitation/marker] Some planned measurements could not run — coverage is reduced.
  - test "ping" could not run
  - test "full_size_ping" could not run
  - test "bidirectional" could not run
  - test "udp" could not run
- **LIM-03** [limitation/marker] The run was interrupted — the report covers only the tests that completed.
  - partial run (interrupt or abort)

## 19. Recommendations

1. Intermittent link: check connector seating, try a different NIC port, run `--mode soak` to catch drops.
2. Install the missing tools (iperf3/ethtool) and rerun for a conclusive result.
3. Isolation test: same machines with a different cable, then the same cable between different machines.

## 20. Limitations & Unavailable Tests

| Test | Reason |
| --- | --- |
| ping | link went down before ping completed |
| full_size_ping | link went down before full-size ping |
| tcp | link went down before TCP throughput |
| bidirectional | link went down before bidirectional stress |
| udp | link went down before UDP testing |

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

