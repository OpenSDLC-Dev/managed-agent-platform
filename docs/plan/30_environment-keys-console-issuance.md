---
status: in-progress
issue: "#43"
---

# Environment keys: console-issued, named, revocable (plan 30)

Closes #43 — the BYOC worker credential (`Authorization: Bearer`) exists and is
enforced, but nothing issues one: `EnsureEnvironmentKey` has no caller, and a
self-hosted operator hand-edits Postgres to provision a worker. End state: an
operator generates **named environment keys in the managed-agent-console UI**,
modeled on the reference console's environments page — each key shown exactly
once at creation, individually revocable, expiring one year after issue — and a
real `ant beta:worker` authenticates end to end with a key issued through that
UI, no DB access anywhere. The work spans two repos: this plan covers the
platform half (storage, primitives, a console-API surface); the console half is
[managed-agent-console's plan 07](https://github.com/OpenSDLC-Dev/managed-agent-console),
which consumes what lands here and must trail it.

## Scope decisions settled with the user on 2026-08-10

1. **The key model becomes reference-faithful: many named keys per
   environment.** Each key carries a name and an expiry (created + 1 year) and
   is revoked individually — the reference console's copy is explicit:
   "Generate one per host so you can revoke access individually." This
   deliberately retires the one-live-key-per-environment invariant that
   migration 0013 introduced (`environment_keys_one_live`); 0013's own comment
   anticipated #43 would revisit issuance.
2. **Issuance is console-only, off the wire, and mirrors the reference
   console's own API.** The reference mints keys on its console's private
   backend, not the public API (verified live, see Ground truth). Rather than
   inventing a namespace for our equivalent, the platform serves the observed
   dialect **path-for-path, and field-for-field except where a divergence is
   declared** (four of them, each reasoned in Architecture):

   `/api/oauth/organizations/{organization_id}/environments/{environment_id}/tokens[...]`

   This is a standing convention, not a one-off: any future console-facing
   endpoint (queue stats, etc.) mirrors the reference console's observed path
   the same way — no naming invented per feature. The `{organization_id}`
   segment surfaces the reserved multi-tenant seam in the URL (v1 accepts only
   the reserved org id `default`; anything else is 404). The `oauth` segment
   and the RFC 6749-shaped create response keep the surface compatible with a
   real OAuth flow later (console OIDC is its own effort, console plan 08 —
   the reference's environment key *is* an OAuth access token in its own
   infrastructure, see Ground truth). Today the namespace authenticates with
   the management `x-api-key`, injected server-side by the console's BFF; the
   wire-compatible `/v1` surface is untouched either way. With a single
   management key there is no way to cryptographically restrict the namespace
   to the console alone; "console-only" means: off the wire, designed for the
   console, documented as such.
3. **No CLI issuance mode.** No `controlplane -issue-env-key` run-once flag;
   the console page is the operator surface. (The endpoint is still an HTTP
   endpoint — a headless operator with the management key can curl it, and the
   platform's own tests do — but no first-class CLI surface ships.)
4. Console scope (recorded here, executed in console plan 07): the
   **Environment keys section** (table + generate/reveal-once/revoke dialogs)
   **and** the **"Set up your self-hosted environment" guide panel**, both on
   self-hosted environment detail pages only — matching the reference, whose
   cloud-environment page shows neither. The reference page's work-queue
   Overview stats panel is out of scope (its `work/stats` endpoint rides the
   environment-key auth lane the console does not hold; separate issue).

## Out of scope

- Work-queue statistics in the console (above).
- Key metadata beyond a name: no last-used tracking, no per-key worker
  attribution, no expiry notifications, no configurable TTL.
- `authorization_details` in the create response (see Architecture — recorded,
  deliberately not emitted in v1).
- Automatic rotation, or any change to how workers *consume* keys — the Bearer
  lane's wire behavior is locked by the real `ant beta:worker` and stays
  byte-identical (the only consumer-visible change: an expired key now fails
  auth exactly like a revoked one).
- Multi-tenant behavior behind the `{organization_id}` path segment: the
  segment exists, but v1 recognizes only `default`.

## Ground truth (verified 2026-08-10)

### Reference console behavior — observed live on platform.claude.com

Observed in a real workspace (environment detail → generate → reveal → revoke;
network calls captured, then the full request/response bodies captured from
the page session; every trial key was revoked immediately):

- Self-hosted environment detail shows an **Environment keys** section: copy
  "An environment key lets a runner on your infrastructure connect to this
  environment and pull jobs. Generate one per host so you can revoke access
  individually." Empty state: "No environment keys yet."
- **Generate** → modal titled "Create environment key" with a single **name**
  field ("Give your environment key a name to help identify it later.",
  placeholder "e.g., Production Server") → success modal "Save your
  environment key": the full key rendered once with a copy button, and "Keep a
  record of the key below. **You won't be able to view it again.**"
- List columns: **Name | ID | Created | Expires**, Expires = Created + 1 year,
  ID a truncated UUID. Per-row trash icon → confirm dialog: "Are you sure you
  want to revoke this environment key? Workers using this key will no longer
  be able to connect. This action cannot be undone."
- The backing API is the console's private BFF, **not** `api.anthropic.com/v1`
  — observed shapes, all under
  `https://platform.claude.com/api/oauth/organizations/{org-uuid}/environments/{env_id}`:

  | Call | Observed shape |
  |---|---|
  | `GET …/tokens` | 200 `{"data":[{"id":"<uuid>","name":"…","created_at":"…Z","expires_at":"…Z"},…],"pagination":{"total":N,"limit":100,"offset":0,"has_more":false}}` — unrevoked tokens only, no `type` field on items |
  | `POST …/tokens` body `{"name":"…"}` | 200 `{"access_token":"sk-ant-oat01-…","expires_in":31536000,"authorization_details":[{"type":"ccr_env","actions":["poll","list","stats"],"environment_id":"env_…"}]}` — an RFC 6749 token response with RFC 9396 rich-authorization details; **no id/name/created_at** — the console refreshes the list with a follow-up GET |
  | `POST …/tokens/{uuid}/revoke` (empty body) | 204, empty body; the token then vanishes from the list |

  So in the reference, an environment key literally **is an OAuth access
  token** (`sk-ant-oat01-…`, the console-OAuth family), minted by its OAuth
  infrastructure and scoped to one environment via `authorization_details`.
- The **cloud** environment detail page has no Environment keys section, no
  setup panel, and no queue-stats Overview — those are self-hosted-only UI.
- The setup panel ("Set up your self-hosted environment") walks: generate key →
  `export ANTHROPIC_ENVIRONMENT_KEY='sk-ant-oat01-...'` → install `ant` from
  GitHub releases → `ant beta:worker poll --environment-id "env_…" --workdir
  "/workspace"`.

### SDK/CLI — the key surface does not exist on the wire

- anthropic-sdk-go: the Environments service is exactly create / get / update /
  list / delete / archive (betaenvironment.go:30–146; api.md:640–693); the
  `BetaEnvironment` resource carries no key field (betaenvironment.go:337–377).
  No endpoint mints, lists, rotates, or reveals an environment key — and the
  absence is meaningful, not a codegen gap: where the reference does put
  credential rotation on the wire, Stainless generates it
  (`POST v1/tunnels/{id}/rotate_token`, betatunnel.go:176–189).
- Consumption is `Authorization: Bearer <key>` with `X-Api-Key` explicitly
  deleted (lib/environments/poller.go:58–66, option/requestoption.go:612–618);
  canonical env var `ANTHROPIC_ENVIRONMENT_KEY` (worker.go:171–198). The key
  also authorizes environment-scoped session events, heartbeat/stop, and skill
  download — already implemented here. (Note the observed
  `authorization_details.actions` list is coarser than what the key actually
  authorizes per the SDK — one reason not to cargo-cult it; see Architecture.)
- anthropic-cli: `ant beta:worker poll|run` takes `--environment-key`
  (required; env `ANTHROPIC_ENVIRONMENT_KEY`) (pkg/cmd/worker.go:39,58); no
  command creates or prints one; `ant auth login` mints console OAuth tokens,
  not environment keys. **No format is enforced anywhere** — fixtures use
  arbitrary strings; the key is an opaque Bearer token to every consumer.

### Platform as-is (what this plan changes)

- `EnsureEnvironmentKey(ctx, pool, environmentID, key)`
  (internal/api/envauth.go:29–69): caller supplies the plaintext; transactionally
  revokes every other live key for the environment, then inserts (or un-revokes)
  the SHA-256 hash — rotate-on-mint, **at most one live key per environment**,
  enforced by the partial unique index `environment_keys_one_live`
  (internal/store/migrations/0013_key_rotation_one_live.sql:49). No caller
  exists outside tests — that is #43's gap.
- `environment_keys` (0001_init.sql:150–161): `id, org_id, workspace_id,
  project_id, environment_id (FK CASCADE), key_hash UNIQUE, created_at,
  revoked_at` — **no name, no expiry**.
- Auth dispatch classifies paths before routing (internal/api/server.go:191–229):
  `/v1/environments/{id}/work…` → environment-key lane; everything unmatched →
  management `x-api-key` lane. `authenticateEnvironmentKey`
  (envauth.go:73–82) resolves hash → environment id, `revoked_at IS NULL` only.
- The platform-managed cloud lane does not use environment keys at all
  (internal/executor has no Bearer/EnvironmentKey reference — it consumes the
  queue in-process), so nothing in this plan touches cloud execution.
- `envkey` (the row-id prefix `domain.NewID("envkey")` already assigns) is not
  in `domain.knownPrefixes` (internal/domain/id.go:53–59), and stays out: that
  set is what `checkID` validates every `/v1` path id against, without asking
  which resource the prefix names, so admitting a private identifier there
  would widen the shape all of them accept. The key ids appear only on the
  off-wire `/api/oauth/…` namespace, which brings its own local check.

## Architecture

### Storage — migration `00NN_environment_keys_named.sql`

(Next free number — 0021 at writing; renumber if plan 29's `mcp_catalogs`
migration lands first.)

```sql
ALTER TABLE environment_keys ADD COLUMN name text NOT NULL DEFAULT '';
ALTER TABLE environment_keys ADD COLUMN expires_at timestamptz;
DROP INDEX IF EXISTS environment_keys_one_live;
CREATE INDEX environment_keys_environment_idx ON environment_keys (environment_id);
```

- Many live keys per environment become legal. `key_hash UNIQUE` stays — a key
  value still binds to one environment for life (with server-side CSPRNG
  generation, a conflict is a cryptographic impossibility and is treated as an
  error, never an un-revoke).
- The replacement index is not optional bookkeeping: `environment_keys_one_live`
  was the table's **only** index on `environment_id` (0001 created none), so it
  had been serving the per-environment lookup and the `ON DELETE CASCADE` from
  `environments` as well as the invariant. 0013 declined a companion index
  because the table then held one row per *rotation*; it now holds one per host
  plus revocation history, so take the companion after all
  (`session_gate_tokens_session_idx`, 0012, is the precedent).
- Pre-existing rows are grandfathered: `name = ''` (the console renders "—"),
  `expires_at = NULL` (never expires, and rendered `null` on the wire — see the
  list route below). Newly issued keys always get both. The alternative,
  backfilling an expiry from `created_at`, would retro-expire credentials a
  running worker is authenticating with — a migration must not revoke a live key
  on its way past. The cost is stated rather than hidden: a grandfathered key is
  a caller-supplied value that never had the 256-bit generation guarantee and
  now never expires either, so docs/self-hosted-security.md §6 tells operators
  to reissue and revoke them.
- `internal/store/store_test.go:330–380` currently proves the one-live index
  exists and repairs; those pins are replaced by pins on the new columns and on
  many-live coexistence.

### Issuance primitives — `internal/api/envkeys.go`

`EnsureEnvironmentKey` and its rotate-on-mint semantics are **deleted**
(no production caller exists), replaced by:

- `IssueEnvironmentKey(ctx, pool, envID, name) (plaintext string, error)` —
  returns the secret and nothing else, because the mirrored create response is
  `{access_token, expires_in}` and carries no row metadata: a caller that wants
  the new row's id or timestamps re-reads the list, exactly as the reference
  console does. Generates the secret server-side: `sk-map-env01-` + base64url of 32 CSPRNG
  bytes (43 chars). The silhouette follows the reference (`sk-ant-oat01-…`,
  per plan 01's "format modeled on") but the prefix is deliberately
  platform-own: our platform is not that OAuth infrastructure, and an issued
  key must never be mistaken for, or accidentally sent as, an Anthropic
  credential. Nothing anywhere parses the format — it stays an opaque Bearer
  token. Row: `domain.NewID("envkey")`, `expires_at = now() + 365 days`
  (matching the observed `expires_in: 31536000`), SHA-256 hash only; the
  plaintext exists in memory for the duration of the request and is returned
  exactly once.
- `ListEnvironmentKeys(ctx, pool, envID, limit, offset)` — unrevoked keys,
  newest first: `id, name, created_at, expires_at`, plus a total count.
  (Expired-but-unrevoked keys are included; the console can badge them.
  Revoked keys vanish, as observed in the reference.)
- `RevokeEnvironmentKey(ctx, pool, envID, keyID)` — sets `revoked_at` where
  both ids match. Idempotent on an already-revoked key (mirrors archive's
  idempotency; the dialect's own re-revoke behavior is unobserved); unknown
  key id — or a key id belonging to another environment — is a uniform 404
  (no cross-environment probing oracle).
- `authenticateEnvironmentKey` gains expiry:
  `AND (expires_at IS NULL OR expires_at > now())`. An expired key fails with
  the same 401 "invalid environment key" as a revoked or unknown one — no
  distinguishing oracle on the auth lane, and zero wire-shape change.

Tests to rewrite rather than patch: envauth_test.go's rotation cases become
many-live coexistence cases (two named keys on one environment both poll
successfully; revoking one kills only it — proven over real HTTP);
keyrotation_test.go's one-live race harness becomes a concurrent-issue harness
(N parallel issues all land live; concurrent revoke-vs-poll never 500s);
cross-environment value binding keeps its test via the retained
`key_hash UNIQUE`.

### The console-API surface — the mirrored dialect, management-authenticated

All under `/api/oauth/organizations/{organization_id}/environments/{environment_id}`,
where v1 accepts only `organization_id == "default"` (the reserved org id;
anything else 404s before the environment lookup):

| Route | Behavior |
|---|---|
| `POST …/tokens` | Body `{"name": "…"}` (required, trimmed, 1–128 chars — a local bound, the dialect's is unobserved; unknown body keys rejected per house style). 200 → `{"access_token":"sk-map-env01-…","expires_in":31536000}`. **The plaintext appears here and never again.** No id/name/created_at in the response — the caller refreshes the list, as the reference console does. |
| `GET …/tokens` | 200 → `{"data":[{"id":"envkey_…","name":…,"created_at":…,"expires_at":…},…],"pagination":{"total":N,"limit":L,"offset":O,"has_more":B}}` — unrevoked keys, newest first; `?limit=` (default 100) and `?offset=` honored. `expires_at` is **nullable**: a grandfathered key renders `null` there, and the console must render that as "never" rather than assume a timestamp. |
| `POST …/tokens/{token_id}/revoke` | 204, empty body. Idempotent when already revoked; 404 for an unknown or foreign `token_id`. |

Deliberate divergences from the observed dialect (each recorded in
DIVERGENCES.md):

- **Key prefix** `sk-map-env01-`, not `sk-ant-oat01-` (rationale above).
- **`id` format** `envkey_…`, not a bare UUID — the id is opaque to every
  consumer, and house ID discipline (`domain.NewID`) wins. It does **not** join
  `domain.knownPrefixes`: that set is the wire-compatible prefix list every
  `/v1` path validates against, and this identifier never appears there.
- **`authorization_details` is not emitted** in v1. The observed value is
  RFC 9396 rich-authorization metadata from the reference's OAuth
  infrastructure, and its `actions` list is demonstrably coarser than what the
  key actually authorizes (SDK ground truth above) — emitting a copied claim
  we neither generate nor enforce would be a lie in a security-adjacent field.
  Recorded verbatim; revisit if the platform ever grows real per-key scoping.
- **Error envelope** is the house `{"type":"error",…}` shape (the dialect's
  error bodies are unobserved).

Auth and routing:

- All three routes take management `x-api-key` — with **no** change to
  `dispatchAuth`: every worker/gate/dual-auth predicate keys on a `/v1/…`
  prefix, so an `/api/…` path already falls to the `default:` management lane
  (internal/api/server.go:213–227). Adding an explicit `/api/` arm would be a
  branch for an unreachable state; a test pins that an environment key
  presented on these routes is a 401 instead. The console BFF already injects
  the management key server-side; it never reaches a browser. (The BFF mirrors
  the path too — the console's own `/api/oauth/…` URL proxies to this one;
  console plan 07.)
- Registered on the same mux with Go 1.22 method patterns, plus the house
  405-fallback entries and JSON error envelope
  (`errInvalid`/`errNotFound`/`errAuth`, internal/api/errors.go:31–45). `{id}`
  is shape-checked via `checkID`; `{token_id}` gets a **local** check in
  `internal/api` instead, because `checkID` validates shape against
  `domain.knownPrefixes` without asking which resource the prefix names — so
  admitting `envkey` there would widen the id shape every `/v1` path accepts
  for a private identifier. `envkey_` stays out of the wire-prefix set,
  following the `apikey_`/`gtk_` precedent (docs/ARCHITECTURE.md §
  internal/gatetoken).
- **Edge semantics** (local choices, dialect-unobserved): unknown environment →
  404 `not_found_error`; **create on a `cloud` environment → 400
  `invalid_request_error`** (the reference offers no key UI for cloud, and our
  cloud lane doesn't consume keys); create on an archived environment → 400;
  list/revoke work regardless of kind or archive state.
- `/v1` is untouched: the wire-compat rung's surface diff must show zero new
  `/v1` routes. Future console-facing endpoints follow this same convention —
  mirror the reference console's observed path under `/api/…`, record the
  observation date and any divergence.

### Docs that move with the change

- **docs/DIVERGENCES.md** — rewrite the line-53 CONFIRMED entry ("Environment
  worker keys — issuance"): the operator surface now exists as the off-wire
  console-API dialect above, mirrored from the reference console's private
  backend (2026-08-10 observation: paths, list/create/revoke shapes, the
  OAuth-token nature of the reference's keys, multi-key + named + 1-year
  expiry); one-live rotation semantics retired with migration 0013's index.
  Add the four dialect divergences and the expiry-401 note (local behavior,
  consistent with reference UX, unobservable on the wire).
- **docs/self-hosted-security.md §6** — rewritten: issuance via the console
  (or curl for headless operators, example against `/api/oauth/organizations/default/…`),
  per-host keys, individual revocation, the 1-year TTL replacing "no TTL", DB
  seeding gone.
- **docs/ARCHITECTURE.md** — envauth/envkeys package rows, the console-API
  dialect under the wire-compatibility model (explicitly: not wire surface;
  mirrored-from-observation convention stated), migration row, security
  invariants (expiry added to the scoped-auth bullet).
- STATE.md (Active work + Tasks) flips when slice 1's PR starts the work;
  changelog.d fragment per PR; README's development notes if operator workflow
  text changes. Each doc moves in the slice whose code invalidates it, not in a
  batch at the end (CLAUDE.md's docs-move-with-code rule): slice 1 rewrites the
  rotation semantics everywhere they are claimed, slice 2 adds the endpoint.
  The DB-seeding instructions live only in docs/self-hosted-security.md — there
  are none under deploy/ to remove.

## Slices

1. **Storage + primitives.** The migration; `IssueEnvironmentKey` /
   `ListEnvironmentKeys` / `RevokeEnvironmentKey`; expiry-aware
   `authenticateEnvironmentKey`; delete `EnsureEnvironmentKey`; rewrite the
   key-invariant/rotation/race tests as above. TDD: the many-live coexistence
   and expiry-401 tests land red first.
2. **The console-API endpoints.** The three `/api/oauth/…` routes, their
   405/404 fallbacks, the local key-id check and the org-segment gate; HTTP
   tests in house style (`s.do`/`s.doRaw`): management auth required and an
   environment key rejected on every route, `organization_id != "default"`
   404s, every error envelope pinned via `wantErr`, a grandfathered row's
   `expires_at` rendering `null`, and the issued key proven end to end — the
   `access_token` from `POST …/tokens` polls
   `GET /v1/environments/{id}/work/poll` over real HTTP, then is revoked and
   polls again to a 401; direct SQL asserts hash-only storage. Its own doc
   updates ride with it (the DIVERGENCES entry for the dialect, the endpoint in
   ARCHITECTURE and self-hosted-security).
3. **Acceptance + archive.** The compose-stack acceptance run recorded in
   docs/HISTORY.md: bring up `deploy/compose`, issue a key with curl against
   the dialect, run a real `ant beta:worker poll --environment-key …
   --base-url http://localhost:…` until it pulls a work item — the issue's
   acceptance, with zero DB edits — then flip this plan to `archived`. Slices 1
   and 2 each carry the doc rewrites their own code invalidates, so nothing
   doc-shaped waits for this slice.

Console repo work (its plan 07: an `/api/oauth/[...path]` BFF passthrough so
the console's own URLs mirror the reference verbatim, zod schemas with
platform file:line cites, `["environment-keys", envId]` hooks, the keys
section + reveal-once dialog + setup panel on self-hosted detail pages, mock
platform routes, e2e/fidelity) starts after slice 2 merges and is tracked
there, not here.

## Verification

- Per slice: `make verify` (no cached results), verifier subagent before any
  done-claim, per CLAUDE.md.
- Wire-compat rung: `/v1` route table diff is empty; the worker lane's behavior
  change is invisible to a compliant worker (expired ⇒ the existing 401 shape).
- Slice 3's acceptance transcript (real `ant` CLI, console-issued key) is the
  plan's exit criterion, recorded in docs/HISTORY.md.

## New DIVERGENCES entries (to record as they land)

- Rewritten CONFIRMED "Environment worker keys — issuance" (slice 1, extended
  in slice 2): the named multi-key model with its 1-year expiry and per-key
  revoke; `environment_keys_one_live` retired; then the console-API dialect
  (`/api/oauth/organizations/{org}/environments/{id}/tokens[…]`) mirrored from
  the reference console's observed private backend, dated 2026-08-10, with the
  four deliberate dialect divergences (key prefix, id format, no
  `authorization_details`, house error envelope).
- CONFIRMED note (slice 1): expired keys 401 identically to revoked ones on the
  Bearer lane — reference-unobservable, chosen to avoid an auth oracle — plus
  the grandfathered NULL expiry the migration deliberately does not backfill.
- INFERRED entry (slice 2), tracked on #43's follow-up: the dialect's edges the
  live observation could not reach, each answered locally and each a guess
  about the reference until a recording says otherwise — whether an
  expired-but-unrevoked key stays listed, whether a repeated revoke is
  idempotent or 404s, the name's length bounds and unknown-body-key rejection,
  what a `cloud` or archived environment answers, and the error-body shape.
  Registered together rather than left implicit: an unrecorded divergence is a
  finding, and "we chose this because nothing was observed" is exactly what the
  INFERRED section exists to hold.
