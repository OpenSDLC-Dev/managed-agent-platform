---
status: in-progress
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
of three roles (`admin` / `developer` / `viewer`) enforced per route. Every
documented CLI and SDK flow — management `x-api-key`, worker environment keys
— behaves identically before and after, and the browser flow never touches
the platform; the identity lane's one observable `/v1` addition (a Bearer JWT
accepted where today only environment keys ride Bearer) is invisible to those
flows and lands as a declared DIVERGENCES entry. The work spans two repos: this
plan covers the platform half (verifier, principals, roles, deployment
wiring); the console half — the browser OAuth dance and forwarding the user's
token instead of a server-held god key — is managed-agent-console's plan 08,
which consumes what lands here and must trail it.

## Scope decisions settled with the user on 2026-08-12

1. **Default bundled IdP: Casdoor, hardened.** The core stays a generic OIDC
   relying party with **no vendor SDK**, built directly on `go-jose/v4`.
   *(Amended 2026-08-12, during slice 1, from "`coreos/go-oidc` +
   `golang.org/x/oauth2`".* `casdoor-go-sdk` was and stays excluded: its JWT
   verification pins a static certificate where a JWKS follower rotates.
   `coreos/go-oidc` is excluded too, on this plan's own terms — its
   `RemoteKeySet` has no cache TTL, and the Architecture section below
   requires a key the provider has removed to stop verifying within a
   minutes-order bound; its single `ClientID` also cannot express the
   multi-audience/`azp` rule below, so adopting it would mean writing the
   security core anyway *plus* carrying a new module. `golang.org/x/oauth2`
   is unneeded because the browser flow lives in the console repo.
   `go-jose/v4` was already in the controlplane binary transitively, so the
   direct dependency adds no module, and its parser takes the signature
   allowlist as a required argument — `alg:none` and HS256 die before any key
   lookup. The rejected options and their evidence are recorded in
   docs/HISTORY.md.)* Casdoor is a *deployment default*, not a code dependency: any
   compliant IdP (Keycloak, Entra ID, Cognito, Google) replaces it by config.
   The bundle ships with the hardening posture in Architecture — pinned
   v3.152.0, zero upstream providers, single organization, token-exchange
   grant disabled, SAML routes blocked — because of CERT/CC VU#780781 (see
   Ground truth; the risk is stated in docs, not hidden). It ships in two of
   the three deployment targets, settled with the user 2026-08-12: **compose**
   (the `iam` profile) and **Helm** (first-party templates behind
   `casdoor.enabled`, default off, because the on-prem cluster is this
   project's core deployment and needs the default IAM too). **GCP uses
   Google's own IdP and SSO** — Cloud Identity Platform or Workspace behind
   IAP — so no Casdoor is deployed there.
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
   out (the user: not needed now); it stays tracked by #56, and its sizing is
   recorded under Out of scope. `ant auth login` compatibility is also out —
   recorded as a divergence with its full obligation list (Out of scope), not
   built.

## Out of scope

- **Multi-tenant activation.** The reserved `org_id`/`workspace_id`/
  `project_id` columns stay at their single-tenant defaults. (Sized during
  planning: ~13 handler files with 60–80 scoped statements, ~6 create-time
  cross-reference checks, one worker-lane policy reversal — env-lane skill
  reads are deliberately global today (server.go:329–360) and become a
  cross-tenant leak — plus the `api_keys_one_live` rebuild that 0013:43–48
  pre-announces, with tests expected at 2–3× the production diff. No
  structural obstacle; this plan's principals and key-issuance surfaces are
  its prerequisites.)
- **`ant auth login` against this platform.** Deliberately not taken on; the
  obligation list is recorded here because the DIVERGENCES entry needs it: a
  console host serving `GET /oauth/authorize` and the
  `/oauth/code/callback?app=anthropic-cli` manual-code page; `POST
  /v1/oauth/token` with all three grants (authorization_code, refresh_token,
  RFC 7523 jwt-bearer — the last also being the reference-native path from an
  enterprise IdP to a *machine* credential) and the `organization`/`account`/
  `workspace` response blocks (login hard-fails without a bound workspace);
  `anthropic-beta: oauth-2025-04-20` and `anthropic-workspace-id` on
  authenticated requests. Doing any of this becomes work only through a
  GitHub issue.
- **End-user identity.** Design principle 5 stands: sessions stay
  user-agnostic; end-user ↔ session ownership remains an application-layer
  concern. This plan authenticates *operators of the platform*, nothing else.
- **Fine-grained / resource-level authorization** (Casbin-style policies,
  per-key scoping, `authorization_details`): three fixed roles only.
- **Casdoor management-plane integration** (auto-provisioning users via its
  API, SCIM, consuming `/api/enforce`): the platform never calls the IdP —
  it only verifies tokens.
- **A `/v1/organizations/*` Admin API** with an admin-key credential class
  (the reference's shape for org management): not in this plan; a future
  GitHub issue if ever.

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
  per week (v3.152.0, 2026-08-11, is the latest at writing — v3.151.0 landed
  the same day).
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
  not a separate permission tier" (docs/ARCHITECTURE.md:821–824). The house
  error set (`errInvalid`/`errConflict`/`errNotFound`/`errAuth`) has no 403 —
  a `permission_error` shape is new with this plan.
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
  real OAuth flow later (console OIDC is its own effort, console plan 08 —
  …)".
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
verifies a compact JWT — signature via JWKS (kid-indexed cache with a
bounded lifetime: refreshed on unknown `kid` under a rate limit, expired on a
minutes-order TTL so a key the IdP removes stops verifying within that bound
instead of living in the cache forever, and a `kid` still absent after a
fresh fetch is rejected; fetches carry a deadline and a response-size cap and
coalesce single-flight, and a failed refresh is the same uniform 401; no
static certs; every issuer/discovery/key URL is `https://`, loopback excepted
for tests — the rule the reference SDK applies to its own endpoints,
internal/auth/https.go), `iss` exact, `aud` contains, non-empty `sub` and
`exp` required, `exp`/`nbf` with ≤60s leeway, `azp` checked against the
expected client when `aud` carries multiple values, algorithm from an
explicit allowlist (RS256/RS512/ES256/ES384/ES512 — never `none`, never HS*)
— and returns a verified claim set. Claim mapping then produces the identity:
subject (`sub`), email and display name (claim names configurable), and
**roles** — a configurable claim name (default `roles`; dotted paths allowed
for nested claims, scalar and array values both accepted) whose values pass
through an explicit value→role map: unmapped values drop, and when several
map, the principal holds the single strongest role by the fixed order
`admin` > `developer` > `viewer` — never claim order, never map iteration
order. No mapped value ⇒ no role ⇒ every role-gated route answers 403. Tokens
are never logged; verification failures are uniform 401s
(`authentication_error`) with no oracle distinguishing expired / bad
signature / wrong audience.

**Mode A — `oidc` (the RP seam).** Config: issuer URL, audience, claim
mappings. Startup runs `/.well-known/openid-configuration` discovery against
the issuer and fails if discovery does; setting `IDENTITY_OIDC_JWKS_URL`
skips discovery entirely — issuer + audience + JWKS URL are then the whole
contract, for OPs whose discovery is absent or wrong. **The accepted credential is pinned,
not inferred: a JWT whose `aud` contains `IDENTITY_OIDC_AUDIENCE` —
canonically the OIDC ID token of the deployment's console application**
(guaranteed JWT-shaped by OIDC Core, where an OAuth access token may be
opaque; an IdP that mints JWT access tokens carrying that audience works
identically, but nothing assumes one). The platform never runs the browser
flow — the console BFF does authorization-code + PKCE against the IdP
(console plan 08) and forwards the user's token per platform call as
`Authorization: Bearer <jwt>`; the platform statelessly verifies. The
recipient question is answered by configuration: the deployment registers
that OIDC client *for this platform's console*, `IDENTITY_OIDC_AUDIENCE`
names it, and a token minted for any other client fails `aud` — within the
deployment the forwarded ID token is a bearer credential, bounded by TLS and
the IdP's token lifetime, which the Casdoor seed keeps short. Headless humans
run the same code flow with any OIDC-native tool against the same client and
curl with the resulting ID token. One mode covers Casdoor, Keycloak, Entra
ID, Cognito, accounts.google.com, and any other compliant OP.

**Mode B — `trusted_proxy` (the vendor-terminated seam).** Config: assertion
header name + verifier settings, with one shipped preset — `gcp-iap`
(`x-goog-iap-jwt-assertion`, ES256, `iss https://cloud.google.com/iap`,
gstatic JWK set, audience `/projects/N/global/backendServices/ID`) — plus a
`custom` escape hatch for any proxy whose assertion is a JWT against a JWKS.
An `aws-alb` preset is deliberately **not** shipped: ALB's assertion is not
JWKS-shaped (a per-`kid` PEM endpoint, signer/issuer claims in the JWT
*header*, and a mandatory expected-signer ARN check — the 2024 ALBeast
bypass, CVE-2024-8901, is exactly apps skipping it), so it needs its own key
source and config, and gets a GitHub issue when someone needs it. A missing
or invalid assertion is 401; the unsigned convenience headers are never read.
The supported topology is stated, not assumed: **the proxy fronts the control
plane's own backend service** — the console reaches the platform through the
same protected path, so every request carries an assertion audience-bound to
the *platform's* backend (`IDENTITY_PROXY_AUDIENCE`), and an assertion minted
for some other backend fails `aud` by design; machine traffic (`/v1` keys,
workers) rides a separate, non-proxied backend at the load balancer. In this
mode the console is **not** a proxying BFF for platform calls: the browser
calls the platform's protected backend directly, so the assertion IAP mints
names the human — a server-to-server BFF hop would make IAP re-authenticate
the *BFF's* workload identity and collapse every user onto one principal
(and forwarding the console backend's own assertion fails `aud` here by
design). Console plan 08 carries this as its Mode B shape.
GCIP-behind-IAP needs nothing more; direct-GCIP login UI is a console
frontend option, never a Go-core concern.

Config (all `IDENTITY_*`, controlplane only):

| Variable | Meaning |
|---|---|
| `IDENTITY_MODE` | `disabled` (default) · `oidc` · `trusted_proxy` |
| `IDENTITY_OIDC_ISSUER` / `IDENTITY_OIDC_AUDIENCE` | Mode A: discovery root; expected `aud` |
| `IDENTITY_OIDC_JWKS_URL` | optional override when discovery is absent/wrong |
| `IDENTITY_PROXY_PRESET` | Mode B: `gcp-iap` · `custom` |
| `IDENTITY_PROXY_HEADER` / `_ISSUER` / `_AUDIENCE` / `_KEYS_URL` / `_ALGS` | Mode B `custom` fields |
| `IDENTITY_CLAIM_ROLES` / `IDENTITY_CLAIM_EMAIL` / `IDENTITY_CLAIM_NAME` | claim names (defaults `roles` / `email` / `name`) |
| `IDENTITY_ROLE_MAP` | value→role pairs, e.g. `platform-admins=admin,eng=developer,everyone=viewer` |

`IDENTITY_MODE=disabled` (the default) is byte-for-byte today's platform: the
OIDC lane does not exist, a Bearer on a management path stays a 401, and
`x-api-key` remains the only management credential. Misconfigured identity
fails startup, not open: mode set with the issuer unreachable, an empty role
map, a malformed pair, a duplicate source value, or a target outside the
three roles are all boot errors — never silently converted into universal
403s.

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
```

- Principals are **JIT-provisioned**: the first verified request upserts by
  `(issuer, subject)` and refreshes `email`/`display_name`/`last_seen_at`.
  No admin pre-registration; authorization comes from claims, so a row is
  identity bookkeeping, not an authority grant. Roles are **not stored** —
  the IdP stays authoritative per request.
- Retention is stated, not implied: the row holds only what audit needs
  (issuer, subject, email, display name, timestamps — nothing else is
  persisted from the token). Revoking the human at the IdP ends platform
  access when their token expires — nothing platform-side keeps them in. An
  operator may DELETE a stale row at any time: `created_by` is a plain text
  column, not a foreign key, so audit history survives as an opaque id. The
  SSO docs section says all of this — including the deliberate absence: the
  platform ships **no** timer that deletes principals. Retention is a policy
  choice that differs per deployment (an erasure regime wants the row gone
  soon; an audit regime wants it stable while `created_by` still points at
  it), and either default is silently wrong for the other; so the platform
  keeps the data minimal, documents the `last_seen_at`-based DELETE an
  operator runs on their own schedule, and leaves the schedule to them.
- `principal_` joins the private-id family (`envkey_`, `apikey_`, `gtk_`) —
  **not** `domain.knownPrefixes`; it never appears on a `/v1` path.
- `api_keys` is untouched here: key ownership (`principal_id`) arrives only
  with slice 5's own migration, alongside whatever the observed dialect
  requires — nothing lands ahead of the slice that needs it (migrations are
  immutable once merged), and a key minted by a key-lane caller with no
  principal would carry NULL ownership: ownership is audit metadata, never
  authority.
- `sessions.created_by` now records whichever principal acted: the api-key
  row id on the key lane (unchanged) or the `principal_…` id on the identity
  lane. Still audit-only, still never rendered.

### Dispatch — one new lane, carved at the single choke point

`dispatchAuth` gains an identity lane; nothing else moves. The default
(management) arm becomes credential dispatch:

- non-empty `x-api-key` → `requireAPIKey`, **full authority, no role model**
  — the machine key's semantics are frozen (bootstrap, CI, BYO automation,
  and the reference's own model: a key *is* its authority). This is the one
  way to reach an `admin` route without an `admin` claim, and it is
  deliberate, not a gap in the gate: in an SSO deployment **no browser
  session and no console process holds a management key** (decision 2 — the
  BFF forwards the user's token instead of acting with a god key), so the
  key lane is reachable only by whoever holds the deployment's root
  credential, which is what "root credential" means. Narrowing it further is
  slice 5's job (named, principal-owned keys) and, past that, an
  admin-key credential class (Out of scope);
- else, when `IDENTITY_MODE` is not `disabled`, the identity lane —
  `IDENTITY_MODE=oidc` takes an `Authorization: Bearer` with a JWT
  silhouette (two dots); `IDENTITY_MODE=trusted_proxy` takes the configured
  assertion header instead and ignores Bearer entirely — then: verify,
  resolve principal, attach `{principal_id, roles}` to context, enforce the
  route's role — **default-deny: a route with no explicit role annotation
  answers 403 on this lane**, so the lane is fail-closed from the moment it
  exists. The two modes are mutually exclusive by config; neither ever falls
  back to the other;
- else → 401 exactly as today.

On the dual-auth worker paths, discrimination extends the existing rule:
`sk-map-env01-`-prefixed Bearer → environment-key lane; JWT silhouette →
identity lane (a grandfathered pre-0021 environment key is caller-supplied
text that could in principle contain two dots — it would misroute to a 401,
fail-closed; docs/self-hosted-security.md §6 already tells operators to
reissue those). Precedence in `trusted_proxy` mode is explicit for the same
reason: environment-key paths and `x-api-key` resolve on their machine lanes
first, the assertion is consulted only after both, and an assertion riding
alongside a machine credential never vouches for it — each collision pair is
pinned by a test. The `/api/` console namespace gets the explicit
dispatch arm consoleapi.go:24–37 said a future lane must bring: same
credential dispatch (management key or identity), so the plan-30 routes keep
working for headless operators while gaining the role gate.

### Roles and the enforcement point

Role annotations live in the route table — `NewHandler` (server.go:50–196) is
the one place every route is declared, and the `s.handle`/`s.handleNoContent`
adapters (server.go:403–424) gain a minimum-role parameter, so each
registration carries its requirement next to its path (the pattern
`requireEnvironmentKeyForSession` set: authorization checked where the route
is defined). Three identity-reachable routes are registered outside those
adapters — the streaming handlers `GET /v1/sessions/{id}/events/stream`,
`GET /v1/skills/{id}/versions/{version}/content`, and
`GET /v1/files/{id}/content` (server.go:73, 103, 109) — and each carries the
same minimum-role check explicitly (all three are `viewer` reads); a
completeness test walks every registration — the `/v1` table and the `/api/`
console routes alike — and fails on any identity-reachable route without a
role, with the lane's default-deny as the backstop beneath it. (`GET
…/work/poll` is also adapter-free but lives on the environment-key lane,
which identity dispatch never reaches.) The matrix:

| Role | May |
|---|---|
| `viewer` | GET/HEAD on every management and console resource **except the environment-key routes** (vault-credential reads render sealed metadata only, so they stay viewer-safe; the env-key section is gated whole, as the reference console gates its equivalent page) |
| `developer` | viewer + all resource CRUD and session lifecycle: agents, environments, sessions (create/events/interrupt/archive), skills, files, session resources, and vault CRUD (the container, not its credentials) |
| `admin` | developer + the credential surfaces, enumerated: environment-key issue/list/revoke (the plan-30 routes — the reference gates its "Generate environment key" behind Console roles; ours now has the gate); vault-credential create/update/delete/archive and `mcp_oauth_validate` (server.go:87–93); the slice-5 api-key surface |

Checks apply **only to the identity lane** (the key lane has no roles to
check). Denial is the new house 403 — `{"type":"error","error":{"type":
"permission_error","message":"…"}}` — joining the house error set in
errors.go, mirroring the reference's error taxonomy (its scope
failures are 403 `permission_error`). Uniform per lane: the message names the
required role, never the caller's.

### The Casdoor bundle — hardened by default

`deploy/compose` gains an optional `iam` profile: a pinned `casbin/casdoor`
image — **v3.152.0**, the latest release at writing and far past the
verified-fixed v2.387.0; bumped deliberately, never floating — backed by its
own database in the existing Postgres
container, seeded with: one organization, one application (the console as
OIDC client — code + PKCE, the console's redirect URL), **zero upstream
providers**, public signup off, the token-exchange grant off, and a role
claim (`roles`) emitted in tokens so `IDENTITY_ROLE_MAP` has something to
map.

**The bootstrap contract, verified against `casdoor@master` on 2026-08-12**
(the mechanics are named here because two of them are load-bearing and a
plausible guess gets them backwards). Casdoor reads `conf/app.conf`, but
`conf.GetConfigString` consults `os.LookupEnv` first for every key, so the
profile configures it entirely through environment variables of the same
camelCase names and mounts no config file: `driverName=postgres`;
`dataSourceName=…dbname=casdoor` — the separate `dbName` key is appended to
the DSN for MySQL only, so Postgres must repeat the database inside the DSN;
`origin` set to the **proxy's** external URL, because that value becomes the
`iss` claim and must equal `IDENTITY_OIDC_ISSUER`; and the entrypoint's
`--createDatabase=true` so the database exists on first boot. Seed data is a
checked-in `init_data.json` mounted at `/init_data.json` (the image's
working directory is `/`; the path comes from the `initDataFile` key). The
two load-bearing settings: **`initDataNewOnly=true`** — the shipped default
is `false`, and with `false` every entity in the file is deleted and re-added
on *every* start, so an unattended restart silently wipes operator edits;
and **`tokenFormat: "JWT"`** (the default) — `JWT-Standard` drops `roles`,
`permissions`, and `groups` from the token entirely, which would leave
`IDENTITY_ROLE_MAP` nothing to map. The seeded application sets
`enableSignUp: false` and a `grantTypes` list without
`urn:ietf:params:oauth:grant-type:token-exchange`; `authorization_code`
stays enabled regardless of that list (`IsGrantTypeValid` special-cases it),
which is the grant we want. `tokenSigningMethod` stays `RS256`, inside the
verifier's allowlist.

The `iam` profile fronts Casdoor with a minimal reverse proxy — an enforced
control, not advice: the browser must reach Casdoor to log in, so
internal-only networking is not available; Casdoor's own port (8000, one
process serving both API and UI) stays unpublished, only the proxy's is
exposed, and the acceptance run curls the blocked routes expecting the
refusal. The block list is verbatim from `routers/router.go`: `GET
/api/get-saml-login` and `POST /api/acs` (the SP role, where five of the nine
CVEs live), `GET /api/saml/metadata` and `* /api/saml/redirect/{owner}/
{application}` (the IdP role we do not use), and the
`/cas/{organization}/{application}/…` family including `POST …/samlValidate`.
docs/self-hosted-security.md gains an SSO section stating the
VU#780781 posture plainly: which CVEs the configuration voids, which the
pinned version fixes (9090), and that enterprises federating their own IdP
should point `IDENTITY_OIDC_ISSUER` straight at it — the bundled Casdoor is
a local-account IdP, not a federation hub.

**Helm carries the same bundle, values-gated and off by default.** The chart
always injects `identity.*` (mode/issuer/audience/claims/role map) into the
controlplane Deployment; `casdoor.enabled` (default `false`) additionally
renders a thin set of **first-party** templates — Deployment, Service, a
Secret, and a ConfigMap carrying the same `init_data.json` — driven by the
same environment contract the compose profile uses (`initDataNewOnly=true`
and `tokenFormat: "JWT"` included), with `origin` pointed at the chart's
ingress host. First-party rather than the upstream `casdoor-helm` subchart,
whose QA shipped a release that ignored its Postgres values and silently fell
back to SQLite: an IdP that quietly loses its user store is worse than one we
render ourselves in fifty lines. The reason the chart carries it at all is
this project's own premise — the target deployment is a private cluster with
no cloud IdP, and "the platform's default IAM works out of the box" has to
mean Helm there, not only compose on a laptop. Owning the templates means
owning their lifecycle (image tag, database, secret, CVE tracking), which the
SSO docs state rather than imply. The SAML block is enforced here too, in the
shape Kubernetes gives: the chart's Ingress carries the same deny rules for
`POST /api/acs`, `GET /api/get-saml-login`, and the SAML-IdP/CAS paths, the
Service is `ClusterIP` (never `NodePort` or `LoadBalancer`), and a
NetworkPolicy admits pod ingress only from the ingress controller — so a
browser reaches Casdoor exclusively through the path that denies those
routes. Both chart states go through `helm lint` and a render assertion, the
enabled one asserting the deny rules and the NetworkPolicy are present.

**On GCP the identity provider is Google's, not ours.** `deploy/gcp` wires
`trusted_proxy` + `gcp-iap` in front of the control plane, with Cloud Identity
Platform or Workspace behind IAP doing the SSO; no Casdoor is deployed there
and `casdoor.enabled` stays `false`. The Terraform grows the matching IAP
variables (backend-service audience, the OAuth brand/client, the IAM bindings
that admit users) and the docs to go with them.

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
   discovery, the `gcp-iap` preset, and config parsing — pure
   library + contract tests against a local fake IdP (httptest JWKS server:
   key rotation, unknown kid, wrong aud/iss/alg, expiry skew, ES256+RS256,
   nested role claims). No route touched; `make verify` green proves nothing
   observable changed.
2. **Principals + the identity lane.** The migration; JIT provisioning;
   `dispatchAuth`'s credential dispatch and the `/api/` arm; dual-auth
   discrimination; the context principal; `created_by` widening; the
   `permission_error` envelope; the `s.handle`/`s.handleNoContent`
   minimum-role parameter lands here (the mechanism), with its first use: the
   console env-key routes require `admin` — and every other identity-reachable
   route **default-denies** until slice 3 annotates it, so the lane is born
   fail-closed and slice 3 relaxes rather than closes. Tests over real HTTP: a Casdoor-shaped and an IAP-shaped
   token both authenticate; wrong iss/aud/alg/expired all 401 uniformly;
   Bearer on management paths with `IDENTITY_MODE=disabled` still 401s; an
   environment key on a console route still 401s; `x-api-key` behavior
   byte-identical throughout.
3. **The role matrix across the route tables.** Annotating every route per
   the matrix — relaxing slice 2's default-deny — including explicit checks
   on the three adapter-free streaming routes, plus the completeness test
   over both route tables;
   per-resource tests (viewer 403 on every mutation and on the env-key list,
   developer 403 on every credential surface, admin green; the key lane
   untouched by all of it). Wire-compat rung: the `/v1` route table diff is
   empty; no response shape changes for key-authenticated callers.
4. **Deployment wiring.** The compose `iam` profile with the hardened Casdoor
   seed; the Helm `identity.*` values plus the `casdoor.enabled` first-party
   templates (default off, same hardened configuration, chart-lint and render
   tests for both states); the GCP Terraform IAP variables and docs, where
   Google's own IdP does the SSO and no Casdoor is deployed;
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
  the local server behave identically with identity disabled and enabled; the
  identity lane's Bearer acceptance on `/v1` is resolved against its
  DIVERGENCES entry, not waved through as invisible.
- Security-sensitive slices (1, 2) get the full dual review regardless of
  diff shape; slice 6's acceptance transcript is the plan's exit criterion.

## New DIVERGENCES entries (to record as they land)

- **Deliberate**: the reference API host serves `POST /v1/oauth/token`
  (authorization_code + refresh_token + jwt-bearer; pkg/cmd/cmd_auth.go,
  sdk config/federation.go) — this platform does not; `ant auth login`,
  OAuth profiles, and OIDC federation against it are unsupported (the Out of
  scope entry records the full obligation list).
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
