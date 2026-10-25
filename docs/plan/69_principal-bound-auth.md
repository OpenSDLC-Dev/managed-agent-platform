---
status: approved
---

# Principal-bound authentication: platform login, stored roles, no preset keys (plan 69)

The owner named two problems on 2026-10-24:

1. **Preset secrets.**
   - Compose requires two of them, `CONTROLPLANE_API_KEY` and
     `BRAIN_API_KEY`. Its example file ships the well-known values
     `local-dev-key` and `local-brain-key`.
   - Helm requires the first one always, and the second one while the model
     gateway is on, which is its default.
   - Each one is full management authority, and each one stays in the
     deployment's configuration for as long as the install runs. A leak of
     either one is a leak of everything.
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
2026-10-25 a builder check and code reviews read the draft. The
owner settled the points they raised that change a security boundary, and
set the roles to match the reference. The author settled the rest; those say
so ("author's call"). "Left to the builder" lists what neither settled.

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
  - The Admin API accepts a personal key or a service-account key that is not
    scoped to a workspace.
- **Console login** is Google, an email link, or SSO (SAML or OIDC). There is
  no password. A member who first arrives through SSO gets the User role,
  and an IdP group can map to a role (support 13132885).
- **Roles are stored in the Console's member list** (docs
  `manage-claude/admin-api`, `manage-claude/workspaces`; support 10186004):
  - An admin assigns them in the Console. The Admin API assigns only
    billing, claude_code_user, developer, managed and user (the SDK's
    `BetaOrganizationUserUpdateParamsRole`).
  - user: the playground only. Cannot see API keys, usage or billing.
  - developer: manages API keys, vaults and credentials, and webhook
    endpoints. Views usage and session traces. Downloads files.
  - admin: everything above, plus members and their roles.
  - The SDK's `BetaOrganizationUser` lists six more organization roles:
    billing, claude_code_user, managed, membership_admin, owner and
    primary_owner. The support centre adds limited developer. Workspace roles
    refine them per workspace.
  - A service account's organization role is developer or admin (docs
    `manage-claude/wif-reference`; the SDK's `BetaServiceAccount`).
- **No setting limits which models a role, a workspace or a key may use.**
  Two limits bound use instead. Neither may exceed the organization's limit
  (docs `manage-claude/workspaces`, `api/rate-limits`):
  - rate limits per workspace and per model: requests, input tokens and
    output tokens per minute;
  - a monthly spend limit per workspace.
- **The Console's own calls use a cookie, on the Console's host:**
  - Its resource calls go to `https://platform.claude.com/v1/...`, a
    same-origin proxy, with the cookie. The API's own host never sees the
    cookie (recordings `2026-09-05` and `2026-09-12-console-141`).
  - It mints an environment key at
    `POST /api/oauth/organizations/{org}/environments/{env}/tokens`, with the
    cookie (recording `2026-09-05`).
  - The playground sends `POST /api/organizations/<uuid>/proxy/v1/messages`
    with a Cookie and no `Authorization` or `x-api-key` (recording
    `2026-10-22-playground-protocol`).
  - All 28 captured proxy calls (27 Runs and one import helper) carry the
    header names `x-organization-uuid` and `x-workspace-id`. The 26 whose
    full request headers were captured carry the cookie. The recording does
    not show whether a CSRF mechanism exists.
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
    (anthropic-cli `pkg/cmd/cmd_auth.go:35`). Its `/v1/organizations`
    routes need `org:admin` as well (`cmd_auth.go:36-38`).
  - No source says what each `user:*` scope admits. Recording request
    managed-agents-wire-recordings#66 asks.

## The platform as-is

- **Lanes.** `dispatchAuth` (`internal/api/server.go`) picks one lane per
  request:
  - **The management key** (`x-api-key`): full authority, no role.
    `apiKeyOffered` reads only `x-api-key`, so a key sent as a Bearer is
    refused. On the dual-auth routes, `dualAuth` sends a Bearer that comes
    without an `x-api-key` to the environment-key lane. The exception is OIDC
    mode, where a JWT-shaped Bearer goes to the human lane.
  - **The identity lane** (`IDENTITY_MODE=oidc` or `trusted_proxy`):
    - The roles are viewer, developer and admin. They come from the claim
      `IDENTITY_CLAIM_ROLES` names, through `IDENTITY_ROLE_MAP`, on every
      request. A person whose claim maps to nothing gets 403 everywhere.
    - The platform stores no role (plan 31). `principals` (migration 0022)
      holds identity bookkeeping only, which the lane writes on every
      request.
  - **Machine credentials**, for one environment, one work item or one gate:
    `sk-map-env01-` keys and `wtk_` on `/v1`, and `gtk_` on
    `/internal/v1/gate/config`.
- **Preset keys:**
  - The control plane does not start without `CONTROLPLANE_API_KEY`. It
    seeds that key into `api_keys`.
  - It seeds `BRAIN_API_KEY` too, when set, as the row named `brain`, and
    archives that row at boot when it is unset. Both keys have full
    management authority.
  - The model gateway takes `CONTROLPLANE_API_KEY` as its admin bootstrap
    key. It lets the bootstrap key and the brain key reach every model with
    no grant.
  - Automated callers depend on them:
    - CD's post-deploy check (`.github/workflows/deploy.yml`) and
      `deploy/gcp/mode2-secret.sh` read `controlplane-api-key` back;
    - CI's gateway checks (`.github/workflows/ci.yml`) call
      `/admin/v1/profiles` with the bootstrap key and the brain key;
    - deploy/gcp/README.md configures staging's gateway catalogue by `curl`
      with the bootstrap key.
