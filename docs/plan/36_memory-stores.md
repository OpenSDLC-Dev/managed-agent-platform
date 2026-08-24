---
status: draft
issue: "#52"
---

# Memory stores — a mounted, versioned directory the agent reads and writes (plan 36)

This plan addresses #52 (its last slice closes it): a **memory store** is a
workspace-scoped collection of text documents; attached to a session through
`resources[]`, it is mounted at `/mnt/memory/<slug>` inside the sandbox, the agent reads
and writes it with the ordinary file tools, every write becomes an immutable, attributed
**memory version**, and the store stays in sync across the sessions that share it. At
drafting the seam is reserved and nothing more — `internal/api/sessionresources.go`
`parseResourceObject` rejects `type:"memory_store"` with "memory_store resources are not
supported yet", `internal/api/sessions.go` `listSessions` short-circuits the
`memory_store_id` filter to an empty page, `internal/domain/id.go`'s `knownPrefixes`
carries no memory prefix, no `/v1/memory_stores` route exists (`ant beta:memory-stores
list` gets our `no such endpoint` 404), and `internal/api/workapi.go` renders the work
item's `secret` as null on every path. After this plan the whole memory surface the
pinned SDK types — stores, memories, versions, the session attachment and its list filter
— answers the real `ant` CLI unchanged; a cloud session's agent reads and writes a
materialized store with writes flowing back as `session_actor` versions; and a
`self_hosted` session's stores are served both by the platform's own BYOC worker and by
the reference's `ant beta:worker poll`, which needs the per-item sessions token this plan
starts issuing.

Scope decisions, proposed 2026-08-24 (awaiting the user; the design below assumes them):

1. **Bump the SDK pin to v1.66.0 as slice 0.** Every memory wire type this plan mirrors
   is byte-identical between v1.63.1 and v1.66.0 (`git diff v1.63.1 v1.66.0 --
   betamemorystore.go betamemorystorememory.go betasessionresource.go
   betaenvironmentwork.go` is empty), so the *schema* needs no bump. What only v1.66.0
   holds is the **reference worker's memory behavior** — `lib/environments/memories.go`
   (the download/sync engine), the `secret` → `sessions_token` decoding in
   `lib/environments/worker.go`, and the file tools' `AllowedRoots`/`ReadOnlyRoots` — the
   behavioral contract slices 5 and 6 are bound by and the registry must cite; a plan
   citing a checkout the repo refuses to read is the doc/code lag the verifier exists to
   catch (plan 35 decision 1's argument, unchanged). Measured cost (a scratch build on
   v1.66.0): one test line (`acceptance/dcf_test.go:54`'s `anthropic.FileMetadata` became
   `BetaFileMetadata` in v1.65.0), 48 live `v1.63.1` labels to advance, the HISTORY
   record, and a citation re-read — the memory files did not move. v1.64.0 also adds a
   fourth actor variant (`service_account_actor`) this platform will never emit.
   Rejected: staying on v1.63.1 and citing v1.66.0 worker behavior as "contract, not
   schema" — every worker citation would point at an unpinned HEAD, and DIVERGENCES'
   `secret` entry (which says the reference worker "never reads the field") would keep a
   claim the pinned checkout contradicts.
