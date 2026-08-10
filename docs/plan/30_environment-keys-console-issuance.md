---
status: draft
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
2. **Issuance is console-only and off the wire.** The reference mints keys on
   its console's private backend, not the public API (verified live, see Ground
   truth). We mirror that architecture: the endpoints live under a
   **`/console/v1/` namespace** that is not part of the wire-compatible `/v1`
   surface, so a real `ant` CLI or Anthropic SDK pointed at this platform sees
   no foreign endpoints, and no future reference claim on `/v1/environments/…`
   can collide. The managed-agent-console's BFF (which injects the management
   `x-api-key` server-side and never exposes it to the browser) is the intended
   caller. With a single management key there is no way to cryptographically
   restrict the namespace to the console alone; "console-only" means: off the
   wire, designed for the console, documented as such.
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
- Automatic rotation, or any change to how workers *consume* keys — the Bearer
  lane's wire behavior is locked by the real `ant beta:worker` and stays
  byte-identical (the only consumer-visible change: an expired key now fails
  auth exactly like a revoked one).
- Multi-tenant scoping: `org_id`/`workspace_id`/`project_id` stay reserved
  single-tenant defaults on `environment_keys`.

## Ground truth (verified 2026-08-10)

### Reference console behavior — observed live on platform.claude.com

Observed in a real workspace (environment detail → generate → reveal → revoke,
network calls captured; the trial key was revoked immediately):

- Self-hosted environment detail shows an **Environment keys** section: copy
  "An environment key lets a runner on your infrastructure connect to this
  environment and pull jobs. Generate one per host so you can revoke access
  individually." Empty state: "No environment keys yet."
- **Generate** → modal titled "Create environment key" with a single **name**
  field ("Give your environment key a name to help identify it later.",
  placeholder "e.g., Production Server") → success modal "Save your
  environment key": the full key rendered once with a copy button, and "Keep a
  record of the key below. **You won't be able to view it again.**"
- Key format: `sk-ant-oat01-…` (the reference's console-OAuth token family,
  ~100 chars). List columns: **Name | ID | Created | Expires**, with
  **Expires = Created + 1 year** and the ID a truncated UUID. Per-row trash
  icon → confirm dialog: "Are you sure you want to revoke this environment
  key? Workers using this key will no longer be able to connect. This action
  cannot be undone."
- The backing API is the console's private BFF, **not** `api.anthropic.com/v1`:
  `GET`/`POST https://platform.claude.com/api/oauth/organizations/{org-uuid}/environments/{env_id}/tokens`
  and `POST …/tokens/{token-uuid}/revoke` → 204, authenticated by the console's
  OAuth session. After revoke the key vanishes from the list.
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
  download — already implemented here.
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
  in `domain.knownPrefixes` (internal/domain/id.go:53–59) — required once key
  ids appear in URL paths.

## Architecture

### Storage — migration `00NN_environment_keys_named.sql`

(Next free number — 0021 at writing; renumber if plan 29's `mcp_catalogs`
migration lands first.)

```sql
ALTER TABLE environment_keys ADD COLUMN name text NOT NULL DEFAULT '';
ALTER TABLE environment_keys ADD COLUMN expires_at timestamptz;
DROP INDEX environment_keys_one_live;
```

- Many live keys per environment become legal. `key_hash UNIQUE` stays — a key
  value still binds to one environment for life (with server-side CSPRNG
  generation, a conflict is a cryptographic impossibility and is treated as an
  error, never an un-revoke).
- Pre-existing rows are grandfathered: `name = ''` (the console renders "—"),
  `expires_at = NULL` (never expires). Newly issued keys always get both.
- `internal/store/store_test.go:330–380` currently proves the one-live index
  exists and repairs; those pins are replaced by pins on the new columns and on
  many-live coexistence.

### Issuance primitives — `internal/api/envkeys.go`

`EnsureEnvironmentKey` and its rotate-on-mint semantics are **deleted**
(no production caller exists), replaced by:

