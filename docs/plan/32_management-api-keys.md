---
status: in-progress
issue: "#378"
---

# Management API keys: named, expiring, console-issued (plan 32)

Closes #378, the slice plan 31 could not ship. Today the management credential is
a single `x-api-key` seeded from `CONTROLPLANE_API_KEY` at boot: one live key per
name, no expiry, no way to issue a second one, and no way for an operator to see
what exists. `api_keys` (migration 0001) already reserves `org_id`/`workspace_id`/
`project_id` at `'default'` and already mints `apikey_`-prefixed ids, so the seam
is built — what is missing is a lifecycle and a surface.

After this plan an **admin** issues named, expiring management keys over the
console API, sees them listed with a masked hint, disables and re-enables them,
and archives the ones that are done — the reference's own model, observed on the
live product rather than inferred.

## Ground truth (verified 2026-08-13, live)

Recorded against `platform.claude.com` with `window.fetch` hooked, five probe keys
created and archived, `sk-ant-…` values redacted at capture. Full transcripts in
#378's comments. What it settled, including four things that contradict what the
public docs alone would have suggested:

1. **There is no key-creation endpoint on the public API, deliberately.** The
   Admin API FAQ: "new API keys can only be created through the Claude Console for
   security reasons." The API reference lists only Retrieve, List and Update.
   Creation is a console-private concern — exactly where plan 30 already put
   environment-key issuance.
2. **Creation takes an absolute instant, or nothing.** `POST …/api_keys` with
   `{"name":…,"expires_at":"<ISO-8601 UTC>"}`; the *Never* choice **omits the field**
   rather than sending null. The dialog's "3 hours / 30 days / Custom N units" is
   entirely client-side sugar — there is no duration vocabulary on the wire.
3. **The secret comes back as `raw_key` on the created resource** — not as a token
   envelope. This differs from the environment-key dialect plan 30 mirrored
   (`{access_token, expires_in}`); the reference runs two different shapes on two
   different surfaces, so we mirror each where it belongs.
