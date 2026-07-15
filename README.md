# managed-agent-platform

An open-source, self-hostable platform for **long-horizon AI agents**, written in Go.

Run the whole thing on-prem or in your own VPC — **your data and your compute never leave your boundary**.

> **Status: early development.** The platform runs tools end-to-end with a human-in-the-loop gate: the control-plane API, the session event log, the config-driven model-provider layer, the brain orchestration loop, the executor and its Docker sandbox, and permission policies are in place. The real `ant` CLI drives the API and streams a model turn back live (against any Anthropic-protocol endpoint); when the model calls a built-in tool (`bash`/`read`/`write`/`edit`/`glob`/`grep`) the brain checks its permission policy — an `always_allow` tool runs immediately in a per-session container and the turn resumes, while an `always_ask` tool suspends the session with a `requires_action` stop_reason and waits for a `user.tool_confirmation` (allow runs it, deny answers it with an error). The **BYOC worker now runs end to end, including recovery** — a `cmd/worker` process in your own network polls the wire-compatible work API, runs a self-hosted session's tools in a local Docker sandbox, and posts the results back, so tool execution can happen entirely on your compute. If a worker dies mid-run, its stranded work is reclaimed once its lease lapses and another worker re-runs the still-unanswered tools, so an interrupted session recovers rather than hanging. Tracing now spans the process boundary: a session's model turns and the tool runs a BYOC worker executes for it share one OTel trace, so a distributed run reads as a single trace end to end. Work items also take a client-managed metadata patch. The queue-stats endpoint is what remains before v1. See [Roadmap](#roadmap) and [CHANGELOG.md](./CHANGELOG.md).

## Why

Most agent platforms are SaaS: your source code, your prompts, and your tool output all flow through someone else's infrastructure. For enterprises with data-residency, compliance, or air-gap requirements, that's a non-starter.

This project is that platform, self-hosted:

- **Bring your own model.** Providers are config-driven (`protocol` · `model` · `base_url` · `api_key`). The Anthropic-protocol provider works against *any* endpoint speaking Anthropic Messages — a gateway, a proxy, or a self-hosted model. An OpenAI-compatible provider (planned) will cover vLLM and most internal gateways. Nothing hard-codes a vendor endpoint.
- **Bring your own compute.** Sandboxes run on Docker or Kubernetes under your control. Customer-run workers pull work from the platform, so **no inbound network access is required** into your environment.
- **Observability is built in.** OpenTelemetry traces, metrics, and logs over standard OTLP — point it at your existing Jaeger/Tempo/Prometheus stack.

## Relationship to Claude Managed Agents

We take Anthropic's **Claude Managed Agents** as our **reference implementation**: we adopt its domain model and keep our public REST API **wire-compatible** with it, so the real `ant` CLI and the Anthropic SDKs can drive this server unchanged.

This is a deliberate compatibility and design choice, not an attempt to reproduce that product. Where our goals — self-hosting, pluggable model backends, first-class OTel — call for something different, we diverge on purpose and document why.

## Architecture

An agent is three independently-swappable pieces:

| Piece | What it is | Property |
|---|---|---|
| **Session** | An append-only **event log** (Postgres) | The single source of truth. All durable state lives here. |
| **Brain** (harness) | The loop that calls the model and routes tool calls | **Stateless, horizontally scalable.** If it crashes, any fresh brain replays the log and continues. |
| **Sandbox** (hands) | A disposable per-session container that runs tools | *Cattle, not pets.* A dying container is one tool-call error, not a lost session. |

Execution is **fully asynchronous through the event log and a work queue.** The brain never runs tools in-process: it emits `agent.tool_use`, an executor pulls that work, runs it inside a sandbox, and posts the result back; the brain wakes and continues. Platform-managed sandboxes and customer-run (BYOC) workers are the **same pull protocol at two deployment points**.

Two security invariants, adopted from the reference design:

1. **Credentials never reach the sandbox.** Repos are cloned with a token the sandbox never sees; tool credentials are injected at egress.
2. **A session is not a context window.** The harness may replay, slice, or rewind the event log before feeding the model, so context strategy is never baked into an irreversible compaction.

## Roadmap

v1 targets the core loop: `create agent → create environment → create session → send a message → the model calls a tool → an executor runs it in a sandbox → results stream back over SSE → a human approves a gated tool → the session goes idle`.

Progress is tracked in two places, updated in the same PR as every change:

- **[CHANGELOG.md](./CHANGELOG.md)** — what has landed, newest first.
- **[STATE.md](./STATE.md)** — live slice-by-slice delivery status and what's next.

Deferred past v1 (seams reserved, not implemented): secret vaults and egress credential injection, scheduled deployments, memory stores, multi-agent threads, skills, and multi-tenant RBAC/SSO.

## Development

Requires **Go 1.25+** and Docker (the storage and API contract tests start
their own disposable Postgres containers, and the sandbox, shell, toolset, and
executor tests start a disposable `debian:stable-slim` container). A missing daemon is a
hard test failure, not a skip, so the coverage gate cannot be hollowed out.

```bash
go build ./...             # build
go test ./...              # unit + contract tests
go vet ./... && gofmt -l . # lint
```

Contributions are welcome. Please read [CLAUDE.md](./CLAUDE.md) first — it documents the architecture, the non-negotiable design principles, and the working conventions (notably: **never guess at the wire schema**; verify against the real `ant` CLI).

## License

[Apache-2.0](./LICENSE)
