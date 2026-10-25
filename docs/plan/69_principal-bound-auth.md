---
status: draft
---

# Principal-bound authentication: platform login, stored roles, no preset keys (plan 69)

The owner named two problems on 2026-10-24:

1. **Preset secrets.** The platform needs two of them, `CONTROLPLANE_API_KEY`
   and `BRAIN_API_KEY`. Each one is full management authority, and each one
   stays in the deployment's configuration for as long as the install runs. A
   leak of either one is a leak of everything.
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

The owner made the decisions below on 2026-10-24 and 2026-10-25. A few are
the author's, and say so ("author's call"). "Left to the builder" lists what
neither settled.

## What the reference does

Sources:

- the public docs at platform.claude.com and the support centre, fetched
  2026-10-24 and 2026-10-25;
- anthropic-cli v1.42.0;
- the recordings in managed-agents-wire-recordings at b65b824.

What they say:

- **API keys** (docs `manage-claude/authentication`, `manage-claude/workspaces`):
  - A key goes in `Authorization: Bearer`. `x-api-key` is a legacy fallback
    that still works.
  - A key is personal (it acts as its user), a service-account key (it acts
    as the service account), or a legacy workspace key with no owner.
  - Keys are created only in the Console.
  - A key carries no scope. It acts with its owner's role.
  - A personal key stops working when its user is removed.
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
    `manage-claude/wif-reference`).
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
  - The Console's other private calls carry `x-organization-uuid` and
    `x-workspace-id` beside the cookie (recording `2026-09-05`).
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
    refused.
  - **The identity lane** (`IDENTITY_MODE=oidc` or `trusted_proxy`):
    - The roles are viewer, developer and admin. They come from IdP claims
      through `IDENTITY_ROLE_MAP`, on every request.
    - The platform stores no role (plan 31). `principals` (migration 0022)
      holds identity bookkeeping only.
  - **Machine credentials**, for one environment, one work item or one gate:
    `sk-map-env01-` keys, `wtk_` and `gtk_`.
- **Preset keys:**
  - The control plane does not start without `CONTROLPLANE_API_KEY`. It
    seeds that key into `api_keys`.
  - It seeds `BRAIN_API_KEY` too, as the row named `brain`. Both keys have
    full management authority.
  - The model gateway takes `CONTROLPLANE_API_KEY` as its admin bootstrap
    key. It lets the brain key reach every model with no grant.
  - Compose and Helm require both values from the operator.
