# CableCheck

CableCheck is a Linux CLI tool that tests the health, stability, and performance of
an Ethernet cable by coordinating measurements between two directly-connected Linux PCs.

> Documentation stub — completed in the documentation phase. See `docs/` for
> architecture, protocol, report schema, and health-rule references.

## Quick start

```bash
# PC 1
cablecheck run --role pc1 --local-ip 192.168.50.1 --peer-ip 192.168.50.2

# PC 2
cablecheck run --role pc2 --local-ip 192.168.50.2 --peer-ip 192.168.50.1 --token <token shown by PC 1>
```

Build: `make build` (requires Go 1.24+). Runtime dependencies: `iperf3`, `ethtool`, `ping`, `ip`.
