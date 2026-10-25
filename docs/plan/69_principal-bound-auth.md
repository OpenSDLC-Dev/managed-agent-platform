---
status: approved
---

# Principal-bound authentication: platform login, stored roles, no preset keys (plan 69)

The owner named two problems on 2026-10-24:

1. **Preset secrets.** Compose requires two of them, `CONTROLPLANE_API_KEY`
   and `BRAIN_API_KEY`; Helm requires the second one while the model gateway
   is on, which is its default. Each one is full management authority, and
   each one stays in the deployment's configuration for as long as the
   install runs. A leak of either one is a leak of everything.
2. **The console's path differs from the reference.** The console's server
   holds the platform key, or forwards the user's IdP token. The reference's
   browser sends only a session cookie.

The owner stated the requirements:

- There are two entry points to models, agents and the other resources:
  - a browser, through the console;
  - the Anthropic SDK.
- The browser path covers three kinds of login:
  - a simple password, for development and test;
  - a private IAM, such as Casdoor;
  - a cloud IAM, such as Google Cloud's.
- No API key is preset. The SDK path works only with a key that someone
  created in the console.
- A call between internal services carries the authority of the principal
  that started it.

The owner made the decisions below on 2026-10-24 and 2026-10-25. On
2026-10-25 a builder check and a code review read the draft, and the owner
settled the points they raised that change a security boundary. The
author settled the rest; those say so ("author's call"). "Left to the
builder" lists what neither settled.

## What the reference does

Sources:

- the public docs at platform.claude.com and the support centre, fetched
  2026-10-24 and 2026-10-25;
- anthropic-cli v1.42.0;
- the recordings in managed-agents-wire-recordings at b65b824;
- anthropic-sdk-go v1.70.1, the version go.mod pins.

What they say:

- **API keys** (docs `manage-claude/authentication`, `manage-claude/workspaces`):
  - A key goes in `Authorization: Bearer`. `x-api-key` is a legacy fallback
    that still works.
  - A key is personal (it acts as its user), a service-account key (it acts
    as the service account), or a legacy workspace key with no owner.
  - Keys are created only in the Console.
  - A key carries no scope. It acts with its owner's role.
  - Personal keys are archived when their user is removed, and archived keys
    are not restored.
- **Console login** is Google, an email link, or SSO (SAML or OIDC). There is
  no password. A member who first arrives through SSO gets the User role,
  and an IdP group can map to a role (support 13132885).
- **Roles are stored in the Console's member list.** An admin assigns them in
  the Console or through the Admin API (docs `manage-claude/admin-api`,
  `manage-claude/workspaces`; support 10186004):
  - user: the playground only. Cannot see API keys, usage or billing.
  - developer: manages API keys, vaults and credentials, and webhook
    endpoints. Views usage and session traces. Downloads files.
  - admin: everything above, plus members and their roles.
  - The other organization roles are claude_code_user, billing, the owner
    pair and, in the support centre only, limited developer. Workspace roles
    refine them per workspace.
  - A service account's organization role is developer or admin (docs
    `manage-claude/wif-reference`; the SDK's `BetaServiceAccount`).
- **No setting limits which models a role, a workspace or a key may use.**
  Two limits bound use instead. Neither may exceed the organization's limit
  (docs `manage-claude/workspaces`, `api/rate-limits`):
  - rate limits per workspace and per model: requests, input tokens and
    output tokens per minute;
  - a monthly spend limit per workspace.
- **The Console's own calls use a cookie, from the same origin:**
  - The playground sends `POST /api/organizations/<uuid>/proxy/v1/messages`
    with a Cookie and no `Authorization` or `x-api-key` (recording
    `2026-10-22-playground-protocol`).
  - The Console's private calls, the playground's among them, carry
    `x-organization-uuid` and `x-workspace-id` beside the cookie (recordings
    `2026-09-05` and `2026-10-22-playground-protocol`).
- **Scopes exist on OAuth tokens only** (docs `manage-claude/wif-reference`):
  - Workload identity federation exchanges an IdP JWT at
    `POST /v1/oauth/token` (the RFC 7523 `jwt-bearer` grant) for a
    short-lived `sk-ant-oat01-` token.
  - The token's scope is one of these:
    - `workspace:inference`: Messages, Models and the OpenAI-compatible
      endpoint;
    - `workspace:developer`: every non-administrative endpoint;
    - `workspace:manage_tunnels`;
    - `org:admin`: the Admin API.
  - The scope is a ceiling. The effective permission is the scope intersected
    with the service account's role. A request outside the scope gets 403.
    "Finer-grained scopes (by resource, or read versus write) are not
    available."
