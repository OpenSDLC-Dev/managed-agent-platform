---
status: draft
issue: "#55"
---

# Files: the `/v1/files` registry and session file resources (plan 08)

This plan lifts the **Files half of #55** out of its reserved seam: the wire-compatible
`/v1/files` registry over the existing object-storage layer, session `resources[]`
accepting `type: "file"` mounts (with the `sesrsc_` sub-resource endpoints), and
materialization of mounted files into sandboxes on both execution halves. The other half
of #55 — **git/repo mounting (`github_repository` resources)** — stays deferred; #55
remains open tracking it, and decision 6 keeps the union seam it will land in. Four PR
slices.

One correction to the record: #55's issue comment claims the reference Files API has
soft-archive semantics (`archived_at`). It does not — `grep -rni archived` over the pinned
SDK hits the archivable managed-agents resources (agents, sessions, threads, deployments,
vaults, …) but never files (betafile.go: zero hits; no `file.archived` constant exists in
shared/constant/constants.go), and the public docs say
"Deleted files cannot be recovered". File lifecycle is hard delete only
(`{"id": …, "type": "file_deleted"}`).

## Ground truth (verified 2026-07-22)

Resolved per CLAUDE.md's order: public docs (the platform.claude.com Files guide, fetched
2026-07-22) → pinned `anthropic-sdk-go` v1.58.0 (`betafile.go`, `betasession.go`,
`betasessionresource.go`; the local tip checkout is byte-identical to the pin — both
declare release 1.58.0, so there is no newer surface to track) → the `ant` CLI source. No
live recording was possible (no Anthropic credentials); everything only a recording can
settle is pre-listed in "Inferences to record" below and lands in docs/DIVERGENCES.md
with its slice.

### Endpoints — files (beta header `files-api-2025-04-14`; all paths carry `?beta=true`)

| Endpoint | Notes |
|---|---|
| `POST /v1/files` | `multipart/form-data` with a required file part named **`file`** (BetaFileUploadParams betafile.go:274-279; apiform falls back to the `json` tag for the part name, internal/apiform/tag.go:23-27). Whether the reference rejects extra, unknown, or duplicate parts is **not** established by the SDK — we parse strictly (one `file` part, anything else 400, the skills-upload precedent) and record that strictness as an inference (slice 1). Filename comes from the part's `Content-Disposition` — the SDK defaults to `anonymous_file` for anonymous readers (encoder.go:372-399), the CLI sends the path basename with a `mime.TypeByExtension` part Content-Type (anthropic-cli flagoptions.go:568-582). Public-docs validation: filename 1–255 chars, forbidden `< > : " | ? * \ / ` and control chars 0–31 → 400 "Invalid filename"; > 500 MB → 413. Returns `FileMetadata`. |
| `GET /v1/files` | Classic `Page` envelope `{data, has_more, first_id, last_id}` (pagination.go:21-34 — **not** the `next_page` cursor our other lists use). Query: `after_id`, `before_id`, `limit` (1–1000, default 20), `scope_id` (filter by scoping resource, e.g. a session ID) — betafile.go:229-246. |
| `GET /v1/files/{file_id}` | `FileMetadata`. |
| `GET /v1/files/{file_id}/content` | Binary stream; the SDK sends `Accept: application/binary` (betafile.go:95). Public docs: **uploaded files are not downloadable** — only files created by skills or the code-execution tool (`downloadable: true`); downloading an uploaded file returns **400**. The CLI derives the local filename from `Content-Disposition: …; filename=…` and falls back to `file-*` temp names without it (anthropic-cli cmdutil.go:433-457). |
| `DELETE /v1/files/{file_id}` | `{"id": …, "type": "file_deleted"}` (betafile.go:153-184). Hard delete, permanent. |

### Objects

