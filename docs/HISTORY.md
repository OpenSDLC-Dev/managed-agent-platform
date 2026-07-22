# HISTORY.md — acceptance and decision records

What a changelog structurally cannot hold: acceptance-run records, review-hardening
records with their evidence, decisions evaluated and rejected, and archived plans'
progress summaries. A change's **narrative** is written once, in
[CHANGELOG.md](../CHANGELOG.md) — never duplicated here (the one-writer rule; CLAUDE.md →
"Iteration workflow"). The as-built system description is
[ARCHITECTURE.md](./ARCHITECTURE.md).

Provenance: this file began 2026-07-16 as the verbatim completed-work archive moved out
of [STATE.md](../STATE.md), and documents — [DIVERGENCES.md](./DIVERGENCES.md) above
all — cite its section headings as evidence anchors. On 2026-07-18 the per-PR delivery
narratives were verified section-by-section against CHANGELOG.md and pruned (git history
is the backstop); every cited heading is preserved below, and anything still under one is
recorded nowhere else.

---

## Delivery slices

| # | Slice | Status |
|---|---|---|
| 0 | `internal/domain` (Anthropic-native types) + `internal/telemetry` (OTel/OTLP, context propagation) | ✅ Done |
| 1 | Postgres schema + migrations (`internal/store`), reserved multi-tenant columns | ✅ Done |
| 2 | Control plane CRUD (agents / environments / sessions) + optimistic versioning + ID prefixes + `x-api-key` auth | ✅ Done |
| 3 | Append-only event log (seq allocation) + `POST /events` + SSE stream (`event_start` / `event_delta` reconciliation) + `span.*` emitted from the same point as OTel spans | ✅ Done |
| 4 | `ModelProvider` (config-driven: protocol / model / base_url / api_key) + `model_providers` routing; first provider passing a single model turn; verify a custom `base_url` works | ✅ Done |
| 5 | Brain orchestration loop (replay → assemble provider request → write Anthropic-native events). No adk runtime. | ✅ Done |
| 6 | tool-exec queue (Postgres `FOR UPDATE SKIP LOCKED`) + executor + Docker sandbox provider + built-in toolset really executing inside the sandbox | ✅ Done |
| 7 | Permission policies + `requires_action` / `user.tool_confirmation` approval round-trip | ✅ Done |
| 8 | Wire-compatible work API (`/work/poll`, `/ack`, `/heartbeat`, `/stop`) + distributable BYOC worker + `traceparent` propagated through work items | ✅ Done (PRs A, B, C1, C-list, C2a, C2b, C3, C2b-2, C-meta, C-stats — per-PR narratives in CHANGELOG.md § 0.1.0) |
| 9 | Kubernetes sandbox provider + Helm chart (with OTLP endpoint values) | ✅ Done (K8s `sandbox.Provider` on the shared contract suite via kind, `SANDBOX_BACKEND` selection, Helm chart, compose stack) |

---

## Skills slice-5 acceptance — Level-1 injection, the full chain end to end (run 2026-07-22) — ✅ passed

The skills plan's slice-5 acceptance (plan E2E-2), against a real model: the opt-in eval task `skill-answer` (`RUN_EVALS=1 go test ./evals -run TestEvals/skill-answer`). The harness uploads a self-authored fixture skill through the public multipart endpoint — a SKILL.md whose instructions point at an `answer.txt` beside it, the answer being a per-trial `{{RECALL}}` token the prompt never contains — creates an agent referencing it at `latest`, and drives one turn that asks only for the passphrase, naming neither the skill nor a path — so the injected Level-1 metadata is the model's only route to learning a skill can answer and where it lives (the discovery mechanism under test; a turn that announced the skill would let the model succeed by exploring the filesystem even if injection regressed). The run log shows `skill created skill_id=… version=…` then `skill materialized session_id=… skill_id=… version=…`, and both graders passed: the transcript carries a tool call reading `skills/eval-secret/SKILL.md` (the agent found and followed the injected Level-1 metadata), and the final `agent.message` contains the `{{RECALL}}` passphrase — reachable **only** through the materialized answer file. Registry → brain resolution + Level-1 injection → executor materialization → the model acting on it, one unbroken chain exercised by a value the model could not otherwise know. (The turn was strengthened after CodeRabbit review to name no skill or path; the re-run above is green.)

## Skills plan — archived 2026-07-22, all five slices delivered

docs/plan/06_skills.md (issue #54) is archived complete. Slice 1: `internal/blob` + `blob/s3` (minio-go) + contract suite, compose/helm object storage (PR #145). Slice 2: the wire-compatible `/v1/skills` registry over object storage, migration 0007, `skillver_` ids, both upload forms, nine endpoints, E2E-1 compose round-trip + real `ant beta:skills` acceptance (PR #146). Slice 3: the run-once anthropic prebuilt-skills importer, date versions, idempotent, self-authored CI fixtures, real-checkout acceptance (PR #147). Slice 4: runtime materialization on both execution halves (executor blob-sourced + wire-only worker SetupSkills twin), the env-key dual-auth read lane, sentinel idempotence, the 500-skills cap, real-model + real `ant beta:worker` acceptance (PR #148, hardened across four Codex review rounds — the sentinel trust boundary, `latest_version` numeric-max concurrency, a hard-bounded archive read). Slice 5: brain Level-1 injection with the `skills.resolve.misses` metric, the E2E-2 eval, and docs closure (PR #152, hardened after a Codex + Claude/Opus + verifier review round — the resolve-miss counter is flushed before the turn's early-return paths so a logged miss always counts, the `model_request` span's `skills.injected`/`skills.block_chars` gained assertions, and the `ReadsSkillFile` eval grader was tightened to a real file read). Deliberate divergences and inferences are in docs/DIVERGENCES.md; the as-built system in docs/ARCHITECTURE.md.

## Skills slice-4 acceptance — materialization on both halves, real model + real `ant beta:worker` (run 2026-07-22) — ✅ passed

The skills plan's slice-4 acceptance, both deployment points.

**Cloud half — full compose stack, real model.** `docker compose up` (controlplane + brain + executor + Postgres + MinIO; brain routed to a real Anthropic-protocol gateway). A fixture skill uploaded via the loose-files form; an agent with `tools:[agent_toolset_20260401]` + `skills:[{type:custom, skill_id}]`; a session posted `user.message` "Run exactly this bash command and show me its output: cat skills/alpha-notes/SKILL.md". The model emitted `agent.tool_use bash {"command":"cat skills/alpha-notes/SKILL.md"}`; the executor provisioned the Docker sandbox, logged `skill materialized session_id=… skill_id=… version=1784657206256533`, and the `agent.tool_result` carried the SKILL.md content **byte-exact**; the model's closing `agent.message` quoted it and the session went idle with a clean stop. Registry → resolution (`latest` → concrete) → blob fetch → sandbox write → bash read, one unbroken chain.

**BYOC half (E2E-3) — the real `ant beta:worker`, whose SDK internals run SetupSkills.** Against a locally-run controlplane (disposable Postgres + MinIO): a `self_hosted` environment with a minted key, the same fixture skill, an agent referencing it at `latest`, and a session suspended on `bash cat skills/alpha-notes/SKILL.md`. The unmodified `ant beta:worker poll --base-url … --environment-key …` claimed the item and its log shows the reference worker's own materialization against this platform: `downloaded skill … skill_id=skill_jzp3zwh12qjx7rankcz9jgyj version=1784656902111009 dest=…/skills/alpha-notes` — the SDK resolved the `latest` alias by listing versions, fetched the version object, downloaded `/content`, and extracted onto its workdir, all through the new environment-key dual-auth lane; then `executing tool … tool=bash` and a posted `user.tool_result` (`is_error:false`) carrying the exact SKILL.md text. A second suspended command (`cat skills/alpha-notes/reference.md`) round-tripped the same way. No CLI or SDK accommodation anywhere.

---

## Skills slice-3 acceptance — real anthropics/skills checkout imported, listed by `ant` (run 2026-07-22) — ✅ passed

The skills plan's slice-3 acceptance: the run-once operator import against a **real, fresh clone of github.com/anthropics/skills** (cloned to a scratch directory — never into this repo, per the license red lines).

**Flow.** Disposable Postgres + MinIO; `go run ./cmd/controlplane -import-anthropic-skills <clone>` with only `DATABASE_URL` + `BLOB_*` set → all four document skills imported at version `20260716` — the clone's last commit date, resolved via git with no flag; the four real SKILL.mds passed the upload validation **unchanged** (descriptions 437–948 runes, under the 1024 cap). An immediate re-run reported `imported 0, skipped 4, failed 0` — idempotence over the same version, no storage traffic. The server was then started on the same database and the **real `ant` CLI** confirmed the catalog: `beta:skills list --source anthropic` returned `xlsx / pptx / pdf / docx`, each `latest_version 20260716`, `source anthropic`; `beta:skills:versions download --skill-id xlsx --version 20260716` streamed a real zip (PK magic) through the short-name id path — the id shape the slice-2 API was built to accept ahead of this slice.

---

## Skills slice-2 acceptance — real `ant beta:skills` against the registry (run 2026-07-21) — ✅ passed

The skills plan's slice-2 acceptance (docs/plan/06_skills.md): the **real `ant` CLI** (built from the local checkout) driving the new `/v1/skills` registry, zip form — the only form the CLI can emit, since it basenames every part filename, which makes it the canonical compatibility probe.

**Setup.** Disposable Postgres + MinIO containers, `cmd/controlplane` run locally with `BLOB_*` pointed at the MinIO, a fixture skill (`financial-skill/{SKILL.md,reference.md}`) zipped locally.

**Flow exercised, all against `--base-url` with unmodified CLI commands.** `beta:skills create --file financial-skill.zip` → the Skill object (`skill_` id, `display_title` defaulted from the frontmatter name, epoch-microsecond `latest_version`, `source:"custom"`); `beta:skills list` / `retrieve` echo it; `beta:skills:versions create` mints a second version (`skillver_` id, name/description/directory extracted from SKILL.md) and `latest_version` follows; `beta:skills:versions list --limit 10` returns both, newest first; `beta:skills:versions download` streams the archive **byte-identical** to the uploaded zip (`cmp` clean); `beta:skills delete` with versions still present is the wire's `400 invalid_request_error`; both `beta:skills:versions delete` calls echo the **version timestamp** as the deleted id (`skill_version_deleted` — the reference's asymmetry); the final `beta:skills delete` returns `skill_deleted` and a retrieve after it the enveloped `404 not_found_error`. No CLI flag, path, or field needed any accommodation.

