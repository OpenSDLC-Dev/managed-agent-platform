---
status: approved
issue: "#54"
---

# Skills distribution + execution

The plan for [#54](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/54): lift
the reserved skills seam (`skill_` prefix, the `parseSkills` normalization that accepts
`skills[]` but never resolves or executes it) into the full feature — a wire-compatible
`/v1/skills` registry backed by S3-compatible object storage, runtime resolution of an
agent's `skills[]`, materialization of skill files into both executor- and BYOC-worker-run
sandboxes, and Level-1 metadata injection by the brain. Five PR slices.

## Ground truth (verified 2026-07-21)

Wire schema resolved per CLAUDE.md's order: public docs → pinned `anthropic-sdk-go`
v1.58.0 (the checkout is byte-identical to the pin for the skills files) → `ant` CLI
source → the Stainless OpenAPI spec the SDK is generated from (`.stats.yml`
`openapi_spec_url`), used only where Go doc comments drop the spec's examples. No live
recording was possible (no Anthropic credentials); everything only a recording can settle
is in "Inferences to record" below and lands in docs/DIVERGENCES.md with its slice.

### Endpoints

All nine paths bake `?beta=true` into the SDK's path string and send
`anthropic-beta: skills-2025-10-02`; both are accepted-and-ignored per the platform's
existing stance (DIVERGENCES.md "anthropic-version / anthropic-beta headers").

| Endpoint | Notes |
| --- | --- |
| `POST /v1/skills` | multipart create (`files[]` parts + optional `display_title` field) |
| `GET /v1/skills` | cursor list; `?limit=` (default 20, max 100), `?page=`, `?source=custom\|anthropic` |
| `GET /v1/skills/{skill_id}` | |
| `DELETE /v1/skills/{skill_id}` | 400 until every version is deleted; response `{id, type:"skill_deleted"}` |
| `POST /v1/skills/{skill_id}/versions` | multipart; **no** `display_title` field |
| `GET /v1/skills/{skill_id}/versions` | cursor list; `?limit=` max **1000** (differs from the skills list's 100) |
| `GET/DELETE .../versions/{version}` | `{version}` slot is documented as the epoch-timestamp string only — `latest` is rejected there; whether the object id is also tolerated is unrecorded (see the recording checklist) — implement timestamp-only |
| `GET .../versions/{version}/content` | archive download; client sends `Accept: application/binary` |

### Objects

Skill: `{id, created_at, updated_at, display_title, latest_version, source
("custom"|"anthropic"), type:"skill"}` — timestamps are typed as plain ISO-8601
**strings** in the SDK on this surface (betaskill.go:117-158); the wire format is the
same ISO string the managed-agents resources emit, only the SDK's Go typing differs
(`string` here vs `time.Time` there). Skill version: `{id,
created_at, description, directory, name, skill_id, type:"skill_version", version}` —
`name`/`description` extracted from SKILL.md frontmatter, `directory` from the uploaded
filenames, `version` a server-minted Unix-epoch-microseconds string
(`"1759178010641129"`). The version object's id prefix is `skillver_` — absent from the
Go SDK's doc comments, present in the OpenAPI spec's examples and the public API
reference pages. Version DELETE echoes the **timestamp** as its `id`
(`{id:"1759178010641129", type:"skill_version_deleted"}`) — an asymmetry to reproduce.

### Multipart upload

Every reference client (Go SDK `apiform` encoder, `ant` CLI, docs curl examples) emits
one part per file with form field name **`files[]`**. The server accepts two forms
(skills-guide, verbatim: "Per-file uploads must keep a common top-level directory in
their paths … and a zip archive must contain the skill directory as its single top-level
entry"):

1. **Loose files** — N parts whose part *filenames* are path-qualified and share one
   common top-level directory (`financial_skill/SKILL.md`). Flat basenames are invalid.
2. **Zip** — a single part that is a zip archive with the skill directory as its only
   top-level entry (detected by magic bytes here; detection method is an inference).

`directory` is *extracted* from the upload (common root segment, or the zip's top entry),
then validated against SKILL.md's `name` case- and underscore-insensitively. Validation
(per the skills-guide): SKILL.md at the directory root; `name` ≤64 chars,
lowercase/digits/hyphens only, no XML
tags, no reserved words "anthropic"/"claude"; `description` non-empty ≤1024 chars, no XML
tags; total ≤30 MB; unknown frontmatter keys tolerated. `display_title` defaults from
`name` and must be unique among the workspace's custom skills. The `ant` CLI can only
emit the zip form (it basenames every part filename — anthropic-cli
pkg/cmd/flagoptions.go, `openFileUpload`), which makes it the canonical compatibility
probe.

### Pagination

`pagination.PageCursor`: envelope `{data, next_page}`, `?page=` token — exactly the
shape `internal/api/page.go` already produces; `listAgents` is the handler template. The
only friction is the versions list's documented max limit of 1000 vs `maxLimit = 100`;
the versions list reuses `page.go`'s existing per-resource mechanism (`parsePageMax`,
as the session-events list already does with `maxEventLimit = 1000`).

### Agent references and runtime semantics

- `skills[]` entries are a discriminated union `{type: "anthropic"|"custom", skill_id,
  version?}`. Anthropic skill_ids are short names (`xlsx`, `pptx`, `docx`, `pdf`) with
  date-based versions; custom ids are `skill_`-tagged. The managed-agents docs' field
  table marks `version` "Custom skills only", but the pinned SDK gives **both** variants
  the same optional `Version` ("Version to pin. Defaults to latest if omitted." —
  betaagent.go:1123-1124, 1178-1179) and both resolved response types require it — so
  validation accepts `version` on both types (as `parseSkills` already does); the
  docs-vs-SDK discrepancy is recorded with slice 2's inferences. Up to **500 skills per
  session**, counted across every agent (managed-agents/skills docs). Skills are not
  session-updatable (matches the existing tools/mcp_servers-only update rule).
- **`"latest"` persists verbatim.** The reference does *not* resolve versions at
  agent/session create: docs say an omitted version "Defaults to `latest` when omitted"
  and the create examples pass the literal; the SDK's own worker resolves the alias to a
  concrete numeric timestamp only at materialization time, by listing versions
  (tools/agenttoolset/skills.go:123-146 — "session.agent.skills[].version may be an
  alias such as \"latest\", which those endpoints reject"). The platform's existing
  `parseSkills` normalization already matches. This PR corrects the DIVERGENCES.md
  entry that claimed the opposite.
- **Materialization** (reference worker, tools/agenttoolset/skills.go): GET the session
  with the **environment key**, then per skill: resolve version → versions Get (for the
  name) → Download → extract into `{workdir}/skills/<name>/`. Extraction accepts
  zip/tar(.gz/.bz2), strips the single top-level wrapper, refuses slip escapes, caps at
  10,000 members / 1 GiB. Per-skill failure is logged and skipped, never fatal. So the
  skills read+download routes must join the env-key dual-auth lane (precedent:
  `isBareSessionPath`, added for this same worker's session read).
- **Level-1 injection is brain-side and required.** Docs: skill metadata is "included in
  the system prompt" at startup, and managed-agents skills incur "a modest cost on the
  session's context window, adding instructions and metadata". The worker emits no
  prompt text, so only the hosted brain can inject. The exact reference template is
  captured by no source — the block format here is an inference; claude-code-source is
  the design reference for budgeting/truncation, not wire behavior.

## Design decisions

1. **Object storage behind `internal/blob`.** A minimal `Store` interface —
   `Put(ctx, key, r, size, contentType)` / `Get(ctx, key)` / `Delete(ctx, key)` — with
   one S3-compatible implementation (`blob/s3`) on **minio-go v7** (single module;
   path-style for MinIO; works against AWS S3/Ceph/anything S3). Config-driven
   (endpoint, credentials, bucket, region, TLS); bucket auto-created at startup. Key
   layout `skills/{skill_id}/{version}.zip`; the namespace deliberately leaves room for
   the deferred Files API (#54's sibling seams) to share the store. Only controlplane
   (upload/download) and executor (materialization) touch S3; the BYOC worker stays
   wire-only — skill bytes always stream through the controlplane's `/content` endpoint,
   no presigned URLs (the reference serves bytes directly). Consistency: blob put before
   the DB transaction; on DB failure a best-effort object delete; rare orphaned objects
   are accepted and documented — GC is a non-goal.
2. **Deployment follows the Postgres precedent.** The helm chart gains a hand-written
   `templates/minio.yaml` (single-node StatefulSet + Service + PVC, `minio.enabled:
   true` by default, explicit root credentials required for the same GitOps-stability
   reason as `postgresql.password`) — deliberately **not** a subchart, per the chart's
   own air-gap rationale — plus `externalObjectStorage` values for BYO S3. Compose
   bundles a MinIO service.
3. **Canonical-zip storage.** Both upload forms normalize to one canonical zip (single
   top-level directory) at create time; the download endpoint streams the stored object
   unmodified; extracted `name`/`description`/`directory` live as columns so the brain
   never reads the archive.
4. **Anthropic prebuilt skills are operator-imported.** A controlplane run-once mode
   (`-import-anthropic-skills <checkout>`) reads skill directories from a local checkout
   of github.com/anthropics/skills (default: `skills/{docx,pdf,pptx,xlsx}`, the four the
   reference catalogs), validates like an upload, and inserts `source='anthropic'` rows
   with short-name ids and a date-based version = the checkout's commit date
   (`YYYYMMDD`; `--version` overrides; idempotent per version). **License red lines**:
   the four document skills are source-available, *not* open source — their content is
   never vendored into this Apache-2.0 repo and CI never clones external repos; CI
   fixtures are self-authored; the real checkout appears only in the operator's import
   and in opt-in evals.
5. **Version resolution at use time, everywhere.** Snapshots keep `latest` verbatim
   (matching the reference). The executor resolves against `latest_version` in the DB;
   the worker resolves over the wire list-and-pick exactly as the reference worker does;
   the brain resolves at request-assembly time. A version uploaded mid-session can skew
   consecutive resolutions — accepted, same as the reference.
6. **Materialization in the executor** happens after `Provision` succeeds in
   `provisionAndRun`: read zip from blob → extract in memory with the reference's guards
   (slip refusal, 10k members / 1 GiB) → loop `Sandbox.WriteFile` into
   `{workdir}/skills/<name>/` → write a sentinel file recording the resolved
   `{skill_id: version}` set so re-entrant provisioning skips rewrites. No new bulk-write
   sandbox primitive — the WriteFile loop is enough at ≤30 MB/skill; revisit only with
   evidence. Missing skill / bad archive: log, skip, continue (reference semantics).
7. **Existence is not validated at agent create.** References are validated for shape
   only; a dangling skill_id surfaces at materialization (skip + log) — consistent with
   the SDK's `skill_not_found_error` appearing as a *deployment pause* reason, i.e.
   late-bound. Recorded as an inference.
8. **No wire events for skill lifecycle.** The reference defines no session-visible
   skill events; observability is internal OTel only (below).

## Slices

Each slice is one PR through the full ritual (verifier, reviews, CI, squash). Docs move
with code; the listed DIVERGENCES.md entries land in the slice that creates the behavior.

1. **Blob store foundation** — `internal/blob` interface + `blob/s3` (minio-go);
   contract suite + MinIO test-container harness (test-support package excluded from the
   coverage denominator like `pgtest`); compose MinIO service; helm `minio.yaml` +
   `externalObjectStorage` + secret/values/README updates; blob op metrics. *(The plan
   itself and the DIVERGENCES.md skills-entry correction land in their own docs PR ahead
   of this slice.)* Acceptance: contract suite green against a real MinIO container;
   compose smoke still green.
2. **Skills registry API** — migration `0007_skills.sql` (`skills` + `skill_versions`,
   the standard org/workspace/project columns, partial-unique `display_title` for
   custom, `latest_version` maintained transactionally); `skillver_` prefix in
   `internal/domain`; a multipart decode path with its own body budget (~32 MiB) beside
   the JSON-only `decodeObject`; both upload forms + canonical-zip normalization +
   frontmatter validation; all nine endpoints; per-resource list limits; wire error
   shapes; API upload/download logs+metrics. Acceptance: contract tests modeled on
   `agents_test.go`; CI compose smoke extended with the curl skills round-trip (E2E-1);
   a real `ant beta:skills` zip-form transcript recorded in docs/HISTORY.md.
3. **Anthropic prebuilt import** — the run-once importer, date versions, idempotent
   upsert, import summary logging; self-authored Apache-2.0 CI fixture skills; license
   stance documented. Acceptance: importing a real checkout locally shows the four
   document skills via `ant beta:skills list --source anthropic`.
4. **Runtime materialization** — skills read+download routes join the env-key dual-auth
   lane; executor post-Provision materialization with sentinel idempotence;
   `internal/worker` SetupSkills twin (wire download, alias resolution, extraction
   guards); per-skill tolerance; the 500-skills-per-session cap at agent create/session
   overrides; materialization spans/metrics/logs on both halves. Acceptance: executor
   and worker contract tests; an end-to-end compose session whose bash tool `cat`s a
   materialized SKILL.md; a real `ant beta:worker poll` transcript (the reference
   worker's own SetupSkills against this platform) recorded in docs/HISTORY.md (E2E-3).
5. **Brain injection + closure** — `buildRequest` reads `agent.Skills`, looks up
   name/description from the store, appends the Level-1 block to `System` (inferred
   template: a skills list of `name - description` lines plus a usage note pointing at
   `skills/<name>/SKILL.md`); resolve-miss logging + injection metrics; the evals E2E
   task (E2E-2); remaining DIVERGENCES entries; README/ARCHITECTURE updates; archive
   this plan; close #54.

## End-to-end acceptance

- **E2E-1 (CI, every PR, wire-only)** — the compose smoke job gains a skills round-trip:
  curl multipart upload of a fixture zip → list/get → version create/list → download and
  byte-compare → delete-skill-before-versions is 400 → delete versions → delete skill.
  Needs MinIO + controlplane only; no model, no docker socket.
- **E2E-2 (opt-in, `RUN_EVALS=1`, full chain)** — an evals task: upload a self-authored
  fixture skill whose SKILL.md points at an answer file the task cannot be solved
  without; agent with `skills[]`; grade that the event log shows a read of
  `skills/<name>/SKILL.md` *and* the final answer matches. Proves registry → resolution
  → materialization → injection → actual model use. A variant uses a real
  anthropics/skills document skill when a checkout is present locally.
- **E2E-3 (BYOC, manual acceptance)** — the real `ant beta:worker poll` (whose SDK
  internals run SetupSkills) against this platform: the strongest wire evidence for the
  env-key lane and `/content`; transcript recorded in docs/HISTORY.md.

## Observability

slog structured logs (bridged to OTLP by `internal/telemetry`); per-package OTel meters
following `internal/events/metrics.go`. Cardinality rule: **no `skill_id` in metric
labels** (unbounded — ids go in logs and span attributes); labels are bounded
`op`/`outcome` only.

| Link | Instrumentation |
| --- | --- |
| api upload/download | slog create/delete/validation-reject (skill_id, version, file count, bytes, reason); `skills.uploads` counter{outcome}, `skills.upload.bytes` / `skills.download.bytes` histograms |
| blob | at the interface seam: `blob.op.duration` histogram{op, outcome}, `blob.op.bytes` histogram{op}; error slog with key+op |
| executor materialization | child span `skills_materialize` under the existing executor span; `skills.materialize.duration` histogram, `skills.materialized` counter{outcome=ok\|not_found\|failed}; a slog line for **every** skipped skill |
| worker twin | same instruments under the worker's existing span/meter |
| brain injection | attrs on the `model_request` span (injected count, block chars); `skills.resolve.misses` counter + slog on any missing reference |
| importer | slog summary only (run-once) |

## Inferences and divergences to record, by slice

Slice 2: zip-form detection by magic bytes; `anonymous_file`/flat-basename rejection;
upload error shapes; download response headers; `display_title` uniqueness case
handling; the docs-vs-SDK discrepancy on anthropic-skill `version` (docs field table
says custom-only, the SDK carries it on both variants). Slice 3: operator-import
provisioning of anthropic-source skills and date-version minting (deliberate divergence
— the reference hosts these itself). Slice 4: no create-time existence validation;
materialization timing (sandbox provision, not session create). Slice 5: the Level-1
injection template; no wire skill-lifecycle events. Already carried by the plan's own
docs PR: the corrected "latest"-normalization entry.

## Recording checklist (deferred until credentials exist)

A future `ant` recording against api.anthropic.com closes: exact upload-rejection
statuses/shapes; zip-form detection method; `anonymous_file` tolerance; the literal
echoed by GET after an omitted version; a real minted `skillver_` value; whether
`{version}` slots tolerate the object id; `display_title` derivation/uniqueness errors;
the reference's injection template (via a session asked to echo its instructions).
Each is cross-linked from its DIVERGENCES.md entry.