`FileMetadata` (betafile.go:186-221): `id`, `created_at` (RFC 3339), `filename`,
`mime_type`, `size_bytes` — all `api:"required"` — plus `type: "file"`, `downloadable`
(bool, no required tag), `scope` (`api:"nullable"`: `{id, type: "session"}`,
betafile.go:133-151). Uploads get `downloadable: false` and `scope: null` (public docs:
"`downloadable` is `false` for files you upload"; scope is set only for files created in a
session's context — nothing in this plan creates those). ID prefix `file_`
(betasessionresource_test.go:156), already in `internal/domain/id.go:30` and
`knownPrefixes`.

### Session resources (beta header `managed-agents-2026-04-01`)

- **Create**: `POST /v1/sessions` `resources[]` is a union discriminated on `type` —
  `github_repository` | `file` | `memory_store` (betasession.go:2203-2220). The file
  variant (betasession.go:693-717): `file_id` (required), `type: "file"` (required),
  `mount_path` (optional — "Mount path in the container. Defaults to
  `/mnt/session/uploads/<file_id>`"). Session **update has no resources field**
  (betasession.go:2320-2337).
- **Materialized resource** (betasessionresource.go:176-209): `{id (sesrsc_…),
  created_at, file_id, mount_path, type: "file", updated_at}` — every field
  `api:"required"`, so the server resolves the default mount path at create and renders it.
- **Sub-endpoints** (betasessionresource.go): `GET /v1/sessions/{sid}/resources/{rid}`;
  `POST …/{rid}` (update) takes only `authorization_token` — "Currently only
  `github_repository` resources support token rotation"; `GET …/resources` (list) uses the
  `PageCursor` envelope `{data, next_page}` (pagination.go:246-255) with `limit` ≤ 1000 and
  **"if omitted, returns all"**; `DELETE …/{rid}` → `{id, type:
  "session_resource_deleted"}`; `POST …/resources` (add) — the request body is the **file
  variant only** and the response is typed as the file resource (betasessionresource.go:735-745).
- Session GET renders `resources[]` as `api:"required"` (betasession.go:1038). Events:
  no `session_resource.*` or `file.*` event exists in the taxonomy; files appear in events
  only as content-block *sources* (file document/image sources, the define_outcome file
  rubric) — none of which this plan implements.

### What the reference worker does **not** do

`ant beta:worker` and the SDK's `lib/environments` worker never call `/v1/files` or the
session-resources endpoints (zero grep hits across `lib/` and `tools/`): the reference
materializes resource mounts server-side; workers download only skills (`SetupSkills`).
Our BYOC worker is architecturally different — it provisions the sandbox itself — so
slice 4 gives it a wire-only fetch path and records the divergence.

## Design decisions

1. **Storage mirrors skills — no new infrastructure.** A `files` table (migration
   `0008_files.sql`: `id`, the org/workspace/project scope columns, `filename`,
   `mime_type`, `size_bytes`, `downloadable boolean NOT NULL DEFAULT false`, nullable
   `scope_type`/`scope_id`, `created_at`) plus bytes at blob key **`files/{file_id}`** —
   the second consumer of the namespace `internal/blob/blob.go:5-7` reserved for exactly
   this. Same transaction ordering as `insertSkill` (internal/api/skills.go:171-200): row
   claimed in the tx, blob put before commit, failed-commit orphan cleaned best-effort, GC
   a non-goal. Files are immutable — no update endpoint, no versions.
2. **Hard delete only, dangling references tolerated by design.** `DELETE` removes the
   row and the object (blob delete is idempotent). Deleting a file some session still
   references is allowed, and a create/add existence check racing a concurrent delete can
   commit a reference to a just-deleted file — both land in the same accepted state: a
   dangling resource whose materialization records `not_found` per-resource and moves on,
   the skills precedent (internal/executor/skills.go:124-132), observable in the
   `files.materialized` counter and the `files_materialize` span, and visible to the agent
   as an absent path. Considered and rejected: a separate `session_resources` table with
   `ON DELETE RESTRICT` — it would make `DELETE /v1/files/{id}` fail for referenced files
   (a wire behavior the reference nowhere documents), add a table the path-scoped
   sub-endpoints don't need, and still not stop the sandbox-side gap (a file deleted after
   materialization already succeeded). Whether the reference restricts or tolerates this
   is on the recording checklist.
3. **Reference-faithful download semantics.** Uploads are `downloadable: false`; a
   management-key `GET …/content` on such a file returns the reference's 400. Bytes still
   reach sandboxes: the executor reads the blob store directly, and the BYOC worker (slice
   4) fetches over an environment-key lane that bypasses the downloadable gate — that lane
   is materialization transport, not the public download feature. Nothing in this plan
   produces a `downloadable: true` file (session-generated outputs are future work; the
   column and `scope` fields are the seam they'll land in).
4. **Caps per the public docs.** 500 MB per file, enforced with `http.MaxBytesReader`
   (plus a small multipart-overhead margin) → 413, and the documented filename validation
   → 400. The reference's 500 GB org-storage quota is deliberately **not** enforced —
   self-hosted operators own their disk (CONFIRMED divergence, slice 1).
5. **Session resources live in `sessions.resources` jsonb** — the column reserved since
   0001_init.sql:80 — as wire-shaped objects, no new table. Every sub-endpoint path
   carries the session ID, so lookups stay inside one row, and mutations take the same
   `FOR UPDATE OF s` session lock the events path uses (internal/api/events.go:46-64).
   `domain.SessionResource` (internal/domain/session.go:87-94) grows the typed file fields
   (`FileID`, `MountPath`, `CreatedAt`, `UpdatedAt`) while keeping the raw envelope for
   forward compatibility.
6. **`resources[]` validation replaces `rejectUnsupportedList`**
   (internal/api/sessions.go:270-285, call at :305-307). `type: "file"` is accepted:
   `file_` ID shape via `checkID`-style validation, **existence checked at create/add**
   (one SELECT in the same tx — cheaper failure locality than skills' unvalidated refs;
   flagged as an inference since the reference's behavior is unrecorded), `mount_path`
   defaulted to `/mnt/session/uploads/<file_id>`, must be absolute, NUL-free, ≤ 1024
   bytes, and unique within the session. `github_repository` and `memory_store` are
   rejected with the established "'X' resources are not supported yet" error — keeping the
   union seam open for the git half of #55. The DIVERGENCES line-28 entry ("session
   resources rejected") is carved down accordingly in slice 2.
7. **Materialization mirrors the skills three-point pattern.** Executor: after
   `materializeSkills`, a `materializeFiles` pass writes each mounted file's blob bytes to
   its `mount_path` via `sb.WriteFile` before tool execution, with sentinel idempotence
   (`{workdir}/.files_materialized` listing `{sesrsc_id, file_id}` pairs, existence
   re-probed since the workdir is agent-writable — the skills tamper analysis carries
   over) and per-resource failure tolerated. Worker: a wire-only twin (`Sessions.Get` for
   `resources[]` → `Files.Download` on the env-key lane → `sb.WriteFile`). Brain: a
   Level-1-style "Mounted files" block (mount path, filename, mime type, size) appended
   after the skills block at request assembly — format inferred, exactly like the skills
   line-67 entry — so the agent can find mounts outside the workdir. Because a mount can
   legitimately be 500 MB and `Sandbox.WriteFile` takes `[]byte`, slice 3 extends the
   sandbox seam with a **streaming write counterpart** (an `io.Reader` + size signature,
   landed through `sandboxtest` so the docker and k8s backends both satisfy it — docker's
   archive-PUT endpoint and a k8s exec-with-stdin both stream naturally); blob `Get` and the
   SDK's `Files.Download` already return streaming readers, so a mount never fully
   buffers in the executor or the worker.
8. **Two pagination envelopes.** `/v1/files` gets the classic `Page` shape — a new
   id-cursor envelope in `internal/api/page.go` alongside `pageJSON` — ordered newest-first
   (`created_at` desc, id tiebreak; ordering is unrecorded, flagged as an inference), with
   `limit` ≤ 1000 and `scope_id` as a plain column filter. The resources list reuses
   `pageJSON` (`next_page`) with the documented return-all default when `limit` is omitted.
9. **Observability mirrors skills names**, same meters, outcome-only labels, IDs in span
   attributes never metric labels: `files.uploads` / `files.upload.bytes` /
   `files.download.bytes` on the api meter; `files.materialized` /
   `files.materialize.duration` and a `files_materialize` span at both execution halves.
   Blob-level metrics come free via the existing `blob.WithMetrics` decorator.
10. **Availability and auth follow the skills precedent.** `blobs == nil` → the files
    endpoints fail 500 like `errSkillsUnavailable` (internal/api/skills.go:96-98).
    Management endpoints ride the x-api-key lane; slice 4 adds an `isFileReadPath`
    predicate to `dispatchAuth` (internal/api/server.go:159-168) admitting **only**
    `GET /v1/files/{id}/content` to the environment-key dual-auth lane — narrower than
    skills' full read set — and the download handler skips the downloadable gate for that
    lane. Unlike skills (whose env lane is deliberately workspace-global — skills are
    shared assets), file content can be sensitive, so the env lane is **session-scoped**:
    the download handler authorizes an environment key only for a file referenced by the
    `resources[]` of a session in that environment (one jsonb containment lookup —
    `resources @> '[{"file_id": …}]'` filtered on `environment_id`). A leaked environment
    key never becomes a workspace-wide file-exfiltration credential. Recorded with the
    auth-scope entry in slice 4 (the reference has no worker file lane at all).

**Non-goals:** git/repo mounting (`github_repository` — stays on #55), memory-store
resources, session-produced output files (nothing sets `scope`/`downloadable: true`;
`scope_id` filtering works but matches nothing until that feature exists), storage
quotas, file content blocks in session events, resource unmounting (deleting a `sesrsc_`
never reaches into a live sandbox).

## Slices

Each slice is one PR through the full ritual (verifier, reviews, CI, squash). Docs move
with code; the DIVERGENCES.md entries listed in "Inferences and divergences to record"
land in the slice that creates the behavior. *(The plan itself lands ahead of slice 1 in
its own docs PR; STATE.md is claimed when implementation starts.)*

1. **The `/v1/files` registry.** Migration `internal/store/migrations/0008_files.sql`
   (bump `wantMigrations` to 8, internal/store/store_test.go:33); `domain.File` +
   wire render; `internal/api/files.go` + `filesupload.go` + `filesmetrics.go`: upload
   (multipart parse mirroring `parseSkillUpload`, one `file` part, filename/size
   validation, `downloadable: false`, tx row-then-blob-put), list (new `Page` envelope +
   `after_id`/`before_id`/`limit`/`scope_id`), get-metadata, download (400 gate; the
   streaming path behind it reuses the `downloadSkillVersion` shape,
   internal/api/skills.go:613-662, plus `Content-Disposition`), delete (row + blob,
   `file_deleted`); routes + 405-fallback entries in `server.go`.
   **Acceptance:** `ant beta:files upload/list/retrieve-metadata/delete` round-trip
   against the local server; `ant beta:files download` returns the reference's 400;
   `make verify` green.
2. **Session `resources[]` + sub-resource endpoints.** Typed union parsing replaces the
   `resources` `rejectUnsupportedList` call (file accepted, other types "not supported
   yet"); `sesrsc_` minting, mount-path defaulting/validation, existence check; jsonb
   persistence + render; the five sub-endpoints (list / get / add / delete / update-reject)
   under the archived-session gate (internal/api/sessions.go:514-515); update the pinned
   tests (sessions_test.go:229 "resources unsupported" case, edge_test.go:78 and :497
   stay green, unit_test.go:111 render defaults).
   **Acceptance:** `ant beta:sessions create --resource '{file_id: …, type: file}'`
   round-trips with a rendered `sesrsc_` resource; all five
   `ant beta:sessions:resources` subcommands behave per the table above.
3. **Executor materialization + brain injection + eval.** The sandbox seam gains the
   streaming write counterpart to `WriteFile` (decision 7), with a `sandboxtest` contract
   case both backends must pass; `sessionForRun` also selects `resources`
   (internal/executor/executor.go:328-368); `internal/executor/files.go` materializes
   mounts by streaming blob → sandbox (sentinel + re-probe, per-resource tolerance,
   metrics + span);
   brain renders the "Mounted files" block into `buildRequest` alongside the skills block
   (internal/brain/brain.go:175-184); new `file-answer` eval mirroring `skill-answer` —
   the passphrase lives only in an uploaded file mounted at the default path, so a correct
   answer requires upload → mount → materialize → read.
   **Acceptance:** E2E-2 — `RUN_EVALS=1` `file-answer` passes against a real model
   endpoint; platform-half chain proven.
4. **BYOC worker + env-key content lane; archive.** `isFileReadPath` dual-auth lane for
   `GET /v1/files/{id}/content` with the lane-aware downloadable bypass and the
   session-scoped authorization check (decision 10);
   `internal/worker/files.go` wire-only twin with the same sentinel and metrics; E2E-3
   manual acceptance (worker on the host materializes a mount; transcript to
   docs/HISTORY.md); README/ARCHITECTURE updates for the landed feature; archive this
   plan, post the Files-half completion on #55 (the issue stays open for git mounting).
   **Acceptance:** `ant beta:worker`-equivalent BYOC run shows the mounted file inside
   the sandbox; the env-key lane rejects non-content file paths.

## End-to-end acceptance

- **E2E-1 (CI, every PR, wire-only):** the compose stack + `ant` CLI built from the
  reference checkout — upload/list/metadata/delete round-trip, download-400, session
  create with a file mount, resources subcommands (extends the existing CI E2E job as
  slices land).
- **E2E-2 (opt-in, `RUN_EVALS=1`):** the `file-answer` eval — the full platform chain
  (API upload → session mount → executor materialization → agent file-tool read →
  answer).
- **E2E-3 (BYOC, manual):** a worker outside the platform materializes the same mount
  wire-only; transcript recorded in docs/HISTORY.md (slice 4).

## Observability

Cardinality rule as everywhere: outcome-only metric labels; file and resource IDs go to
span attributes and logs.

| Link | Instrumentation |
|---|---|
| API upload/download/delete | `files.uploads` (counter, `outcome` ∈ ok\|invalid\|error), `files.upload.bytes`, `files.download.bytes` (histograms, "By") on the api meter; request span from `withTracing` as today |
| Blob traffic | existing `blob.op.duration` / `blob.op.bytes` via `WithMetrics` |
| Executor materialization | `files_materialize` span (`files.referenced`, `files.materialized`, `files.unchanged`); `files.materialized` counter (`outcome` ∈ ok\|not_found\|failed), `files.materialize.duration` |
| Worker materialization | identical names on the worker meter (the skills twin-name precedent) |
| Brain injection | span attributes `files.injected`, `files.block_chars` on the model_request span (the skills precedent, brain.go:207-210) |

## Inferences and divergences to record, by slice

Slice 1 — DIVERGENCES.md entries: **CONFIRMED** — org-storage quota not enforced
(decision 4); **INFERRED** — list ordering newest-first and cursor direction; strict
multipart parsing (extra, unknown, or duplicate parts rejected — the SDK proves only the
required `file` part); filename edge cases (missing/`anonymous_file` parts, the exact 400
messages); download response headers (`Content-Disposition` presence); upload part
Content-Type → `mime_type` fallback.

Slice 2 — **CONFIRMED** — the line-28 create-rejection entry carved down (file resources
now accepted; `github_repository`/`memory_store` still rejected); **INFERRED** — file
existence validated at create/add; duplicate-mount-path rejection; resource mutations on
running/terminated sessions; deletion does not unmount; deleting a still-referenced file
tolerated (the dangling-reference state of decision 2); the update (token-rotation)
error shape for file resources.

Slice 3 — **INFERRED** — the "Mounted files" block format and placement (the skills
line-67 twin); sentinel idempotence instead of re-materializing every item (extends the
skills line-32 analysis to file mounts).

Slice 4 — **CONFIRMED/INFERRED** — file-content reads on the environment-key lane with
the downloadable bypass, session-scoped per decision 10 (the reference worker has no file
path at all, so the lane itself and its auth scope are ours to define and record).

## Recording checklist (deferred until credentials exist)

Each lands cross-linked from its DIVERGENCES.md entry when a real `ant` recording becomes
possible:

- `GET /v1/files` result ordering and empty-page `first_id`/`last_id` rendering.
- Upload with a missing/pathological filename part; duplicate filename uploads; extra,
  unknown, or duplicate multipart parts; the exact invalid-filename and 413 error
  messages.
- `DELETE /v1/files/{id}` on a file still referenced by a session's `resources[]` —
  restricted or tolerated, and what later materialization does.
- Whether create-time `resources[]` validates file existence, and the error shape.
- Resource add/delete on a running or terminated session; whether deletion unmounts.
- The update (token-rotation) error for a `file` resource.
- `Content-Disposition` on `/v1/files/{id}/content` responses.
- Whether the reference renders the resolved default `mount_path` at create (the SDK
  comment documents the default; the materialized-row shape implies the server fills it).
- How `scope` gets set and whether session-scoped files appear in unfiltered lists.