---

## Slice-8 acceptance — real `ant beta:worker` end to end (run 2026-07-16) — ✅ passed

The plan's slice-8 acceptance, deferred until the local stack existed, has now been run and **passed**.

**Setup.** The docker-compose stack (controlplane + brain + Postgres, no executor — the worker replaces it for self-hosted), the brain pointed at a real Anthropic-protocol endpoint (a MiniMax gateway, from `.env`). A self-hosted environment, an agent carrying the built-in `agent_toolset` (bash), and a session — all created by the **real `ant` CLI** (built from the local checkout) driving the control plane over `--base-url`. An environment key was seeded directly into the DB (the issuance primitive `EnsureEnvironmentKey` has no operator surface yet — see Deferred below).

**Flow exercised.** A `user.message` asking the model to run `echo hello-from-byoc-worker` → the brain called MiniMax, which emitted `agent.tool_use` (`bash`) → because the agent toolset defaults to `always_allow` and the environment is self-hosted, the brain enqueued a self-hosted `tool_exec` work item → a real **`ant beta:worker poll`** (also the local-checkout binary, authenticated with the seeded environment key) polled the environment's queue, claimed the item, reconciled the session by listing its events, ran `bash` in its in-process runner, and posted the `user.tool_result` (`hello-from-byoc-worker\n`) → the brain resumed, produced the final `agent.message` quoting the output, and the session went `idle`. The full event log confirmed the round-trip (`user.message` → `agent.tool_use` → `user.tool_result` → `agent.message` → `session.status_idle`).

**Bug found and fixed (this PR).** The worker's reconcile step — the SDK `SessionToolRunner` listing the session's events, `GET …/events?limit=1000` — was rejected `400` by our shared list cap of 100 (poll and ack had already succeeded), so the worker could never read the outstanding `agent.tool_use` and no tool ran. The reference's general list convention is 1-to-1000 (documented on most SDK list params; the agents list is the documented exception at "maximum 100"), and the worker requests exactly 1000, while the SDK's event-list param documents no explicit cap — so the events endpoint gets `maxEventLimit = 1000` (a compatible upper bound, not a proven reference cap; some cap is needed since an unbounded limit is a query-cost risk) while the other lists keep 100. With the fix the acceptance passes end to end.

**Deferred follow-up.** There is no operator-facing way to **issue** an environment key: `EnsureEnvironmentKey` exists and is tested but is wired to nothing (no endpoint, no CLI, no bootstrap seed). The reference mints these off-wire (its console), so a self-hosted operator needs an equivalent primitive. Tracked for a follow-up iteration (design choice pending: an off-wire admin command vs. a management endpoint).

---

## Completed

Pruned 2026-07-18 (plan 03). Each subsection's delivery narrative lives **once** in
[CHANGELOG.md](../CHANGELOG.md) — find it under § 0.1.0 (or § [Unreleased] for later
work) by the same slice/PR tag the heading carries; sections without a tag carry their
own pointer line below — and its per-file reference in
[ARCHITECTURE.md](./ARCHITECTURE.md) § "Package reference". The headings survive as
citation anchors for [DIVERGENCES.md](./DIVERGENCES.md); where a citation's parenthetical
quotes prose from a pruned body, that prose lives on in the matching CHANGELOG entry or
ARCHITECTURE row. What remains under a heading is recorded nowhere else.

### Repository & tooling

*Narrative: the foundation entries at the bottom of CHANGELOG § 0.1.0 — "Project
foundation", "CI pipeline", "CI coverage gate", "GitHub checks", "Dual code review",
"Docs-consistency rule", "STATE.md".*

### `internal/domain` — Anthropic-native core types

*Narrative: CHANGELOG § 0.1.0 → "Project foundation" (slice 0).*

### `internal/telemetry` — OTel init + W3C trace-context propagation

*Narrative: CHANGELOG § 0.1.0 → "`internal/telemetry` — OTel foundation"; the log bridge
is under § [Unreleased] → "OTel logs on the execution chain".*

### `internal/store` — Postgres schema + migrations

*Narrative: CHANGELOG § 0.1.0 → "`internal/store` — Postgres schema + embedded
migrations (slice 1)".*

### `internal/api` + `cmd/controlplane` — wire-compatible control-plane CRUD

*Narrative: CHANGELOG § 0.1.0 → "`internal/api` + `cmd/controlplane`" (slice 2).*

### `internal/events` + events API — append-only log, send/list, SSE stream (slice 3)

### `internal/provider` — config-driven model access (slice 4)

#### OpenAI-compatible adapter (`internal/provider/openai`)

*Narrative: CHANGELOG § 0.1.0 → "OpenAI-compatible provider adapter".*

### `internal/brain` + `internal/queue` + state machine — the orchestration loop (slice 5)

### `internal/sandbox` — the hands (slice 6, first part)

A disposable container per session, driven over the Docker Engine API. This section is
kept nearly whole: it is the canonical record of how the exec deadline was driven to its
final design by seven rounds of adversarial review — measurements, attacks, and the
chronology of verifier passes refuted by reviewers — which exists nowhere else.

**Slice-6 decisions (documented divergences, all deliberate):**
- **No `Attach`.** The plan's `SandboxProvider` had `Provision` + `Attach(sandboxID)`. `Provision` is instead idempotent per session — it adopts the session's running container — which is the only thing an executor ever needed `Attach` for, and it spares us persisting a sandbox id nothing else would read.
- **`Glob`/`Grep` are not on the interface.** The plan listed them on `Sandbox`. They are pure functions of `Exec` and the file primitives, so they belong once in the toolset layer rather than re-implemented by every backend. `Checkpoint` is likewise absent: the plan marks it 后续, and a seam is an interface boundary, not a method every backend must stub.
- **A deadline is enforced twice, and only the outside one is a guarantee.** Docker has no API to kill a running exec, so the killing has to happen inside the container: a wrapper `exec`s the command — so the command *becomes* the exec's own process — after forking a watchdog that, at the deadline, `kill -9`s the command's **process group** (`set -m` makes it a group leader). But that watchdog is a process the command can find and kill. So `Exec` never lets the container decide. It stops waiting `Timeout + 2s` after the deadline (`killGrace`), and it decides the verdict from outside. The watchdog still earns its keep — an honest command's runaway loop stops burning CPU, and the sandbox learns a real exit code — it is simply never believed.
  **Why the command *becomes* the exec, rather than running under a wrapper shell.** The pid Docker reports for an exec is what `Exec` watches from outside. If that pid is a wrapper and the command is its child, the command can `kill $PPID` to make the watched pid vanish while it runs on — a reviewer (Codex) demonstrated exactly that against the earlier design: kill the watchdog, kill the wrapper, run 2s past a 1s deadline, and the call reports `ExitCode=137 TimedOut=false`. `exec`ing the command closes it structurally: the watched pid *is* the command, and a command cannot kill itself and keep running. (The shared contract test now kills the watchdog **and** the parent a prober might watch, then runs long — it fails against the old wrapper and passes against this one.)
  **The verdict is two probes of one question: was the command's process still alive?** Alive as the deadline arrived, and the exec exits 137? Then the watchdog killed it — a timeout. Still alive once the deadline *and* `overrunSlop` (500 ms) had passed? Then no exit code it later reports can be believed, because on the honest path the watchdog would already have killed it — also a timeout.
  **The guarantee, and its soft edge.** A command cannot outrun its deadline by more than `killGrace` — `Exec` stops waiting on its own clock and reports `TimedOut` whatever the container does; that bound is hard. Detecting an overrun *within* that window is softer, because it rests on `top`, and `top`'s answer reflects when the daemon ran `ps`, not when the probe fired — so a command that overruns, exits inside the grace window, and induces the daemon to run its confirming `ps` only after that exit can have the overrun read as a clean finish (up to `killGrace`, and only under attacker-timed daemon delay). The hard bound holds regardless; what leaks is the sub-`killGrace` overrun *label*. Its real containment is the reserved cgroup limits (`PidsLimit`, `CpuQuota`), which also cap the process-count pressure a command needs to stall `ps` — the probe is the cheap first line, not the wall. `TimedOut` is the authoritative field; `ExitCode` may be the kill's, or the one a command that dodged the kill picked for itself.
  Consequences worth knowing: the wrapper keeps **no state inside the container** (a marker file, the first design, let a command forge a timeout it never hit or erase one it did); the deadline a command must have been alive at is the watchdog's rounded-up one, not the caller's request, so a command that kills itself early is never mislabelled; timeouts round up to whole seconds (`sleep` takes no portable fractional argument); the first probe fires `probeLead` (50 ms) *before* the deadline, because at the deadline a command the watchdog has just killed looks exactly like one that was never running — the price is that a command SIGKILLing itself inside that 50 ms reads as a timeout. The watchdog polls `kill -0` on the command rather than sleeping the whole deadline, so an honest command that finishes early takes its watchdog with it within one poll — no stray `sleep` piles up across a session's thousands of quick commands. Its own stdio goes to `/dev/null`; because the command *is* the exec, its stderr reaches the tool result untouched, and a SIGKILL leaves no shell "…Killed…" line behind to begin with.