- **The gateway** (plan 59):
  - Its inference endpoints already take a key as `x-api-key` or as a Bearer
    (`internal/modelgateway/auth.go`).
  - It refuses any other key that has no key policy ("this API key has no
    model grant").
  - A key policy (`key_policies`) names the aliases the key may use. It can
    also set RPM and TPM limits.
  - Its usage ledger, daily rollups and rate windows are keyed by
    `api_key_id` (migration 0050).
  - The admin API (`/admin/v1`) takes the bootstrap key or an operator token.
    An operator token is an IdP token: viewer and developer can read, admin
    can write.
- **The console** (managed-agent-console) is a Next.js server that proxies
  the browser's calls through its own `/api/` routes:
  - In password mode or open mode it sends `PLATFORM_API_KEY`, which is
    `CONTROLPLANE_API_KEY`.
  - In OIDC mode it forwards the user's ID token as a Bearer.
  - It refuses `trusted_proxy` at boot.
  - It neither shows nor revokes a `cloud` environment's keys. Revoking one
    is API-only (docs/self-hosted-security.md §6).
- **The brain's model-turn claim** is one statement (`queue.Claim`), with no
  transaction around it. `wtk_` is minted elsewhere: in the worker poll's
  transaction (`claimWork` in `internal/api/workapi.go`), and only for a
  session with a memory store.
- **Ownership is audit only.**
  - `api_keys` records who issued a key, not who owns it.
  - `sessions.created_by` and `deployments.created_by` record the acting
    credential. They authorize nothing (design principle 5).
  - Two kinds of session have no `created_by`: one that a deployment's
    schedule fires (migration 0031), and one that a dream's pipeline starts
    (`internal/api/dreamstart.go`). A manual deployment run's session
    records the caller who fired it.
- **The default installs have no login and no console.**
  - Compose, Helm and GCP staging all leave `IDENTITY_MODE` empty.
  - Neither compose nor Helm deploys the console. It ships as its own image,
    `ghcr.io/opensdlc-dev/managed-agent-console`.
  - The bundled Casdoor registers the console's client with a secret. In
    compose, the callback is `http://localhost:5173/callback` and the
    secret a seeded development value (`deploy/compose/casdoor-init-data.json`).
    In Helm, both come from `casdoor.console` values, and the secret is
    required. The console sends the secret when it is configured, and uses
    PKCE either way.
  - Staging is plain HTTP on a bare LoadBalancer IP. Its values file
    prepares `trusted_proxy` with `roles: email` and a role map for when IAP
    fronts it.

## Decisions

### 1. No preset credential, and how the first admin appears

- These credentials are removed:
  - `CONTROLPLANE_API_KEY`, which the gateway also takes as its admin
    bootstrap key;
  - `BRAIN_API_KEY`;
  - the gateway's operator token.
- **There is no transition release.** No release is cut between slice 1 and
  slice 7 (owner, 2026-10-25). Slice 1 adds a precondition to
  docs/RELEASING.md: no release while STATE.md says a plan holds releases.
  STATE.md says so from slice 1 to slice 7. Under SemVer 0.x the removal is
  a minor bump.
- **The rule does not make a slice safe.** CD deploys every merge to main to
  GCP staging, so each slice must be safe to run there as it lands (author's
  call). A slice may let a credential do what it can do today, or what a
  credential of its kind can do at the end state, and nothing else. Today an
  unowned key can do anything on the control plane, and the bootstrap key
  anything on the gateway's admin API.
- **An unowned key** is a management key with no owner: a seeded key, any
  key issued before slice 3 gives keys owners, or a key an unowned key
  issues after that. A session, deployment or dream whose creator's
  principal row is gone (deleted before slice 1, under today's retention)
  counts as started
  by one.
- **What an unowned key does until slice 7** is stated here, once; other
  decisions refer to this list:
  - It keeps today's authority on `/v1` and on the private API's existing
    routes, through `x-api-key` only, as today.
  - It creates only unowned keys and names no principal. It reaches none of
    the authenticated routes this plan adds, which take only the cookie
    (decision 3). So
    it can make nothing that outlives slice 7 beyond what it can make today:
    environment keys and resources.
  - On the gateway it keeps today's grant check and key policy. The
    bootstrap key and the brain key keep their passes.
  - The work it started keeps running, even if the key is later archived or
    expires: as an initiator it passes the claim's check whatever its state
    (decision 8).
- **From slice 3, a preset value that matches an owned key stops the
  control plane and the gateway at start-up.** Today the control plane's
  seed strips such a row of its issuer and expiry, and the gateway grants
  the value its pass by digest alone. Either would make an owned key act as
  an unowned one.
- **Every unowned key stops working at slice 7**, whoever issued it.
  Environment keys (`sk-map-env01-`) are not affected. People create new
  keys in the console. The upgrade note says so. At slice 7, a session
  started by an unowned key stops at its next model turn, and a deployment
  created by one fails at its next run. An admin can point such a
  deployment at a service account.
- **The control plane needs a login mode from slice 7** (author's call). It
  refuses to start with `IDENTITY_MODE` empty or `disabled`, because without a login and
  a preset key nobody could manage it. Before slice 7, an install with no
  login mode prints no setup code, because nobody could redeem it.
- **The first admin is created with a one-time setup code:**
  - The control plane prints a code to its log at start-up, but only while
    all of these hold (author's calls):
    - A login mode is configured.
    - The install has never had an admin. A stored flag records the first
      admin, so a later "no admin" state never reopens the door.
    - No IdP mapping can give the admin role. Where one can, the IdP
      supplies the admins. So an upgraded install whose mapping names admins
      prints no code.
  - Each replica prints its own code. Any unexpired code works, and
    redeeming one spends them all (author's call). Each code is 20 random
    bytes, so several live codes do not make a guess practical.
  - In OIDC and IAP modes, the first person who signs in and presents a code
    becomes admin. A person whom a mapping matches cannot redeem a code,
    because the mapping owns their role. So an install whose mapping matches
    everyone gets its first admin by mapping one in the IdP.
  - In password mode, presenting a code creates a new local account, with
    the password given at that moment, as admin (author's call). That
    redemption is unauthenticated, so it is throttled like a login.
  - Password mode is for development only, so there the rule is looser: a
    code is printed whenever no local account holds admin (author's call).
    An install that moves to password mode from another mode then gets its
    first local admin the same way.
  - Whoever can read the logs before the first admin exists can claim the
    role. The security doc says so, and says to redeem the code at once.
- **There is no built-in recovery.** If no admin can sign in (the IdP is
  down, the last admin left, a password is lost), the operator edits the
  database by hand. docs/self-hosted-security.md will say how (slice 1).
- **The last admin is protected** (author's call):
  - No admin can demote or remove the last active admin, themselves
    included.
  - Only people count. An admin service account does not, because member
    management never takes an owned key (decision 3), so a service account
    cannot add another admin.
  - An IdP mapping can still demote the last admin (decision 4). Recovery is
    then a database edit.

### 2. The platform owns login

The control plane runs every login and sets the session cookie. The console
renders pages, and from slice 7 holds no secret; until then an install
without a login keeps `PLATFORM_API_KEY` there. A deployment uses one of
three login modes:

- **Password.** Local accounts, one for each person:
  - An admin creates each account, and its first password, in the console.
    An account a setup code creates is the exception (decision 1).
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

"Sign-in" in decision 4 means a login, or any verified request that carries
an IdP credential: an IdP Bearer, the proxy's assertion, or the gateway's
operator token. Each applies the mapping. From slice 7 only a login does; in
IAP mode the login reads the proxy's assertion.

### 3. One origin, and the cookie in two places only

- One origin serves the console, the control plane's private API and the
  gateway's admin API (author's call: the same entry point routes the
  gateway's admin paths to it).
- **Compose and Helm deploy the console**, from slice 2, from its published
  image (author's call). Keys, members and environment keys are managed
  only there, so every install needs one.
- The session cookie is accepted on the private `/api/...` paths and on the
  gateway's admin API, and nowhere else. `/v1` never accepts it.
- **The console's resource views use a private proxy path,**
  `/api/organizations/{org}/proxy/v1/...`. In this plan it reaches the
  control plane's `/v1` only, under the same role rules as `/v1`, so a user
  reaches nothing through it. A playground (#995) would take the same path
  to the gateway, as the reference's playground does. That issue decides how
  the person's authority reaches the gateway.
- **That is a divergence** (author's call). The reference's Console calls
  `/v1/...` with the cookie, but on the Console's own host, so the API host
  never sees the cookie. Here the console and the API share one origin, so a
  cookie on `/v1` would reach the SDK's surface. docs/DIVERGENCES.md records
  it.
- **Those two surfaces take the cookie only** (owner, 2026-10-25). This
  holds from slice 7. Until then each surface keeps the credentials it takes
  today, so that an install without a login keeps working:
  - The private API takes unowned keys (decision 1), the IdP Bearer and the
    proxy's assertion. An owned key (slice 3 on) is refused there, although
    today's handling would admit any active key row.
  - The gateway's admin API takes the bootstrap key and the operator token.
    It refuses every other key, owned or not, as today.
  - Every authenticated route this plan adds takes only the cookie, from
    the slice that adds it: members and service accounts, naming a principal
    when a key is created, a deployment's initiator, the proxy path, and the
    gateway's rate limits. The login routes and the setup code's redemption
    (decision 1) are how a person gets the cookie, so they take none. An
    install without a login therefore sets no rate limit before slice 7.

  From slice 7:
  - A leaked key cannot create keys, members or environment keys, and cannot
    repoint the gateway's providers or vendor credentials.
  - Issuing or revoking an environment key needs a person signed in to the
    console, including during an incident. The reference also mints them
    from its console. So the console must issue and revoke every
    environment's keys, `cloud` ones included (slice 6). The scripted
    issuance in docs/self-hosted-security.md §6 goes.
  - **This diverges from the reference's Admin API**, which takes an admin's
    key. This plan builds no Admin API: members, service accounts and keys
    are managed only in the console. docs/DIVERGENCES.md records it.
  - Automated fleets get no issuance route from this plan. Workload identity
    federation (#992) needs one, because its tokens are not cookies; that
    route is designed then.
- **`/v1` takes no human session.** From slice 3, the identity lane leaves
  `/v1`: both the IdP Bearer and the proxy's assertion header. A person's
  credential on `/v1` is then an API key. The machine credentials stay on
  their own routes: environment keys and `wtk_` on `/v1`, `gtk_` on
  `/internal/v1/gate/config`, and the delegated inference token (decision 8)
  on the gateway's `/v1`.
- **How a Bearer on `/v1` is read**, from slice 3 (author's call). Its
  prefix decides the lane, so no request pays a second lookup:
  - The work routes still take only an environment key, or the `wtk_`
    token where it is admitted today (its own item's heartbeat and stop).
  - An environment-key prefix (`sk-map-env01-`) goes where it goes today,
    including #840's answer to a revoked environment key on a management
    route.
  - The issued-key prefix (`sk-map-api01-`) takes the API-key lane, which
    admits only an owned key. An unowned, archived or expired key is refused
    as an unknown `x-api-key` is.
  - Any other Bearer goes where it goes today, except to the identity lane,
    which has left `/v1`. That covers a `wtk_` token, a seeded key's value
    and a grandfathered environment key.
  - The private `/api/` paths keep today's handling until slice 7, except
    that an owned key is refused there.

  So of the management keys, only an owned key works as a Bearer on the
  control plane's `/v1`. Environment keys and `wtk_` tokens keep today's
  Bearer handling. The gateway's changes are decisions 8 and 10.

### 4. Principals and stored roles

- A principal is a **user** or a **service account**. An admin creates
  service accounts in the console.
- **A principal has one organization role: user, developer or admin, as in
  the reference** (owner, 2026-10-25):
  - The role is stored in the platform. An admin assigns it in the console.
  - **user can only use the playground.** This plan builds no playground, so
    for now a user can sign in and do nothing else.
  - **There is no read-only role.** Reading resources needs developer, as in
    the reference, where viewing usage and session traces is developer's.
  - **A first sign-in that matches no mapping gets user**, as in the
    reference. A user reaches nothing in this plan, so an open IdP, such as
    Google accounts, admits no reader. If the playground (#995) lets a user
    call models, an outsider could spend, so that issue settles the first
    sign-in's role again.
  - A service account is developer or admin, as in the reference.
- **The IdP mapping is optional**, in OIDC and IAP modes alike, on the claim
  `IDENTITY_CLAIM_ROLES` names (staging's values map emails). When one is
  configured, it applies at every sign-in:
  - A person a mapping matches has the role the IdP gives. The console
    cannot change that role.
  - A person whose role came from the mapping, and whom it no longer
    matches, goes back to user. From then on the console manages their
    role.
  - A person no mapping has matched is managed in the console.
  - A setup code does not exempt anyone. If a mapping later matches the
    person who redeemed it, the mapping sets their role.
  - On upgrade, an existing `IDENTITY_ROLE_MAP` keeps working, so a mapped
    admin is still admin at their next sign-in. A map entry that says
    `viewer` now gives user, which reads nothing. The upgrade note says to
    map those people to developer if they must read.
- **The gateway's operator token follows the same rules**, from slice 1
  (author's call). Until slice 7 removes it, it is the gateway admin API's
  identity lane. So, as the control plane's identity lane does, it applies
  the mapping, creates the principal on first use, reads the stored role,
  and refuses a removed member.
- **The IdP reaches the platform only at sign-in.** A demotion or a removal
  there takes effect at the person's next sign-in. A person who uses only
  API keys never signs in, so their keys keep their stored role. An admin
  removes such a person in the console. The security doc says so.
  Automatic sync needs SCIM, which is out of scope (owner, 2026-10-25).
- **Removing a member, or archiving a service account** (author's calls;
  archiving the keys and restoring none follow the reference):
  - It is a lasting deactivation. A removed person who signs in again is
    refused, not re-created.
  - It wins over the mapping: a removed person whom a mapping still matches
    stays removed.
  - It archives every key the principal owns, from slice 3, when keys get
    owners.
  - Restoring the principal restores no key.
  - It revokes no environment key the person issued. An environment key
    belongs to its environment, and an admin revokes it in the console. The
    security doc says so.
- **Principals are never deleted, from slice 1** (author's call). The
  DELETE that docs/self-hosted-security.md §8 recommends becomes a scrub:
  - It clears a principal's personal fields, such as the email and name.
  - It keeps the row: its id, role and deactivation, and what sign-in needs
    to recognise the person (a digest of the IdP subject will do).

  So a removed person stays removed after an erasure, because the row and
  its deactivation survive, and the work anyone started still resolves to
  its initiator. Slice 1 makes
  the database refuse every DELETE on principals. A retention job written
  to today's §8 then fails loudly instead of undoing a removal, and the
  upgrade note says to switch it to the scrub.
- Roles are organization-wide. There is no workspace layer.
- The existing viewer role goes. Plan 31's per-request roles end here: a role
  is now stored, whether the mapping or an admin set it.
- **No playground is built by this plan.** It is later work (#995).

| Role | May |
|---|---|
| user | Sign in. Nothing else in this plan: reads no resource, creates no key, starts no session itself and calls no model directly. What the person set up before a demotion keeps running: their sessions and dreams, and their deployments' scheduled runs, which start new sessions in their name (decision 8). The reference's user uses the playground; #995 decides what this platform's user may do there. |
| developer | Read every resource: agents, environments, sessions and their events, deployments, files, skills, memory stores, vaults and credential metadata, dreams. Create, change, archive and delete them, including vault credentials. Create and manage their own API keys. Call every non-administrative endpoint, with every model. Read the gateway's configuration and usage. |
| admin | Everything developer may. Manage members, their roles and service accounts. Manage every API key. Issue and revoke environment keys. Change the model gateway's configuration and rate limits. Redact memory versions. |

Notes on the table:

- **A person who only needs to read gets developer**, which can also write.
  The reference has the same gap.
- **developer's vault credentials follow the reference.** Limited developer
  and above manage vaults and credentials there (support 10186004). Here
  developer manages vaults today, but only admin writes their credentials.
- **developer reads the gateway's usage** (author's call), as the
  reference's developer views usage.
- **Environment keys stay admin-only.** No recording shows which reference
  role may issue them (managed-agents-wire-recordings#40).

### 5. API keys belong to principals

- A key is created only through the console's private API:
  - a developer or an admin creates keys for themselves;
  - an admin creates keys for a service account.

  A key belongs to the caller's principal, whichever credential the call
  used, or to the service account an admin names. Naming a principal takes
  the cookie (decision 3). A call made with an unowned key creates only an
  unowned key (decision 1).
- A key carries no scope. It acts with its owner's role at the time of each
  request:
  - When the owner is removed, the key is archived (decision 4).
  - When the owner's role changes, the key's reach changes with it.
- From slice 3, `/v1` accepts a key as `x-api-key` or as
  `Authorization: Bearer`, as the reference does. The gateway's inference
  endpoints already do. An unowned key stays on `x-api-key` on `/v1`, as
  today (decision 1).

### 6. Scopes, only where a credential needs one

- In this plan only the delegated inference token (decision 8) carries a
  scope: `workspace:inference`. Only the gateway's inference endpoints check
  it (author's call; CLAUDE.md: do not build ahead of the slice).
- **The gateway's inference endpoints are every route that calls a model or
  lists them** (owner, 2026-10-25). Each is served at the root and under its
  own protocol's prefix (`/anthropic` or `/openai`); `/v1/models` is served
  under both:
  - `/v1/messages` and `/v1/messages/count_tokens`;
  - `/v1/models`;
  - `/v1/chat/completions`;
  - `/v1/responses` and its stored-response routes;
  - `/v1/embeddings` and `/v1/rerank`.

  The reference's `workspace:inference` names only the first three groups,
  because it serves no others. The class it describes, a workload that only
  calls models and manages no resource, covers the rest. No scope of our own
  is added, because the reference offers none finer. docs/DIVERGENCES.md
  records the extension.
- The delegated token reaches only the endpoints its scope names. A request
  outside them gets 403 `permission_error`. Its principal is checked by
  membership (decision 8). A scoped credential that a principal holds
  directly, as workload identity federation's tokens will be, also has its
  scope intersected with the principal's role, as in the reference (#992).
- Classes for the control plane's routes (`workspace:developer`,
  `org:admin`) come with workload identity federation, the first credential
  that needs them. #550 tracks that the reference splits admission by route
  and this platform does not.
- The `user:*` scopes wait for recording #66.

### 7. Principle 5, amended

- A session is still not bound to an application's end user. That stays an
  application-layer concern.
- A session now records what started it: the signed-in person, the API
  key's owner, or, until slice 7, an unowned key.
- The session's model calls act with that principal's authority, through the
  delegated token (decision 8). That is the only internal call that reaches
  past the session itself.
- The other internal credentials keep their narrow scope: `wtk_` covers one
  work item, `gtk_` one gate, an environment key one environment's queue.
  They carry no initiator. A removed initiator's session can still finish
  the tool calls its last model turn queued, and then stops at its next
  model turn.
- **No route repoints a session's initiator** (owner, 2026-10-25). Its
  history stays readable. Work that must outlive a person's membership
  starts under a service account; the security doc says so. A deployment
  can be repointed (decision 9), because it fires again and again.
- CLAUDE.md changes in the slice that does this.

### 8. A delegated inference token replaces the brain key

- **Minting.** The brain's claim of a model turn mints a token for that turn.
  The claim is one statement today, so this changes the claim path: the
  mint goes inside the brain's own claim. It reuses the platform's one mint
  for internal bearers: `internal/gatetoken`'s, which `internal/worktoken`
  already reuses under another prefix.
- **The token** names the session's initiator as its subject and the brain
  as its actor. Its scope is `workspace:inference`. It is valid only while
  the claim holds.
- **Only the gateway's inference endpoints accept it.** It is stored apart
  from API keys, so the control plane's Bearer lookup (decision 3) can never
  match it (author's call).
- **Who may call models.**
  - Directly, with an API key: developer and admin, and until slice 7 an
    unowned key under today's grant rules (decision 10).
  - Through a session: the session's initiator, while they stay a member.
  - The playground (#995) decides whether a user may call models through the
    console.
- **The claim checks the initiator before it mints.** It checks
  membership, not role (owner, 2026-10-25):
  - A removed person or an archived service account stops the session at
    its next model turn. A role change stops neither a session, a dream,
    nor a deployment's scheduled runs, which start new sessions under the
    initiator's name: the work was set up while its initiator held
    developer.
  - Until slice 7, an unowned key passes whatever its state (decision 1),
    so slice 5 stops nothing that runs today.
  - A refused turn fails on the brain's existing path. The claim leases the
    item as usual and mints nothing, so it takes no new lock (#643). The
    brain refuses where a failed model call is settled today, after replay,
    so the settlement covers the input that woke the turn. It settles the
    `session.error` a refused model call draws today
    (`model_request_failed_error`), with a message of ours.
  - The item then leaves the queue as a failed model call's does. Input
    that arrives while the refusal settles requeues it once, however many
    events it holds, and draws one more refusal.
  - The check also covers a route that skips the gateway: Helm with
    `modelgateway.enabled=false`, or compose with a direct providers file.
- **The gateway checks the same on each call**, in the lookup that
  authenticates the token: one joined query, not extra round trips (author's
  call).
- **Every session has an initiator from slice 5 on.** The backfill takes it
  from `created_by` (author's calls):
  - a principal: that principal;
  - a key that has an owner (slice 3 on): the owner;
  - an unowned key, or a principal whose row is gone: that id, which
    decision 1 treats as an unowned key;
  - none, for a session that a deployment's schedule fired: the deployment's
    initiator, which is backfilled the same way from its own `created_by`;
  - none, for a dream's session: the dream's initiator, which is backfilled
    the same way from the dream's own `created_by`.

  The claim applies the same rules to a session that still has no
  initiator, such as one an old replica creates during a rolling deploy.
  Only a session that none of them resolves fails its turn, as a refused
  initiator's does. At slice 7 an unowned key stops counting as an initiator
  (decision 1).

### 9. Deployments name their initiator

- A deployment's initiator is its creator by default. Its scheduled runs
  start their sessions with that initiator's authority.
- A manual run starts its session with the authority of the person or key
  that fired it, as `created_by` records today (author's call).
- An admin may name a service account as a deployment's initiator instead,
  so that it outlives a change of staff (author's call). Only an admin sees
  service accounts, and the setting takes the cookie (decision 3).
- The initiator is set through the console's private API, not through a
  `/v1` field, so the wire shape does not change (author's call).
- If the initiator is no longer a member when a scheduled run fires, the
  run fails with `unknown_error` (author's call), which auto-pauses the
  deployment. An admin then points it at a service account, and it is
  unpaused as any paused deployment is. A demotion alone fails no run
  (decision 8).
- Dreams follow the same rule (author's call). A dream's session is checked
  as any session is (decision 8), and the dream then fails as it does today
  when its session's model call fails, with `internal_error`.
- No recording shows the reference's rule or its answers.
  docs/DIVERGENCES.md records both.

### 10. The model gateway

- Every role that may call models may call every model.
- **From slice 4, an owned key needs no model grant.** The gateway admits it
  while its owner may call models, checked in the lookup that authenticates
  it. A delegated token is checked in the same lookup, against its
  initiator's membership (decision 8).
- **An unowned key keeps today's rules until it goes** (decision 1). So no
  slice widens which models an existing key reaches.
- From slice 3, the admin API takes a new key policy only for an unowned
  key, so slice 4 drops no policy an admin wrote for an owned key.
- Grants and key policies are deleted at slice 7. No allowlist replaces
  them.
- An admin sets rate limits per model for the whole organization: requests,
  input tokens and output tokens per minute. They apply to every call from
  slice 4, and replace the key policies' RPM and TPM limits when those go.
- **No limit isolates one credential from another** after slice 7. A
  runaway or leaked key can use a model's whole organization limit. The
  reference bounds use per workspace, and this plan has no workspaces.
  docs/DIVERGENCES.md records it with the organization-level limits.
- **Usage is attributed to the principal a call acts for** (author's call):
  an API key's owner, or a delegated token's initiator. The key is recorded
  too when the call used one. An unowned key stands for itself.
  The ledger and its rollups stop being keyed by `api_key_id`. The rate
  windows keep a per-key count for unowned keys' policies until slice 7.
- A monthly spend limit comes later, because it needs a price for each
  model.
- The admin API takes the console cookie, from slice 2. From slice 7 it takes
  nothing else (decision 3). Developer and above read; admin writes.

### 11. Later work

The plan's shape keeps room for four pieces of work. Each GitHub issue holds
its own scope:

- workload identity federation (#992);
- `ant auth login` (#993);
- the monthly spend limit (#994);
- the console playground (#995).

## Left to the builder

Neither the owner nor the author settled these. Each choice must keep the
decisions above.

- **The session:** its lifetime, and how logout works. The cookie is
  HttpOnly; it is Secure, except on a loopback development install; and its
  server-side session can be revoked, so that a removal or a role change
  takes effect before it expires.
- **Protection against cross-site requests** that carry the cookie. A
  cross-site form must not be able to make one. A required custom header,
  such as the `x-organization-uuid` the reference's calls carry, is one way.
- **Password storage, and throttling** of login and of the setup code's
  unauthenticated redemption.
- **The private paths** for login, members, service accounts and rate
  limits:
  - Use a reference shape where one exists. The SDK's Admin API types
    (`BetaOrganizationUser`, `BetaServiceAccount`) are one.
  - Where none exists, register the choice in docs/DIVERGENCES.md.
- **How one origin is served** in compose and Helm: a reverse proxy, an
  ingress, or the console's rewrites. The console's own server routes under
  `/api/` must not collide with the control plane's private paths. Those
  routes are `health`, `login`, `auth/{login,callback,logout,session}`, and
  the proxies. The `console` and `oauth` proxies serve an install without a
  login until slice 7, while an install with a login sends those paths
  straight to the control plane. So the routing depends on the login mode
  until slice 7.
- **IAP topology.** `/v1` must be reachable with an API key alone, so an IAP
  deployment routes `/v1` past the proxy.
- **The setup code's** lifetime: hours, not days.
- **How tests and CI get a key.** They use the same setup code, login and
  private API that a person uses, not a second path.
- **The OIDC client library.** `internal/identity` already verifies ID
  tokens. golang.org/x/oauth2 is an indirect dependency; if it is adopted
  for the code exchange, it stays in one package, as go-jose does.
- **The delegated token's** storage, apart from API keys (decision 8), and
  how the brain's provider sends it in place of a route's static `api_key`.
  The token is bound to one lease, so a reclaimed item's old token stops
  working.
- **The claim's new work** keeps #643's lock order: the claim takes no
  session lock while it holds the work item. Decision 8's refusal needs
  none.
- **The usage ledger's** new columns, and what happens to its existing rows.
- **The slice order** below. Each slice keeps main deployable, and safe to
  run on staging (decision 1).
- **Rolling deploys.** CD rolls every binary at once, so expand before you
  contract: a sender switches to a new credential only after its receiver
  accepts it, and a receiver drops a credential only a slice after its last
  sender stopped.

## Out of scope

- Workspaces.
- The roles that need workspaces, billing or an Enterprise plan: limited
  developer, claude_code_user, billing, managed, membership_admin, owner and
  primary_owner.
- SAML, SCIM (and with it any automatic sync from the IdP), App Attest and
  the Admin API.
- Multi-tenant activation (#56). Its dormant plan, plan 42, still assumes
  plan 31's claim-derived roles. When it resumes, it starts from this plan.

## Slices

A suggested order. Each slice is one PR in this repository. When the console
changes, a companion PR goes to managed-agent-console. No release is cut
until slice 7 has landed (decision 1), but CD deploys each slice to staging,
so each one must be safe to run as it lands. Slices 1 to 5 add the new
paths, and make the narrower changes each one names (slice 1 drops viewer,
slice 3 takes the human lane off `/v1`). Slice 6 moves every install onto
the new paths, and slice 7 removes the old ones.

1. **Stored roles:**
   - user, developer and admin stored on principals; viewer goes, and reads
     need developer;
   - the principal kind for service accounts;
   - the setup code, redeemed on the identity lane until slice 2;
   - the identity lane reads stored roles, and the mapping applies, in both
     modes, at every verified request;
   - deactivation is honoured (no API sets it yet), the database refuses
     every DELETE on principals, and §8's retention becomes a scrub
     (decision 4);
   - the gateway's operator token reads the stored role too;
   - vault credentials open to developer;
   - docs/RELEASING.md's new precondition, and STATE.md's release hold.
2. **Platform login and the cookie:**
   - password (with the setup code's admin account), OIDC and IAP login on
     the control plane;
   - the bundled Casdoor, in compose and Helm, registers the control plane's
     callback, and the control plane holds that client's secret (the one
     configured secret, decision 2);
   - the session cookie, on private paths and the gateway's admin API,
     beside today's credentials;
   - the private API for members and service accounts, with the last-admin
     guard;
   - the proxy path, and one origin for an install that configures a login;
   - compose and Helm deploy the console;
   - in the console: login, members and setup-code pages, and the cookie
     wherever a login is configured. Without one, it keeps
     `PLATFORM_API_KEY` until slice 7.
3. **Principal-owned keys:**
   - keys created from now on belong to the caller's principal or to the
     service account an admin names, and act with its role;
   - removing a member or archiving a service account archives the keys it
     owns;
   - an owned key as `x-api-key` or Bearer on `/v1`, with a Bearer's lane
     read from its prefix;
   - an owned key is refused on the private paths, and the gateway's admin
     API takes new key policies for unowned keys only;
   - a preset value that matches an owned key stops start-up, and the seed's
     comment that records refusing to boot as rejected is rewritten;
   - the identity lane leaves `/v1`: the IdP Bearer and the proxy's
     assertion.
4. **The gateway:**
   - an owned key needs no grant, and is admitted while its owner may call
     models; unowned keys keep their grants and policies;
   - rate limits per model for the organization;
   - usage attributed to principals.
5. **The initiator and the delegated token:**
   - sessions, deployments and dreams record their initiator, with the
     backfill;
   - the gateway accepts the per-claim inference token; the claim mints it
     and checks the initiator's membership;
   - the deployment's service-account rule, and a scheduled run's
     auto-pause when its initiator is gone;
   - the brain keeps sending `BRAIN_API_KEY` (expand before contract).
6. **Every install signs in:**
   - the brain sends the delegated token instead of `BRAIN_API_KEY`;
   - compose defaults to password mode, as a development install;
   - Helm requires a login mode;
   - CI, the acceptance suite and the evals get their keys through the
     setup code and login, and CI's gateway checks use the cookie;
   - the console issues and revokes every environment's keys, `cloud` ones
     included;
   - GCP staging moves to OIDC, with Google accounts
     (`accounts.google.com`) as the IdP. That needs HTTPS first: Google
     refuses an `http` or bare-IP redirect URI, and the Secure cookie is not
     sent over `http`. The owner settles how staging gets HTTPS before this
     slice (2026-10-25). The OAuth client is a cloud change the owner makes;
   - after this slice deploys, an admin creates a service-account key in
     staging's console and stores it as a CD secret (author's call).
     deploy/gcp/README.md says how.
7. **No preset key:**
   - `CONTROLPLANE_API_KEY` (the gateway's bootstrap key), `BRAIN_API_KEY`
     (and the gateway's brain-key pass) and the operator token (the
     gateway's `IDENTITY_*` admin sign-in) removed, with every reference to
     them: compose and `.env.example`, Helm, CI's workflows (`ci.yml` and
     `deploy.yml`), `deploy/gcp`'s values, README, `mode2-secret.sh` and its
     test, and every doc that tells an operator to set them. The console's
     `PLATFORM_API_KEY` goes from compose and Helm (slice 2 added it) and,
     in the companion PR, from the console. A search of this repository
     for `CONTROLPLANE_API_KEY`, `BRAIN_API_KEY`, `PLATFORM_API_KEY`,
     `controlplane-api-key` and `brain-api-key` then finds no live use,
     only records that keep history: merged migrations, plans,
     docs/DIVERGENCES.md's entries, the changelog and its fragments, and
     the upgrade note;
   - CD's post-deploy check reads slice 6's service-account key from the CD
     secret;
   - the control plane refuses to start without a login mode;
   - the private paths and the gateway's admin API take the cookie only;
   - every unowned key stops working, and grants and key policies are
     deleted;
   - deploy/gcp/README.md configures staging's gateway in the console.

## Docs that move with the change

- **CLAUDE.md:** principle 4, whose gateway route for the brain sends the
  delegated token rather than a configured `api_key` (slice 6); principle 5;
  the wire rule "Auth: management via `x-api-key`"; and the compose
  command's list of services, which gains the console (slice 2).
- **docs/ARCHITECTURE.md:** the auth lanes and the gateway's auth.
- **docs/RELEASING.md:** the precondition that a plan can hold releases
  (decision 1, slice 1).
- **docs/self-hosted-security.md:**
  - §6: environment keys are issued and revoked in the console only,
    `cloud` ones included, and removing a person revokes none they issued;
  - §8: the principal retention DELETE becomes a scrub, and the database
    refuses every DELETE on principals (decision 4);
  - §9 (SSO): who may sign in, the first sign-in as user, the mapping, and
    that the IdP reaches the platform only at sign-in;
  - §10 (the management key's lifecycle), rewritten; it also says how to
    recover by editing the database.
- **docs/DIVERGENCES.md:**
  - New entries:
    - local password accounts;
    - roles and limits at organization level, with no workspaces, so no
      limit isolates one credential from another (decision 10);
    - the console's resource views on a private proxy path, with no cookie
      on `/v1` (decision 3);
    - no Admin API: members, service accounts and keys are console-only
      (decision 3);
    - `workspace:inference` also covers the gateway's Responses, embeddings
      and rerank routes, which the reference does not serve (decision 6);
    - the setup code, and the other private paths this plan adds where no
      reference shape exists;
    - a deployment's and a dream's initiator: the creator by default, a
      service account an admin names, set through the private API, and
      unchanged by a demotion (decisions 8 and 9);
    - a session whose initiator is gone fails its turn with today's
      `session.error` and a message of ours, a scheduled run fails with
      `unknown_error`, and a dream with `internal_error` (decisions 8 and
      9): no recording shows the reference's answer.
  - Converged, with no entry to retire, because none records today's
    mismatch: the console's cookie-authenticated private API.
  - Converged by rewriting entries in the list below: the Bearer API key
    ("Human authentication"), for owned keys from slice 3 and for every key
    at slice 7, since an unowned key stays on `x-api-key` until it goes and
    no release carries the gap; and model use governed by role and rate
    limits ("Model gateway admin API").
  - Rewritten or retired:
    - "Human authentication (`/v1` management paths and the `/api/`
      console namespace)". Its sentence "its console authenticates humans
      with the tokens that endpoint mints" has no source and goes. Its
      missing-key 401 for a Bearer on a management path no longer covers an
      issued-prefix Bearer, which draws the invalid-key 401 (decision 3).
    - "unknown_error is never recorded, and an unclassified fire failure
      records nothing at all", which decision 9's scheduled run makes
      untrue for a gone initiator.
    - "`/api/console/…` — `principal_id` on key create, a rename's name
      rule, and the workspace read (#820)", whose `principal_id` answer
      assumes no service accounts exist.
    - "Console roles — three (`admin`/`developer`/`viewer`), bound to IdP
      claims rather than a platform user store".
    - "Which role each management route requires".
    - "Model gateway admin API (`/admin/v1/`)".
    - "`can_manage` omitted from the listing while the surface stays
      admin-only".
    - "`sk-map-api01-` as the issued-key prefix, and `principal`/`api_key`
      actors where the reference says `user`".
    - "Environment worker keys — issuance".
    - "Which console role may issue an environment key — `admin` here,
      unobserved there".
    - "`IDENTITY_MODE=trusted_proxy` — a mode with no reference analog".
    - "`/api/oauth/organizations/{organization_id}/environments/{environment_id}/tokens[…]`
      — the console-API dialect".
    - "A revoked environment key on a management route — the reference's
      revoked sentence, not the missing-key 401 (#840)".
    - "A key seeded from `CONTROLPLANE_API_KEY` is listed but not
      console-mutable".
    - "Memory-version actors — `user_actor.user_id` is a `principal_` id,
      `api_actor.api_key_id` an `apikey_` id", once password accounts exist.
    - "Memory-version `created_by` / `redacted_by` —
      `service_account_actor` is never emitted", once service accounts
      act.
- **README.md, deploy/gcp/README.md, docs/deploy-gcp.md and the compose and
  Helm docs:** installing with the console and without a preset key, and
  staging's login and gateway setup.
- **Archived plans this one reverses**, each recorded in docs/HISTORY.md:
  - plan 31, which kept roles in the IdP and gave the management key full
    authority;
  - plan 32, which kept the key seeded from `CONTROLPLANE_API_KEY` beside
    the issued ones;
  - plan 59, whose key policies, bootstrap key and brain-key pass go.

## Verification

- Each slice writes its contract tests first. They cover the role matrix and
  every credential lane.
- The existing completeness test (`internal/api/rolematrix_test.go`) keeps
  failing on any identity-reachable route without a role.
- End to end, the real `ant` CLI runs against a fresh compose install. It
  uses a key created in the console, sent once as `x-api-key` and once as a
  Bearer.
- After slice 7, a compose install with no preset key in its configuration
  starts and passes its smoke test.
