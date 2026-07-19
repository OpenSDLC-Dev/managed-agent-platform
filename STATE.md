# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#95](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/95) /
[#110](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/110) — the K8s sandbox killed a
command on its deadline and reported `TimedOut: false`. No plan file (single-PR fix; triage
`needs_plan: false`).

## Tasks

- [x] Root-caused: the pre-deadline liveness probe is itself an in-pod exec, so its answer lands an
      apiserver round trip late and misses the watchdog's kill. Independently confirmed.
- [x] Fixed: the watchdog marks its own kill, the wrapper folds the mark onto the exit line, and
      `classifyTimeout` weighs it alongside a recorded SIGKILL.
- [x] Pinned without a cluster: wrapper and `exitScript` under host `/bin/bash`, plus table tests on
      `classifyTimeout`/`parseExit`. Three mutations each fail exactly their own case.
- [ ] Verifier, dual review, PR green on CI's kind cluster.
