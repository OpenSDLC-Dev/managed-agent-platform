---
status: archived
issue: "#55"
---

> **Archived — completed.** Both slices landed (PRs #329 and the slice-2 PR that
> archives this file), closing #55; the e-wire-cli acceptance transcript is in
> docs/HISTORY.md, and the progress summary moved there in the archiving PR.
> BYOC repository materialization was deferred by design and is tracked in #322.

# Git/repo mounting: `github_repository` session resources (plan 25)

This plan delivers the **second half of #55**: session `resources[]` accepting
`type: "github_repository"` — a GitHub repository cloned into the session's sandbox at a
mount path, with a write-only authorization token and the already-scaffolded token-rotation
endpoint going live. The Files half landed with plan 08 (archived); this plan lifts the
union seam that plan deliberately kept open (`internal/api/sessionresources.go:60`,
rejection at `:104-111`).

Three decisions were put to the user and settled on 2026-08-06:

1. **The clone runs platform-side, via go-git** — never inside the sandbox (decision 1).
2. **BYOC stays reference-faithful: no worker materialization** — the symmetric
   extension is tracked in #322 (decision 9).
3. **A failed clone is surfaced, not fatal** — a `session.error` variant plus a tolerated
   absent path (decision 4).

Two PR slices. Verification follows the runtime-observation philosophy adapted in
"Verification" below — every slice ships its matrix rows as executable checks, probes
included, before the slice is done.

## Ground truth (verified 2026-08-06)

Resolved per CLAUDE.md's order: public docs (platform.claude.com, fetched 2026-08-06) →
pinned `anthropic-sdk-go` v1.61.0 (the local tip checkout is byte-identical to the pin —
both declare release 1.61.0, so there is no newer surface to track) → the `ant` CLI
source. No live recording was possible; everything only a recording can settle is
pre-listed in "Recording checklist" and lands in docs/DIVERGENCES.md with its slice.

### The wire shape (beta header `managed-agents-2026-04-01`; paths carry `?beta=true`)

**Create** — `POST /v1/sessions` `resources[]`, the `github_repository` arm of the
three-variant union (betasession.go:722-733):

| Field | Requiredness | Notes |
|---|---|---|
| `type` | required | const `"github_repository"` |
| `url` | required | "Github URL of the repository" |
| `authorization_token` | required | "GitHub authorization token used to clone the repository" — **write-only** |
| `mount_path` | optional | "Mount path in the container. Defaults to `/workspace/<repo-name>`." Used literally (files, by contrast, re-root under `/mnt/session/uploads`) |
| `checkout` | optional | union `{type:"branch", name}` \| `{type:"commit", sha}` (betasession.go:803-807 registers exactly these two); "Defaults to the repository's default branch"; `sha` is documented as a **full** commit SHA |

**Read** (betasessionresource.go:211-221): `{id (sesrsc_…), created_at, mount_path, type,
updated_at, url, checkout}` — every field `api:"required"` except `checkout`
(`api:"nullable"`). **No response type anywhere in the module carries
`authorization_token`**; the SDK states it outright: "The authorization token is
write-only and never returned" (betadeployment.go:1208-1209), and the public docs repeat
it ("is not echoed in API responses").

**Sub-endpoints** (all under `/v1/sessions/{sid}/resources`):

- `POST …/{rid}` (update) — **the rotation endpoint**: body is exactly
  `{"authorization_token": …}` (required; betasessionresource.go:690-698), "Currently only
  `github_repository` resources support token rotation", response is the full rendered
  resource union. Our scaffold already parses precisely this body and 404s correctly
  (`internal/api/sessionresources.go:401-433`); today it always ends in the 400 because
  every stored resource is a file — this plan makes the happy path real.
- `POST …/resources` (add) — the request body is typed as the **file variant only** and
  the response as the file resource; a repo cannot be attached after create. The public
  docs agree: "Repositories are attached for the lifetime of the session; to change which
  repositories are mounted, create a new session." Our add handler keeps rejecting
  non-file types — that is now wire-faithful, not a stopgap.
- `GET` list/get and `DELETE` are shape-unchanged; the repo variant simply appears in
  their unions.

### Reference behavior established from the docs and clients

- **Auth model is a raw GitHub token** (the guide's examples use `ghp_…`; fine-grained
  PATs recommended, scope table documented). No GitHub App, no OAuth, no environment-level
  GitHub configuration exists anywhere in the surface.
- **The clone is server-side.** The `ant` CLI's only mention of `github_repository` is the
  rotation flag's usage string (pkg/cmd/betasessionresource.go); a repo is attached by
  hand-written JSON through the generic `--resource` flag. The SDK worker's per-item setup
  is `SetupSkills` and nothing else (lib/environments/worker.go:309-321) — no clone, no
  git, no credential injection anywhere client-side (exhaustive greps, all empty).
