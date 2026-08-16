# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); released sections
group entries newest-first by the PR that landed them.

A change and its changelog entry land in the **same PR** — the entry as a
fragment in [changelog.d/](./changelog.d/), its body the final entry verbatim.
A release PR assembles the fragments into a dated section here (`make
changelog`; see [docs/RELEASING.md](./docs/RELEASING.md)); post-release, the
section moves to [docs/changelog/](./docs/changelog/) behind the index stub
below (`make changelog-archive`, relative links re-based, byte-reversibly);
no other PR edits this file. The fragment is the **one place a change's
narrative is written**: [docs/HISTORY.md](./docs/HISTORY.md) holds only what
a changelog structurally cannot (acceptance-run and review-hardening records,
decisions evaluated and rejected, archived plans' progress summaries), never
a second copy of an entry here.

## [Unreleased]

Unreleased changes accumulate as one fragment file per PR in
[changelog.d/](./changelog.d/) and are assembled into a dated section here
by `make changelog` at release time (see [docs/RELEASING.md](./docs/RELEASING.md)).

## [0.3.0] - 2026-08-16

### Added

- **Two documented lists and one documented limit are now enforced by tests** (#413) — a doc that
  goes stale silently is the failure this work exists to stop, so three of them are pinned. The
  changelog assembler refuses a fragment over the 1,500-byte cap `changelog.d/README.md` states,
  measured in bytes rather than runes because the entries are full of multi-byte punctuation; the
  test reads that number back out of the README, so the cap cannot move in one place only, and the
  gate now runs the loader over the repository's own `changelog.d/` — an over-cap or malformed
  fragment fails in the pull request that writes it instead of months later in a release PR. An
  offline test fails if any doc enumerates a wire ID-prefix list that disagrees with
  `internal/domain/id.go`, and another fails if a `RUN_LIVE_*` consent variable the tree reads has
  no row in README's tier table. Both found real drift on their first run and this release carries
  the corrections: `outc_` was missing from both prefix lists, `skillver_` from CLAUDE.md's and
  `vcrd_` from ARCHITECTURE's, and the tier table omitted `RUN_LIVE_KMS_TESTS`.

- **The MCP path is covered by an eval trial and a live third-party server (#45)** — `mcp-answer` joins the eval suite as its sixteenth trial: a passphrase that exists only in what an MCP server's tool returns, so a graded answer exercises discovery, the default confirmation gate and the call itself. A new live tier, `RUN_LIVE_MCP_TESTS=1`, dials a server this repository did not write — `MCP_LIVE_SERVER_URL`, plus an optional `MCP_LIVE_SERVER_TOKEN` whose absence is an anonymous dial rather than a rotted credential. Its opt-in contract is every live tier's (README.md's tier table): the environment variable is the consent, never `.env`, and once opted in a missing URL fails rather than skips. With this the MCP toolset is complete — an agent's `mcp_toolset` calls a real MCP server's tools end to end.

- **A sandbox reaches the MCP servers its agent declares — plan 29 slice 6a (#45)** — `limited` networking's `allow_mcp_servers` now widens the per-session egress gate as well as the platform's own dial: the gate config carries the `host:port` endpoints the session's resolved agent declares MCP servers at, and a process inside the sandbox reaches them alongside `allowed_hosts`. They ride beside the policy rather than folded into it, so an operator's own list is still served as written, and they are port-scoped, because an agent author naming one endpoint should not thereby open the ports beside it. A declaration that would widen the gate past the server it names is not sent — a wildcard host (an `mcp_servers` url passes no URL grammar, so `https://*.example.com/` would become the suffix rule a host set reads it as), a scheme the platform would not dial, a literal address the platform's own MCP client refuses, an impossible port, or anything the operator list's own host grammar rejects (asked of `egress.ValidateHostEntry` rather than restated, which also excludes IPv6 literals the gate cannot match consistently across its two handlers). Names cannot be judged that way, so the gate dials an endpoint that only these declarations admit under the same address floor, on the resolved address. This closes the MCP half of the gate's fail-closed divergence; `allow_package_managers` is still not honored (#50).

- **An expired MCP OAuth token refreshes at the dial — plan 29 slice 5b (#45)** — An `mcp_oauth` credential whose `expires_at` has passed, or is within a minute of passing, performs the RFC 6749 refresh-token grant against its stored token endpoint before the MCP server is dialled, and the rotated tokens are sealed back onto the credential — under a compare-and-set, and best-effort, so the next dial in this session or another starts from them while a lost write costs no more than another exchange. The exchange's wire shape now lives in one place, `internal/oauthrefresh`, shared with the `mcp_oauth_validate` probe that already performed it. A credential that cannot refresh sends the token it has and lets the server answer, which is the reference's documented no-refresh-token behaviour. An issuer that refuses the grant is `mcp_authentication_failed_error`, and so is any other answer this platform cannot turn into a token — including one whose access token cannot be sent as a header, whose replacement refresh token is stored anyway so a later dial can still buy a usable one; one that is unreachable, 5xx-ing, or answering 408, 425 or 429 is neither, so the work item retries rather than answering a failure that says nothing about the credential. The token endpoint is dialled through the same address guard and no-redirect rule as every other credential-supplied URL, and an address that guard refuses is reported against the credential rather than retried forever.

- **MCP servers authenticate with the session's vault credentials (#45)** — A session whose attached vaults hold an `mcp_oauth` or `static_bearer` credential for a declared MCP server now dials it with that token as `Authorization: Bearer`, on discovery and on tool calls. Matching is by normalized `mcp_server_url`, the first vault with a match wins, and a server nothing matches is dialled with only what its URL itself carries — userinfo still sends net/http's Basic header, which a matched token replaces. Credentials re-resolve per dial with no cache, so a rotation or an archive reaches the next dial without a restart. A refused credential now reads as one: a 401 or 403, and a matched credential the platform cannot open — never dialled anonymously instead — are `mcp_authentication_failed_error`, not `mcp_connection_failed_error`. A failed lookup is neither, and retries.

- **Oversized MCP answers spill instead of vanishing (#45)** — An MCP tool answer too large for the model's context was truncated and the rest lost. Its text is now written whole into the session's sandbox and the result names the file — `/tmp/tool_outputs/<call id>.txt`, one per call, the convention built-in tools have used since #226. Images and blobs are named, not written. The trigger is whether rendering dropped or truncated a block, never the answer's size. Spilling never creates or heals a sandbox — it uses a new read-only `sandbox.Provider.Attach` — so a session with no running sandbox, and every `self_hosted` session (a deliberate divergence), truncates as before. Resource labels such as a link's URI are now capped at 2 KiB.

- **An agent's MCP tools now reach the model (#45)** — An `mcp_toolset` expanded to nothing at request assembly; it now expands to the tools its server reported, applying the entry's `default_config` and `configs[]`. Each enabled tool is offered to the model as `mcp__{server}__{tool}` — the session wire still carries the bare name and `mcp_server_name` — and a call commits as `agent.mcp_tool_use`. MCP calls gate like any other ask tool: `always_ask` is the toolset default, so the turn idles with `requires_action` until confirmed. A prefixed name past the Messages API's 64-byte limit, a name already taken, or a definition past the 256 KiB one request carries costs that tool a log line, not the turn. A session with undiscovered servers suspends its first turn to list them.

- **The `mcp_exec` driver now answers MCP tool calls (#45)** — Its second job beside discovery: an outstanding `agent.mcp_tool_use` is dialled, called, and answered with an `agent.mcp_tool_result`. A tool that ran and failed is a result with `is_error` for the model to self-correct from, not a work-item fault; a call that never reached its server gets a result plus a `session.error` typed `mcp_connection_failed_error` (`retry_status` `retrying`), its endpoint cut to `scheme://host` since an `mcp_servers` entry may carry a credential. Content the wire cannot carry is mapped, not dropped: an embedded resource becomes a document, a resource link and audio become text. An answer is held to the 100 KiB every tool result gets, one call to two minutes, and a whole pass to `EXECUTOR_MCP_PASS_TIMEOUT` (default 5m), which now covers discovery and execution alike. Nothing emits an MCP call yet.

- **An admin can issue, list, disable and retire management API keys from the console** ([#378](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/378)) — three routes under `/api/console/organizations/{org}/workspaces/{workspace}/api_keys` (`POST`, `GET`, and `POST …/{key_id}`) end "one credential, changeable only by restarting the control plane": issue a named key with an optional `expires_at` and receive the secret **once**, as `raw_key`; list every key with a masked `partial_key_hint` and never a secret; disable one reversibly and re-enable it; archive one for good. Omitting `expires_at` means never, and `expired` is derived from it at read time rather than settable. Nothing is hard-deleted, and `archived` is terminal here. On the SSO lane the whole surface — the listing included — is gated at **admin**; a management `x-api-key` also reaches these routes, and can mint further management keys. A key issued over SSO records the issuing human's `principal_`-prefixed id. The key seeded from `CONTROLPLANE_API_KEY` is **listed but not console-mutable**: rotate it by restarting with a new value. There is deliberately no `/v1` twin; that absence and the omitted fields are registered in [docs/DIVERGENCES.md](docs/DIVERGENCES.md). Operator rules: [docs/self-hosted-security.md](docs/self-hosted-security.md) §10.

- **An MCP tool call can stop and ask a human, and a refused one is answered in its own shape (#45)** — `agent.mcp_tool_use` joins the confirmable set: an ask-gated MCP call blocks the `requires_action` gate, appears in `session.status_idle`'s `event_ids`, and is released by the same `user.tool_confirmation` keyed by `tool_use_id` a built-in uses. `always_ask` is the MCP toolset's default, so most MCP calls arrive gated. A denial is answered in the family of the call it refuses — `agent.mcp_tool_result` keyed by `mcp_tool_use_id`, `is_error` true, carrying the `deny_message`. Client-executed custom tools stay ungated: the platform cannot stop what it does not run. A session parked on an MCP approval is excluded from idle-TTL sandbox reaping, and its wait counts in `approval.wait.duration`.

- **Console SSO and RBAC, proven end to end — plan 31 archived** ([plan 31](docs/plan/31_console-sso-rbac.md), [#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)) — the chain was driven over HTTP against the shipped `deploy/compose` stack with `--profile iam`, on genuine Casdoor tokens minted by authorization-code + PKCE: a viewer is refused a mutation with **403** and reads it at **200**, an admin issues an environment key over the console surface, a real `ant` long-polls with it, and forged or revoked keys are **401**. The property that matters: with the identity lane fully configured, a management `x-api-key` still answers **200** — humans were added, machines were not disturbed. The run, and the Casdoor quirk that its OAuth parameters ride the query string while the credentials ride the JSON body, are in [docs/HISTORY.md](docs/HISTORY.md). Its api-key issuance slice was deferred to [#378](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/378).

- **A session's MCP servers are discovered, and their tools are on record (#45)** — A new internal work kind, `mcp_exec`: for each MCP server a session's agent declares, the executor connects, lists the tools, and writes the listing into the new `mcp_catalogs` table — or records why it could not. It runs in the platform's process for cloud and self-hosted sessions alike, never on a BYOC worker. A failed server is recorded and re-dialled once per work cycle — not per turn — while a ready listing is not re-fetched. Every dial is checked against the session's networking policy first — `limited` admits a server through `allow_mcp_servers`, an unrecognized policy refuses — a failure reason keeps only `scheme://host`, since an endpoint may carry a credential, and one listing is capped at 256 KiB. Rows are per session and drop when `mcp_servers` is patched. Nothing offers these tools to a model yet.

- **The bundled identity provider — single sign-on with no enterprise IdP** ([plan 31](docs/plan/31_console-sso-rbac.md), #56) — `docker compose --profile iam up` now starts a pinned Casdoor (`casbin/casdoor:3.152.0`) behind a TLS-terminating proxy, and Helm renders the same bundle from `casdoor.enabled` (default `false`). Both are opt-in and inert when off: with `IDENTITY_MODE` unset, auth stays `x-api-key` only. The chart requires `casdoor.adminPassword` — it re-owns the `built-in/admin` account Casdoor otherwise creates on a published default — plus `console.clientSecret`, `console.redirectURIs`, `ingress.host` and `identity.roleMap`, and refuses to render on a mismatched audience or issuer. No upstream providers are configured and SAML and CAS are blocked; [docs/self-hosted-security.md](docs/self-hosted-security.md) §9 records the CVE posture (VU#780781). Configure it from [the compose README](./deploy/compose/README.md) or the [chart README](./deploy/helm/managed-agent-platform/README.md), which carry the traps: the role claim must be `groups`, since Casdoor's `roles` carries objects mapping to nothing, and `SSL_CERT_FILE` replaces Go's certificate list rather than adding to it. On GCP, Terraform wires Google's IAP instead — new `iap_backend_service` and `iap_members` variables and a derived `identity_proxy_audience` output — but the OAuth consent screen is not Terraform-managed and stays an operator step in [deploy/gcp/README.md](./deploy/gcp/README.md).

- **The role matrix — every route says what it needs** (plan 31 slice 3, #56) —
  slice 2 gated the control plane at a floor that denied every human; each
  identity-reachable route now declares its minimum role instead. Reads are
  `viewer`, the streaming ones included; resource CRUD and session lifecycle are
  `developer`; vault-credential create, update, delete, archive and
  `mcp_oauth_validate` are `admin`, alongside the whole environment-key surface,
  listing included. Vault-credential *reads* stay `viewer` — they return sealed
  metadata, never a secret. Two edges are deliberate and look like mistakes until you know
  why: the vault itself is `developer`, so deleting or archiving one purges the credentials
  inside it — `admin` bounds who may read, write or mint a secret, not who may destroy the
  container holding it — and the session-resource routes stay `developer` although they take
  a `github_repository` `authorization_token`. The work API and the gate-config route keep
  `RoleNone`, which no role satisfies, so a human gets 403 there; worker
  environment keys and the management `x-api-key` are unaffected. A denial names
  the route's requirement, not the caller's role, and no path or method moved.

- **The identity lane — humans reach the control plane, default-denied** ([plan 31](docs/plan/31_console-sso-rbac.md), [#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)) — with `IDENTITY_MODE` set, a human credential (a Bearer JWT in `oidc` mode, the configured signed assertion header in `trusted_proxy`) is verified into a principal, provisioned on sign-in into a new `principals` table keyed on `(issuer, subject)`; roles come from the provider per request and are never stored, a `sub` over 255 characters is refused rather than truncated, and `sessions.created_by` records the principal id. A management `x-api-key` still wins outright. On the dual-auth routes a Bearer with a JWT silhouette takes the human lane and anything else stays a worker environment key — platform-minted keys carry no dots, but a self-chosen pre-0021 key may, and 401s there. Every route declares a minimum role, all at the floor as this lands — denying every human — except the console environment-key routes (`admin`); the per-route split arrives with the role matrix, later in this same release. A token mapping to no role is 403, not 401. Retention — and why a returning person is provisioned afresh — is [docs/self-hosted-security.md](docs/self-hosted-security.md) §8. Unset, the default, nothing changes save one exception every deployment gets: a repeated `x-api-key` header is now a 401 rather than resolved by header order.

- **`internal/identity` — the human-authentication boundary** (#56). A strict OIDC
  relying party for any compliant provider, plus a trusted-proxy mode (`gcp-iap` preset
  or `custom`) for deployments where the cloud terminates authentication. `Verify`
  authenticates one compact JWT — the signature allowlist is
  RS256/RS512/ES256/ES384/ES512, so `alg:none` and HS256 never reach a key lookup —
  returning a principal whose role (`viewer` < `developer` < `admin`) comes from a
  configurable claim. How that claim name is read is fixed at configuration time so a token
  cannot choose it: a URI-shaped name (`https://corp.example/roles`, the Auth0 convention) is
  one flat key, dots included, while any other dotted name (`resource_access.console.roles`,
  the Keycloak shape) is a path. Signing keys are cached for five minutes, so a revoked key
  stops verifying within that bound. `IDENTITY_MODE` is `disabled` by default; `oidc` and
  `trusted_proxy` read the `IDENTITY_OIDC_*` / `IDENTITY_PROXY_*` / `IDENTITY_CLAIM_*`
  variables and a required `IDENTITY_ROLE_MAP`, and any misconfiguration fails startup
  rather than open. No route consumes it yet; the `/v1` wire, the `ant` CLI and machine
  credentials are untouched.

- **Plan 31 drafted — console SSO and RBAC** ([docs/plan/31_console-sso-rbac.md](docs/plan/31_console-sso-rbac.md), [#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)) — the design for the SSO and RBAC half of #56: today one `x-api-key` is the whole management authority, and the plan has humans authenticate through **any standards-compliant OIDC provider**, or a trusted identity-aware proxy (`gcp-iap` preset), with the control plane resolving each request to a principal holding one claim-mapped role — `admin`, `developer` or `viewer` — enforced per route. A hardened Casdoor ships as the self-host default, replaceable by config with Keycloak or any cloud IdP. Machine credentials and every documented CLI and SDK flow keep working unchanged; the one observable `/v1` change — management paths also accepting a Bearer JWT — is registered in [docs/DIVERGENCES.md](docs/DIVERGENCES.md). Multi-tenant activation stays in #56, sized but not built.

- **An operator surface for environment keys — issue, list and revoke over HTTP** ([#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)) — a self-hosted operator no longer seeds a BYOC worker's credential into Postgres: `POST`, `GET` and `POST …/{token_id}/revoke` under `/api/oauth/organizations/{org}/environments/{environment_id}/tokens` issue, list and retire keys. Issuance returns the plaintext exactly once, as an RFC 6749 token response (`access_token` plus `expires_in`), and is refused on a `cloud` or archived environment; the listing never shows a secret; revoke is idempotent, 204-bodiless, and reaches only the owning environment's keys. All three take the management `x-api-key`; an environment key is rejected. **`/v1` gains no route** — the namespace is off the wire, mirroring the reference console's own, with four deliberate departures registered in [docs/DIVERGENCES.md](docs/DIVERGENCES.md). Curl recipes: [docs/self-hosted-security.md](docs/self-hosted-security.md) §6.

- **Plan 30 drafted — console-issued environment keys** ([docs/plan/30_environment-keys-console-issuance.md](docs/plan/30_environment-keys-console-issuance.md), [#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)) — the design for the operator surface that issues BYOC worker credentials, which the platform has never had: a self-hosted operator has had to seed a key into Postgres by hand. The plan settles the model on the reference console's, observed live on 2026-08-10 down to its request and response bodies, and keeps issuance off the `/v1` wire on a namespace mirroring that console's own private API, giving future console-facing endpoints a convention to follow. Its four deliberate departures from that dialect are reasoned in the plan. Three slices follow: storage and primitives, the endpoints, then docs and a real `ant beta:worker` acceptance run.

- **An MCP client (#45)** — `internal/mcp` connects to a customer-named MCP server over Streamable HTTP, lists its tools and calls one, keeping the official `modelcontextprotocol/go-sdk` inside that package: tools arrive in Anthropic tool-definition shape (`input_schema`), and an entry with an unusable name, a non-object schema, or a duplicate name is skipped rather than failing the whole listing. Connections are per-work-item and bounded — 30s to dial, 2 minutes per listing or call, 100 pages, 8 MiB of response bytes cumulative per connection and 64 KiB per header block. The client refuses redirects, sends its bearer token only to the endpoint's own origin, guards every dial through `internal/dialguard`, and propagates W3C trace context to the server. It ignores `HTTP_PROXY`/`HTTPS_PROXY` deliberately — a proxy would move the dial off the target and leave the guard vetting the proxy — so egress that must go through one needs its own client. Divergences from the reference are recorded in docs/DIVERGENCES.md.

- **Continuous delivery to the GCP staging environment** (#347; the ref assertion below was
  corrected in #351). A new
  `.github/workflows/deploy.yml` builds, pushes, installs and smoke-tests on every push to
  `main` and on `workflow_dispatch`, using the new `deploy/gcp/staging-values.yaml`. The
  deployment is **mode 2** (Cloud SQL, Cloud Storage, Cloud KMS) behind the pre-created Secret
  `map-platform`, so the values file carries no credential: the job authenticates by Workload
  Identity Federation and assembles that Secret from Secret Manager, and there is no GitHub
  secret. A dispatch from any ref but `refs/heads/main` is refused before the auth step, and
  again cloud-side by the provider's `assertion.ref`; `id-token: write` is scoped to the one
  job rather than the workflow. CD never runs Terraform — `make gcp-bootstrap` and
  `make gcp-db-init` stay human-driven, which is also why rotation differs by secret:
  [deploy/gcp/README.md](./deploy/gcp/README.md) carries the per-secret procedure. The smoke
  check demands 200 with the management key and 401 without — over plain HTTP on a bare IP, so
  the key travels unencrypted, which is deliberate for staging only and costed in
  [docs/deploy-gcp.md](docs/deploy-gcp.md) under "Exposing the control plane". `model-providers`
  holds a placeholder key a human must replace.

### Changed

- **Plan 34 archived: the documentation is smaller than the system it documents** (#413) —
  Tracked markdown went 2,665,735 → 2,166,819 bytes (−19%), against 2,382,801 bytes of
  non-test Go. The final slice cut nothing. `docs/HISTORY.md` and `docs/history/` hold what a
  changelog structurally cannot — measurements, attacks that were tried, alternatives
  rejected — so their 414 KB → 120 KB target is retired rather than met by deleting evidence,
  the fourth of nine targets to fail on measurement. `HISTORY.md`'s provenance paragraph now
  carries what the next trimmer needs: that what sits under a cited heading is generally
  recorded nowhere else, and that the heading is not the unit of citation, since much of what
  points here quotes a sentence or a number instead of naming a section. Of the nine targets,
  four were retired, three missed after real cuts, and two met or beaten. Why each was
  retired, and the root cause they share, is in `docs/plan/34_doc-trim.md` and the plan's
  record in `docs/HISTORY.md`.

- **`docs/DIVERGENCES.md` becomes the registry its own header promises** (#413) — Twenty-four
  entries had grown into design essays restating derivations their code comments already carry;
  each is now a row that states the surface, what this platform does, whether that is CONFIRMED
  or INFERRED, and where the reasoning lives. 313 KB → 228 KB with all 142 entries, both section
  headers and every cross-document citation intact. Each row was drafted and then attacked by an
  independent reviewer hunting for claims surviving neither in the row nor anywhere in the repo:
  twenty-five did, and were restored — among them a published `pause_turn` sentence that is the
  turn-classification entry's own strongest counter-evidence, and the strip-not-reject argument
  the 0.2.0 changelog points back here for. Nine accuracy defects were fixed rather than carried
  forward, most inherited from the original entries: an error message named as the only one
  lacking a field path where the code has three, a field the SDK's error union requires on every
  variant presented as ours, a bounded path-id enumeration widened to "every", two Anthropic
  bounds attributed to the MCP protocol, and a cumulative byte ceiling stated as a bound on
  delivered bytes when the code counts only part of what crosses the wire. Four citations of
  an "SDK v1.62.0 checkout" are settled against the pinned v1.61.0 they were read at — one in
  archived plan 29, edited for that correction alone.

- **The steering docs and two deployment READMEs stop restating what they point at** (#413) —
  [CLAUDE.md](./CLAUDE.md) gains the standing rule this plan was written from: prefer a comment
  beside the code, and let a document earn its place with what spans files. It then follows its
  own rule, dropping the coverage-denominator list, the test-support inventory and the reviewer
  pins in favour of the Makefile recipe, the naming convention and the `run-reviews` skill that
  own them. [README.md](./README.md) stops narrating delivery the changelog already holds — its
  status line says what runs today, and its roadmap lists only what is deferred, which its own
  "progress is tracked in" pointer had always promised. The chart's "Notable values" table and
  the compose stack's variable table were third homes behind `values.yaml`, `docker-compose.yml`,
  `.env.example` and the binaries' own package docs; both become the shape of the decisions
  instead. Three gaps closed rather than trimmed: how a role-claim *name* is read (a URI-shaped
  name is one flat key, any other dotted name a path — fixed at configuration time so no token
  can choose the reading); that a session's MCP servers are dialled from the **executor
  process**, so a firewall around the sandbox does not bound them; and that `runAsUser` alone
  does not turn off with `0`, which is a valid uid meaning root.

- **ARCHITECTURE.md's package reference is a map now, not a second copy of the code** (#413) — the
  per-file tables were 298,654 of the file's 325,163 bytes, and they were the derivative: nearly
  every package carries more comment than the document spent on it, most by three to ten times
  (`internal/api` 192 KB against 50 KB, `internal/executor` 169 KB against 43 KB).
  Those comments sit beside the code and cannot drift from it; the tables did, and one still
  spelled the gate token `gatetok_` where the constant has always been `gtk_`. What replaces them
  is each package's place in the flow — the one thing a package doc cannot hold, because it is
  about the neighbours — and a pointer to read the package. Nothing was deleted unchecked: of about
  a hundred issue references only four were absent from the source, three of those pure provenance
  and the fourth an open issue; every identifier was checked; and of the citations a 2026-07 note
  warned might quote prose relocated into these rows, exactly one did — `DIVERGENCES.md` restates
  that fact in full itself, and the note's now-stale pointer is corrected here. The
  cross-package sections — overview, topology, execution flow, the wire model, security
  invariants, observability, testing — are kept byte for byte. Recover the original with
  `git show 98c9ac9:docs/ARCHITECTURE.md`.

- **Changelog fragments have a size** — `changelog.d/README.md` now sets one: 60–120 words,
  hard cap 1,500 bytes. A fragment carries what a release-notes reader needs; the longer forms
  have homes already — the `docs/plan/` file for a decision, [docs/HISTORY.md](./docs/HISTORY.md)
  for an acceptance record, [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) for a wire claim, the
  comment beside the code for a mechanism, and the PR itself for a bug's forensics. Every
  unreleased fragment over the cap was recut to it, which is the last moment they can be: at the
  next `make changelog` they fold into CHANGELOG.md and are frozen. First slice of
  [plan 34](./docs/plan/34_doc-trim.md) (#413).

- **A slow MCP server no longer hides the ones behind it, and unreachable servers reach the session log (#45)** — Discovery dialled a session's declared MCP servers serially, so one that never answered spent the whole pass budget and the servers behind it stayed unreached for the life of the session, their tools never offered to the model. Dials now run concurrently, at most eight in flight, each with its own 8 MiB response budget and rows still in declaration order, so a tarpit costs one dial-and-list rather than every server's, still bounded by `EXECUTOR_MCP_PASS_TIMEOUT` (default 5m). A discovery failure now reaches the session log too, naming the server: `mcp_connection_failed_error`, or `mcp_authentication_failed_error` when the credential was the problem, once per work cycle rather than per turn. A server the pass merely ran out of time on says nothing, and does not silence its later verdict.

- **Plan 32 archived — the management-key lifecycle accepted against real Casdoor tokens and the real `ant` CLI** ([#378](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/378)) — over real HTTP against the shipped `deploy/compose` stack with `--profile iam`: an admin issued a key on the console API, drove a read and a mutation on `/v1` with it, disabled it and was refused 401, re-enabled it and was served 200, then archived it for good. A viewer was 403 throughout, and no listing ever carried a key value. A key minted 45 seconds out lapsed on the clock, listed as `expired`, refused re-activation, and still archived. A real `ant` 1.21.0 followed the same key through every state. One caveat the run measured: `requireRole` gates the *human* lane, so a management `x-api-key` still reaches these admin routes and can mint further management keys, recording the issuing key's row id rather than a person. Operator rules: [docs/self-hosted-security.md](docs/self-hosted-security.md) §10. Transcript: [docs/HISTORY.md](docs/HISTORY.md).

- **Management API keys gain a lifecycle: status, expiry and a masked hint** ([#378](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/378)) — migration 0024 replaces the binary `revoked_at` with a `status` of `active` · `inactive` · `archived`, so a key can be **disabled reversibly** and "delete" is an archive that leaves the row readable; existing revoked rows become archived and the column is dropped. A nullable `expires_at` arrives with **absent meaning never**, and existing rows are grandfathered rather than backfilled, so the migration cannot retro-expire a live credential. A fourth state, `expired`, is computed at read time against the database's clock and never stored, so a key stops working the instant it lapses. `created_by` records the issuer with no foreign key, so removing a principal from your IdP leaves the keys they issued working. One operator rule lands ahead of the console surface: naming a value in `CONTROLPLANE_API_KEY` makes that key env-var-managed whatever it was before, adopting an already-issued row and clearing its issuer and expiry, logged as a warning at boot. No route changes here, and the same `x-api-key` keeps working. Operator rules: [docs/self-hosted-security.md](docs/self-hosted-security.md) §10.

- **The `repo-answer` eval runs again** (#358) — the repository-mounting trial
  is back in the live suite now that its fixture exists. It needs
  `EVAL_GITHUB_REPO_URL` and `EVAL_GITHUB_REPO_TOKEN` — renamed from
  `GITHUB_EVAL_REPO_URL` / `GITHUB_EVAL_REPO_TOKEN`, since GitHub refuses to
  store a secret whose name begins with `GITHUB_` — naming a **private**
  repository holding a root-level `PASSPHRASE.txt`, and a fine-grained token
  scoped to that one repository at Contents: Read-only. That privacy is
  load-bearing: it is the only answer-style trial with no per-trial nonce. The
  token and the passphrase now join the artifact scrub, a clone the executor
  retries past a network fault no longer reds the trial, and offline tests pin
  the registered trial set and the count the docs spell, so the two cannot drift
  apart again.

- **Plan 30 archived — console-issued environment keys accepted against the real `ant` CLI** ([#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43), [docs/plan/30_environment-keys-console-issuance.md](docs/plan/30_environment-keys-console-issuance.md)) — a key issued over the console API drove a real `ant beta:worker poll`, with no manual database edits at any point. Revoking through the console returned a bodiless 204 and the *running* worker exited on the platform's `authentication_error`; with two hosts keyed separately, revoking one left the other polling on its still-valid key — the per-host revocation the retired rotate-on-mint model could not offer. A follow-up run closed [#363](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/363), the one criterion the plan had archived without: a real worker claimed a queued `tool_exec`, ran `bash` in-process and settled the result back on the same console-issued key. Both transcripts are in [docs/HISTORY.md](docs/HISTORY.md).

- **Environment keys become per-host: named, individually revocable, expiring** ([#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)) — a BYOC worker credential is no longer *the* credential: rotate-on-mint, which made each new key an environment's only live one, gives way to the reference console's model. The platform now **generates** the secret (256 CSPRNG bits behind an `sk-map-env01-` prefix), returns it once, and stores only its SHA-256 hash alongside a name and a one-year expiry; an environment holds a key per host, and revoking one leaves the others polling. **Keys minted before migration 0021 carry no name and no expiry, and keep working until revoked** — backfilling one would have retro-expired credentials a running worker is authenticating with. No wire shape changed. Lifecycle, expiry and rotation: [docs/self-hosted-security.md](docs/self-hosted-security.md) §6.

- **The `repo-answer` eval was parked, then restored within this release** (issue #358,
  PR #359). The repository-mounting trial was registered unconditionally, but `make eval`
  sets `RUN_EVALS=1` and the trial's fixture configuration fails rather than skips once
  opted in — so with no fixture repository anywhere, the nightly eval job aborted before
  its first session, reddening every run from 2026-08-08 while its other trials passed.
  Taking it out of the registered set returned the nightly to green and the task count to
  the fourteen the docs spell; `evals/repo_test.go` kept the trial and its graders intact.
  It rejoined the suite later in this same release, once the fixture existed — see the
  restore entry; the full account is in docs/HISTORY.md.

- **Deployment identifiers leave the public repository** (#355, PR #356; console
  [#69](https://github.com/OpenSDLC-Dev/managed-agent-console/issues/69)). The
  deploy workflow, `staging-values.yaml` and the GCP runbook no longer name one
  operator's project, cluster, registry, buckets, KMS key or identities. Eleven
  GitHub Actions **variables** carry them instead (not secrets — none is a
  credential): `GCP_PROJECT_ID`, `GCP_ZONE`, `GKE_CLUSTER`, `ARTIFACT_REGISTRY`
  (one `HOST/PROJECT/REPOSITORY` string), `WIF_PROVIDER`,
  `DEPLOY_SERVICE_ACCOUNT`, `BLOB_BUCKET`, `KMS_KEY_NAME`,
  `CONTROLPLANE_SERVICE_ACCOUNT`, `BRAIN_SERVICE_ACCOUNT`,
  `EXECUTOR_SERVICE_ACCOUNT` — sourced per `deploy/gcp/README.md`. A deploy fails
  before authenticating if any is unset, blank or comma-bearing.
  `staging-values.yaml` becomes a reference deployment written against
  `registry.invalid` and `your-project`; the workflow overrides those keys and
  then reads the three Workload Identity annotations back from the cluster, so a
  lost override fails the run instead of silently degrading the brain.

- **Resolved toolset echo, and `mcp_toolset` validated like its built-in twin (#45)** — Every agent read, a session's resolved-agent snapshot, and the `agent` in `session.updated` now render both toolset kinds with `configs` and `default_config` present and each omitted `enabled` / `permission_policy` filled in: `mcp_toolset` resolves to `{"enabled":true,"permission_policy":{"type":"always_ask"}}`, the built-in kind to `always_allow` — both now documented rather than inferred (#59). An `mcp_toolset` naming a server absent from `mcp_servers`, or `mcp_servers` cleared while a toolset still references it, is a 400 on agent create and update and on session create and update, correcting the one-way reading recorded under #66. Unknown keys, a `permission_policy.type` outside the enum, or a wrong-typed leaf are 400s naming the field path, closing the fail-open (#26) where a misspelled key silently resolved to the toolset default.

- **docs/HISTORY.md splits by period** (plan 28 slice 2, archiving the
  plan). The 31 sections whose events belong to 2026-07 — 1,327 lines —
  move to `docs/history/2026-07.md`, relative links re-based for the new
  directory the way `changelog-archive` does it — the split itself was
  byte-reversible, a recomposition from the two output files reproducing
  the pre-split file exactly. That is a property of the move, not a
  standing invariant: the archive is an ordinary document afterwards, and
  plan 34 has since corrected passages inside it.
  HISTORY.md keeps its intro, the Delivery-slices table,
  and the current month over a pointer paragraph naming the archives; 96
  references to moved sections re-point (69 citations in
  docs/DIVERGENCES.md, the rest across archived plan files,
  docs/ARCHITECTURE.md, README.md and one Go comment), while references
  inside archives themselves — the byte-frozen `docs/changelog/` files and
  a `docs/history/` file recording its own era — resolve through
  HISTORY.md's pointer instead.

- **CHANGELOG.md slims to an index; released sections archive per release**
  (plan 28 slice 1). The new `archive` subcommand of `tools/changelog`
  (`make changelog-archive VERSION=X.Y.Z`) moves a released section to
  `docs/changelog/X.Y.Z.md` — re-basing its relative links for the new
  directory, byte-reversibly — and leaves an index stub — the exact dated
  heading over one line naming the section's Keep-a-Changelog groups and
  linking the archive file — so `latest`, the release tag-sanity check, and
  existing `CHANGELOG.md § [X.Y.Z]` citations keep resolving unchanged. The
  move is guarded: the archive must be exactly one section, the document
  must round-trip byte-for-byte, the written archive must invert to the
  moved section, an existing archive file is never clobbered (an interrupted
  run's byte-identical leftover converges the retry), and `notes` refuses to
  ship a stub as a release body (re-runs of a release workflow read the
  tag's checkout, where the section is still inline). Applied to 0.2.0 and
  0.1.0: CHANGELOG.md drops from 6,448 lines to a 35-line index at the cut,
  and the archiving step joins docs/RELEASING.md's ritual as its
  post-release step.

### Fixed

- **A falsified provenance claim, in all three places it was asserted** (#413) — The archive
  header of `docs/history/2026-07.md`, the unreleased `history-split` changelog fragment, and
  `docs/HISTORY.md`'s own plan-28 record each promised that the archive and `HISTORY.md` still
  recompose to the pre-split file byte-for-byte. Slice 3 of this plan had edited that archive,
  so none of the three was true; all now scope byte-reversibility to the move rather than
  asserting it as a standing invariant. The third copy was found only by review, after the
  first two were fixed — grep for the claim, not for the copies you remember. Separately, a
  quoted sentence about the docker wrapper's marker file had drifted:
  `internal/sandbox/docker/api_test.go` cited the archive while quoting `HISTORY.md`'s
  version, which had merged two different archive sentences inside one set of quotation
  marks. Both now quote one archive clause exactly and attribute the `/tmp` detail to the
  separate sentence it comes from. Two archived decisions that plan 29 has since reversed —
  confirmation gating scoped to `agent.tool_use`, and `SandboxProvider` having no `Attach` —
  carry dated notes rather than edits: an overturned decision is what an archive is for.

- **Twelve comments described shipped code as unbuilt, or claimed more than it does** (#413) —
  `internal/worker` said its lease loop "is a later increment" although `cmd/worker` has driven it
  for months, and `internal/queue` said "two kinds share the work_items table" where five do. Eight
  more still pointed at "a later slice" that has since landed: the gate that builds the egress
  substitution engine, the resolution that fills it, `Spec.Env`'s vault placeholders, three sites in
  `internal/identity` waiting on a principals table `upsertPrincipal` writes today, a fourth waiting
  on a test fake clock that now exists, and the fake OpenID provider's own package doc. Two others overstated an invariant: the API's credential map
  called every remaining `/v1` route management-only, though the work API takes the environment key
  and nothing else, and `Spec.Image` claimed every mismatch is refused with `ErrSpecMismatch`,
  though a wedged gated pod is reclaimed first, deliberately. Each is rewritten from the code, and
  the queue's dividing line is now stated correctly: `Claim` is the in-process lane, `Poll` serves a
  BYOC worker exactly one thing — a `tool_exec` item on a `self_hosted` environment, never a
  `model_turn` row.

- **Two documentation defects with teeth** — `changelog.d/cd-build-on-runner.fixed.md` did not
  begin with `- `, which the fragment loader rejects outright, so the next `make changelog` would
  have failed at assembly; it is a well-formed entry now. And `docs/deploy-gcp.md`'s sandbox-pool
  sizing still said nothing calls `Sandbox.Destroy`, so operators were told to size for
  cumulative rather than concurrent sessions — a reap loop has run in every executor since plan
  24, and the guidance now names what frees a pod and what weakens that: with no blob backend
  the idle tier is off, so an idle session holds its pod until it terminates, and a
  `self_hosted` sandbox belongs to the customer's worker and is never reaped. (#413)

- **A wedged work item is ended and handed back, not held forever** (#383). A sandbox
  call that never returned (the Kubernetes exec stalls in CI, #318) kept its lease
  renewing, so the item stayed `active`, unreclaimable by any other executor, and crash
  recovery never fired. The executor and the BYOC worker now bound an item's *silence*,
  not its runtime: a holder that reports no finished step for `EXECUTOR_STALL_TIMEOUT` /
  `WORKER_STALL_TIMEOUT` (default 30m) has its work cancelled and its lease left to
  lapse, while results it already answered are committed best-effort, so a tool usually does
  not re-run — but a settlement slower than the lease's remainder, or a call that ignores
  cancellation, still commits nothing, and an outputs harvest always discards its staged bytes
  rather than commit half a snapshot. Both refuse
  a budget below the longest single step plus a minute (11m; more when
  `EXECUTOR_REPO_CLONE_TIMEOUT` is raised). Recovery still needs a second replica (#396);
  bounding the call itself is #395.

- **A slow lease renewal no longer abandons work whose lease is still live** (#392). Each renewal
  is bounded so it cannot outlive the lease it races, but the bound only held for a punctual tick:
  a renewal that overran its interval left the next attempt's deadline as much as a third of a
  lease *inside* the lease just bought. A slow database then failed the renewal with a
  deadline-exceeded error, cancelling a brain turn or an executor's tool run nothing else could
  yet claim. Each attempt is now budgeted from the last successful renewal's return; a tick
  arriving after that budget is spent reports the lease lost without dialing the database. A
  renewal timeout still does not prove the row free (#400).

- **A tool call the docker sandbox timed out is no longer reported as an ordinary kill** (#390).
  The deadline was always enforced; the label was not. Classification rested on two probes a
  loaded host can schedule after the watchdog's kill has landed: both read false beside exit code
  137, and the model was told its command had died rather than run too long. The watchdog now
  marks its own kill, read as a third witness; the k8s backend lost the same verdict to the same
  race (#95, #110) and shares the fix. The mark can only *raise* a timeout verdict — a hostile
  command can suppress its own mark and get the old mislabel back — and nothing gates the kill on
  it, so none outlives its deadline.

- **The management-key lifecycle now matches the measured reference** (#389). Probed against the
  live console, three of five recorded inferences went against us and two more divergences
  nobody had suspected surfaced. A repeated archive is refused with 400 instead
  of succeeding as a no-op: nothing may be patched onto an archived row, an empty body included.
  A key disabled and then left to lapse renders `expired`, not `inactive`; the precedence is
  `archived` > `expired` > the stored status. A lapsed key admits archiving alone — disabling and
  renaming it are now refused. A past `expires_at` is accepted at issue, minting a key born
  `expired`, previously rejected. An empty patch on a live key answers 200 with the unchanged
  resource. Operator rules: docs/self-hosted-security.md §10; the differing refusal wording is
  registered in docs/DIVERGENCES.md.

- **A BYOC worker mounts its session's files even when the download declares no
  length** (#386). The worker passed the download's `Content-Length` to the sandbox
  write seam unchecked; Go reports `-1` when an intermediary chunks or decompresses the
  body, so the seam refused it and every such mount was skipped, leaving the session an
  empty workspace the system prompt still described as mounted. A length-less body is
  now spooled to a temp file to measure it, bounded at the Files API's 500 MB per-file
  cap and refused rather than truncated above it; the spool is unlinked immediately, so
  a killed worker leaves no customer bytes on disk. Streaming writes now reject a
  non-length size before the first byte moves — a sandbox-contract rule every backend
  must pass. One limit remains: a body delimited only by the connection closing cannot be
  told from a complete one by any client, so behind such a hop a mount can still land short.

- **A zero-byte write no longer opens a Kubernetes exec stream that can never close**
  (#318). A write of no bytes opened an exec stdin stream the client closed at once; the
  pod's side then never completed, stalling the run until `go test`'s package alarm killed
  the whole binary. Such a write now asks for no stream. Because that stream also counted
  the bytes, the write reads one byte itself first, so a stream that disagrees with its
  declared size is still refused instead of landing an empty file over the target. The
  backend's watchdog assertion now measures a paired difference rather than absolute
  latency, so a loaded host no longer fails it. Bounding a wedged exec in production is
  #383.

- **A confirmation sent together with the last outstanding tool result no longer strands the session (#45)** — Resolving a `requires_action` gate and posting an outstanding tool's result in one `POST /events` left the resume counting only the calls the batch denied, never the ones its own results answered. With a client-executed call remaining, the session committed `running` with everything answered and nothing queued, refusing archive and delete until a `user.interrupt`. With a platform-executed one, it enqueued a `tool_exec` (or `web_exec`) for a call already answered, starting a sandbox for nothing, and on a `self_hosted` environment that empty work item was briefly pollable by a BYOC worker. The resume now counts the batch's own results alongside its denials.

- **Environment-key tests now pin the invariants their comments claim** ([#362](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/362)) — four test-only defects from plan 30 slice 1, on security-adjacent invariants: **nothing was ever exposed, and no production code changed.** Three were assertions that did not test what their comments said — an index pin the dropped index also satisfied, a rejection check any error passed, ordering assertions decided by clock resolution — and the fourth a doc comment describing the retired rotate-on-mint contract. The underlying invariants were all still covered elsewhere except the key listing's `id DESC` tiebreak, which nothing covered and a new test now pins; the ordering fix also ends a failure that struck at random when two keys were issued inside one microsecond.

- **A killed test run no longer strands its Docker fixture forever** (#346). The four
  Dockerized fixtures (`pgtest`, `blobtest`, `secretstest`, `gcstest`) remove their
  per-test-binary container in a `defer` a killed process never reaches, so a Ctrl-C or
  a `go test` timeout stranded the container and its anonymous volume for good: one
  machine accumulated twenty strays holding 12.6 GB. Every fixture container now carries
  the label `dev.opensdlc.managed-agent-platform.test-fixture` naming its harness, and
  each fixture's `TestMain` force-removes labelled containers older than six hours, with
  `-v` so the volume goes too. Nothing external has to cooperate: a bare
  `go test ./internal/store` sweeps. The six-hour floor spares a sibling suite running
  concurrently, so a stray outlives the run that killed it and is reclaimed later.

- **The delivery pipeline builds its images on the runner** (#349). The deploy
  workflow now runs `docker build` and `docker push` itself instead of
  submitting to Cloud Build, which failed at the build step with the caller
  forbidden from the project's `_cloudbuild` staging bucket and stayed red with
  both documented IAM remedies applied. `gcloud builds submit` stages the source
  as the *caller* rather than as the build's `--service-account`, so every
  manual run that worked was made by a human with Owner and none exercised the
  path CI takes. Building on the runner needs one permission the deploy identity
  already holds, `roles/artifactregistry.writer`, and no bucket, staging upload or
  Cloud Build API. `deploy/gcp/cloudbuild.yaml` is unchanged and remains the
  manual path's build definition.

- **The GCP Cloud Build path builds again**. `deploy/gcp/cloudbuild.yaml` had
  described a build nobody had run since the Dockerfile gained
  `FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build`:
  `BUILDPLATFORM`, `TARGETOS` and `TARGETARCH` are BuildKit-only variables, so
  under Cloud Build's classic builder they expand to the empty string and the
  daemon rejects the platform specifier outright —
  `failed to parse platform : "" is an invalid component of ""`. Both docker
  steps now carry `env: ["DOCKER_BUILDKIT=1"]`, which is what makes the build run
  at all rather than a performance choice. Alongside it,
  `options.logging: CLOUD_LOGGING_ONLY`, which becomes mandatory as soon as a
  build names its own service account — and it must, because a project created
  under an organisation no longer receives the automatic Editor grant on the
  Compute Engine default service account, and that identity therefore cannot read
  the source tarball `gcloud builds submit` has just uploaded
  (`does not have storage.objects.get access to … <project>_cloudbuild`). The two
  out-of-band IAM grants that follow from it — `roles/logging.logWriter` on the
  project and `roles/storage.admin` on the `_cloudbuild` bucket, both on the
  deploy identity, which uploads that tarball as well as reading it back — are
  recorded in
  [deploy/gcp/README.md](./deploy/gcp/README.md#continuous-delivery), since
  nothing in the repository would otherwise say they exist.

- **The stray root build outputs are untracked and ignored** (#338). PR #333
  committed an 80 MB Mach-O arm64 `worker` — a dev-time
  `go build -o worker ./cmd/worker` swept in by `git add -A` — and it shipped
  inside the v0.2.0 tag. `.gitignore` already carried `/gate`, so the hazard was
  known and only the list was short, which is exactly how it recurred: every
  `cmd/` name (`brain`, `controlplane`, `executor`, `gate`, `worker`) and
  `tools/changelog` is now listed, root-anchored so the `cmd/<name>/`
  directories stay tracked. `.dockerignore` gains the same set, which git
  ignoring does not cover — a developer who builds locally still has the binary
  on disk, and the Dockerfile's `COPY . .` would ship ~80 MB of host-arch
  executable into the image and its layer cache, the same context-size concern
  that file's `.claude/worktrees` entry already documents. History rewriting
  stayed out of scope, so the blob remains in pack history: this shrinks fresh
  working trees, not clone size.

### Security

- **The dial guard reads every NAT64 layout and refuses three more IPv6 transition forms (#45)** — `internal/dialguard`, under the vault-credential probe and the new MCP client, decoded only NAT64's `/96` layout: `64:ff9b:1:a9fe:a9:fe00:808:808` read as `8.8.8.8` rather than the cloud metadata address `169.254.169.254` it targets, while conformant `/48` and `/56` mappings of legitimate targets were refused outright. All six RFC 6052 prefix lengths are tried now; IPv4-compatible, IPv4-translated and ISATAP addresses embedding a blocked target are refused; an address the guard cannot parse is refused rather than admitted; and the guard checks the address as well as its decoded target. Know the edges: only `64:ff9b::/32` is decoded, so NAT64 on an operator's own Network-Specific Prefix is not covered, and 6rd and RFC 2529 6over4 are left undecoded by decision. RFC 1918 stays allowed, and addresses under the RFC 8215 local-use prefix may be over-refused.

- **`hashicorp/setup-terraform` moved to its current major, 3.1.2 → 4.0.1**
  (Dependabot #344). The action had been pinned at 3.1.2 since the GCP staging
  Terraform brought it in (#250), and this is the first bump Dependabot has
  proposed for it; the new pin's SHA resolves to the tag its trailing comment
  claims. v4.0.0's only breaking change is the runtime — the action now runs on
  Node 24, the same runner floor the earlier batch of action majors
  (#187–#190) established for `checkout`, `setup-go`, `upload-artifact` and
  `setup-helm` — and every job in this repository is `ubuntu-latest`, so only a
  self-hosted runner would need attention. v4.0.1 on top clears the `DEP0169`
  `url.parse()` deprecation warning that the Node 24 move surfaced. Nothing in the
  workflow changed beyond the pin: the action's sole consumer is the
  `terraform` job, and all seven of its checks are green on the new pin,
  including the two that actually invoke the installed binary — `gcp-fmt` and
  `gcp-validate`, the latter re-initialising both configurations with
  `-backend=false` before validating them.

## [0.2.0] - 2026-08-07

Added · Changed · Fixed · Security — the full section lives in [docs/changelog/0.2.0.md](./docs/changelog/0.2.0.md).

## [0.1.0] - 2026-07-17

Added · Changed · Fixed — the full section lives in [docs/changelog/0.1.0.md](./docs/changelog/0.1.0.md).

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
