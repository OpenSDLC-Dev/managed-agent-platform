# CLAUDE.md

Guidance for Claude Code (and contributors) working in this repository. [AGENTS.md](./AGENTS.md) is its distilled mirror for external AI coding agents and reviewers.

## What this is

An **open-source, self-hostable platform for long-horizon agents**, written in **Go**, Apache-2.0.
Goal: let enterprises run the whole thing **on-prem / in their own VPC** — data and compute never leave their boundary.

We take Anthropic's **Claude Managed Agents** as our **reference implementation**: we adopt its domain model and keep the public REST API **wire-compatible** with it, so the real `ant` CLI and Anthropic SDKs can drive our server unchanged. Referencing it is a deliberate compatibility and design choice, not an attempt to reproduce it — where our goals (self-hosting, pluggable model backends, OTel) call for something different, we diverge on purpose and say so.

## Plans, state, and backlog

- **Plans live in [docs/plan/](./docs/plan/)** — one file per plan, `NN_short-name.md`, the number assigned when the file enters the repo. Frontmatter carries `status: draft | approved | in-progress | archived` and an optional `issue:`. The status is not decoration: it changes **in the PR that changes the reality** — `in-progress` in the one that starts development, `archived` in the one that finishes or supersedes it (the file says which). A plan approved in plan mode is copied here in the first PR that touches it and the repo copy is canonical from then on; if that same PR starts the work, it lands as `in-progress` directly.
- **Plan files carry no progress tracking** — no checklists ticked as work lands, whatever the status. While a plan is active its progress lives in STATE.md; once it archives, the delivery record is docs/HISTORY.md and CHANGELOG.md.
- **Current state: [STATE.md](./STATE.md)** — the active-work tracker, and nothing else: what is in flight (**none** when nothing is) and its task checklist, within a ~30-line budget it enforces on itself. Read it at the start of a session; update it in every PR that starts, advances, or finishes the work it tracks — a plan's PRs and plan-less issue work alike. Its own header says where everything it refuses to hold lives; the routing rule behind that is workflow step 2 below.
- **The backlog is GitHub issues** — the only backlog. Neither plan files nor STATE.md accumulate future work. Post-v1 deferrals are the open issues whose titles carry the `(post-v1)` marker (`gh issue list --state open --limit 100 --search "post-v1 in:title"`, which also returns the occasional issue that merely mentions the phrase) — do not build ahead of them, and mark a new deferral the same way so the query stays the whole list; wire assumptions awaiting a real managed-agents recording are cross-linked from [docs/DIVERGENCES.md](./docs/DIVERGENCES.md)'s INFERRED section.
- **Starting work from a GitHub issue?** Dispatch the **`issue-triage` subagent** first (`.claude/agents/issue-triage.md`, which states its own judgment criteria) to decide whether the issue needs a plan file before implementing. Its verdict is advisory: drafting the plan, or turning its tasks into STATE.md's, stays with the main agent. Not for work that already has a plan, or for requests that never touch an issue.

The v1 design plan is [docs/plan/01_v1-managed-agent-platform.md](./docs/plan/01_v1-managed-agent-platform.md) (archived; mostly implemented) — consult it for rationale before large architectural changes.

## Core architecture — decouple brain / hands / session

An agent is three independently-swappable pieces (a pattern we take from the reference): the **session** — an append-only event log in Postgres, the single source of truth; the **brain/harness** — the stateless, horizontally-scalable loop that calls the model and routes tool calls (a crashed brain loses nothing: any fresh brain replays the log and continues); and the **sandbox ("hands")** — a disposable per-session container that runs tools ("cattle not pets": a dying container is one tool-call error, not a lost session). Four server `cmd/` binaries — `controlplane` · `brain` · `executor` · `worker`; a fifth, `cmd/gate`, is the plan-12 egress sidecar (not a server — a per-session forward proxy).