- **The reference does not mount repos (or files) on self_hosted environments at all**:
  "Anthropic doesn't mount files or GitHub repositories into self-hosted sandboxes. To
  make session-specific files available, pass file references (such as an S3 path or
  commit SHA) in the session `metadata` field."
  (platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
- **Push/PR flow is not a platform feature**: the guide wires the GitHub MCP server
  (`https://api.githubcopilot.com/mcp/`) into the *agent* and the agent creates branches
  and PRs itself. The platform's job ends at the clone.
- The reference **caches repositories across sessions** for faster starts. We deliberately
  do not (non-goal).

## Design decisions

1. **The clone is control-plane resource materialization, not sandbox egress.** The
   executor clones with **go-git** (pure Go, crossbuild-safe) into an executor-local
   temporary directory, packs the working tree — `.git/` included — into a tar written to
   a local temp file (streaming needs a size up front; a repository must never buffer in
   memory), streams it into the sandbox via the existing `Sandbox.WriteFileStream` to a
   `/tmp` staging path, and extracts **via stage-and-rename**: one `Exec` untars into a
   fresh sandbox-side temporary directory and then renames it into place, so `<mount>`
   (and its `.git`) exists only once the tree is complete — a mid-extraction failure
   (disk full, unwritable member, kill) can never leave a partial tree for the
   decision-5 probe to trust — and the staging tar plus any partial temporary are
   removed on **every** exit path, success or failure (cleanup is unconditional, never
   sequenced behind an `&&`). Paths reach that command shell-quoted with the
   established single-quote escaping (the `test -e '<path>'` precedent): `mount_path`
   is request-controlled text, and unquoted interpolation would be command injection
   inside the sandbox. Consequences, and why this shape won: the token never enters the
   sandbox in any form (no env var, no credential file; go-git sends it as a per-request
   `Authorization` header — `BasicAuth{Username: "x-access-token"}` — so the on-disk clone
   and its `.git/config` carry only the clean URL); the egress gate and a `limited`
   environment's `allowed_hosts` do not govern the clone, exactly as they do not govern
   the executor's blob downloads for file mounts — so #166's HTTPS-substitution gap never
   applies; and sandbox images need `tar` (present in `debian:stable-slim`) but never git.
   Rejected: in-sandbox `git clone` (token becomes agent-readable, requires git in a
   customer-chosen image, requires github.com in `allowed_hosts`, collides with #166);
   shelling out to a git binary executor-side (a deployment prerequisite on every
   executor/worker image and a non-hermetic test suite, for compatibility this feature
   does not need). Temp dirs and tar files are removed in `defer` on every path.
   Clones are **budgeted, and the byte budget is enforced as bytes land** — the
   executor-local spool sits outside the sandbox's own CPU/storage hardening, so one
   unbounded repository could exhaust executor disk and disrupt unrelated sessions. A
   per-repo deadline (`EXECUTOR_REPO_CLONE_TIMEOUT`, default 5m) and a byte budget
   (`EXECUTOR_REPO_CLONE_MAX_BYTES`, default 1 GiB, counted over tree + tar) abandon
   the clone and surface as the decision-4 reasons `timeout` / `too_large`; the byte
   meter wraps the storage go-git writes through and errors past the cap **during**
   the clone — post-hoc accounting would let an adversarial repository blow far past
   the limit before anyone looked, so m-oversize asserts the peak spool, not just the
   final error. The spool is removed on every exit. Aggregates need no further knobs:
   the executor's work loop is serial (one `step` at a time,
   internal/executor/executor.go:224-229) and the spool is cleaned between sequential
   clones, so per-process peak disk is one repo's budget; create caps a session at
   **eight** `github_repository` resources (INFERRED, ours — files cap at 500), so a
   session's aggregate clone time is bounded by count × deadline. Inode exhaustion by
   a many-tiny-files repository inside the byte budget is bounded only by the deadline
   — accepted for this slice and stated here rather than guessed at with an
   entry-count knob.
2. **The token is sealed beside the row, never inside the echoed array.** What
   `sessions.resources` jsonb stores is the *rendered* wire object — which is token-free
   by schema — so session GET's verbatim echo cannot leak the token *by construction*.
   The secret itself lands in a new table `session_resource_credentials` (migration
   `0020`): `resource_id` (PK, `sesrsc_…`), `session_id` (FK → sessions **ON DELETE
   CASCADE** — the #266 lesson: no orphaned secrets when a session is deleted),
   `token_ciphertext bytea`, `token_key_id text`, timestamps — the `0011_vaults.sql`
   column naming (`secret_ciphertext`/`secret_key_id`) carried over. Sealing rides
   `secrets.Cipher`. Deployment plumbing already exists — compose and Helm set
   `SECRETS_BACKEND` on the executor — but the executor's cipher is today constructed
   for **startup validation only** and discarded before `executor.New`
   (cmd/executor/main.go:264-277, whose comment records the prior stance: egress
   substitution decrypts controlplane-side, "never in the executor"). Slice 2 threads
   it into the constructor and updates that comment: decrypting the clone token
   executor-side is a deliberate, narrow extension — the executor already sits inside
   the trust boundary holding the database, the blob store, and the model keys — and
   the alternative (a controlplane internal endpoint on the gate-config pattern) would
   add a plaintext-token HTTP hop where a process-local decrypt suffices. The create
   transaction validates, renders, and inserts row(s) atomically with the session;
   rotation updates the ciphertext and bumps the resource's `updated_at` inside the
   jsonb under the same session `FOR UPDATE` lock the other mutations take. Credential
   rows die only with their session (the cascade): repo resources themselves are
   immutable post-create (decision 3). A server without a configured cipher refuses a
   repo-bearing create the way the vault endpoints refuse without one — unavailable, not
   silently unencrypted. The two processes share one cipher contract: the control plane
   already constructs its cipher for `api.NewHandler` (it encrypts vault credentials
   today) and encrypts on create/rotate; the executor decrypts through the same
   `SECRETS_BACKEND` configuration; and the row's `token_key_id`, stored beside the
   ciphertext exactly as the vaults table stores its pair, is what keeps a decrypt
   honest across backend key rotation. Unit W exercises the encrypt half and unit M
   the decrypt half against one shared test backend. No handler ever logs a resource
   payload; slog carries ids and the url, never the token.
3. **Create-time validation, no network.** `url` must be the exact canonical form
   `https://github.com/{owner}/{repo}`: scheme `https`, host `github.com` with no
   userinfo and no port, exactly two non-empty path segments (an optional `.git` on
   the second), no query, no fragment, no trailing slash — anything else (http, other
   hosts including GitHub Enterprise, extra segments,
   `https://TOKEN@github.com/o/r`, `…?token=x`) is a 400 (strictness INFERRED). The
   url is a *rendered, logged* field, so a credential smuggled into it would break the
   write-only-token guarantee — the grammar is what keeps the token sweep meaningful.
   `authorization_token` must be non-empty (required per schema; the empty string's
   fate is unrecorded — we 400, INFERRED) and at most **8 KiB** (400 above it,
   INFERRED — real PATs are under a hundred bytes, and the cap keeps every
   `secrets.Cipher` backend inside its plaintext ceiling: gcpkms wraps
   `ErrPlaintextTooLarge` at 64 KiB, 8 KiB on HSM keys, so an oversized token can
   never degrade into a backend-dependent 500); `checkout` is parsed
   strictly (unknown `type` or unknown keys 400 via the `rejectUnknownKeys` precedent;
   `branch.name` non-empty; `commit.sha` exactly 40 hex — "full commit SHA", INFERRED);
   `mount_path` rides `validateMountPath` plus per-session uniqueness across *all*
   resources, defaulting to `/workspace/<repo-name>` where `<repo-name>` is the URL's last
   segment with `.git` stripped (derivation INFERRED). The repo arm is additionally
   stricter than the landed file arm: the path must equal its `path.Clean` form —
   `/workspace/./repo`, doubled separators, and trailing slashes are 400s, because
   raw-string uniqueness is otherwise evadable by aliases naming the same directory
   (INFERRED) — and uniqueness is judged on the cleaned form against every other
   resource. Nesting is directional: a file mounted **inside** a repo's tree is the
   supported overlay (decision 5's ordering), but no resource of any type may sit at a
   proper **ancestor** of a repo's mount path (a file there is a non-directory where
   the clone needs a directory), and repo mounts may not nest within each other (a
   probe-failed re-clone of the outer would clear the inner). Two reserved targets are
   rejected outright: `/` (extraction over the rootfs breaks the sandbox image
   contract, and a re-clone's cleared target would be the sandbox itself) and any
   ancestor of the platform's staging path. Everything else stays legal on purpose —
   the mount sits inside the agent's own blast radius, bash can already write
   anywhere in the sandbox, and the reference documents `mount_path` as an arbitrary
   container path (the landed file arm has the same latitude). The default is resolved and
   rendered at create (the files precedent, and the docs' List example shows a concrete
   path). `checkout` is stored and rendered **as given — `null` when omitted**: resolving
   "the repository's default branch" would take a GitHub call inside the create
   transaction, which we will not make; the default resolves at clone time (INFERRED —
   the docs' List example shows a populated `checkout` that *may* mean the reference
   resolves it at create; the recording checklist settles it). No existence or credential
   check at create — the first materialization is the probe (files checked a row in-tx;
   there is no in-tx equivalent for a remote repo). Post-create immutability is
   **total**: add stays file-only (ground truth above), and `DELETE …/{rid}` on a
   `github_repository` resource is **rejected** — the guide's "attached for the
   lifetime of the session" says repos cannot be unmounted, and the SDK's generic
   Delete proves only that the route exists, so the rejection is INFERRED and on the
   recording checklist. File resources in the same session keep their landed delete
   semantics.
4. **A failed clone is surfaced, never fatal** (user decision). `materializeRepos`
   tolerates per-resource failure exactly like files — the run continues, the agent sees
   an absent path, the work item completes. But unlike files it also appends a
   `session.error` event (domain/event.go:41) with `payload.error.type =
   "github_repository_clone_error"`, naming `resource_id`, `url`, and a machine-readable
   `reason` ∈ `auth | not_found | network | checkout | too_large | timeout | internal` — mirroring
   `credential_host_unreachable_error` (internal/api/gateconfig.go:186), including its
   dedupe — whose identity is **(resource_id, reason)**: the existing-event query
   filters on the payload's resource id and reason, not only `error.type`
   (gateconfig.go:174 is the query-shape template; its session-scoped key would be
   too coarse here — two failing repos are two events, and a reason flip re-emits).
   No concurrent-writer race needs guarding: the work-item lease already makes the
   materializing executor the session's single writer. go-git error text goes to logs only, and only after a credential scrub —
   auth rides a header, not the URL, so messages are token-free by construction; the
   scrub is belt and braces.
5. **Materialization order and idempotence.** The pass slots into the established
   sequence at internal/executor/executor.go:417-422: `provisionSandbox` →
   `materializeSkills` → **`materializeRepos`** → `materializeFiles` — repos before files
   so a file mount may deliberately overlay into a checkout (mount paths are
   exact-match-unique, not nesting-exclusive). Idempotence is **the probe alone — no
   marker**, a deliberate divergence from the files sentinel: a repo materializes iff
   `test -e '<mount>/.git'` fails. A marker earns its keep by detecting configuration
   drift, and a repo mount has none to detect — the set is fixed at create (decision 3),
   the checkout never changes, and rotation deliberately does not re-clone (a rotated
   token affects future clones only, the retroactive twin of the mutation-rejection
   semantics — INFERRED). A marker would also be actively wrong here: plan 24 strips
   agent-writable `.materialized` sentinels from checkpoints at capture (its decision
   5), so a restored workspace presents exactly the state — tree present, marker gone —
   that must **not** be re-cloned; the probe reads the only authority there is, the
   tree itself. Probe passes → skip; the agent's working tree is never clobbered.
   Probe fails → the tree is gone (the agent removed it, or a reap recreated the
   sandbox without a checkpoint) and the repo is re-cloned **fresh** — staged and
   renamed into place after any remnant at the mount is removed, never merged into
   survivors. The stage-and-rename shape (decision 1) is also what makes the probe
   trustworthy: `<mount>/.git` can only ever name a completely extracted tree.
6. **Checkpoint interplay (plan 24).** Capture covers the **configured** workdir
   (`EXECUTOR_WORKDIR`, defaulting to `sandbox.DefaultWorkdir` `/workspace` —
   cmd/executor/main.go:156, internal/sandbox/sandbox.go:29). Out of the box the wire's
   default mount `/workspace/<repo-name>` sits inside it, so a checkpointed reap
   restores the working tree, the decision-5 probe passes, and the agent's uncommitted
   work survives — zero repo-specific checkpoint code, and no dependence on a restored
   marker (which plan 24 would have stripped). Three edges, surfaced rather than
   solved: a reap past the checkpoint budget or a crash-recreate restores nothing — no
   tree, failed probe, fresh clone at the configured checkout; a mount outside the
   configured workdir (a custom path — or the wire default itself, under a deployment
   that moved `EXECUTOR_WORKDIR`) is outside the capture set and re-clones fresh after
   a reap, so operators who move the workdir should mount repos inside it; and under
   read-only-rootfs hardening such a mount may not be writable at all (`WritablePaths`,
   internal/sandbox/hardening.go — the docs/self-hosted-security.md §4 stance), in
   which case materialization fails and surfaces per decision 4.
7. **go-git mechanics.** Branch checkout (or none): `Clone` with `ReferenceName:
   refs/heads/<name>` (`SingleBranch`), omitted → the remote's HEAD. Commit checkout:
   full clone, then a detached worktree checkout at the SHA. Clones are full, not shallow
   (a commit checkout must reach arbitrary history; depth is not in the wire schema —
   INFERRED); submodules are not recursed and LFS content is not smudged (INFERRED
   minimal; non-goals).
8. **Brain injection.** A "Mounted repositories" block appended after the "Mounted
   files" block (the slot at internal/brain/brain.go:279; internal/brain/files.go is the
   template): per repo — mount path, url, and the checkout descriptor (branch name,
   commit SHA, or "default branch"). The block is emitted **only for `cloud`
   environments**: on self_hosted nothing materializes repos (decision 9 — nor does the
   reference), and asserting a mount that does not exist would be a false statement to
   the model. `claimLiveSession` (internal/brain/brain.go) reads only the sessions row
   today, so the slice adds the environment's kind to that read (a join on
   `environment_id` under the same lock). The brain never reads the credentials table,
   so the block cannot leak what it never sees. Best-effort like files: a dangling
   entry is a counted miss (`repos.resolve.misses`), never a failed turn.
9. **BYOC: reference-faithful no-op** (user decision). The worker's resource pass keeps
   filtering to `type == "file"`; a self_hosted session carrying a repo resource gets no
   platform materialization — which is exactly the reference's documented behavior for
   self-hosted sandboxes, so for once the two implementations agree by doing the same
   nothing. The symmetric extension (a worker `SetupRepos` plus an environment-key
   credential lane) is deliberately **not** built: it would mint a plaintext-PAT delivery
   route whose design belongs with #165's `work.secret` credential-delivery decision.
   #322 tracks it; #55 closes with slice 2.
10. **Observability** (cardinality rule as everywhere: outcome-only metric labels; ids
    and urls in span attributes and structured logs, never metric labels — and never the
    token anywhere). Executor: a `repos_materialize` span (`repos.referenced`,
    `repos.materialized`, `repos.unchanged`, `repos.failed`), `repos.materialized`
    counter (`outcome` ∈ ok\|auth\|not_found\|network\|checkout\|too_large\|timeout\|internal\|unchanged
    — one label per decision-4 reason, plus ok/unchanged),
    `repos.materialize.duration` and `repos.materialize.bytes` histograms; `slog` on
    every link (session id, resource id, url, reason). Brain: `repos.injected` /
    `repos.block_chars` span attributes on the model_request span, `repos.resolve.misses`
    counter (the files twins). API: rotation and repo-bearing creates log structured
    events on the existing request instrumentation.

**Non-goals:** push/write-back of any kind (branches, commits, PRs are the agent's own
work — via bash in its sandbox or the GitHub MCP server, per the reference guide; the
platform never writes to GitHub); repo caching across sessions; GitHub App/OAuth auth
models; GitHub Enterprise hosts; submodule recursion; LFS smudge; shallow-clone tuning;
post-create add/remove of repo resources (the add rejection is wire-faithful, the
delete rejection INFERRED — decision 3); BYOC worker
materialization (deferred to #322); `memory_store` resources (stay rejected);
webhook/event surface for clone lifecycle beyond the decision-4 error variant.

## Verification

The philosophy is adapted from the verifiable-react architecture in the "how we Claude
Code" workshop (phase-3-verify, `docs/verification.html`): **verification is runtime
observation at the surface** — build the real artifact, run it, drive it to where the
changed code executes, and capture what the surface shows. Tests and typechecks gate the
merge; verification confirms the running system behaves at the boundary where a consumer
meets it. Its rules, translated to this repo:

- **Surfaces, not internals.** This feature has three: **S1 the wire** (REST bodies and
  the SSE stream — what the `ant` CLI and SDKs see), **S2 the sandbox filesystem** (what
  the agent's tools see — probed with `Exec`, reading git metadata as plain files:
  `.git/HEAD` names the checkout without needing a git binary), and **S3 the event log
  and telemetry** (session.error variants, spans, counters, structured logs). Every
  check below asserts against one of these, on machine-readable output — never on
  internal function returns.
- **Fixtures + act + observe.** Each case is a named, reproducible configuration
  (fixture), a driving step (act), and a surface assertion (observe). The matrix below
  is the case registry; slice PRs implement each row as an executable check.
- **Probes are mandatory.** Rows marked 🔍 are adversarial or off-happy-path. A unit
  whose implemented checks are all happy-path is a review-rejectable defect in the slice
  that ships it — "a check list that's all ✅ and no 🔍 is a happy-path replay."
- **The framework must catch lies** (mutation duty — the repo's standing
  mutation-test-every-new-guard rule, and the workshop's "intentional failure"). Every
  new guard is shown **red against code without the guard** before the slice is done:
  deliberately render the token → `w-token-sweep` must FAIL; drop a rejection arm → its
  400 case must FAIL; remove the `.git`-probe skip → `m-idempotence` must FAIL; remove the
  dedupe → `m-error-dedupe` must FAIL. A regression test that never saw the broken code
  proves nothing.
- **Verdicts:** PASS (drove it, every check held) / FAIL (observed and wrong) / BLOCKED
  (could not reach an observable state — a missing Docker daemon, a dead fixture server;
  **BLOCKED is not FAIL and never silently becomes SKIP**) / SKIP (nothing to observe —
  used only for opt-in live tiers that were not opted into). **When in doubt, FAIL**: an
  exception in a check is a FAIL with the stack as evidence, ambiguous output is a FAIL
  with the raw output attached; a probe that "mostly passed" is a FAIL.
- **One pipeline.** The matrix rows *are* the Go integration tests `make test` runs (the
  `pgtest` + sandbox + fixture-server harnesses), the `ant`-CLI acceptance transcript,
  and the opt-in eval — CI, the verifier subagent, and a human reproduce the same
  commands and read the same surfaces. No bespoke second harness whose green can drift
  from CI's.
- **Reporting.** Each slice's PR description carries its unit's verdict table — one row
  per case, verdict plus evidence (test name / transcript pointer). The verifier
  subagent re-derives and re-runs; its verdict goes in the PR too.

### Unit W — the wire (slice 1; harness: in-process server + real Postgres via `pgtest`)

| Case | 🔍 | Act (drive) | Observe (surface S1 unless noted) |
|---|---|---|---|
| w-create-minimal | | create session, `resources: [{type, url, authorization_token}]` | 200 (this server's create status everywhere); rendered resource has `sesrsc_` id, `mount_path` `/workspace/<repo>`, `checkout: null`, `created_at == updated_at` |
| w-create-full | | + `mount_path`, `checkout: branch` | echoed exactly as given |
| w-create-commit | | `checkout: {type: commit, sha}` | echoed; sha preserved verbatim |
| w-multi | | two repos + one file mount in one create | all three rendered, types correct, paths distinct |
| w-token-sweep | 🔍 | sweep **every** surface a token could reach: the create response body itself, the rotation 200 body (both token values), GET session, GET resource, LIST resources, GET events (the stored-event surface — the SSE stream is a live tail from connect time with no history replay), and the captured `slog` output | raw bytes contain neither the key `authorization_token` nor either token value — **zero hits, any hit anywhere is FAIL** (the mutating responses are the ones a rendering defect leaks through) |
| w-no-token | 🔍 | omit / empty-string `authorization_token`; a token above 8 KiB | 400 `invalid_request_error` each |
| w-bad-url | 🔍 | `http://…`, non-github host, `https://github.com/onlyowner`, extra segments, garbage, `https://TOKEN@github.com/o/r`, `…?token=x`, `…#frag`, an explicit port | 400 each (the canonical-grammar rule) |
| w-bad-checkout | 🔍 | `type: "tag"`, branch without `name`, sha not 40-hex, unknown keys | 400 each |
| w-mount-collision | 🔍 | two resources (repo/repo and repo/file) sharing a `mount_path`; aliases of one path (`/workspace/repo` vs `/workspace/./repo`) | 400 each (uniqueness on the cleaned form) |
| w-unclean-mount | 🔍 | `mount_path` `/workspace/./r`, `/workspace//r`, trailing slash | 400 each (the clean-form rule) |
| w-nesting | 🔍 | a file at `/workspace/repo` + a repo at `/workspace/repo/src`; two nested repos; a repo at `/` | 400 each (the nesting-direction and reserved-target rules); the inverse — a file *inside* a repo path — stays accepted |
| w-repo-cap | 🔍 | nine `github_repository` resources in one create | 400 (the eight-repo cap) |
| w-add-post-create | 🔍 | `POST …/resources` add with a `github_repository` body | 400 (add stays file-only — wire-faithful) |
| w-rotate | | `POST …/{rid}` `{authorization_token: new}` | 200, full rendered resource, `updated_at` bumped; storage: ciphertext changed and the new token round-trips through the cipher |
| w-rotate-bad | 🔍 | unknown body key; empty token; a file resource's rid; unknown rid | 400 / 400 / 400 (the existing scaffold message) / 404 |
| w-delete-rejected | 🔍 | DELETE the repo rid; then DELETE a file rid in the same session | 400 for the repo (attached-for-lifetime, INFERRED); the file still deletes — the union dispatch discriminates |
| w-archived-gate | 🔍 | rotation/delete on an archived session | the established archived-session error |
| w-no-cipher | 🔍 | server with no `SECRETS_BACKEND`, repo-bearing create | refused as unavailable (the vaults precedent), never stored unencrypted |

### Unit M — materialization (slice 2; harness: `pgtest` + Docker sandbox + an in-process smart-HTTP git fixture served with go-git's server-side transport — no git binary, loopback only; rows seeded directly so fixture URLs point at the loopback listener)

| Case | 🔍 | Act (drive) | Observe |
|---|---|---|---|
| m-clone-default | | run a work item | S2: `<mount>/.git` exists; fixture file content matches; `.git/HEAD` names the default branch |
| m-clone-branch | | checkout: branch | S2: `.git/HEAD` names that ref; branch-tip file present |
| m-clone-commit | | checkout: commit | S2: `.git/HEAD` contains the bare SHA (detached) |
| m-overlay | | one repo + one file mounted inside the checkout | S2: both present (repos-before-files order observable) |
| m-token-absent | 🔍 | after materialization: `Exec` env dump; read `.git/config`; sweep `~/.git-credentials`, `.netrc` | token value nowhere in the sandbox — the load-bearing security probe; any hit is FAIL |
| m-bad-token | 🔍 | fixture answers 401 | S2: mount absent; S3: one `session.error` with `error.type github_repository_clone_error`, `reason: auth`, visible on SSE; the work item still completes and tool results still post |
| m-repo-gone | 🔍 | fixture answers 404 | `reason: not_found`, same tolerance |
| m-bad-sha | 🔍 | commit checkout at an unknown SHA | `reason: checkout`, same tolerance |
| m-partial | 🔍 | repo A fails, repo B + a file mount remain | B and the file still materialize |
| m-idempotence | 🔍 | second work item; agent file created inside the checkout between runs | fixture server saw no second clone; the agent's file survives |
| m-tamper | 🔍 | agent `rm -rf`s the checkout | next pass re-clones fresh |
| m-error-dedupe | 🔍 | two failing passes, same reason | exactly one `session.error` event |
| m-oversize | 🔍 | fixture repo larger than `EXECUTOR_REPO_CLONE_MAX_BYTES` | the spool's **peak** never exceeds the budget (the meter aborts mid-clone); mount absent; `reason: too_large`; spool removed; run continues |
| m-stall | 🔍 | fixture handler stalls past `EXECUTOR_REPO_CLONE_TIMEOUT` | `reason: timeout`; spool removed; run continues |
| m-interrupted-extract | 🔍 | force the in-sandbox extraction to fail mid-way (unwritable member / full target) | mount absent — stage-and-rename leaves no partial `.git`; staging cleaned; the next pass retries fresh |
| m-metachar-mount | 🔍 | a mount_path containing a space and a single quote | materializes at the literal path; no command injection (the quoting rule) |
| m-checkpoint | 🔍 | checkpoint → reap → re-provision (the plan-24 harness) | restored tree; the `.git` probe passes and the pass skips (no marker involved); agent modifications survive |
| m-brain-block | | assemble a model request (`modeltest` fake) | system prompt carries "Mounted repositories" with url + checkout descriptor |
| m-brain-byoc | 🔍 | a `self_hosted` session carrying a repo resource; assemble a model request | **no** "Mounted repositories" block — nothing materializes there, so nothing is asserted |
| m-brain-no-token | 🔍 | sweep the entire assembled request | token value nowhere |
| m-telemetry | | any of the above | S3: `repos_materialize` span attrs and `repos.materialized` outcomes match the case (telemetry test reader) |

### Unit E — end to end

- **e-wire-cli** (manual acceptance, slice 1; wire-only — no work item, so no clone and
  no network): the compose stack + the `ant` CLI built from the reference checkout —
  session create with a `--resource` `github_repository` object, resources
  list/retrieve, update (rotation), the delete rejection; the CLI transcript swept for the token (the
  w-token-sweep twin at the outermost consumer). Transcript recorded in docs/HISTORY.md,
  the established acceptance-record ritual.
- **e-repo-answer** (opt-in, `RUN_EVALS=1`): a passphrase lives only in a file inside a
  real GitHub fixture repository; the agent must answer it — proving
  create → seal → claim → clone → tar → extract → brain block → model → file-tool read as
  one chain. Configuration rides `.env` under the established live-tier contract
  (`GITHUB_EVAL_REPO_URL`, `GITHUB_EVAL_REPO_TOKEN`; consent via `RUN_EVALS=1`; once
  opted in, missing configuration fails rather than skips). The recorded transcript is
  an artifact in a public repository: the record never quotes `.env`, and the
  acceptance ritual greps the assembled record for the token value before it lands in
  docs/HISTORY.md — a hit blocks the record. The w-token-sweep rule, applied to our
  own paperwork.

## Slices

Each slice is one PR through the full ritual (verifier, reviews, CI, squash). Docs move
with code; DIVERGENCES.md entries land in the slice that creates the behavior. Plan 08's
two standing requirements carry over unchanged: every slice ships an end-to-end
integration test over the real chain it adds, and every load-bearing link emits `slog`
plus OTel metrics/spans. Additionally, every slice's PR reports its verification-unit
verdict table (philosophy above) with the mutation duty discharged.

1. **The wire + sealed storage.** Migration `0020_session_resource_credentials.sql`
   (bump the migration-count pin in store_test.go); the `github_repository` union arm in
   `internal/api/sessionresources.go` — parse/validate per decision 3, render per the
   read shape, seal per decision 2; rotation goes live; the delete rejection lands
   beside the add endpoint's file-only stance — post-create immutability becomes
   documented wire fidelity. Implements
   **unit W** (all rows) as `pgtest` integration tests plus the **e-wire-cli**
   acceptance run (transcript → docs/HISTORY.md). Docs: CHANGELOG; DIVERGENCES — carve
   the create-rejection entry
   (`github_repository` no longer rejected at create; `memory_store` still is) and add
   the slice-1 INFERRED entries below; plan flips `in-progress`; STATE.md picks up
   Active work + Tasks.
2. **The clone: executor materialization + brain injection + eval; archive.** The go-git
   dependency (CLAUDE.md/AGENTS.md primary-deps line updated); the startup cipher
   threaded into `executor.New` and the main.go comment updated (decision 2); the clone
   budgets (decision 1); `internal/executor/repos.go` — `materializeRepos` per decisions
   1/4/5/7 with the smart-HTTP fixture living in in-package `_test.go` files (no new
   test-support package, keeping the coverage denominator honest; go-git ships the
   server-side transport, not an `http.Handler` — the fixture wraps it in a thin
   pkt-line HTTP shim); the `session.error` emission + dedupe; observability per
   decision 10; the brain's environment-kind-gated "Mounted repositories" block
   (decision 8); the `repo-answer` eval and its two `.env` names (documented in
   CLAUDE.md's `.env` paragraph). Implements **unit M** and **e-repo-answer**. Closes
   out: archive this plan (progress summary → docs/HISTORY.md), post the completion
   comment on #55 and close it (the BYOC deferral is already tracked in #322).

## End-to-end acceptance

- **E2E-1 (manual, slice 1):** e-wire-cli — wire-compat proven against the real `ant`
  CLI; transcript in docs/HISTORY.md.
- **E2E-2 (opt-in, `RUN_EVALS=1`):** e-repo-answer — the full platform chain against real
  GitHub and a real model endpoint; transcript recorded in docs/HISTORY.md with the
  archive.
- No BYOC E2E exists by design (decision 9) — the follow-up issue owns it.

## Inferences and divergences to record, by slice

Slice 1 — **CONFIRMED**: the create-rejection entry carved down; add stays file-only
(wire-faithful per the SDK's typed Add). **INFERRED**: URL strictness (the canonical
grammar — github.com only, no userinfo/port/query/fragment, the exact 400s);
empty-token rejection and the 8 KiB token cap; the eight-repo session cap; 40-hex sha
validation; `<repo-name>` derivation for the default mount path; the clean-form
`mount_path` strictness, nesting-direction rule, and reserved targets (the repo arm
only); the repo-delete rejection (attached-for-lifetime); checkout stored/rendered
as-given (`null` when omitted, not resolved to the default branch at create); rotation
error shapes beyond the scaffold; the cipher-unavailable refusal.

Slice 2 — **INFERRED**: clone timing (first work item, on our lazy-provision lifecycle);
full/non-shallow clone, no submodules, no LFS; the `github_repository_clone_error`
variant, its reasons (`too_large`/`timeout` are ours — the budgets have no wire
surface), and its dedupe (the reference surfaces clone failures in an unrecorded way —
possibly not as an event at all); rotation's non-retroactivity; probe-only idempotence
(no sentinel — fresh re-clone only when `<mount>/.git` is gone); the "Mounted
repositories" block format, placement, and `cloud`-only gating (the files-block twin);
**CONFIRMED-by-docs**: no BYOC materialization (the reference mounts nothing on
self_hosted — our worker's skip matches it).

## Recording checklist (deferred until a real `ant` recording is possible)

- Whether create resolves `checkout` to the default branch (and `mount_path` edge cases
  of the `<repo-name>` derivation — `.git` suffixes, trailing slashes).
- Whether an empty or whitespace `authorization_token` is accepted for public repos.
- The add endpoint's exact error for a `github_repository` body, and rotation's exact
  error shapes for non-repo resources and unknown ids.
- `DELETE …/{rid}` on a `github_repository` resource — accepted or rejected (we reject:
  the guide's attached-for-lifetime reading).
- Whether rotation affects an already-provisioned sandbox (re-written credentials for
  future fetches) or only future clones.
- Clone failure surfacing: which event (if any), its payload, and whether the session
  degrades or errors.
- Non-github.com URLs (GitHub Enterprise Server) — accepted or rejected, and how.
- Per-session repo-count limits (files cap at 500; no repo cap is documented).
- Cache semantics (scope, invalidation, keying) — informational; we do not cache.
