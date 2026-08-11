---
status: draft
issue: "#56"
---

# Console SSO and RBAC: pluggable identity, platform-enforced roles (plan 31)

Delivers the SSO + RBAC half of #56 (multi-tenant activation stays in that
issue's backlog — see Out of scope). Today one `x-api-key` is the whole
management authority: whoever holds it is root, the console BFF wields it for
every user action, and `created_by` can only ever name a key row. End state:
**humans authenticate through any standards-compliant OIDC identity provider**
— the platform is a strict, vendor-SDK-free relying party, with **Casdoor
bundled as the hardened self-host default** and a **trusted-proxy mode**
(Google Cloud IAP first) for deployments where the cloud vendor terminates
auth — and the control plane resolves each request to a **principal** with one
of three roles (`admin` / `developer` / `viewer`) enforced per route. The
`/v1` wire surface, the `ant` CLI, and every SDK flow are byte-identical
before and after: machine credentials (`x-api-key`, environment keys) are
untouched, and SSO never appears on the wire. The work spans two repos: this
plan covers the platform half (verifier, principals, roles, deployment
wiring); the console half — the browser OAuth dance and forwarding the user's
token instead of a server-held god key — is managed-agent-console's plan 08,
which consumes what lands here and must trail it.

## Scope decisions settled with the user on 2026-08-12

1. **Default bundled IdP: Casdoor, hardened.** The core stays a generic OIDC
   relying party (`coreos/go-oidc` + `golang.org/x/oauth2`; **no
   casdoor-go-sdk** — its JWT verification pins a static certificate where
   go-oidc follows JWKS rotation, so the vendor SDK is strictly worse for the
   RP role). Casdoor is a *deployment default*, not a code dependency: any
   compliant IdP (Keycloak, Entra ID, Cognito, Google) replaces it by config.
   The bundle ships with the hardening posture in Architecture — pinned
   version ≥ v2.387.0, zero upstream providers, single organization,
   token-exchange grant disabled, SAML routes blocked — because of CERT/CC
   VU#780781 (see Ground truth; the risk is stated in docs, not hidden).
2. **Enforcement lives in the control plane** — the reference's own shape
   (Ground truth: roles gate what humans can mint and manage; the API server
   enforces per-credential authority). The platform verifies the user's token
   itself, resolves a principal, and enforces roles per route. The BFF stops
   acting as the user with a god key; it forwards the user's token. A
   BFF-only enforcement (platform keeps one all-powerful key) was considered
   and rejected: no real principal on the platform, audit stops at the BFF,
   and the gate is void for anyone holding the key.
3. **Three roles, claim-mapped:** `admin` / `developer` / `viewer`. Roles are
   derived from the token's claims on every request (the IdP stays
   authoritative; the platform stores no role), through a configurable
   claim-name + value→role mapping. An authenticated principal whose claims
   map to no role holds no authority (fail-closed).
4. **This plan is SSO + RBAC only.** Multi-tenant activation is deliberately
   out (the user: not needed now). It stays tracked by #56; the Roadmap
   section records what it would add. `ant auth login` compatibility is also
   out — recorded as a divergence and a roadmap item, not built.

## Out of scope

- **Multi-tenant activation.** The reserved `org_id`/`workspace_id`/
  `project_id` columns stay at their single-tenant defaults. (Sized during
  planning: ~13 handler files with 60–80 scoped statements, ~6 create-time
  cross-reference checks, one worker-lane policy reversal — env-lane skill
  reads are deliberately global today (server.go:329–360) and become a
  cross-tenant leak — plus the `api_keys_one_live` rebuild 0013:43–48
  pre-announces, with tests expected at 2–3× the production diff. No
  structural obstacle; this plan's principals and key-issuance surfaces are
  its prerequisites.)
- **`ant auth login` against this platform.** The obligations are known
  precisely (Ground truth / Roadmap) and deliberately not taken on.
- **End-user identity.** Design principle 5 stands: sessions stay
  user-agnostic; end-user ↔ session ownership remains an application-layer
  concern. This plan authenticates *operators of the platform*, nothing else.
- **Fine-grained / resource-level authorization** (Casbin-style policies,
  per-key scoping, `authorization_details`): three fixed roles only.
