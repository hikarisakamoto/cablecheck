## What and why

<!-- One or two sentences: what changes, and what problem it solves. -->

Closes #

## Checklist

- [ ] `make check` passes locally (fmt, vet, `go mod tidy -diff`, tests, `-race`, build)
- [ ] No new third-party dependencies — standard library only, `go.mod` still has zero requires
- [ ] Tests added or updated, and still hermetic (no real ping/ip/ethtool/iperf3, no network, no wall-clock sleeps)
- [ ] If report output changed: golden files regenerated with `go test ./internal/reporting -update` and the diff reviewed
- [ ] `README.md` / `docs/` updated if a flag or user-visible behaviour changed
- [ ] Commits follow Conventional Commits, one logical change each
