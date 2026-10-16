# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**A status the image's startup chose is no answer**
([plan 67](./docs/plan/67_startup-exit-status.md), #942): every platform script that
reads its exit status runs framed, and a status that came with no frame is the image's
startup's, never the script's answer.

## Tasks

- [x] PR 1 — the sandbox layer: `StartupExitError` and `ExecStatus`, every backend
      site on Docker and Kubernetes with a contract row; the toolset answers the error
      as a tool error
- [ ] PR 2 — the toolset: `fileFault`, the bash tool's `done` marker, ripgrep's install
- [ ] PR 3 — the executor and worker callers; the image contract in
      docs/self-hosted-security.md; the plan's close