- **Casdoor management-plane integration** (auto-provisioning users via its
  API, SCIM, consuming `/api/enforce`): the platform never calls the IdP —
  it only verifies tokens.
- **A `/v1/organizations/*` Admin API** with an admin-key credential class
  (the reference's shape for org management): roadmap, not now.

## Ground truth

### How the reference enforces authorization (public docs, checked 2026-08-12)

- **Roles live in the Console and gate credential minting; they never travel
  on API requests.** Org roles: User, Claude Code User, Limited Developer,
  Developer, Billing, Admin, (Primary) Owner
  (support.claude.com article 10186004; the Admin API doc's wire values are
  `user`, `claude_code_user`, `developer`, `billing`, `admin`). Regular API
  keys are created in the Console only — the Admin API cannot create one
  "for security reasons" — and **environment-key generation is
  Console-only** too (platform.claude.com/docs/en/managed-agents/
  self-hosted-sandboxes). Our plan-30 issuance surface is that page's
  equivalent; this plan adds the role gate it presumes.
- **The API server enforces per-credential authority.** Keys are scoped to a
  workspace ("API keys are scoped to a single workspace and can only access
  resources within that workspace" — workspaces doc); `/v1/organizations/*`
  accepts only the separate `sk-ant-admin…` key class or an OAuth token with
  the `org:admin` scope — a regular key is refused; OAuth tokens are checked
  per endpoint and fail 403 with "OAuth token does not meet scope
  requirement…". Bearer *user* tokens are a first-class `/v1` credential in
  the reference — after `ant auth login`, the CLI drives `/v1` with
  `Authorization: Bearer` plus `anthropic-beta: oauth-2025-04-20`
  (pkg/cmd/cmdutil.go:92–99).
- **Human login is out-of-band, on a separate console host.** `ant auth
  login` is authorization-code + PKCE: the authorize page lives on the
  console host (`https://platform.claude.com/oauth/authorize`,
  pkg/cmd/cmd_auth.go:264; `--console-url` / `ANTHROPIC_CONSOLE_URL`), and
  only the machine leg — `POST /v1/oauth/token`, three grants:
  authorization_code (form-encoded), refresh_token (JSON +
  `anthropic-beta: oauth-2025-04-20`), RFC 7523 jwt-bearer federation (JSON +
  both betas) — rides the API host (cmd_auth.go:1081–1096, 1186–1211;
  sdk config/federation.go:141–235). A server that only ever sees
  `x-api-key` and environment keys owes **no OAuth endpoint at all**; the
  CLI's management/worker flows never touch one.
- **Org and workspace surface through credentials, not URLs.** No management
  path or request header carries an organization id (sdk
  internal/auth/types.go:32–35 — the org travels only as the
  `anthropic-organization-id` *response* header); the workspace rides the
  `anthropic-workspace-id` request header from the credential's profile
  (option/requestoption.go:523). Tenancy context comes from the credential —
  exactly the assumption our reserved-seam design already makes.

### Casdoor: capabilities and the security picture (verified to source, 2026-08-12)

- **Capabilities** (casdoor.org docs): full OIDC provider — code + PKCE
  (S256), discovery document, JWKS endpoint, per-application issuers;
  federation to ~100 upstream OAuth/OIDC/SAML/LDAP IdPs; admin UI; MFA;
  multi-organization; Casbin RBAC with roles/groups deliverable as token
  claims (per-application token format, array-typed claim control). Go +
  Postgres, one light container, Apache-2.0, ~14k stars, multiple releases
  per week (v3.151.0 on 2026-08-11).
- **CERT/CC VU#780781 (published 2026-05-28): nine CVEs affecting
  ≤ 2.362.0**, coordination with the vendor failed ("we have not received a
  statement from the vendor"), and the advisories name no fixed version.
  Verified against the source at HEAD (v3.152.0):
  - Five are in the **SAML service-provider path** (upstream SAML IdP
    federation, `object/saml_sp.go` reached only via a configured
    `Category == "SAML"` provider): CVE-2026-9090 (cert taken from the
    incoming SAMLResponse, CVSS 9.1) — **already fixed silently in v2.387.0
    (commit `d14674e6`, 2026-04-05, seven weeks before disclosure)**;
    CVE-2026-9093/9095/9096/9098 (no audience check, no replay protection,
    time bounds ignored, unsolicited responses) — unpatched, but unreachable
    with no SAML provider configured. The routes stay registered regardless
    (`routers/router.go`), so defense-in-depth blocks `POST /api/acs` and
    `GET /api/get-saml-login` at the proxy.
  - Four are elsewhere: CVE-2026-9091 (MFA bypass) and CVE-2026-9092
    (unverified-email account takeover, 9.1) both live on the
    **upstream-provider binding path — any social/OAuth upstream, not just
    SAML** — so the effective mitigation is zero upstream providers;
    CVE-2026-9094 (cross-org escalation) is voided by single-org deployment;
    CVE-2026-9097 (revoked JWT accepted in token exchange, 9.8) is mitigated
    by disabling the token-exchange grant and short token lifetimes.
  - Consequence for the bundle: Casdoor serves as the **local-account IdP**
    (users, MFA, admin UI, OIDC OP). An enterprise wiring its own IdP points
    the platform's OIDC seam **directly at that IdP** — Casdoor leaves the
    path entirely, rather than federating through its vulnerable binding
    code.
- Alternatives were evaluated: Keycloak (mature security process; JVM,
  ~500MB images), Zitadel (best technical fit; AGPL since v3), Authentik
  (open-core, non-Go stack), Dex (no user store — fails "works with zero
  external IdP"). Casdoor is the only candidate matching the stack and the
  license with a local user store; the CVE posture is priced in above.

### Cloud-vendor adapters (checked 2026-08-12)

- **Google Cloud Identity Platform is not a generic OP** — its discovery
  document has no `authorization_endpoint`; sign-in requires Google's client
  SDKs. But its tokens are plain RS256 JWTs
  (`iss = https://securetoken.google.com/{project}`), so the *backend* seam
  is unaffected. The recommended GCP shape is **GCIP (or any IdP) behind
  IAP**.
- **IAP** injects `x-goog-iap-jwt-assertion` — an ES256 JWT the backend must
  verify (`iss = https://cloud.google.com/iap`, keys at
  `https://www.gstatic.com/iap/verify/public_key-jwk`, audience
  `/projects/N/global/backendServices/ID`); the unsigned
  `x-goog-authenticated-user-*` convenience headers are never trusted. Plain
  **Sign in with Google** (`accounts.google.com`) is a certified OP and works
  through the generic RP seam (restrict orgs via the signed `hd` claim, never
  the request parameter).
- **The same two shapes generalize**: AWS ALB OIDC auth injects
  `x-amzn-oidc-data` (ES256; signer validation is mandatory — the 2024
  "ALBeast" bypass, CVE-2024-8901, came from apps skipping it); Cognito and
  Entra ID are standard OPs for the RP seam. So the minimal adapter surface
  is **two modes sharing one verifier**, with presets — no vendor SDK
  anywhere.

### Platform as-is (what this plan changes)

- All inbound auth converges on `dispatchAuth` (internal/api/server.go:206–
  244), classifying on the escaped path ahead of the router; `dualAuth`
  (server.go:249–257) picks the env lane only when a Bearer is present and no
  non-empty `x-api-key` is. Four lanes exist: management `x-api-key`
  (`requireAPIKey`, internal/api/auth.go:70–88 — hash lookup in `api_keys`,
  auth.go:56–65), environment keys (envauth.go), the gate token
  (gateauth.go), and the dual-auth worker reads. The management key is seeded
  from `CONTROLPLANE_API_KEY` at startup (cmd/controlplane/main.go:110–112,
  124) via `EnsureAPIKey` (auth.go:29–52).
- One valid `x-api-key` is full management authority — the console API
  "delegates no authority the management key did not already hold, and is
  not a separate permission tier" (docs/ARCHITECTURE.md:821–824). There is no
  403 in the house error set: `errors.go` defines
  `errInvalid`/`errNotFound`/`errAuth` only — a `permission_error` shape is
  new with this plan.
- The authenticated principal is the `api_keys` row id, stored in context
  (`ctxKeyPrincipal`, errors.go:49–53) and consumed at exactly one site —
  `sessions.created_by` (sessions.go:596–600, 614–619; audit-only,
  0001_init.sql:82–85). Widening what a principal is has one choke point
  (`authenticate`) and one reader (`principalFrom`, auth.go:90–93).
- The console namespace (`/api/oauth/organizations/{org}/environments/{id}/
  tokens[…]`, consoleapi.go:44–53, routes server.go:117–124) authenticates
  with the management key by falling through to `dispatchAuth`'s default arm
  — consoleapi.go:31–32 explicitly warns a future off-`/v1` lane must
  re-derive that reasoning. Plan 30 shaped the namespace for this moment:
  the `oauth` segment and RFC 6749 responses were kept "compatible with a
  real OAuth flow later (console OIDC is its own effort, console plan 08)".
- `principal` has no table; `api_keys` has no owner column; nothing issues a
  management key at runtime (bootstrap env var only).

## Architecture

### `internal/identity` — one verifier, two modes, no vendor SDKs

New package, dependency-injected into `api.NewHandler` the way the cipher and
blob store are (server.go:46–47), constructed in `cmd/controlplane/main.go`
from env config following the `SECRETS_BACKEND` optional-backend pattern
(main.go:156–162).

**The shared verifier** is the security core both modes consume: given
{issuer, audience(s), key source, allowed algorithms, claim mappings}, it
verifies a compact JWT — signature via JWKS (kid-indexed cache; refresh on
unknown kid, rate-limited; no static certs), `iss` exact, `aud` contains,
`exp`/`nbf` with ≤60s leeway, algorithm from an explicit allowlist (RS256/
RS512/ES256/ES384/ES512 — never `none`, never HS*) — and returns a verified
claim set. Claim mapping then produces the identity: subject (`sub`), email
(claim name configurable, default `email`), display name, and **roles** — a
configurable claim name (default `roles`; dotted paths allowed for nested
claims) whose values pass through an explicit value→role map. Unmapped values
drop; no mapped value ⇒ no role ⇒ every role-gated route answers 403. Tokens
are never logged; verification failures are uniform 401s
(`authentication_error`) with no oracle distinguishing expired / bad
signature / wrong audience.

**Mode A — `oidc` (the RP seam).** Config: issuer URL (drives
`/.well-known/openid-configuration` discovery at startup; JWKS URI can be
overridden), audience (the console application's client id), claim mappings.
The platform never runs the browser flow — the console BFF does
authorization-code + PKCE against the IdP (console plan 08) and forwards the
user's token on every platform call as `Authorization: Bearer <jwt>`; the
platform statelessly verifies. Headless humans use any OIDC-native tool to
obtain a token from the IdP and curl with it. One mode covers Casdoor,
Keycloak, Entra ID, Cognito, accounts.google.com, and any other compliant OP.

**Mode B — `trusted_proxy` (the vendor-terminated seam).** Config: assertion
header name + verifier settings, with two shipped presets — `gcp-iap`
(`x-goog-iap-jwt-assertion`, ES256, `iss https://cloud.google.com/iap`,
gstatic JWK set, audience `/projects/N/global/backendServices/ID`) and
`aws-alb` (`x-amzn-oidc-data`, ES256, the regional public-key endpoint,
signer validation mandatory) — plus a `custom` escape hatch. A missing or
invalid assertion is 401; the unsigned convenience headers are never read.
GCIP-behind-IAP needs nothing more; direct-GCIP login UI is a console
frontend option, never a Go-core concern.

Config (all `IDENTITY_*`, controlplane only):

| Variable | Meaning |
|---|---|
| `IDENTITY_MODE` | `disabled` (default) · `oidc` · `trusted_proxy` |
| `IDENTITY_OIDC_ISSUER` / `IDENTITY_OIDC_AUDIENCE` | Mode A: discovery root; expected `aud` |
| `IDENTITY_OIDC_JWKS_URL` | optional override when discovery is absent/wrong |
| `IDENTITY_PROXY_PRESET` | Mode B: `gcp-iap` · `aws-alb` · `custom` |
| `IDENTITY_PROXY_HEADER` / `_ISSUER` / `_AUDIENCE` / `_KEYS_URL` / `_ALGS` | Mode B `custom` fields |
| `IDENTITY_CLAIM_ROLES` / `IDENTITY_CLAIM_EMAIL` | claim names (defaults `roles` / `email`) |
| `IDENTITY_ROLE_MAP` | value→role pairs, e.g. `platform-admins=admin,eng=developer,everyone=viewer` |

`IDENTITY_MODE=disabled` (the default) is byte-for-byte today's platform: the
OIDC lane does not exist, a Bearer on a management path stays a 401, and
`x-api-key` remains the only management credential. Misconfigured identity
(mode set, issuer unreachable, empty role map) fails startup, not open.

### Principals — migration `00NN_principals.sql` (next free number at landing)

```sql
CREATE TABLE principals (
    id            text PRIMARY KEY,          -- principal_… (domain.NewID)
    issuer        text NOT NULL,
    subject       text NOT NULL,
    email         text NOT NULL DEFAULT '',
    display_name  text NOT NULL DEFAULT '',
    org_id        text NOT NULL DEFAULT 'default',
    workspace_id  text NOT NULL DEFAULT 'default',
    project_id    text NOT NULL DEFAULT 'default',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);
ALTER TABLE api_keys ADD COLUMN principal_id text REFERENCES principals(id);
```

- Principals are **JIT-provisioned**: the first verified request upserts by
  `(issuer, subject)` and refreshes `email`/`display_name`/`last_seen_at`.
  No admin pre-registration; authorization comes from claims, so a row is
  identity bookkeeping, not an authority grant. Roles are **not stored** —
  the IdP stays authoritative per request.
- `principal_` joins the private-id family (`envkey_`, `apikey_`, `gtk_`) —
  **not** `domain.knownPrefixes`; it never appears on a `/v1` path.
- `api_keys.principal_id` is nullable: the bootstrap key predates identity
  (`EnsureAPIKey` leaves it NULL); the slice-5 issuance surface sets it.
- `sessions.created_by` now records whichever principal acted: the api-key
  row id on the key lane (unchanged) or the `principal_…` id on the identity
  lane. Still audit-only, still never rendered.

### Dispatch — one new lane, carved at the single choke point

`dispatchAuth` gains an identity lane; nothing else moves. The default
(management) arm becomes credential dispatch:

- non-empty `x-api-key` → `requireAPIKey`, **full authority, no role model**
  — the machine key's semantics are frozen (bootstrap, CI, BYO automation,
  and the reference's own model: a key *is* its authority);
- else `Authorization: Bearer` with a JWT silhouette (two dots) and
  `IDENTITY_MODE=oidc` → the identity lane: verify, resolve principal,
  attach `{principal_id, roles}` to context, enforce the route's role;
- else → 401 exactly as today.

On the dual-auth worker paths, discrimination extends the existing rule:
`sk-map-env01-`-prefixed Bearer → environment-key lane; JWT silhouette →
identity lane (a grandfathered pre-0021 environment key is caller-supplied
text that could in principle contain two dots — it would misroute to a 401,
fail-closed; docs/self-hosted-security.md §6 already tells operators to
reissue those). In `trusted_proxy` mode the identity lane keys on the
assertion header instead of a Bearer; `x-api-key` and environment-key lanes
are untouched in both modes. The `/api/` console namespace gets the explicit
dispatch arm consoleapi.go:24–37 said a future lane must bring: same
credential dispatch (management key or identity), so the plan-30 routes keep
working for headless operators while gaining the role gate.

### Roles and the enforcement point

Role annotations live in the route table — `NewHandler` (server.go:50–196) is
the one place every route is declared, and the `s.handle`/`s.handleNoContent`
adapters (server.go:403–424) gain a minimum-role parameter, so each
registration carries its requirement next to its path (the pattern
`requireEnvironmentKeyForSession` set: authorization checked where the route
is defined). The matrix:

| Role | May |
|---|---|
| `viewer` | GET/HEAD on every management and console resource (no plaintext credential ever renders anywhere, so reads are safe wholesale) |
| `developer` | viewer + all resource CRUD and session lifecycle: agents, environments, sessions (create/events/interrupt/archive), skills, files, session resources, vault CRUD |
| `admin` | developer + every credential surface: environment-key issue/list/revoke (the plan-30 routes — the reference gates its "Generate environment key" behind Console roles; ours now has the gate), vault *credential* create/update/delete, and the slice-5 api-key surface |

Checks apply **only to the identity lane** (the key lane has no roles to
check). Denial is the new house 403 — `{"type":"error","error":{"type":
"permission_error","message":"…"}}` — joining `errInvalid`/`errNotFound`/
`errAuth` in errors.go, mirroring the reference's error taxonomy (its scope
failures are 403 `permission_error`). Uniform per lane: the message names the
required role, never the caller's.

### The Casdoor bundle — hardened by default

`deploy/compose` gains an optional `iam` profile: a pinned `casbin/casdoor`
image (a v3.x tag ≥ the verified-fixed v2.387.0; exact tag pinned at landing
and bumped deliberately), backed by its own database in the existing Postgres
container, seeded with: one organization, one application (the console as
OIDC client — code + PKCE, the console's redirect URL), **zero upstream
providers**, public signup off, the token-exchange grant off, and a role
claim (`roles`) emitted in tokens so `IDENTITY_ROLE_MAP` has something to
map. The compose fronting proxy (or the docs, where no proxy runs) blocks
`POST /api/acs` and `GET /api/get-saml-login` plus the unused SAML-IdP/CAS
routes. docs/self-hosted-security.md gains an SSO section stating the
VU#780781 posture plainly: which CVEs the configuration voids, which the
pinned version fixes (9090), and that enterprises federating their own IdP
should point `IDENTITY_OIDC_ISSUER` straight at it — the bundled Casdoor is
a local-account IdP, not a federation hub. Helm: `identity.*` values
(mode/issuer/audience/claims/role map) injected into the controlplane
Deployment; no Casdoor subchart in v1 (its chart QA is weak — the
casdoor-helm Postgres-values bug — and a hard dependency on a pinned image is
cleaner); the values doc shows wiring an existing IdP and, for GKE,
`trusted_proxy` + `gcp-iap` with the Terraform under deploy/gcp growing the
matching IAP variables and docs.

## Docs that move with the change

- **docs/ARCHITECTURE.md** — `internal/identity` package rows; the auth-lane
  table gains the identity lane and the role matrix; security invariants add:
  verifier rules (allowlist algs, JWKS-only keys, uniform 401), fail-closed
  `disabled` default, roles-from-claims (platform stores no authority),
  key-lane semantics frozen.
- **docs/DIVERGENCES.md** — the entries listed at the end, as their slices
  land.
- **docs/self-hosted-security.md** — the SSO section (Casdoor posture, BYO
  IdP, IAP mode); the api-key section when slice 5 lands.
- **README.md** — status line gains SSO/RBAC once slices land.
- **STATE.md** — flips to this plan when slice 1's PR starts the work;
  `changelog.d/` fragment per PR. Each doc moves in the slice whose code
  invalidates it.

## Slices

1. **`internal/identity`: the verifier and both modes.** The shared verifier
   (JWKS cache, alg allowlist, iss/aud/exp discipline), claim→role mapping,
   discovery, the `gcp-iap`/`aws-alb` presets, and config parsing — pure
   library + contract tests against a local fake IdP (httptest JWKS server:
   key rotation, unknown kid, wrong aud/iss/alg, expiry skew, ES256+RS256,
   nested role claims). No route touched; `make verify` green proves nothing
   observable changed.
2. **Principals + the identity lane.** The migration; JIT provisioning;
   `dispatchAuth`'s credential dispatch and the `/api/` arm; dual-auth
   discrimination; the context principal; `created_by` widening; the
   `permission_error` envelope. First enforcement: the console env-key routes
   require `admin`. Tests over real HTTP: a Casdoor-shaped and an IAP-shaped
   token both authenticate; wrong iss/aud/alg/expired all 401 uniformly;
   Bearer on management paths with `IDENTITY_MODE=disabled` still 401s; an
   environment key on a console route still 401s; `x-api-key` behavior
   byte-identical throughout.
3. **The role matrix across the route tables.** The `s.handle` role
   parameter; annotations per the matrix; per-resource tests (viewer 403 on
   every mutation, developer 403 on every credential surface, admin green;
   the key lane untouched by all of it). Wire-compat rung: the `/v1` route
   table diff is empty; no response shape changes.
4. **Deployment wiring.** The compose `iam` profile with the hardened Casdoor
   seed; helm `identity.*` values; the GCP Terraform IAP variables and docs;
   docs/self-hosted-security.md's SSO section.
5. **The api-key issuance surface** — `admin`-gated minting of named,
   principal-owned management keys, retiring hand-edited `CONTROLPLANE_API_KEY`
   rotation as the only path. **Gated on a live observation** of the
   reference console's key-management dialect (the standing convention from
   plan 30: console-facing endpoints mirror the observed reference path, no
   invented naming) — needs a recording session like 2026-08-10's; if the
   observation cannot be made, this slice moves to its own follow-up issue
   rather than shipping an invented dialect.
6. **Acceptance + archive.** Compose stack with the `iam` profile up; a real
   token minted from the bundled Casdoor (scripted code+PKCE against its OP
   endpoints); the chain proven end to end: viewer token 403s a mutation →
   admin token issues an environment key over the console API → a real
   `ant beta:worker poll --environment-key …` authenticates with it; recorded
   in docs/HISTORY.md, plan flipped to `archived`.

Console-repo work (its plan 08: the browser code+PKCE flow, forwarding the
user's token instead of the server-held management key, role-aware UI) starts
after slice 2 merges and is tracked there, not here.

## Verification

- Per slice: `make verify` (no cached results), the verifier subagent before
  any done-claim, per CLAUDE.md.
- Wire-compat rung: the `/v1` route-table diff stays empty across all slices;
  `ant beta:agents/environments/sessions` and `ant beta:worker poll` against
  the local server behave identically with identity disabled and enabled.
- Security-sensitive slices (1, 2) get the full dual review regardless of
  diff shape; slice 6's acceptance transcript is the plan's exit criterion.

## New DIVERGENCES entries (to record as they land)

- **Deliberate**: the reference API host serves `POST /v1/oauth/token`
  (authorization_code + refresh_token + jwt-bearer; pkg/cmd/cmd_auth.go,
  sdk config/federation.go) — this platform does not; `ant auth login`,
  OAuth profiles, and OIDC federation against it are unsupported (Roadmap).
  `/v1` management calls here accept the deployment IdP's JWTs as Bearer
  where the reference accepts its console-OAuth tokens — both off-wire to
  every documented CLI/SDK management and worker flow, which ride
  `x-api-key`/environment keys.
- **Deliberate**: a three-role model (`admin`/`developer`/`viewer`) where the
  reference has seven console roles; roles bind to the deployment IdP's
  claims, not to a platform user store.
- **Deliberate**: `trusted_proxy` mode has no reference analog (the
  reference is never deployed behind a customer's IAP); it exists for the
  self-hosting goal.
- **INFERRED**: which reference console roles gate "Generate environment
  key" is undocumented; `admin`-only here is a local judgment (the
  reference's Developer role *can* manage API keys, so a future recording
  may justify relaxing the env-key gate to `developer`).

## Roadmap (recorded, deliberately not built)

- **`ant auth login` compatibility** — the full obligation list is known:
  a console host serving `GET /oauth/authorize` and the
  `/oauth/code/callback?app=anthropic-cli` manual-code page; `POST
  /v1/oauth/token` with all three grants and the `organization`/`account`/
  `workspace` response blocks (login hard-fails without a bound workspace);
  `anthropic-beta: oauth-2025-04-20` and `anthropic-workspace-id` on
  authenticated requests.
- **RFC 7523 federation** (`fdrl_` rules, service accounts) — the
  reference-native path from an enterprise IdP to a *machine* credential.
- **A `/v1/organizations/*` Admin API** with an admin-key credential class,
  mirroring the reference's documented surface.
- **Multi-tenant activation** (#56): principal-scoped tenancy over the
  reserved columns; sized in Out of scope.