- **The gateway** (plan 59):
  - It refuses a key that has no key policy ("this API key has no model
    grant").
  - A key policy (`key_policies`) names the aliases the key may use. It can
    also set RPM and TPM limits.
  - The admin API (`/admin/v1`) takes the bootstrap key or an operator token.
    An operator token is an IdP token: viewer can read, admin can write.
- **The console** (managed-agent-console) is a Next.js server that proxies
  the browser's calls:
  - In password mode or open mode it sends `PLATFORM_API_KEY`, which is
    `CONTROLPLANE_API_KEY`.
  - In OIDC mode it forwards the user's ID token as a Bearer.
  - It refuses `trusted_proxy` at boot.
- **Ownership is audit only.**
  - `api_keys` records who issued a key, not who owns it.
  - `sessions.created_by` and `deployments.created_by` record the acting
    credential. They authorize nothing (design principle 5).

## Decisions

### 1. No preset credential

- These four credentials are removed:
  - `CONTROLPLANE_API_KEY`;
  - `BRAIN_API_KEY`;
  - the gateway's bootstrap key;
  - the gateway's operator token.
- There is no transition release. Under SemVer 0.x the removal is a minor
  bump (docs/RELEASING.md).
- **Every API key that exists before this plan stops working**, whoever
  issued it. People create new keys in the console. The upgrade note says
  so. Work that an old key started stops too:
  - a session started by an old key stops at its next model turn;
  - a deployment created by an old key fails at its next run, and an admin
    can point it at a service account.
- The first admin is created with a one-time setup code:
  - The control plane prints the code to its log at start-up, while no admin
    exists.
  - The first person who signs in and presents the code becomes admin.
  - The code is then spent.
- **There is no built-in recovery.** While an admin exists, no setup code is
  printed. If no admin can sign in (the IdP is down, the last admin left, a
  password is lost), the operator edits the database by hand.
  docs/self-hosted-security.md says how.
- The last admin cannot demote or remove themselves (author's call).

### 2. The platform owns login

The control plane runs every login and sets the session cookie. The console
renders pages and holds no secret. A deployment uses one of three login
modes:

- **Password.** Local accounts, one for each person:
  - An admin creates each account, and its first password, in the console.
  - The mode is for development and test only. The control plane refuses it
    unless the deployment says it is a development install.
- **OIDC.** The control plane is the relying party of an authorization-code
  flow with PKCE, against Casdoor or another OIDC provider.
- **IAP.** A cloud identity-aware proxy in front does the authentication:
  - The control plane verifies the proxy's signed assertion. This is the
    existing `trusted_proxy` mode and its `gcp-iap` preset.
  - The assertion carries no groups, so roles come from the platform's
    store.

### 3. One origin, and the cookie on private paths only

- The console and the platform's private API are served from one origin.
- The control plane accepts the session cookie on the private `/api/...`
  paths only, never on `/v1`. This matches the reference, whose Console and
  API are on separate hosts.
- **The private paths take the cookie only.** No API key reaches them:
  - A leaked key cannot create keys, members or environment keys for itself.
  - Issuing an environment key needs a person signed in to the console.
    Automated fleets wait for workload identity federation (decision 11).
  - The gateway's admin API is the exception (decision 10).
- `/v1` takes API keys only. The identity lane's IdP Bearer on `/v1` is
  removed.
- The console's resource views go through a private proxy path,
  `/api/organizations/{org}/proxy/v1/...`, as the reference's playground
  does. When a playground exists, its calls take the same path and reach the
  gateway with the signed-in user's authority, not with a service
  credential.

### 4. Principals and stored roles

- A principal is a **user** or a **service account**. An admin creates
  service accounts in the console.
- Each principal has one organization role: **user**, **developer** or
  **admin**:
  - The role is stored in the platform. An admin assigns it in the console.
  - A person who signs in for the first time through OIDC or IAP gets the
    user role. Here user reads every resource, so the deployer controls who
    may sign in, at the IdP or at the proxy. The owner kept this rule on
    2026-10-25, after the builder check raised it; the security doc says
    it.
  - In OIDC mode, an IdP group may map to a role. The mapping is optional.
    When one is configured, it applies at every sign-in:
    - A person a mapping matches has the role the IdP gives. The console
      cannot change that role, and a demotion at the IdP takes effect.
    - A person no mapping matches is managed in the console.
    - On upgrade, an existing `IDENTITY_ROLE_MAP` keeps working, so an OIDC
      admin is still admin at their next sign-in.
  - IAP assertions carry no groups, so every IAP user is managed in the
    console.
  - A service account is developer or admin, as in the reference.
- Removing a member is a lasting deactivation. A removed person who signs in
  again is refused, not re-created; only an admin restores them (author's
  call).
- Roles are organization-wide. There is no workspace layer.
- The existing viewer role folds into user. Plan 31's claim-derived roles end
  here.
- **No playground is built by this plan.** The owner wants one later. Until
  it exists, user calls no model.

| Role | May |
|---|---|
| user | Sign in to the console. Use the playground, once one exists. Read every resource: agents, environments, sessions and their events, deployments, files, skills, memory stores, vaults and credential metadata, dreams. Create no API key. |
| developer | Everything user may. Create and manage their own API keys. Call every non-administrative endpoint, with every model. Create, change, archive and delete resources, including vault credentials. |
| admin | Everything developer may. Manage members, their roles and service accounts. Manage every API key. Issue and revoke environment keys. Configure the model gateway and its rate limits. Redact memory versions. |

Notes on the table:

- **user's read access goes beyond the reference.** The reference's user has
  the playground only.
- **developer's vault credentials follow the reference.** Limited developer
  and above manage vaults and credentials there (support 10186004). Here only
  admin does today.
- **Environment keys stay admin-only.** No recording shows which reference
  role may issue them (managed-agents-wire-recordings#40).

### 5. API keys belong to principals

- A key is created only through the console's private API:
  - a developer or an admin creates keys for themselves;
  - an admin creates keys for a service account.
- A key carries no scope. It acts with its owner's role at the time of each
  request:
  - When the owner is removed, the key stops working.
  - When the owner's role changes, the key's reach changes with it.
- `/v1` and the gateway's inference endpoints accept a key as `x-api-key` or
  as `Authorization: Bearer`, as the reference does.

### 6. Scopes classify routes

- Every route belongs to one class. The class names are the reference's:
  - inference: `workspace:inference`;
  - non-administrative: `workspace:developer`;
  - administrative: `org:admin`.
- A credential that carries a scope gets the scope intersected with its
  principal's role. A request outside that gets 403 `permission_error`.
- In this plan only the delegated inference token (decision 8) carries a
  scope. Workload identity federation uses the same classes later.
- The `user:*` scopes wait for recording #66.

### 7. Principle 5, amended

- A session is still not bound to an application's end user. That stays an
  application-layer concern.
- A session now records the principal that started it: the signed-in person,
  or the API key's owner.
- Internal calls made for the session act with that principal's authority.
- CLAUDE.md and AGENTS.md change in the slice that does this.

### 8. A delegated inference token replaces the brain key

- When the brain claims a model turn, the claim also mints a token for that
  turn. It does this in the same transaction, as the claim mints `wtk_` now.
- The token names the session's initiator as its subject and the brain as
  its actor. Its scope is `workspace:inference`. It is valid only while the
  claim holds.
- The roles that may call models through the API are developer and admin.
- The claim checks the initiator before it mints the token (author's call):
  - If the initiator is gone, or now holds the user role, the claim mints
    nothing and the turn fails. A demoted initiator's sessions therefore
    stop at their next model turn.
  - This check also covers a route that does not go through the gateway:
    Helm with `modelgateway.enabled=false`, or compose with a direct
    providers file.
- On each call, the gateway checks the same two things again.

### 9. Deployments name their initiator

- By default, a deployment starts its sessions with its creator's authority.
- A deployment may name a service account instead, so that it outlives a
  change of staff.
- If the initiator is gone, or may no longer call models, when a run fires,
  the run fails with a recorded error.
- Dreams follow the same rule (author's call).
- No recording shows the reference's rule.

### 10. The model gateway

- Every role that may call models may call every model.
- Model grants and key policies are removed. No allowlist replaces them.
- An admin sets rate limits per model for the whole organization: requests,
  input tokens and output tokens per minute. They replace the RPM and TPM
  limits of each key policy.
- A monthly spend limit comes later, because it needs a price for each
  model.
- The admin API takes an admin's console cookie or an admin's API key. The
  reference's Admin API also takes a personal key.

### 11. Later phases

These are not in this plan, but the plan's shape keeps room for them:

- **Workload identity federation:**
  - the OAuth token endpoint with the `jwt-bearer` grant;
  - federation issuers and rules;
  - short-lived, scoped tokens for service accounts.
- **`ant auth login`:** the authorization-code flow and the `user:*`
  scopes, after recording #66.
- **The monthly spend limit.**

## Left to the builder

The owner did not settle these. Each choice must keep the decisions above.

- **The session cookie:**
  - its format, its lifetime and how logout works;
  - it is HttpOnly;
  - it is Secure, except on a loopback development install;
  - it is revocable on the server, so that a removal or a role change takes
    effect before the cookie expires.
- **Protection against cross-site requests** that carry the cookie. A
  cross-site form must not be able to make one. The reference's
  `x-organization-uuid` header is one way.
- **Password storage and login throttling.**
- **The private paths** for login, members, service accounts and rate
  limits:
  - Use a recorded reference shape where one exists.
  - Where none exists, register the choice in docs/DIVERGENCES.md.
- **How one origin is served** in compose and Helm: a reverse proxy, an
  ingress, or the console's rewrites.
- **The setup code:**
  - its length and its lifetime;
  - whether an operator may supply it for unattended installs.
- **How tests and CI get a key.** They use the same login and private API
  that a person uses, not a second path.
- **The OIDC client library.**
  - golang.org/x/oauth2 is already an indirect dependency.
  - If it is adopted, it stays in one package, as go-jose does.
- **The delegated token:**
  - its format and storage;
  - how the brain's provider sends it in place of a route's static `api_key`.
- **The slice order** below. Each slice keeps main deployable.

## Out of scope

- Workspaces.
- The roles that need workspaces or billing: limited developer,
  claude_code_user, billing, owner.
- SAML, SCIM, App Attest and Admin API keys.
- Multi-tenant activation (#56).

## Slices

A suggested order. Each slice is one PR in this repository. When the console
changes, a companion PR goes to managed-agent-console.

1. **Stored roles:**
   - roles on principals;
   - service accounts;
   - the setup-code bootstrap;
   - the identity lane reads stored roles;
   - viewer becomes user;
   - vault credentials open to developer.
2. **Platform login and the cookie:**
   - password, OIDC and IAP login on the control plane;
   - the session cookie on private paths;
   - the private API for members and service accounts;
   - the proxy path;
   - the identity Bearer on `/v1` removed;
   - in the console: one origin, no `PLATFORM_API_KEY`, login pages only.
3. **Principal-owned keys:**
   - keys created from now on are bound to owners and act with their roles;
   - `x-api-key` and Bearer on `/v1`;
   - keys from before this plan keep today's lane until slice 6.
4. **The initiator and the delegated token:**
   - sessions and deployments record their initiator;
   - route classes;
   - the per-claim inference token;
   - `BRAIN_API_KEY` removed.
5. **The gateway:**
   - grants and key policies removed;
   - rate limits per model for the organization;
   - the admin API on roles;
   - the bootstrap key and the operator token removed.
6. **No preset key:**
   - `CONTROLPLANE_API_KEY` removed;
   - every key from before this plan stops working;
   - compose, Helm, CI, the acceptance suite and the evals move to the setup
     code and login;
   - GCP staging moves to OIDC, with Google accounts (`accounts.google.com`)
     as the IdP. That needs an OAuth client in the GCP project. It is a
     cloud change, and the owner makes it.

## Docs that move with the change

- **CLAUDE.md and AGENTS.md:** principle 5, and the wire rule "Auth:
  management via `x-api-key`".
- **docs/ARCHITECTURE.md:** the auth lanes and the gateway's auth.
- **docs/self-hosted-security.md:** §9 (SSO) and §10 (the management key's
  lifecycle), rewritten. It also says who controls sign-in (decision 4) and
  how to recover by editing the database (decision 1).
- **docs/DIVERGENCES.md:**
  - New entries:
    - local password accounts;
    - user's read access;
    - roles and limits at organization level, with no workspaces.
  - Converged:
    - the Bearer API key;
    - the console's cookie-authenticated private API;
    - model use governed by role and rate limits.
  - "Human authentication (`/v1` management paths and the `/api/` console
    namespace)" is rewritten. Its sentence "its console authenticates humans
    with the tokens that endpoint mints" has no source and goes.
- **README.md and the compose and Helm docs:** installing without a preset
  key.
- **Plan 31 (archived):** this plan supersedes it on two points. Plan 31
  kept roles in the IdP, and it gave the management key full authority.
  docs/HISTORY.md records the reversal.

## Verification

- Each slice writes its contract tests first. They cover the role matrix,
  the route classes and every credential lane.
- A completeness test fails on any route that lacks a role or a class.
- End to end, the real `ant` CLI runs against a fresh compose install. It
  uses a key created in the console, sent once as `x-api-key` and once as a
  Bearer.
- A compose install with no preset key in its configuration starts and
  passes its smoke test.
