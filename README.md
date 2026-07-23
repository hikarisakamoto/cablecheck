# CableCheck

[![Go](https://img.shields.io/github/go-mod/go-version/hikarisakamoto/cablecheck?logo=go&logoColor=white&label=go)](go.mod)
[![License](https://img.shields.io/github/license/hikarisakamoto/cablecheck?color=brightgreen)](LICENSE)
[![Release](https://img.shields.io/github/v/release/hikarisakamoto/cablecheck)](https://github.com/hikarisakamoto/cablecheck/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/hikarisakamoto/cablecheck/total)](https://github.com/hikarisakamoto/cablecheck/releases)
[![Stars](https://img.shields.io/github/stars/hikarisakamoto/cablecheck)](https://github.com/hikarisakamoto/cablecheck/stargazers)
[![Last commit](https://img.shields.io/github/last-commit/hikarisakamoto/cablecheck)](https://github.com/hikarisakamoto/cablecheck/commits/main)
[![Platform](https://img.shields.io/badge/platform-Linux-informational)](#supported-environment)

CableCheck is a Linux command-line tool for testing the health, stability, and performance of an Ethernet link between two directly connected PCs. PC1 coordinates the run and PC2 executes the measurements. Together they inspect link negotiation and NIC counters, watch link state through sysfs, and run bidirectional ping, full-size ping, TCP, UDP, and stress tests with `ping`, `ip`, `ethtool`, and `iperf3`.

You get a rule-based classification (`EXCELLENT`, `GOOD`, `WARNING`, `POOR`, `FAILED`, or `INCONCLUSIVE`) plus a human report, machine-readable JSON, a short text summary, and the raw evidence.

## What CableCheck can and cannot prove

CableCheck can expose symptoms consistent with a bad cable: CRC and framing errors under load, carrier drops, renegotiation, half duplex, reduced negotiated speed, size-dependent packet loss, retransmissions, UDP loss, and cable-test/TDR faults.

What it can't do is prove the cable is the failed component. The measured path also contains both connectors, both NICs or ports, any USB adapters, their drivers, and both hosts. A `POOR` or `FAILED` result is strong evidence that deserves isolation testing, not a component-level verdict. Repeat the test with a known-good cable, then test the original cable between different machines or ports if you need to.

A loopback, bridge, veth, VLAN, wireless, or other virtual interface doesn't exercise a physical cable. CableCheck rejects such an interface by default. If you explicitly allow one for a demo, the result is `INCONCLUSIVE`.

## Supported environment

CableCheck is a Go 1.26, standard-library-only program for Linux. It relies on Linux interface metadata, `/sys/class/net`, iproute2 JSON output, and physical Ethernet NICs. Local and release builds support Linux `amd64` and `arm64`.

Runtime requirements on **both** PCs:

- `iperf3` 3.7 or newer, with JSON support. It's validated through 3.17, and newer releases are accepted because the JSON output is backward-compatible and feature detection reads `iperf3 --help`. Native `--bidir` is used only when both peers support it; otherwise CableCheck uses two coordinated one-way phases.
- `ethtool` for link state and NIC statistics.
- `iputils` `ping`. BusyBox `ping` isn't supported.
- `iproute2` for `ip -j` interface and counter data.
- A physical wired Ethernet interface. USB Ethernet adapters work, but can make performance results host-limited.

Install the packages with your distribution's package manager:

```bash
# Arch Linux
sudo pacman -S iperf3 ethtool iputils iproute2

# Debian / Ubuntu
sudo apt update
sudo apt install iperf3 ethtool iputils-ping iproute2

# Fedora
sudo dnf install iperf3 ethtool iputils iproute
```

Build from source:

```bash
make build
./cablecheck version
```

`make build` produces a single static `cablecheck` binary in the repository root. It has no runtime Go dependency and is portable between Linux machines of the same architecture, so build once and copy the binary to the other PC.

### Install it as a command

The rest of this guide invokes the tool as `cablecheck`. For that to work from any directory, put the binary on your `PATH` — do this on **both** PCs:

```bash
# System-wide (needs root)
sudo install -m 0755 cablecheck /usr/local/bin/cablecheck

# Or per-user, if ~/.local/bin is on your PATH
install -m 0755 cablecheck ~/.local/bin/cablecheck
```

Confirm it's registered:

```bash
cablecheck version
```

If you'd rather not install it, run it straight from the build directory as `./cablecheck`, substituting `./cablecheck` for `cablecheck` in every command below.

## Prepare the direct link

Connect the two Ethernet ports directly. Modern NICs normally handle MDI-X automatically; a crossover cable is usually unnecessary.

First list interface names and assigned addresses:

```bash
ip addr
```

Interface names vary by machine, such as `enp3s0`, `eno1`, or `enx...`. Substitute the real name for `enpXsY` below. Assign temporary addresses on an otherwise unused subnet and bring each interface up:

```bash
# PC1
sudo ip addr add 192.168.50.1/24 dev enpXsY
sudo ip link set dev enpXsY up

# PC2
sudo ip addr add 192.168.50.2/24 dev enpXsY
sudo ip link set dev enpXsY up
```

Run `ip addr` again to confirm that PC1 owns `192.168.50.1` and PC2 owns `192.168.50.2`. CableCheck normally discovers the interface by an exact match on `--local-ip`. Use `--interface enpXsY` only when you want to require a particular interface.

The `ip addr add` assignments are temporary. They disappear on reboot, or you can remove them once testing is done. See [Tear down the link](#tear-down-the-link) below.

## Check the machines first

`doctor` checks the required tools, verifies iputils `ping`, detects the supported `iperf3` features, inventories interfaces, probes passwordless sudo, and checks that the output directory is writable. It doesn't contact the other PC or run a cable test.

```bash
cablecheck doctor
cablecheck doctor --interface enpXsY --output .
```

Warnings don't make `doctor` fail. Any failed check makes it exit 4.

## Run a test

Start PC1 first. It binds only `192.168.50.1`, generates a short 6-digit session token when `--token` is omitted, and prints both the token and a ready-to-copy PC2 command:

```bash
# PC1: coordinator
cablecheck run --role pc1 --local-ip 192.168.50.1 --peer-ip 192.168.50.2
```

Copy the displayed token into the PC2 command:

```bash
# PC2: worker
cablecheck run --role pc2 --local-ip 192.168.50.2 --peer-ip 192.168.50.1 --token <token shown by PC1>
```

To skip looking up the address, name the interface instead and let CableCheck infer `--local-ip` from it, as long as the interface has exactly one IPv4 address:

```bash
# --local-ip inferred from the interface's sole IPv4 address
cablecheck run --role pc1 --interface enpXsY --peer-ip 192.168.50.2
```

PC2 binds its outgoing control connection to its `--local-ip` and retries connection attempts for up to 60 seconds. PC1 accepts only the configured peer IP.

After the authenticated handshake, each terminal waits for its local operator. Type:

```text
start
```

Once both sides have sent `ready`, PC1 sends a synchronized start confirmation with a 3.5-second lead. Each side anchors the countdown to receipt of that message and prints `3… 2… 1… GO`. The interactive commands are `start`, `status`, and `quit`. `--non-interactive` sends readiness automatically.

### Session tokens

The token authenticates the two CableCheck processes for one session. When `--token` is omitted, PC1 generates a random 6-digit code (from a cryptographic source) that's easy to read aloud and retype on PC2. PC2 always requires `--token`. A token you supply yourself must contain 6–128 printable ASCII characters with no whitespace.

The 6-digit code is a session guard for a trusted direct link. It prevents an accidental cross-connection to the wrong process, not a determined attacker. The token is sent in plaintext inside the opening control message, so it isn't encryption and doesn't make an untrusted network safe. CableCheck never writes the token to reports or structured logs.

## Test modes

All modes inspect link settings, take per-peer counter snapshots, and run a sysfs link monitor at a 1-second default interval. TCP uses four parallel streams by default. The UDP rate defaults to 80% of negotiated link speed. When speed is unknown, CableCheck uses 100 Mbit/s and records that limitation.

| Mode | Default workload |
|---|---|
| `quick` | 500-packet stability ping at a requested 20 ms interval in both directions; 100 full-size, don't-fragment pings at 200 ms in both directions; one 30 s TCP run in each direction; one 30 s bidirectional stress run; one 20 s UDP run in each direction; initial/final counters. |
| `standard` | 1,500-packet stability ping at 20 ms in both directions; the same 100-packet full-size test; two 60 s TCP runs in each direction; one 60 s bidirectional stress run; a 30 s UDP run in each direction at the primary rate and another in each direction at half that rate; initial/final counters. |
| `soak` | A one-hour wall-clock budget by default. After one link inspection and initial counters, each cycle takes counters, runs 500-packet ping in both directions, one 60 s TCP run in each direction, and one 20 s UDP run in each direction. `periodic` inserts a 60 s default idle gap between cycles; `continuous` runs cycles back-to-back. Full-size ping and bidirectional stress are not part of soak cycles. |

The native bidirectional test runs both directions together. If either peer lacks `iperf3 --bidir`, the fallback uses two one-way phases and takes twice the configured TCP duration.

Examples:

```bash
# Standard mode
cablecheck run --role pc1 --local-ip 192.168.50.1 --peer-ip 192.168.50.2 --mode standard

# Six-hour continuous soak on PC1; use the printed token on PC2
cablecheck run --role pc1 --local-ip 192.168.50.1 --peer-ip 192.168.50.2 \
  --mode soak --soak-duration 6h --soak-load continuous
```

### `run` flags and defaults

Flags must follow the subcommand. Boolean flags take no separate value; use `--cable-test=false`, not `--cable-test false`.

| Flag | Default and meaning |
|---|---|
| `--role pc1\|pc2` | Required. PC1 coordinates; PC2 works. |
| `--local-ip IPv4` | Required unless `--interface` is given; the local tested-interface address. |
| `--peer-ip IPv4` | Required; the other PC's tested-interface address. |
| `--interface name` | Empty: discover by exact ownership of `--local-ip`. When given, `--local-ip` may be omitted and is inferred from this interface's sole IPv4 address (zero or several IPv4 addresses require an explicit `--local-ip`). |
| `--control-port N` | `44300`; control TCP port, range 1024–65535. It must not equal either iperf port. |
| `--iperf-port N` | `44301`; range 1024–65534. `N+1` is also reserved for bidirectional fallback. |
| `--token string` | Empty on PC1 means generate one; required on PC2. |
| `--mode quick\|standard\|soak` | `quick`. |
| `--tcp-duration D` | `30s` quick; `60s` standard and soak. |
| `--udp-duration D` | `20s` quick and soak; `30s` standard. |
| `--udp-rate rate` | Empty: derive 80% of negotiated speed. Accepts 1M–40G decimal bit rates such as `800M` or `2.5G`. |
| `--parallel-streams N` | `4`; range 1–16. |
| `--soak-duration D` | `1h` in soak mode; invalid outside soak; range 60 s–24 h. |
| `--soak-load periodic\|continuous` | `periodic` in soak mode; invalid outside soak. |
| `--monitor-interval D` | `1s`; range 200 ms–30 s. |
| `--cable-test` | `false`; append `ethtool --cable-test`. |
| `--cable-test-tdr` | `false`; request TDR and imply `--cable-test`. |
| `--verbose` | `false`; show verbose progress/debug logging. |
| `--non-interactive` | `false`; send readiness without waiting for `start`. |
| `--no-sudo` | `false`; never probe or use sudo. |
| `--no-report-transfer` | `false`; disable the PC1-to-PC2 report copy. |
| `--allow-virtual-interface` | `false`; permit loopback/virtual interfaces for demos, yielding `INCONCLUSIVE`. |
| `--output dir` | `.`; existing parent directory for the timestamped report directory; `..` path elements are rejected. |

TCP and UDP durations accept 5 seconds through 10 minutes. Ports must be unprivileged; the control port must not collide with the iperf ports.

## Optional cable diagnostics and privileges

Normal operation is unprivileged. Link inspection, NIC statistics, `ip`, `ping`, `iperf3`, sysfs monitoring, and report generation don't require root.

Only `ethtool --cable-test` and `ethtool --cable-test-tdr` may require root or `CAP_NET_ADMIN`. They're opt-in because a driver may temporarily drop the link while testing. CableCheck uses the current EUID when already root; otherwise it uses only passwordless `sudo -n` and never prompts mid-test. `--no-sudo` skips the sudo probe. Missing privilege or driver support makes the diagnostic unavailable, but doesn't itself mark the cable failed.

```bash
cablecheck run --role pc1 --local-ip 192.168.50.1 --peer-ip 192.168.50.2 --cable-test

# Base cable test plus TDR, if supported by NIC, driver, kernel, and ethtool
cablecheck run --role pc1 --local-ip 192.168.50.1 --peer-ip 192.168.50.2 --cable-test-tdr
```

The cable-test step widens both peers' control-channel idle timeout before the disruptive command. Any link events in that coordinated window are marked self-inflicted and excluded from spontaneous carrier-error evidence.

## Interpreting the result

| Classification | Meaning |
|---|---|
| `EXCELLENT` | Clean physical, transport, and performance evidence with sufficient coverage; score 95–100. |
| `GOOD` | No warning-level fault, but an informational deviation or reduced noncritical coverage prevents `EXCELLENT`; score 80–94. |
| `WARNING` | A warning-level physical, transport, or performance deviation—such as modest counter movement, reduced speed, low loss, or throughput below 70% of link rate; score 51–79. |
| `POOR` | Strong physical evidence or a poor transport/performance result not explained solely by a host limit; score 26–50. |
| `FAILED` | Failure-level physical evidence, such as link down, at least three carrier events, severe CRC movement, an open/short cable-test result, or correlated UDP loss and physical errors; score 0–25. |
| `INCONCLUSIVE` | The evidence cannot support a cable verdict: virtual interface, critical evidence missing, an otherwise clean partial run, poor performance explained by CPU/USB host limitation, or a throughput test that could not reach the peer's data port (firewall/routing on the receiving side). Score is JSON `null`. |

Physical evidence dominates. CPU saturation can soften poor performance to `INCONCLUSIVE`, but it never hides physical `POOR` or `FAILED` evidence. See [docs/health-rules.md](docs/health-rules.md) for the complete thresholds and scoring rules.

## Reports and raw data

Each process creates a private timestamped directory under `--output`. PC1's
authoritative directory has this layout:

```text
cablecheck-report-YYYY-MM-DD_HH-MM-SS/
├── summary.txt       short operator summary
├── report.md         full human-readable report
├── report.json       schema-versioned source record
└── raw/              command output and CableCheck debug evidence
```

PC2 creates its own `raw/` evidence while the run is active and always writes a
local `diagnostic.json` on exit. That file records its role, test ID, mode, IPs,
final state, any error, the reason and detail of a peer abort, PC1's verdict, and
an index of its own raw files. It isn't a full report (no classification) and is
never transferred; it exists so a failed run is debuggable from PC2 alone. By
default PC1 then transfers `report.json`, `report.md`, and `summary.txt` into
PC2's report directory. If transfer is disabled or fails, PC2 keeps its local
raw data and writes a local summary fallback instead. Since `raw/` and
`diagnostic.json` are never transferred, inspect both machines' local report
directories when diagnosing parser or driver behavior.

The transfer manifest carries each file's size and SHA-256. PC2 accepts only the three fixed filenames, caps each file at 8 MiB and the set at 16 MiB, writes to a `.part` file, verifies size and digest, then renames it. A failed file is retried once. Transfer failure is a warning and doesn't change the health classification or exit code. Set `--no-report-transfer` on either peer to disable or decline transfer.

Re-render Markdown and text from a saved JSON record without re-evaluating its verdict:

```bash
cablecheck report cablecheck-report-2026-07-19_12-00-00/report.json
cablecheck report --output /existing/output/dir path/to/report.json
```

See [docs/report-schema.md](docs/report-schema.md) for the JSON contract and the committed [healthy example](examples/healthy/report.json).

## Tear down the link

CableCheck cleans up after **itself** automatically, on both a normal finish and Ctrl-C. It stops every `iperf3` server it started, terminates its own child processes (by tracked PID and process group, never with a blanket `pkill`), releases its control and `iperf3` ports, and removes its temporary run state under `$XDG_RUNTIME_DIR/cablecheck` (or `/tmp`). Report directories are deliverables, so they're kept.

The only thing you undo by hand is the temporary network configuration you added in [Prepare the direct link](#prepare-the-direct-link). Reverse those two steps on **each** PC: remove the address, and set the interface back down if you brought it up only for this test.

```bash
# PC1
sudo ip addr del 192.168.50.1/24 dev enpXsY
sudo ip link set dev enpXsY down

# PC2
sudo ip addr del 192.168.50.2/24 dev enpXsY
sudo ip link set dev enpXsY down
```

Substitute the real interface name for `enpXsY`. Skip the `ip link set ... down` step if the interface was already up and in use before testing. A reboot also clears the temporary address if you prefer not to remove it manually.

If a run was killed uncleanly (for example `kill -9`) and left an `iperf3` server or stale run state behind, the next run's preflight detects the leftover. It verifies ownership against `/proc` first, then fails with guidance to clear it rather than touching an unrelated process.

## Compare with a known-good cable

Use controlled substitutions:

1. Save the original report and raw directories.
2. Keep both PCs, NIC ports, IP settings, mode, and test parameters unchanged.
3. Replace only the cable with a known-good Cat5e/Cat6 cable and rerun.
4. Compare negotiated speed, CRC/framing and carrier deltas, ping/full-size loss, retransmissions, UDP loss, and the findings list.
5. If the symptom persists, move the test to different NIC ports or different PCs before blaming the original cable.

This A/B comparison is much stronger than an isolated throughput number.

## Common false positives

- **Host-limited throughput:** CPU saturation, interrupt handling, memory pressure, or another workload can hold TCP well below line rate with clean physical counters.
- **USB Ethernet adapters:** the USB bus, adapter chipset, thermals, or driver can cap or destabilize throughput. CableCheck records USB attachment as host-limitation evidence.
- **Power saving and frequency scaling:** CPU or NIC power management can produce uneven throughput and latency spikes.
- **Self-inflicted UDP saturation:** an explicit rate above 95% of known link speed is recorded as near-saturation, and its loss isn't used as cable evidence.
- **MTU mismatch:** don't-fragment ping errors point to configuration, not directly to the cable.
- **Unsupported or missing counters:** absence means “not measured,” not zero errors. Missing critical evidence can make the result `INCONCLUSIVE`.

## Security assumptions

CableCheck is for a trusted direct cable or trusted isolated LAN only. The control protocol is authenticated by the shared token but isn't encrypted. Don't run it on an untrusted or hostile network.

PC1 binds the control listener only to the supplied `--local-ip`, never `0.0.0.0`, and silently rejects connections whose source IP isn't `--peer-ip`. The token is compared in constant time and omitted from reports and logs. Protocol payloads are decoded into a fixed catalog of structs and test operations, with no arbitrary type or command deserialization. These safeguards don't replace transport encryption.

## Exit codes

| Code | Meaning |
|---:|---|
| 0 | Completed with `GOOD` or `EXCELLENT`. |
| 1 | Completed with `WARNING`. |
| 2 | Completed with `POOR` or `FAILED`. |
| 3 | Completed with `INCONCLUSIVE`. |
| 4 | Configuration or dependency failure, including `doctor` failure and a PC2 token/handshake rejection. |
| 5 | Peer or orchestration failure, including peer abort, disconnect, request timeout, plan failure, or a coordinator that cannot establish a valid handshake. |
| 6 | Local interrupt: Ctrl+C/SIGTERM, `quit`, or interactive stdin EOF. |
| 7 | Internal error, including report persistence failure or an invalid internal verdict. |

Ctrl+C attempts to preserve completed measurements in a partial PC1 report. The interrupted side exits 6, and the other side normally observes a peer abort and exits 5.

## Troubleshooting

**“required tool not found” or `doctor` reports FAIL**  
Install the exact package shown by `cablecheck doctor`. Confirm that `ping -V` identifies iputils and that `iperf3 --version` is 3.7 or newer.

**Local IP is not assigned / interface not found**  
Run `ip -j addr`, check the exact address and interface name, bring the interface up, and reapply the temporary address. `--interface` doesn't bypass the requirement that the interface own `--local-ip`.

**Interface is down or no carrier appears**  
Check both connectors and NIC LEDs, run `ip link show dev enpXsY`, and verify that both interfaces are up before starting CableCheck.

**PC2 seems to hang at “ready — waiting for peer”**  
This is normal. The synchronized start waits until *both* sides are ready. Type `start` in each terminal, or pass `--non-interactive` to auto-ready. PC2 proceeds the moment PC1 confirms the start.

**PC2 cannot connect**  
Start PC1 first, confirm both control commands use mirrored IPs, verify TCP port 44300 is not filtered, and check that each machine can reach the other's direct-link address. PC2 retries for up to 60 seconds.

**Throughput test cannot connect / result is INCONCLUSIVE citing a firewall**  
The control channel only needs the dialing side to reach the listener. The throughput tests also need the *receiving* side to accept an inbound iperf3 connection on the data ports (`--iperf-port` and base+1). A host firewall (ufw, firewalld) that denies inbound traffic drops those connections even though the control channel worked, so the throughput test is recorded as `INCONCLUSIVE` with a firewall recommendation instead of a cable verdict. Allow the peer on both machines (for a trusted direct link, `sudo ufw allow from <peer-ip>`) or open the data ports, then rerun.

**Token rejected**  
Copy the current token printed by PC1 exactly. Restart PC2 with that token. PC1 allows three wrong-token handshake attempts before it exits 5.

**Port already in use**  
Choose a free `--control-port` and `--iperf-port` on both peers. CableCheck requires both the iperf base port and base+1 to be free.

**Cable test unavailable**  
This is expected on NICs or drivers without ethtool netlink cable-test support. Run without `--no-sudo`, configure passwordless sudo if appropriate, or run CableCheck as root only when you explicitly need cable diagnostics. Normal tests do not need root.

**Low throughput with clean counters**  
Close other workloads, watch CPU utilization, try `--parallel-streams 1`, disable aggressive power saving temporarily, and repeat without USB adapters where possible. Compare against a known-good cable before assigning blame.

**Report did not arrive on PC2**  
Check whether either side used `--no-report-transfer`, then inspect both `raw/cablecheck-*.log` files. PC1's report remains authoritative even when transfer is declined or fails.

## Loopback end-to-end demo

The repository includes stub `iperf3` and `ethtool` tools and a scripted two-peer loopback demo:

```bash
make demo-e2e
# or, after make build:
./scripts/demo-e2e.sh
```

The script runs PC1 on `127.0.0.1` and PC2 on `127.0.0.2` with `--allow-virtual-interface --non-interactive`, checks both report directories, verifies matching SHA-256 hashes for the transferred `report.json`, and tests offline report regeneration. Each CableCheck peer exits 3 because loopback correctly forces `INCONCLUSIVE`. The wrapper script treats those expected peer exits as success and exits 0.

## Further documentation

- [Architecture](docs/architecture.md)
- [Control protocol](docs/protocol.md)
- [Report schema](docs/report-schema.md)
- [Health rules](docs/health-rules.md)

CableCheck is licensed under GPL-3.0. See [LICENSE](LICENSE).
