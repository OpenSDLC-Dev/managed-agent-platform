# managed-agent-platform

An open-source, self-hostable platform for **long-horizon AI agents**, written in Go.

Run the whole thing on-prem or in your own VPC — **your data and your compute never leave your boundary**.

> **Status: v0.2.0 — the v1 loop is complete, and releases now ship as artifacts.** A `v*` tag publishes container images and the Helm chart to GHCR, worker binaries and clamped release notes to the GitHub Release ([docs/RELEASING.md](./docs/RELEASING.md)); the changelog assembles from per-PR fragments. The wire-compatible control-plane API, the append-only session event log with SSE streaming, config-driven model providers (Anthropic-protocol and OpenAI-compatible), the brain orchestration loop, tool execution in per-session Docker or Kubernetes sandboxes, and permission policies with human-in-the-loop approval all work end to end. The `agent_toolset_20260401` toolset is complete: `web_search` (Tavily-protocol) and `web_fetch` (Jina-Reader-protocol) execute in the platform executor on both deployment modes behind config-driven backends. A BYOC worker runs a self-hosted session's tools on your own compute over the wire-compatible work API, with dead-worker recovery and one OTel trace across the process boundary. Session **outcomes** run the loop the reference docs describe: `user.define_outcome` with a text or file rubric, a platform-provisioned grader scoring each work cycle — deliverables harvested from `/mnt/session/outputs/` into the Files API — and revision feedback up to `max_iterations`, verified live against the reference doc's own DCF example. Sandboxes now have a full lifecycle: an executor-resident reaper destroys the sandboxes of deleted, archived and terminated sessions, and an idle session past a configurable TTL has its workspace checkpointed to object storage and restored — files, shell state and harvested deliverables intact — when the next message arrives (plan 24, verified end to end on both backends). A session can mount **GitHub repositories** as well as files: the executor clones each one platform-side with go-git and lands the checkout in the sandbox, so the write-only token never enters the container the agent controls. The real `ant` CLI — including `ant beta:worker` — drives all of it unchanged. Deploy locally with [docker-compose](./deploy/compose) or to Kubernetes with the [Helm chart](./deploy/helm); see [CHANGELOG.md](./CHANGELOG.md) for what has landed and the [issue tracker](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues) for what's next.

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

