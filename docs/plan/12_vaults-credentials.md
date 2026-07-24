---
status: draft
issue: "#50"
---

# Vaults + egress-time credential injection (plan 12)

This plan lifts **#50** out of its reserved seam: the wire-compatible `/v1/vaults` and
nested credentials API with encrypted-at-rest secret storage (OpenBao transit), session
attachment via `vault_ids`, and the first phase of the reserved egress point — a
per-session domain gate that finally honors `limited` networking's `allowed_hosts` and
carries the placeholder-substitution engine. Four PR slices.

Deliberately **not** in this plan, each with its own tracker:

- **BYOC credential delivery** ([#165](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/165)):
  `work.secret` stays null. This matches the reference — its public vaults doc states
  *"Environment variable credentials (`environment_variable`) are not yet supported with
  self-hosted sandboxes"*, and its shipped SDK/CLI worker never reads the field
  (anthropic-sdk-go `lib/environments/worker.go` reads only `work.ID/EnvironmentID/Data.*`;
  the `poller_test.go` fixture calls the field "unused-by-helper").
- **TLS-terminating (MITM) substitution for in-sandbox HTTPS traffic**
  ([#166](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/166)): phase 2 of
  the egress point. Phase 1 (slice 4 here) substitutes only where the platform already
  sees plaintext; the reference's managed sandbox demonstrably substitutes inside
  sandbox-originated HTTPS (see "Ground truth"), so until #166 lands, the "CLI inside the
  sandbox authenticates via env var over HTTPS" scenario sends the literal placeholder.
- **web_fetch / web_search** (#47): the tools themselves. Slice 4 builds the egress
  policy and substitution engine they were waiting on; #47 builds the tools.
- **MCP-credential consumption** (#45): `mcp_oauth` / `static_bearer` are stored,
  validated, and rotated here, but nothing consumes them until the MCP client exists.
  Scheduled auto-refresh of expiring OAuth tokens also waits for #45 — with no consumer,
  a background refresh loop would be motion without purpose; `mcp_oauth_validate`'s
  on-demand refresh probe (slice 2) covers the management need.
- **Vault webhooks** (`vault.*`, `vault_credential.*`): no webhook surface exists on this
  platform.
- **Deployment `vault_ids`** (max 50, full-replacement update): deployments are not
  implemented (#51 tracks the nearest deployment work).
- **Session-update attachment**: the wire itself rejects it ("Not yet supported;
  requests setting this field are rejected" — betasession.go:2409); our existing
  rejection is already correct and stays.

## Ground truth (verified 2026-07-23)

Resolved per CLAUDE.md's order: **public docs** (the platform.claude.com "Authenticate
with vaults" guide, fetched 2026-07-23) → **pinned `anthropic-sdk-go` v1.59.0**
(`betavault.go`, `betavaultcredential.go`, `betasession.go`, `betaenvironmentwork.go`;
the local checkout HEAD is the v1.59.0 tag — identical to the pin, zero drift) → the
**`ant` CLI source** (`pkg/cmd/betavault.go`, `betavaultcredential.go`, `worker.go`). No
live recording was possible; everything only a recording can settle is pre-listed in
"Inferences and divergences to record" and lands in docs/DIVERGENCES.md with its slice.

### Endpoints (beta header `managed-agents-2026-04-01`; all paths carry `?beta=true`)

| Verb | Path | Notes |
|---|---|---|
| POST | `/v1/vaults` | create; `display_name` required (1–255) |
| GET | `/v1/vaults` | list; `limit` (default 20, max 100), `page`, `include_archived` |
| GET | `/v1/vaults/{id}` | |
| POST | `/v1/vaults/{id}` | update (POST, not PATCH — same as every managed-agents resource) |
| DELETE | `/v1/vaults/{id}` | returns `{"id", "type":"vault_deleted"}` |
| POST | `/v1/vaults/{id}/archive` | idempotent; returns the full vault |
| POST | `/v1/vaults/{vid}/credentials` | create; `auth` union required |
| GET | `/v1/vaults/{vid}/credentials` | list; same params as vault list |
| GET/POST/DELETE | `/v1/vaults/{vid}/credentials/{cid}` | get / update / delete (`vault_credential_deleted`) |
| POST | `…/credentials/{cid}/archive` | idempotent; returns the full credential |
| POST | `…/credentials/{cid}/mcp_oauth_validate` | live probe; returns `vault_credential_validation` |

### Objects

- **Vault** (`type:"vault"`, id `vlt_…`): `id`, `display_name`, `metadata`,
  `created_at`, `updated_at`, `archived_at` (nullable). Workspace-scoped in the
  reference; single-tenant defaults here (reserved scope columns as everywhere).
- **Credential** (`type:"vault_credential"`, id `vcrd_…` — **a prefix this repo does not
  know yet**; slice 2 adds it to `domain/id.go` and CLAUDE.md's prefix list): `id`,
  `vault_id`, `display_name` (nullable — the only nullable top-level field), `auth`
  (union), `metadata`, timestamps, `archived_at`.
- **Auth union**, discriminated on `type`:
  - `mcp_oauth`: `mcp_server_url` (immutable), `expires_at` (nullable), `refresh`
    (nullable: `client_id`, `token_endpoint`, `token_endpoint_auth`
    none/client_secret_basic/client_secret_post, `resource`, `scope`). Write-only:
    `access_token`, `refresh_token`, `client_secret`. The refresh **update** union drops
    `none` and freezes `client_id`/`token_endpoint` (locked after create, per the docs).
  - `static_bearer`: `mcp_server_url` (immutable). Write-only: `token`.
  - `environment_variable`: `secret_name` (immutable), `networking`
    (`unrestricted` | `limited{allowed_hosts}`; ≤16 entries, each a bare hostname, IPv4,
    or `*.`-prefixed wildcard — no URLs/ports/paths/IPv6; exact match, `*.` matches
    subdomains but not the apex; update is **full replacement**), `injection_location`
    (`{body, header}`). Write-only: `secret_value`.
- **Validation** (`type:"vault_credential_validation"`): `status`
  `valid`/`invalid`/`unknown` (invalid = the grant is gone / OAuth 4xx; unknown =
  transient 5xx/429/network), `mcp_probe` `{method, http_response}` (the failing MCP
  step, e.g. `initialize`; body truncated + scrubbed), `refresh` `{status
  succeeded|failed|connect_error|no_refresh_token, http_response}`,
  `has_refresh_token`, `validated_at`.

### Hard semantics from the public docs (settled 2026-07-23, previously unknowable from types)

- **The sandbox stores env-var secrets as opaque placeholders**; substitution happens at
  egress on agent-initiated outbound requests. A placeholder in a disabled
  `injection_location` is *neither substituted nor stripped* — it reaches the third
  party literally. Signature-computing clients (AWS SigV4) produce invalid signatures.
  Together these prove the reference's managed egress sees request plaintext (the #166
  gap). Substitution is **outbound only** — a token exchanged *for* the secret arrives
  in the sandbox unredacted.
- **Two-level gate**: credential `allowed_hosts` decides which requests *use* the
  secret; the environment's networking decides which requests are *allowed*. Both must
  admit the host; the mismatch surfaces as the `credential_host_unreachable_error`
  session error event (`credential_id`, `vault_id`, `message`, `retry_status`).
- **`injection_location` is asymmetric**: on create, omitting the whole object enables
  both locations, while a provided object defaults omitted fields to `false`; on update,
  fields merge individually. Disabling both locations is a 400; explicit `null` (object
  or field) is a 400 ("omit the field instead"). Responses always render both fields.
- **Uniqueness and caps**: `secret_name` / `mcp_server_url` unique among *active*
  credentials per vault (duplicate → 409, archived keys are freed); ≤20 credentials per
  vault; metadata ≤16 pairs, keys ≤64, values ≤512; vault `display_name` 1–255 required,
  credential `display_name` ≤255 optional.
- **Archive purges the secret payload** and keeps the record for audit; running sessions
  continue, future sessions referencing the vault fail. Delete is a hard delete.
- **Credentials are re-resolved periodically during a session** — rotation, archive, and
  `injection_location` changes propagate without a session restart. When several
  attached vaults match the same key, the first vault with a match wins. Credentials are
  not validated at write time; a bad credential surfaces as a session error and does not
  block the session.
- **Session attachment is create-time-only, top-level `vault_ids`**
  (betasession.go:2090; the session resource echoes it as required, empty when none) —
  *not* inside the agent definition, which carries no vault fields anywhere.

## Design decisions

**D1 — Secrets at rest: OpenBao transit, ciphertext in Postgres.** A new
`internal/secrets` package defines the cipher seam — `Encrypt(ctx, plaintext) →
{ciphertext, key_id}` / `Decrypt(ctx, ciphertext, key_id)` — with two backends behind
the repo's standard shared contract suite: `openbao` (transit engine; production
default) and `local` (AES-256-GCM under a configured 32-byte master key; tests and
minimal deployments — `make test` must never require a live bao). Postgres stays the
single canonical store: resource rows plus `bytea` ciphertext plus `key_id`; no
dual-write, backups stay atomic, and a bao outage degrades only decrypt-time paths
(egress substitution, `mcp_oauth_validate`), never metadata CRUD. Archive **deletes the
ciphertext** and keeps the row (the docs' "secrets are purged; records are retained").
The contract suite's bao leg runs against a dev-mode OpenBao container (Docker is
already a hard test dependency); the new test-support package joins the coverage-gate
exclusion list in the Makefile and CLAUDE.md.

**D2 — Deployment shape: the MinIO pattern.** Compose gains an `openbao` service;
persistence matters (transit keys must outlive restarts or the ciphertext is bricked),
so the bundled instance uses persistent storage plus automated init/unseal — prefer
OpenBao's static/auto-unseal configuration if the pinned version supports it (verify in
slice 1), else an entrypoint script that stores the unseal key alongside the data volume,
documented as a dev-grade convenience. Helm mirrors `minio.yaml` exactly: a hand-written
single-node StatefulSet behind `openbao.enabled` (no subchart — air-gap installs must
not pull external charts), an `externalOpenBao` block (`address`, auth, transit key
name, tls) for enterprises pointing at their own OpenBao/Vault (the platform speaks the
plain transit HTTP API — any Vault-compatible endpoint works, the same posture as
"plain S3"), `existingSecret` compatibility, and — with both disabled — fallback to the
`local` cipher with the master key in the chart Secret. `BAO_ADDR` + credentials reach
**controlplane** (encrypt on write, decrypt for poll-render later under #165 and for
`mcp_oauth_validate`) and **executor** (decrypt at substitution time); **brain** joins
with #45; the **worker never talks to bao**.

**D3 — Egress in two phases; phase 1 has no TLS interception.** Slice 4 builds a
per-session forward proxy — the domain gate: sandbox egress rides `HTTP_PROXY` /
`HTTPS_PROXY` env vars through it, HTTPS passes as opaque CONNECT tunnels admitted or
refused on SNI/Host against the environment's networking policy. `limited` finally
means "only `allowed_hosts`" instead of "no route at all" (superseding the fails-closed
divergence, DIVERGENCES.md:42), and unrestricted gains the reference's notion of a
safety boundary point. Substitution happens only where the platform holds plaintext:
plain-HTTP requests through the gate, and platform-process egress once #47's tools
exist. In-sandbox **HTTPS** bodies/headers keep their placeholders until #166 — a
documented, deliberate gap, not an oversight.

**D4 — Placeholders are ours to mint.** The reference calls the sandbox-visible value an
"opaque placeholder" and defines no format; we mint `vltph_`-prefixed random tokens per
credential-resolution, injected as `secret_name=<placeholder>` env vars at sandbox
provision. This needs the one seam neither backend has today: an `Env` field on
`sandbox.Spec` threaded into the Docker container config and the K8s pod spec. The
substitution engine — placeholder registry, `allowed_hosts` matcher (exact / `*.`
subdomain-not-apex), `injection_location` scoping, `credential_host_unreachable_error`
emission — lands as one shared internal package consumed by the gate (plain HTTP), by
#47's tools, by #166's TLS-terminating phase, and by #165's worker: written once here,
never forked per consumer.

**D5 — Session attachment goes live; resolution is read-time.** Session create stops
rejecting non-empty `vault_ids` (the DIVERGENCES.md:28 carve-out shrinks again):
each id must name an existing, unarchived vault. Credential resolution happens when an
execution needs it (provision-time for placeholders, egress-time for values), reading
current rows — which delivers the reference's "re-resolved periodically, rotation
propagates without restart" semantics for free, no cache invalidation machinery.
First-attached-vault-wins on key collision across vaults, matching the docs.

**D6 — BYOC stays at the reference's line.** `work.secret` remains null on every path
(the existing workapi mirror and its DIVERGENCES entry stand, now pointing at #165 for
the extension); attaching a vault to a session on a `self_hosted` environment is
accepted — attachment is environment-type-agnostic — but env-var credentials are simply
never delivered there, matching the reference's "not yet supported with self-hosted
sandboxes".

**D7 — Documented limits are enforced.** Metadata caps (16/64/512), display_name
lengths, ≤20 credentials, ≤16 allowed_hosts, uniqueness-409, injection_location's
400s: all enforced as the docs state. No sibling resource enforces metadata caps today;
that asymmetry is deliberate (the vault caps are documented, the siblings' are not) and
recorded once in DIVERGENCES.md rather than silently harmonized in either direction.

**D8 — `mcp_oauth_validate` is a real probe from slice 2.** It needs no MCP client:
attempt the refresh exchange when a refresh block exists (`succeeded` / `failed` /
`connect_error` / `no_refresh_token`), then probe the MCP server with a streamable-HTTP
`initialize` under the (possibly refreshed) token, scrub and truncate captured bodies,
and map to `valid` / `invalid` (4xx) / `unknown` (transient). A successful refresh
persists the rotated tokens, mirroring the reference's managed refresh.

## Slices

**Slice 1 — the cipher seam and its infrastructure.** `internal/secrets`: the
`Cipher` interface, `local` AES-256-GCM backend, `openbao` transit backend, shared
contract suite + bao-container test support (Makefile/CLAUDE.md exclusion-list update),
config plumbing (`SECRETS_BACKEND`, `SECRETS_MASTER_KEY` / `BAO_ADDR`+auth+key-name)
into controlplane and executor. Compose `openbao` service with persistence + unseal
automation; helm `openbao.enabled` StatefulSet, `externalOpenBao`, `existingSecret`
rules, values documentation; CI's helm lint/render and compose smoke stay green.
*Verify: contract suite green on both backends; compose up → encrypt/decrypt round-trip
survives an `openbao` container restart.*

**Slice 2 — `/v1/vaults` and credentials, wire-complete.** Migration `0011_vaults.sql`
(vaults + vault_credentials, reserved scope columns, ciphertext `bytea` + `key_id`,
partial-unique index on active credential keys); `vcrd` prefix in `domain/id.go` and
CLAUDE.md's wire rules; handlers mirroring the environments exemplar (POST-update,
archive-with-purge, tombstone deletes, keyset pagination, `patchMetadata`
emptyDeletes=false, strict unknown-key rejection); the full auth-union
validation surface (write-only fields, immutable anchors, injection_location
asymmetry and 400s, networking full-replacement, D7 limits); `mcp_oauth_validate` per
D8. DIVERGENCES entries per the list below. *Verify: `ant beta:vaults` /
`beta:vaults:credentials` end-to-end against the local server; secret values absent
from every response, log, and error.*

**Slice 3 — sessions attach vaults.** Session create accepts `vault_ids` (existence +
unarchived validation; the DIVERGENCES.md:28 rejection entry updates); the resolution
package (attached vaults → active env-var credentials, first-vault-wins) with read-time
semantics per D5; update keeps its wire-faithful rejection. *Verify: create-with-vault
round-trips `vault_ids`; archived vault → create fails; update still rejects.*

**Slice 4 — the egress gate, phase 1.** `sandbox.Spec.Env` seam in both backends;
per-session gate proxy (Docker: internal per-session network, sandbox reaches only the
proxy, proxy CONNECT-filters on the environment policy; K8s: sidecar listener + the
existing init-container mechanics repointed at "only the sidecar routes out");
`HTTP(S)_PROXY` injection; placeholder minting + env injection per D4; the substitution
engine package with plain-HTTP substitution live in the gate;
`credential_host_unreachable_error` emission; sandboxtest contract rows updated from
"limited = no route" to "limited = only allowed_hosts through the gate"; DIVERGENCES.md:42
superseded, self-hosted-security.md's reserved-work section rewritten. *Verify: contract
suite — limited sandbox curls an allowed host through the gate and is refused a
non-allowed one; plain-HTTP placeholder substituted, HTTPS placeholder passes through
literally (the documented #166 gap); K8s parity on kind.*

## End-to-end acceptance (after slice 4)

Build `ant` from the read-only checkout; against the compose stack: create a vault and
an env-var credential (`allowed_hosts: [httpbin.org]`, header-only) via the CLI; create
a session on a `limited` environment with the vault attached; drive a bash tool call
that curls `http://httpbin.org/headers` with the placeholder env var — response shows
the substituted header; curl any other host — refused by the gate;
`GET /v1/vaults/{id}/credentials/{cid}` never shows `secret_value`; archive the
credential, re-resolve, placeholder no longer minted. Record the transcript in
docs/HISTORY.md as the acceptance run.

## Observability

CRUD spans follow the existing api conventions. The gate emits one span per egress
decision (host, verdict, matched rule; never values) and the substitution engine one per
substitution (credential_id, location; never values). The Redactor posture extends:
plaintext secrets exist only inside the cipher and the substitution call path, are never
logged, and never enter session events — the same invariant the provider Redactor
enforces for model keys.

## Inferences and divergences to record, by slice

Slice 2: enforcement of documented caps as hard 400s where the reference's live behavior
is unobserved (documented-but-unrecorded); whether credential routes 404 on a
`{vault_id}` path segment that mismatches the credential's actual vault (we will —
INFERRED); update-on-archived-credential behavior (we reject like archived environments —
INFERRED); `mcp_oauth_validate` on a non-`mcp_oauth` credential (we 400 — INFERRED);
probe details of D8 (initialize framing, scrub rules — INFERRED against #78's recording
checklist). Slice 3: the error status/shape for `vault_ids` naming a missing or archived
vault on session create (we 400 with the standard envelope — INFERRED; the docs say only
"future sessions … fail"). Slice 4: the placeholder token format (opaque by design —
ours, recorded as deliberate); phase-1's literal-placeholder pass-through for in-sandbox
HTTPS (deliberate divergence until #166); `unrestricted`'s safety-blocklist contents
(reference's list unpublished — ours is INFERRED). Standing: `work.secret` null
(existing entry re-pointed at #165).