- **The probe reads the daemon's process list, because both obvious clocks are wrong.** The exec's **output stream** does not close when the command exits: a process the command backgrounds inherits its stdout, and the daemon holds the stream open for it — measured at ~2s, then force-closed. And the daemon's own `Running` flag on the exec tracks that same stream rather than the process — measured at **2.06s** for a command whose process was gone in **40ms**. Timing either one charges a command for stragglers it never waited for, and `sleep 300 & echo started` under a one-second deadline came back `ExitCode: 0` *and* `TimedOut: true`. What does track the process is `GET /containers/{id}/top`, cross-referenced with the exec's `Pid`: `ps` runs on the daemon's host, needs nothing from the image, and is the one view of the sandbox that the sandbox cannot edit. The same measurement fixes the other end — once `Exec` gives up on a stream it asks for an exit code only when the probes say the command is gone, because a real daemon publishes one 1.7s after such a stream is closed and never publishes one for a command still running.
  Two safety properties fall out of doing it this way. A pid the daemon will not name is **fatal**, not ignored: a zero pid would answer "gone" to every probe and disarm the deadline in silence. And a probe the daemon will not answer counts the command as **still running**, because hiding an overrun breaks the guarantee while mislabelling one costs a tool call. That fail-open direction is deliberate, and the overrun probe is careful about it in one more way: its confirmation runs on a clock the output stream's close cannot stop, because a command that overran and then exited *during* its own confirming `top` would otherwise have that exit — not its overrun — read as the answer, and the overrun erased (a reviewer, Codex, found exactly that: the confirmation rode the stream's cancellation, so a clean exit mid-probe came back `TimedOut=false`). What the fail-open direction costs is paid by an honest command a broken `top` can no longer tell from an overrun: one that finished on time but left a straggler holding its output open past the deadline. It reads as a timeout while `top` is down; a working `top` sees the command's pid already gone and clears it, so this misread costs a tool call, never a hidden overrun — a `top` *outage* fails toward the timeout label. (A command that SIGKILLs itself inside the 50 ms probe lead is a separate, unconditional cost of sampling a lead ahead of the deadline: the pre-deadline probe cannot tell a self-kill in that window from the watchdog's, so it reads as a timeout regardless of `top` — the probe-lead cost noted above, not a `top`-outage cost.) Where `top` can still mislead the other way is not an outage but a late-run `ps` answering as of after the command exited: that is the soft edge in the guarantee above, and the reserved cgroup limits, not this probe, are its containment.
- **What the deadline does *not* do: reclaim the container.** The kill is a process *group*, not a process tree, so a child that calls `setsid` escapes it; and when `Exec` stops waiting it abandons the exec — closing the HTTP stream is not a kill primitive. Either way a process the command left behind keeps running, and a sabotaging command can pin a core, until the session's container is destroyed. Every `Exec` call is bounded, so no session wedges, and no command that outlives its deadline escapes the timeout label. What escapes is a process detached from a command that *finished inside* its deadline — `nohup work & exit 0` is honest by every measure the sandbox has, and the work goes on. The real answer is cgroup limits (`PidsLimit`, `CpuQuota`) at provision time, which belongs with the container hardening item, not with the exec path.
- **A container is adopted only if the platform owns it.** `Provision` names a container after the session, so the name alone is not evidence: a container left by an earlier deployment, or one that collides, can wear it. Both adoption paths (an existing container, and the loser of a create race) check the `dev.opensdlc.managed-agent-platform.session-id` label first. The point is not only isolation — a container's **network mode is fixed when it is created**, so adopting a foreign container is how a `limited` session would quietly acquire a `bridge` container's route out, defeating the fail-closed rule. This is not a trust boundary against a hostile daemon co-tenant — the label is world-writable and forgeable by anyone with daemon access, who already owns every sandbox on the host — it defends against the accidents, which are the realistic failure on a single-tenant daemon.
- **404s are classified by the daemon's own message, anchored.** The archive endpoints echo the requested path into their missing-path error, so a substring search for "No such container" let a file *named* that turn its own missing-file error into a destroyed-sandbox error. The exec endpoints have a third 404, "No such exec instance", which is a lost exec and not a lost sandbox.
- **`Exec` itself is stateless per call.** Each `Exec` is its own `docker exec` of `/bin/bash -c`, so `cd` does not survive between calls at this layer. The reference's `bash` tool *does* persist state (`restart: true` resets it); that persistence is built one layer up, in `internal/sandbox/shell`, as a pure function of this stateless `Exec` plus the file primitives — deliberately, so the deadline `Exec` enforces from outside the container is inherited verbatim by every command.
- **A command that backgrounds a process without redirecting its output still costs ~2s.** The command's *result* is correct and its deadline is judged on its own life, but the call does not return until the daemon force-closes the stream the straggler is holding. `docker exec` behaves the same way; `cmd >/dev/null 2>&1 &` returns immediately. Cutting the read short as soon as the probes see the command exit is possible and was not done: it would drop output the command itself wrote and the daemon had not yet flushed, to save two seconds on a tool call the agent chose to shape this way. If it ever matters, it belongs behind the toolset's `bash` tool.

**Test coverage:** the contract suite runs against a real Docker daemon (a missing daemon is a hard failure, not a skip — as with `pgtest`), using `debian:stable-slim`: the official `bash` image does *not* have `/bin/bash` (its bash is in `/usr/local/bin`), which is exactly the assumption the image contract pins. It asserts the deadline kills the command's children along with it, leaves no watchdog behind, leaks nothing into the tool result's stderr, does not mistake a straggler for the command, and does not read a fast exit 124 — the code GNU `timeout(1)` gives a killed command — as a timeout of its own. The crown-jewel subtest tears down every guard the command can see — a watchdog beside it or below it, and the parent a prober might watch in its place — then holds a marker alive past the deadline, and asserts the command never both outlives its deadline and is reported finished; a third case overruns and then exits clean, leaving no marker to see, so the timeout has to be called from the overrun alone; a fourth mutes its own stdout and stderr and then overruns, because a reviewer argued a command could EOF the output stream early and cancel the probes by closing its own fds — measured against a real daemon, it cannot: the daemon holds the exec stream open until the *process* exits, so closing the container-side fds does not close it, the abandoned-plus-overran path still fires, and the command reads as a timeout. It passes against this backend and *fails* against the earlier fork-a-child wrapper, which is the whole reason it is in the shared suite rather than the Docker tests: a backend that enforces its deadline only inside the sandbox, or that watches a killable wrapper, fails it while passing every other assertion in the file. A scripted fake daemon over `httptest` covers what a real one will not reproduce on demand — and models an exec the way a real one runs it, with the process's life and the output stream's life set independently: image-missing → pull → retry, a lost create race adopting the winner's container, a container that wears the session's name without its label, a pull failure delivered inside a 200 stream, garbled and non-JSON replies, start/exec/inspect failures, an exec the daemon never stops calling running, context cancellation mid-poll, symlink and oversize and truncated archives, a `No such exec instance` 404, a path whose own name is `No such container`, the daemon's system-error stream frame, a command that overran and then exited *during* its own `top` probe with the stream closing mid-request, and the unix-socket transport itself.

Every guard is mutation-tested (disable it → exactly its own test fails). Seven rounds of review plus the mutation pass drove the deadline to where it is; the first six found real defects, all fixed here: a non-container 404 from `WriteFile` reported as `ErrNotFound`; the `/tmp` marker a sandboxed command could write to forge or erase its own timeout; a single-PID kill that left the command's children alive; bash's killed-job announcement landing in the tool result's stderr; a leaked watchdog `sleep` per timed command; `TimedOut` compared against the caller's deadline rather than the watchdog's rounded-up one; a path whose name is `No such container` turning its own missing-file error into a destroyed-sandbox error; a stale exec id read as a destroyed sandbox; the daemon's system-error frame folded into stdout as if the command had printed it; a container adopted by name without checking the ownership label; the overrun confirmation cancelled by the command's own stream close, so a clean exit mid-probe erased the overrun; and the two deadline probes run in sequence, so a first probe stalled on a slow `top` left the overrun probe unreached. The seventh round found a residual left in by design rather than a defect fixed here: `top` reports as of when the daemon ran `ps`, so a command that times a daemon `ps`-stall to land just after its own exit can hide a sub-`killGrace` overrun — accepted and documented (in the guarantee above, and as (7) below), with the reserved cgroup limits as its real containment. An eighth round — a fresh Codex (`gpt-5.5`/`xhigh`) and Claude pass on the final branch — turned up no new fixable bypass in the deadline. Its headline finding, a command muting its own stdout and stderr to EOF the stream and cancel the probes before they fire, was reproduced against a real daemon and *did not hold*: the daemon keeps the exec stream open until the process exits, so closing the container-side fds does not close it, and the muted overrun still reads as a timeout (now pinned by the fourth crown-jewel case). Its second finding is a narrow same-family limitation, not a fix: `execPid` polls on the caller's context before the probes are scheduled (anchored to `start`), so a daemon pathologically slow to report the exec's `pid` — slower than `deadline − probeLead` — can push the pre-deadline probe past the watchdog's kill, and a command the watchdog *did* contain reads `TimedOut=false`. Like the seventh, it is a degraded daemon weakening the sub-`killGrace` *label*, never the hard bound (the command was killed on time) and never a hidden overrun; the reserved cgroup limits are the family's real answer. The round's kept findings were otherwise doc and test hygiene: the restored fast-exit-124 guard above, the README daemon rationale, and a `top`-outage note that had wrongly implied a healthy `top` clears an honest self-kill inside the probe lead — it does not, that cost is unconditional.

