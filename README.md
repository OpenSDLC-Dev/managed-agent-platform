# managed-agent-platform

An open-source, self-hostable platform for **long-horizon AI agents**, written in Go.

Run the whole thing on-prem or in your own VPC — **your data and your compute never leave your boundary**.

> **Status: the v1 loop is complete.** The wire-compatible control-plane API, the append-only session event log with SSE streaming, config-driven model providers (Anthropic-protocol and OpenAI-compatible), the brain orchestration loop, tool execution in per-session Docker or Kubernetes sandboxes, and permission policies with human-in-the-loop approval all work end to end. A BYOC worker runs a self-hosted session's tools on your own compute over the wire-compatible work API, with dead-worker recovery and one OTel trace across the process boundary. The real `ant` CLI — including `ant beta:worker` — drives all of it unchanged. Deploy locally with [docker-compose](./deploy/compose) or to Kubernetes with the [Helm chart](./deploy/helm); see [CHANGELOG.md](./CHANGELOG.md) for what has landed and the [issue tracker](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues) for what's next.

## Why

Most agent platforms are SaaS: your source code, your prompts, and your tool output all flow through someone else's infrastructure. For enterprises with data-residency, compliance, or air-gap requirements, that's a non-starter.

This project is that platform, self-hosted:

- **Bring your own model.** Providers are config-driven (`protocol` · `model` · `base_url` · `api_key`). The Anthropic-protocol provider works against *any* endpoint speaking Anthropic Messages — a gateway, a proxy, or a self-hosted model — and an OpenAI-compatible provider covers OpenAI, vLLM, and most internal gateways. Nothing hard-codes a vendor endpoint.
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

v1 delivered the core loop: `create agent → create environment → create session → send a message → the model calls a tool → an executor runs it in a sandbox → results stream back over SSE → a human approves a gated tool → the session goes idle`.

Progress is tracked in:

- **[CHANGELOG.md](./CHANGELOG.md)** — what has landed, newest first.
- **[GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues)** — the backlog and open questions.
- **[STATE.md](./STATE.md)** — the current snapshot; the full delivery history lives in [docs/HISTORY.md](./docs/HISTORY.md).

Deferred past v1 (seams reserved, not implemented — each tracked as an issue): secret vaults and egress credential injection, scheduled deployments, memory stores, multi-agent threads, skills, and multi-tenant RBAC/SSO.

## Development

Requires **Go 1.26+** and Docker (the storage and API contract tests start
their own disposable Postgres containers, and the sandbox, shell, toolset, and
executor tests start a disposable `debian:stable-slim` container). The Kubernetes
sandbox provider's contract test additionally needs a cluster — a local
[kind](https://kind.sigs.k8s.io) cluster works, and CI provisions one. A missing
daemon or cluster is a hard test failure, not a skip, so the coverage gate cannot
be hollowed out.

```bash
go build ./...             # build
go test ./...              # unit + contract tests
go vet ./... && gofmt -l . # lint
```

**Run the platform locally** with the docker-compose stack — controlplane, brain, and executor against a bundled Postgres (and an optional Jaeger):

```bash
cd deploy/compose
cp .env.example .env          # set CONTROLPLANE_API_KEY
docker compose up --build     # control plane on http://localhost:8080 (loopback)
```

Then drive it with the real CLI: `ANTHROPIC_API_KEY=<key> ant --base-url http://localhost:8080 beta:agents list` (management commands take `--base-url` explicitly; they ignore `ANTHROPIC_BASE_URL`, which only the worker/auth subcommands honor). The stack idles until you point the brain at your model endpoint (copy `model-providers.example.json` and set `MODEL_PROVIDERS_FILE`). See [`deploy/compose/README.md`](./deploy/compose/README.md) for details; production deploys use the [Helm chart](./deploy/helm).

Contributions are welcome. Please read [CLAUDE.md](./CLAUDE.md) first — it documents the architecture, the non-negotiable design principles, and the working conventions (notably: **never guess at the wire schema**; verify against the real `ant` CLI).

## License

[Apache-2.0](./LICENSE)
