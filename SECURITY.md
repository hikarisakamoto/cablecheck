# Security

## Threat model first, because it changes what counts as a bug

CableCheck is built for one situation: a trusted direct Ethernet cable between two of
your machines, or a trusted isolated LAN. That assumption shapes everything below.

The control channel between PC1 and PC2 is authenticated by a shared session token, but
it is **not encrypted**. The token — a 6-digit code from a cryptographic source by
default, or a 6–128 char string you supply — is a guard against accidentally connecting
to the wrong host on the same segment. It is not protection against someone who can see
or shape your traffic. Treat it like a session guard, not a secret that stands up to a
determined attacker.

So: don't run CableCheck on an untrusted or hostile network. That isn't a limitation
we're planning to fix; it's the design. If you need a link tested over anything you don't
control, CableCheck is the wrong tool.

The [README security assumptions](README.md#security-assumptions) and the
[protocol security model](docs/protocol.md) go deeper on the invariants that hold today —
listener binding, constant-time token comparison, the fixed message catalog, the
frame-length cap, and the checked report transfer.

## Supported versions

Security fixes land on `main`. There are no tagged releases yet; once there are, fixes go
into the newest one and older tags won't be back-patched. Either way, build from `main`
before reporting so we're both looking at the same code.

## Reporting a vulnerability

Report privately. **Please don't open a public issue for a suspected vulnerability.**

- Preferred: GitHub private vulnerability reporting — the **Report a vulnerability**
  button under the repo's **Security** tab
  (github.com/hikarisakamoto/cablecheck → Security → Advisories).
- Fallback: email **[TODO: contact email]**. <!-- TODO: add contact email before publishing -->

Include enough to reproduce:

- the affected release or commit SHA,
- what the issue is and why it matters,
- concrete repro steps, and a minimal command line or capture if you have one.

This is a single-maintainer project, so I can't promise a fixed response time — but a
security report jumps the queue. I'll acknowledge it as soon as I can and keep you posted.
Coordinated disclosure is appreciated — give me a reasonable window to ship a fix before
going public. If you'd like credit, say so and I'll
name you; if you'd rather stay anonymous, that's fine too.

## In scope

Things I'd genuinely want to know about, because they break an invariant that's supposed
to hold:

- The session token leaking anywhere it shouldn't — a `report.json`/`report.md`/
  `summary.txt` field, a log line, raw evidence.
- Process termination signalling the wrong PID or process group (ownership is meant to be
  verified against `/proc` and the euid before anything is signalled).
- Path traversal or escaping the directory you passed to `--output`.
- The 4-byte frame-length decoder being coaxed into over-allocating past its cap.
- The report transfer accepting anything outside its three fixed filenames, or bypassing
  the per-file/total size caps or the SHA-256 verification.
- Getting the opt-in `sudo -n` path (`ethtool --cable-test` / `--cable-test-tdr`) to run
  something it shouldn't, or to prompt interactively mid-test.

## Out of scope

- **The lack of transport encryption.** It's documented above and in the README — a design
  choice for trusted links, not an oversight.
- **Anything that assumes an untrusted or hostile network.** That's outside the supported
  environment, so a finding there isn't a CableCheck bug.
- **Attacks that need privileges the operator already has on their own machine.** If you
  can already run commands as that user or as root, you don't need CableCheck to do damage.
