# Contributing to CableCheck

Thanks for pitching in. The most useful contributions are usually:

- A good bug report — `cablecheck doctor` output plus a clear repro.
- Real-world test results from actual links, especially A/B comparisons against a known-good cable.
- Doc fixes.
- Code.

Small, focused changes get reviewed and merged faster than big ones.

This is a single-maintainer project, so please keep that in mind on turnaround.

## Hard constraints

Read these before you write any code. They aren't up for negotiation:

- **Linux only**, `amd64` and `arm64`. No macOS, no Windows.
- **Go 1.26.**
- **Standard library only.** `go.mod` has zero `require`s and there is no `go.sum`. Never add a third-party dependency, and `go mod tidy -diff` must stay clean. A PR that pulls in a dependency will be declined.

## Dev setup

Clone the repo, then install the runtime prerequisites on the machine (both PCs, if you're testing a real link): `iperf3` 3.7+, `ethtool`, `iputils` `ping` (BusyBox `ping` won't work), and `iproute2`. Then build and sanity-check your environment:

```bash
git clone https://github.com/hikarisakamoto/cablecheck
cd cablecheck
make build
./cablecheck doctor
```

`doctor` checks the local environment — required tools, `ping` identity, `iperf3` features, interfaces, passwordless `sudo`, output dir writable — without touching the peer. If it complains, fix that first.

## The gate

Run `make check` before you push. It's the whole bar: `gofmt`, `go vet`, `go mod tidy -diff`, `go test ./...`, the mandatory race run (`go test -race -shuffle=on ./...`), and a build. All of it has to pass — the race/shuffle run in particular, since ordering bugs love to hide behind a clean linear pass.

If your change touches report **output**, regenerate the byte-exact golden files and review the diff before committing:

```bash
go test ./internal/reporting -update
```

Eyeball that diff. An `-update` that changes files you didn't mean to touch is a bug in your change, not a rubber stamp.

## Tests stay hermetic

Tests never hit the real world: no live `ping`/`ip`/`ethtool`/`iperf3`, no network, no real PIDs, no wall-clock sleeps. External tools are faked and time comes from an injected clock. Match the existing patterns rather than reinventing them — [AGENTS.md](AGENTS.md) spells them out.

## Commits and PRs

- [Conventional Commits](https://www.conventionalcommits.org), single-line subject, e.g. `fix: enforce transfer size bounds`.
- One logical change per commit. One focused topic per PR.
- In the PR description, explain the *why*, not just the what, and link the issue it closes.

## Where to look next

- [AGENTS.md](AGENTS.md) — the contract for changing the code (invariants, patterns, gotchas). Read it before non-trivial work.
- [docs/](docs/) — design: architecture, protocol, health rules, report schema.
- [README.md](README.md) — end-user usage.
- [SECURITY.md](SECURITY.md) — CableCheck is for a trusted direct cable or isolated LAN only; don't file vulnerabilities as public issues, report them per this file.
