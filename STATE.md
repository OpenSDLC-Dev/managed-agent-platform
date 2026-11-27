# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [AGENTS.md](./AGENTS.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

Agents workspace overview for the native Console. See the [private report contract](./docs/DIVERGENCES.md#private-agents-workspace-report).

## Tasks

The [PostgreSQL contract](./internal/api/agentsoverview_test.go) covers authorization, window boundaries, error recovery, active clocks and chart ranks. Consumer acceptance runs in managed-agent-console.