- **`ant auth login`** runs an authorization-code flow:
  - It goes to `{console}/oauth/authorize`, then `{api}/v1/oauth/token`.
  - It asks for `user:profile user:inference user:developer`
    (anthropic-cli `pkg/cmd/cmd_auth.go:35`).
  - No source says what each `user:*` scope admits. Recording request
    managed-agents-wire-recordings#66 asks.

## The platform as-is

- **Lanes.** `dispatchAuth` (`internal/api/server.go`) picks one lane per
  request:
  - **The management key** (`x-api-key`): full authority, no role.
    `apiKeyOffered` reads only `x-api-key`, so a key sent as a Bearer is
    refused. On the dual-auth routes, `dualAuth` sends any Bearer without an
    `x-api-key` to the environment-key lane.
  - **The identity lane** (`IDENTITY_MODE=oidc` or `trusted_proxy`):
    - The roles are viewer, developer and admin. They come from the claim
      `IDENTITY_CLAIM_ROLES` names, through `IDENTITY_ROLE_MAP`, on every
      request. A person whose claim maps to nothing gets 403 everywhere.
    - The platform stores no role (plan 31). `principals` (migration 0022)
      holds identity bookkeeping only.
  - **Machine credentials**, for one environment, one work item or one gate:
    `sk-map-env01-` keys, `wtk_` and `gtk_`.
- **Preset keys:**
  - The control plane does not start without `CONTROLPLANE_API_KEY`. It
    seeds that key into `api_keys`.
  - It seeds `BRAIN_API_KEY` too, when set, as the row named `brain`. Both
    keys have full management authority.
  - The model gateway takes `CONTROLPLANE_API_KEY` as its admin bootstrap
    key. It lets the bootstrap key and the brain key reach every model with
    no grant.
  - Compose requires both values from the operator. Helm requires the brain
    key while `modelgateway.enabled` is true, its default.
  - CD's post-deploy check (`.github/workflows/deploy.yml`) reads
    `controlplane-api-key` back and calls `GET /v1/agents` with it.