Self-hosting means you own the infrastructure these run on. The **[self-hosted shared-responsibility model](./docs/self-hosted-security.md)** draws the line: what the platform enforces in code (credential isolation, scoped auth, fail-closed egress, and the sandbox's own cgroup limits and capability drops — on by default) versus what you configure (the sandbox image, egress policy for non-`limited` environments, a hardened container runtime, environment-key rotation).

For Google Cloud specifically, **[docs/deploy-gcp.md](./docs/deploy-gcp.md)** is the deployment guide — GKE with Cloud SQL, Cloud Storage and Cloud KMS, the settings that are required rather than recommended (`podPidsLimit`, TLS in front of the control plane), sandbox-pool sizing from measured numbers, the gVisor boundary, and the teardown step `terraform destroy` does not do. The Terraform it describes is [deploy/gcp/](./deploy/gcp/). Both deployment shapes have been stood up and accepted on real GKE — the bundled-services one and the Google-managed one, the latter with the platform's database role held outside `cloudsqlsuperuser` and every backing service reached with the deployment's own credential rather than the operator's; the two acceptance records are in [docs/HISTORY.md](./docs/HISTORY.md). Production *shape*, not a readiness verdict: the guide names what it does not establish.

## Roadmap

v1 delivered the core loop: `create agent → create environment → create session → send a message → the model calls a tool → an executor runs it in a sandbox → results stream back over SSE → a human approves a gated tool → the session goes idle`.

Progress is tracked in:

- **[CHANGELOG.md](./CHANGELOG.md)** — landed work, newest first; new changes accumulate as fragments in [changelog.d/](./changelog.d/) until a release folds them in, and released sections archive to [docs/changelog/](./docs/changelog/) behind index stubs post-release.
- **[GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues)** — the backlog and open questions.
- **[STATE.md](./STATE.md)** — the active work and its task progress. The as-built system is [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md); acceptance and decision records are [docs/HISTORY.md](./docs/HISTORY.md) (older months archive to [docs/history/](./docs/history/)).

Deferred past v1 (seams reserved, not implemented — each tracked as an issue): scheduled deployments, memory stores, multi-agent threads, and multi-tenant RBAC/SSO. Already landed, by contrast: **Vaults** ([#50](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/50), plan 12) are complete: the `/v1/vaults` and nested credentials management surface — cipher-sealed write-only secrets, the full `mcp_oauth`/`static_bearer`/`environment_variable` auth union, and the live `mcp_oauth_validate` probe — is wire-complete, sessions attach vaults at create time (`vault_ids`), and the egress gate is live end-to-end on both backends (a Docker gate-pair, a K8s native sidecar) — `limited` means only `allowed_hosts` through the per-session gate, with vault placeholders substituted on plain-HTTP egress (in-sandbox HTTPS keeps its placeholders until [#166](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/166)). The end-to-end acceptance passed (real `ant` CLI + gated sandbox, credential substitution and both revocation halves proven; docs/history/2026-07.md). BYOC gate delivery ([#165](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/165)) and TLS-terminating in-sandbox substitution (#166) are the split-out follow-ons. Skills ([#54](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/54)) are complete: the `/v1/skills` registry over object storage, the anthropic prebuilt-skills import, sandbox materialization on both execution halves, and Level-1 metadata injection into the system prompt by the brain — the whole chain exercised end to end by the `skill-answer` eval, whose passphrase lives only in the materialized skill file so a correct answer requires every link. The **Files half** of ([#55](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/55)) is likewise complete: the `/v1/files` registry over object storage, session `resources[]` file mounts, and streaming materialization of those mounts into sandboxes on both execution halves — the platform executor straight from object storage, the BYOC worker over an environment-scoped environment-key content lane — proven by the `file-answer` eval. The **git half** (plan 25) is complete too, closing #55: `github_repository` session resources with sealed write-only tokens, live token rotation and lifetime attachment, and the clone itself — the executor opens the sealed token and clones with go-git **platform-side**, so the token never enters the sandbox and the agent's checkout carries a token-free `.git/config`; the tree ships in over the existing sandbox write path, extraction stages and renames so a half-written checkout is never trusted, a clone that fails surfaces as a `session.error` and leaves the session running, and the brain tells the model where its checkouts are. Repository materialization on **BYOC** compute is the deliberate exception, deferred to [#322](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/322): a self_hosted session accepts the resource and mounts nothing, and is not told otherwise.

## Development

Requires **Go 1.26+** and Docker (the storage and API contract tests start
their own disposable Postgres containers, and the sandbox, shell, toolset, and
executor tests start a disposable `debian:stable-slim` container). The Kubernetes
sandbox provider's contract test additionally needs a cluster — a local
[kind](https://kind.sigs.k8s.io) cluster works, and CI provisions one. A missing
daemon or cluster is a hard test failure, not a skip, so the coverage gate cannot
be hollowed out.

```bash
make build                 # build (go build ./...)
make test                  # unit + contract tests (go test -count=1, with coverage profile)
make vet fmt-check         # lint
make verify                # the whole Go gate (CI additionally runs its helm, terraform and compose jobs)
make eval                  # RUN_EVALS=1: the live end-to-end eval suite (real model + sandboxes)
```

Tests come in tiers. The first two run on every PR and call no model; the rest drive a
real endpoint, so they cost money and are **opt-in by an environment variable** — a
configured `.env` supplies the endpoint, never the consent to spend it on. Once opted in,
missing configuration **fails** rather than skipping: a safety net that quietly skips
itself when its credentials rot is not a safety net.

| Tier | Opt-in | What it proves |
|---|---|---|
| Unit & contract | — | logic, wire shapes, scripted provider streams |
| Dependency integration | — | real Postgres, Docker, and Kubernetes (hard-fail without them) |
| Live-model contract | `RUN_LIVE_MODEL_TESTS=1` | one real turn against your endpoint, through the adapter whose protocol it speaks (the other adapter's test skips) |
| Live web backends | `RUN_LIVE_WEB_TESTS=1` | one real Tavily search and one real Jina Reader fetch through the web-tool adapters (`TAVILY_API_KEY` / `JINA_API_KEY`) |
| Live Cloud Storage | `RUN_LIVE_GCS_TESTS=1` | the shared object-storage contract suite against a real GCS bucket (`GCS_BUCKET`) through the GCS-native backend, authenticating with Application Default Credentials — the only tier that exercises that client's production read transport, which the hermetic `fake-gcs-server` cannot serve |
| Live acceptance | `RUN_LIVE_ACCEPTANCE_TESTS=1` | the reference doc's [define-outcomes example](https://platform.claude.com/docs/en/managed-agents/define-outcomes) end-to-end against an externally running stack through the typed Go SDK — configured by `ACCEPTANCE_BASE_URL` / `ACCEPTANCE_API_KEY` / `ACCEPTANCE_MODEL` from the environment only, never `.env` (its deterministic scripted-model rehearsal runs in the default tier) |
| Live-system evals | `RUN_EVALS=1` (`make eval`) | whole sessions: API → brain → real model → sandbox → SSE, deterministically graded. [Fourteen regression tasks](./docs/plan/02_evals-system.md) spanning the built-in toolset, permission allow/deny, single- and multi-turn, skill injection, and outcome grading; results land in `evals/artifacts/`. CI also runs this tier daily on a schedule ([`evals.yml`](./.github/workflows/evals.yml)), reading the same four variables from the `evals` deployment environment's secrets — and failing rather than skipping while they are unset |

Configure the endpoint once in a gitignored repo-root `.env` — `MODEL_PROTOCOL`
(`anthropic`|`openai`), `MODEL_BASE_URL`, `MODEL_API_KEY`, `MODEL_ID`, plus
`TAVILY_API_KEY` / `JINA_API_KEY` for the web-backend tier and `GCS_BUCKET` for the Cloud
Storage tier (a bucket name, not a credential — that tier authenticates with Application
Default Credentials) — and the live tiers
read it (the environment wins over the file). Never commit real credentials.

Those are *test* settings. What a running deployment reads is separate: `BLOB_BACKEND`
picks the object-storage backend — unset or `s3` for any S3-compatible endpoint
(`BLOB_ENDPOINT` and its `BLOB_*` companions), `gcs` for Google Cloud Storage natively,
which takes `BLOB_BUCKET` alone and authenticates with Application Default Credentials, so
there is no key to distribute.

**Run the platform locally** with the docker-compose stack — controlplane, brain, and executor against a bundled Postgres, MinIO, and OpenBao (and an optional Jaeger):

```bash
cd deploy/compose
cp .env.example .env          # set CONTROLPLANE_API_KEY
docker compose up --build     # control plane on http://localhost:8080 (loopback)
```

Then drive it with the real CLI: `ANTHROPIC_API_KEY=<key> ant --base-url http://localhost:8080 beta:agents list` (management commands take `--base-url` explicitly; they ignore `ANTHROPIC_BASE_URL`, which only the worker/auth subcommands honor). The stack idles until you point the brain at your model endpoint (copy `model-providers.example.json` and set `MODEL_PROVIDERS_FILE`). See [`deploy/compose/README.md`](./deploy/compose/README.md) for details; production deploys use the [Helm chart](./deploy/helm) — from v0.2.0 onward installable straight from the registry (`helm install map oci://ghcr.io/opensdlc-dev/charts/managed-agent-platform --version <X.Y.Z> ...`), with prebuilt worker binaries attached to each [GitHub Release](https://github.com/OpenSDLC-Dev/managed-agent-platform/releases).

Contributions are welcome. Please read [CLAUDE.md](./CLAUDE.md) first — it documents the non-negotiable design principles and the working conventions (notably: **never guess at the wire schema**; verify against the real `ant` CLI) — and [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) for how the platform is built.

## License

[Apache-2.0](./LICENSE)