4. **Nothing is hard-deleted.** `status` is `active` | `inactive` | `archived`
   (the server's own 400 says so), plus a derived, unsettable `expired`. The UI's
   *Delete* is `{"status":"archived"}`, and archived rows are still returned by the
   API — the console filters them client-side.
5. **Names are not unique.** Two live keys named `map-probe-dup` were created back
   to back, both 200.
6. **The console list is a bare JSON array** — no envelope — while the public
   `GET /v1/organizations/api_keys` returns `{data, first_id, has_more, last_id}`.
7. **The error envelope is already ours**: `{type, error:{type,message}, request_id}`
   with a `req_` id, which `internal/api/errors.go` renders today.
8. Row shape: `id · type · name · workspace_id · created_at · created_by ·
   partial_key_hint · status · expires_at · principal · can_manage`. A
   `GET …/api_keys/policy` also exists (`max_api_key_age_hours`,
   `workspace_max_api_keys`, `organization_max_api_keys`), all unset.

## Decisions

1. **No `/v1` surface at all.** We serve neither creation (the reference withholds
   it) nor the Admin API's read/update pair: `/v1/organizations/api_keys` belongs to
   the Console Admin API, authenticated by an `sk-ant-admin…` credential class this
   platform does not have, and outside our Managed Agents wire-compat target.
   Registered in DIVERGENCES rather than left as an unexplained gap.
2. **Path mirrors the reference segment-for-segment**, as
   `internal/api/consoleapi.go` already says its paths do:
   `/api/console/organizations/{org}/workspaces/{workspace}/api_keys` and
   `…/api_keys/{key_id}`. Both segments resolve only `default` (`reservedOrganization`),
   the same 404 an unknown organization gets today. The `workspaces` segment is
   carried because the reference carries it and because `workspace_id` is already a
   reserved column — reserving a seam, not implementing one. Note the prefix is
   `/api/console/`, not plan 30's `/api/oauth/`: the reference uses both, and each
   surface keeps the one it was observed under.
3. **Three states, replacing the binary `revoked_at`.** `status` becomes
   `active` | `inactive` | `archived`, with `expired` **derived** at read time from
   `expires_at` and never stored — matching the reference, where the settable enum
   has three values and the list filter has four.
4. **`api_keys_one_live` is narrowed, not dropped.** The reference allows duplicate
   names, so operator-issued keys must be able to share one. But `EnsureAPIKey`
   depends on one-live-per-name to rotate `CONTROLPLANE_API_KEY` safely against
   concurrent replicas (the race 0013 closed, #72). The invariant therefore survives
   over exactly the rows that need it: unique on `name`
   `WHERE status = 'active' AND created_by IS NULL`. Keying it on **created_by**
   rather than on the literal name `'bootstrap'` is deliberate — a row nobody issued
   is env-var-managed by definition, so the schema states the semantics instead of
   depending on a string constant that lives in `cmd/controlplane`, and the
   guarantee holds for every name `EnsureAPIKey` is ever called with rather than
   only the production one.
5. **`partial_key_hint` is stored, not derived.** We keep only a hash, so a masked
   hint cannot be reconstructed — it needs its own column. Format mirrors the
   reference: the fixed prefix, three characters, `...`, the last four.
6. **Issued keys carry a `sk-map-api01-` prefix**, the family plan 30 established
   with `sk-map-env01-`. The bootstrap key keeps whatever value the operator set;
   its hint is computed at boot from the plaintext `EnsureAPIKey` already holds.
7. **`expires_at` is client-supplied and optional**, unlike environment keys'
   server-fixed `EnvironmentKeyTTL`. That is a real divergence between our own two
   surfaces, and it is the reference's: an operator issuing a management credential
   picks its lifetime; a worker credential's lifetime is the platform's business.
   Absent means never expires, and the response reports `expires_at: null`.
8. **`can_manage` is omitted.** These routes are admin-only (decision 9), so the
   field would be a constant `true` — a speculative column for a permission split we
   have not built. Recorded in DIVERGENCES with the reason.
9. **Admin-gated**, registered exactly as plan 30's console routes are —
   `s.handle(identity.RoleAdmin, …)`. With identity disabled the management
   `x-api-key` reaches them as it reaches every other management route.
10. **The `policy` endpoint is out of scope.** Observed, recorded, not built: no
    caller wants it and every value it returns is unset on the reference. It goes to
    the backlog, not this plan.
11. **The resource carries `workspace_id` and `principal` as `null`, and
    `created_by` populated.** This is the question #378 left open, and the recording
    answers it: on the reference's own single-tenant account both fields came back
    **null**, so emitting null is mirroring rather than reserving-by-guess, and #56
    can fill them without a shape change. `created_by` is different — it is real
    information we can supply today, because plan 31 shipped `principals`: an admin
    issuing a key over SSO has a stable `principal_` id, and with identity disabled
    the issuer is the machine key's own row. Migration 0024 therefore adds a nullable
    `created_by text` (no foreign key — a principal may be removed while the key it
    issued keeps working, which is exactly what the reference's page states: "API
    keys are owned by workspaces and remain active even after the creator is
    removed"). It renders as `{"id":…,"type":…}` with type `principal` or `api_key`,
    against the reference's `user` — a divergence, because we have no `user_` id to
    give.

## Slices

1. **Lifecycle in the schema, no new surface.** Migration 0024: `status` (checked
   enum, backfilled `archived` where `revoked_at` is set), `expires_at`,
   `partial_key_hint`, `created_by`; `api_keys_one_live` dropped and re-created
   over the rows nobody issued (decision 4 — keyed on `created_by IS NULL`, not on
   the literal name `bootstrap`).
   `authenticate` accepts a key only when `status = 'active'` and it has not
   expired; `EnsureAPIKey` writes a hint and keeps its rotation. Nothing observable
   changes for an existing deployment — the gate green is the proof.
2. **The console surface.** `POST` (issue, returning the resource plus `raw_key`),
   `GET` (bare array, archived rows included as the reference returns them),
   `POST …/{key_id}` (status and name updates, rejecting `expired` with the same
   message shape the reference produces). Name bounds reuse
   `environmentKeyNameMax`. 405 fallbacks registered as plan 30's are. All four
   DIVERGENCES entries land here, with the surface that diverges — including the
   `sk-map-api01-` one: slice 1 declares the constant, but until something mints a
   key with it there is no observable divergence to register.
3. **Docs and acceptance.** A
   `docs/self-hosted-security.md` section on management-key rotation beside the
   environment-key one; an acceptance run that issues a key over the console API,
   drives `/v1` with it, disables it and is refused, re-enables it, archives it and
   is refused again — recorded in docs/HISTORY.md.

## Verification

Contract tests per slice against the Dockerized Postgres the API suite already
uses: the status/expiry matrix in `authenticate`, bootstrap rotation across the
index change, migration backfill from a pre-0024 row, and each route's role
gating (admin passes, developer and viewer get `permission_error`). The acceptance
run in slice 3 is the end-to-end proof and the only place a real key is exercised
against `/v1`.

## New DIVERGENCES entries

- No `/v1/organizations/api_keys`: creation withheld as the reference withholds it,
  read/update omitted as Admin-API surface outside our compat target (slice 2).
- Client-supplied `expires_at` on management keys against server-fixed TTL on
  environment keys, and what "absent" means (slice 2).
- `can_manage` omitted from the listing while the surface stays admin-only (slice 2).
- `sk-map-api01-` as the issued-key prefix, beside `sk-map-env01-` (slice 2).