- `IssueEnvironmentKey(ctx, pool, envID, name) (envKeyRow, plaintext string, error)` —
  generates the secret server-side: `sk-map-env01-` + base64url of 32 CSPRNG
  bytes (43 chars). The silhouette follows the reference (`sk-ant-oat01-…`,
  per plan 01's "format modeled on") but the prefix is deliberately
  platform-own so an issued key can never be mistaken for, or accidentally sent
  as, an Anthropic credential. Nothing anywhere parses the format — it stays an
  opaque Bearer token. Row: `domain.NewID("envkey")`, `expires_at = now() +
  365 days`, SHA-256 hash only; the plaintext exists in memory for the duration
  of the request and is returned exactly once.
- `ListEnvironmentKeys(ctx, pool, envID)` — unrevoked keys, newest first:
  `id, name, created_at, expires_at`. (Expired-but-unrevoked keys are included;
  the console can badge them. Revoked keys vanish, as observed in the
  reference.)
- `RevokeEnvironmentKey(ctx, pool, envID, keyID)` — sets `revoked_at` where
  both ids match. Idempotent on an already-revoked key (mirrors archive's
  idempotency); unknown key id — or a key id belonging to another environment —
  is a uniform 404 (no cross-environment probing oracle).
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

### The console-API surface — `/console/v1`, management-authenticated

| Route | Behavior |
|---|---|
| `POST /console/v1/environments/{id}/keys` | Body `{"name": "…"}` (required, trimmed, 1–128 chars; unknown body keys rejected per house style). 200 → `{"id":"envkey_…","type":"environment_key","name":…,"created_at":…,"expires_at":…,"key":"sk-map-env01-…"}`. **`key` appears here and never again.** |
| `GET /console/v1/environments/{id}/keys` | 200 → `{"data":[{id,type,name,created_at,expires_at},…],"has_more":false}` — the platform's list envelope so the console's existing `Page<T>` type fits; unpaginated (bounded by one-key-per-host issuance). |
| `POST /console/v1/environments/{id}/keys/{key_id}/revoke` | 204. Idempotent when already revoked; 404 for unknown/foreign `key_id`. |

- **Auth**: all three routes take management `x-api-key` (`requireAPIKey`);
  `dispatchAuth` gets a `/console/` classification arm ahead of its default so
  the namespace can never fall into the environment-key or dual lanes; an
  environment key presented here is a 401. The console BFF already injects the
  management key server-side; it never reaches a browser.
- **Routing**: registered on the same mux with Go 1.22 method patterns, plus
  the house 405-fallback entries and the JSON error envelope
  (`errInvalid`/`errNotFound`/`errAuth`, internal/api/errors.go:31–45). `{id}`
  and `{key_id}` are shape-checked via `checkID`; `envkey` joins
  `domain.knownPrefixes`.
- **Edge semantics** (local choices — the reference console's private API is
  not a wire-compat obligation, but these mirror what it shows): unknown
  environment → 404 `not_found_error`; **create on a `cloud` environment → 400
  `invalid_request_error`** (the reference offers no key UI for cloud, and our
  cloud lane doesn't consume keys); create on an archived environment → 400;
  list/revoke work regardless of kind or archive state.
- `/v1` is untouched: the wire-compat rung's surface diff must show zero new
  `/v1` routes.

### Docs that move with the change

- **docs/DIVERGENCES.md** — rewrite the line-53 CONFIRMED entry ("Environment
  worker keys — issuance"): the operator surface now exists as the off-wire
  `/console/v1` namespace consumed by the managed-agent-console; the reference
  keeps issuance on its console's private backend (evidence updated with the
  2026-08-10 live observation: the `…/environments/{id}/tokens[/{id}/revoke]`
  BFF calls, multi-key + named + 1-year-expiry model); one-live rotation
  semantics retired with migration 0013's index. Add the expiry-401 note (local
  behavior, consistent with reference UX, unobservable on the wire).
- **docs/self-hosted-security.md §6** — rewritten: issuance via the console
  (or curl for headless operators), per-host keys, individual revocation,
  the 1-year TTL replacing "no TTL", DB seeding gone.
- **docs/ARCHITECTURE.md** — envauth/envkeys package rows, the `/console/v1`
  namespace under the wire-compatibility model (explicitly: not wire surface),
  migration row, security invariants (expiry added to the scoped-auth bullet).
- STATE.md (Active work + Tasks) flips when slice 1's PR starts the work;
  changelog.d fragment per PR; README's development notes if operator workflow
  text changes; deploy/compose docs lose their DB-seeding instructions.

## Slices

1. **Storage + primitives.** The migration; `IssueEnvironmentKey` /
   `ListEnvironmentKeys` / `RevokeEnvironmentKey`; expiry-aware
   `authenticateEnvironmentKey`; delete `EnsureEnvironmentKey`; rewrite the
   key-invariant/rotation/race tests as above. TDD: the many-live coexistence
   and expiry-401 tests land red first.
2. **The `/console/v1` endpoints.** `dispatchAuth` arm, three routes, 405/404
   fallbacks, `envkey` prefix; HTTP tests in house style (`s.do`/`s.doRaw`):
   management auth required and environment key rejected on every route, every
   error envelope pinned via `wantErr`, and the issued key proven end to end —
   the plaintext from `POST …/keys` polls `GET /v1/environments/{id}/work/poll`
   over real HTTP, then is revoked and polls again to a 401; direct SQL asserts
   hash-only storage.
3. **Docs + acceptance.** DIVERGENCES / self-hosted-security / ARCHITECTURE
   rewrites; compose-stack acceptance run recorded in docs/HISTORY.md: bring up
   `deploy/compose`, issue a key with curl against `/console/v1`, run a real
   `ant beta:worker poll --environment-key … --base-url http://localhost:…`
   until it pulls a work item — the issue's acceptance, with zero DB edits.

Console repo work (its plan 07: BFF prefix allow-list, zod schemas with
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

- Rewritten CONFIRMED "Environment worker keys — issuance" (slice 2): off-wire
  `/console/v1` issuance namespace; named multi-key model with 1-year expiry;
  observed-console evidence; `environment_keys_one_live` retired.
- CONFIRMED note (slice 1): expired keys 401 identically to revoked ones on the
  Bearer lane — reference-unobservable, chosen to avoid an auth oracle.