Seven came from **the sandboxed command attacking its own deadline**. Six were reproduced and fixed; the seventh was reproduced and accepted as a documented residual. (1) Kill the watchdog, run 4.4s past a 1s deadline, report success — fixed by bounding the wait in `Exec`. (2) Once the bound existed, the narrower trick: kill the watchdog, overrun the deadline, and exit *inside* the grace window, so the bound never fires and the exit code is the command's own (measured **2.06s under a 1s deadline, `ExitCode=0 TimedOut=false`**) — fixed by the two probes. (3) The mirror of it, an *honest* command accused: a backgrounded process holding the exec's stdout made `sleep 30 & echo started` read as a timeout — fixed by timing the command's process, not its stream. (4) With the command running as a child of a wrapper shell, `kill $PPID` made the pid the probe watched vanish while the command ran on (measured **2s past a 1s deadline, `ExitCode=137 TimedOut=false`**, reported by two independent reviewers) — fixed by `exec`ing the command so the watched pid *is* the command. (5) Subtler still, once the probes matched the exec's pid: kill the watchdog, overrun the deadline, and exit *during* the overrun probe's own `top` request — the command's stream closes, `Exec` stops probing, and a confirmation riding that cancellation reads the command as finished and returns `TimedOut=false` for a real overrun (Codex, against the exec-wrapper commit) — fixed by running the overrun confirmation on a clock the stream's close cannot stop. (6) The sibling of (5), found re-reviewing that very fix: with the two probes run in sequence, a *first* probe stalled on a slow `top` was still waiting when the command overran and exited, so the stream's close cancelled the whole wait before the overrun probe was ever reached (Codex again) — fixed by running the two probes on independent clocks, so nothing can keep the overrun instant from being measured. (7) The limit behind (5) and (6): even with the probes independent, `top` reports as of when the daemon ran `ps`, not when the probe fired, so a command that overruns, exits inside the grace window, and induces the daemon to run its confirming `ps` only after that exit hides the overrun — bounded by `killGrace`, and only under attacker-timed daemon delay (Codex, re-reviewing the independent-probes fix). This one is accepted and documented rather than fixed: a robust fix means concurrent rapid polling with its own cost and failure surface, and the plan already reserves cgroup limits as the real containment for a command that abuses the daemon — limits that also cap the process pressure needed to stall `ps`. (1)–(4) were each closed with a contract test that fails against the vulnerable code, binding every backend; the general invariant behind (5) and (6) — overran, then exited clean — is a shared subtest too, but the specific `top`-probe races are ones a shared test cannot stage against a real daemon, so they are pinned by Docker unit tests. A verifier pass asserted (2) did not exist and a reviewer proved it did; a verifier pass certified the fix for (1)–(3) and a reviewer found (4) in it; a verifier pass certified (4) and Codex found (5) beside it; a verifier pass certified (5) and Codex found (6) in the re-review; a verifier pass certified (6) and Codex, re-reviewing that fix, found (7) — the point at which the tool reaches the limit of what `top` can witness, and the residual is documented rather than chased. The pattern is why the deadline is pinned by adversarial tests that fail on the vulnerable code, not by a verdict.

The last round found the defect from the other direction — an honest command **accused** of a timeout. A reviewer reasoned that a backgrounded process holding the exec's stdout would stall `Exec` for the full grace period and make it report `TimedOut` and SIGKILL. Reproduced against a real daemon, the mechanism was wrong (the daemon force-closes such a stream after ~2s) but a worse bug was underneath: `sleep 30 & echo started` under a **one-second** deadline returned `ExitCode: 0` together with `TimedOut: true`. Redirecting the straggler's output made it vanish, which named the cause. The fix retired the stream as a clock. Its replacement was chosen by measurement, not by reading the API: the daemon's `Running` flag turned out to track the stream too (2.06s versus the process's real 40ms), and only `top` tracks the process. The mutation pass separately caught a *test* that passed for the wrong reason — the caller-cancellation case tripped during `execStart` and never reached the branch it claimed to pin.

### `internal/sandbox/shell` — the persistent bash tool (slice 6, second part)

Known limits, each an accepted cost rather than a fix (the delivery narrative and the
divergences-from-a-resident-shell list are in CHANGELOG.md § 0.1.0):

- **Snapshots and command files accumulate.** Every call writes both and nothing prunes them; `restart` prunes nothing either. The container is per-session and disposable and each snapshot is a few KB, so a long session costs tens of MB and a destroyed sandbox takes it all with it — a garbage collector is not worth its failure modes yet.
- **A `Run` that fails *after* the command ran discards the command's output.** The snapshot probe or the `head` write against a broken container returns the error with an empty `Result`. That is deliberate: the alternative hands the caller a transcript for a call whose state the next call will not see, which invites treating it as committed. Both reviewers raised it; it stays a documented choice rather than a fix.
- **Concurrent `Run` calls on one session race on `head`.** Two in-flight calls snapshot into separate directories and the last to commit wins. The shell package itself does not lock `head`; serializing a session's bash calls belongs with the caller. (Since partly superseded: the executor now serializes a session's `tool_exec` work per-(session, kind), so the platform's own path cannot race — the unlocked `head` remains a property of the package.)
- **A cwd whose name ends in a newline is not round-tripped.** The snapshot stores the cwd with `pwd` and restores it through `$(<file)`; command substitution strips *all* trailing newlines, so a directory whose own name ends in one is restored a byte short and the `cd` lands elsewhere or is skipped. It is a pre-existing property of the substitution (the earlier `$(cat …)` behaved identically), it costs only cwd (exports, functions, aliases, options are unaffected), and a newline-terminated directory name does not occur in real use — reading the value byte-exact to handle it is machinery a hot, safety-critical path does not warrant. Codex raised it.

Every guard in the package is mutation-tested — disable a guard and exactly the test that
pins it fails. (The full mutation matrix was pruned with the delivery narrative; the
tests themselves are its ground truth.)

### `internal/toolset` — the built-in tools (slice 6, third part)

Two accepted-cost notes recorded nowhere else:

- **`singleQuote` is a third copy** of the POSIX single-quote escape (`sandbox/docker` and `sandbox/shell` have their own). Each is one line and independently tested; a shared home would couple three sandbox-tree packages for a trivial helper, so the duplication stays, as the shell package's own note already accepts for its copy.
- The glob pipeline's failure handling is carried on reasoning rather than a failing-on-the-old-code test: a broken pipeline (a `stat` without `--printf`, a missing tool) and a mid-listing `stat` race are both non-deterministic to stage against a real GNU-coreutils container, so `pipefail`'s "never a silent no-matches" guarantee rests on the happy-path glob tests proving the NUL-delimited pipeline works plus the argument that any masked mid-pipeline failure would otherwise read to the model as an empty directory. A second reviewer (Codex) confirmed the masking behavior a `pipefail`-less pipeline would have had, which is why `pipefail` is kept and the per-file-race-errors-conservatively cost is accepted.

### `internal/executor` + `cmd/executor` + brain `agent_toolset` expansion — the closed loop (slice 6, fourth part)

### permission policies + the confirmation round-trip (slice 7)

Two records CHANGELOG's entry does not carry — a liveness design decision, and the
review-hardening acceptance record (each correctness fix landed with a test that fails on
the vulnerable code):

**Liveness — a gated turn re-idles rather than chains, and that is deliberate.** Both the brain's suspend and the API's partial re-idle commit under the session row lock with no chain-or-idle check (unlike `end_turn`): a session blocked on human confirmation is genuinely waiting on input, and a `user.message` that arrived mid-turn stays unprocessed and rides the next replay once the gate clears — it is never lost, and the session is not spuriously woken past a pending approval. This mirrors the slice-6 running-suspend, which also relies on replay-on-resume for mid-turn input.

**Review hardening (same PR, from the dual review + verifier).** A Codex (`gpt-5.5`/`xhigh`) pass, the verifier (opus), and the Claude review (also `gpt-5.5`-class) converged; seven gaps — six correctness, one cleanup — were fixed here, each correctness one with a test that fails on the vulnerable code:
- **A `user.message` no longer bypasses the confirmation gate.** Slice 7 is the first to leave a session *idle* while it still carries an unanswered `agent.tool_use`, and the `user.message → running + model_turn` trigger did not check for that: a message posted while awaiting approval woke a turn whose replay hands the model an assistant `tool_use` with no matching result — a request the Messages API rejects, producing a spurious `session.error`. `POST /events` now computes the session's unconfirmed-ask blocking set up front (`UnconfirmedAskEvents`) and a `user.message` resumes only when it is empty; while gated the message appends and rides the next replay once the gate clears. The confirmation case is checked **first**, so a batch mixing a gate-clearing confirmation with a `user.message` runs the confirmed tool rather than waking on the message past it. (All three reviewers found this independently — the strongest signal in the pass.)
- **A tool result can no longer answer an unconfirmed ask tool.** `ValidateToolResults` accepted a `user.tool_result` for an ask-gated `agent.tool_use` (it only checked kind and already-answered), so a self_hosted client could answer a gated tool before approval — bypassing the human gate and, on a later denial, double-answering the tool use on the append-only log. It now rejects a result for an ask-gated tool that has no confirmation. (Codex.)
- **A malformed toolset is a 400 at agent creation, not a turn-time wedge.** With `Tools` now resolving `permission_policy` (via the shared `resolveToolset`), an agent whose `agent_toolset` carried a bad policy — or a malformed `enabled` — was accepted at create (`parseTools` treated the entry as opaque) and then failed *every* turn when the brain resolved it, an unusable agent with no create-time error. `parseTools` now calls `toolset.Validate` on each `agent_toolset` entry, so the resolver's own check runs at creation. (Claude.)
- **A denied gate no longer provisions a sandbox for nothing.** The confirmation resume chose `tool_exec` vs `model_turn` from `HasUnansweredToolUse`, which counts client-executed `agent.custom_tool_use` as unanswered — so denying an ask tool while a custom tool was still outstanding enqueued a `tool_exec` the executor would claim, provision a container for, and find no built-in to run (a wasted provision; an infinite reclaim if provisioning failed). The resume now enqueues a `tool_exec` only when an allowed *platform* tool is unanswered (`HasUnansweredPlatformToolUse`); when the only remaining work is a custom tool it enqueues nothing and waits for the client's result, and when every tool is answered it resumes the brain — mirroring the non-ask suspend, which never runs an executor for a custom-only turn. (Claude.)
- **Confirmation gating is scoped to `agent.tool_use` only.** `confirmableToolUseTypes` also listed `agent.mcp_tool_use`, but the denial synthesis emits an `agent.tool_result`/`tool_use_id` — the wrong shape for an MCP tool (`agent.mcp_tool_result`/`mcp_tool_use_id`). MCP is not gated in v1 (the brain stamps a policy on nothing else), so the speculative entry was removed; gating MCP is slice-8+ work that must extend the denial synthesis with it. (Codex.)
- **Policy validation is uniformly lazy.** `resolveToolset` validated `default_config.permission_policy` eagerly while per-tool policies were validated only for enabled tools, so a malformed default rejected an agent even when every enabled tool overrode it or the toolset was off. Now a policy is validated only for a tool that actually resolves into the enabled set — a malformed policy on a disabled or overridden-away tool has no effect and is ignored. (Codex.)
- **(cleanup) One `classify` pass replaces the parallel `classifyTools`/`classifyPolicies`.** Both re-ran `resolveToolset` over the same entry each tool-calling turn, and `classifyTools` marshaled every built-in definition back to JSON just to recover its name; `classify` now resolves once and reads names from `toolset.Policies`' keys, so the two maps (name→event type, name→policy) can no longer drift. (Claude.)