**Execution is fully async through the event log + work queue.** The brain runs no agent tool in-process — it emits `agent.tool_use`, an executor pulls the work, runs it in a sandbox, posts the result event, and the brain wakes and continues. (The six *delegation* tools are the exception that proves the rule: they touch no sandbox, so the settlement answers them in the transaction that commits the turn calling them.) Platform-managed `cloud` and customer BYOC `self_hosted` are the **same pull protocol at two deployment points**.

The as-built depth — process topology, the full execution flow (permissions/HITL, crash recovery), the wire-compatibility model, a per-package reference, security invariants, observability, testing architecture — is **[docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md)**.

## Non-negotiable design principles

1. **Anthropic's domain model is the single source of truth.** All internal types (`agent`/`environment`/`session`/`event`/`span`/`stop_reason`/`permission_policy`) are Anthropic-native and match the wire schema. Never bend them to a third-party library's shape.
2. **adk-go (`google.golang.org/adk/v2`) is a source of ideas, never a foundation.** (Distinct from Claude Managed Agents, which *is* our authoritative reference for the domain model.) adk-go is NOT a dependency of the domain layer. Where its abstractions conflict with the Anthropic model — its genai-centric `Event`/`session.Service`, its in-process `Runner`, `server/adkrest` — **do not use them**. Only borrow narrow, non-conflicting helpers, and only when they clearly save work. If a borrow ever conflicts, drop it and hand-roll.
3. **Observability is built in, not bolted on.** Every cross-process call propagates OTel context (W3C `traceparent`, including through work items to BYOC workers). Anthropic `span.*` events and OTel spans are emitted from the **same** instrumentation point so they never drift.
4. **Model providers are config-driven.** A provider is constructed from config: `protocol` (`anthropic`|`openai`) · `model` · `base_url` · `api_key` (+ optional headers). The Anthropic-protocol provider must work against **any** endpoint speaking Anthropic Messages (gateway, proxy, self-hosted model) — never hard-code `api.anthropic.com`. `model` string → provider is resolved via the `model_providers` config/table.
5. **Sessions are NOT bound to an end-user.** Scoping keys are `org`/`workspace`/`project` (reserved now, single-tenant defaults in v1). There is no `user_id` on a session (this is a deliberate divergence from adk's `AppName`+`UserID`). End-user ↔ session ownership is an **application-layer** concern; the platform stays user-agnostic. Apps use session `metadata` and the audit-only `created_by` as hooks.

Two standing product decisions travel with these principles: **v1's first-class scenario is a general task agent** (bash + file + web toolset; repository mounting shipped with plan 25 as a wire-compatible session resource, but the toolset is still not built around a repo-centric coding agent), and the project is **Apache-2.0, pure open source — no open-core edition gating**.

## Wire-compatibility rules

- Mirror Anthropic's resource model, paths, JSON fields, and **ID prefixes**: `agent_` `env_` `sesn_` (accept a `session_` alias on input too — a lenient parse of ours, unobserved on the reference) `sevt_` `work_` `vlt_` `vcrd_` `sesrsc_` `depl_` `drun_` `file_` `skill_` `skillver_` `outc_` `sthr_` `memstore_` `mem_` `memver_` — `knownPrefixes` in `internal/domain/id.go` is the list, and a test fails this line if it drifts from it.
- Accept and ignore `anthropic-version` / `anthropic-beta` headers; honor `?beta=true` where the reference does.
- Auth: management via `x-api-key`; workers via environment key (`Authorization: Bearer`, scoped to one environment's work queue).
- Event taxonomy is `{domain}.{action}` — see [docs/plan/01_v1-managed-agent-platform.md](./docs/plan/01_v1-managed-agent-platform.md)'s Component 2 for the full list. SSE deltas use `content_delta` (NOT Messages API's `content_block_delta`).
- Never guess at a wire shape. Resolution order: public docs → the local reference checkouts (the Go SDK's typed schema first, then the `ant` CLI source; see [docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md)) → recording a real `ant` CLI stream for behavior the types can't capture (ordering, SSE framing, defaults).
- Deliberate divergences from the reference, and inferences about reference behavior not yet confirmed, are recorded in **[docs/DIVERGENCES.md](./docs/DIVERGENCES.md)** — the single registry. The verifier's wire-compat rung and external reviewers resolve intentional mismatches against it; a divergence not in the registry is a finding.

## Reference source checkouts (local)

Six read-only local reference sources serve as ground truth and design reference. Their GitHub URLs, repo-relative local paths, per-project roles, authority order, and caveats live in **[docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md)**. In short: `anthropic-sdk-go` is the typed wire schema, `anthropic-cli` is client-side behavior, `claude-code-source` is harness design reference only (a local source snapshot, not a git checkout — never a wire-schema source, never copy code from it), `adk-go` is ideas only per design principle 2, and `deepseek-harness` and `openai/codex` are harness design references like `claude-code-source` — never wire sources, never copied from. In a new session, `/add-dir` them when needed.

## Repo layout

```
cmd/{controlplane,brain,executor,worker}   # the four server binaries
cmd/gate                                   # per-session egress sidecar (plan 12)
internal/
  domain/     # Anthropic-native types — the source of truth; no adk/genai here
  api/        # wire-compatible REST handlers, ID prefixes, auth, work API
  events/     # append-only store, SSE (Postgres LISTEN/NOTIFY), delta reconciliation
  brain/      # orchestration loop, replay, provider request assembly
  provider/   # ModelProvider iface + anthropic/ + openai/ + registry (model→provider)
  executor/   # tool_exec consumer — the platform-managed half of the pull protocol
  worker/     # BYOC worker — the customer-hosted twin of executor/, wire-only
  toolset/    # the built-in tools (agent_toolset_20260401)
  webtool/    # web_fetch/web_search backends: Searcher/Fetcher ifaces + tavily/ + jina/
  mcp/        # the MCP client: a thin wrapper over the official go-sdk (plan 29)
  dialguard/  # the address guard under the vault probe's dials, the MCP client's
              #   DefaultClient and the token-endpoint refresh (SSRF floor; all three live)
  oauthrefresh/ # the RFC 6749 refresh-token grant, shared by the mcp_oauth_validate
              #   probe and the executor's dial-time credential refresh (plan 29)
  sandbox/    # Sandbox/Provider iface + docker/ + k8s/ + backend selection + shell/
  blob/       # object-storage seam: Store iface + s3/ (S3-compatible via minio-go)
              #   + gcs/ (GCS-native, Application Default Credentials) + backend/ (selection)
  secrets/    # credential-cipher seam: Cipher iface + backend/ (selection) + local/ (AES-GCM)
              #   + openbao/ (transit) + gcpkms/ (Cloud KMS)
  skills/     # skill-upload validation + canonical-zip normalization (SKILL.md frontmatter)
  queue/      # work queue (Postgres FOR UPDATE SKIP LOCKED; redis optional later)
  identity/   # the human-auth boundary: OIDC / trusted-proxy JWT verifier, claim→role
              #   mapping, bounded JWKS cache (go-jose; no vendor SDK)
  telemetry/  # OTel/OTLP init; span ↔ span.* same-source instrumentation
  store/      # Postgres schema/migrations, reserved multi-tenant columns
deploy/{helm,compose,gcp}
```

**Test support is named, not listed here**: a directory under `internal/` whose name ends in `test` is one (`pgtest`, `sandboxtest`, `mcptest`, …). The `test` recipe in the Makefile holds the authoritative set, because it has to spell them out to keep them out of the coverage denominator — so adding one means editing that recipe: both its exclusion expression and the comment above it that names the same packages in prose. Beside them sit the top-level `evals/` live suite and `acceptance/` doc-example suite. There is no `internal/policy`: permission policy lives across `domain`/`toolset`/`brain`/`api`.

What a package actually does is its own doc comment — `go doc ./internal/<pkg>`, or the files themselves where the substance is unexported. [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) → "Package reference" is a one-line-per-package map to them; the subsystems that span packages are covered in its other sections, chiefly "Execution flow".

**Dependencies**: `go.mod` is the list, and each package's doc comment argues the choice it made. What neither shows is **containment**, which is the part worth stating once: the MCP go-sdk belongs to `internal/mcp` alone and go-jose to `internal/identity` alone — neither library's types may reach the domain layer. Two more carry consequences past their own package: go-git is pure Go, which is why no image needs a `git` binary at runtime, and `google.golang.org/adk/v2` is **not** a dependency at all, by principle 2.

Two invariants the compiler will not catch for you:

- **`internal/domain` stays stdlib-only** and Anthropic-native. No adk-go, no genai, no provider SDK, ever.
- **A migration is immutable once merged.** New DDL goes in a new numbered file under `internal/store/migrations/`; editing a merged one silently diverges every database that already ran it.

## Development

> The Go version is `go.mod`'s. Docker is available; `psql` is **not** — reach Postgres through the container.

The Go merge gate has one executable source — the root **`Makefile`**; prose and CI name its targets instead of duplicating commands (CI additionally runs its `helm`, `terraform` and `compose` jobs — chart lint/render, the GCP staging Terraform's credential-free checks plus the deploy-side scripts CI runs rather than reads, and a compose smoke test — which stay in ci.yml, and a PR needs the whole workflow green):

```
make verify               # the whole Go gate: build + crossbuild + vet + fmt-check + test + cover-gate
make build crossbuild     # host build + linux/arm cross-compile of ./internal/... (worker portability)
make vet fmt-check        # lint
make test cover-gate      # go test -count=1 with the coverage profile, then the ≥90% gate
docker compose -f deploy/compose/docker-compose.yml up   # local: controlplane+brain+executor+Postgres+MinIO+OpenBao(+Jaeger)
make openbao-init-test   # ...and the compose + chart OpenBao init scripts, run against a fake `bao`
make cd-outcome-test     # ...and the CD failure notifier's classifier, which `workflow_run` would otherwise first run on main
make parked-test         # ...and the parked-cluster label rule `deploy.yml` and `staging-parked.yml` share
make retry-test          # ...and the retry wrapper both notifiers copy, lifted out of the workflow YAML and run
make identifiers-test    # ...and the rule that the operator's coordinates stay in Actions variables, never in docs/
make gcp-fmt gcp-validate gcp-split-check gcp-lint   # GCP staging Terraform, credential-free
make gcp-bootstrap-test gcp-split-check-test gcp-dbinit-test gcp-power-test gcp-tfvars-test gcp-env-targets-test  # ...and its tooling, run rather than read
```

The `gcp-*` targets — like `eval`, `openbao-init-test`, `cd-outcome-test`, `parked-test`, `retry-test`, `identifiers-test`, `registry-check` (the half of the [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) pointer guard that asks GitHub whether an issue is still open; its shape half runs inside the gate as `tools/registrycheck`'s own test) and the release tooling (`changelog`, `changelog-notes`; [docs/RELEASING.md](./docs/RELEASING.md)) — sit **outside** the gate on purpose: `deploy/gcp/`'s Terraform is developer tooling for GCP deployment (plan 20, Decision 9), never a dependency of the platform, its build, or `make verify`. The ten `gcp-*` checking targets above need no credentials and all run in CI; the apply targets cost money and are interactive by design, and `gcp-env-stop`/`gcp-env-start`/`gcp-env-status` park and revive staging without Terraform, state or tfvars at all. `environment/`'s state is a GCS bucket `foundation/` owns (#478), passed to `init` as `-backend-config` rather than committed, so `PROJECT` is now required by the targets that touch it; `foundation/`'s own state stays local, deliberately. Each recipe says what it is for. Terraform is not installed by this repo, and is not in Homebrew core: `brew install hashicorp/tap/terraform`.

CI (`.github/workflows/ci.yml`) invokes the same targets, so the gate cannot drift between the docs, the verifier, and the merge check. A second workflow, `registry.yml`, runs `make registry-check` nightly and on any PR touching the registry, the guard or the Makefile — the state rung a credential-free gate cannot hold. The coverage gate is **total statement coverage ≥ 90%** over the **logic packages** under `./internal/...` — `cmd/` main glue and test support are deliberately outside the denominator, and the `test` recipe both computes that denominator and argues the exclusion. `make test` needs Docker (store/API/sandbox suites) and a Kubernetes cluster (the K8s sandbox contract test; a local kind cluster works) — a missing daemon or cluster is a hard failure, not a skip.

**`.env` (gitignored) holds every live tier's configuration** — the model endpoint (`MODEL_PROTOCOL`, `MODEL_BASE_URL`, `MODEL_API_KEY`, `MODEL_ID`) and, beside it, the web, MCP, Cloud KMS, Cloud Storage and eval-fixture names. **Never commit real credentials.** Which variable buys which tier — and what that tier costs — is [README.md](./README.md)'s test-tier table; the names themselves are in the packages that read them (`internal/modeltest` and its siblings) and in [.worktreeinclude](./.worktreeinclude), which also explains why a worktree needs the file copied in at all.

**The contract they all share is worth carrying in your head**: the *file* supplies configuration, an *environment variable* supplies consent (`RUN_LIVE_*`, or `RUN_EVALS` for the suite), and once opted in, **missing configuration fails rather than skips** — except an MCP token, whose absence is an anonymous dial the protocol admits rather than a rotted credential. So `make test` never spends money unless a variable opted it in, and a rotted credential can never masquerade as a green build. The tiers, and the eval system built on them, were planned in [docs/plan/02_evals-system.md](./docs/plan/02_evals-system.md).

Parallel sessions run in git worktrees (`claude --worktree`), and a worktree is a fresh checkout — it starts without the gitignored files that make the repo runnable, and `modeltest` resolves `.env` from the *worktree's* root, not the main checkout's. `.worktreeinclude` is the list of what gets copied in, and argues each entry (and each deliberate omission) in place.

Verify wire-compat end-to-end by pointing the real `ant` CLI at the local server with `--base-url http://localhost:PORT` and running `ant beta:agents/environments/sessions ...` and `ant beta:worker poll`. No `ant` binary is installed — build it from the read-only checkout (path in docs/REFERENCE_PROJECTS.md): `go build -o <scratch>/ant ./cmd/ant`. (Management commands ignore `ANTHROPIC_BASE_URL` — the CLI builds its client with `WithoutEnvironmentDefaults` and its global `--base-url` flag has no env source; only the worker/auth subcommands honor the env var.)

The module path `github.com/OpenSDLC-Dev/managed-agent-platform` carries the owner's mixed case deliberately — it must match the GitHub owner exactly (Go escapes the uppercase letters in the module cache).

## Iteration workflow (branch → review → PR → CI → squash merge)

Every change lands through a PR; **never commit directly to `main`**. Releases are their own ritual — a release PR plus an annotated tag; versioning policy and the steps live in [docs/RELEASING.md](./docs/RELEASING.md).

Review tiering: a diff whose changed paths (by `git diff main...HEAD --name-only`) are exclusively documentation markdown — `*.md` files outside `.claude/`, excluding CLAUDE.md and AGENTS.md themselves — may run a **single** code reviewer instead of the dual pass in step 4, but always keeps the verifier run, including its docs-consistency rung. Markdown that steers behavior (`.claude/` agents and skills, CLAUDE.md, AGENTS.md), anything else, or any ambiguity takes the full ritual.

1. Branch off a fresh `main`: `git checkout main && git pull && git checkout -b <type>/<short-name>` (e.g. `feat/telemetry`, `fix/event-seq`, `chore/ci`).
2. Develop on the branch (TDD as below). **Docs move with code, in the same PR:** a `changelog.d/` fragment for every notable change — the fragment body is the final CHANGELOG.md entry verbatim (format: [changelog.d/README.md](./changelog.d/README.md)), still the **one** place a change's narrative is written; only a release PR edits CHANGELOG.md itself, via `make changelog` — plus the post-release `make changelog-archive` move of a released section to docs/changelog/ (docs/HISTORY.md receives only acceptance-run and review-hardening records, decisions evaluated and rejected, and archived plans' progress summaries — never a per-PR narrative); the active plan's frontmatter status and STATE.md's Active work/Tasks (truthful, within its ~30-line budget) whenever the change starts, advances, or archives a plan — or starts, advances, or finishes plan-less issue work STATE.md tracks; a docs/DIVERGENCES.md entry for any new wire divergence or inference; README.md (status line, development notes) whenever the change alters what it describes — README's roadmap section deliberately defers to CHANGELOG.md and the issue tracker instead of tracking work itself. A doc that overclaims or lags the code is a defect, not a nice-to-have — the verifier checks docs consistency as a dedicated rung.
3. Run the **verifier subagent** (see "Independent verification"); fix findings before review.
4. **Dual code review**, one pass each: the Codex reviewer and `/code-review` (Claude reviewer). **Model and reasoning effort are not defaults you may accept — a weak reviewer finds nothing, and its silence is indistinguishable from a clean bill of health.** So the invocations, the model pins, and the ground rules every pass shares are the **`run-reviews` skill**'s (`.claude/skills/run-reviews/SKILL.md`), which governs both reviewers and the verifier: read it before launching any of them, rather than working from memory of what it said. Address findings from both; if a fix changes behavior, re-run the verifier.
5. Push and open the PR (`gh pr create`); include the verifier verdict and the outcome of every review the tier required in the description — a stalled reviewer pass (see the skill's stall note) is reported as such, never silently dropped.
6. Wait for CI (`.github/workflows/ci.yml`) to be fully green (`gh pr checks --watch`) **and settle every review thread** — CodeRabbit and other bots included: each thread closed by a fix or by an evidence-backed refutation posted as a reply, then resolved. Red CI → fix on the branch; never merge red.
7. **Squash merge** only with CI green and zero unresolved review threads (`gh pr merge --squash --delete-branch`), then sync local: `git checkout main && git pull`.

## How to work in this repo

These bias toward caution over speed. For trivial changes, use judgment.

**Think before coding.** State assumptions explicitly rather than picking silently. This codebase has a specific failure mode: *guessing at the wire schema*. When an exact JSON shape isn't in the public docs, do not invent it — read the reference checkouts (SDK types first), and record a real `ant` CLI stream when only behavior can answer. Likewise, if a requirement admits two readings (e.g. whether a field is session-local or agent-level), surface both and ask. If something is confusing, stop and name what's confusing. If a simpler approach exists, say so — push back when warranted.

**Assessment is a deliverable too.** When the user is describing a problem or asking a question rather than requesting a change, report your findings and stop — don't fix until asked. Before any state-changing command (killing a server, dropping a test database, rewriting `.claude/` or workflow config), check the evidence supports *that specific* action — a pattern-matched signal may have a different cause.

**Pause only when the work genuinely requires the user.** That means: a destructive or irreversible action, a real scope change, or input only they can provide (credentials, account approvals, a product decision). Hitting one of these, ask and end the turn. Everything else — retries, missing information you can gather, long verification loops — is yours to push through; never end the turn on a promise of work not yet done.

**Simplicity first.** Write the minimum code that solves the problem; nothing speculative. The plan deliberately *reserves seams* for vaults, deployments, memory, multi-agent threads, and skills — reserving a seam means a column or an interface boundary, **not** an implementation. Do not build ahead of the current slice. No abstractions for single-use code, no configurability nobody asked for, no error handling for impossible states. If 200 lines could be 50, rewrite it. The test: would a senior engineer call this overcomplicated?

**Surgical changes.** Every changed line should trace directly to the request. Don't "improve" adjacent code, comments, or formatting; match the existing style even where you'd choose differently. Clean up orphans *your* change created (now-unused imports, functions); leave pre-existing dead code alone — mention it instead of deleting it.

**Documents say what code cannot.** Before writing prose, ask whether a comment beside the code would carry it better. It usually would: a comment cannot drift from what it describes, and a document always can — this repo's package reference was three to ten times smaller than the comments it restated, and wrong in places they were right. A document earns its place with what spans files: a subsystem's shape, an invariant two packages share, a decision and the alternative it beat, a contract an operator depends on. It does not earn it by restating a signature, inventorying files, or narrating behavior a reader can run. **Then write it short** — say each thing once, where it belongs, and stop; the concrete number beats "several", the name beats "the relevant component", and a sentence that only announces the next one is deletable. Length is not the target; it is what falls out of saying each thing once. **But never trade truth for brevity**: a shorter sentence that overclaims is a defect, not an improvement — qualify it, or leave it long. Before cutting a fact, confirm it survives somewhere — a comment, an open issue, a changelog entry — and if it does not, move it there first.

**Leave the machine as you found it.** Whatever a run spawns, it owns. Interrupting `make verify` still leaks one fixture container per test binary, because the `defer` that removes it never runs — `internal/dockertest` bounds the damage by labelling and later reaping them (#346), but background load, servers and probes have no such backstop. So: note what is already running before you start, look afterwards at what *you* started (`ps`, `docker ps -a`) rather than trusting that a `kill` worked, and remove only what is yours. A fixture label names the suite, never the session, so a parallel session's containers and the local kind cluster are indistinguishable from your own — when the only evidence is a timestamp, ask before sweeping.

**Goal-driven execution.** Turn each task into a verifiable goal before writing code, and state the plan as steps with their checks:

```
1. [Step] → verify: [check]
```

Concretely here: "add validation" → write the failing test for invalid input first; "fix the bug" → write the reproducing test first; "refactor X" → tests green before and after. Strong success criteria let you loop to done without check-ins; weak ones ("make it work") don't.

**Independent verification (definition of done).** The implementer never certifies their own work. Before any nontrivial change is declared done — before STATE.md's task progress claims new behavior, before a commit that claims working behavior — dispatch the **`verifier` subagent** with what changed and the success criteria; `.claude/agents/verifier.md` defines the rungs it runs, docs consistency among them. A FAIL or an unresolved blocker means the work is **not done**: fix, then re-verify. Dispatch it with no model override so its pinned model wins — a weak verifier certifies anything. Its verdict and evidence belong in the final report to the user.

**Report only what you can evidence.** Every progress claim cites a tool result from this session — a test run, a diff, a CLI transcript. Unverified work is labeled as such; failures are reported with their output; skipped steps are named. Done-and-verified is stated plainly, without hedging.

**Write the final report for a reader who wasn't there.** Open with the outcome, then the supporting detail, then what's needed from the user. Complete sentences, terms spelled out, no arrow chains, no labels invented mid-session unless re-introduced. If short and clear conflict, choose clear.

**Testing conventions.**
- **TDD** for anything with behavior: contract test first, then implement. This matters most for provider adapters, event/JSON round-trips against the wire schema, sandbox providers, and the work-queue lease state machine.
- Keep files focused and small; one clear responsibility per package.
- Provider-, sandbox-, and queue-backend variability lives behind interfaces with a **shared contract test suite** — every new backend must pass the same suite.
- Confine lossy conversions to a single package (`provider/openai`) and test them hard; the Anthropic-protocol provider should be near-zero-conversion.