- **The gateway** (plan 59):
  - It refuses any other key that has no key policy ("this API key has no
    model grant").
  - A key policy (`key_policies`) names the aliases the key may use. It can
    also set RPM and TPM limits.
  - The admin API (`/admin/v1`) takes the bootstrap key or an operator token.
    An operator token is an IdP token: viewer and developer can read, admin
    can write.
- **The console** (managed-agent-console) is a Next.js server that proxies
  the browser's calls:
  - In password mode or open mode it sends `PLATFORM_API_KEY`, which is
    `CONTROLPLANE_API_KEY`.
  - In OIDC mode it forwards the user's ID token as a Bearer.
  - It refuses `trusted_proxy` at boot.
- **The brain's model-turn claim** is one statement (`queue.Claim`), with no
  transaction around it. `wtk_` is minted elsewhere: in the worker poll's
  transaction (`claimWork` in `internal/api/workapi.go`), and only for a
  session with a memory store.
- **Ownership is audit only.**
  - `api_keys` records who issued a key, not who owns it.
  - `sessions.created_by` and `deployments.created_by` record the acting
    credential. They authorize nothing (design principle 5).
- **GCP staging** is plain HTTP on a bare LoadBalancer IP, with
  `identity.mode` empty. Its values file prepares `trusted_proxy` with
  `roles: email` and a role map for when IAP fronts it.

## Decisions

### 1. No preset credential, and how the first admin appears

- These four credentials are removed:
  - `CONTROLPLANE_API_KEY`;
  - `BRAIN_API_KEY`;
  - the gateway's bootstrap key;
  - the gateway's operator token.
- **There is no transition release.** Slices 1 to 6 land with no release
  between them (owner, 2026-10-25). If a fix release has to be cut in that
  time, its upgrade note states the intermediate state as it is. Under
  SemVer 0.x the removal is a minor bump (docs/RELEASING.md).
- **Every management API key that exists before this plan stops working at
  slice 6**, whoever issued it. Environment keys (`sk-map-env01-`) are not
  affected. People create new keys in the console. The upgrade note says
  so. Work that an old key started:
  - keeps running until slice 6 (decision 8);
  - at slice 6, a session started by an old key stops at its next model
    turn, and a deployment created by one fails at its next run. An admin
    can point such a deployment at a service account.
- **The first admin is created with a one-time setup code:**
  - The control plane prints a code to its log at start-up, but only while
    the install has never had an admin. A stored flag records that the
    first admin exists, so a later "no admin" state never reopens the
    door (author's call).
  - Each replica prints its own code. Any unexpired code works, and
    redeeming one spends them all (author's call).
  - In OIDC and IAP modes, the first person who signs in and presents a
    code becomes admin.
  - In password mode, no account can exist before an admin does. So
    presenting a code creates the first local account, with the password
    given at that moment, as admin (author's call).
  - Whoever can read the logs before the first admin exists can claim the
    role. The security doc says so, and says to redeem the code at once.
- **There is no built-in recovery.** If no admin can sign in (the IdP is
  down, the last admin left, a password is lost), the operator edits the
  database by hand. docs/self-hosted-security.md will say how (slice 1).
- **The last admin is protected** (author's call). No admin can demote or
  remove the last active admin, themselves included. An IdP mapping can
  still demote them (decision 4); recovery is then a database edit.

### 2. The platform owns login

The control plane runs every login and sets the session cookie. The console
renders pages and holds no secret. A deployment uses one of three login
modes:

- **Password.** Local accounts, one for each person:
  - An admin creates each account, and its first password, in the console.
    The first account is the exception (decision 1).
  - The mode is for development and test only. The control plane refuses it
    unless the deployment says it is a development install.
- **OIDC.** The control plane is the relying party of an authorization-code
  flow with PKCE, against Casdoor or another OIDC provider. It verifies the
  ID token with `internal/identity`'s existing discovery and JWKS code.
- **IAP.** A cloud identity-aware proxy in front does the authentication.
  The control plane verifies the proxy's signed assertion. This is the
  existing `trusted_proxy` mode and its `gcp-iap` preset.

The design adds one kind of configured secret, and only one: the OIDC client
secret, where the IdP requires a confidential client. It grants no authority
on the platform. The session cookie carries an opaque session id that the
server looks up, so no cookie-signing key exists (author's call).

Until slice 2 ships login, "sign-in" in decision 4 means every verified
request on the identity lane.

### 3. One origin, and the cookie in two places only

- One origin serves the console, the control plane's private API and the
  gateway's admin API (author's call: the same entry point routes the
  gateway's admin paths to it).
- The session cookie is accepted on the private `/api/...` paths and on the
  gateway's admin API, and nowhere else. `/v1` never accepts it. This
  matches the reference, whose Console and API are on separate hosts.
- **Those two surfaces take the cookie only.** No API key reaches them
  (owner, 2026-10-25):
  - A leaked key cannot create keys, members or environment keys, and cannot
    repoint the gateway's providers or vendor credentials.
  - Issuing or revoking an environment key needs a person signed in to the
    console, including during an incident. The scripted issuance in
    docs/self-hosted-security.md §6 goes.
  - Automated fleets get no issuance route from this plan. Workload identity
    federation (decision 11) needs one, because its tokens are not cookies;
    that route is designed then.
- **`/v1` takes no human session.** The identity lane's IdP Bearer leaves
  `/v1` in slice 3, when owned keys replace it. A person's credential on
  `/v1` is then an API key. `/v1` keeps its machine credentials on their own
  routes: environment keys, `wtk_` and `gtk_`, and on the gateway's `/v1`
  the delegated inference token (decision 8).
- **A Bearer is told apart by its prefix** on the dual-auth routes (author's
  call): `sk-map-api01-` takes the API-key lane, and anything else stays on
  the environment-key lane, as today. A grandfathered environment key
  without a prefix therefore keeps working.
- The console's resource views go through a private proxy path,
  `/api/organizations/{org}/proxy/v1/...`, as the reference's playground
  does. When a playground exists, its calls take the same path, carry the
  signed-in person's authority, and reach the gateway with it.

### 4. Principals and stored roles

- A principal is a **user** or a **service account**. An admin creates
  service accounts in the console.
- A principal has one organization role, **user**, **developer** or
  **admin**, or none yet:
  - The role is stored in the platform. An admin assigns it in the console.
  - **A person whose first sign-in matches no mapping gets no role** (owner,
    2026-10-25, after the review). They exist as a member who waits for a
    role, and they see nothing until an admin assigns one. This keeps
    today's boundary on upgrade, where an unmapped person gets 403
    everywhere. It also keeps an open IdP, such as Google accounts, from
    admitting readers.
  - A service account is developer or admin, as in the reference.
- **The IdP mapping is optional**, in OIDC and IAP modes alike, on the claim
  `IDENTITY_CLAIM_ROLES` names (staging's values map emails). When one is
  configured, it applies at every sign-in:
  - A person a mapping matches has the role the IdP gives. The console
    cannot change that role.
  - A person whose role came from the mapping, and whom it no longer
    matches, goes back to no role.
  - A person no mapping has matched is managed in the console.
  - On upgrade, an existing `IDENTITY_ROLE_MAP` keeps working, so a mapped
    admin is still admin at their next sign-in. A map that says `viewer`
    means `user`.
- **The IdP reaches the platform only at sign-in.** A demotion or a removal
  there takes effect at the person's next sign-in. A person who uses only
  API keys never signs in, so their keys keep their stored role. An admin
  removes such a person in the console. The security doc says so.
  Automatic sync needs SCIM, which is out of scope (owner, 2026-10-25).
- **Removing a member** (author's calls, the last two as in the reference):
  - It is a lasting deactivation. A removed person who signs in again is
    refused, not re-created.
  - It wins over the mapping: a removed person whom a mapping still matches
    stays removed.
  - It archives every key the member owns.
  - Restoring the member restores no key.
- **Retention.** The principal DELETE that docs/self-hosted-security.md §8
  recommends must keep deactivated principals, and principals that own
  keys, sessions or deployments. §8 is rewritten in slice 1.
- Roles are organization-wide. There is no workspace layer.
- The existing viewer role folds into user. Plan 31's claim-derived roles end
  here.
- **No playground is built by this plan.** The owner wants one later.

| Role | May |
|---|---|
| (none yet) | Sign in, and see that an admin has to assign a role. Nothing else. |
| user | Read every resource: agents, environments, sessions and their events, deployments, files, skills, memory stores, vaults and credential metadata, dreams. Use the playground, once one exists. Create no API key. |
| developer | Everything user may. Create and manage their own API keys. Call every non-administrative endpoint, with every model. Create, change, archive and delete resources, including vault credentials. Read the gateway's configuration and usage. |
| admin | Everything developer may. Manage members, their roles and service accounts. Manage every API key. Issue and revoke environment keys. Change the model gateway's configuration and rate limits. Redact memory versions. |

Notes on the table:

- **user's read access goes beyond the reference.** The reference's user has
  the playground only.
- **developer's vault credentials follow the reference.** Limited developer
  and above manage vaults and credentials there (support 10186004). Here only
  admin does today.
- **developer reads the gateway's usage** (author's call), as today's viewer
  and developer do, and as the reference's developer views usage.
- **Environment keys stay admin-only.** No recording shows which reference
  role may issue them (managed-agents-wire-recordings#40).

### 5. API keys belong to principals

- A key is created only through the console's private API:
  - a developer or an admin creates keys for themselves;
  - an admin creates keys for a service account.
- A key carries no scope. It acts with its owner's role at the time of each
  request:
  - When the owner is removed, the key is archived (decision 4).
  - When the owner's role changes, the key's reach changes with it.
- `/v1` and the gateway's inference endpoints accept a key as `x-api-key` or
  as `Authorization: Bearer`, as the reference does.

### 6. Scopes, only where a credential needs one

- In this plan only the delegated inference token (decision 8) carries a
  scope: `workspace:inference`. Only the gateway's inference endpoints check
  it (author's call; CLAUDE.md: do not build ahead of the slice).
- A scoped credential gets its scope intersected with its principal's role.
  A request outside that gets 403 `permission_error`.
- Classes for the control plane's routes (`workspace:developer`,
  `org:admin`) come with workload identity federation, the first credential
  that needs them.
- The `user:*` scopes wait for recording #66.

### 7. Principle 5, amended

- A session is still not bound to an application's end user. That stays an
  application-layer concern.
- A session now records the principal that started it: the signed-in person,
  or the API key's owner.
- Internal calls made for the session act with that principal's authority.
- CLAUDE.md and AGENTS.md change in the slice that does this.

### 8. A delegated inference token replaces the brain key

- **Minting.** The brain's claim of a model turn mints a token for that turn.
  The claim is one statement today, so this changes the claim path: the
  mint goes inside the brain's own claim. It reuses the platform's one mint
  for internal bearers: `internal/gatetoken`'s, which `internal/worktoken`
  already reuses under another prefix.
- **The token** names the session's initiator as its subject and the brain
  as its actor. Its scope is `workspace:inference`. It is valid only while
  the claim holds.
- **Who may call models.** Through any credential except the console's
  cookie, only developer and admin. A user reaches a model only through the
  console's playground path, once it exists (decision 3).
- **The claim checks the initiator before it mints** (author's call). If the
  initiator is gone, has no role, or holds only user, the claim mints
  nothing and the turn fails. So a demoted initiator's sessions stop at
  their next model turn. The check also covers a route that skips the
  gateway: Helm with `modelgateway.enabled=false`, or compose with a direct
  providers file.
- **The gateway checks again on each call**, in the same lookup that
  authenticates the token: one joined query, not extra round trips (author's
  call).
- **Every session has an initiator from slice 4 on** (author's call):
  - Rows from before slice 4 are backfilled from `created_by` where it names
    a principal, and otherwise from the old key's row.
  - Until slice 6, an old key counts as a valid initiator, with today's
    authority. At slice 6 it does not (decision 1).

### 9. Deployments name their initiator

- By default, a deployment starts its sessions with its creator's authority.
- A deployment may name a service account instead, so that it outlives a
  change of staff. The service account's role must be no higher than the
  role of the person who sets it (author's call).
- If the initiator is gone, or may no longer call models, when a run fires,
  the run fails with a recorded error.
- Dreams follow the same rule (author's call).
- No recording shows the reference's rule.

### 10. The model gateway

- Every role that may call models may call every model.
- Model grants and key policies are removed. No allowlist replaces them. The
  bootstrap key's and the brain key's pass around grants goes with them.
- An admin sets rate limits per model for the whole organization: requests,
  input tokens and output tokens per minute. They replace the RPM and TPM
  limits of each key policy.
- A monthly spend limit comes later, because it needs a price for each
  model.
- The admin API takes the console cookie only (decision 3). Developer and
  above read; admin writes.

### 11. Later phases

These are not in this plan, but the plan's shape keeps room for them:

- **Workload identity federation:**
  - the OAuth token endpoint with the `jwt-bearer` grant;
  - federation issuers and rules;
  - short-lived, scoped tokens for service accounts;
  - the route classes of decision 6, and an issuance route for automated
    fleets (decision 3).
- **`ant auth login`:** the authorization-code flow and the `user:*`
  scopes, after recording #66.
- **The monthly spend limit.**

## Left to the builder

Neither the owner nor the author settled these. Each choice must keep the
decisions above.

- **The session:** its lifetime, and how logout works. The cookie is
  HttpOnly; it is Secure, except on a loopback development install; and its
  server-side session can be revoked, so that a removal or a role change
  takes effect before it expires.
- **Protection against cross-site requests** that carry the cookie. A
  cross-site form must not be able to make one. The reference's
  `x-organization-uuid` header is one way.
- **Password storage and login throttling.**
- **The private paths** for login, members, service accounts and rate
  limits:
  - Use a reference shape where one exists. The SDK's Admin API types
    (`BetaOrganizationUser`, `BetaServiceAccount`) are one.
  - Where none exists, register the choice in docs/DIVERGENCES.md.
- **How one origin is served** in compose and Helm: a reverse proxy, an
  ingress, or the console's rewrites.
- **The setup code's** length and lifetime: hours, not days.
- **How tests and CI get a key.** They use the same login and private API
  that a person uses, not a second path.
- **The OIDC client library.** `internal/identity` already verifies ID
  tokens. golang.org/x/oauth2 is an indirect dependency; if it is adopted
  for the code exchange, it stays in one package, as go-jose does.
- **The delegated token's** storage, and how the brain's provider sends it
  in place of a route's static `api_key`.
- **The slice order** below. Each slice keeps main deployable.

## Out of scope

- Workspaces.
- The roles that need workspaces or billing: limited developer,
  claude_code_user, billing, owner.
- SAML, SCIM (and with it any automatic sync from the IdP), App Attest and
  Admin API keys.
- Multi-tenant activation (#56).

## Slices

A suggested order. Each slice is one PR in this repository. When the console
changes, a companion PR goes to managed-agent-console. No release is cut
until slice 6 has landed (decision 1).

1. **Stored roles:**
   - roles on principals, including the no-role state;
   - the principal kind for service accounts;
   - the setup code, redeemed on the identity lane until slice 2;
   - the identity lane reads stored roles, and the mapping applies, in both
     modes, at every verified request;
   - deactivation is honoured (no API sets it yet);
   - viewer becomes user;
   - vault credentials open to developer.
2. **Platform login and the cookie:**
   - password (with the setup code's first account), OIDC and IAP login on
     the control plane;
   - the session cookie, on private paths and the gateway's admin API;
   - the private API for members and service accounts, with the last-admin
     guard and key archiving on removal;
   - the proxy path;
   - in the console: one origin, no `PLATFORM_API_KEY`, login, members and
     setup-code pages.
3. **Principal-owned keys:**
   - keys created from now on are bound to owners and act with their roles;
   - `x-api-key` and Bearer on `/v1`, with the prefix rule on dual-auth
     routes;
   - the identity lane's IdP Bearer leaves `/v1`;
   - keys from before this plan keep today's lane until slice 6.
4. **The initiator and the delegated token:**
   - sessions and deployments record their initiator, with the backfill;
   - the per-claim inference token, and the claim's initiator check;
   - the deployment's service-account rule;
   - `BRAIN_API_KEY` removed.
5. **The gateway:**
   - grants and key policies removed;
   - rate limits per model for the organization;
   - the admin API on the cookie;
   - the bootstrap key and the operator token removed.
6. **No preset key:**
   - `CONTROLPLANE_API_KEY` removed;
   - every management key from before this plan stops working;
   - compose, Helm, CI, the acceptance suite and the evals move to the setup
     code and login;
   - CD's post-deploy check, and `deploy/gcp/mode2-secret.sh`, move to a
     service-account key created in the console and kept as a CD secret
     (author's call);
   - GCP staging moves to OIDC, with Google accounts (`accounts.google.com`)
     as the IdP. That needs HTTPS first: Google refuses an `http` or bare-IP
     redirect URI, and the Secure cookie is not sent over `http`. The owner
     settles how staging gets HTTPS before this slice (2026-10-25), and the
     OAuth client is a cloud change the owner makes.

## Docs that move with the change

- **CLAUDE.md and AGENTS.md:** principle 5, and the wire rule "Auth:
  management via `x-api-key`".
- **docs/ARCHITECTURE.md:** the auth lanes and the gateway's auth.
- **docs/self-hosted-security.md:**
  - §6: environment keys are issued and revoked in the console only;
  - §8: the principal retention DELETE (decision 4);
  - §9 (SSO): who may sign in, the no-role first sign-in, the mapping, and
    that the IdP reaches the platform only at sign-in;
  - §10 (the management key's lifecycle), rewritten; it also says how to
    recover by editing the database.
- **docs/DIVERGENCES.md:**
  - New entries:
    - local password accounts;
    - the no-role first sign-in, and user's read access;
    - roles and limits at organization level, with no workspaces;
    - the private paths this plan adds, where no reference shape exists.
  - Converged:
    - the Bearer API key;
    - the console's cookie-authenticated private API;
    - model use governed by role and rate limits.
  - Rewritten or retired:
    - "Human authentication (`/v1` management paths and the `/api/`
      console namespace)". Its sentence "its console authenticates humans
      with the tokens that endpoint mints" has no source and goes.
    - "`sk-map-api01-` as the issued-key prefix, and `principal`/`api_key`
      actors where the reference says `user`".
    - "A key seeded from `CONTROLPLANE_API_KEY` is listed but not
      console-mutable".
    - "Memory-version `created_by` / `redacted_by` —
      `service_account_actor` is never emitted", once service accounts
      act.
- **README.md and the compose and Helm docs:** installing without a preset
  key.
- **Plan 31 (archived):** this plan supersedes it on two points. Plan 31
  kept roles in the IdP, and it gave the management key full authority.
  docs/HISTORY.md records the reversal.

## Verification

- Each slice writes its contract tests first. They cover the role matrix and
  every credential lane.
- The existing completeness test (`internal/api/rolematrix_test.go`) keeps
  failing on any identity-reachable route without a role.
- End to end, the real `ant` CLI runs against a fresh compose install. It
  uses a key created in the console, sent once as `x-api-key` and once as a
  Bearer.
- A compose install with no preset key in its configuration starts and
  passes its smoke test.