### work API — environment-key auth + `/work/poll` (slice 8, first part)

### work API — the work-item lifecycle: get / ack / heartbeat / stop (slice 8, second part)

### work API — environment-key auth on a session's worker-facing routes (slice 8, PR C1)

### work API — the work-items list (slice 8, PR C-list)

### BYOC worker — the tool-exec driver over HTTP (slice 8, PR C2a)

### BYOC worker — the lease loop + `cmd/worker` binary (slice 8, PR C2b)

### BYOC worker — `traceparent` propagation across the process boundary (slice 8, PR C2b-2)

### work API — the work-item metadata update (slice 8, PR C-meta)

### work API — the queue-stats endpoint (slice 8, PR C-stats)

## Kubernetes sandbox provider (slice 9)

Delivery narrative: the four slice-9 entries in CHANGELOG.md § 0.1.0 (Kubernetes sandbox
provider; config-driven backend selection; Helm chart; the compose stack). Per-file
reference: [ARCHITECTURE.md](./ARCHITECTURE.md) § "Package reference".

---

## Harness decisions — evaluated and rejected (2026-07-16)

Reviewed against the loop-engineering playbook (state file / objective gate / skills /
automations) with all v1 slices done. Rejected: a gofmt PostToolUse hook (near-zero value
over CI's gate), a fifth code reviewer (four checkers + CI already run per PR; the
marginal yield is unmeasured), and a standing LESSONS.md ledger (CHANGELOG, this archive,
and CLAUDE.md already cover its content). `/loop` and git worktrees were **not**
rejected — they are planned future practice. This restructure put the first prerequisite
in place (the slim state file); a single executable gate (`make verify`) and an on-demand
review skill land in follow-up PRs.

---

## Eval test system (phase 1, #30)

The first test that drives a whole session the way a customer does — public REST → brain → a real model → work queue → executor → Docker sandbox → SSE idle — and grades the transcript deterministically. Every other loop test in the repo scripts the provider; nothing before this exercised the real product path, and #30 also flagged that `.env` presence alone was triggering a paid model call during an ordinary `go test`.