2. **`self_hosted` memory is in this plan, as its last two slices; until they land, a
   `memory_store` resource on a `self_hosted` session is a 400.** The reference's own
   worker at v1.66.0 *fails the work item* when a session has a store and the item carries
   no sessions token (`worker.go:516-528`, `ErrSessionMemoryNoToken`) — the session then
   sits idle with no error event. So "accept the attachment, materialize nothing" (the
   `github_repository` precedent, #322) is not available here: it would break every
   `ant beta:worker poll` ≥ 1.26.1 the moment a store is attached. The clean interim is a
   refusal with a clear error, registered CONFIRMED with a tracker, and lifted by slice 6.
   Rejected: a second plan for BYOC memory — the sessions token's acceptance matrix
   (design decision 15) and the memory routes' auth are one design, and splitting them
   would have plan 36 choose the memory routes' lanes without the token they must admit.
3. **Dreams are out of scope.** A dream is an asynchronous consolidation job that runs a
   pipeline session over a store plus up to 100 transcripts and writes a cloned output
   store; the docs call the shapes "volatile", gate them behind a research-preview access
   form, and state nothing about the pipeline's agent, tools, stages, or the output store's
   fields. It is a model-driven feature of its own (harness references treat it as one:
   Claude Code's auto-dream and Codex's phase-2 consolidation are each a subsystem),
   sitting on top of the storage this plan builds. `/v1/dreams` stays absent (our 404),
   registered CONFIRMED against a new `(post-v1)` issue; the `drm_` prefix is not added.
4. **The unrecorded behaviors go under #78 unless the user supplies a Managed Agents key.**
   Ten behaviors are stated nowhere (the recording checklist): above all the system
   prompt's "memory section" wording, which a session can only reveal by echoing it. Every
   one lands INFERRED against #78 with a parenthetical; a recording before slice 3 merges
   converges the entries it settles, one after lands as corrections in place.
5. **`memory_store.*` webhooks are not delivered** — this platform delivers no webhooks
   (plan 21's precedent for outcome events, #261) — and **versions are retained without
   pruning**: the reference keeps versions 30 days "however, the recent versions are always
   kept", a rule with an unstated count; this plan retains every version and files the
   pruning job as its own issue rather than inventing the count.

Out of scope, besides decisions 3 and 5: the `service_account_actor` variant (no service
accounts exist here; the three other actors cover every writer), deployments' memory
resource config (`betadeployment.go`, #51), an organization store cap and memory-endpoint
rate limits (values unpublished; nothing here is metered), a relevance selector or
always-loaded index over memory content (the reference exposes the directory and a prompt
note, nothing more — harness ideas beyond that are not wire behavior), and importing a
harness's memory directory (Claude Code's `projects/<key>/memory/`) into a store.

## Ground truth (verified 2026-08-24)

### Wire shapes

Read at `anthropic-sdk-go` v1.63.1 with `git show v1.63.1:<file>`; every file below is
byte-identical at v1.66.0 except where marked. Official docs fetched 2026-08-24
(`platform.claude.com/docs/en/managed-agents/memory`, `…/self-hosted-sandboxes`, and the
`api/beta/memory_stores/**` reference pages).

- **Routes** (`api.md:1057-1114`; every path literally carries `?beta=true`, every
  store/memory/version method prepends `anthropic-beta: agent-memory-2026-07-22`,
  `betamemorystore.go:52`): `POST|GET /v1/memory_stores`, `GET|POST|DELETE
  /v1/memory_stores/{id}`, `POST …/{id}/archive`; `POST|GET …/{id}/memories`,
  `GET|POST|DELETE …/{id}/memories/{mid}`; `GET …/{id}/memory_versions[/{vid}]`, `POST
  …/{id}/memory_versions/{vid}/redact`. Update is `POST` (`betamemorystore.go:85`);
  delete passes params so `expected_content_sha256` rides the query
  (`betamemorystorememory.go:145`). No unarchive, no version create/update/delete.
- **Store** `BetaManagedAgentsMemoryStore` (`betamemorystore.go:176-216`): `id`
  (`memstore_…`), `type:"memory_store"`, `name` (1–255, no control chars, "need not be
  unique"), `description` ("Empty string when unset", ≤ 1024), `metadata` (≤ 16 pairs,
  keys 1–64, values ≤ 512, "Not visible to the agent"), `created_at`, `updated_at`,
  `archived_at` nullable. Create params `:230-247`; update `:263-278` (`description` ""
  clears; `metadata` is a patch — string upserts, null deletes, omitted keeps; renaming
  "changes the slug used for the store's `mount_path` in sessions created after the
  update"); list `:288-307` (`created_at[gte|lte]` inclusive, `include_archived` default
  false, `limit` 1–100 default 20, `page`). Tombstone `{id, type:"memory_store_deleted"}`
  (`:148-174`): "The store and all its memories and versions are no longer retrievable."
- **Memory** `BetaManagedAgentsMemory` (`betamemorystorememory.go:180-240`): `id`
  (`mem_…`, "Stable across renames"), `type:"memory"`, `memory_store_id`,
  `memory_version_id` ("the authoritative head pointer"), `path` (starts with `/`,
  case-sensitive, unique within a store, ≤ 1,024 bytes), `content` nullable (≤ 102,400
  bytes; null when `view=basic`), `content_sha256` and `content_size_bytes` ("Always
  populated, regardless of `view`" — "The server applies no normalization"),
  `created_at`, `updated_at`. Create `:414-430` (`content` required — `""` for empty;
  `path` "Must not contain empty segments, `.` or `..` segments, control or format
  characters, and must be NFC-normalized"); update `:469-494` (`content` and `path` each
  optional — rename-only is legal; `precondition {type:"content_sha256",
  content_sha256}`); list `:513-538` (`path_prefix` "Must end with `/`", `depth` 0 or 1,
  `limit` "Capped at 20 when `view=full`. Both `memory` and `memory_prefix` items count
  toward the limit", `view`); delete `:549-556`. `view` (`:369-379`): retrieve defaults
  `full`, list/create/update default `basic`. Precondition (`:381-412`): "On mismatch,
  the request returns `memory_precondition_failed_error` (HTTP 409) … If the
  precondition fails but the stored state already exactly matches the requested `content`
  and `path`, the server returns 200 instead of 409." List items are a union of `memory`
  and `memory_prefix {path, type}` (`:248-367`) — "a list-time rollup, not a stored
  resource … interleaves with `memory` items in path order." Tombstone `{id,
  type:"memory_deleted"}`: "The memory's version history persists and remains listable."
- **Version** `BetaManagedAgentsMemoryVersion` (`betamemorystorememoryversion.go:221-297`):
  `id` (`memver_…`), `type:"memory_version"`, `memory_id`, `memory_store_id`, `operation
  ∈ created|modified|deleted` ("Every non-no-op mutation to a memory appends exactly one
  version row"), `content` (null when `view=basic`, `deleted`, or redacted),
  `content_sha256` / `content_size_bytes` (null when redacted or `deleted`), `path` ("null
  if and only if `redacted_at` is set"), `created_at`, `created_by`, `redacted_at`,
  `redacted_by`. "Retrieving a redacted version returns 200 with `content`, `path`,
  `content_size_bytes`, and `content_sha256` set to `null`; branch on `redacted_at`, not
  HTTP status." Actor union (`:113-370`, three variants at v1.63.1): `session_actor
  {session_id}` — "a write made by an agent during a session, through the mounted
  filesystem at `/mnt/memory/`"; `api_actor {api_key_id}` — "This identifies the key, not
  the secret"; `user_actor {user_id}` — "a human user through the Anthropic Console".
  v1.64.0 adds `service_account_actor {service_account_id}` and a `service_account_id`
  list filter. List params (`:392-418`): `memory_id`, `operation`, `session_id`,
  `api_key_id`, `created_at[gte|lte]`, `limit`, `page`, `view` — the docs add the order:
  "`created_at` descending (newest first), with `id` as tiebreak"; no default `limit` is
  stated. Redact (`:429-434`): no body.
- **Session attachment.** Create param `BetaManagedAgentsMemoryStoreResourceParam`
  (`betasession.go:896-935`): `memory_store_id` (required), `type:"memory_store"`,
  `instructions` ("Rendered into the memory section of the system prompt. Max 4096
  chars"), `access ∈ read_write|read_only`. Response variant
  `BetaManagedAgentsMemoryStoreResource` (`betasessionresource.go:317-352`): exactly
  `memory_store_id, type, access` (nullable), `description` ("snapshotted at attach time.
  Rendered into the agent's system prompt. Empty string when the store has no
  description"), `instructions` (nullable), `mount_path` (nullable; "e.g.
  /mnt/memory/user-preferences. Derived from the store's name. Output-only"), `name`
  (nullable; "snapshotted at attach time. Later edits to the store's name do not
  propagate") — **no `id`, `created_at` or `updated_at`**, unlike the file (`:176-197`)
  and repository (`:211-245`) variants. `BetaSessionResourceAddParams` wraps only file
  params (`:735-745`) and the update body is only `authorization_token` (`:690-698`):
  attach is create-time only. Sessions list filter `memory_store_id` — "Filter sessions
  whose resources contain a memory_store with this memory store ID"
  (`betasession.go:2830-2832`).
- **Work item secret.** `BetaSelfHostedWork.Secret` is required at the pin
  (`betaenvironmentwork.go:303-305`: "May be populated when polling for work; null on all
  other retrieval paths"); its *contents* are stated only by the v1.66.0 worker, which
  decodes unpadded URL-safe base64 of a JSON object and reads its `sessions_token` key
  (`lib/environments/worker.go:782-811`; the test fixture also carries
  `session_ingress_token` and `auth[]`, neither consumed).
- **Session events**: zero memory mentions in `betasessionevent.go` at either tag —
  memory I/O is visible only as ordinary `agent.tool_use`/`agent.tool_result`.
- **Webhooks**: `memory_store.created|archived|deleted` only (`betawebhook.go:514-515`);
  "Individual memories and memory versions emit no webhook events."
- **Dreams** (`betadream.go`): `POST|GET /v1/dreams`, `GET …/{id}`, `POST
  …/{id}/archive|cancel`, header `dreaming-2026-04-21`, id `drm_…`; excluded (scope
  decision 3).
- **Beta headers**: the docs say memory-store endpoints take `agent-memory-2026-07-22`
  *instead of* `managed-agents-2026-04-01` and that "sending both returns a `400`"; the
  CLI's own tests send `--beta message-batches-2024-09-24` on memory calls. This platform
  accepts and ignores `anthropic-beta` (`internal/api/doc.go`); nothing here changes that.
- **Prefixes**: `memstore_`, `mem_`, `memver_` (SDK comments); `drm_` (dreams guide);
  cursors "a `page_...` value" (docs; the SDK treats `next_page` as opaque,
  `pagination.go:269-278`).

### Documented semantics the design rests on

- Mount and tools (memory guide): "Each attached store is mounted inside the session's
  sandbox as a directory under `/mnt/memory/`. The directory name is the store's display
  name sanitized to a filesystem-safe slug (lowercased; non-alphanumeric runs become a
  single hyphen) … The agent reads and writes the store with the standard agent toolset.
  Writes under the mount path are persisted back to the store and stay in sync across
  sessions that share it; writes to any other path under `/mnt/memory/` fail, because the
  sandbox mounts that parent directory read-only. A short description of each mount
  (display name, mount path, access mode, store `description`, and any `instructions`) is
  automatically added to the system prompt." "`access` is enforced at the filesystem
  level: a `read_only` mount rejects writes, while writes to a `read_write` mount produce
  memory versions attributed to the session." The prompt's wording is published nowhere.
- Limits (memory guide): 8 stores per session; 2,000 memories per store ("writes to new
  memories fail: both direct `memories.create` calls and the agent's file writes to
  unmapped paths. Existing memories remain readable and editable"); `access` "defaults to
  `read_write`"; "memory stores can only be attached at session creation time; adding or
  removing one from a running session is not supported."
- Lifecycle: "Archiving makes a store read-only and prevents it from being attached to
  new sessions. Archiving is one-way; there is no unarchive." Delete "permanently
  remove[s] a store along with all of its memories and versions." "Create does not
  overwrite; to change an existing memory, use `memories.update`." "A version that is the
  current head of a live memory cannot be redacted." "There is no dedicated restore
  endpoint; to roll back, retrieve the version you want and write its `content` back."
- Self-hosted (self-hosted-sandboxes guide): the worker "Downloads each attached store to
  its `mount_path` … authenticating with the work item's per-session `secret`", "Adds
  those directories to the file tools' allowed roots, and the directories of stores
  attached with `access: "read_only"` to their read-only roots", "Reconciles local and
  remote changes after tool calls, at most once per sync interval (15 seconds by
  default)", "Runs a final sync when the session ends". "Conflicts resolve in favor of the
  store." "Changes made through `bash` … are not blocked locally: they are never synced to
  the store." "Each store directory contains a marker file named
  `.anthropic-memory-store` … the worker does not sync a directory whose marker is missing
  or altered." "While memory support is enabled, a work item that arrives without a
  per-session `secret` for a session with attached stores fails rather than running
  without memory." The same guide says the `ant` CLI worker "does not mount memory
  stores" — stale for v1.26.1, whose `go.mod` pins SDK v1.66.0 and whose `beta:worker
  poll` runs the SDK worker in-process (`anthropic-cli pkg/cmd/worker.go:87-96`).
- The reference worker's algorithm (`lib/environments/memories.go` at v1.66.0, the only
  visible implementation of the sync): download with `list view=full limit=20`; sync with
  `list view=basic limit=100` then one `GET view=full` per memory to pull; a new local
  file is a `create`, a changed one an `update` with `precondition = the listed sha`, a
  locally deleted one a `DELETE expected_content_sha256 = baseline sha` after a 30 s
  corroboration window; a file changed on both sides takes the server's; a file the
  server refuses (400/413) is skipped until its bytes change; a store attached
  `read_only` "pulls but never pushes"; on 409 the local edit loses without retry; the
  marker's content is `"version 1\n<memory_store_id>"`; the root must not pre-exist;
  Windows hosts are refused (`O_NOFOLLOW`). It never calls store CRUD or the versions
  endpoints and never renames. Its token split (`worker.go:339-603`): the per-item
  credential is the bearer for heartbeat, force-stop, `GET /v1/sessions/{id}`, skill
  reads, the event stream/list/send, and every memory call — poll, ack and the poller's
  own stop keep the environment key; the SDK deletes `X-Api-Key` on those calls, so a
  server that refuses the token on any of them ends the item at its first heartbeat
  (401 is fatal to the lease).

### Platform starting state

`sessions.resources` is wire-shaped jsonb (`0001_init.sql:80`), read by the API
(`sessionresources.go`), the brain (`brain.go:472-517`, `files.go`, `repos.go`), the
executor (`executor.go:858-940`) and the worker (`worker/files.go`), each decoding the
array by `type`; only `resourceID`/`checkResourceID` assume an `id`, and the list
endpoint's cursor is built from the last element's id. The `Sandbox` interface is `ID,
Exec, ReadFile (4 MiB cap), ReadFileStream, WriteFile, WriteFileStream, WriteFiles,
Destroy` — no list, hash or delete primitive; `/mnt` is one writable volume
(`hardening.go:182-193` `WritablePaths`); `Spec` carries no mounts. The toolset's
`resolve` confines nothing (`toolset.go:238-245`) and `bash` is unconfined by
construction. The executor runs `provisionSandbox → materializeSkills → materializeRepos
→ materializeFiles → runTools`, commits results through `commitResults(ctx, sid, results,
settle func(ctx, tx))` (`drain.go:130`), and its reaper takes the session lock before
`captureCheckpoint` + `Destroy`; checkpoint roots are the workdir, the shell state root
and `/mnt/session/outputs` — `/mnt/memory` would not be captured. The brain concatenates
skills → files → repos blocks into `req.System` (`replay.go:39-53`), each best-effort with
a miss counter. Auth lanes are chosen by path in `dispatchAuth` (`server.go:243-310`);
the per-session `gtk_` gate token (`internal/gatetoken`, `0012_session_gate_tokens.sql`)
is the precedent for a hash-at-rest, one-live-per-scope bearer outside `knownPrefixes`.
Handlers learn the caller from `principalFrom(ctx)` (`auth.go:227`): an `apikey_` id on
the machine lane, a `principal_` id on the identity lane. Next migration `0028`
(`wantMigrations = 27`), next plan number 36, STATE.md `Active work: None`.

## Design decisions

1. **Three Postgres tables, no blob.** `memory_stores` (id, name, description,
   metadata jsonb, archived_at, the reserved org/workspace/project columns, created_by,
   timestamps), `memories` (id, memory_store_id → stores `ON DELETE CASCADE`, path,
   content text, content_sha256, content_size_bytes, memory_version_id, timestamps;
   `UNIQUE (memory_store_id, path)`), `memory_versions` (id, memory_store_id → stores
   `ON DELETE CASCADE`, memory_id **without** a foreign key — versions outlive their
   memory by contract —, operation, path, content, content_sha256, content_size_bytes,
   created_by jsonb in the wire's actor shape, created_at, redacted_at, redacted_by
   jsonb). A memory is ≤ 100 KB and a store ≤ 2,000 memories: the largest store is
   ~200 MB of text, which Postgres holds without ceremony, and a version row carries its
   content in full so a redaction is one `UPDATE … SET content = NULL, path = NULL …`.
   Rejected: content in the blob store (`blob.FilesKey`'s namespace) — a second store to
   keep consistent under the session row lock, for objects a hundred times smaller than
   the files it was built for.
2. **Prefixes `memstore`, `mem`, `memver` join `knownPrefixes`**, CLAUDE.md's prefix line
   and `id_test.go`; `drm` does not (scope decision 3 — adding it would make every `/v1`
   path accept a shape nothing serves).
3. **Store lifecycle follows vaults.** Archive is the idempotent `archived_at =
   COALESCE(archived_at, now())` returning the store; any mutation of an archived store —
   update, memory create/update/delete, redact — is a 400 `"memory store %s is archived"`
   (the vault rule; the reference states only "read-only"), and an archived store cannot
   be attached (400, the `validateAttachedVaults` shape). Delete is a hard delete with
   the `memory_store_deleted` tombstone, cascading memories and versions by FK. A store
   still referenced by sessions' `resources[]` may be deleted (the files precedent,
   DIVERGENCES' files entry): the element stays in the jsonb, later materialization
   records the missing store per resource (decision 10), and the sync skips it. Rejected:
   a RESTRICT (the environments precedent) — it would need a containment scan in the
   delete handler and would pin a store to every archived session that ever mounted it.
4. **Memory writes are one transaction: head row + version row.** Create at an existing
   path → 409 `memory_path_conflict_error {conflicting_memory_id, conflicting_path}`
   (the type is typed with those fields; its status is unrecorded — INFERRED); rename
   onto an existing path → the same. Update with a mismatched precondition → 409
   `memory_precondition_failed_error`, with the documented 200 short-circuit when the
   stored `content` and `path` already equal the request's; delete with a mismatched
   `expected_content_sha256` → the same 409 (the reference worker accepts 409 or 412;
   INFERRED). A no-op update (same content, same path) appends no version and returns
   the head unchanged. Path validation: leading `/`, ≥ 1 non-empty segment, no `.`/`..`
   segments, no control or format characters (Unicode `Cc`/`Cf`), NFC-normalized
   (`golang.org/x/text/unicode/norm`, already an indirect dependency, is the one new
   direct import), ≤ 1,024 bytes; content ≤ 102,400 bytes and valid UTF-8 → 400
   otherwise (the reference has a 413 `request_too_large`; whether oversized *content*
   is a 400 or a 413 is unrecorded — the worker treats both alike — INFERRED 400). The
   2,001st memory → 400 `"memory store %s holds 2000 memories"`. Two new error
   constructors beside `errConflict` carry the memory-specific types; the envelope is
   `writeError`'s.
5. **Listing.** Memories list in byte-wise path order with a keyset cursor on `path`
   (the reference says "a stable, server-defined order" and "path order" for the
   interleaving — INFERRED byte order); `depth=1` rolls descendants below
   `path_prefix + <first segment>/` into one `memory_prefix` item per segment, in one
   query (a `CASE` on whether the remainder after the prefix contains `/`, `DISTINCT` on
   the rolled path), other `depth` values 400; `path_prefix` omitted means `/`; `view=full`
   caps `limit` at 20 silently (documented). Versions list newest-first by `(created_at,
   id)` descending, default `limit` 20 (unstated — INFERRED from the sibling lists);
   stores newest-first. Cursors are this platform's `base64url("k1|…")` keyset tokens,
   not `page_…` values — opaque to every client (registered as a note). `include_archived`
   and `created_at[gte|lte]` reuse the vault list's helpers.
6. **Actors.** A management write records `api_actor {api_key_id: <apikey_ id>}` when a
   machine key authenticated it and `user_actor {user_id: <principal_ id>}` when a human
   did (`principalFrom`'s two lanes — the reference's `user_` prefix is its Console's,
   ours is `principal_`, registered); a sync write records `session_actor {session_id}`
   with no thread id (the shape has none; plan 35's shared sandbox makes every thread's
   write the session's). `service_account_actor` is never emitted. Redaction (RoleAdmin —
   it is a compliance action) refuses the head version of a live memory with a 400
   (status unrecorded — INFERRED) and records `redacted_by` the same way.
7. **The attachment element has no id.** A `memory_store` element in `sessions.resources`
   is `{type, memory_store_id, access, instructions, name, description, mount_path}` —
   the response variant verbatim, `name`/`description`/`mount_path` snapshotted in the
   create transaction from the store row read `FOR SHARE`. `access` omitted is stored and
   echoed as `"read_write"` (the documented default; whether the reference echoes the
   string or `null` is unrecorded — INFERRED); `instructions` omitted is `null`. Create
   rejects with a 400: an unknown store ("memory store %s not found"), an archived one,
   more than 8 stores, the same store twice, two stores whose slugs collide, `instructions`
   over 4,096 chars, and — until slice 6 — any store on a `self_hosted` environment
   (scope decision 2; the environment's `kind` is already read `FOR SHARE` at
   `sessions.go:583`). `GET`/`DELETE /v1/sessions/{id}/resources/{rid}` cannot name an
   id-less element, so they keep answering the shape-check 404 (INFERRED — the
   reference's answer is unobserved); `POST …/resources` keeps its files-only rule; the
   list endpoint builds its cursor from the last element *with* an id and never emits
   one derived from a memory element (a latent paging bug the id-less element would
   trigger). `GET /v1/sessions?memory_store_id=` becomes real: a shape-validated id and
   `resources @> '[{"type":"memory_store","memory_store_id":$1}]'` (the
   `fileMountedInEnvironment` precedent), no index until a list needs one.
8. **Mount path.** `mount_path = "/mnt/memory/" + slug(name)` where `slug` lowercases,
   collapses every non-`[a-z0-9]` run to one `-`, and trims leading/trailing hyphens; a
   name with no alphanumerics slugs to the store id's token (ours — the reference's
   behavior is unrecorded). The slug is computed at attach time from the snapshotted
   name (renames change later sessions only — documented). A file or repository mount
   at or below `/mnt/memory` is refused by the existing `mountPathTaken`/`repoMountBelow`
   family, so the memory root is never shadowed.
9. **The brain's "Memory stores" block** sits after the repositories block in
   `buildRequest` (placement INFERRED, like the other three): a lead line and one entry
   per attached store — display name, mount path, access mode, the snapshotted
   description and, when set, the instructions verbatim — assembled from the `resources`
   jsonb the turn already holds, with `memory.injected`/`memory.block_chars` span
   attributes. The wording is ours (INFERRED; the recording checklist's first item); it
   states the mount rules the reference documents (read the directory before starting,
   write only under the mount path, a `read_only` store refuses writes) and nothing a
   harness invents — the customer's `instructions` field is the extension point. Until
   slice 6 the block renders on `cloud` sessions only (the repos-block precedent); slice
   6 removes the gate. A store deleted after attach still renders from the snapshot,
   suffixed with the hedge the repos block uses for a failed clone ("NOT AVAILABLE: the
   memory store no longer exists").
10. **Cloud materialization is the files pattern, sourced from the store.** On every
    `tool_exec` run, after `materializeFiles`, `materializeMemory` writes each attached
    store's memories to `<mount_path>/<path>` and the marker
    `<mount_path>/.anthropic-memory-store` = `"version 1\n<memory_store_id>"` (the
    reference worker's marker byte-for-byte, so a sandbox the reference could inspect
    looks the same), skipping a store whose marker is present and matches (one `test -e`
    chain, the files precedent) — the sync (decision 11) then reconciles instead of
    re-downloading. Files are written with mode `0666` and directories `0777` so a
    sandbox running under `SANDBOX_RUN_AS_USER` can write what root materialized. A
    missing store row is a logged, counted miss (`memory.resolve.misses`), never a failed
    run: the directory is not created and the brain's hedge (decision 9) tells the agent.
    A restored checkpoint has no `/mnt/memory` (plan 24's roots are unchanged — the store
    is the source of truth, so nothing needs capturing); the next run re-materializes.
11. **Sync-back runs at the end of every `tool_exec` run and in the reaper, in the
    results transaction.** After `runTools` and before `commitResults`, for each attached
    store: one `Exec` — `cd <mount> && find . -type f ! -name .anthropic-memory-store
    -print0 | sort -z | xargs -0 sha256sum` (coreutils the images already provide, as
    `tar`/`test` are; output bounded by `MaxOutputBytes`, which fits 2,000 entries with
    room) — gives the local tree; the baseline is `session_memory_sync (session_id,
    memory_store_id, path, content_sha256, refused_sha256)` — the `{path → sha}` "as of
    the last sync" the reference keeps in-process, kept in Postgres here because
    executors are stateless and any of them may run the next item; the remote state is
    the store's head rows. The decision table is the reference worker's, applied inside
    `commitResults`'s `settle` so a tool result and the versions it produced commit
    together: local changed & remote unchanged → `ReadFile` and write head + version
    (`session_actor`, `created`/`modified`); remote changed & local unchanged → write the
    file; changed on both sides → the store wins, the file is overwritten and the
    conflict counted; locally deleted & remote unchanged since baseline → head deleted
    and a `deleted` version appended — at the next sync, with no 30 s corroboration
    window: the run boundary is already a quiescent point (every tool call has returned;
    the platform's write tool has no temp-file phase), and a background process still
    writing is the same accepted residual `bash` already is; remotely deleted & local
    unchanged → the file is removed; remotely deleted & locally edited → re-created
    (writable store) or kept unsynced (read-only). A `read_only` or archived store, and a
    store whose marker is missing or altered, is pulled from and never pushed to.
    Content the store refuses (non-UTF-8, > 102,400 bytes, an invalid path segment, the
    2,001st memory) is skipped, warned once and remembered in `refused_sha256` until the
    bytes change — the reference's `refusedSHAs`. The reaper runs the same sync before
    `captureCheckpoint` + `Destroy`, so nothing a tool wrote after its run returned is
    lost with the container. Cross-session visibility is therefore one run of each
    session, against the reference cloud's "almost immediately" (registered). Rejected:
    intercepting `write`/`edit` in the toolset and committing each call as a version —
    it misses `bash` and `mv`, which the reference's live mount captures; a FUSE/network
    mount — a new backend obligation on both sandbox providers for a directory ≤ 200 MB.
12. **`read_only` on cloud is enforced by the file tools, exactly as the reference's own
    worker enforces it.** `toolset.Runner` gains `ReadOnlyRoots` and `MemoryRoot`: `write`
    and `edit` refuse a path inside a read-only store's directory with the reference
    worker's wording ("%s is inside read-only directory %s") and any path under
    `/mnt/memory` outside an attached store's directory ("writes to any other path under
    `/mnt/memory/` fail"); `bash` stays unconfined, its writes to a read-only store never
    sync and the next remote change overwrites them (documented for self-hosted; the
    reference cloud's filesystem-level enforcement is a divergence we register — a
    per-store read-only bind mount would be new `Spec` plumbing on both backends for a
    guardrail the reference itself calls "a guardrail for the file tools only, not a
    sandbox"). The executor and the worker both set the roots from the session's
    `resources`.
13. **Threads share the session's stores.** One sandbox per coordinator session (plan 35),
    so every thread sees the same mounts; serial tool execution across siblings keeps the
    run-end sync race-free; the actor is the session's. Nothing per thread is added.
14. **Roles**: reads RoleViewer; store create/update/archive/delete and memory
    create/update/delete RoleDeveloper; redact RoleAdmin.
15. **The sessions token: minted per work item, hashed at rest, accepted on the
    reference worker's whole matrix** (slice 5). `Poll` mints a `wtk_`-prefixed token
    (the `gtk_` shape: 32 random bytes, Crockford base32, outside `knownPrefixes`) in the
    claim transaction, stores `sha256` in `work_session_tokens (id, work_id, session_id,
    environment_id, token_hash, revoked_at)` — one live row per work item, revoked when
    the item leaves `leased`/`running` (ack, stop, lease expiry, reclaim mints afresh) or
    the session is archived — and renders `secret = base64url(JSON {"sessions_token":
    "<wtk_…>"})` **on the poll response only**; every other path keeps `null` (the SDK's
    "null on all other retrieval paths"). A new lane middleware resolves a `wtk_` bearer
    to `(work_id, session_id, environment_id)` and admits it where the v1.66.0 worker
    sends it: `POST …/work/{work_id}/heartbeat|stop` for its own item, `GET
    /v1/sessions/{id}` and the session's events list/stream/send for its own session,
    the skill read paths (workspace-global, as the env key is), and the memory routes for
    stores its session attaches (a containment check on `resources`, the files lane's
    scoping). On the memory routes the environment key is refused with a 401 (the
    reference "reject[s] the environment key"; its status is unrecorded — INFERRED) and a
    management key is accepted; on every other admitted path the environment key keeps
    working (our worker and the poller's own stop use it). Lane selection stays by path
    and token shape: a `wtk_` bearer is a base32 token with no dots, so `LooksLikeJWT`
    never misroutes it, and `dualAuth` tries the work token before the environment key.
    #165's vault-credential bundle would be another key in the same JSON envelope; the
    issue is re-scoped to say so and stays open. Rejected: keeping `secret` null and
    admitting the environment key on the memory routes — the worker fails the item
    before its first memory call (ground truth), and a per-item token is also narrower
    than an environment key that today reads every session in its environment.
16. **The BYOC worker runs the same sync over the wire** (slice 6). `internal/worker`
    decodes the item's `secret`, and `SetupMemory` — the twin of decision 10 — lists each
    store with `view=full limit=20` through the typed SDK and writes files and the marker
    into the sandbox it provisioned; after each tool call it runs the decision table with
    the platform's `memsync` package (decision 17) against a `view=basic limit=100` listing
    plus per-memory `GET view=full`, uploading with `precondition = the listed sha`,
    deleting with `expected_content_sha256 = baseline sha`, and treating 409 as "the
    local edit loses" and 404 on update as "re-create" — the reference's status handling,
    which the platform's routes produce. Its baseline lives in the worker process, as the
    reference's does (one worker serves one item start to finish). A missing token with
    stores attached fails the item the way the reference does (`ErrSessionMemoryNoToken`'s
    text), so the two workers agree on what a session without memory looks like. The
    `self_hosted` 400 (decision 7) is lifted, the brain's gate (decision 9) removed, and
    the real `ant beta:worker poll` (v1.26.1, SDK v1.66.0) serves a session with a store
    against this server unchanged — the slice's acceptance.
17. **`internal/memsync` holds what both halves share**: the slug, the marker bytes, the
    tree-hash command and its parser, the path and content validation the API also uses,
    and the pure decision table `Plan(local, baseline, remote) → actions` — a logic
    package under coverage, with the reference worker's nine-case matrix as its contract
    test. The DB side (executor) and the wire side (worker) apply the actions.
18. **Telemetry.** Executor/worker: `memory.materialized` (outcome-labelled),
    `memory.sync.duration`, `memory.sync.actions` (`action ∈ pulled|pushed|deleted|
    conflict|refused`), spans `memory_materialize`/`memory_sync` with
    `memory.stores`/`memory.changed` attributes; brain: `memory.injected`,
    `memory.block_chars`, `memory.resolve.misses`; API: `memory.stores` and
    `memory.memories` request metrics with outcome-only labels. Ids ride structured
    logs, never labels.
19. **Evals.** Two opt-in trials join `evals/`: `memoryRecall` — a passphrase seeded into
    a store through the API is read from `/mnt/memory/<slug>/…` by the agent (the
    `fileAnswer` shape: the token appears in no prompt); `memoryWrite` — the agent is told
    to record a fact, and after the session idles the store holds a memory whose head
    version is a `session_actor` write. The pinned task count moves 17 → 19 in
    `TestTaskSetIsPinned`, README and ARCHITECTURE.

## Slices

Ordered so no landed slice leaves an incoherent state; each is one PR through the full
ritual. Lifecycle per CLAUDE.md: slice 0's PR flips this plan to `in-progress` and takes
over STATE.md; **every slice lands its DIVERGENCES entries in the PR that introduces the
behavior**; the last slice archives the plan and closes #52.

0. **SDK bump v1.63.1→v1.66.0** (scope decision 1; the plan-05/11 ritual): pin, pairwise
   diffs of every mirrored file, `acceptance/dcf_test.go`'s one type rename, the 48 live
   labels advanced, the `secret` entry's evidence clause corrected in place (the v1.66.0
   worker reads it), a CONFIRMED entry for `service_account_actor` (never emitted) and
   for the v1.66.0 `AgentToolConfigUnion`/`BetaFileMetadata` renames as far as they touch
   mirrored shapes, HISTORY record.
1. **Stores** (decisions 1–3, 5, 6, 14): migration `0028_memory_stores.sql`
   (`memory_stores` only), the three prefixes, `/v1/memory_stores` create/get/update/
   list/delete/archive with the vault handler idiom, `created_by`, the metadata patch,
   the archived-mutation 400. `ant beta:memory-stores *` works.
2. **Memories and versions** (decisions 1, 4–6, 14): migration `0029_memories.sql`
   (`memories`, `memory_versions`, the unique path index, the `(memory_store_id,
   created_at DESC, id DESC)` versions index), the five memory routes with `view`,
   precondition and `expected_content_sha256`, the list with `path_prefix`/`depth`/prefix
   rollups, the three version routes, actors, the two error types, the 2,000 cap. `ant
   beta:memory-stores:memories *` and `:memory-versions *` work; the CLI's own suite
   (`TEST_API_BASE_URL=… go test ./pkg/cmd -run 'TestBetaMemoryStores'`) passes against
   the server.
3. **Attachment, filter and prompt** (decisions 7–9, 13): the `memory_store` arm of
   `parseResourceObject` accepted on `cloud` sessions, the snapshot and slug, the seven
   400s, the id-less element through list/get/delete and the cursor fix, the
   `memory_store_id` filter, `mountPathTaken` for `/mnt/memory`, the brain block on
   `cloud`, the fixture prefixes in `sessions_test.go` corrected, DIVERGENCES `:43`
   carved down a third time and `:44` rewritten in place. Inert in the sandbox: the
   agent is told about a directory slice 4 fills.
4. **Cloud materialization and sync** (decisions 10–12, 17–19): `internal/memsync`,
   migration `0030_session_memory_sync.sql`, `materializeMemory`, the run-end and reaper
   syncs inside `commitResults`, `Runner.ReadOnlyRoots`/`MemoryRoot` and the two tool
   refusals, telemetry, the two evals, an end-to-end integration test on the docker
   sandbox (attach → materialize → tool write → version row → second session sees it).
   This slice meets #52's cloud acceptance.
5. **The sessions token** (decision 15): migration `0031_work_session_tokens.sql`,
   `internal/worktoken` (the `gatetoken` shape), the poll-time mint and `secret`
   rendering, the lane middleware and its admission matrix, the memory routes' env-key
   refusal, `internal/worker` reading the secret (using the token only where slice 6
   needs it), #165 re-scoped. Behavior-neutral for sessions without stores: the v1.66.0
   worker's heartbeat/stop/session/skills calls now carry the token and succeed as they
   did with the key.
6. **BYOC memory** (decision 16): `SetupMemory` and the worker's run-end sync, the
   `self_hosted` 400 lifted, the brain gate removed, the `ErrSessionMemoryNoToken`
   twin, an integration test against the in-process API server, and the acceptance
   transcript of the real `ant beta:worker poll` serving a session with a store. This
   slice meets #52's `self_hosted` acceptance.
7. **Close-out**: HISTORY acceptance + review-hardening records and the progress summary,
   ARCHITECTURE "Execution flow"/"Wire-compatibility model"/package rows and a
   security-invariant paragraph (what the sessions token can reach, and that `read_only`
   is a tool guardrail), README status + "what runs" + roadmap line, plan → `archived`,
   STATE.md → "None.", #52 closed.

## Verification

Every guard is shown red against code without it before its slice is done (plan 25's
mutation duty); the matrix rows are `make test` integration tests unless marked 🔍
(a probe on the live stack) or CLI (a recorded `ant` transcript).

- Slice 0: `make verify` green in WSL on the bumped pin; the citation re-read tallied in
  the HISTORY record; the registry's `v1.63.1` labels advanced (`grep` count 0 outside
  historical records).
- Slice 1: API contract tests per route — name/description/metadata bounds each a 400;
  the metadata patch (upsert, null-delete, omit-keep); `include_archived`, the inclusive
  date bounds and keyset paging; archive idempotent, update-after-archive 400, delete
  tombstone; `created_by` on both auth lanes; the 405 fallbacks; `TestEveryIdentity
  ReachableRouteDeclaresARole`. CLI: `ant beta:memory-stores create|retrieve|update|
  list|archive|delete`.
- Slice 2: path validation table (each documented rejection its own case, an NFD path
  among them); content bounds and UTF-8; `view` defaults per endpoint and the `full` cap;
  `depth=1` rollups interleaved in path order across a page boundary; `path_prefix`
  segment alignment (`/notes/` excludes `/notes-archive/`); precondition 409 and the 200
  short-circuit; rename-only appends `modified`; delete precondition 409, 404 after;
  version rows per operation with nulls as specified; redact nulls the four fields, head
  refused; the 2,000 cap; the actors on both lanes; store delete cascades. CLI: the §7
  command list from the CLI research (recorded into HISTORY); the CLI's own
  `TestBetaMemoryStores*` suite against the server.
- Slice 3: the seven create-time 400s; the element shape (no `id`, `access` echoed,
  `mount_path` derived, snapshot survives a later rename); list paging with a memory
  element last on a page; get/delete by rid 404; the filter's containment (a session
  attaching two stores matches both ids; a deleted store still matches); the brain block
  on `cloud`, absent on `self_hosted`, hedged for a deleted store; `mountPathTaken` under
  `/mnt/memory`. CLI: `ant beta:sessions create --resource '{type: memory_store, …}'`,
  `beta:sessions:resources list`, `beta:sessions list --memory-store-id`.
- Slice 4: `memsync.Plan`'s nine-case matrix; materialization idempotence via the marker
  and the miss for a deleted store; the docker integration: a tool write → head + version
  (`session_actor`), a second session's next run sees it, a both-sides change takes the
  store's, a local delete deletes the head and appends `deleted`, a refused file is
  attempted once until it changes, a read-only store's `write`/`edit` refusal wording,
  a `bash` write to a read-only store never syncs and is overwritten by the next remote
  change, writes under `/mnt/memory` outside a store refused, the reaper's sync before
  destroy, a run under `SANDBOX_RUN_AS_USER` can write what root materialized, the
  restored checkpoint re-materializes; 🔍 both evals opt-in green; the telemetry rows.
- Slice 5: token minted once per claim and rotated on reclaim; hash-only at rest; the
  admission matrix (each row admitted, each non-row refused, the env key refused on the
  memory routes with a 401, a management key admitted); a token scoped to one item's
  session cannot read a sibling session in the same environment; revocation on ack/stop/
  expiry/archive; `secret` non-null on poll only; the v1.66.0 SDK worker's first
  heartbeat with the token succeeds (a test built on the SDK's `EnvironmentWorker`
  against the in-process server, with no stores).
- Slice 6: the worker integration (download → tool write → upload with precondition →
  version attributed to the session; 409 loses; 404 re-creates; read-only never pushes;
  missing token fails the item with the reference's text); CLI: the real `ant beta:worker
  poll` serving a session with two stores, one `read_only`, recorded end to end.

## New DIVERGENCES entries (to record as they land)

- (slice 0) `service_account_actor` never emitted (CONFIRMED, ours — no service
  accounts). The `secret` entry's evidence corrected in place: the v1.66.0 worker decodes
  it.
- (slice 1) `/v1/dreams` absent (CONFIRMED, tracked by the new `(post-v1)` issue);
  `memory_store.*` webhooks not delivered (CONFIRMED, #261's sibling); list cursors are
  `k1|…` keyset tokens, not `page_…` (note — opaque either way); the reference's
  400-on-both-beta-headers is not mirrored (note under the existing accept-and-ignore
  rule); versions retained without pruning (CONFIRMED, tracked by the new retention
  issue); archived-store mutation is a 400 (INFERRED — "read-only" without a status);
  `user_actor.user_id` is a `principal_` id (CONFIRMED, ours).
- (slice 2) `memory_path_conflict_error` is a 409 on create-at-existing-path and
  rename-onto-existing-path (INFERRED); delete-precondition mismatch is a 409
  `memory_precondition_failed_error` (INFERRED — the worker accepts 409 or 412);
  oversized/invalid content is a 400 (INFERRED — 413 possible); memories list in
  byte-wise path order, `path_prefix` defaults to `/` (INFERRED); versions list default
  `limit` 20 (INFERRED); redacting the head is a 400 (INFERRED).
- (slice 3) `access` omitted echoes `"read_write"` (INFERRED); the element carries no
  `id` and `GET`/`DELETE …/resources/{rid}` answer 404 for it (INFERRED); the same store
  twice, a slug collision and an all-symbol name are ours (INFERRED — unobserved); an
  archived store attaches with a 400 (INFERRED); a deleted store's attachment is tolerated
  and hedged (INFERRED — the files precedent); the memory block's wording and placement
  (INFERRED); `memory_store` on `self_hosted` is a 400 until slice 6 (CONFIRMED, tracked
  by #52 with a parenthetical); the `memory_store_id` filter entry rewritten in place.
- (slice 4) sync at the run boundary rather than a live mount — cross-session visibility
  is one run, not "almost immediately", and a `bash` write to a read-only store is
  unsynced rather than refused at the filesystem (CONFIRMED, ours); no delete
  corroboration window (CONFIRMED, ours); the marker file matches the reference worker's
  (note).
- (slice 5) `secret` populated on poll as `base64url(JSON {"sessions_token"})` (INFERRED —
  the envelope is read from the reference worker, the reference's other keys are not
  produced); the env key refused on memory routes with a 401 (INFERRED); the token's
  admission matrix (INFERRED from the worker's calls); token per work item, not per
  session (INFERRED from "the bearer for this item's work lifecycle").
- (slice 6) `file` and `github_repository` on `self_hosted` remain accepted here where the
  reference 400s them ("self-hosted environments accept only `memory_store` resources")
  — an existing superset, now stated (CONFIRMED, ours).

## Recording checklist (a Managed Agents key; `ant --format raw` on every call)

In priority order — each settles entries above:

1. The system prompt's memory section: a session with two stores (one `read_only` with
   `instructions`) whose first `user.message` asks the agent to print, verbatim, every
   part of its system prompt that mentions `/mnt/memory`.
2. `GET /v1/sessions/{id}` and `…/resources` with a store attached: `access` when omitted
   (`"read_write"` or `null`), whether `mount_path` is ever `null`, the element's key set.
3. `GET`/`DELETE …/resources/{rid}` naming a memory element, if any handle exists.
4. `memories.create` at an existing path, and a rename onto one: status and error type.
5. `DELETE …/memories/{id}?expected_content_sha256=<wrong>`: 409 or 412, and the type.
6. `POST …/memory_versions/{head}/redact`: status and type.
7. Two attached stores named "Notes" and "notes"; the same store attached twice.
8. `GET …/memory_versions` with no `limit`: the page size; a `next_page` value's shape.
9. Content of 102,401 bytes on create: 400 or 413.
10. A `secret` from a real poll (redacted): its key set beyond `sessions_token`.

Every entry marked INFERRED above maps to one of these items; entries marked *ours* are
platform choices no recording can confirm or refute, and have none by design.

## Known consequences, not fixed here

- **Version growth is unbounded** (scope decision 5): a chatty agent writing a 100 KB
  memory every turn adds 100 KB per turn to `memory_versions`. The retention issue names
  the rule to implement (30 days, newest N kept) once N is known or chosen.
- **Cross-session freshness is one run.** Two cloud sessions editing the same memory
  concurrently converge at their next runs with the store winning — the reference's
  documented self-hosted behavior, slower than its cloud mount.
- **The sessions token widens `Poll`'s response.** Any environment-key holder polling
  receives a bearer for the claimed item's session — the reference's model, and narrower
  than the key itself; the `--on-work` script path pipes it to stdin, as the reference
  documents.
- **`self_hosted` sessions with stores are refused until slice 6**, not degraded — a
  deliberate hard edge so no reference worker fails an item silently in the interim.
- **The dreams issue inherits a design question**: a consolidation pipeline needs a model
  binding (principle 4 — config-driven, never a hosted model), an internal agent, and a
  clone of a store; none of that is reserved here beyond the tables it would read.

## End-to-end acceptance (recorded into docs/HISTORY.md by slices 4 and 6)

Cloud (slice 4): `ant beta:memory-stores create` → `ant beta:memory-stores:memories
create` (a passphrase at `/facts/secret.md`) → `ant beta:sessions create` with the store
`read_write` and `instructions` → a `user.message` asking for the passphrase and to record
today's date at `/log/today.md` → the stream shows `agent.tool_use` reads and writes under
`/mnt/memory/<slug>/` and the final message carries the passphrase → `ant
beta:memory-stores:memories list --view full` shows `/log/today.md` → `ant
beta:memory-stores:memory-versions list --session-id <sesn>` shows its `created` version
attributed `session_actor` → a second session attached `read_only` reads `/log/today.md`
and its write is refused → `ant beta:sessions list --memory-store-id <id>` lists both.

`self_hosted` (slice 6): the same store attached to a session on a `self_hosted`
environment, served by the real `ant beta:worker poll --environment-key …` built from the
v1.26.1 checkout: the poll response carries `secret`, the worker's log shows the store
downloaded to `/mnt/memory/<slug>`, the agent's write uploads with a precondition, and the
version list shows the session's write.
