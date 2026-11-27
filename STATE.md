# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [AGENTS.md](./AGENTS.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 42](./docs/plan/42_multi-tenant-activation.md) stages 1–4 cover workspace-scoped resources, execution, browser and CLI credentials, and Gateway accounting. [#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56) remains open for workspace management and activation.

Agents workspace overview for the native Console. See the [private report contract](./docs/DIVERGENCES.md#private-agents-workspace-report).

## Tasks

The [Default upgrade contract](./internal/store/workspaces_migration_test.go) compares effective roles before and after migration and exercises existing credentials and work. The [HTTP grant contract](./internal/api/workspacemembership_test.go) checks live authorization changes. The [A/B contracts](./internal/api/workspacescope_test.go) exercise cross-workspace reads, file mounts, queue and event ownership, and concurrent revocation. The [SQL inventory](./internal/api/scopematrix_test.go) checks resource queries and their parent-derived operations.

The [PostgreSQL contract](./internal/api/agentsoverview_test.go) covers authorization, window boundaries, error recovery, active clocks and chart ranks. Consumer acceptance runs in managed-agent-console.