**Harness form.** A top-level `evals/` package of `*_test.go` only — no runner binary, because `go test` already gives subtests, timeouts, `-v`, and panic-safe cleanup. `TestEvals` composes the platform in one process exactly as `cmd/*` do: a `pgtest` Postgres, the real `api.NewHandler`, a `provider.Registry` routing `*` to the `.env` endpoint, and live `brain` and `executor` loops against `docker.New`. Only `main()` glue is bypassed (CI's compose job smokes that). A hand-rolled REST client speaks `map[string]any`, never the domain structs, so a wire regression a struct tag would round-trip past stays visible to a grader. The suite is opt-in through `RUN_EVALS`, a second gated tier in `internal/modeltest` alongside the provider smokes' `RUN_LIVE_MODEL_TESTS`: unset skips (an ordinary `go test ./...` makes zero paid calls even with `.env` present — the behaviour change #30 demanded), set-but-misconfigured fails rather than skips. A `TierEnabled` answers the one caller a `*testing.T` skip cannot serve — `TestMain`, which starts Postgres before any test can skip.

**Grading is deterministic and code-based**, never an LLM judge — a judge's own drift is indistinguishable from the drift the suite exists to catch. Each prompt demands a per-trial random nonce, so an exact-match check tests the agent rather than the grader's generosity. Every trial runs a core pack (reaches idle with `stop_reason.type == end_turn`; no `session.error`; every `agent.tool_use` joined by exactly one `agent.tool_result`; token usage populated; the idle observed on the SSE stream), and each finding is classed **P**latform (our bug, a red run to fix), **M**odel (the model wandered), or **E**ither, so a red run says whose problem it is rather than "probably the model". Artifacts land in `evals/artifacts/`: a `report.json`, a `summary.md`, and one transcript per failed trial — including an aborted one, fetched best-effort in the deferred recorder because a drive timeout is the failure triage most needs. The report reduces the endpoint to host:port and scrubs known secrets from every rendered artifact, because a credential in `MODEL_BASE_URL`'s query would otherwise ride a transport error's quoted URL onto disk.

**The ten tasks** exercise the built-in toolset at two strengths — `edit`, `grep` and `bash`'s failing command by a result contract tying a call to its own output, `read`'s slice byte-exact, and `bash`, `read`, `glob` and `write` on a required tool-use floor (write's effect further pinned by its written artifact, glob invocation-only since a bare path list has no stable order to pin — since tightened by #98 and #99) — single and multi turn, allow and deny, seeded and unseeded, with three negatives. Tasks 1–3 (`fib-quickstart`, `echo-notool`, `shell-state`) landed with the harness; tasks 4–10 added three mechanisms. **Seed planting** writes files into the session's container before turn 1 by pre-provisioning it; the executor adopts that same container (by session label) when it runs the first tool, so the agent sees the seeds — used by `edit-config`, `needle-search`, `perm-deny`, `view-range`. **Gated toolsets** (`always_ask` via `default_config`) and a **confirmation-aware drive loop** power the permission pair: `perm-allow` and `perm-deny` drive the bridge end to end — a gated tool suspends the session on a `requires_action` idle, the loop posts a `user.tool_confirmation` (allow or deny) referencing the event id `requires_action` named, and grading pins the pause, the `evaluated_permission == "ask"` stamp, the result sequenced after the approval, and — on deny — the synthesized `is_error` result carrying the deny message with the seeded file left untouched. `exit-code` pins a failed command's `exit code:` trailer, correlated to the failing call's own result (the load-bearing check; the model's reported code is a weaker secondary signal, since cat of a missing file conventionally exits 1); `journal-multiturn` pins event replay and sandbox reuse across two turns (the executor adopts the session's container by construction); `view-range` pins `read`'s `view_range` slicing byte-for-byte as an off-by-one guard.

Prompts are written the way the docs tell a user to write them — a prompt tuned until only our platform's quirks satisfy it stops being a regression test. Two that a refusal-prone live model (MiniMax-M3) balked at were reworded to exercise the platform rather than trip a safety reflex: `perm-deny`'s "delete a file in a `protected` directory" became a benign append the reviewer declines, and `view-range`'s "SECRET" marker copied "to another file" — which the model read as exfiltration — became a plain marker copied to a file. All ten run 10/10 green live.

Deliberately deferred and filed as issues: a daily scheduled CI run ([#96](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/96), needs repo `MODEL_*` secrets), `tool_choice`/`disable_parallel_tool_use` for phase 1.5 (on #30), and production sandbox reaping ([#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64)). The harness reaps its own containers at stack teardown.

---

## Work Stop 204 (plan 04) — archived 2026-07-20

[docs/plan/04_work-stop-204.md](./plan/04_work-stop-204.md), delivered in one PR (**#122**). The
change and the reasoning behind it are the CHANGELOG § [Unreleased] entry; recorded here is only
what a changelog cannot hold.

**The generalizable lesson: "confirmed" did not mean re-derivable.** The registry's entry was not a
guess — it was CONFIRMED, and the measurement behind it reproduces to this day. What it lacked was a
check that the thing measured was the thing claimed. Two rules fall out, both wider than this
endpoint: a client-side workaround shipped by the reference SDK is evidence *for* the behavior it
works around, never against it; and a CONFIRMED entry earns re-derivation whenever the change under
review depends on it, because the artifact that confirmed it may never have supported it.

**Evaluated and rejected — keeping 200 + JSON as a deliberate leniency divergence.** Adversarial
review made the case seriously, and it was measured rather than asserted: 200 + JSON satisfies a
strict superset of clients (the generated method decodes it; the bypassing helper and the `ant` CLI
tolerate either shape). Rejected because "superset of clients" is the wrong objective for a
compatibility layer — the extra consumer it buys is one already broken against Anthropic's own
service, so the leniency bought compatibility with this platform rather than with the reference. Two
reviewers reached this position independently and both were overruled on that ground; recorded here
because the argument is a good one and will recur the next time a divergence looks harmlessly
permissive.

**What the review round corrected — including an error of ours.** CLAUDE.md ranks *public docs*
above the reference checkouts, and the plan asserted the typed schema ranked first. That was simply
wrong, and it mattered: the top-ranked source disagrees with the change. The conclusion survived
(the spec-side witnesses are one witness, not three), but the framing had to change from "not a
divergence" to a deliberate divergence from the published spec, left open for a recording (#78) to
close, with the compatibility break stated rather than glossed. Separately, the plan's aside that
the empty-poll response might follow Stop to 204 had its evidence backwards — the poller calls
`Work.Poll` with no bypass and its empty-queue branch needs a decoded body — so `200` + `null`
stands. Both corrections came from reviewers reading the primary sources rather than the plan.

**Why the mutation check was the load-bearing evidence.** Asserting the resulting work state cannot
catch a missing decoder bypass: the stop succeeds server-side either way, and the only damage is a
fictional `worker: force-stop failed` on every clean finish — invisible to a suite that checks the
database. Only removing the bypass and watching `TestWorkerForceStopAcceptsNoContent` fail with the
SDK's quoted error string proves the test constrains anything. A green suite was not evidence here;
a red one was.

---

## Docs restructure (plan 03) — archived 2026-07-18

[docs/plan/03_docs-restructure.md](./plan/03_docs-restructure.md), delivered complete in
three PRs (narratives in CHANGELOG § [Unreleased]): **#101** — docs/ARCHITECTURE.md
created as the as-built reference and HISTORY.md slimmed 530 → 217 lines under the
one-writer rule (verifier ×2, Codex 17 findings, /code-review 5 — all resolved against
source); **#102** — STATE.md reduced to a pure active-work tracker (63 → 23 lines), the
verifier's STATE checks rewritten; **the issue-triage PR** — `.claude/agents/issue-triage.md`
(Sonnet 5, read-only, strict-JSON `needs_plan` judgment) with its CLAUDE.md trigger rule,
archiving this plan.

---

## K8s silent short write (#103/#86) — investigation record (2026-07-19)

Narrative and the defect itself: CHANGELOG.md § [Unreleased]. Recorded here is only what a
changelog cannot hold — a refuted mechanism, the alternatives rejected, and the verification record.

**A confidently-argued mechanism that was wrong.** The first account blamed client-go teardown:
`StreamWithContext` returning before `cat` exits, letting `defer conn.Close()` cut stdin. Refuted at
source — `v4.go`'s `wg.Wait()` then `return <-errorChan`, gated by `io.ReadAll(errorStream)`, cannot
return before the remote process terminates; and `WriteFile` passes a non-nil stderr, so `cat`
holds fd 2 regardless of the fd 1 redirection. Recorded because CLAUDE.md warns that reviewers and
investigators produce exactly this kind of plausible, well-cited, wrong finding.

**Evaluated and rejected.**
- *Swapping `NewSPDYExecutor` for the WebSocket or fallback executor.* Measured, not assumed: with
  the old `exec cat` script the WebSocket executor lost the same 1 MiB payload 14/15 times (SPDY
  15/15). The loss is transport-independent, so the swap fixes nothing and would change every exec
  in the backend.
- *Dropping `exec` alone.* Empirically sufficient, but it rests on an inferred mechanism nobody
  instrumented. The length check makes the guarantee hold whatever the layer.
- *Verifying by re-reading the target (`wc -c < "$1"`).* Landed first and caught in review by both
  reviewers independently: re-reading asks what the path holds now, not what the stream delivered,
  so it fails a **successful** write to `/dev/null` or any device node (reproduced: old exit 14, new
  exit 0), to a file the sandbox user may write but not read, or to a path another process in the
  sandbox is touching — and it is TOCTOU besides. Replaced by counting the stream itself
  (`tee "$1" | wc -c`), which measures exactly the quantity that went missing.
- *A `sandbox.ErrShortWrite` sentinel.* No caller distinguishes it, and `sandbox.go` already commits
  the package to all-or-nothing semantics; a sentinel would legitimize the outcome it rejects.
- *Mirroring the check on the read side.* `ReadFile` has the same structural hazard (exit 0 + short
  stdout reads as success) and `readScript` already computes `sz`, but the evidence rules it out as
  the cause here and a naive fix false-positives on a file rewritten between the `stat` and the
  `cat`. Filed as #105 rather than widening this diff — closed there by marking the end of the
  stream rather than counting it (record below).

Also left out deliberately: `cat > "$1"` truncates the target before the first byte arrives, so a
detected short write still leaves a truncated file on disk. Making the write all-or-nothing is
already tracked by #71 (atomic `WriteFile`), and doing it here would widen this diff into that one.

**Local verification blocker.** The K8s contract suite could not be run to green on the development
machine: it fails with transport `EOF`s on unmodified `main` too (17/21/8 subtests), so it is
environmental. Node restart, cluster recreate, image sideload, a Docker Desktop restart and a
connection cooldown all failed to restore it; `kubectl exec` stays ~93-100% while the Go process
breaks under provision churn, and it is not FD limits (`ulimit -n` 1048576) or TIME_WAIT exhaustion.
Targeted evidence stands on its own (the two new tests, the #103 subtest, and the docker backend on
the same shared subtest all pass); CI's fresh kind cluster on Linux is the gate.

---

## K8s read-side short-read guard (#105) — design record (2026-07-19)

The hazard and the guard: CHANGELOG.md § [Unreleased]. Recorded here is only what a changelog
cannot hold — what was measured and rejected, and the sense in which nothing was reproduced.

**Nothing was observed.** Roughly 35 reads of 1 MiB, 4 MiB and 20 MiB files through the pod-exec
stream produced zero silent short reads. Three runs failed with a transport `error: EOF` delivering
**zero** bytes and a non-zero exit — the same environmental flakiness the #103 record above
describes, which `client.exec` already surfaces as `err != nil` rather than a `streamResult`, so
`ReadFile` already errored on those. The guard closes a structural hazard, not a reproduced failure.

**Evaluated and rejected.**

- *Comparing the bytes received against `readScript`'s existing `sz`.* The fix the issue asked for a
  decision on, and worse than the issue argued. Beyond the file rewritten between the `stat` and the
  `cat`, it breaks every procfs read: measured in-pod, `/proc/self/status` reports a `stat` size of 0
  while `cat` streams ~1.1 KB, so an ordinary `/proc/meminfo` would come back as a short read. It is
  also a re-read of the target, the mistake #103's review rejected.
- *Counting the stream on stderr (`cat "$f" | tee /dev/fd/3 | wc -c` under `pipefail`, the literal
  mirror of `writeScript`).* Byte-exact in every probe — 0 B to 20 MiB, as root and under
  `runAsUser: 65534` — and rejected on portability, not correctness. `/dev/fd/3` is a reopen rather
  than a dup: against a socket descriptor it fails with `ENXIO`, and under an in-pod uid transition
  (`su`, `setpriv`) with `EPERM` (measured), so every read in the backend would depend on what kind
  of stdio a container runtime hands an exec — an invisible dependency in the backend whose whole
  purpose is customer clusters nobody here has seen. Its one advantage over a marker, catching a hole
  in the middle of the stream, has no reachable input: client-go copies stdout with a single
  `io.Copy` (`streamProtocolV2.copyStdout`), which stops at its first error, so every loss is a
  suffix truncation. It also adds a second stream whose independent damage becomes a false positive
  on an otherwise good read.
- *A fixed marker constant instead of a per-call `nonce()`.* Cheaper, and it puts the literal in the
  repo — in `k8s.go` and in this file — so files a sandboxed agent routinely reads would contain it,
  turning a negligible false negative into one with instances on disk. `nonce()` already existed, is
  `crypto/rand`, and rides in argv.
- *A `sandbox.ErrShortRead` sentinel.* Same answer as `ErrShortWrite` above. A plain error falls
  through `toolset`'s default arm to the executor as a backend fault, which is where a retriable
  transport failure belongs — the model must not be handed a truncated file to route around, least
  of all through `edit`, which writes back what it read.

**Stated as inference, not instrumentation.** That a marker suffices rests on reading client-go's
stdout copy and concluding no mid-stream hole can reach the buffer. Nobody induced one; there is no
way to. This file already records that a confidently-argued, well-cited claim about this exact
transport was wrong once.

**Verification record.** Each half of the guard was proven able to fail, in throwaway copies rather
than by assertion: reverting `readScript` to `exec cat "$f"` turns `TestReadScriptMarksWhatItSent`
red; reverting the read buffer's room to `MaxFileBytes + 1` turns the live `ReadFileAtTheCap` red;
and flipping one byte while keeping the length turns it red at that subtest's content comparison
specifically. The procfs case that ruled out the `sz` comparison was measured in a real pod, not
argued: `/proc/meminfo` stats as 0 bytes and streams 1392. The gate split as the blocker above
predicts — locally `make verify` is red only in `internal/sandbox/k8s`, reproduced on unmodified
`main` in a fresh worktree, while CI's kind cluster ran that package to `ok` in 51.0s with the full
contract suite. Coverage 91.38-91.48% across runs.

## `internal/events/toolflow.go` characterization suite — verification record (2026-07-19)

Narrative: CHANGELOG.md § [Unreleased] → Added. Recorded here is only the verification, plus one
claim this project made and then had to retract.

**A claim of ours that was wrong.** The first version of the changelog entry said the two
`if extraRefs == nil { extraRefs = []string{} }` normalizations were "previously unguarded by any
test". Both reviewers challenged it and the measurement refuted it. With the new file absent, removing the
`extraRefs` line turns `./internal/brain/` red (`TestParallelToolCallsResumeOnFullSet`) *and*
`./internal/api/` red (five confirmation tests); removing the `extraConfirmed` line leaves
`./internal/brain/` green and turns `./internal/api/` red
(`TestConfirmationUserMessageDoesNotBypassGate`) — `brain_test.go:155` calls `HasUnansweredToolUse`
directly, and `internal/api/confirmation_test.go` covers the `UnconfirmedAskEvents` side. The trap is real and the
new tests name it directly; the novelty claim was not. Recorded because the wrong claim was the
entry's headline, and it survived self-review.

**Verification record.** Each case was proven able to fail by mutating `toolflow.go` in place,
running the suite, and reverting — never by assertion. Twenty-two single-edit breakages in total.
Caught on the first pass: dropped `r.session_id` / `c.session_id` predicates, `ORDER BY tu.seq` →
`tu.id`, both nil normalizations removed, `seen` re-keyed by kind+id, `wantUse` pinned to
`agent.tool_use`, the ask gate deleted, the platform variant widened to every tool-use kind or made
to ignore `extraRefs`, `confirmableToolUseTypes` widened in either query, the `Scan` error check
dropped, and the COALESCE reduced to a single key.

Seven survived the first suite and each is now closed by a named subtest, with the same mutation
re-run to prove the closure: the second and third `COALESCE` arms trading places in
`hasUnansweredToolUse` and in `ValidateToolResults`, and — caught only by the re-verification pass,
after the first fix drove the (1,3) and (2,3) pairs but not (1,2) — the first and second arms trading
places in `hasUnansweredToolUse` (every existing leg carried one key, or compared
only the first arm against a later one); the `c.type` predicate in `ValidateToolResults`' ask gate
and in `ValidateToolConfirmations`' already-confirmed check (no fixture had ever put a second event
carrying the gated id beside the confirmation); `ValidateToolResults` swallowing its payload-decode
error; and the ask gate restricted to built-ins. For the last of these the closure was double-checked
in both directions — with the new subtest removed the mutation goes green again, with it present that
subtest is the one that fails.

**Two mutations that cannot be caught, and were not chased.** Dropping `c.type = $4` outright leaves
`$4` unbound, and pgx rejects the query at run time (`expected 3 arguments, got 4`) — the suite fails
loudly on it, so it is a broken query rather than a silent regression.
Widening that same predicate to admit `user.tool_result` is unobservable through the ask gate: a
`user.tool_result` carrying the id makes the tool use *answered*, and the answered check fires first,
so both implementations return the same error. The observable form of the widening — admitting a type
that carries the id without answering it — is what the new fixtures use.

**Method note.** Two intermediate results in this audit were false positives from a careless harness
and are recorded so the next audit avoids them: a `grep FAIL` verdict counted build failures and SQL
syntax errors as "caught", and a mutation pattern indented with two tabs matched *both* confirmation
subqueries as a suffix of the three-tab one, so an occurrence-0 edit silently hit
`ValidateToolResults` twice while `ValidateToolConfirmations` was never mutated at all. Every verdict
above is from the corrected harness, which vets the build and distinguishes the two sites explicitly.

## Provider credential redaction (#83) — review-hardening record (2026-07-20)

What [CHANGELOG.md](../CHANGELOG.md)'s entry describes as one coherent redaction arrived through
three review rounds, each of which found the previous round's fix incomplete in the same way. The
record is kept because the *pattern* is reusable and the CHANGELOG cannot hold it: every gap was a
rendering of the credential nobody had thought to enumerate, and two of them were hidden by test
fixtures chosen for readability.

**Round 1 — verifier, after the first commit.** Confirmed the five leak paths closed and every test
failing-first, then found three residuals. An unparsable `base_url` leaked its password because
`NewRedactor`'s own `url.Parse` failed, so the site's comment claimed a coverage it did not have and
both of its blocks were unexecuted by any test. `isAuthHeader` missed `apikey` — no separator, so
none of the substring rules matched — which is Kong's key-auth default and Supabase's convention.
And the 4 KiB quote cap could sever a credential mid-token: demonstrated leaving 8 characters of the
key in the message, matching no registered secret.

**Round 2 — Claude review panel, six lenses with three adversarial refuters per finding.** Seventeen
findings were refuted under verification; three survived, all the same defect found independently by
three lenses, with 3/3 refuters failing to refute it and an end-to-end reproduction attached:
`url.Parse` stores a `base_url` password decoded while `url.URL.String()` re-encodes it, so
registering the decoded form matched nothing for any password containing a character RFC 3986
requires be escaped in userinfo. **The regression test passed only because its fixture,
`pw-secret-456`, was URL-safe — the single class of password for which the two renderings
coincide.** Re-verification then found a fourth rendering by the same reasoning: `net/http` derives
an `Authorization: Basic` header from userinfo whenever the request carries none, which is always
under the anthropic protocol, so an auth-echoing endpoint quotes the credential base64-encoded.

**Round 3 — external reviewer (Codex `gpt-5.6-sol`), reading only the first commit.** Independently
reproduced the same conclusions and added four. Three were leaks: custom auth header names
(`X-Auth`, `X-Signature`, `X-Credential`); userinfo carrying a username and no password, the
token-as-userinfo convention, where the username *is* the credential rather than an identifier
standing beside one; and `resp.Status`, which HTTP/1 lets a server fill with arbitrary text, being
interpolated unredacted beside the body that was not. The fourth was an over-redaction the fix had
itself introduced: splitting a header value on any space registered the second word of a value that
is not a credential pair, so `x-route-key: "pool alpha"` blanked "alpha" out of every diagnostic
naming the pool — the opposite of the carve-out's purpose.

**Decisions evaluated and rejected.** Shape-matching `Bearer`/`Authorization`-looking tokens, which
the issue floated, was rejected on evidence: the observed anthropic echo was a bare value with no
scheme prefix and no header name beside it, so a shape matcher would have missed the very leak the
issue was filed for, and `base_url` may point at any gateway whose token format is unknowable.
Chasing a credential re-encoded by Go's HTML-escaping JSON encoder was rejected as the same
speculative pattern-matching, and buys nothing against an endpoint that transforms deliberately.
Redacting a model's *successful* output was rejected as a category error — model output is a trusted
boundary, and scrubbing it would corrupt the content the session exists to record. Redacting a
`base_url` **username** when a password stands beside it was rejected because a username identifies
rather than authenticates, and masking it costs a diagnostic to hide nothing.

**Method note, worth repeating.** Two tests in this change passed against the unfixed code before
being corrected: the base_url fixture above, and a truncation test whose padding arithmetic left
only 3 characters of the key inside the quote budget while its assertion checked runs of 5 or more.
Both looked like coverage. The habit that caught them — and that the next change should copy — is to
overlay new test files onto a `git archive` of the *previous* commit and confirm each fails **for
the intended reason**, not on a nil error, a panic, or a build failure.

---

## Eval grader rigor (#99) — review-hardening record (2026-07-20)

[CHANGELOG.md](../CHANGELOG.md)'s entry says what changed. Kept here is what a changelog
structurally cannot hold: the acceptance evidence, and the two occasions on which a
confidently-argued claim of mine was refuted by someone checking it.

**The meta-defect three reviewers converged on.** Codex, `/code-review` and the verifier
worked independently and every confirmed finding had the same shape: **a grader whose own
behavior no mutation of itself could catch.** Deleting `ConfirmedResult`'s
dangling-confirmation join, or reverting `EvaluatedPermissionAsk` to first-call-only, left
the whole unit suite green. A grader that cannot fail when broken is not a test, and the
suite had several. Mutation testing became the acceptance bar for the rest of the work: a
behavior counts as pinned only when breaking it reds a test that *names* that behavior.

**Mutation matrix at the final state** — 17 probes, 16 killed (15 by the unit tier, 1 by a
live run), each run in a throwaway `tar`/`git archive` copy, never the checkout. Killed:
`ConfirmedResult`'s dangling-name join, its empty-`tool_use_id` reject and its content
check; `CallResult`'s terminal missing-`is_error` and its resultless-sibling skip;
`GlobPathList`'s empty-success reject, missing-`is_error` reject and absolute-path check;
`NotInToolTraffic`'s encoded scan, decoded scan and tool-result scan; `OnlyIf`'s
every-premise semantics; `fill`'s `{{RECALL}}` substitution; `EvaluatedPermissionAsk`'s
every-call sweep; `toolCallsWith`'s marker filling and its decoded-input matching.

**The one survivor, and why it is not a gap.** `FileLines` filling tokens into its *path*
survives everything. The reason is not a missing test: every `FileLines` call site passes a
token-free literal path, and the one tokened path is graded by `FileEquals` — so `fill` is
the identity there and the mutant is **equivalent** on the current task set, unkillable by
any run. The code stays as correct defensive symmetry with `Seed` and `FileEquals`. The
claim that *is* provable was proved by running it: mutating `FileEquals`'s path fill reds
`journal-multiturn` live with `file-equals:/tmp/provenance-{{NONCE}}.txt: sandbox: no such
file`. Recording an equivalent mutant as an equivalent mutant, rather than as a killed one,
is the point of the entry.

**Live acceptance: five runs, and the pattern beats the score.** Across five `make eval`
runs against a real endpoint (MiniMax-M3) at successive revisions — 10/10, 9/10, 10/10,
then after merging `main` 9/10, 10/10 — **no Platform-class grader fired even once.** The
two reds were `journal-multiturn`'s `file-lines` (Either: the model wrote turn 1 without a
trailing newline, so the appended line concatenated) and `perm-deny`'s `tool-called-with`
(Model, with zero tool calls: the model never ran the instructed command). That pattern,
not any single 10/10, is the evidence the classing works — a live model wandering produces
Model and Either reds, never a Platform red. Reporting the two 9/10 runs rather than
re-rolling to a clean sweep is part of the record.

**Two defects the live suite found that no reviewer did.** Substitution was split across
two helpers, so a grader searched for a literal `{{RECALL}}` while the model had said the
code back correctly — a token live on one side of a check and literal on the other is not a
bug a unit test written against the same misunderstanding will catch, so the two spellings
were merged into `(*Trial).fill` rather than documented. And the recall prompt called the
token a "code word" and forbade writing it anywhere, at which the model **refused** turn
two, reading the pair as a secret and the request to repeat it as an attempt to extract it
— the same trap `view-range` already avoids by not calling its marker a SECRET. A prompt
that sounds like a confidentiality rule tests the model's refusal reflex, not the platform.

**Two of my own claims were wrong, and neither was caught by me.** The verifier refuted a
mutation-verification claim I had made about `ConfirmedResult`'s dangling join: my probe
predated a structural check I later added, so by the time I reported it the mutation
survived for a different reason than the one I had tested. It also refuted my statement
that the `FileLines` survivor was "pinned by the live suite" — it is an equivalent mutant,
as above. Separately, a finding *I* raised in self-review — that `OnlyIf`'s premise and
`BashCommandWith` could disagree and open a window where neither grader fires — was refuted
against the source: `BashCommandWith`'s matched set is a **subset** of the premise's, so it
passing implies the premise holds. The direction of my concern was backwards. The habit
worth copying is the one CLAUDE.md already states and this change kept proving: verify
every finding, including your own, against the source before acting on it.

**A reviewer disagreement resolved by the user, not by argument.** Codex asked for
`ConfirmedResult` to grade *every* confirmed call; `/code-review` demonstrated that exactly
that strictness is a false-red generator, since the toolset gates every tool and a model
that writes a file then verifies it with a second marker-carrying command earns a second
confirmed result — a verification exiting non-zero would fail the trial with the platform
behaving perfectly. The two reviewers wanted opposite things, which is a decision and not a
defect, so it went to the user, who chose the simplification. `ConfirmedResult` now grades a
confirmed call the way `CallResult` grades a called one: one satisfying call is enough,
with the dangling-name join #99 asked for kept and run *before* the markers narrow anything.

**The vacuity argument needed a task-by-task check, not a general one.** `ConfirmedResult`
goes vacuous when no *matching* call was confirmed, and its comment justified that by
naming `EvaluatedPermissionAsk` as the sibling owning the window. The verifier found that
`perm-deny` — the only task where `ConfirmedResult` is Platform-class — did not deploy that
grader at all, and that the stamp it checks is not proof a suspension happened anyway. The
fix was both halves: `perm-deny` gained `EvaluatedPermissionAsk("bash", Platform)`, and the
comment now names each task's real owner of the window (in `perm-deny`, the seeded file
staying unchanged). A safety argument that holds "in general" but not in the one place the
grader is Platform-class is not a safety argument.
---

## anthropic-sdk-go v1.58.0 bump (#120) — wire-schema verification record (2026-07-20)

The bump itself is two lines of `go.mod`/`go.sum`. It is recorded here because CLAUDE.md makes the
pinned SDK this project's **authoritative typed wire schema**, so a minor-version bump is a
wire-schema event whose *outcome* has to be auditable even when — as here — the outcome is that
no field or enum drifted and no code change became necessary. Without this record the next bump has
to redo the same diff to learn that. (Stated that precisely rather than as "contract-neutral": one
documented bound *did* widen — see the custom tool `Description` below — it simply lands where this
repo enforces nothing.)

**What the range contains.** Two upstream releases: v1.57.0 (a "dreaming" API and tool-runner
permission gating) and v1.58.0 (MCP Tunnels). Endpoint count went 116 → 131 (`.stats.yml:1`).

**The decisive measurement.** Every SDK file carrying managed-agents schema this repo mirrors is
**byte-identical** across the two versions (`cmp`): `betasessionevent.go`, `betasession.go`,
`betasessionthread.go`, and — because a session's shape reaches past those three —
`betasessionresource.go` (the `Resources` union this repo emits at `internal/api/sessions.go:35`),
`betasessionthreadevent.go`, `betaagentversion.go`, `betaenvironment.go`, `betaenvironmentwork.go`.
The event taxonomy, the ID prefixes, and every session field this repo mirrors are therefore
unchanged by construction, not by inspection. The first three alone would *not* have been sufficient
proof, which is why the list is enumerated here.

**The three questions #120 asked, answered.**

- *Do the changed `betaagent.go` / `betamessage.go` / `betasession*.go` types alter a shape
  `internal/domain` or `internal/api` mirrors?* **No.** `betaagent.go:1288`, `betamessage.go` and
  `betamessagebatch.go` changed in **doc comments only** — no field, type, or enum moved (proven by
  diffing the three with comment and blank lines stripped: empty). Two of those comments did shift
  meaning, and neither reaches this repo. A custom tool's `Description` bound relaxed from 1–1024 to
  1–4096 characters (`betaagent.go:1288`), which costs nothing because `internal/api/wire.go:244`
  only requires the description be non-empty and never enforced a length bound to relax; and
  `betamessage.go:4850-4853` re-words model fallbacks ("the **four** override fields … **replace**
  the corresponding top-level field" → "The override fields … **set** the corresponding parameter"),
  alongside a fuller `speed` description — both on model-fallback and speed surfaces this repo does
  not implement.
- *Does `shared/constant/constants.go` add stop reasons or event types the taxonomy should carry?*
  **No.** It adds exactly three constants — `Tunnel`, `TunnelCertificate`, `TunnelToken`
  (`constants.go:206-208`) — belonging to the new tunnels product, not to the `{domain}.{action}`
  session taxonomy. No new stop reason. The remainder of that file's diff is gofmt realignment.
- *Do the new `betatunnel*` / `betadream` surfaces imply behavior worth a DIVERGENCES entry?*
  **No, and this was decided rather than skipped** — see below.

**Decisions evaluated and rejected.**

*A docs/DIVERGENCES.md entry for the tunnels/dreams surfaces* was rejected. The registry records
deliberate divergences from reference behavior and inferences about behavior not yet confirmed;
`/v1/tunnels` and `/v1/dreams` are neither. They are reference **product areas this repo has not
built**, and logging those would grow the registry into a mirror of everything upstream ships — work
the GitHub backlog already owns. A divergence needs two implementations that disagree; here there is
only one.

*Adopting the v1.57.0 tool-runner permission gating* was rejected as a non-change, and the reason is
worth keeping: that rewrite gates dispatch on `evaluated_permission`, holds `ask` calls for
`user.tool_confirmation`, and fails closed on an unrecognized permission — which is the SDK's
**client-side helper** catching up to behavior this repo already implements **server-side**
(`internal/brain/brain.go:391` stamps the field, `internal/events/toolflow.go:199-260` gates results
and validates confirmations, `internal/api/events.go:83,124,226` drives the `requires_action`
round-trip). The enums agree exactly: SDK `betasessionevent.go:1013-1015` (`allow`/`ask`/`deny`)
versus `internal/domain/agent.go:49-51`. The SDK's fail-closed rule is a client concern; this repo is
the authority *emitting* the field and only ever emits `allow` or `ask`. Read as convergence
evidence, not as a gap.

Likewise the `betasessionutil.go:45,66-70` accumulator fix — keep canonical `agent.message` events
(valid `processed_at`), drop only unreconciled previews — relies on precisely the distinction this
repo already maintains (`internal/api/events.go:583-585` emits `processed_at` null-until-processed;
the brain mints a preview-reserved message id).

**Citation durability.** The bump moved the live pinned-version label in three places —
`.claude/agents/verifier.md`, `docs/REFERENCE_PROJECTS.md`, and the Stop Work entry in
docs/DIVERGENCES.md — and every file:line that entry cites was re-read at v1.58.0 rather than
assumed: `lib/environments/poller.go:439-465` (the 204 / `WithResponseBodyInto` comment) and
`worker_test.go:118-120` (`WriteHeader(http.StatusNoContent)`) hold verbatim — `diff -rq` shows the
whole `lib/` tree identical between versions. `api.md` **did** change, so `api.md:656-673` was
checked rather than trusted: the work-resource section did not shift, and those lines are
byte-identical across both versions, still declaring the `BetaSelfHostedWork` return on Stop. The
v1.56.0 mentions surviving in CHANGELOG.md and archived `docs/plan/04` are historical records of
what was true when those PRs landed and were deliberately left alone.

**Evidence.** `make verify` green at total statement coverage **91.92%** (including the Docker and
K8s sandbox suites). Every SDK request type, response field, JSON tag, service method, option,
paginator, SSE helper and error decoder this repo uses is unchanged: the defining file of each is
among the byte-identical set, enumerated from the non-test import sites
(`internal/provider/anthropic/anthropic.go`, `internal/worker/{client,lease,toolexec}.go`;
`internal/worker/{lease,toolexec}_test.go` import the SDK too, and the compile-and-test pass covers
what they reference). One shape *did* change and is called out rather than smoothed over:
`sdk.Client`'s embedded `BetaService` gained `Dreams` and `Tunnels` fields (`beta.go:20`), so the
struct layout differs even though no call site's behavior does. "No code change required" is exact;
"zero runtime difference" would not be — the two new services are constructed at client init, and
the version-identifying `User-Agent` / `X-Stainless-Package-Version` headers change value.

**Process note — a rejected decision that was itself wrong, and got reversed in review.**
`issue-triage` returned `needs_plan: true` on the wire-schema-verification trigger. The
implementation initially declined to author a plan, reasoning that a plan is a forward-looking
decomposition across PRs while this is a single PR whose entire deliverable *is* the verification
outcome, so a plan created and archived in one commit would record that outcome in a file whose
status says the work had not started.

That reasoning was wrong, and is kept here because the failure mode is worth recognizing next time.
`.claude/agents/issue-triage.md`'s judgment criteria say the wire-schema trigger fires
"**unconditionally**, however well-scoped the issue already looks: the resolution itself belongs in a
plan, never improvised mid-implementation" — a rule written precisely to defeat the "but this case is
small" argument, which is the argument that was made. The lifecycle objection was also false: a plan
may land directly as `in-progress` (CLAUDE.md's plan bullet says so), which is what
[05_sdk-bump-1.58.0.md](./plan/05_sdk-bump-1.58.0.md) does.

Worth noting for anyone weighing reviewer disagreement by count: two of the three review passes
(the verifier and the Claude-side reviewer) examined this decision and endorsed it as defensible.
Only the Codex pass called it a blocking finding, and the Codex pass was right — it was the one that
quoted the governing sentence instead of reasoning from the rule's purpose.
