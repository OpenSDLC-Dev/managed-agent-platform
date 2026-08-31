# managed-agent-platform

An open-source, self-hostable platform for **long-horizon AI agents**, written in Go.

Run the whole thing on-prem or in your own VPC — **your data and your compute never leave your boundary**.

> **Status: v0.3.0, with multi-agent coordination merged to `main` and unreleased — the v1 loop is complete; agents reach MCP servers, the control plane authenticates people, a coordinator agent's roster now runs as concurrent session threads, and a session mounts versioned memory stores it reads and writes back.** A `v*` tag publishes container images and the Helm chart to GHCR, and worker binaries with clamped release notes to the GitHub Release ([docs/RELEASING.md](./docs/RELEASING.md)).

What runs today, end to end:

- **The core loop** — the wire-compatible control-plane API over an append-only session event log with SSE streaming, config-driven model providers (Anthropic-protocol and OpenAI-compatible), and the brain orchestration loop.
- **Tools in sandboxes** — the complete `agent_toolset_20260401`, executing in per-session Docker or Kubernetes sandboxes under permission policies with human-in-the-loop approval. `web_search` and `web_fetch` are the deliberate exception: they run in the platform executor behind config-driven backends, on both deployment modes, with no sandbox involved.
- **Your own compute** — BYOC workers run a self-hosted session's tools, with dead-worker recovery and a single OTel trace across the process boundary.
- **Sandbox lifecycle** — an executor-resident reaper destroys the sandboxes of deleted, archived and terminated **cloud** sessions, and checkpoints an idle one's workspace to object storage, restoring it intact when the next message arrives. A `self_hosted` session's sandbox belongs to its BYOC worker and is never touched.
- **Session resources** — **skills**, **files** and **GitHub repositories** mount into a session; repositories are cloned platform-side, so the token never enters the container the agent controls.
- **Memory stores** — a workspace-scoped, versioned collection of text documents attaches through `resources[]`, mounts at `/mnt/memory/<slug>`, and is read and written with the ordinary file tools; every write becomes an attributed, immutable version, and the store stays in sync across the sessions that share it, `cloud` and BYOC alike — the BYOC worker reconciling over the wire with the per-item sessions token its work items carry.
- **Outcomes** — a text or file rubric is graded each work cycle, deliverables are harvested into the Files API, and revision feedback runs up to `max_iterations`.
- **Vaults and egress** — `/v1/vaults` holds cipher-sealed, write-only credentials that a session attaches at create time; a `limited` environment's traffic leaves through a per-session egress gate that enforces `allowed_hosts` and substitutes those credentials on the way out, so the sandbox only ever holds an opaque placeholder.
- **MCP servers** — declared per agent, discovered, offered to the model and answered under human confirmation by default, with vault credentials matched and expiring OAuth tokens refreshed at the dial.
- **Multi-agent teams** — a coordinator agent's `multiagent` roster runs as concurrent **session threads** on one shared sandbox, each thread with its own agent, event log, status and stream, matching the reference's `sthr_` thread resource and its `/threads` routes. The coordinator spawns its agents, messages them, lists them and waits on them; they report back, and the session's status is a fold over its threads'. Cloud and BYOC alike — the customer-run worker stays thread-unaware, because the session view it already reads carries the child calls it must run.
- **Human authentication** — the control plane is a vendor-SDK-free OIDC relying party ([Casdoor](https://casdoor.org) bundled as an optional hardened default, plus a trusted-proxy mode where the cloud terminates auth). It resolves each request to a **principal** whose claims map to one of `admin` · `developer` · `viewer`, and refuses one that maps to none — identity is default-deny.

Machine credentials keep their own lane ahead of identity: `x-api-key`, and every environment key this platform minted, behave identically whether identity is configured or not, which is why the real `ant` CLI — `ant beta:worker` included — drives every machine flow unchanged. (The one exception is a grandfathered key whose operator-chosen value happens to be JWT-shaped; it is refused fail-closed rather than over-authorized, and [docs/self-hosted-security.md](./docs/self-hosted-security.md) §6 says to reissue it before enabling SSO.) Human *login* is the one thing it cannot: this platform deliberately serves no `POST /v1/oauth/token`, so people sign in to their own IdP out of band ([docs/DIVERGENCES.md](./docs/DIVERGENCES.md) registers the divergence). Deploy locally with [docker-compose](./deploy/compose) or to Kubernetes with the [Helm chart](./deploy/helm); [CHANGELOG.md](./CHANGELOG.md) is what landed when, and the [issue tracker](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues) is what's next.

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

Execution is **fully asynchronous through the event log and a work queue.** The brain runs no agent tool in-process: it emits `agent.tool_use`, an executor pulls that work, runs it inside a sandbox, and posts the result back; the brain wakes and continues. (Multi-agent delegation is the one exception — those tools touch no sandbox and are answered where the turn commits.) Platform-managed sandboxes and customer-run (BYOC) workers are the **same pull protocol at two deployment points**.

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

Deferred past v1 — **seams reserved, not implemented**, each tracked as an issue. The canonical list is the open issues whose titles carry the `(post-v1)` marker, which [CLAUDE.md](./CLAUDE.md) gives the query for; the notes below single out what most often surprises a reader, and are neither the whole of that set nor confined to it. Vaults, skills, files, repository mounting and memory stores were on this list and have since landed; what remains includes:

- **Scheduled deployments** are landing under plan 37 ([#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)): the `/v1/deployments` surface serves, `POST /run` fires a manual run, and the scheduler now fires the schedule itself — what remains is the two run list endpoints, so run history is not yet readable over the wire.
- The **multi-tenant** half of RBAC and SSO ([#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56), which stays open for it). Single-tenant SSO and the three-role matrix are done.
- **Repository materialization on BYOC compute** ([#322](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/322)): a `self_hosted` session accepts a `github_repository` resource and mounts nothing, without being told otherwise.
- **BYOC gate delivery** ([#165](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/165)) and **credential substitution inside TLS** ([#166](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/166)) — until #166, an in-sandbox HTTPS request keeps its vault placeholders rather than having them substituted at egress.

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
make registry-check        # ask GitHub about docs/DIVERGENCES.md's pointers: live trackers open, provenance known
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
| Live MCP server | `RUN_LIVE_MCP_TESTS=1` | one handshake and one listing against a third-party MCP server (`MCP_LIVE_SERVER_URL`, optional `MCP_LIVE_SERVER_TOKEN`) through the guarded production client. Every other MCP test speaks to a fixture built from the same go-sdk the client is, so both ends agree even where that understanding is wrong; this is the only tier a real implementation can contradict |
| Live Cloud KMS | `RUN_LIVE_KMS_TESTS=1` | the shared credential-cipher contract suite against a real Cloud KMS key (`GCPKMS_KEY_NAME`), authenticating with Application Default Credentials — the tier that proves the envelope-encryption path against the service rather than a fake |
| Live Cloud Storage | `RUN_LIVE_GCS_TESTS=1` | the shared object-storage contract suite against a real GCS bucket (`GCS_BUCKET`) through the GCS-native backend, authenticating with Application Default Credentials — the only tier that exercises that client's production read transport, which the hermetic `fake-gcs-server` cannot serve |
| Live acceptance | `RUN_LIVE_ACCEPTANCE_TESTS=1` | the reference doc's [define-outcomes example](https://platform.claude.com/docs/en/managed-agents/define-outcomes) end-to-end against an externally running stack through the typed Go SDK — configured by `ACCEPTANCE_BASE_URL` / `ACCEPTANCE_API_KEY` / `ACCEPTANCE_MODEL` from the environment only, never `.env` (its deterministic scripted-model rehearsal runs in the default tier) |
| Live-system evals | `RUN_EVALS=1` (`make eval`) | whole sessions: API → brain → real model → sandbox → SSE, deterministically graded. [Nineteen regression tasks](./docs/plan/02_evals-system.md) spanning the built-in toolset, permission allow/deny, single- and multi-turn, skill injection, file, repository and MCP-server mounting, outcome grading, coordinator delegation across two worker threads, and memory stores in both directions; results land in `evals/artifacts/`. CI also runs this tier daily on a schedule ([`evals.yml`](./.github/workflows/evals.yml)), reading the same four model variables plus `EVAL_GITHUB_REPO_URL` / `EVAL_GITHUB_REPO_TOKEN` from the `evals` deployment environment's secrets — and failing rather than skipping while they are unset |

Configure the dotenv-loading tiers once in a gitignored repo-root `.env`: the endpoint
itself — `MODEL_PROTOCOL` (`anthropic`|`openai`), `MODEL_BASE_URL`, `MODEL_API_KEY`,
`MODEL_ID` — and the per-tier names the table above spells out for the model, web, MCP,
KMS, Cloud Storage and eval tiers. The environment wins over the file. **Live acceptance is
the exception**: `RUN_LIVE_ACCEPTANCE_TESTS` and its `ACCEPTANCE_*` settings are read from
the process environment only and are never loaded from `.env`, because that tier drives an
externally running stack rather than this checkout's.
**Never commit real credentials.** The eval fixture is the one entry with a requirement of
its own: `EVAL_GITHUB_REPO_URL` must name a **private** repository holding a
`PASSPHRASE.txt`, and `EVAL_GITHUB_REPO_TOKEN` a fine-grained, single-repository,
Contents: Read-only token for it. That trial carries no per-trial nonce, so the
repository's privacy is what keeps the answer secret.

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
