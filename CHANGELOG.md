# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); released sections
group entries newest-first by the PR that landed them.

A change and its changelog entry land in the **same PR** — the entry as a
fragment in [changelog.d/](./changelog.d/), its body the final entry verbatim.
A release PR assembles the fragments into a dated section here (`make
changelog`; see [docs/RELEASING.md](./docs/RELEASING.md)); post-release, the
section moves to [docs/changelog/](./docs/changelog/) behind the index stub
below (`make changelog-archive`, relative links re-based, byte-reversibly);
no other PR edits this file. The fragment is the **one place a change's
narrative is written**: [docs/HISTORY.md](./docs/HISTORY.md) holds only what
a changelog structurally cannot (acceptance-run and review-hardening records,
decisions evaluated and rejected, archived plans' progress summaries), never
a second copy of an entry here.

## [Unreleased]

Unreleased changes accumulate as one fragment file per PR in
[changelog.d/](./changelog.d/) and are assembled into a dated section here
by `make changelog` at release time (see [docs/RELEASING.md](./docs/RELEASING.md)).

## [0.4.0] - 2026-09-17

### Added

- **A pull request that brings an SDK transition fails until it is dispositioned** ([plan 51](docs/plan/51_sdk-reference-binding.md) slice 4, [#722](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/722)) — [`sdk-bump.yml`](./.github/workflows/sdk-bump.yml) runs `make sdk-bump-report` on any pull request touching `go.mod`, `docs/DIVERGENCES.md` or a Go file, and renders it into the step summary. It fails while a transition awaits a disposition, or when the report could not read a source the corpus cites. An anchor the pin no longer holds is dispositioned by an `absent at` in its own registry line or comment paragraph. That clause names what went, at a tag after the anchor's stamp and no later than the pin. An `absent at` anchor the pin holds again is dispositioned by dropping the clause. Neither moves a stamp. Each transition awaiting a disposition names the line that would disposition it, unless stamped at the pin, where the claim itself is wrong. The ritual is in [docs/REFERENCE_PROJECTS.md](docs/REFERENCE_PROJECTS.md). The gate now accepts an `absent at` on a file the pin no longer ships when it sits beside an earlier anchor on that file, so a deleted file can be dispositioned too. The registry's four current transitions were all dispositioned already.

- **`tools/sdkref` — the SDK citations get a grammar, and a bump gets a report** ([#722](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/722), [plan 51](docs/plan/51_sdk-reference-binding.md) slice 1) — A citation into the SDK makes a temporal claim and gives a locator, and a pin bump answers the two differently, so fusing them into `at the pinned v1.66.0 betasession.go:544-550` makes every citation look invalidated at once. The grammar separates them: `since`, `checked against` or `absent at` a source and tag, then a symbol anchor, a schema path into the bundled spec, or a line span with a reason from a closed set. `tools/sdkref` reads both halves of the corpus — `docs/DIVERGENCES.md` and the citations in Go comments — and checks shape and resolution inside `make verify`, resolution only at the version `go.mod` pins. `make sdk-bump-report` is what a pin bump is read through: anchors gone at the new pin, `absent at` anchors resolving again, line spans the sources contradict, stamps behind the pin, and everything it could not check, named as loudly as the rest. Shape and resolution fail the gate on any finding, including a source named beside a symbol or a package path with no tag; `make sdk-bump-report` prints what they find, above its report.

- **Plan 51 approved — bind to the SDK by symbol, date the check, and let a bump report itself** ([docs/plan/51_sdk-reference-binding.md](docs/plan/51_sdk-reference-binding.md), [#722](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/722)) — `docs/DIVERGENCES.md` named an `anthropic-sdk-go` version 184 times and the Go comments 54 more, and one syntax fuses two things: the temporal claim (`since v1.63.0`, `at the pinned v1.66.0`) and the locator (`betasession.go:544-550`). A bump therefore reads as invalidating all of them, so it is done wholesale, falsifying true `since` sentences, or not at all — which is what happened: `internal/events/inbound.go:164` called v1.66.0 the pin when `go.mod` already pinned v1.70.1. The plan separates the axes: three temporal forms (`since`, `checked against`, `absent at`), a symbol anchor qualified by receiver or struct, and a reason-tagged line span only where no symbol resolves. A `tools/sdkref` guard fails on shape and on pin-stamped resolution with each form's polarity, and separately resolves every module-backed anchor against the pin in both polarities — the bump report. A `go.mod`-triggered workflow fails only on transitions nobody dispositioned, a disposition being one `absent at` clause that never advances the checked stamp. Measured: of 104 coordinate/tag pairs, all resolve to a unique declaration and 103 survive the bump already taken. Four slices, the second closing #660.

- **An expired file eventually leaves the registry** — the controlplane sweeps at startup and then hourly for files whose expiry passed more than 30 days ago and removes the row and then, best-effort, its object — the end of the grace window the reference publishes: past `expires_at` the content stops being served immediately, while the metadata "remains readable for up to 30 days". Until now nothing removed them, so an expired file's metadata would have been readable forever. A file with no expiry is never a candidate, the window is measured from `expires_at` rather than from the upload, and each sweep takes a bounded, oldest-first batch, so a backlog drains as a queue over successive hours rather than as one unbounded pass. A tick's object deletes run on a context the sweep's own shutdown cannot cancel, because their rows are already gone and nothing else knows those keys — so the chart and the compose stack now give the control plane a 60-second stop grace, longer than the sum an orderly exit can take; the platform defaults of 30 and 10 seconds would have killed that drain partway (#655).

- **Uploaded files can expire** — `POST /v1/files` accepts the documented `expires_in_seconds` parameter (3600 to 7776000, either part order) instead of rejecting it with a 400, and `expires_at` now carries the instant it produces — the upload time plus that value, computed by the database that stamps `created_at`, rather than null on every file. Past that instant the content route answers 404 on both the management and the environment-key lane, a session can no longer mount the file, and the metadata route and the list keep answering with `expires_at` in the past, which is the published behavior for the grace window that follows. Two consequences worth naming: an expired file behaves exactly as a deleted one does at every mount point — the platform-managed executor and the BYOC worker both skip it, the brain stops describing it to the model, and a scheduled deployment whose template mounts it fails its runs the same way a deleted file already made it fail — and an expired file can no longer be named as an outcome rubric (#655).

- **`GET /v1/files?ids[]=` restricts a list to the files you name** — The documented batch filter is implemented: up to 100 ids counted after de-duplication, combinable with `scope_id` but not with `page`, `limit`, `after_id` or `before_id`, and always answered as a single page with `next_page` null however many ids were sent. Ids that resolve to nothing — unknown, deleted, or malformed — are silently omitted from the results rather than failing the request, though they still count toward the 100, so a request of exactly 100 ids plus a typo is refused rather than shortened. The bare `ids=` spelling is accepted alongside the bracketed one the SDK sends (#652).

- **A dream can consolidate a store in place** — plan 41 slice 4 ([docs/plan/41_dreams.md](./docs/plan/41_dreams.md), [#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)) lifts the refusal that stood on `output_behavior.type: update_existing` and gives it its runtime path. The target must be the job's own `memory_store` input, and the job then rewrites that store rather than a clone of it: no second store is created, and `outputs[]` names the input. **At most one live in-place dream holds a store** — a partial unique index over the dreams that have not closed — so a second create against a held store answers `409 conflict_error` naming the holder and carrying `x-should-retry: false`, the header that stops the SDK retrying a conflict it cannot win. The hold survives every terminal status until the closing arm archives the pipeline session. **The in-place session runs with `bash` disabled**, which turns the write jail around memory from a prompt rule into a code one: with only the file tools left, `write` and `edit` refuse any `/mnt/memory` path outside the mounted store, so no store but the caller's own is reachable whatever the transcripts say. The price is deletion — nothing left can remove a file — so the merge stage retires a memory by rewriting it as a one-line tombstone naming its successor and listing it for removal in the report and in the store's index, for the caller to delete through the memories API.

- **A dream's pipeline runs its four stages** — plan 41 slice 3 ([docs/plan/41_dreams.md](./docs/plan/41_dreams.md), [#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)) replaces slice 2's single fused turn with the four the plan designs, each one `user.message` the runner posts when the session idles: **orient** over the transcript index into a routing plan, **digest** through one delegated thread per batch of eight transcripts — thirteen for a hundred, where one per transcript would hit the platform's live-thread cap — each writing one batch digest in a fixed schema, **merge** the digests into the store under the plan's priority rules, then **index and audit**, which rewrites the store's `MEMORY.md` and writes a report. Every stage after the first opens by checking the previous stage's artefact and redoing what is missing, which is the pipeline's one recovery: the runner cannot read the sandbox, and a container recreated after a crash starts with an empty scratch. Each stage carries its own turn cap, every thread's counted, so a stage that loops fails the dream rather than spending its way to the timeout — and three of the four caps are measured against live runs rather than reasoned. The live eval tier gains a seeded dream graded on what it consolidated (a stale fact replaced, a duplicate merged, a planted secret absent, an untouched memory intact, a `NO SIGNAL` transcript producing nothing) and a hundred-transcript run at the plan's bound.

- **Plan 47 approved — a cancelled sandbox command leaves itself running** ([docs/plan/47_cancelled-exec-kills-nothing.md](docs/plan/47_cancelled-exec-kills-nothing.md), [#598](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/598)) — the design for the race plan 40's review found: the sandbox seam promises nothing about a command when the caller's context is cancelled, and both backends return `ctx.Err()` signalling nothing, so a stall or a lost lease leaves it running while the advisory lock releases and a reclaiming executor adopts it. Reading the code moved three things: only the package install is inside that lock; every executor sandbox command but that pass's is unbudgeted, so the cancellation is their only bound; and on Kubernetes the bulk and stream writes are in-pod commands of their own, so a fix confined to `Exec` would leave skills and file materialization untouched while the contract suite went green. Key decisions: the **seam**, not the method, terminates the command's process group before returning, best effort, on a detached `WithoutCancel` context whose budget stays under the lease keeper's beat and whose expiry is not an error — a cancellation must never become a hang; the kill re-checks liveness first, a pid being reassignable; docker's exec wrapper gains a pid file where k8s already writes one. Two slices: a crashed holder runs no code, so it stays slice 2, with four candidates and the constraint that a Postgres lock cannot reach BYOC.

- **Dreams run — the pipeline behind the routes** — plan 41 slice 2 ([docs/plan/41_dreams.md](./docs/plan/41_dreams.md), [#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)) puts a runner under `/v1/dreams`: a controlplane sweep beside the deployment scheduler claims a `pending` dream, renders each input session's transcript — secrets shape-redacted, 24 KiB apiece, streamed rather than loaded — mounts them as `file` resources beside an `INDEX.md`, clones the input store, and drives a pipeline session over the clone under the dream's own model, mirroring `usage` and settling `completed`, `failed` or `canceled`. Cancel interrupts a running session; the closing pass archives it, deletes the transcript files and stamps the dream done. The pipeline runs on a hidden internal agent and environment no list or id-addressed route shows, and its session — listed and streamable like any other — is **read-only to the public API while the dream owns it**: every mutating route on it, and a delete of one of its transcript files, answers a 400 until the dream closes. Three controlplane knobs, in the chart and compose too: `DREAM_TICK_INTERVAL` (30s; `0` disables the runner, and `POST /v1/dreams` then answers 500 `api_error`), `DREAM_TIMEOUT` (2h) and `DREAM_MAX_INPUT_BYTES` (64 MiB). Transitional: the pipeline is **one stage** until slice 3 lands the four and their digest threads, and `output_behavior.type: update_existing` stays a 400 until slice 4.

- **The dream surface — create, retrieve, list, archive and cancel** — plan 41 slice 1's routes ([docs/plan/41_dreams.md](./docs/plan/41_dreams.md), [#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)), so `ant beta:dreams create|retrieve|list|archive|cancel` works against this platform. A dream is a memory-consolidation job over one memory store and 1–100 session transcripts; the resource carries all fourteen keys, and `drm_` joins the accepted ID prefixes. Create checks every nested object against its own key set — both `inputs[]` arms, `model`, `output_behavior` — and refuses a missing or archived input store, a missing input session and duplicate session ids. The list is newest-first with `limit` (default 20, max 100), `include_archived`, `statuses[]` (bare `statuses` too) and the exclusive `created_at[gt]`/`[lt]` bounds. Cancel and archive follow the guide's state machine, each idempotent and each a 400 out of state. Reads need the viewer role, the rest developer. Two things this slice deliberately does not do: **no runner exists yet**, so a created dream stays `pending` until the next slice lands the pipeline, and **`output_behavior.type: update_existing` is refused with a 400** until the slice that lands the in-place path. Seven new entries in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md), the `/v1/dreams` entry rewritten from "not served" to served, and the beta-header rule amended in place for `dreaming-2026-04-21`.

- **Plan 42 drafted, and its gating recording taken — multi-tenant activation** ([docs/plan/42_multi-tenant-activation.md](./docs/plan/42_multi-tenant-activation.md), [#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)) — the design for the remaining half of #56: the reserved `org_id`/`workspace_id`/`project_id` columns become real scoping, with the **workspace as the isolation unit** and org and project frozen at `default`. Every request runs in exactly one workspace, its credential's or the one its header narrows to; every statement on a scoped table carries the predicate, is admitted by a named guard rule, or is a reviewed exemption; a resource in another workspace answers exactly as an absent one. Enforcement is application-level predicates behind a source-reading completeness guard, plus a composite workspace foreign key. Six slices, the operator surface last. It re-sizes the issue: 211 statements across 32 files, not 60–80 across 13. **The multi-workspace recording slice 1 was gated on was taken 2026-09-05**, closing all five of the plan's NOT OBSERVED items — among them, a cross-tenant id answers 404 indistinguishably from an absent one, an environment key carries no workspace at all, and `anthropic-organization-id` is a bare UUID — and settling the sixth gating question too: an archived workspace's key is refused as an unknown one. One lane of that sixth stays unobserved — an environment key belonging to an archived workspace.

- **Plan 41 approved — dreams, memory-consolidation jobs** ([docs/plan/41_dreams.md](docs/plan/41_dreams.md), [#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)) — the design for the reference's `/v1/dreams` surface, pulled forward from post-v1: an asynchronous job that reads one memory store plus 1–100 session transcripts and writes a consolidated **clone** (or, with `output_behavior: update_existing`, the input store in place), running as an ordinary session the dream's `session_id` names, under the model the request binds. Ground truth is `anthropic-sdk-go` v1.70.1 (the pin; v1.71.0 changes nothing here), the SDK's bundled OpenAPI spec, and the public dreams guide and API reference. Key decisions: a controlplane runner with stateless, row-resumable ticks and one soft lease (a five-minute start claim, never renewed); one hidden internal agent whose `self` roster lets `agent_with_overrides` bind every dream's model; transcripts rendered, secret-redacted and mounted as `file` resources; a four-stage pipeline whose digest stage runs on session threads; the clone written by the start arm under `session_actor`; the in-place hold answered with the spec's 409 `conflict_error`. Five slices, the first a recording against the reference (attempted 2026-09-05, blocked on preview enrollment; the rest proceed with their inferences registered against #78); the memory-store org cap, webhooks and any dream schedule stay out.

- **A cloud environment's `config.packages` is installed into the sandbox** — the field was accepted, stored and echoed, then read by nothing, so an agent's first `import pandas` raised (#353). The executor now installs each non-empty manager list before the session's first tool runs, in the reference's alphabetical order (`apt`, `cargo`, `gem`, `go`, `npm`, `pip`), every entry passed to its manager verbatim so its native pin syntax works. A manager that cannot install becomes a `session.error` of type `environment_package_install_error` — reason `failed`, `manager_missing`, `timeout`, `invalid`, `sandbox_not_root` or `rootfs_read_only`, carrying the command's output tail, deduped, with `retry_status` `retrying` until an unchanged list has failed three times in that sandbox and `exhausted` at the cap — and the session runs on without it. `EXECUTOR_PACKAGE_INSTALL_TIMEOUT` (10m) bounds one manager and joins the repository-clone budget in the stall-budget floor, so raising it without `EXECUTOR_STALL_TIMEOUT` refuses startup and names the knob. The sandbox must run as root with a writable root filesystem; the install repeats per sandbox, with no cross-session cache (#595). Design in [docs/plan/40_environment-packages.md](./docs/plan/40_environment-packages.md), the six registry entries in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md).

- **A `limited` sandbox reaches the package registries its environment opens (#591, #594)** — `allow_package_managers` now widens the per-session egress gate, where it had parsed and then done nothing, by a thirty-host set: Python, npm, Rust, Ruby, Go, PHP and Java registries, Ubuntu's apt mirrors, and — the part the flag's name does not suggest — source forges and container registries. A recording of the reference probed eighty hosts across three `limited` configurations; every entry was observed admitted with the flag on and refused with it off, in two control rounds, so none is a guess, and thirty is a lower bound rather than parity. Matching is by exact host and on any port: `test.pypi.org` and the `pythonhosted.org` apex stay refused beside their admitted siblings, and apt is Ubuntu's alone, so `apt-get` on a Debian base image — this platform's own default — reaches no mirror. Three properties come with the widening: the flag alone opens the set, independently of what `config.packages` lists; a dial only it admits is held to the platform's address floor; and its names are resolved absolutely (#596).

- **A `protocol: anthropic` model route can opt into flattening `search_result`
  blocks in replayed `tool_result` content** (#565) — set `flatten_search_results: true`
  in `model_providers` config for an endpoint whose Messages implementation rejects the
  block, as MiniMax's does (`400 invalid params, invalid tool_result content`), which
  otherwise ends the first turn after a `web_search` call with `retries_exhausted`. Each
  `search_result` block is rewritten to text using the rendering shared with
  `internal/provider/openai` (`provider.SearchResultText`); every other content block,
  the string form of `tool_result` content, and a block the rendering cannot parse all
  pass through unchanged. Default off; on a `protocol: openai` route, which already
  flattens unconditionally, the provider registry refuses it at startup — see
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) for why.

- **`make pins-test` now holds the companion clause too: every `actions/checkout` drops the
  credential** (#558). The guard added for #518 read the SHA pin and deliberately deferred this
  half, because a rule for it would have gone red on the three call sites that lacked the flag;
  those are fixed in the same change, so the rung lands with them rather than before them. Reading
  an input means reading a block, which the pin rung had refused to do — so the guard now measures
  a step's extent from its key column, in both directions since YAML fixes no key order, and reads
  only the lines at its `with:` block's own column. `persist-credentials` anywhere else stops the
  run by name and line rather than being counted: under `env:`, inside a block scalar, nested
  under another input, in flow style, set twice, or spelled in a way the scan cannot read. Each is
  a way the input reaches a diff, reads as compliant, and reaches checkout never. An explaining
  comment is deliberately not required, and the docstring says why.

- **`make pins-test` now checks that every action a workflow runs is pinned to a commit SHA**
  (#518), and runs in CI. `.github/dependabot.yml` has stated that rule since #96 and nothing in
  the repository could notice when #472 put four mutable tags back, so this checks the shape: a
  40-character commit SHA, and a trailing `# vX.Y.Z` naming the release — not because Dependabot
  needs one, but because forty hex characters say nothing about what runs, so without it the diff
  that moves an action is unreviewable. Shape is all it checks and its header says so: whether a
  SHA *is* the release beside it needs the network, which a gate has not got. Local actions and
  `docker://` images are exempt for want of a SHA. A `uses:` it cannot place stops the run by name
  and line rather than passing silently, and its self-test runs first, because a broken pattern
  and a clean repository otherwise print the same thing.

- **Run history over the wire: `GET /v1/deployment_runs` and `GET /v1/deployment_runs/{id}`** (plan 37 slice 5, [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). Both viewer reads. The list is newest-first, keyset-paged, capped at the published `limit` maximum of 1000 (the one managed-agents list whose stated cap is 1000 rather than the shared 100), and carries every published filter: `deployment_id` (200 with empty data for a well-formed unknown id, the published rule), `trigger_type`, `has_error`, and all four `created_at` comparators. `has_error=false` filters on the durable `succeeded_at` marker rather than the session link, and the single read renders a success whose session was later deleted with `session_id` and `error` both null — still a legible success — which closes [#520](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/520)'s render half and lands as a registry entry ([docs/DIVERGENCES.md](docs/DIVERGENCES.md)). A new `acceptance/` case drives the whole family — `Deployments.New`/`Get`/`List`/`Run`, then the run-record surface — through the typed SDK client against the control plane alone — no brain, no executor, no Docker sandbox (Postgres stays the suite's standing fixture).

- **The deployment scheduler: a cron schedule now fires** (plan 37 slice 4, [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). A 30-second controlplane sweep on every replica — no leader election: at most one committed run per occurrence rests on the partial unique index over `(deployment_id, scheduled_at)`, the reference's own idempotency key — reads one clock (`SELECT now()`, Postgres's) and fires each due deployment's single most recent occurrence inside a one-hour catch-up window: "missed triggers are not backfilled", generalized from unpause to downtime. A classified failure auto-pauses the deployment (`paused_reason.error` carries the type); an unclassified one rolls back whole — no run row, no pause, the next tick retries — because `unknown_error` would pause with no auto-resume shipped. The fired session is unattributed: a ticker has no principal. Every writer of the deployment row and the fire take a 5-second `lock_timeout`; a lost claim or a deadlock victim is a quiet retry, a lock timeout past the claim a counted abandonment. One poisoned schedule costs only its own fires, and the skipped count saturates at 1,000. Spans `deployment.tick`/`deployment.fire` (idle sweeps export none) and three instruments: `deployment.fires` by outcome, `deployment.occurrences.skipped`, `deployment.tick.duration`. Twelve registry entries ([docs/DIVERGENCES.md](docs/DIVERGENCES.md)).

- **Manual deployment runs: `POST /v1/deployments/{id}/run`** (plan 37 slice 3, [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). The manual trigger fires a deployment: one transaction records a persistent `drun_` run row, creates the session through the same path `POST /v1/sessions` takes, and settles exactly one of `session_id` and `error`. A classified failure — archived environment, vault or memory store, missing vault, store or file — is a 200 carrying the error-bearing run, never pauses the deployment, and rolls the half-made session back; anything unclassified records no run and answers the HTTP error. Fired sessions carry `deployment_id` (migration `0032`; the session-create surface still refuses the key), and the sessions list's `deployment_id` filter is real: scoped, keyset-paged, 200-with-empty for an unknown id. Success is durable — `succeeded_at` settles the run and `last_run_at` keys off it, so deleting a session no longer pulls that field backwards ([#520](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/520)'s read-path half; the run lists render in a later slice). Archived deployments answer 400; paused ones still run manually. Three registry entries ([docs/DIVERGENCES.md](docs/DIVERGENCES.md)).

- **The deployment surface — CRUD, archive, pause and unpause** — plan 37 slice 1's routes ([docs/plan/37_scheduled-deployments.md](./docs/plan/37_scheduled-deployments.md), [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). A deployment binds an agent, pinned to a concrete version, to an environment, credentials, initial events and an optional POSIX cron schedule. Nothing fires yet (the manual run and the scheduler are later slices), so this is the configuration a fire will read, plus two timestamps computed per read rather than stored: `upcoming_runs_at`, five whenever five exist and `[]` once archived, and `last_run_at`, the most recent scheduled run's start. `status` and `paused_reason` are computed too, from the pause columns, so the two cannot disagree — an archived deployment reports `active`. Archiving is terminal for update, pause and unpause; `GET` still answers. The list filters by agent, status, creation time and archived-ness. Refused once rather than nightly: a cron expression that can never fire, initial events the fire's normalizer would reject, a file rubric with no object storage, an unknown key or missing type in the schedule or agent object, and `budget`, since an unenforced ceiling is worse than a 400 ([#432](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/432)). Repository tokens are sealed before the write, never echoed.

- **The cron engine and the deployment schema** — plan 37 slice 1, starting the plan ([docs/plan/37_scheduled-deployments.md](docs/plan/37_scheduled-deployments.md) → `in-progress`, [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). `internal/cron` is the occurrence engine behind a deployment's schedule: the reference's 5-field POSIX dialect — no seconds or year field, no `L`/`W`/`#`/`?`, no `@daily` — matched literally against a wall clock in an IANA zone. `Due`, `Next` and `Upcoming` share one walk, so the list a client reads in `upcoming_runs_at` and the instant the scheduler fires cannot disagree. It imports `time/tzdata` itself rather than leaving that to a `main`: the server image ships no zoneinfo, so without it every non-UTC deployment would be refused in the image and never in the gate. Day-of-month and day-of-week union rather than intersect; a wall clock a spring-forward skips does not fire, one a fall-back repeats fires twice. The walk steps fields, not minutes, so its twelve-year bound costs nothing per read. Migration `0031` adds `deployments` and `deployment_runs`, keyed by the reference's own idempotency key, `UNIQUE (deployment_id, scheduled_at) WHERE scheduled_at IS NOT NULL`. `scheduled_at` is `timestamptz` for correctness: a fall-back's two occurrences share a wall clock and differ only as instants, so a zone-less column would collapse them and the index would swallow the second fire. No route is served yet.

- **`make identifiers-test` now checks the documentation for the shapes an operator's
  coordinates take** (#514), and runs in CI. A guard cannot search for the values
  themselves without containing them, which is the thing being prevented, so it searches
  for four shapes: a routable IPv4 address, a bare 12-digit project number, and an
  Artifact Registry path or `*.iam.gserviceaccount.com` address whose project component is
  not one of this repository's placeholders. It is **not** a proof that the repository is
  clean — a bare project id in running text has no shape to match, and source files are
  outside the scope on purpose. Its self-test runs first on every invocation, because a
  broken pattern and a clean repository otherwise print the same thing.

- **Plan 37 approved — scheduled deployments** ([docs/plan/37_scheduled-deployments.md](docs/plan/37_scheduled-deployments.md), [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)) — the design for the last v1 non-goal on the roadmap: a **deployment** binds an agent to an environment, credentials and initial events, and an optional 5-field POSIX cron schedule fires it, each attempt recorded as an immutable **deployment run** naming the session it created or the error that stopped it. Ten operations over eight paths, no delete anywhere. Key decisions: the scheduler is a controlplane sweep rather than a fifth binary, guaranteeing **at most one committed run per occurrence** across replicas through a partial unique index on `(deployment_id, scheduled_at)` — the reference's own published idempotency key — with no leader election and no advisory lock; one `SELECT now()` per tick, so replica clock skew cannot shift a schedule; `internal/cron` is hand-rolled and stdlib-only, embedding `time/tzdata` so no image can lack zoneinfo. Six slices. Webhooks ([#261](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/261)) and budgets ([#432](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/432)) are excluded and named. Four scope decisions — rejecting `budget`, admitting `system.message`, refusing to archive an agent a live deployment pins, and firing with no overlap brake — were settled with the owner before it landed.

- **`make gcp-env-tfvars` and `make gcp-env-init`, the two things a fresh checkout needs
  before it can touch staging** (#478). Remote state alone would not have made a destroy
  portable: Terraform evaluates the whole configuration before destroying anything, `*.tfvars`
  is gitignored, and a partial backend has to be told its bucket — so a clone stops twice. The
  generator replaces forty lines of documented shell, including the part that mattered:
  `gcloud builds get-default-service-account` prints `projects/…/serviceAccounts/EMAIL` in
  some versions and the bare email in others, and on Windows appends a CR. Each writes a file
  that looks right and then fails variable validation — at plan, so nothing is half-created,
  but a `destroy` fails identically and a destroy is what you run against an environment that
  is billing. It refuses rather than guesses: a disabled Cloud Build API names the step that
  enables it; an answer that is not a service account is rejected against a pattern
  deliberately *stricter* than Terraform's, which admits quotes and `${…}` that a quoted HCL
  string will not survive; and an existing tfvars is never overwritten, since it may carry
  `master_authorized_cidrs` or the two IAP settings. `gcp-env-init` is the cheap half — it
  points a checkout at the state and changes nothing, so `terraform output` and
  `make gcp-db-init` work without an apply. `make gcp-tfvars-test` runs all of it against a
  fake `gcloud`.

- **Memory versions no longer accumulate forever** (#476) — the control plane now
  sweeps at startup and then hourly for versions past the reference's 30-day
  window that are not among their memory's newest **five**, the last of plan 36's
  known consequences that was
  a job to write rather than a decision to make: a chatty agent rewriting a 100 kB
  memory every turn had been adding 100 kB per turn to a table nothing ever pruned.
  The window is the reference's, published; the count is this platform's, because
  the reference says "the recent versions are always kept" without saying how many.
  A live memory's head is never swept. A deleted memory's history prunes by the
  same rule as any other, which leaves it listable rather than immortal, and so
  does a redacted version. A store's hard delete still takes everything at once.
  [docs/DIVERGENCES.md](docs/DIVERGENCES.md) carries the count and what it rests on.

- **Memory stores are now covered where plan 36 said they would be and were not**
  (#488) — two rows the slice recorded as owed. The four memory instruments
  (`memory.materialized`, `memory.materialize.duration`, `memory.sync.actions`,
  `memory.sync.duration`) get the meter-reading test the skills, files and repos
  ones already had, so they cannot stop recording with every other memory test
  still green. And the whole path now runs through a real Docker container the
  agent does not own: a store materialized by the daemon, an unprivileged shell
  appending to one of its files in place with `>>` and creating another with
  `>`, and the run-end sync pushing both. Two rows already pinned the 0666
  mode's *value*, which a maintainer flipping it would update in lockstep; this
  is the first that shows why it has to be 0666 — set back to 0644 the append
  fails with `Permission denied`. The scenario needs a sandbox image that hands
  the uid the workdir, the shell state root and `/mnt`, which is what
  [docs/self-hosted-security.md](docs/self-hosted-security.md) §2 and §4 already
  ask of a non-root image; the test builds one rather than pretending
  `SANDBOX_RUN_AS_USER` alone is enough on this backend.

- **A staging environment parked and then forgotten now says so** (#504). Parking is a
  deliberate cost saving, so `deploy.yml` skips its deploying steps and finishes **green** and
  `deploy-alert.yml` stays silent on such a run in both directions — both correct, and between
  them nothing gets louder as the weeks pass while the LoadBalancer forwarding rules and the
  two storage charges go on billing. A new weekly `staging-parked.yml` asks the **cluster**
  rather than the deploy history — a deploy run exists only when somebody pushes, and a quiet
  fortnight is exactly when an environment gets forgotten — and keeps one issue open while the
  `power-saved-*` labels are there: opened only by a scheduled or hand-dispatched run, so the
  cadence is the grace period and no threshold had to be invented; never commented on again
  while it stands, because a standing flag's age is the point; and closed by the workflow
  itself once the cluster is unparked *or* gone, with a completed `deploy` among its triggers
  so reviving staging retires it in minutes. The label rule all of that turns on — a KEY
  beginning with `power-saved-`, one `;` away from also matching a label VALUE — moved into
  `.github/scripts/parked.sh`, which `deploy.yml` now shares so the two can never disagree
  about the same cluster, with `make parked-test` behind it.

- **A failed GCP deploy now opens an issue instead of notifying nobody** (#479). `ci` failing
  blocks a merge and is impossible to miss; `deploy` runs after the merge and reported to
  whoever thought to open the Actions tab — which is how the outage fixed in #469 ran red on
  every push for **seven days** unnoticed. A new `deploy-alert.yml` keeps one issue per
  outage: opened on the first failure and assigned, where it can be, to the run's actor;
  commented on each further one; closed by the next run that actually deploys. It names the
  step that failed rather than only the run, and says where the failure fell relative to
  `helm upgrade` — which decides whether staging is running the new commit, the old one, or
  part of each. It also answers the half of this that parking created: a run skipped because
  staging is parked is **green having deployed nothing**, so the notifier reads the run's own
  steps rather than its conclusion and never lets such a run close an open issue, and
  `deploy.yml` now says so on the run summary where it is legible without opening a log.

- **`cloudSQLProxy.connectionTest` makes the Cloud SQL proxy prove its instance is reachable**
  (#493). The proxy reports itself started as soon as its listener is bound — it dials
  nothing, and none of its three health endpoints dials either, `/readiness` included. Once
  the DSN routes through the proxy that is survivable, because the platform pings the
  database before serving and exits when it cannot, so a wrong instance crash-loops the pods
  and `helm upgrade --wait --atomic` rolls the release back anyway; what it costs is the
  diagnosis, since the visible failure is three application containers unable to reach
  Postgres. Setting this passes `--run-connection-test`, so the sidecar dials first and exits
  naming the instance it could not open. It matters most in the window where the proxy is
  enabled but the DSN does **not** route through it yet — a cutover's first step — because
  nothing exercises the proxy there at all. Off by default, since it otherwise duplicates the
  platform's own check a second earlier. It is a boolean: pass it with `--set`, never
  `--set-string`. GCP staging turns it on.

- **Memory stores work on `self_hosted` environments** (plan 36 slice 6, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)). A `self_hosted` session may now attach a `memory_store` — the interim 400 is lifted — and its BYOC worker mounts and syncs it over the wire, the twin of the cloud executor's half. The worker decodes the item's `secret` to the per-item sessions token (slice 5), lands each store from a `view=full` listing (marker and baseline beside the files), and at the run's boundary — before the tools when the sandbox already held the mount, once when the run ends, and a push-only flush that saves writes even when a stop cuts the run off first — reconciles the directory with the store through the five memory routes the token admits: pushes with the listed sha as precondition, deletes with the baseline sha, per-memory `GET`s for pulls, the store winning a both-sides change. It reads the routes' own statuses the reference's way — a 409 is the local edit losing, a 404 on update a re-create, a 400 `is archived` turns the rest of that sync pull-only, the 2,000 cap refused but not remembered. A session with a store but no token fails the item with the reference's `ErrSessionMemoryNoToken` rather than run it amnesiac, and the brain now renders the "Memory stores" block on both environment kinds. Worker telemetry gains the executor's `memory.*` instruments; one registry entry restated ([docs/DIVERGENCES.md](./docs/DIVERGENCES.md)).

- **A BYOC work item for a session with memory stores carries its sessions token** (plan 36 slice 5, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)). The poll response of an item whose session attaches a memory store now renders `secret` as the reference worker's envelope, `base64url(JSON {"sessions_token": "wtk_…"})` — a per-item token minted in the claim's own transaction (a failed insert leaves the item unclaimed), hashed at rest (migration 0030, `internal/worktoken`), null on every other path and item, the response `no-store`. The token is the worker's credential for that item, where the v1.66.0 reference worker sends it: the item's own heartbeat and stop, its own session's read and events, the skill reads, and the memories of the stores its session attaches (list, create, get, update, delete — a write is the session's version); a sibling session or an unattached store is not found, everything else refused, and the environment key is refused on the memory routes. It lives as long as the item — a re-hand-out, a lapsed lease or an archive ends it, a stop a minute after its request (the worker's wind-down and memory flush). Behavior-neutral until slice 6 lifts the `self_hosted` attachment refusal; the reference worker's `HandleItem` runs against the in-process server in a test (`github.com/creack/pty` joins go.mod, test-only). Two registry entries ([docs/DIVERGENCES.md](./docs/DIVERGENCES.md)); #165 re-scoped to the same envelope.

- **A `cloud` session's memory stores are mounted, told to the agent, and synced back** (plan 36 slice 4, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)). The executor lands every attached store at `/mnt/memory/<slug>` before the tools run — the memories as `0666` files beside a marker and a baseline — and the brain renders a "Memory stores" block after the repositories block: each mount with its access, description and instructions; an archived store reads read-only, a deleted one unavailable. When a tool run ends — and before it begins, for a mount the sandbox already held — the directory is reconciled with the store in three phases: the tree is hashed (a failed listing skips it), the changes settle inside the transaction that commits the run (pushes carry a compare-and-set and a `session_actor` version; the store wins a both-sides change; an emptied mount of several files is re-downloaded, marker and all, never read as deletions), and the settlement is written back. A `read_only` or archived store, or a directory whose marker is missing or altered, is pulled from, never pushed to; `write`/`edit` refuse a `read_only` or archived store's files and any path under `/mnt/memory` outside a store; the reaper syncs before every reap except a deleted session's. `FileWrite.Mode` on both sandbox backends, two evals (`memory-recall`, `memory-write`), five telemetry instruments, four registry entries ([docs/DIVERGENCES.md](./docs/DIVERGENCES.md)).

- **A `cloud` session attaches memory stores** — `resources[]` on session create accepts `{type: "memory_store", memory_store_id, access?, instructions?}` (plan 36 slice 3, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)). The element is stored in the reference's response shape — no id; `name`, `description` and `mount_path` snapshotted from the store at attach time — with `access` echoed as `read_write` when omitted and `mount_path` = `/mnt/memory/<slug of the name>`, both this platform's readings of what the reference leaves unstated (registered as inferences), and the create is refused with a 400 for an unknown, archived, twice-attached or ninth store, two stores whose slugs collide, `instructions` over 4,096 characters, or a `self_hosted` environment (until slice 6 issues the sessions token). `GET /v1/sessions?memory_store_id=` filters by containment, the resources list pages across the id-less element, and a repository may no longer mount at or below `/mnt/memory`. Nothing is mounted or told to the agent yet — that is slice 4. Seven registry entries added and three rewritten ([docs/DIVERGENCES.md](./docs/DIVERGENCES.md)).

- **Memories and memory versions inside a store** — plan 36 slice 2 ([docs/plan/36_memory-stores.md](./docs/plan/36_memory-stores.md) decisions 1, 4-6, 14, 17, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)): the five `…/memories` routes and the three `…/memory_versions` routes, so `ant beta:memory-stores:memories …` and `…:memory-versions …` work. A memory is text at a path — at most 1,024 bytes of NFC-normalized path with no empty, `.` or `..` segments, at most 100 kB of content — and a path is occupied by a memory at it, above it or below it, so `/a` and `/a/b` cannot both exist (409 `memory_path_conflict_error`, naming the blocker). `view=basic|full` chooses whether content rides along; the digest and size always do. Lists page by path in byte order, filter on `path_prefix`, and with `depth=1` roll deeper entries into `memory_prefix` markers. Updates take a `content_sha256` precondition (409 `memory_precondition_failed_error`; an update that changes nothing writes no version), deletes take `expected_content_sha256`, and every write appends an immutable version attributed to the API key or the person who made it. Redaction (admin) nulls a version's content, digest, size and path for good. A store holds 2,000 memories; deleting it removes them and their history. Eleven new entries in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md).

- **Memory stores — the `/v1/memory_stores` surface** — plan 36 slice 1 ([docs/plan/36_memory-stores.md](./docs/plan/36_memory-stores.md) decisions 1-3, 5, 6, 14, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)): six wire-compatible routes — create, retrieve, update, list, archive and delete — over a new `memory_stores` table, so `ant beta:memory-stores …` works against this platform. A store carries `name` (1–255 characters, no control characters), `description` (up to 1,024, `""` when unset, an empty string clears it) and `metadata` (the documented 16/64/512 caps, patched on update: a string upserts, a null deletes, an omitted key is kept). The list is newest-first with the inclusive `created_at[gte]`/`created_at[lte]` bounds, `include_archived` (default false) and keyset paging. Archive is idempotent and one-way, and an archived store is read-only: retrieve and list keep serving it, an update answers 400. Delete is a hard delete returning the `memory_store_deleted` tombstone. Reads need the viewer role, the rest developer; `memstore_`, `mem_` and `memver_` join the accepted ID prefixes. Six new entries in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md), `/v1/dreams` among them, and the accept-and-ignore beta-header rule amended in place. The memories and versions inside a store arrive next.

- **Staging can be parked between uses, and revived from any machine.** `make gcp-env-stop`
  resizes every GKE node pool to zero and stops Cloud SQL; `make gcp-env-start` reverses it;
  `make gcp-env-status` reports what is parked. Together they end the two charges that
  dominate the bill while keeping every resource — unlike `make gcp-env-destroy`, which takes
  the staging database with it. They need no Terraform, no state and no tfvars, only
  credentials for the project, so parking works from a machine that has never run an apply.
  The parked node counts live in cluster resource labels rather than on a laptop, so `start`
  restores the sizes that were actually running, and refuses the whole revival rather than
  guessing when one is missing. Order is enforced both ways: nodes drain before the database
  stops, and Cloud SQL reports `RUNNABLE` before any node returns. A parked environment is
  now skipped by `deploy.yml` with a notice rather than failing its smoke test — keyed on
  those labels, so a cluster at zero nodes that nobody parked still fails loudly. Do not
  `terraform apply` while parked; see [deploy/gcp/README.md](./deploy/gcp/README.md). (#480)

- **Plan 36 approved — memory stores** ([docs/plan/36_memory-stores.md](docs/plan/36_memory-stores.md), [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)) — the design for one of the v1 non-goals still on the roadmap: a workspace-scoped **memory store** attached through `resources[]`, mounted at `/mnt/memory/<slug>` and read and written with the ordinary file tools, every write an immutable, attributed **memory version**. Ground truth is the official memory and self-hosted guides, the OpenAPI spec, and `anthropic-sdk-go` at the pinned tag (every memory type is already there) plus, for the sync protocol, the reference worker's own implementation at v1.66.0. Key decisions: three Postgres tables, no blob; the id-less attachment element the reference's type dictates; materialization on the files pattern with sync-back at the end of every tool run, the store winning conflicts under a compare-and-set; a per-work-item **sessions token** in the poll response's `secret` for sessions with stores, because the reference worker fails an item with stores and no token; the BYOC worker running the same sync over the wire. Eight slices, the first an SDK bump to v1.66.0; dreams ([#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)), webhooks and version pruning ([#476](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/476)) are excluded and named. Its seven scope decisions were settled with the owner before it landed.

- **Two agents messaging each other can no longer loop forever on delegation alone** — #442
  bounded one thread chaining its own turn, but that count lives on the claimed work item, whose
  life is exactly one uninterrupted run of chained turns. A ping-ponging pair escaped it three
  ways: the peer's wake inserts a fresh row taking the column default, restarting the count; a
  turn that woke anybody requeues with it cleared; and `chainInput` reads a peer's message as
  input, which beats the cap outright. The third is not in the issue, and a pair that both stay
  `running` never wake each other at all — so closing the first two would have left it alive. A
  session now counts every turn that called a delegation tool in `sessions.delegation_turns`, on
  a row no wake replaces, and **refuses the claim** at 625 (`maxLiveThreads × maxSettlementChain`).
  Refusing rather than cutting keeps it safe: it never intervenes in a turn's fate, so it cannot
  idle a thread whose sandbox command is still in flight, and `send_to_agent` stays truthful —
  the peer really is woken, and its own next claim is refused. A refused turn issues no model
  request; the thread idles `end_turn` carrying `session_delegation_exhausted_error`. A message
  from outside returns the budget, a tool result does not — so a `self_hosted` pair emitting one
  real tool call per message is still unbounded, where a `cloud` one is not. Ours, not the
  reference's `budget`. (#447)

- **The registry's pointer invariant is executable now, not asserted** — `Tracked: #N` in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) is a present-tense claim that work is outstanding,
  written once and falsified later by someone closing #N somewhere this repository cannot see;
  that is how 65 of 111 pointers went stale before anyone noticed. `tools/registrycheck`
  re-derives the fact instead of trusting it. Its shape rules — the clause grammar, an INFERRED
  entry with no live tracker, a tracker shared by several entries that says nothing about the one
  citing it, and a bare `(line NN)` cross-reference, which had already drifted 77 lines once —
  are offline and free, so they run inside `make verify` as that package's own test. The rule
  that needs GitHub cannot join a gate that is credential-free by design, so `make registry-check`
  carries it and a new scheduled workflow runs that daily and on every pull request touching the
  registry. Every rung is proved against a document mutated to break exactly it, because a guard
  that has never seen a broken file proves nothing. (#452)

- **Coordinator delegation — a roster's agents run as real session threads** — plan 35 slice 4 ([docs/plan/35_multiagent-threads.md](docs/plan/35_multiagent-threads.md) decisions 6, 7, 8, 11, 13, [#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)): the primary thread of a session with a roster is offered `create_agent`, `send_to_agent`, `list_agents` and `wait_for_agents`, and every child `submit_result` and `send_to_parent`. Each is answered inside the settlement that emits it: a spawn's thread row, its child's first turn and the four events announcing both commit together; `wait_for_agents` parks the coordinator idle when something can still report, and a `submit_result` ends the child's turn. Agents message each other with `agent.thread_message_sent`/`_received`, which is also how a coordinator learns a child was interrupted, archived, out of retries or done without reporting — an ending that takes away the last child a parked coordinator was waiting on wakes it too; replay renders a received message as user text. The 25-thread cap counts the primary. On a `self_hosted` session the session list and stream now also carry every child thread's `agent.tool_use` and the results answering it, and the BYOC worker, told only that the session has a roster, walks the whole log and re-scans until nothing is left. Skills materialize as the roster's union. Registered in docs/DIVERGENCES.md ("Coordinator delegation", "The `self_hosted` session view").

- **Thread execution substrate — session status is a fold over its threads** — plan 35 slice 3 ([docs/plan/35_multiagent-threads.md](docs/plan/35_multiagent-threads.md) decisions 3, 4, 5, 9, 14, 15, [#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)): migration 0026 keys `model_turn` items by `(session, thread)`; a session's status is now derived from its live threads' (`running ≻ rescheduling ≻ idle`, the idle stop reason `requires_action ≻ retries_exhausted ≻ end_turn` with `event_ids` unioned), emitted only when the fold moves, so a single-agent session's wire is unchanged. The brain runs a child's turn on the child's log with the child's agent and settles the child's row; the exec drivers, the API's resume arms and the `tool_exec` stop re-arm run only the runnable set (allow-policy calls and confirmed asks — an ask on one thread never gates a sibling's call), wake each thread on its own; MCP servers are discovered and dialed per thread's agent; outcome grading runs on the primary at the session's quiescence. Inbound `session_thread_id` on confirmations, results and interrupts is accepted and validated; a thread-scoped `user.interrupt` ends that thread alone — the shared exec item stays and its late result is dropped; both drivers also cancel the call mid-run ([#441](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/441)). Registered in docs/DIVERGENCES.md ("Session status as a fold over threads").

- **Session threads — every session has a primary thread** — plan 35 slice 2 ([docs/plan/35_multiagent-threads.md](docs/plan/35_multiagent-threads.md) decisions 1, 2, 12, [#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)): the reference's `sthr_` session-thread resource and its five routes — `GET /v1/sessions/{id}/threads`, `GET …/threads/{tid}`, `POST …/threads/{tid}/archive`, `GET …/threads/{tid}/events`, `GET …/threads/{tid}/stream` — land wire-compatibly. Migration 0025 gives every existing and new session one primary thread whose id derives from the session's (`sthr_` + the session token), whose agent is the session's and whose status and usage follow the session's; its events list and stream are the session view. Every `session.status_running`/`_idle`/`_rescheduled` is now preceded in the same batch by the primary thread's `session.thread_status_*` naming the thread and its agent — single-agent sessions included (older histories hold no thread events). Events are stored once and filtered per surface, so a child thread's cross-posts carry `session_thread_id` on the session view and null on the child's own. Archiving the primary is a 400; an idle child terminates on archive (its pending calls closed first), every live child on the session's archive or delete. Nothing spawns a child thread yet — slice 3 runs them. Registered in docs/DIVERGENCES.md ("Session threads — the primary thread and the thread substrate").

- **Multiagent roster on agents and sessions** — plan 35 slice 1 ([docs/plan/35_multiagent-threads.md](docs/plan/35_multiagent-threads.md) decision 10, [#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)): agent create/update accept `multiagent: {type:"coordinator", agents:[…]}` and resolve it inside the write transaction — a bare agent id or a versionless reference pins the member's current version, an explicit `version` is kept, `{type:"self"}` — or any entry naming the coordinator's own id, so a GET echoes back — resolves to its own id and the version the write produces — enforcing the documented constraints (1–20 entries, distinct members, one `self`, members present, unarchived and not themselves coordinators) as 400s that name the offending entry; on update the roster is replaced as a whole and `null` clears it. Session create snapshots the roster into `agent.multiagent` as full member definitions (the wire's `SessionThreadAgent`), the `self` member being the session's own overridden coordinator spec; a session update patching the coordinator's tools or MCP servers rewrites that copy too; `multiagent` inside `agent_with_overrides` is now an explicit 400 instead of a silent drop. At this slice the roster is inert at runtime — a coordinator session still runs as a single-agent session; the entries below put its members on threads. Registered in docs/DIVERGENCES.md ("Multiagent roster — resolution and snapshot mechanics").

- **Plan 35 drafted — multi-agent session threads** ([docs/plan/35_multiagent-threads.md](docs/plan/35_multiagent-threads.md), [#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)) — the design for the coordinator topology: a coordinator agent's `multiagent` roster runs as concurrent **session threads** on one shared sandbox, each with its own event log, status, usage and stream, matching the reference's typed `sthr_` thread resource, its five `/threads` routes and its thread events on the wire. Ground truth is the official multiagent docs plus `anthropic-sdk-go` read at the pinned tag; the six delegation tools (`create_agent` … `send_to_parent`) are typed nowhere and land as registered inferences. Key decisions: a real thread row with the primary thread's id derived from the session's (every existing session gains a listable primary thread by SQL backfill); `events.thread_id` NULL = primary, cross-posts stored once and filtered per stream; one session-wide `seq`; session status as a fold over thread statuses; delegation resolved inside settlement, `wait_for_agents` answered immediately; coordinator sessions on `self_hosted` too, the BYOC worker staying thread-unaware because the session-level view it reads carries child tool calls (an inference the docs leave open). Six slices, the first an SDK bump to v1.63.1. `docs/REFERENCE_PROJECTS.md` gains `deepseek-harness` and `openai/codex` as harness design references. Landed as `draft`; slice 0 starts it.

### Changed

- **The MCP go-sdk's citations are held to the SDK citation grammar** ([#729](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/729)) — `tools/sdkref` now governs the go-sdk beside `anthropic-sdk-go`, go-jose and the `ant` CLI, and resolves it at the version `go.mod` pins. Its 51 citations in `docs/DIVERGENCES.md` and in the comments under `internal/mcp` and `internal/executor` each name a symbol and say what their tag means, as in `checked against go-sdk v1.7.0 — mcp/streamable.go streamableClientConn.checkResponse`, and reading them at that tag corrected three claims the code did not bear out. `make verify` now fails on one stamped at the pin that names a symbol the pin does not declare, and a pull request that bumps the go-sdk gets the same bump report as the SDK's and fails the same way until each transition is dispositioned ([docs/REFERENCE_PROJECTS.md](docs/REFERENCE_PROJECTS.md)). Rung 1 no longer sets a project name aside: a version written after a name, or after a link to it, belongs to another project only when the name is a module `go.mod` requires other than a governed source's, a word that merely contains a source's name, such as `mongo-sdk` or `go-sdk-tools`, or a required module that ends in one, such as `example.com/acme/go-sdk`, names no source, and a link to a source's issues or pull requests names nothing in it.

- **Plan 51 archived — the SDK citations are bound by symbol** ([#722](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/722), [docs/plan/51_sdk-reference-binding.md](docs/plan/51_sdk-reference-binding.md)) — four slices: `tools/sdkref` and its three rungs; the registry's citations migrated to the grammar, closing [#660](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/660); the Go comments' citations migrated; and the gate failing on shape and pin resolution. A pull request that brings a transition — a moved pin, or an edited citation — now fails until it is dispositioned. A bump costs one line in each entry citing a symbol the reference deleted, renamed or brought back, and nothing for the rest of the corpus. What the plan left out stays named in it: semantic drift under a surviving name, coordinates into this repository, and `anthropic-cli` beyond shape. Two follow-ups are filed: [#737](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/737) and [#738](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/738). The progress summary is in [docs/HISTORY.md](docs/HISTORY.md).

- **The default session listing hides `terminated`, as the reference's does** ([#574](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/574)) — `GET /v1/sessions` now drops a `terminated` session as well as an archived one. A 2026-09-03 sweep of the reference listed 28 sessions and deleted all 28, then a re-list with `include_archived=true` returned a 29th that was `terminated` and never archived — the blind spot that leaked that session's own resources, and one anything reconciling by listing inherits. Matching it means `include_archived`'s name now covers a status that is not archival, which is the reference's oddity adopted along with its behavior. An explicit `statuses[]=terminated` still returns them, so "terminated by failure" stays a question the API can answer on its own; **that half is ours** — the sweep never probed a status-filtered list, and the alternative reading, where nothing reaches past the exclusion, would leave a documented `statuses[]` value matching nothing without a flag its description never mentions. The session itself is untouched and still answers `GET /v1/sessions/{id}`.

- **An archived session now reports `terminated`, as the reference does** ([#710](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/710)) — a 2026-09-12 recording shows the reference reporting every archived session `terminated`, while this platform reported whatever it last held, normally `idle`. The status is now projected from `archived_at` when a session is rendered, and the list's `statuses[]` filter matches the same rule: `statuses[]=terminated` finds an archived session and `statuses[]=idle` no longer returns one, both alongside `include_archived=true`. Nothing is written — the stored column still never holds `terminated`, so everything that reads it for a decision (the running check before archive and delete, the sandbox reaper, the dream runner's arms) still sees what a settlement actually wrote, and a real terminal status, when one is produced, reaches them unmasked.

- **CLAUDE.md drops its two pre-v1 leftovers** (#715) — the `(post-v1)` marker fenced work
  off until v1 shipped, so now that it has, "do not build ahead of them" is wrong rather than
  merely stale, and a steering document is a thing contributors obey. The backlog bullet keeps
  only the rule that outlives v1 — GitHub issues are the only backlog — and the query listing
  the still-open deferrals moves to [README.md](./README.md), the one place that already
  defined them as seams reserved but not implemented, with the `--limit 100` bound and the
  false-positive caveat it was given in #445. Nothing now asks that a new deferral be marked;
  the marker stays on the titles already carrying it, so the query keeps working as an index
  of what those issues are. Beside it, Simplicity first stops naming vaults, deployments,
  memory, multi-agent threads and skills as reserved seams, all five having shipped since;
  what a reserved seam *is* still stands, because tenancy is still one. Draft plan 42's
  decision 8 loses the sentence that maintained the marker for #56, whose title had already
  lost it.

- **Ending a session now tears its sandbox down in seconds rather than at the next reap interval** — Deleting or archiving a session left its container or Pod running until the executor's reaper next ticked, up to a full `EXECUTOR_REAP_INTERVAL` (60s by default) later. On a laptop stack that looked like a leak; under load it was up to a minute of unpaid CPU and disk per ended session, and no API surface reports sandbox liveness, so nothing could poll for it. The ending transaction now publishes a wake the executor listens for, and the sweep it triggers is the ordinary one — same ownership, same per-session lock, same tiers — so idle-TTL and orphan behaviour are untouched and the interval becomes the worst case for teardown rather than the usual one. The wake rides the commit, so it adds no post-commit wait, reaches nobody before the row it is owed to, and reaches nobody if the ending rolls back. It may also be lost without harm: what the sweep re-reads is that row — the tombstone a delete wrote, `archived_at` for an archive — so a missed wake costs one interval, never a sandbox. A wake is work too, since every listening executor sweeps all it owns, so the reaper's load now follows the rate at which sessions end. Each executor holds one connection for the `LISTEN` outside its pool: size a server's limit for `pool_max_conns` + 1 per executor. No existing DSN needs changing. Supersedes plan 24's Decision 1 (#354).

- **Archiving an agent no longer reads the deployments it archived years ago to name the ones blocking it** — The refusal that lists the deployments pinning an agent had only an `agent_id` index behind it, so it read every deployment that agent has ever had, sorted them, and went back to the table to discard the archived ones. A new partial index carries the seek, the predicate and the ordering together. Naming the blockers is still linear in the live deployments, which is what it counts; what it stops being linear in is a history nothing deletes. It runs while the archive holds a row lock that the agent's deployment creates and repins queue behind, so the saving is theirs as much as the archive's (#523).

- **Plan 41 archived — dreams** ([#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475), [docs/plan/41_dreams.md](docs/plan/41_dreams.md)) — four code slices, from the five routes and their state machine to the in-place run: a **dream** consolidates one memory store over 1–100 session transcripts, rendered and secret-redacted into a session it drives through four stages, writing a clone or — with `output_behavior: update_existing` — the caller's own store. The transitional refusals the earlier bullets name are lifted: the pipeline is four stages, and `update_existing` is served. Slices 2-4 were accepted against a real model, slice 3 at the hundred-transcript bound. What the plan excluded stays named — the memory-store org cap as a registry entry rather than an inference, dream webhooks, any dream schedule — and the two executor sync questions the reviews surfaced are filed ([#626](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/626), [#631](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/631)). Slice 0's recording was never taken — `/v1/dreams` is a research preview this organization is not enrolled in — so the twelve entries it would have settled stay INFERRED in [docs/DIVERGENCES.md](docs/DIVERGENCES.md) against [#78](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/78). The progress summary and the acceptance records are in [docs/HISTORY.md](docs/HISTORY.md).

- **`unrestricted` egress keeps an address floor, and a refused dial says so** — a sandbox on an `unrestricted` environment may still reach every *host*, and the address each name resolves to is now judged before the connect: loopback, link-local (the cloud metadata endpoint), the unspecified address and multicast are refused, while RFC 1918 stays reachable by design — on-prem services on the operator's own private network are the self-hosted case. That matches a recording of the reference, which answers `http://169.254.169.254/` with **403 `Destination IP is in a private/reserved range`** and `https://example.com/` with 200 on one environment. The refusal carries that shape here too, where a dial the floor stopped used to surface as a 502 `cannot reach host` — an unreachable origin rather than a policy answer; a declared MCP endpoint or a package registry resolving to a refused address is corrected the same way. **This covers the sessions a gate is actually provisioned for** — `limited` ones and vault-attached ones, on a deployment that configures a gate image. Any other session networks directly with no gate in its path, so it is untouched and tracked as [#620](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/620).

- **The dials that reach a customer-supplied destination resolve their name once, and the address they judged is the address they use** — the per-session gate, the MCP client behind the executor, the vault-credential probe, the OAuth token refresh and the JWKS fetch now open their sockets through one dialler (`internal/dialguard`), which looks a name up **once**, holds every address that came back to the platform's address floor before any connect, and connects to those addresses with the multi-address failover, per-address budget and dual-stack fallback the standard dialler was providing. Previously the resolution happened below every decision the platform made: a host was admitted, and a credential chosen for it, while the address only ever appeared one syscall before `connect(2)`. This is not a universal egress check — configured model providers and web backends dial with their own clients, as before — and what a session may reach is unchanged, with one exception worth naming: an answer carrying an interface zone was refused outright by the mechanism this replaces, and is now dialled if the address floor admits it, which is what the standard dialler does with the same answer. What your resolver answers is unchanged too: a declared name that resolves to a private address still receives the credential chosen for it, which is the half of [#601](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/601) that needs a policy decision and keeps it open (plan 44).

- **Two spellings of one host are now one host, everywhere egress compares names (#609)** — `internal/egress`, `internal/vaultresolve` and `internal/mcp` each carried a hand-rolled ASCII case fold; they now share one canonicalizer that compares IDNA A-labels. Nothing plain-ASCII moves — an all-ASCII host still folds by case alone. What changes is Unicode: `Ä.example` and `ä.example`, U+212A (the Kelvin sign) before `.example` and `k.example`, `ſtrasse.example` and `strasse.example` are each one host, because each pair resolves to one name — while the Greek sigmas, which the fold #611 replaced merged into one, stay two. The MCP bearer that #609's first half withheld from those hosts rides again. An `allowed_hosts` entry written as a Unicode name is stored, and echoed back, as its A-label: `bücher.example` reads back `xn--bcher-kva.example`. A host IDNA refuses — an `_`-prefixed internal name, `s3--us-west-2.example.com`, an address literal, a `host:port` — comes back case-folded and behaves exactly as before. Both echoes are registered in docs/DIVERGENCES.md; docs/plan/43 carries the measurements.

- **Skills now serve the GA wire shape** — The Skills API was still serving the shape the reference retired on 2026-08-27, so no released SDK could create a skill against this platform in either direction: clients send `display_name`, which was rejected, while the platform wanted `display_title`, which no current SDK offers (#566). A skill now carries `display_name`, `latest_version_id` and a `source` object; a version carries the six GA fields and is addressed by a `skver_…` id or the alias `latest`, with the Unix-epoch number still accepted so pins made before this change keep resolving. `DELETE /v1/skills/{id}` deletes the skill's versions with it, and deleting a skill's only version is refused — the reference's semantics in both directions. `display_name` derives from the SKILL.md frontmatter, caps at 255, and is no longer required to be unique; both skills lists cap at 1000; the archive download sends the `Content-Disposition` the reference sends. **A BYOC worker must be upgraded together with the control plane**: the `version` field left the version object and the `latest` alias became addressable in the same change, so under a skew in either direction a `latest`-pinned skill — the default — quietly fails to materialize. The design is `docs/plan/39_skills-ga-shape.md`, pinned to a recording of the live endpoint.

- **The registry's MCP, egress, skills and environment-key inferences, settled against a second recording** — The second 2026-09-03 wave, 281 request/response pairs against the live managed-agents endpoint, settled fifteen of the archive's open comparison entries, narrowed two and dissolved one, touching 24 [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) entries. Thirteen findings said this platform was wrong rather than merely unobserved, and each became an issue (#570-#574, #577-#579, #581-#582, #589-#591): among them the link-local address floor `unrestricted` keeps where our sandbox egress does not, the create-time `allow_mcp_servers` gate, the package-registry hosts `allow_package_managers` opens, 403 not being an MCP authentication failure, the composed `mcp__{server}__{tool}` name we cap at 64 bytes where the reference does not, terminated sessions hidden from its default listing, the session-scoped `file_id` a resource echoes, and a past `auth.expires_at` we accept where it answers 400. `list_cost` in cents went to #432, and #576 is registered where the registry had been silent. Two entries were corrected rather than confirmed: `mcp_oauth_validate` probes nothing on this account, and the file-mount sentinel question dissolves because the reference mounts session storage over FUSE rather than streaming it.

- **Plan 37 archived — scheduled deployments** ([#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51), [docs/plan/37_scheduled-deployments.md](docs/plan/37_scheduled-deployments.md)) — six slices, from the cron engine and the deployment schema to the run lists: a deployment binds an agent to an environment, credentials, resources and initial events; `POST /run` fires it by hand, a 5-field POSIX cron schedule fires it on the clock — at most one run per occurrence across every replica, bounded catch-up, auto-pause on the fourteen pausing error types — and `/v1/deployment_runs` reads the history back. The close-out adds the two security-invariant bullets to [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): a scheduled session's `created_by` is NULL by design (a schedule is nobody), and a schedule does not bypass the permission policy — an `always_ask` tool call parks the session on a pending confirmation, unreapable until a human answers, interrupts or archives it, deliberately not refused at create. What the plan excluded is named and filed: webhooks ([#261](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/261)), budgets ([#432](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/432)), jitter and the overlap brake ([docs/DIVERGENCES.md](docs/DIVERGENCES.md)). The progress summary is in [docs/HISTORY.md](docs/HISTORY.md).

- **The answer-style eval trials drop the word "secret" from their prompts** —
  the five trials that plant a passphrase behind a mount, a skill, an MCP tool
  or a memory store now ask for "this task's passphrase" rather than "this
  task's secret passphrase" (`repo-answer`, which never used that phrase,
  instead stops describing its fixture file as holding a "secret passphrase"),
  and the four fixtures this repo plants lose the word from their planted text
  too; `repo-answer`'s fixture repository is operator-owned and unchanged, and
  the skill trial's `eval-secret` identifier stays deliberately — the measured
  variant kept it. Measured before landing, against the same endpoint on the
  same day: the "secret" wording failed 10 of 31 attempts across
  `file-answer`, `skill-answer` and `mcp-answer` — refusals, denials that the
  tool exists, fabricated answers — while the identical prompts without it
  failed 0 of 24; `memory-recall` and `repo-answer` take the change by
  analogy, unmeasured. The 2026-08-12 measurement recorded beside `repoAnswer`
  moved the ask's shape as well as the word; this change moves the word alone.
  Graders are wording-independent and untouched. (#530)

- **Session creation is extracted into a transaction-scoped `createSessionInTx`** (plan 37 slice 2, [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). `POST /v1/sessions`' transaction body — environment check, agent resolution, resource materialization, the session/thread/credential inserts, initial events — is now one function running inside a caller-supplied transaction, Begin/Commit and the post-commit metrics staying with the handler. Behavior-neutral: no wire shape, message or status changes, with the entire existing session suite as the regression test. The seam is what lets slice 3's `POST /run` and slice 4's scheduled fire create a session mid-transaction without a second copy of the platform's most load-bearing handler.

- **The eval suite retries model non-compliance once, reported** — a trial
  whose failures are all Either/Model class earns one fresh attempt (new
  session, new nonce) before the run reds; any other class — Platform above
  all — still reds immediately and is never retried, per plan 02's classing
  line ("M model non-compliance (one retry, reported)", now built and extended
  to Either, whose evidence cannot separate model from platform any better).
  Nothing is silent: the summary headline says `(N retried)` on any run that
  needed one, the superseded attempt keeps its record (`attempt: 1` in
  report.json), its transcript and its failure detail, the table marks both
  rows (`FAIL (retried)`, `PASS (retry)`), the headline count judges final
  attempts only, and the token totals still charge both. This is what stops a
  single stochastic refusal from a live endpoint redding the nightly while
  keeping every genuine platform signal loud. A retried skill trial re-uploads
  its fixture unchanged, display names no longer being unique. (#528)

- **Archiving an agent is now refused while a live deployment pins it** — plan 37 slice 1's second-to-last piece ([docs/plan/37_scheduled-deployments.md](./docs/plan/37_scheduled-deployments.md), [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). `POST /v1/agents/{id}/archive` answers 400 and names the deployments blocking it, up to five with a count of the rest; archive those first and the agent archives. The reference cascades here — archiving a deployment when its agent is archived — and this platform deliberately does not: nothing unarchives a deployment and there is no `DELETE /v1/deployments`, so one mistaken archive would destroy every schedule pinning that agent past recovery, while a refusal is recoverable by retrying in the right order ([docs/DIVERGENCES.md](./docs/DIVERGENCES.md)). Two things never block: an already-archived deployment, which can never fire, and a second archive of an agent that is already archived, which stays idempotent. The route became a transaction so the check is race-free against a concurrent deployment create or update.

- **`environment/`'s Terraform state moved to a bucket, so destroy and re-apply are no longer
  tied to one laptop** (#478). Local state made two of the four Terraform operations
  non-portable and one of them **silently**: `terraform destroy` iterates the state, so on a
  machine that does not hold it destroy finds nothing to destroy and reports *success* while
  the environment keeps billing. The bucket is owned by `foundation/`, which has to outlive
  every `make gcp-env-destroy`, and is versioned so a state lost to a bad apply is
  recoverable. The backend block is deliberately **partial**: a backend cannot interpolate a
  variable, and a bucket name is an operator identifier this public repository does not carry
  (#356), so it arrives at `init` time — derived from `PROJECT` and `NAME_PREFIX` on both
  sides rather than recorded as one more coordinate, which is why the targets that touch it
  now require `PROJECT`. Two guards stop the move reintroducing what it removes: those two
  variables choose the *bucket* while `terraform.tfvars` chooses the *resources*, so a
  disagreement is refused rather than applied against another environment's state; and a
  destroy over an empty remote state is refused, that being the original silent no-op wearing
  a new backend. `make gcp-env-migrate-state` is the one-time move, from the machine still
  holding the local file. `foundation/`'s own state stays local, deliberately.

- **The merge gate's test step now allows 30 minutes per package instead of `go test`'s
  10-minute default** (#490). The two largest Postgres-backed suites, `internal/api` and
  `internal/executor`, now run about ten minutes each on a loaded 8-CPU box — the
  executor measured past the default at 613 s, the api just under at 591 s — and a
  package killed at the ceiling reports `panic: test timed out` with a sub-second test
  "running", which reads like a hang rather than the budget it is. The default is sized
  for unit tests; these run a fixture container per binary and migrate a fresh database
  per test, and their per-test guards are the real limits. It also stops feeding a known
  leak: a timed-out binary skips the `defer` that removes its pgtest fixture, so every
  ceiling hit left idle containers to slow the next run. A genuinely wedged suite still
  fails, three times slower to say so.

- **GCP staging runs the Cloud SQL Auth Proxy** (#492). `cloudSQLProxy.enabled` is on in
  [deploy/gcp/staging-values.yaml](./deploy/gcp/staging-values.yaml), so the control plane,
  brain and executor each get the proxy as a native sidecar, and `deploy.yml` resolves the
  instance's connection name from the Cloud SQL Admin API at deploy time — no operator's
  project enters the deploy configuration, and no new repository variable is needed. The
  point is that a rebuilt Cloud SQL instance gets a new private IP while its connection name
  does not change, so the DSN stops being a thing a rebuild silently invalidates. The
  workloads' IAM was already in place — `environment/` grants all three service accounts
  `roles/cloudsql.client` unconditionally — but **the deploy identity needs a new
  `roles/cloudsql.viewer` grant** to read the instance, recorded in
  [deploy/gcp/README.md](./deploy/gcp/README.md) beside the other grants that live outside
  Terraform. **One step is deliberately not automatic**: `database-url` is a human-created
  secret by design, so pointing it at the proxy's loopback socket is an operator action,
  documented with its rollback in the same file. Until then the proxy runs unused and
  connectivity is unchanged.

- **Plan 36 archived — memory stores** ([#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52), [docs/plan/36_memory-stores.md](docs/plan/36_memory-stores.md)) — eight slices, from the SDK bump to v1.66.0 that pinned the wire to the BYOC worker's half that runs the sync over the wire. A workspace-scoped **memory store** attaches through `resources[]`, mounts at `/mnt/memory/<slug>`, is read and written with the ordinary file tools, and stays in sync across the sessions that share it — every write an immutable, attributed **memory version**, the store winning conflicts under a compare-and-set — on both `cloud` and `self_hosted`. The `cloud` path was accepted against the real `ant` CLI and a real model with two memory evals; the `self_hosted` path is covered by the worker lease-loop integration test, that `cloud` acceptance, and two verifier passes, the one remaining artifact — a live `ant beta:worker poll` transcript — deferred and tracked ([#495](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/495)). What the plan excluded is named and filed: dreams ([#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)), version pruning ([#476](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/476)), and the run-boundary sync cadence, all in [docs/DIVERGENCES.md](docs/DIVERGENCES.md). The progress summary, the acceptance record and slice 6's review record are in [docs/HISTORY.md](docs/HISTORY.md).

- **anthropic-sdk-go bumped v1.63.1 → v1.66.0** — plan 36 slice 0, starting the plan ([docs/plan/36_memory-stores.md](docs/plan/36_memory-stores.md) → `in-progress`, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)): only v1.66.0 pins the reference worker's memory-store behavior the plan's later slices must match. One user-visible change rides with it — v1.66.0 splits the built-in tool config into a union whose eight variants each carry `type` beside `name`, so a v1.66.0-shaped `tools` no longer gets a 400: `type` is accepted when it equals `name` (a mismatch stays a 400) and is now rendered on every built-in entry of the resolved echo, request or no request. The same release puts the web tools' `allowed_domains`/`blocked_domains`/`max_content_tokens`/`user_location` on the wire; those keys stay refused, the fence staying operator-side (`WEBTOOL_ALLOWED_DOMAINS`), and honoring them is [#481](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/481). Registry corrections: the work item's `secret` entry now records what the v1.66.0 worker actually does with the field, and the "per-tool allowed domains are configured by no wire field" inference is closed. Enumeration and citation audit: docs/HISTORY.md's "anthropic-sdk-go v1.66.0 bump" record.

- **Plan 35 archived — multi-agent session threads, the coordinator topology** ([#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53), [docs/plan/35_multiagent-threads.md](docs/plan/35_multiagent-threads.md)) — six slices, from the SDK bump that pinned the wire to the delegation tools that made the topology reachable. A coordinator agent's `multiagent` roster runs as concurrent **session threads** on one shared sandbox, each with its own agent, event log, status and stream; the session's status is a fold over them, and a single-agent session is the one-thread case of the same machinery, its wire unchanged. Accepted against the real `ant` CLI and a real model on both `cloud` and `self_hosted`, with the `coordinator-team` eval trial run live. What the plan left undone was filed rather than dropped, and both are closed in this release ([#441](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/441), [#442](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/442)). The inferences it had to make — the six delegation tools' schemas and answers, the `self_hosted` view widening, the status fold — are registered in [docs/DIVERGENCES.md](docs/DIVERGENCES.md) and tracked by [#78](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/78) against a future recording of a real coordinator session. The progress summary and slice 4's review record are in [docs/HISTORY.md](docs/HISTORY.md).

- **Upgrading to this release is a coordinated rollout** — migration 0026 re-keys the work queue's live-item dedup by `(session, thread, kind)` and drops the old `(session, kind)` unique index (plan 35 slice 3, [#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)), so a replica still running the previous release fails its enqueues (`ON CONFLICT` no longer matches a unique index) until it is replaced. The window is transient and self-healing — a settlement that cannot enqueue rolls back and its turn replays after lease reclaim; an API send surfaces a retryable 500 — but plan for enqueue errors from old replicas during a rolling upgrade, or restart the fleet together. The new index's `NULLS NOT DISTINCT` also sets the external-database floor at PostgreSQL 15 (the bundled deployments run 16).

- **anthropic-sdk-go bumped v1.61.0 → v1.63.1** — plan 35 slice 0, starting the plan ([docs/plan/35_multiagent-threads.md](docs/plan/35_multiagent-threads.md) → `in-progress`, [#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)): the threads surface it builds verifies against the latest release. Every managed-agents type change in the range is v1.62.0's and out of scope — advisor, budgets and list-cost usage, `session.usage`, `redacted`, `inference_geo` — so it is registered in docs/DIVERGENCES.md and filed (#430–#433), not built: `budget` on session create/update and an inbound `redacted` block are 400s, `inference_geo` is dropped like `effort`, and the session resource renders `budget: null`. Four v1.63.x behavior changes converged: an inverted `view_range` reads empty instead of erroring; skill archives materialize only regular files and directories (a symlink or FIFO entry is skipped, never written as a file; an upload whose `SKILL.md` is one is refused); an unwritable-target reason is spelled in Go's lowercase errno form; and a tool that succeeds silently posts `(no output)` — with every empty inbound text block now refused (400), the reference API's own rule for tool results, extended here to messages because an empty text block wedges replay. The full enumeration — 36 citations re-read, 21 line drifts and one content restatement corrected — is docs/HISTORY.md's "anthropic-sdk-go v1.63.1 bump" record.

### Fixed

- **The SDK claims that named a source but no tag are dated, and two no longer held** ([plan 51](docs/plan/51_sdk-reference-binding.md) slice 4, [#722](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/722)) — twelve claims in [docs/DIVERGENCES.md](docs/DIVERGENCES.md) and the Go comments named `anthropic-sdk-go`, go-jose or the `ant` CLI beside a symbol or a package, with no tag. Nine now carry a citation read at the SDK's v1.70.1, go-jose's v4.1.4 or the CLI's v1.30.0; three sat beside a citation that already dates them, and no longer name the source. Two claims did not hold at those tags. The environment-delete entry said the CLI's only `force` flag is work stop's, but v1.30.0's `ant apply --force` is another; work stop's is still the only one the CLI sends to the API. The session-events limit entry called the agents list the one SDK list documenting a maximum of 100; several managed-agents lists do.

- **The Go comments cite the SDK by symbol, and each citation says what its tag means** ([plan 51](docs/plan/51_sdk-reference-binding.md) slice 3, [#722](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/722)) — the comments' `anthropic-sdk-go`, go-jose and `ant` CLI citations are rewritten in plan 51's grammar — 158 citations across 78 files, each resolved at the tag it names — and `tools/sdkref` reports no finding in them or in the registry. Line coordinates into a tag are symbol anchors, and a `since v1.63.1` names its source. Reading each claim at its tag also corrected the ones that did not hold: `internal/events/inbound.go` no longer calls v1.66.0 the pin; the brain's turn classification no longer says the SDK's agentic loop classifies a turn by its tool blocks, which it stopped doing at v1.67.0; the Anthropic adapter no longer dates the compaction of `param.SetJSON` bytes to v1.60.0, which the path it takes predates; and the session-resources list limit quotes the SDK's "max 1000" rather than a "1 to 1000" no tag carries.

- **The divergence registry cites the SDK by symbol, and each citation says what its tag means** ([#660](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/660), [plan 51](docs/plan/51_sdk-reference-binding.md) slice 2) — [docs/DIVERGENCES.md](docs/DIVERGENCES.md)'s SDK, `ant` CLI and spec citations are rewritten in plan 51's grammar — 363 citations across 143 entries, each resolved at the tag it names — and `tools/sdkref` now reports nothing in the registry. The bare `:NNN` continuations that borrowed a file from the prose around them, #660's defect, are gone, and the `anthropic-openapi.yml` coordinates, which named a file no tag ships, are schema paths into the spec the pinned SDK bundles. Reading each claim at its tag also corrected the ones the SDK had moved past: the reference worker no longer force-stops an item whose lease it lost, the environment-delete and deployment-action params gained a workspace header, the skills toolset's cleanup keeps the skills directory, and the doc comments two entries attribute to `SetupSkills` now sit on `SetupSkillsFromSession`.

- **Archives count the status moves their child endings make** ([#731](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/731)) — ending a child thread can move its session's status: from `rescheduling` to `idle` when the last retrying child goes, or, for a session stored `terminated`, to `idle` and back. A session archive (the dream runner's closing one included) and a thread archive wrote each such move to the log but left it out of the `session.status.transitions` metric; each is now counted. A `wait_for_agents` still does not wait on a child at `rescheduling`, deliberately: nothing resumes a thread resting there, so a coordinator parked on one would never be woken. Nothing leaves a thread at `rescheduling` across a commit or stores a session `terminated` today, so none of these cases was reachable yet.

- **The thread and session archive rules are recorded side by side** ([#730](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/730)) — archiving a child thread is refused unless it is `idle`, following the reference's "Archive only succeeds if the thread is `idle`", while a session's archive or delete is refused only when `running`, following its session docs. The two part at `rescheduling`: a child there is refused its own archive but ended by its session's whenever no other thread is running. Each page also admits the other reading, so docs/DIVERGENCES.md records both answers as inferences, and tests pin both halves. No behavior changes.

- **The dream runner no longer archives a session that started running** ([#716](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/716)) — a dream's closing arm decided between interrupting its session and archiving it from a status read without a lock, so a session that went from idle to running in that window was archived mid-turn. Archiving closes the log to appends, leaving a brain working against a log that refuses it — a state no caller could produce, because the public archive path takes the session row's lock and re-checks the status under it. The closing arm now does the same, and interrupts instead, leaving the close for the next tick — taking the session's usage under that lock too, since the close folds it in and nothing re-reads it afterwards. The read that selects which arm runs stays unlocked on purpose: an ask commits together with the flip to idle, so a stale busy read there costs a tick and nothing more.

- **The verifier reads the SDK pin instead of being told a version that rots** ([#724](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/724)) — `.claude/agents/verifier.md` told the verifier to judge wire compatibility against "the SDK version pinned in `go.mod` (v1.66.0)" while `go.mod` pinned v1.70.1. #593 moved the pin on 2026-09-05 and swept neither steering document; #667 corrected `docs/REFERENCE_PROJECTS.md`'s copy by hand and nothing reached `.claude/`, because nothing linked either copy to the pin. Both now name `go.mod` as the authority and state no version, and the verifier is told to read it there rather than trust a version named in any document, its own instructions included. Three `docs/DIVERGENCES.md` citations stop equating their stamped tag with the pin, which two of them had wrong; the registry's other pin restatements are [plan 51](docs/plan/51_sdk-reference-binding.md) slice 2's to migrate. `TestSteeringDocsDoNotPinTheSDKVersion` (`internal/domain/docs_test.go`) holds the two documents to this inside `make verify`: each must still carry the clause sending a reader to `go.mod`, `verifier.md` may name no `vX.Y.Z` tag at all, and `REFERENCE_PROJECTS.md` only one that `since`, `checked against` or `absent at` dates — plan 51's convention. Both rules key on the legal form rather than on the defect's wording, so rewording or reflowing the claim cannot disarm them.

- **The last six object deletes that orphaned on a store refusal now record what they owe**
  ([#703](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/703), which folds in
  #693) — deleting a file, closing or settling a dream, deleting a skill or one of its
  versions, and the harvest replacing a snapshot each removed their rows and then removed the
  objects best-effort. The rows were gone by then, and the ids were the objects' only names, so
  a store that refused or a process that died in that window left bytes no tier could
  enumerate. Each now enqueues its keys on the same transaction that removes the rows, for the
  sweeper plan 50 built — the shape `deleteSession` and the expired-file sweep already used, so
  no object a committed row named is orphaned by a store having a bad day any more. A skill
  cascade is the biggest beneficiary: it was N sequential deletes with nothing left to answer
  the client with, and a bad day orphaned every archive the skill had rather than the odd one.
  One class of object deliberately keeps the best-effort delete, under helpers renamed to say
  why: the one whose row **never committed** is owed to nothing, and queueing it would retry a
  delete past the cancellation that is all that protects a commit which may in fact have
  landed.

- **Three registry entries stop guessing where the 2026-09-12 recordings answer** — the skills upload row's central inference is refuted: the reference selects archive handling by the filename's **extension**, not by the `PK\x03\x04` magic bytes this platform reads, so a real archive named `archive.txt` is expanded here and refused there. Its other questions close — an unnamed `files[]` part is a 400 rather than an `anonymous_file`, two further unknown part names are ignored as decision 8 extrapolated from one, and of the two non-regular manifest types tried the reference refuses the symlink and accepts the FIFO, where one predicate here refuses both. Both mechanism and width are [#630](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/630)'s to converge or register. The web-tools row gains the half it had recorded as *created and never run*: a fenced `web_search` filters **silently** — no error, no `url_not_allowed` — where its fetch twin errors, so [#481](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/481) has two answers to match rather than one; exactly 64 list entries are accepted, and a positive `max_content_tokens` truncates with no marker. That row's inference that a blocked fetch is not a counted request is withdrawn: a *successful* unfenced fetch reports the same `0`. Finally the session-budget row's *unobserved* clause is answered — the reference renders `budget: null` for a session created without one, as we already do.

- **A session archive leaves its primary thread alone, as the reference does** ([#713](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/713)) — archiving a session used to stamp the session's `archived_at` onto the primary thread row and move that row's `updated_at` with it. A 2026-09-12 recording shows the reference doing neither: its archived session's primary keeps the status, the null `archived_at` and the `updated_at` its own last change left, and only the session row moves. `GET /v1/sessions/{id}/threads` after an archive now renders the same. Child threads are untouched by this — the session's end still terminates and archives every live child. Migration 0040 clears the column on existing primary rows, which the old archive path and 0025's backfill had both filled, so one deployment does not render an archived primary for last week's session and an unarchived one for tomorrow's. A session archived by an old replica partway through the rolling upgrade can still keep one, which [#720](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/720) closes with a constraint once no replica writes the mirror. The `updated_at` those rows already moved is not recoverable and stays as it is.

- **A recording settles what archiving a session does to its primary thread** ([#78](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/78)) — the registry carried the primary's end as one unchecked inference: it never emits `terminated`, and its row carries the session's `archived_at`. A 2026-09-12 recording, which read two archived sessions' threads and their full event lists together, splits the two. The archive does not end the primary: it is still `idle`, and the 41-event list of the session that had run turns announces neither that nor the `terminated` status the session itself reports — though a stored list cannot rule out an ephemeral announcement, so the wording stays scoped to what was read. The other half is refuted. The reference does not mirror the session's `archived_at` onto that row, leaving the primary unarchived with its `updated_at` untouched, where this platform set both until [#713](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/713) dropped the mirror rather than argue it as a divergence. The session status this platform never wrote is [#710](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/710)'s, which now projects it from `archived_at` at read time.

- **A restarting executor tears down its predecessor's sandboxes at once, not an interval
  later** — the sandbox reaper waited out a full `EXECUTOR_REAP_INTERVAL` before its first
  pass, because a Go ticker does not fire when it is created. It usually had a boot pass
  anyway, but not one of its own: the reap-kick listener sweeps whenever it establishes a
  `LISTEN`, and that wake was carrying it. Where the listener never establishes — a pooler
  that refuses `LISTEN` or multiplexes away the backend that would deliver it, a server with
  no connection to spare for the dial — an executor restarting more often than the interval
  left its predecessor's containers standing after every restart, and a crash-looping one left
  them for as long as it kept crashing. The loop now passes before it waits, as the five
  control-plane sweeps already do (#709).

- **A restart no longer loses a scheduled run, or fails a dream that never started** — the
  deployment scheduler and the dream runner each waited out a full tick before their first
  pass, so a restart cost the work that fell in that window rather than delaying it: an
  occurrence that was still inside the one-hour catch-up window when the control plane booted
  could be outside it one tick later, never fired and recorded nowhere, and a dream left
  pending with less than a tick of its timeout budget was failed as `timeout` without ever
  starting. Both loops now take a pass before the first wait, as the memory, expired-file and
  object-delete sweeps already did — normally, since the dream runner's pass still yields when
  the shared sweep budget is saturated (#699).

- **A `dialguard` test no longer risks holding a CI run for its package's whole
  30-minute timeout** (#689). `TestTheLosingFamilysConnectionIsClosed` waited on an
  unbounded channel for a connection the losing address family produces only if its
  dial was entered at all — which a fallback winning first can prevent. One `coverage`
  job was lost to it, on a diff with no path to the package. The losing family's entry
  into the dial is now waited for rather than assumed, and every wait in the test is
  bounded, so a starved run fails in seconds rather than taking the whole run down
  with it. No other test in the package waits on a value handoff a cancellation can
  skip. Test-only; no production code changed.

- **A backlog of expired files drains in one pass, and an open dream's transcript is never
  swept** — the expired-file sweep took one bounded batch per hour, a ceiling that existed to
  cap how many object deletes a single tick owed. It owes none since it started recording its
  debt instead of paying it, so the batch now bounds a transaction rather than an hour and a
  tick keeps going until a batch comes back short: a registry with 50,000 expired rows clears
  on the next tick instead of over the following two days, during which its metadata outlived
  the window the reference publishes. The sweep also refuses a file an open dream owns, which
  is the refusal the manual delete route already makes — nothing stamps an expiry on a
  transcript today, but that was a fact about the current writers rather than an invariant,
  and the cost of it changing was a transcript removed from under a runner still appending to
  it. A closed dream's transcript stays ordinary. One log key is corrected: the success line
  said `expired_before` and carried a duration, where its sibling sweep says `older_than`
  (#698).

- **The expired-file sweep can no longer lose the objects it orphans** — it removed a
  file's row and then deleted the object best-effort, which left three ways for a batch to
  vanish without trace: a shutdown landing while the statement's ids were still being read
  could commit the removal and lose the ids with it, a store refusing every key of a healthy
  sweep did the same for up to a thousand objects an hour while logging counts and no ids,
  and a process dying between the two steps did it without either going wrong. However it
  happened, nothing in any tier still named those objects. The sweep now records the
  debt instead of paying it — one `pending_object_deletes` row per object, written in the
  transaction that removes the file rows — so the removal and the record commit together or
  not at all, and the object-delete drain that already serves session deletes retries each
  key with backoff and drops none. This sweep therefore reaches no object store: it has no
  budget to run out of, nothing best-effort left in it, and a deployment whose store is down
  or absent falls behind rather than losing anything. The control plane's termination grace
  period is unchanged but is now margin rather than arithmetic, the detached 30-second
  cleanup it was sized for having gone with the object deletes (#696, and the first finding
  of #698).

- **The bundled MinIO now comes from quay.io** — Docker Hub stopped serving the
  `minio` namespace, answering an anonymous pull with "repository does not exist"
  rather than a rate limit, so every machine without the image already cached lost
  it at once: the compose stack, the chart's in-cluster MinIO, and the
  object-storage contract tests alike. All three now pin the identical release at
  `quay.io/minio/minio` — MinIO's own mirror, where that tag was checked to
  resolve to the same manifest digest the Docker Hub copy had. An operator
  mirroring images into a private registry needs a second remote pointed at
  quay.io for this one, because a remote repository pointed at Docker Hub cannot
  serve it; `deploy/gcp`'s mirror output says so where it explains the rewrite
  (#701).

- **A session delete no longer loses the objects it fails to remove** — Deleting a session removed its deliverables' bytes and its workspace checkpoint on the request path, best-effort under a time budget. Whatever a slow or refusing object store did not finish was orphaned for good: the registry rows were already gone, so nothing named those objects any more and no later delete, sweep or reap could find them. The delete now records what it owes rather than paying it — one `pending_object_deletes` row per key, written in the transaction that removes the session, so the debt commits exactly when the rows that referred to those objects stop existing — and a control-plane sweeper drains the queue, deferring a refused key with exponential backoff up to an hour rather than dropping it. An object-store outage therefore no longer costs a delete its bytes, and no longer slows the response either: the request path does not reach the store at all. The workspace checkpoint now rides with the deliverables. Its old excuse was that the executor's reaper removed the same key, but a reap pass visits only the sandboxes the provider still owns, so a session whose sandbox the idle tier had already destroyed had no second remover (#645, #320).

- **The registry says how the reference resolves a skill's `latest`, as the pinned SDK does it** ([#657](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/657)) — the alias has always been resolved at skill-materialization time rather than at agent create, and [docs/DIVERGENCES.md](docs/DIVERGENCES.md) said so; what it also said was that the reference's worker resolves it *itself*, by listing a skill's versions and picking the newest. The reference stopped doing that, and the function is gone at anthropic-sdk-go v1.70.1 — the version `go.mod` pins, which is where we read it rather than when it changed: the worker now retrieves the version addressed by the alias and downloads by the concrete id that call returns, leaving the resolution to the server. Where it happens is unchanged; who performs it is not, and the entry now says which. No code changed — a retrieve addressed by `latest` already echoes the version row's own id, which the download accepts — but the two calls were only ever pinned apart, and the newer flow is the composition of them, so a test now walks it end to end on the bare and `?beta=true` paths alike.

- **A memory path carrying U+2028 or U+2029 is refused, and a memory already stored at one can still be edited** ([#656](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/656)) — the documented path rule gained the Unicode line and paragraph separators between the SDK version this platform was built against and the one `go.mod` pins, and `ValidatePath` had kept refusing only the older list: control and format characters, `Cc` and `Cf`. Those two separators are `Zl` and `Zp`, which are neither, so a path could carry a line break that no rule caught — invisible in a listing, and load-bearing on the sync lane, where a path is a filename read off a sandbox rather than a string a client chose. The API now answers the documented 400 for them, on create and on rename alike, and the sync lane refuses such a file locally instead of pushing it. Because the old rule let those paths through, a store may already hold one: the sync now validates a path only on the push that would **create** a memory, since a push carrying a memory's id sends content alone, leaving the name to the store's own copy. Refusing that one would have stranded the edit — nothing retries a refusal, and the next remote change overwrites it.

- **A file's two scope columns can no longer disagree** — `files.scope_type` and `files.scope_id` were independently nullable, so a row could carry an id without a type: `GET /v1/files?scope_id=` would return it while the file object itself reported no scope. A new `CHECK` makes the pair move together. No writer in this platform could produce such a row — uploads and dream files write neither column, the outputs harvest writes both in one statement, and there is no production `UPDATE` of the table at all — so the migration is expected to apply everywhere; it validates rather than deferring, and an operator whose database somehow holds one will be stopped at startup rather than served a file that contradicts its own listing (#659).

- **The divergence registry says which SDK tag each entry was checked against** ([#612](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/612)) — `go.mod` moved to anthropic-sdk-go v1.70.1 while [docs/DIVERGENCES.md](docs/DIVERGENCES.md) still judged the wire at v1.66.0, and its coordinates had drifted with it: the `memory_store.*` webhook entry cited `betawebhook.go:514-515`, which at v1.70.1 names event-data types, not event names. **25 entries had every one of their coordinates resolve** — each matched by its own text at both tags, not by arithmetic — and are re-based onto v1.70.1: 51 coordinates move across 23 of them, two were already byte-stable, and four that named no tag now name one. 35 further entries cite no SDK line — the spec or a symbol — and move only their version label. **No other entry is touched.** An entry is re-based only if every coordinate resolves under an extraction that refuses to guess which file a bare `:1134-1137` continuation belongs to, so one resting on such a continuation is left alone even where a hand reading would place it. Re-labelling un-rechecked numbers turns an accurate citation into a wrong one. Their `v1.66.0` is the stamp it always was, a convention [docs/REFERENCE_PROJECTS.md](docs/REFERENCE_PROJECTS.md) now states outright; [#660](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/660) carries them per entry — with the keyset pointer moved there too, 61 entry lines change.

- **Every file object carries `expires_at`, and carries `scope` only when it has one** — `GET /v1/files`, `GET /v1/files/{id}` and the upload response now spell out `expires_at` (present on every file object, `null` when the file has no expiry) and omit `scope` entirely for a file with no scope, instead of sending `scope: null` and no `expires_at` at all. Both halves now match the recorded reference behavior: of the eight file objects in the wire archive, eight carry `expires_at: null` and only the two session-scoped ones carry `scope`. A client reading `expires_at` no longer sees a missing key where the reference sends a present-but-null one (#651).

- **A session delete's terminal frames no longer follow the deleting client out** — Deleting a session commits, then broadcasts the frames its stream subscribers are waiting for: one `session.thread_status_terminated` per live child thread, then `session.deleted`. Those broadcasts ran on the deleting request's own context, so a client or proxy that gave up any time after the commit succeeded cancelled them for everyone — and they cannot be republished, the rows they would have been appended to having cascaded away with the session. They now run detached from the request under their own short budget, the decision the same function's object cleanup already made for the same reason. The two kinds of frame were owed differently, and only one of them was ever recoverable: a `session.deleted` still reached its watchers a ping later, synthesized by the stream's own check for a session that is gone, while a child thread's termination had no second source anywhere and was lost outright (#646).

- **The files list carries `next_page`, and the cursor it carries walks** — `GET /v1/files` answered `{data, has_more, first_id, last_id}` where the reference sends `next_page` beside those three — the documented list shape for the beta this platform pins — explicitly `null` when no page follows. A client that reads a missing key and a null key alike never noticed; a strict schema validator, or a language whose JSON binding raises on an absent property, did. It was also the half of pagination the reference's own SDK now reads: `Beta.Files.List` moved to a `next_page` cursor in anthropic-sdk-go v1.68.0 and stopped reading `has_more` at all, so a current client took the first page and stopped there — and the `?page=` cursor it sent back was accepted and silently ignored, which is the same wrong answer arriving without an error. The route now emits the key on every page, empty ones included, carrying the position after that page in the list's newest-first order, and takes it back as `?page=` — joining the lists whose cursor grammar it already shares, foreign-cursor rejection (#534) included, and refusing `page` combined with `after_id`/`before_id` as the reference does. Those two and `has_more`/`first_id`/`last_id` are otherwise unchanged (#544).

- **A cursor from the wrong list is now refused instead of quietly answered** — Every list ordered by creation time checked page cursors against the foreign kinds by hand, and eleven of the fourteen spellings were short: seven admitted both a sequence cursor from a session's event list and a path cursor from a memories list, and four admitted a path cursor. Either decodes to a zero timestamp and an empty id — a legal position for a keyset comparison — so it bound into the query and produced a plausible 200 where the reference publishes a 400. What that 200 contained depended on which way the comparison happened to point: the descending lists reported end-of-history, while the ascending ones — a session's threads, and the sessions list itself under `order=asc` — served the whole first page — the rows a request carrying no cursor would have returned. One shared predicate now names the kinds a time-ordered list cannot honor, so the fourteen guards agree and the next list to be added inherits the rule rather than rebuilding it (#534).

- **Deleting a session now removes the files it produced** — A session's harvested deliverables are registered in the files table under a polymorphic scope, which carries no foreign key, so nothing tied them to the session that produced them: deleting the session left both the registry rows and their bytes in object storage, and clearing them meant deleting each file individually through the files API. The delete now removes the rows in the same transaction that removes the session. Files uploaded through the files API are untouched, which is the reference's own split — a file the session produced is scoped to it and goes with it, an uploaded file is an independent resource — and archiving a session, which preserves its history, still leaves everything in place. The objects are not removed on the request path: the delete enqueues their keys in the same transaction and a sweeper removes them, retrying whatever a store refuses (#266, #645).

- **Deleting a session while its gate re-mints no longer deadlocks** — The two paths took the session row and its gate-token rows in opposite orders, so a delete racing a re-mint closed a lock cycle and Postgres aborted whichever side it picked: a 500 from the delete, or a failed gate provisioning, with no way for the caller to tell in advance which it would be. Minting now takes the session row first, the order the delete already took, so the race resolves rather than aborting. It also settles what happens when the delete gets there first, which was previously that same unpredictable pick: the re-mint waits for it and then fails against a session that is gone, the same outcome the cycle produced whenever the delete happened to be the survivor. When the re-mint gets there first there is no loser at all — it commits, and the delete cascades the new token away with the session. Both outcomes were already retryable and neither could corrupt anything; what changes is that the abort stops happening (#313).

- **An MCP server answering 403 on a dial is now a connection failure, not a refused credential** — The session's error event said `mcp_authentication_failed_error`, which asserts the server refused the credential, when the server had accepted it well enough to make a decision about the request. It says `mcp_connection_failed_error`, which is what a recording of the reference answers: five servers dialled in one turn returned an authentication failure for 401 alone, with 403 ("access forbidden"), 407, 500 and 502 all reported as connection failures. The call is still answered with an `is_error` result so the turn carries on, and the event still carries `retry_status: retrying`, which the same recording confirms the reference sends for both types. Two things travel with the type: the stored message names the exchange that failed rather than the credential, and a 403 answered as the execution pass runs out of time is now dropped like every other connection failure in that position rather than kept. What the reference does with a 403 on the tool call rather than the dial is unobserved (#78), and what this platform should do with one is #641 (#572).

- **`web_search` results now tell the model it may cite them** — Each `search_result` block the built-in `web_search` tool returns carries `citations.enabled: true`, which is what a recording of the reference caught it sending on all ten of its own result blocks. This platform emitted `false`, and that flag is what tells the model whether it may cite a block — so a model driven through this platform was told it may not cite results the reference would have let it cite. That is model-visible rather than a rendering detail, and on a `flatten_search_results` or `protocol: openai` route the flag does not survive the conversion at all (#548).

- **The `redacted` content block is a settled answer, not pending work** — a `user.message` carrying `{"type":"redacted"}` has always been refused with `400 content block type "redacted" is not allowed here`, and the reference's published docs confirm it refuses one the same way, so that half is conformance rather than a divergence. The emit half stays a divergence and is settled too: the reference emits the block as a placeholder for content withheld by Anthropic model policy, and this platform never will, because it runs no such policy and nothing has asked for one. [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) now records both, with the warning that the SDK's inbound param union registers a `redacted` arm for a request the reference itself refuses — so the inbound vocabulary must never be widened from the generated types. (#430)

- **The Docker sandbox tests and the provider now agree on which daemon they mean** (#627).
  Those tests shell out to the `docker` CLI for states the sandbox API cannot produce — a container
  stopped but not destroyed, an image built outside the provider, a container it does not own. The CLI
  follows the active `docker context`; the provider reads `DOCKER_HOST` and then the well-known socket.
  On a host where those name different daemons a fixture was created against one and read back from the
  other, and the failure read as though the product had lost it. Every such call in the package now goes
  through one helper that passes the provider's own address as `--host`, which outranks both environment
  variables. The helper that reads a container's state also stops slicing that status out of index
  arithmetic that could panic on empty output, and stops discarding the daemon's message when the read
  fails. The documented requirements for `make test` now name the `docker` binary, which the fixtures
  that drive the daemon through the CLI rather than its HTTP API have always needed. No product
  behaviour changes.

- **A Docker sandbox test stops racing the teardown it asks for** (#625).
  `TestExportWorksOnAStoppedContainer` stopped its container by killing that container's init from a
  command running inside it, and then asserted on the command's own exit status — a status the teardown
  can pre-empt, which is how the test turned up red in CI. It now stops the container from outside, as
  the attach test always did, through one helper the two of them share; that helper bounds the wait, so
  a daemon which never answers fails a named test rather than the whole package. Test-only.

- **The Claude review workflow skips Dependabot's PRs instead of failing them** (#608).
  `claude-code-review.yml`'s job now carries `if: github.actor != 'dependabot[bot]'`.
  `claude-code-action` refuses a bot actor it was not told to allow, so the `claude-review`
  check went red on #603 and #604 before reading a line of either diff, and allow-listing would
  not have helped: a run Dependabot triggers sees only Dependabot secrets, so the review's OAuth
  token is empty there. The check is now skipped on Dependabot's PRs; the job still runs for a
  human actor, and ci.yml runs on Dependabot PRs as before. Branch protection did not require
  the check when those two merged, so nothing was blocked; the red X was noise.

- **`packages` under `limited` networking without `allow_package_managers` is refused at create, as the reference refuses it** — the platform parsed the two blocks independently and accepted the pair, so the packages were installed against a gate that would never let them out instead of being refused at the request (#576). `POST /v1/environments` and its update now answer 400 `invalid_request_error`, `packages require networking.allow_package_managers to be true under limited networking`. The check runs on the merged config, so an update that adds packages to a `limited` environment, one that switches a packaged environment to `limited`, and any config patch on a row stored before the rule are all refused alike, until the flag is set or the lists are cleared. A `packages` entry that is empty or begins with `-` is refused too, naming its manager: `--index-url=…` is an option every manager would honor, and no quoting turns it back into a package name. A single manager's list is capped at 16 KiB at create; and because `go` expands to one invocation per entry — and a list stored before that cap existed never passed it — the executor also refuses, at install, a manager whose *assembled* command would exceed Linux's ~128 KiB single-`execve`-argument ceiling, recording a terminal `invalid` error rather than faulting into a reclaim loop.

- **A skill version pinned by id is no longer silently replaced by the newest one** — The brain, the executor and the BYOC worker each decided "already concrete versus resolve the alias" with the same test: all digits meant a concrete version, anything else meant `latest`. Under the GA shape a client pins a version by its `skver_…` id, which is not digits — so a pinned version resolved to whatever the newest version happened to be, and the wrong archive was materialized into the sandbox. That was a wrong answer rather than a refusal, and it could not be seen from the outside. All three now resolve explicitly: the literal `latest` through the skill's latest version, an id to that version, digits verbatim. A reference that resolves to nothing — an unknown id, or one belonging to another skill — is now a logged miss and a not-found skip, the same late-bound tolerance a dangling skill reference has always had, instead of quietly serving the newest version (#566).

- **Session deliverables now reach the Files API without an outcome** — Files the agent wrote under `/mnt/session/outputs/` were harvested only when a session had an active outcome, so `GET /v1/files?scope_id=<session>` stayed empty forever for any session that never sent `user.define_outcome` — the reference documents no such requirement (#263). Five brain settlement folds now schedule the same harvest: a tool-less `end_turn`, a retries-exhausted failure, a confirmation gate, a delegated turn's park, cut chain or gated ending, and a run that exhausted its delegation budget — not literally every idle, since the two folds that end an outcome cycle already published the tree and the two idle folds the API makes (a `user.interrupt`, a session's last-thread archive) stay out of scope, tracked by #586. Nothing about the snapshot itself changed — the same walk, caps, path rules and whole-snapshot replace — and an idle harvest never spins up a sandbox for a session that ran no tools: it reuses a live one or does nothing. The grading path is unchanged: a grading cycle's harvest still chains the grading turn, and an interrupt landing mid-grading-harvest still discards. `self_hosted` sessions still harvest on neither trigger; the platform cannot reach a BYOC sandbox. Design and the readings the reference docs leave open: [docs/plan/38_idle-outputs-harvest.md](./docs/plan/38_idle-outputs-harvest.md).

- **A tool name the model was not offered no longer hangs the session forever** — calling a name absent from the turn's tool set (a real toolset tool the agent disabled, an MCP tool whose listing failed, or a plain hallucination) used to commit as `agent.custom_tool_use` and wait for a `user.custom_tool_result` no client had declared the tool to post, leaving the thread `running`, the session `running`, and any queued `user.define_outcome` never evaluated — silently, with no `session.error` and no stop reason. It now settles the same way a delegation call does: an ordinary `agent.tool_use` stamped `evaluated_permission: deny`, answered in the same commit by an `is_error` `agent.tool_result` naming the tool unknown, and the turn chains to the model's next request once nothing else it called is still outstanding (a gated or `always_ask` call sharing the turn is still waited on as usual), so the model can self-correct — bounded by the same 25-turn per-thread cap a delegation chain is, and never spending the session-wide delegation budget unless the run held a genuine delegation call too. Reproduced with a third-party model (MiniMax-M3) behind the anthropic-protocol provider calling a disabled `edit` tool; not reproduced with Claude models. Distinct from #375, still open, which covers a *declared* custom tool's own missing idle/`requires_action` gate. (#567)

- **`config.packages` accepts a `type` key, and every response echoes `"type":"packages"` back** — creating (`POST /v1/environments`) or updating (`POST /v1/environments/{id}`) one 400'd `packages.type must be a list of packages` on a `packages` object carrying `{"type":"packages", ...}`, the shape two of Anthropic's own cwc-workshops (`research-desk`, `production-ready-agent`) send at their first environment create ([#382](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/382)). `packages.type` is now accepted absent, or exactly `"packages"` when present — any other value, including `""` and `null`, 400s. The key is never persisted; every cloud environment's create/get/list/update/archive response now carries it regardless. Whether the reference always renders it is unconfirmed — recorded as an inference in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md). This was the request/response half of #382 only; the runtime half — the sandbox actually installing the listed packages — landed in this same release with #353.

- **The `internal/api` listing tests stop resting on millisecond gaps in the wall clock** (#561).
  Thirteen of them read a position or a `created_at` boundary out of a listing whose rows were
  written 1.5 to 6 ms apart — less than the 20 ms backwards step of the database clock recorded in
  #411. Those orders are the wire contract the tests exist to pin, so they are kept and assigned
  instead: one shared helper stamps the named rows a second apart, from a single `now()`, in one
  statement. Where a test still needs a row written afterwards to come back newest, that now turns
  on a second rather than on the milliseconds a request leaves. The memory-version listing also
  gains the `id`-tiebreak case its quoted contract always promised and nothing exercised. Test-only.

- **`DELETE /v1/environments/{id}` refuses a self-hosted environment whose queue still holds work, unless `force=true`** — the reference's own refusal, recorded against its live endpoint on 2026-09-02 ([#546](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/546)). A self-hosted environment holding a `tool_exec` item that has not yet stopped now answers `409 invalid_request_error` in the reference's sentence — "Cannot delete self-hosted environment with work in the queue. Either archive the environment first to allow the queue to drain, or use force=true to delete immediately." — where the delete used to be refused for having sessions, which sent an operator to delete those, cascading the queued work away. Archiving the environment still drains the queue as the message describes. `?force=true` lifts the queue refusal and only that: the sessions' own refusal stands, because a session is the event log, so a forced delete answers 400 here where the reference answers 200. Cloud environments and drained queues are unaffected. That divergence, and the readings the recording leaves open, are in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md).

- **The memsync shell-contract tests stop reddening `make verify` on macOS** (#554). Both
  handed `mountPrelude` a `t.TempDir()`, which macOS puts under `/var` — a symlink to
  `/private/var` — so the guard refused the mount and the tests failed before reaching the
  contract they pin. The fixtures now resolve to their physical path, while the two that
  deliberately build a symlink still pass it unresolved to assert the refusal. Their
  prerequisite check was too shallow to have protected them anyway: it probed one of the six
  GNU extensions these commands use, so a host with BSD `rmdir` ran the suite and returned a
  wrong answer rather than skipping. All six are probed now — each test only the ones its own
  command uses, so macOS keeps the hash tree it can run and skips only the removal — and on
  Linux a missing one fails instead of skipping. Test-only.

- **The memory-version tests stop resting a wire contract on the wall clock** (#551).
  `TestMemoryVersionsPerOperation` asserts a four-row newest-first listing whose rows four
  HTTP requests wrote a few milliseconds apart. That order is the contract the test exists to
  pin, so it is kept and the clock taken out from under it instead: one statement stamps the
  four rows a second apart, three of them named by the id their own responses returned and
  the fourth, which the delete response does not name, by exclusion.
  `TestMemoryVersionRedact` took the superseded row by position and now names it by id, which
  needs no order at all. Test-only.

- **Two queue tests no longer race the 50 ms window they assert inside** (#537).
  `TestExpiredLeaseIsReclaimed` claimed a work item for 50 ms and then asserted a
  second claim found nothing yet; `TestPollReservesWithoutTransition` did the same
  with a 50 ms poll reservation. Each assertion holds only while its window is
  open, so a scheduling gap wider than 50 ms — one Postgres round trip on a
  contended runner — had the queue reclaim correctly and the test fail, reddening
  the `coverage` job on a commit carrying no Go at all. Both windows now widen to a
  minute, and the reclaim they go on to exercise is reached by backdating
  `lease_expires_at` in SQL rather than by sleeping the lease out, so no Go clock
  decides either transition. Test-only; no production code changed.

- **The memory-store sync suites stop asserting a version order the schema cannot
  guarantee** (#525). `versionsOf`, in both the executor's and the BYOC worker's memory
  tests, compared a path's version list in `ORDER BY created_at, id` order — but
  `memory_versions` carries no write-order key: `created_at` defaults to `now()`, which
  Postgres freezes at BEGIN, and the tiebreak behind it is a random `memver_` id. Both
  helpers now compare a sorted list, which still catches a wrong, missing, extra or
  duplicated version. Test-only; no production code changed.

- **Three wire behaviors corrected against a real managed-agents recording** — A recording of Anthropic's endpoint (#78) settled several assumptions this platform had decided the other way, and three of them were wrong on the wire. A worker key used against another environment's work route now answers **403 `permission_error`** ("Token not authorized for this environment") instead of 401 — the reference answers an unknown environment id the same way, so it is a scope failure, not an authentication one, and `permission_error` is no longer treated as reachable only on the human-auth lane. `POST /v1/agents/{id}` now answers **200** to `{"version": null}` and to a field-less `{}`, leaving `version`, `updated_at` and the `/versions` snapshot list untouched, where it used to reject the null with 400 and bump the version for an empty body; the precondition a version-only body carries is still honoured, so a stale one is still 409. And an abandoned tool call left by `user.interrupt` now carries the reference's own sentence, "Tool execution was interrupted before completion. Please retry." The same recording settled three registry inferences — the heartbeat precondition, a repeated work ack, and the optimistic-conflict envelope — the first two staying divergences by choice, and turned up four mismatches that are bugs, not divergences, owned by #539, #542, #543 and #544. Confirmations and remaining gaps are recorded in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md).

- **journal-multiturn's file check forgives blank lines** — the 2026-09-01
  manual eval run red the trial's retry on `printf '\nentry-two\n' >>` against
  a file already ending in a newline: an empty line between entries, with every
  platform signal (first line unchanged, second below it) intact. The trial now
  grades with `FileLinesIgnoringBlanks`, a variant scoped to files the model
  assembles by appending across turns; `FileLines` itself stays exact, so no
  single-write trial loosens. `perm-deny`'s untouched-file assertion moves the
  other way, to the byte-exact `FileEquals` its comment always claimed —
  "unchanged" is not a place for forgiveness. (#533)

- **`DELETE /v1/environments/{id}` now names every blocker and the remedy that fits it** — plan 37 slice 1's last piece ([docs/plan/37_scheduled-deployments.md](./docs/plan/37_scheduled-deployments.md), [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)). The handler mapped every foreign-key violation to *"environment … still has sessions; delete them first"*, which was harmless while sessions were the only table that could block the delete. Deployments now can, so an operator whose environment had no sessions at all was told to delete some. The refusal is built from what actually references the environment: it lists the blocking deployments, up to five with a count of the rest, counts the sessions beside them, and names what will clear them. A live deployment can be pointed at another environment; an archived one refuses every update and so can never be moved, which is the case where the environment can no longer be deleted at all and archiving it is what remains — advice that stops once the environment is archived.

- **The `reads-file` eval grader recognizes reads its old rule refused** — it
  accepted only a `read` of the exact mount path or a `bash` command carrying
  the whole path as a substring, so reads the toolset admits — a
  workdir-relative `read` path, a `grep` rooted over the file, a `cd` and
  `cat` split across two bash calls by the persistent shell — reddened the
  trial as "the agent never read the mounted file"; the 2026-08-19 nightly's
  `repo-answer` failed exactly that way with the correct passphrase in its
  final answer. The grader now mirrors the toolset's path-resolution rule for
  `read`, counts a `grep` whose search root covers the file and whose result
  carried matches (`glob` still does not count — names, no bytes), matches
  `bash` on the file's basename — deliberately loose, cwd-blind evidence, with
  each trial's answer grader load-bearing — and counts only calls whose result
  succeeded, so a failed `cat` no longer passes for a read. (#526)

- **The Workload Identity Federation commands in the GCP deployment notes could not run as
  written** (#508). `deploy/gcp/README.md` took `--project` out of `$WIF_PROVIDER`, which
  carries the project **number**, on the stated grounds that "`gcloud` accepts either". It
  does not: every `gcloud iam workload-identity-pools` verb — `describe`, `list`, `create`,
  `create-oidc` and `update-oidc`, pools and providers alike — refuses a project number in
  `--project` before it makes any API call, and asks for the ID instead. That broke both
  commands the file documents, including the `update-oidc` that sets the attribute condition
  CD's security actually rests on. Both now pass `$WIF_PROVIDER` **whole** as the positional
  resource name, which needs no `--project`, `--location` or `--workload-identity-pool` at
  all — the number is perfectly good where it already sits, inside that name. What the
  rewrite removes is failure modes rather than ambiguity about which provider is meant: a
  stray `--project`, an unset `core/project`, and four lines of prefix-stripping to get wrong.

- **The CD notifier's retry no longer folds a failed attempt's output into the answer**
  (#507). `deploy-alert.yml` wraps its four GitHub API reads in a three-attempt `retry`, and
  every caller captures what that helper prints — but it ran each attempt with stdout already
  wired to the capture, so bytes emitted before a failure stayed there and the caller parsed
  them joined to the attempt that worked: a truncated body became unparseable JSON, and the
  open-issue lookup returned `123123` where it meant "nothing open". Each attempt is now
  buffered and only the successful one printed, matching the copy `staging-parked.yml` already
  carried. The two helpers stay deliberately separate — two notifiers retrying differently
  harms nobody, unlike the parked-cluster rule they do share — so `make retry-test` lifts each
  one out of its workflow and runs it against a flaky command, and neither can drift back
  alone.

- **Six registry entries stopped naming a closed issue as their live tracker** (#502) —
  archiving plan 36 closed
  [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52), but six
  [docs/DIVERGENCES.md](docs/DIVERGENCES.md) entries still cited it as *live*, so each
  named an issue that could no longer settle it — the nightly pointer guard's
  `live-tracker-open` finding, and a red `registry` check on any PR touching the registry,
  the guard or the Makefile. All six are now provenance (`#52 (delivered)`): the work
  landed, so no issue is owed. The `self_hosted` resource-superset entry additionally
  names the still-open
  [#322](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/322) for the
  repository arm it does not settle. No divergence text changed.

- **`TestEnqueueNotifiesWorkChannelOnCommit` stops racing the broker's coverage-start
  wake** (#486). The listener wakes every subscriber once when LISTEN becomes active
  (`setReady` then `wakeAll`), and `Ready` can return between those two steps — so the
  test's non-blocking drain of that one-time wake could miss it, leaving it to arrive
  inside the 150 ms window where the test asserts that no wake precedes the enqueue's
  commit, reddening `internal/queue` under gate load. The drain now blocks for the
  coverage-start wake it is guaranteed to receive, so only the enqueue's own commit
  NOTIFY can follow it. No production code changed.

- **Two lease-keeper tests no longer abandon a healthy turn on a loaded fixture
  Postgres** (#483). The keeper bounds each `Extend` by what the lease has left, so the
  brain's `TestLongTimeToFirstTokenKeepsLease` (250 ms lease) and the executor's
  `TestLeaseRenewedDuringSlowProvision` (300 ms) left a sub-200 ms budget that one slow
  UPDATE on a contended fixture could overrun — reddening the `coverage` job on commits
  carrying no relevant Go. Both leases are now 1500 ms, matching the keeper-budget tests
  whose `Extend` tolerates more than a second of contention, with the tests' timing
  scaled to still exercise the keeper's renewal. Production lease TTLs (2 min brain,
  15 min executor) and the keeper's deliberate abandon-on-timeout are unchanged; this is
  a test-timing fix.

- **The chart's docs no longer promise a Cloud SQL proxy backstop that does not exist** (#492).
  Three places — the chart README, [docs/deploy-gcp.md](./docs/deploy-gcp.md) and the
  `instanceConnectionName` guard's own comment — said a well-formed but wrong instance
  connection name is caught because "the proxy rejects it at startup". It is not: the proxy
  dials nothing unless asked to, and none of its three health endpoints dials either, so it
  reports itself up on an instance nobody can reach. The render guard is a shape filter, not
  a backstop, and the documents now say which layer actually catches what.

- **A request body that is not valid UTF-8 is refused, on every route that decodes a JSON object** — Go's JSON decoder replaces an invalid byte with U+FFFD rather than rejecting it, so a body carrying one was accepted and stored altered; a memory's `content`, whose documented contract is valid UTF-8, was the surface where that showed (plan 36 slice 2, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)). Every body the shared object decoder reads now answers `400 request body must be valid UTF-8` before it is parsed — JSON is UTF-8 by definition, so nothing legal is refused (the one body read outside it, the work-stop request's single bool, stores nothing). A lone-surrogate escape (`\ud800`) is still decoded to U+FFFD, which is the decoder's documented behavior for an escape, not a byte the client sent.

- **Metadata keys must be at least one character, on every resource** — the documented metadata contract says keys are 1–64 characters, and only the upper bound was checked: `{"": "v"}` was accepted on agents, environments, sessions, vaults, vault credentials and work items, and now on memory stores. A create carrying an empty key, or a patch upserting one, answers `400 metadata keys cannot be empty` (plan 36 slice 1, [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52)). The bound applies to what a request sends, not to what a row already holds: a bag that acquired an empty key before this release stays patchable, and `{"": null}` deletes the key.

- **v0.2.0's published release body links into the repository from anywhere** — its notes were
  rendered before #423 and left 59 repo-root-relative targets across 39 paths relative. The body
  has been re-rendered from the tagged section by today's tool and replaced in place: identical
  bar those targets, clamped at the same group boundary, flags and assets untouched. A re-run of
  the tag could not have done it — every step runs the tooling as of the tagged commit — and
  would undo it, since a re-run reverts what was edited in place afterwards. The rationale #423
  recorded was itself wrong: it said such a target 404s off the release page, where github.com's
  renderer in fact resolves it against the repository at the tag, so the page always read
  correctly. What carried it unresolved is the raw body — the REST API, `gh release view`,
  mirrors — and the root-relative `body_html`. [docs/RELEASING.md](./docs/RELEASING.md),
  [changelog.d/README.md](./changelog.d/README.md), #423's own entry and the tool's own comments
  now say that. (#425)

- **The session-id prefix entry is an inference, not a mirror** — Its whole text in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) said the alternate `session_` form is accepted
  "mirroring the reference's dual form", which nothing ever observed: the claim dates to this
  repo's first commit and its citations resolve to this repo's own wire rule. Every concrete
  session id in the pinned SDK, the `ant` CLI and the public sessions pages is `sesn_`; the only
  reference-side `session_` is the work-data schema's description of `data.id`, which the SDK
  worker feeds straight back into session paths. So the entry moves to INFERRED under #78 rather
  than to the architecture notes, keeps the lenient parse, and says what a recording would
  settle. (#463)
- **Four registry entries name the recording that would settle them** — The management-key
  `expires_at` entry records a 1–128-character `name` bound it calls unobserved and carried no
  tracker at all. Its `can_manage` twin (falsy case unobserved), the mixed-permission entry
  (reference behavior unspecified) and the web-tools entry (`citations.enabled`) had the same
  gap, the last of them behind a closed issue. Each now points at #78 with a parenthetical
  naming what it leaves open — a gap `registrycheck` cannot see, since it demands a pointer
  only under the INFERRED heading. (#465)

- **Continuous delivery to the GCP staging environment deploys again** — the chart renders a
  namespaced `Role` and `RoleBinding` for the executor, and writing them is guarded twice over
  by systems that do not see each other. Cloud IAM refuses first: the deploy identity holds
  `roles/container.developer`, which carries `get` and `list` on RBAC resources and no write
  verb. Kubernetes refuses second, and its escalation guard resolves what a principal holds
  from RBAC objects alone — GKE's IAM permissions arrive through an authorization webhook the
  rule resolver cannot see, so closing the first gate only moved the error. Helm patches an
  object only where what it renders differs from what it recorded, so both gates stayed
  invisible across the 45 runs that reached the install step and shut the moment v0.3.0's chart
  bump restamped `helm.sh/chart` on every rendered object: for seven days afterwards every one
  of the 23 pushes to `main` failed at `helm upgrade` and rolled back. A `mapCdRbacWriter`
  custom role answers the first gate and an in-cluster basis Role the second — neither granting
  `escalate` or `bind`. [`deploy/gcp/README.md`](./deploy/gcp/README.md) records both, and why
  `roles/container.admin` was refused. (#469)

- **The registry says when a work item is enqueued** — There the session itself is the work
  item, queued when it is created or when a long-dormant one receives a message, and the worker
  spawns an execution context per item and runs that session's tool calls. Here an item is
  created only by a commit that leaves a runnable sandbox tool call standing, so a session that
  never runs one never appears in `…/work` at all, and one that does appears first at its first
  confirmed sandbox call rather than at create. The payload is unchanged and an item's duration is its
  holder's business on both sides; only the trigger differs, argued from the brain owning the
  session while a worker holds neither a database handle nor a model credential. Of the five
  kinds sharing `work_items` only `tool_exec` reaches the wire — and the real `ant beta:worker`
  has settled this platform's items unmodified in every acceptance run that drove it. (#457)
- **`workAPIScope`'s comment names the hazards instead of counting the kinds** — It said two
  other row kinds share `work_items`; five do. The predicate was always right, so nothing was
  reachable that should not be, but a reader checking it against the comment would have read
  the scope as narrower than the table warrants. (#462)

- **Two converged registry entries say which divergence they are the record of** — The
  skills-`latest` entry and the management-key `expires_at` entry in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) both read as mirrors of the reference, which made
  them look mis-sectioned under the test #450 wrote into the CONFIRMED heading. Both are in fact
  converged, and each now opens by saying so. The skills entry's divergence was never the version
  normalization, which always matched: it was the deferral behind the field — no `/v1/skills` API,
  no storage, no materialization, no injection — and the sentence naming it had been dropped when
  skills slice 5 closed it, leaving "The deferral is now closed" with nothing to refer back to and
  a title advertising the match. The `expires_at` entry's was its refusal of an instant already
  past, which #389 measured at `200` on the reference and lifted. (#458)

- **A second compose stack no longer joins the first** — `deploy/compose/docker-compose.yml`
  pinned its default network's name so the executor's `SANDBOX_DOCKER_GATE_NETWORK` could name
  it verbatim, and so `docker compose -p other up` did not isolate: the second stack landed on
  the running one's network, where `postgres`, `openbao` and `minio` resolved to whichever
  container answered. Not theoretical — a branch stack's brain migrated a running stack's
  database, because every binary migrates at startup. The gate network and the two `:local`
  image tags now derive from the compose project name, which Compose resolves before it
  interpolates, so the default project renders unchanged and a stack already up under it is
  adopted rather than recreated, while `-p <name>` gives a second stack its own network,
  containers, volumes and images. CI renders the file under two project names to keep it that
  way. Host ports are what a project name still does not separate, and the compose README says
  so. Separately, the migration that did the damage is no longer silent: a process with
  migrations to apply now names the database, the address that actually answered and the host
  it was configured with — the two disagreeing is the tell — and each version, before the first
  statement runs. Only that moment is announced; a process that finds nothing to apply says
  nothing, as before. (#438)

- **Every shared pointer in the registry says what its own entry leaves open** — 33 pointers in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) named a tracker that several entries share — #78,
  the recording tracker, above all — and so told a reader nothing about the entry citing them.
  #453 had given 51 of their siblings a parenthetical and left these on no principle but the
  accident of which trackers happened to close; each now carries one, written from its own entry.
  Five turned out to be worse than bare. #56 was "Console SSO and RBAC" when they were written
  and is now "Multi-tenant activation (post-v1)", which settles none of them: three record
  shipped divergences with nothing outstanding at all and become provenance, and two are waiting
  on a recording and re-point at #78. An issue whose scope moves out from under a pointer is a
  second kind of rot, and no issue-state check can see it — only writing the parenthetical does.
  (#452)

- **The registry's sections say what belongs in them** — `docs/DIVERGENCES.md`'s CONFIRMED and
  INFERRED headings carried no definition, so entries drifted into a section their own text
  denied. Both now state their test: a converged entry stays under CONFIRMED as the record of
  what once diverged, and an entry no recording could settle is not an inference. Under it the
  dial-address floor moves to CONFIRMED and the agent-toolset default policy — settled by the
  public guide, not a recording — becomes an architecture note; the two converged entries stay,
  and #59 is closed as answered. The `block_ms` entry regains the live pointer its two open
  readings had lost. (#450)
- **The session-status entry says what the public docs settle** — It claimed a session awaiting
  a tool stays `running`, and that `requires_action` serves confirmation gates alone. The
  first half is right and now says so; the second is wrong. The managed-agents docs make a
  custom-tool result a first-class blocker — the session parks at `session.status_idle` with
  `stop_reason.requires_action` — where this platform stays `running`, so a client written to
  the published loop waits forever. That is a bug rather than a divergence, so the entry names
  #375, which owns it, rather than the catch-all recording tracker, and its cadence twin moves
  beside it. A `self_hosted` worker's tool result rests on a field description alone, and
  becomes its own INFERRED entry instead. (#451)

- **`TestKeySetFetchDeadline` stops racing the deadline it exists to test**
  (#381, #422). The test drives the one branch a fake clock cannot reach — a real
  context deadline on a key-set fetch — and then asserted that the fake provider
  had *served* a second request. But a fetch its 50 ms deadline kills before the
  server accepts it never reaches that handler, and that is the deadline working
  rather than failing: the assertion required a request designed to lose a race
  to win it first, so under CI load it reddened the `coverage` job on diffs
  containing no Go at all. The fetches are now watched at the client, where the
  outcome is not a race — the attempt is recorded whichever phase the deadline
  interrupts, and `context.DeadlineExceeded` is what the test pins. No production
  code changed.

- **A failed OpenBao init no longer leaves an init.json that lies about it** — both bundled
  init scripts, `deploy/compose/openbao-init.sh` and the chart's sidecar, created and
  truncated `init.json` before the `bao operator init` that fills it, so a failed init left a
  0-byte file behind. The branch that recovers a lost volume tested for a *missing* file,
  walked past the empty one, and the next run died on `no root_token` against a vault that was
  initialized and whose root token had never been captured — in-cluster, on every kubelet
  restart. The output now lands only once init has succeeded, and the recovery guard turns on
  the extracted token rather than on the file holding it, so every unusable `init.json`
  reaches the diagnostic that names the volume. In the case #439 reported the volumes still
  have to be re-created; what changes is that the script says so instead of pointing at a
  file. The chart's remedy line, which named a PVC that cannot be the problem, is corrected
  with it. `make openbao-init-test` now runs both scripts against a fake `bao` in CI, which
  nothing did before. (#439)

- **`docs/DIVERGENCES.md`'s `Tracked:` pointers, and CLAUDE.md's post-v1 list, stop naming
  closed issues** — 65 of the registry's 111 pointers cited an issue that had since closed, so a
  reader following one landed on delivered work and learned nothing about what is still open.
  Five already carried a provenance annotation and were right; the other 60 are repaired, and
  the two failure modes differently. In the CONFIRMED section a closed tracker is provenance and
  now says so. An INFERRED entry whose tracker closed was orphaned, and re-points at an open
  tracker — #78, the recording tracker, in almost every case — with a parenthetical naming what
  that entry still leaves open, and the closed issue demoted to a `landed for #N` clause; the
  one entry no recording can settle went to #450 instead, and two CONFIRMED entries took the
  same re-point because their prose still named a live question their pointer did not. The
  file's Format legend now states the rule in writing, so the next contributor inherits it:
  `Tracked:` names an **open** issue, and a closed one may appear only as provenance.
  CLAUDE.md's enumeration `#50–#57 (+ #77)` — five of the nine already closed, and blind to
  #261, a post-v1 deferral filed outside the range — gives way to the `(post-v1)` title marker
  every such issue carries, and a bounded query for it. (#445)

- **A BYOC worker no longer runs a command nobody will read** — plan 35 decision 9 says an answered call is a cancelled call, in both exec drivers. The platform executor did that from the start; the customer-hosted worker ran every call under the shared work item's context alone, and a thread-scoped interrupt never stops that item. So interrupting a child thread left its `sleep 3600` burning customer compute to the tool timeout, and the result the worker then posted for a call the log had already answered was refused — aborting the whole pass and holding every sibling call behind it for a lease TTL. The worker now asks before starting each call, again every ten seconds while it runs, and once more if the post is refused. Each read walks the session's events newest-first and stops at the call itself, which is exact where a fixed look-back is not: a thread-scoped interrupt answers a whole thread's calls in one append, so the answer to a wide turn's oldest call sits behind every sibling's. The last read is uncapped, because it decides between skipping a call that is done and faulting the pass over it; the two before it are capped, and giving up there only loses an answer, never invents one. A refusal with any other cause still stops the pass — an ask awaiting its human, or an archived session, which wears the same status and is refused before any validation runs. ([#441](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/441))

- **A coordinator's all-settlement turns can no longer loop forever** — a delegated turn whose calls were all answered inside the settlement has nothing left to wait for, so it hands its own turn straight back; a model that keeps making such turns (a repeated `list_agents`, a `wait_for_agents` that keeps timing out) drove that with no delay and no bound. The thread never idled, so the session stayed `running` — archive and delete refused it — and because the queue serves the oldest turn first and a re-queue does not move it, the loop starved every other session's turn on a single-brain deployment. Twenty-five consecutive such turns now cut the chain: the thread idles on `end_turn` and its log carries a `session.error` of type `delegation_chain_exhausted_error` saying why, and a message — or a report from a child it spawned — resumes the work. Anything the thread is fed, and anything it schedules for another agent, resets the count, so a coordinator working through a roster is unaffected; a cut child tells its coordinator, so a parked one is never left waiting on it; two agents messaging each other are a separate loop, tracked in #447. The thread wake paths also gained the unanswered-tool-call guard the API's message trigger already had. (#442)

- **The multiagent advisor entry is a settled refusal, not pending work** — a `{type:"advisor", model}` entry in an agent's `multiagent` roster has been refused with `400 entry type must be "agent" or "self"` since the roster shipped, and [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) now records that refusal as the answer rather than a surface awaiting a build. Two statements there are corrected with it: the roster stopped being "inert at runtime" when session threads began running its members, and it is the advisor *variant* of the reference's three agent unions that never renders — the unions themselves do. (#431)

- **AGENTS.md stops forbidding the release ritual's own last step, and the fragment sweep
  stops depending on a fresh local `main`** — compressing AGENTS.md's docs-move-with-code
  bullet dropped the carve-out that lets `make changelog-archive` touch CHANGELOG.md, so for
  all of v0.3.0 the mirror external agents read called that step a rule violation; CLAUDE.md
  had kept it. Restored, with three corrections to [docs/RELEASING.md](./docs/RELEASING.md)
  beside it: step 6's sweep enumerates consumed fragments against `origin/main` rather than
  `main`, because a stale local ref moves the merge base back and sweeps earlier cuts' too;
  its exclusion matches the archives' version-shaped names instead of exempting all of
  docs/changelog/, so anything beside them not named like a version is swept; and step 9 returns
  Active work to **None** only when nothing else is in flight. Step 8 now also says what
  becomes of a change that merges after the release PR and is then reached by an advanced
  tag: it ships in that release while its fragment folds into the next one's section, so
  the fragment names its version. (#426)

- **Release notes link into the repository at the tag, for readers of the raw body** —
  `make changelog-notes` copied the changelog section verbatim, leaving every repo-root-relative
  target relative in the published body. github.com's release renderer resolves those against
  the repository at the tag, so the page reads correctly — but the raw body is what the REST
  API, `gh release view` and mirrors serve, and its `body_html` is root-relative, so both
  break away from github.com. The v0.3.0 body carries 30 such links across 12 documents and
  is the first rendered without clamping; it is also the first tagged past this fix, so it
  publishes them absolute and never exhibited the defect. The archive path had re-based links
  for their new directory since plan 28, and the truncation trailer was already absolute for
  precisely this reason — the notes path alone had no equivalent. Both relative forms
  [changelog.d/README.md](./changelog.d/README.md) permits are now rewritten to
  `…/blob/vX.Y.Z/…` at the tag, link-reference definitions included, anchors preserved and
  fenced examples left as written, while a form with no mapping — a `..` segment among them —
  fails the notes rather than publishing a dead link. That failure lands *after* the tag is
  pushed, so link forms are worth a glance while the fragment is still in review. (#423)

### Security

- **A package-manager credential no longer rides in the install command** — a `config.packages` entry may carry a URL credential (`pip: ["git+https://user:token@host/repo"]`, and the same nested in pip's PEP 508 or npm's alias syntax), and the assembled install command is one execve argument: on Kubernetes it is also the exec subresource's `command` parameters, which the apiserver's audit log records wherever auditing covers `pods/exec` — an exposure to cluster operators, outside the session's trust domain and outliving it. The credential is now lifted out and written into the file that manager's fetcher actually reads — a netrc for pip, npm and go, plus npm's per-host pair for npm's own fetcher — in a scratch `HOME` removed both by the install's own trap and by the executor after it returns. The published `packages_digest` is taken over that stripped form too, so a digest an environment key can read is no longer an offline oracle for a weak credential; the in-sandbox sentinel still compares the list as written, so rotating a credential re-installs and its failure is its own event. Unchanged: a read from **inside** the same sandbox, which no file placement can close while the install and the agent share a root user, and six entry shapes — named in `docs/self-hosted-security.md` — which keep both the argv and the digest exposure. Made worse in exactly one: npm 6, which authenticates a non-registry fetch from no `.npmrc` key at all.

- **The egress gate refuses an authority that names no host** — `CONNECT :443` and `http://:80/x` reach the gate with an empty host, and under `unrestricted` networking the policy admitted them without examining any host at all. `:443` is Go's "local system" dial form, so the gate connected to loopback in the network namespace it shares with the sandbox; on the plain-HTTP path a credential whose own arm is `unrestricted` ignores its `Hosts` list, so its secret would have been substituted into a request delivered to that listener. The policy itself now refuses it, before any host set is consulted, so every caller inherits the check rather than each handler carrying its own. A `limited` environment was never affected — an empty host matches no allow-list entry. Found while reviewing the host-canonicalization fix and fixed with the address floor it belongs beside.

- **An environment's `networking.allowed_hosts` is validated, and the gate dials the name it admitted (#609)** — The list was stored as arbitrary strings, and an entry matched exactly the string it was written as and nothing else — which no request host can be, since the gate splits the port off before it asks and net/http refuses the one authority that would survive that split with a colon in it. A typo therefore read as the operator's fence when it was really a hole in it. Create and update now hold each entry to the grammar a vault credential's list has always used, and answer 400 otherwise. **Two shapes that did work are now refused at write time** — an IPv6 literal such as `::1`, and an underscore label such as `_acme.example.com` — because the published grammar admits neither. Rows stored before this are not migrated and not re-validated: only what a patch newly supplies is checked, and an entry whose value the row already holds is carried through, so reading a config back and posting it unchanged is not a 400. One class does stop matching, because both sides of the comparison now canonicalize — an entry whose canonical form is not a hostname, such as one led by an ideographic full stop. Separately, the gate's CONNECT handler dials the canonical name rather than the authority the sandbox wrote — admission and connection could otherwise name two different hosts — while a scoped address keeps its zone identifier exactly, since that names a local interface.

- **The MCP bearer no longer follows a host that only Unicode folding calls the same (#609, first half)** — `internal/mcp` decided whether a vault-resolved token rode a request by comparing the request's host with the endpoint's using `strings.EqualFold`, which folds by Unicode. The Greek sigmas share a fold orbit that IDNA keeps apart — `σ.example` and `ς.example` punycode to different names — so two domains counted as one origin and the bearer went to the second. Hostname case is now folded in ASCII, as `internal/egress` and `internal/vaultresolve` already did. The cost is that every other orbit `EqualFold` merged is now two origins even where IDNA collapses it — `Ä.example` against `ä.example` — so the token is withheld; a withheld bearer costs a 401 where a shared one costs the secret. Both the leak and the cost need an origin the caller did not spell, which this repository's redirect-refusing MCP clients do not produce. The issue's other half — that an environment's `networking.allowed_hosts` is never validated — stays open.

- **An IDN alias no longer passes as the ASCII host it folds to (#606)** — `egress.NormalizeHost` folded case by Unicode, which maps several non-ASCII letters onto ASCII ones: `gİthub.com` (U+0130) normalized to `github.com`, so the gate admitted the request — and the substitution engine, which chooses a credential with that same matcher, would then hand it a secret scoped to the ASCII host, wherever a vault credential naming that host was attached. Go's HTTP stack meanwhile resolves the request through IDNA to `xn--github-qyd.com`, a name anyone may register. Admission and substitution are separate controls with separate preconditions; sharing one matcher is what let a single alias defeat both. Case folding a hostname is an ASCII operation, and it is now done that way, matching `internal/vaultresolve`'s `lowerHost`. What narrows is narrow and errs toward refusal: `ValidateHostEntry` already restricts a credential's `allowed_hosts`, the forwarded MCP endpoints, `WEBTOOL_ALLOWED_DOMAINS` and the curated registry set to ASCII, but an environment's own `networking.allowed_hosts` is unvalidated, so a U-label written there stops matching a differently-cased spelling of itself — refused, never sent elsewhere ([#609](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/609)). The defect predated the thirty-host registry set, but that set is what made `github.com` reachable without an operator listing it, so the two land together.

- **`allow_package_managers` reaches source forges, and on any port (#594)** — Now that the set is sized, what the flag grants is worth stating plainly: `github.com`, `api.github.com`, `gitlab.com`, `bitbucket.org`, the GitHub content hosts, and the Docker and GitHub container registries. A `limited` sandbox with the flag set reaches every one of those hosts — and, because the class is matched by host rather than by endpoint, can open a tunnel to **any port** on any of the thirty, `github.com:22` included. That is reach neither the flag's name nor the reference's own wording ("public package registries (such as PyPI and npm)") suggests. The container half is narrower than it looks: the registries answer manifests, but the hosts they redirect layer downloads to are refused, and no probe ran a real image pull. This is what the reference admits rather than a choice made here, but an operator who set the flag so `pip` could reach its index is granting considerably more than that, and should re-read [docs/self-hosted-security.md](./docs/self-hosted-security.md) §5.

- **The egress gate resolves a curated registry name absolutely (#596)** — A `limited` sandbox's dial to `pypi.org` or `files.pythonhosted.org` now reaches the resolver with a trailing dot, so the `search` list is skipped and the public name is the only one tried. A gate sidecar inheriting a Kubernetes `ndots:5` resolver could otherwise be answered from an internal zone, turning a curated grant into reach the list never gave. Only this class is rooted, it being the one allow-list the platform itself authors: an agent's `allow_mcp_servers` endpoints and an operator's own `allowed_hosts` resolve exactly as before, and #601 carries what that leaves open. Nothing the origin sees changes — the `Host` header, the CONNECT authority and the TLS server name keep the spelling the sandbox sent.

- **The last three `actions/checkout` call sites drop the job's git credential** (#558).
  `release.yml`, `claude.yml` and `claude-code-review.yml` never set `persist-credentials: false`,
  so `GITHUB_TOKEN` stayed wired into the job's git config — in the workflow that publishes the
  images, the chart and the Release, and in the two holding `id-token: write` and an OAuth token.
  It was not merely sitting there unused: `http.<server>.extraheader` REPLACES the header git
  derives from a remote's URL, so in the two Claude jobs it outranked the App token
  `claude-code-action` mints for itself, and was the credential every git request actually
  carried — the action's own attempt to remove it looks for a `.git/config` key that
  `actions/checkout` v6+ no longer writes. Dropping it is corrective rather than tidy, and for
  `claude.yml` it changes who git speaks as. It also corrects a released claim: v0.2.0's
  supply-chain entry said every checkout in the repository had gained the flag, and `git show
  v0.2.0:.github/workflows/release.yml` shows that one never did. That record is frozen, so the
  correction is written here and in #558 instead.

- **The two Claude workflows no longer run actions from a mutable tag** (#518). Every `uses:` in
  `.github/workflows/` is pinned to a commit SHA — the rule `.github/dependabot.yml` opens by
  stating — except in `claude.yml` and `claude-code-review.yml`, which arrived generated by the
  Claude Code GitHub App installer (#472) carrying `actions/checkout@v4` and
  `anthropics/claude-code-action@v1`. `@v1` is a pointer, not a pin: upstream force-moves it on
  every release, and it moved onto a new commit while this change was being written — so two jobs
  holding `id-token: write` and an OAuth token ran whatever it last pointed at. Both are now `<sha> # <release>` like the
  other thirty. That also takes checkout from v4 to v7.0.1 in these two files, which is what #518
  asked for and is said here rather than smuggled in: what the steps see is `node20` → `node24`,
  since v7's fork-PR guard fires only on `pull_request_target`/`workflow_run` and `@v4` already
  resolves to the v4.4.0 that ships it.

- **A wrong-environment work-API refusal now reveals that the token is genuine** — Matching the reference's recorded answer on `/work/poll` (#78) means a live environment key aimed at an environment it does not cover returns 403 `permission_error` rather than the 401 `authentication_error` this platform used to return, and a garbage bearer still returns 401. The same answer now covers every work route, from one shared scope check; only the poll route was recorded, and the registry files that generalization as an inference. The two are therefore distinguishable, where before they were not. What is still withheld is the part that matters: a real-but-uncovered environment id and a nonexistent one answer with the identical message, so a token cannot be used to enumerate which environments exist — that indistinguishability is now pinned verbatim by a test rather than left implicit. A caller holding a live key already learns it is genuine from the environment it does cover, so the incremental disclosure is small. Operators who relied on the old, deliberately-opaque 401 should note the change; the reasoning is recorded in [docs/DIVERGENCES.md](./docs/DIVERGENCES.md).

- **The operator's own deployment coordinates are out of the documentation** (#514).
  #355/#356 moved them into GitHub Actions variables by hand, but that sweep stopped at
  `deploy/` and `.github/`: three acceptance records in `docs/HISTORY.md` still carried a
  project id, two project numbers, an Artifact Registry path and a zone, and — found while
  fixing the rest — two **routable** LoadBalancer addresses. A project id survived in a Go
  test fixture, and the staging database's private address in `deploy/gcp/README.md` went
  with them. Each is now the variable name the workflow reads, or a placeholder where no
  variable exists; every acceptance record still makes the same claim about the same run.
  Nothing here was a credential and no rotation is implied — these were coordinates a
  public repository had no reason to publish.

- **The BYOC worker no longer runs an ask-gated tool call before its human answers** — plan 35 slice 4 ([#53](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/53)): a turn that suspends on two `always_ask` calls is released one confirmation at a time, and the `self_hosted` worker's scan read no `evaluated_permission` at all — so the verdict releasing one call could hand it both, running a command nobody had approved. The scan now requires an `allow` `user.tool_confirmation` naming any call whose evaluated permission is `ask` — a `deny` runs on no confirmation at all — the narrowing the platform's own drivers took in slice 3. A call carrying no evaluated permission still reads as allowed, so nothing already on a log changes. It binds every `self_hosted` session, single-agent and coordinator alike; `cloud` sessions have no BYOC worker.

## [0.3.0] - 2026-08-16

Added · Changed · Fixed · Security — the full section lives in [docs/changelog/0.3.0.md](./docs/changelog/0.3.0.md).

## [0.2.0] - 2026-08-07

Added · Changed · Fixed · Security — the full section lives in [docs/changelog/0.2.0.md](./docs/changelog/0.2.0.md).

## [0.1.0] - 2026-07-17

Added · Changed · Fixed — the full section lives in [docs/changelog/0.1.0.md](./docs/changelog/0.1.0.md).

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
