---
status: draft
issue: "#56"
---

# Multi-tenant activation — the workspace becomes a real scoping key (plan 42)

Resolves **#56**. Today the reserved tenancy columns are decoration. Fourteen tables carry
`org_id`/`workspace_id`/`project_id` as `text NOT NULL DEFAULT 'default'`; **exactly two
statement lines in the whole non-test tree name any of those columns**, and they are the two
halves of one `INSERT … SELECT` (`internal/brain/delegate.go:363` and `:365`); **no statement
anywhere filters on them**. *Counting rule: `grep -rn "org_id\|workspace_id\|project_id"
--include=*.go internal/ cmd/`, minus `_test.go` files and `internal/pgtest/`, returns seven
lines — those two, four doc-comment lines (`internal/store/store.go:36` and `:37`,
`internal/api/consoleapikeys.go:31` and `:48`) and one wire field (`:59`).* `internal/store/store.go:34` says the situation in as
many words: "The schema also carries multi-tenancy it does not yet enforce".

End state: **the workspace is a real isolation unit.** Every credential resolves to exactly
one workspace at authentication time; every statement against a scoped table carries the
scope triple as a predicate, is keyed on an id its own function already resolved under scope,
or is a named exemption a reviewer signed off; every insert stamps it; every create-time cross-reference asserts
the referenced row is in the caller's workspace; and a resource in another workspace is
byte-identically indistinguishable from one that does not exist. The one deliberately-global
worker read — the `/v1/skills/{id}` family on the environment-key and `wtk_` lanes
(`internal/api/server.go:559-568`, the predicate at `:569`) — is reversed, because it becomes
a cross-tenant leak the moment tenants are real.

`org_id` and `project_id` stay **enforced but constant**: carried in every predicate and every
insert, never settable, never on the wire.

Six slices, six PRs. **Slices 1-5 cannot produce a cross-tenant answer in production**,
because the surface that makes a second workspace possible lands only in slice 6 — so no
intermediate release can be misconfigured into a half-enforced multi-tenant deployment. That
is a sequencing property, not merely an ordering preference, and it is the plan's answer to
"is an intermediate release leaky?".

**Slice 1 did not start until the live recording of §5 existed; it was taken 2026-09-05 and
the gate is lifted** (§5.1). This mirrors plan 31's own provision for #378: the console-key
surface there was shaped by a real capture before it was built, and four entries in
`docs/DIVERGENCES.md` still cite it — `:125`-`:128`. *Counting rule: `grep -c "#378"
docs/DIVERGENCES.md` → 4.*

---

## 1. Scope decisions settled with the user on 2026-09-03

1. **Proceed, for administrative separation between teams that trust each other.** Row
   scoping is the whole job. The shared substrates — sandbox daemon, blob bucket, credential
   cipher key, work queue, upstream prompt cache — stay registered non-goals rather than
   blockers, because the threat model is a team that must not *see* another team's resources,
   not one that may attack it (§3, §6.10).
2. **One organization per deployment.** `org_id` is frozen at `'default'` and there is no
   organization-creation surface: nothing in the sources gives an organization a *management*
   surface this platform must mirror, and activating it later is additive — every row and
   every predicate already carries the column.
3. **Keep — do not drop — the four write-only scope column sets** on `events`, `work_items`,
   `session_threads` and `principals`. Three are stamped by copy from the session;
   `principals` stays unstamped because the row is not the authority (§6.2).
   Dropping is irreversible and forecloses per-workspace event and work aggregates.
   (`environment_keys` is a fifth table whose columns also stay at the default. It is excluded
   for §6.1's *different* reason — the environment is the authority — not by this decision.)
4. **SSO with a second workspace and `IDENTITY_CLAIM_WORKSPACES` unconfigured fails closed** —
   403 naming the missing configuration. A single-workspace deployment upgrades untouched;
   creating a second workspace refuses every human until the claim is configured, which is an
   operational cliff and belongs in the release note.
5. **A management key acts in exactly one workspace.** No all-workspaces / nullable
   `workspace_id` key class. Workspace administration is a *capability* of the env-var-managed
   bootstrap key and of an SSO `admin`, never a wider resource scope, so no predicate ever
   grows an `OR caller_is_super` arm.
6. **A live multi-workspace console/API recording will be made before slice 1** — an account
   action the user performs, in the shape of #378's 2026-08-13 capture. Slice 1 is gated on
   it. §5 lists exactly what to capture and which entries it converts. **Taken 2026-09-05;
   items 1-5 recorded in full and item 6 on one of its two lanes, the gate lifted** (§5.1).
7. **The reference's per-organization limits stay unenforced** — 100 workspaces, 500 GB of
   files, 1,000 scheduled deployments — re-argued on operator-owned capacity rather than on
   single-tenancy. Enforcement belongs to #46. The recording adds a supporting fact the
   decision did not have: the reference's *rate* limits are organization-wide too, one counter
   decrementing across workspaces inside a window (§5.1), so per-workspace metering is not a
   thing this platform is failing to mirror.
8. **#56 lands first**, assuming independence from #46 and #550; whichever lands second
   inherits the other's tests. #56's own title carries the `(post-v1)` marker that CLAUDE.md
   makes the whole deferral list, so decision 1 is the explicit go-ahead that lifts it for this
   issue alone; slice 6 closing #56 takes it out of that open-issue query with no title edit,
   and every non-goal §3 opens gets the marker so the query stays the whole list.

## 2. Design rules this plan sets on its own

These follow from §1 plus the sources; they are recorded here so a reviewer can attack the
rule rather than reconstruct it from the slices.

1. **The workspace is the isolation unit.** The reference's own rule: "Every request runs in
   exactly one workspace and can only access resources within that workspace" (workspaces
   doc § "API keys and resource scoping", fetched 2026-09-03), and the same page names
   sessions as a workspace-resolved resource twice (§4.2).
2. **`project_id` is frozen at `'default'`.** No project resource, field, id prefix, header or
   spec path exists in the SDK v1.66.0, the CLI v1.26.1, the fetched spec or the public docs;
   the docs model a project *as* a workspace ("Use workspaces to separate different
   projects"). Carrying it in the predicate costs one term in one helper and keeps
   CLAUDE.md principle 5's sentence true.
3. **Tenancy is credential-borne. `/v1` gains no org or workspace path segment, ever.**
   `docs/plan/31_console-sso-rbac.md:166-172` establishes from the SDK that no reference
   management path or request header carries an organization id — the org travels only as the
   `anthropic-organization-id` *response* header — and that the workspace rides the
   `anthropic-workspace-id` *request* header. The `/api/` console dialect keeps the segments
   the reference's console has.
4. **`anthropic-workspace-id` is honored on every lane, under one rule: it may only narrow to
   a workspace the credential already covers.** Not an OAuth-lane feature — verified in the
   pinned checkout: `anthropic-cli/pkg/cmd/workspace_test.go:18` (`TestWorkspaceIDHeader`)
   runs `ant --api-key k --workspace-id wrkspc_flag models list` and asserts the server saw
   the header, and `:50` (`TestWorkspaceIDHeaderOnWorker`) asserts the same on a Bearer
   environment-key `beta:worker poll`. That first test's second case (`:24`) also shows the
   header has an **`ANTHROPIC_WORKSPACE_ID` environment source**, which is worth a deployment
   knowing: a worker or a client can be pointed at a workspace without a command line.
5. **Enforcement is application-level predicates, with a source-reading completeness guard and
   a database write floor.** Row-level security is rejected on evidence (§6.6). The guard is
   `internal/api/rolematrix_test.go`'s mechanism applied to the same class of problem; the
   floor is a composite `(org_id, workspace_id)` foreign key on the nine root tables.
6. **The human lane fails closed**, mirroring plan 31 decision 3's rule for roles: "An
   authenticated principal whose claims map to no role holds no authority (fail-closed)"
   (`docs/plan/31_console-sso-rbac.md:89-90`).

## 3. Out of scope

**Five exclusions are inherited from `docs/plan/31_console-sso-rbac.md:108-129`** — the count
is the bullets in that range, and the fifth is load-bearing here, so it is named rather than
summarised:

- **`ant auth login`** (`:108-118`) — still not taken on.
- **End-user identity** (`:119-121`) — principle 5 stands.
- **Fine-grained / resource-level authorization** (`:122-123`) — three fixed roles only, and
  therefore **no per-workspace role granularity**, where the reference has five workspace-level
  roles on top of its organization roles (§4.2).
- **Casdoor management-plane integration** (`:124-126`) — "the platform never calls the IdP —
  it only verifies tokens." This is *why* §6.2 derives workspace membership from a token claim
  rather than reading groups from Casdoor's API, and it is what §9's CVE argument rests on.
- **A `/v1/organizations/*` Admin API with an admin-key credential class** (`:127-129`). This
  exclusion is now a **choice against a known shape, not against silence**: the reference's
  workspace-administration surface is fully documented — `POST /v1/organizations/workspaces`,
  `GET /v1/organizations/workspaces?limit=10&include_archived=false`,
  `POST /v1/organizations/workspaces/{id}/archive`, `/members` add/update/remove,
  `ant beta:organization:workspaces …`, and `client.beta.organization.workspaces` (workspaces
  doc lines 144-810, fetched 2026-09-03) — while being **absent from every pinned checkout**
  (no `betaorganization*.go` or `betaworkspace*.go` in `anthropic-sdk-go` v1.66.0; no
  organization command among the 85 files of `anthropic-cli/pkg/cmd`; none of the fetched
  spec's 96 paths matches `/organizations|workspace|api_key/i`). Checkout silence is drift, and
  this repo's resolution order puts public docs first, so slice 6 registers two divergences for
  the shape it declines (§7.6).

Added by this plan, each with its reason. Each becomes a registered non-goal with its own
issue; none is a blocker under decision 1.

- **Per-tenant sandbox isolation.** One executor's Docker daemon or Kubernetes namespace runs
  every tenant's containers it serves; the reaper's candidate set is that endpoint's own
  `Owned` list, not a database sweep (`internal/executor/reaper.go:3-9`).
  `docs/self-hosted-security.md:37` and `:1200-1210` already state the daemon is one trust
  domain and the `ours` label "guards against *accidents*". That stays true and becomes
  materially louder.
- **Per-tenant object-storage isolation.** Blob keys carry no tenant:
  `blob.FilesKey(id)` is `"files/" + id` (`internal/blob/blob.go:26`) and
  `skills.BlobKey(skillID, version)` is `"skills/" + skillID + "/" + version + ".zip"`
  (`internal/skills/extract.go:173`). After this plan the SQL predicate is the *only* thing
  between tenant B and tenant A's bytes, and the orphan-cleanup paths delete by key with no
  tenant check at all (`internal/api/files.go:86` `deleteOrphanedFile`, called at `:139` and
  `:300`; `internal/api/skills.go:234` `deleteOrphanedObject`, called at `:315`, `:478`, `:614`
  and `:801`), so a predicate bug on the destructive statements of
  §7.5 destroys another tenant's objects rather than merely reading them.
- **Per-tenant credential encryption.** `secrets.Cipher.Encrypt(ctx, plaintext)` takes no
  tenant, namespace or key selector (`internal/secrets/secrets.go:34` the interface, `:38` the
  signature), so every tenant's `vault_credentials.secret_ciphertext` is sealed under one
  deployment key — one local AES-256-GCM key, one OpenBao transit key, or one Cloud KMS key. A
  missed predicate on that table is therefore credential *disclosure*, not metadata
  disclosure, which is why §7.5 enumerates its `internal/api` statements by hand rather than
  leaving them to slice 4's bulk, and why §6.5's rule (g) covers the table from slice 2. A
  per-tenant key envelope would change the `Cipher` interface and its shared contract suite
  (`internal/secrets/secretstest`).
- **Queue fairness, named as distinct from rate limiting.** `queue.Claim` is one global FIFO —
  `ORDER BY w.created_at … FOR UPDATE OF w SKIP LOCKED LIMIT 1`
  (`internal/queue/queue.go:300`, the sort at `:310`) — so one tenant's burst of accepted,
  rate-limit-compliant work delays every other tenant's turns. **#46 does not cover this**: it
  throttles ingress, not dispatch. Named separately so a reviewer does not read "#46 covers
  it" and be wrong.
- **Upstream prompt-cache isolation.** The workspaces doc's own note: "Prompt caches are also
  isolated per workspace on the Claude API … On Amazon Bedrock and Google Cloud, prompt caches
  are isolated per organization" (fetched 2026-09-03). A self-hosted deployment resolves one
  provider per `model` string with no tenant dimension (CLAUDE.md principle 4), so tenants
  share whatever cache the upstream keeps. Nothing in this plan changes that.
- **Tenant data deletion.** Archive is not deletion (§6.9).
- **Per-tenant retention policy.** The memory-version retention sweep
  (`internal/api/memoryretention.go:108` `pruneMemoryVersions`, statement `:109-118`) is one
  platform-wide policy no tenant can vary. Narrowing it needs a join to `memory_stores` —
  `memory_versions.memory_store_id` is right there (`0029_memories.sql:37`), so the join is one
  line; the reason this is deferred is that a per-tenant *policy* needs a place to live, not
  that the join is hard.
- **Per-organization quotas and limits** (decision 7): #46, `docs/DIVERGENCES.md:102`'s 500 GB
  file quota, `:147`'s 1,000-scheduled-deployments cap, and the docs' 100-workspaces cap.
  **Only `:102` argues from single-tenancy** — a fixed 500 GB ceiling would be arbitrary on a
  single-tenant deployment — so that half expires here and is **re-argued** (§7.6) on the
  operator-owns-its-own-disk half the same entry already states. `:147` carries no tenancy
  word at all: its operator-owned-capacity reason survives this plan unchanged. Neither is
  implemented.
- **Webhooks (#261).** All 44 spec webhook event data schemas require `organization_id` and
  `workspace_id` (§4.2), but the reference SDK exposes
  verification only — no registration, list or delete endpoints. This plan makes #261's
  "whether to invent an org/workspace-scoped registration API" question cheaper; it does not
  answer it.
- **`files.scope_type`'s polymorphism (#266).** `0008_files.sql:24` is a nullable `text` with
  no CHECK, and `listFiles` filters on `scope_id` alone (`internal/api/files.go:215-219`). The
  tenant predicate on `files` contains it today; any future workspace-scoped upload path makes
  `scope_id` collide across tenants. This plan must not deepen it.

## 4. Ground truth

### 4.1 Sources, and how they are cited

Pinned: `anthropic-sdk-go` **v1.66.0**, `anthropic-cli` **v1.26.1** — every SDK and CLI citation
below is stated against those tags and true there (`git show v1.66.0:<path>`). The working
checkouts have since moved to v1.71.0 and v1.31.0, and `go.mod:8` now pins v1.70.1 while
`docs/REFERENCE_PROJECTS.md:51` still names v1.66.0 as the wire-compat judge, so a reader
opening the checkout will not find these lines where this plan puts them. Re-pinning the plan
means re-fetching the spec and re-deriving §4.2's counts from it; that is its own change, and
the registry's own entries are unaffected by it.
Spec: the Stainless document at the URL `anthropic-sdk-go/.stats.yml:2` names, fetched
2026-09-03; **the local copy is JSON-serialized, so its digest does not match `.stats.yml:3`'s
`openapi_spec_hash`** — content is what is cited, and every spec claim below is cited by
**schema name, parameter name or path**, never by line, so a re-fetch can check it. Every
spec-derived **count** in §4.2-§4.3 — the 79/33/46 `x-api-key` declarations, the 96 paths, the
zero header occurrences, the four archive/disable schemas — is likewise this plan's working
index rather than a coordinate a reviewer can re-check: nothing in the repository or either
checkout carries the document, so re-deriving one means re-fetching it, and only the same
document re-derives it. Where an SDK twin exists it is cited beside the count
(`betadeployment.go:1451`, `:1803`; `betadeploymentrun.go:534`, `:880`; `betawebhook.go`'s 44
required `organization_id` and 44 required `workspace_id` fields), and the twin is what a
reviewer checks. That
follows the precedent of `docs/DIVERGENCES.md:153`, which cites
`anthropic-openapi.yml:29161 and :25626 (the two run-error schemas)` by line into a local file
this repo does not carry; the registry states no citation convention (its format note at `:10`
sets none, and both styles appear in it), so this is a choice for new entries rather than an
appeal to one.

**Every `file:line` here is as of this branch's merge base, and one file has already moved
three times under it.** `docs/self-hosted-security.md` gained ~72 lines in a single merge while
this plan was in review, staling twenty citations at once; `docs/DIVERGENCES.md` shifted three
entries by one, and `internal/api/environments.go` moved by ~65. Each was re-derived rather
than left, but the class does not close — so before spending any coordinate in this plan, grep
for the string it names rather than trusting the number, and re-derive any count from the rule
printed beside it. The counting rules are in the plan for exactly that reason; the coordinates
are a convenience that decays.

**The 2026-09-05 tenancy recording** (§5) is cited by file and entry index — `batch8.json`
idx *N* for the console and environment-key lanes, `batch9.curl.txt:N` for the key lane, where
*N* is the line the probe's `###PROBE` marker sits on. Both live in
`OpenSDLC-Dev/managed-agents-wire-recordings` under `2026-09-05/`, alongside `FINDINGS8.md`,
which carries the evidence and — for three claims — says plainly which bytes are *not* there.
That repository is **private**, so these are working coordinates for someone who can open it
rather than ones a public reviewer can re-check; every claim citing them is stated so that a
re-recording of the same probe checks it. The `batchN.index.txt` beside each log maps probe
name to index, which is how a citation survives a renumbering — as one already has: these two
batches landed as 7 and 8, and were renumbered to 8 and 9 when another session's batch 7
reached the repository first.

Public docs fetched 2026-09-03 — the whole set this plan cites:
`/docs/en/build-with-claude/workspaces`, `/docs/en/manage-claude/authentication`,
`/docs/en/api/overview`, `/docs/en/managed-agents/skills` and
`/docs/en/managed-agents/environments`; plus, fetched 2026-09-04 and complete, the two Admin
API api-keys pages `/docs/en/api/admin/api_keys/list` and
`/docs/en/api/admin/api_keys/retrieve` — spelled with an **underscore**, which is the spelling
the workspaces and authentication pages themselves link. An earlier pass recorded both as
`Not Found` under hyphenated spellings — `api-keys`, the list tried twice — which were guesses;
the retrieve page is what settles the api-key `scope` field (§4.2). The skills
page was re-read the same day for its display-name derivation (§7.5). **One** page returns a
literal `Not Found` body, re-checked 2026-09-04: `/docs/en/api/managed-agents/overview`.
`/en/api/admin/workspaces/retrieve` returned 200. **These
pages live outside the repository**, so a line citation into one is this plan's working index
rather than a coordinate a reviewer can re-check: every quotation is verbatim so a re-fetch
finds it by string, and the registry entries the slices add use the registry's own style for a
fetched page — slug, fetch date, quoted sentence (`docs/DIVERGENCES.md:147`, `:150`).

### 4.2 The reference, from those sources

- **The credential carries the tenant.** "Every request runs in exactly one workspace and can
  only access resources within that workspace" (workspaces doc:823). The key kinds, quoted
  with the qualifier that matters: "A **workspace key** (**a legacy key without an owner**)
  belongs to the workspace it was created in and always runs there." · "A **personal key** or
  **service account key** acts as its user or service account. A single-workspace key always
  runs in the workspace chosen when it was created. A multi-workspace key runs in the workspace
  named by each request's `anthropic-workspace-id` header. Accounts must have access to the
  workspace to use it." (`:825-826`). The authentication doc's key-types table labels the third
  "**Workspace key** (legacy)" (`auth doc:29`) and adds "Workspace API keys still work but
  should be considered legacy" (`:35`).
- **The spec is thin on this, and unevenly so.** `x-api-key` is declared **79** times across
  the spec's **96** paths; **33** of those declarations carry "Each key is scoped to a
  Workspace." and **46** are bare, with no description at all. *Counting rule: parameter
  objects named `x-api-key` on the operations of `paths`, partitioned on whether the
  `description` contains that sentence.* The descriptive set is exactly Files, Message
  Batches, Models and Skills; **the bare set is exactly the managed-agents families this plan
  sweeps** — agents, environments, sessions (+events/resources/threads), deployments,
  deployment_runs, memory_stores (+memories/memory_versions), vaults (+credentials), dreams,
  tunnels (+certificates), user_profiles, and two `/work/` routes: 46 declarations over 39
  distinct paths. So the spec is not the citation for the agents/sessions/deployments/vaults
  surface; the workspaces doc is. Likewise **neither header name appears in the spec at all**
  (`anthropic-workspace-id`: 0 occurrences; `anthropic-organization-id`: 0), so the header
  semantics below are public-docs facts.
- **`anthropic-workspace-id` request header.** "Required with a multi-workspace API key.
  Optional for other API keys. Not used with Workload Identity Federation tokens, which select
  a workspace at token exchange" (api/overview:49). The rejections, quoted verbatim from the
  authentication doc:323 — "A header value that isn't
  a valid workspace ID returns a 400 `invalid_request_error` with the message
  `anthropic-workspace-id header must be a valid workspace ID.` If the workspace doesn't exist,
  or the key's user or service account doesn't have access to it, the API returns a 404
  `not_found_error` with the message ``Workspace `<id>` not found.``, the same response as for
  any unknown workspace." **Two of that sentence's three arms are unconditional of any key** —
  the malformed value and the workspace that does not exist; the *access* arm names "the key's
  user or service account", i.e. an identity-linked key class decision 5 mints none of, which is
  why §6.2's fourth bullet was INFERRED from these pages alone. **§5's fourth item observed that
  arm directly on a workspace-scoped key** and it holds there too, so the bullet is CONFIRMED
  from the wire rather than from this sentence (§4.3 item 3). A fourth arm, on the omission
  side, is conditioned on that same class: "If a request made with a key that isn't scoped to a workspace omits
  the header, the API returns a 400 `invalid_request_error`" — "anthropic-workspace-id is
  required when authenticating with an identity-linked API key; send the id of the workspace
  this request acts in." (`:310-320`).
- **What the docs do *not* say, and §6.2 therefore inferred.** For the single-workspace class —
  the one this platform mints — nothing records what a header naming a *different existing*
  workspace does. "Always runs there" and "Optional for other API keys" read as *inert*; the
  404 clause's second half ("the key's user or service account doesn't have access to it")
  reads as *refused*. Both readings were live until §5's fourth item settled them:
  **refused, 404** (§4.3 item 3). The docs are still silent; the wire is not.
- **Two response headers** (api/overview:128-129). `anthropic-organization-id`: "The ID of the
  organization that the API key or access token used in the request belongs to." — one
  sentence, **no absence rule recorded**. `anthropic-workspace-id`: "The `wrkspc_`-prefixed ID
  of the workspace that the API key or access token resolved to … including when that is your
  organization's Default Workspace. Absent when the credential doesn't resolve to a workspace
  (for example, on Admin API requests) or the request fails before authentication completes."
  The SDK reads the workspace header only on error paths and appends `(Workspace-ID: …)` to
  the error string (`anthropic-sdk-go/internal/apierror/apierror.go:29` the field, `:69-70` the
  suffix, as `docs/DIVERGENCES.md:47` itself cites); it never reads the organization header, and says why — `OrganizationID` "is
  intentionally absent: the server exposes the caller's organization only as the
  anthropic-organization-id *response* header" (`internal/auth/types.go:32-35`, the range
  `docs/plan/31_console-sso-rbac.md:167-168` already cites).
- **Workspace ids are `wrkspc_`-tagged.** "Workspace identifiers use the `wrkspc_` prefix"
  (workspaces doc:15); the Default Workspace "has a `wrkspc_` ID like any other workspace"
  (`:17`, `:1102`). The header accepts that tagged form:
  `anthropic-sdk-go/config/writers.go:45-49` — "Stored as
  the tagged `wrkspc_...` form — the same format the CLI flag, profile config, and
  anthropic-workspace-id header accept" (the parallel sentence, in its own words, at
  `anthropic-cli/pkg/cmd/cmd_auth.go:52-56`).
  **`anthropic-sdk-go/config/federation.go:77-86`'s "or the literal
  `default`" is not evidence about the header**: it is the token-exchange *body* parameter, and
  its own comment says "per-request workspace selection (the anthropic-workspace-id header) is
  not supported for federation tokens". Accepting the literal `default` on input is therefore
  ours, and slice 1 registers it as such.
- **Organization-id format: two different fields, so no inconsistency is shown.** The *response
  header*'s documented value is a bare UUID — the workspaces doc's own example is
  `anthropic-organization-id: 0d0e7a3b-52f1-4c7e-9a51-3f6f2f7c1b9e` (`:852`), and the CLI's
  `/v1/oauth/token` response model reads the same value out of `organization.uuid`
  (`anthropic-cli/pkg/cmd/cmd_auth.go:62-65`). The tagged `org_011CZkZZAe0sMna4vkBdtrfx` is
  the **webhook body**'s `data.organization_id`, a payload field on a different surface (the
  spec's `BetaWebhookEvent` example, whose in-checkout twin the organization-behavior bullet
  below cites). So the header's shape rested on one documented example rather than a stated
  rule; **§5 item 3 recorded the live wire and it is that shape** — a bare UUID, on both lanes
  (§4.3 item 4).
- **Workspace-scoped resources.** The doc's list is explicitly non-exhaustive: "Resources
  scoped to workspaces **include**: Files … Message Batches … Skills" (`:828-832`). The same
  page carries the plan's best managed-agents evidence, uncited by the sizing that preceded
  it: "The same accessors read the header from other Claude API endpoints too, **including the
  Claude Managed Agents APIs**. For example, read `anthropic-workspace-id` from the response
  that **creates a session** to record which workspace the session belongs to." (`:1005`), and
  "Open that workspace in the Console to find the request's resources, such as **sessions**,
  files, message batches, and skills" (`:1012`). Memory stores: "a named container for agent
  memories, **scoped to a workspace**" (`anthropic-sdk-go/betamemorystore.go:176`).
- **Two families sit outside API-key workspace scoping**, in that same section: MCP tunnels,
  "managed with a `workspace:manage_tunnels` OAuth token obtained through Workload Identity
  Federation, not an API key" (`:836`), and workspaces and organization members themselves,
  "managed at the organization level through the Admin API" (`:837`). Neither has a counterpart
  here — this platform has no tunnel resource, and §3 declines the Admin API — so the carve-out
  is recorded here rather than as a registry entry.
- **Skills split by source**, from the skills page:22 — a custom skill is "uploaded to **your
  workspace**", while "**Anthropic pre-built skills are already available in every
  workspace**". The same page:24 settles the display name in one sentence: "These examples omit
  the optional `display_name` field, so the skill's display name is derived from the `name`
  field in `SKILL.md`. An explicit `display_name` can be up to 255 characters and doesn't need
  to be unique within your workspace." **All three of those are now settled, and not by this
  plan**: plan 39's 2026-09-04 recording renamed the field to `display_name` (the pinned SDK's
  `display_title` at `betaskill.go:128`, `:177`, `:226` being the pre-GA spelling), matched the
  255-character cap (`internal/api/skillsupload.go:27`), and showed two skills accepted under
  one name — so `0033_skills_display_name.sql:28` dropped the uniqueness this platform used to
  enforce, and `docs/DIVERGENCES.md:60` records the whole entry as converged. What survives for
  this plan is only the **source split**: the catalog is every workspace's, a custom skill is
  one workspace's, which is §6.3's carve-out.
- **Archiving a workspace archives its keys, but no status is recorded for the refusal.**
  Archiving "archives every API key created for that workspace within seconds" (workspaces
  doc), and the authentication doc's key-types table says a workspace key "Stops working when …
  its workspace is archived"; the archive endpoint is
  `POST /v1/organizations/workspaces/{id}/archive` (`:355`). **The refusal is documented; its
  status is not** — the only status either page gives is for an *expired* key ("After a key
  expires, requests made with it return a `401 authentication_error`"), which is what §6.1
  aligned to and **§5's last item recorded: that same 401 `authentication_error`, with the
  unknown-key message verbatim** (§6.1). **The Default Workspace is exempt from
  archiving entirely**: it "cannot be renamed, archived, or deleted" (workspaces doc, re-read
  2026-09-05, the FAQ repeating it verbatim), the rule §7.6's archive handler mirrors.
- **Four archive/disable schemas**, verified by name in the fetched spec: two pause reasons
  requiring only `type` — `BetaManagedAgentsWorkspaceArchivedDeploymentPausedReasonError`
  ("The deployment's workspace was archived."),
  `BetaManagedAgentsOrganizationDisabledDeploymentPausedReasonError` ("The deployment's
  organization is disabled.") — and two run errors requiring `type` and `message` —
  `BetaManagedAgentsWorkspaceArchivedRunError`, `BetaManagedAgentsOrganizationDisabledRunError`
  (SDK twins at `betadeployment.go:1451`, `:1803`; `betadeploymentrun.go:534`, `:880`). The
  run errors' `message` carries only the description "Human-readable error description.", so
  **any wording we emit is ours** and is registered **CONFIRMED** — a deliberate divergence, not
  an inference, because the reference documents no wording a recording could settle it against.
  `docs/DIVERGENCES.md:151` and `:152` are the precedent for the neighbouring class (a run-error
  type nothing here records) and sit in the CONFIRMED section (`:35`), not the INFERRED one
  (`:166`).
- **The organization has observable behavior**, which is why decision 2 rests on the absence of
  a *management* surface rather than on the absence of behavior:
  `BetaManagedAgentsBillingError` — "The caller's **organization** or workspace cannot make
  model requests" (`betasessionevent.go:1228-1230`); `organization_disabled_error` above; all
  **44** webhook event data schemas requiring `organization_id` and `workspace_id` alike, the
  tagged `org_011CZkZZAe0sMna4vkBdtrfx` / `wrkspc_011CZkZaBF1tNoB5wlCeusgy` pair riding
  `BetaWebhookEvent`'s `data` example — and, because §4.1's own rule prefers an in-checkout
  twin to a spec example, the identical literals in
  `anthropic-sdk-go/betawebhook_test.go:21`'s `TestBetaWebhookUnwrap` payload (*counting rule:
  `grep -c 'json:"organization_id" api:"required"' anthropic-sdk-go/betawebhook.go` → 44, same
  for `workspace_id`; `:68` and `:91` are representative sites, and `BetaWebhookEventData`
  itself is a bare 44-variant `oneOf` with no example of its own*); and the
  workspaces doc's role inheritance — "**Organization admins** automatically receive Workspace
  Admin access to all workspaces" (`:49`).
- **The reference has role granularity at both levels.** Five workspace-level roles — Workspace
  User, Limited Developer, Developer, Admin, Billing (workspaces doc:39-45) — with documented
  inheritance from the organization's own roles (`:47-52`, `:1114-1122`), whose count this page
  never states; `docs/DIVERGENCES.md:122` records **seven** of those from plan 31's console
  ground truth. This platform has three **org-wide** roles and no per-workspace granularity at
  all, which is §3's fine-grained exclusion. So `:122`'s divergence *grows* along a second axis
  — per-workspace granularity — rather than changing its count of seven.
- **The api-key `scope` field is fully documented, and it is not an enum.** The Retrieve API Key
  page (fetched 2026-09-04, §4.1) gives it as "`scope: object or object` — Where the API key
  belongs: its Workspace (`{"type": "workspace", "workspace_id": "wrkspc_..."}`, with the
  Workspace's real ID even when it is the organization's default Workspace), or the organization
  (`{"type": "organization"}`) for a principal-bound API key that has no Workspace", of which
  variant it adds "Only a principal-bound API key can have this scope"; the top-level field
  reads "`workspace_id: string or null` — **Deprecated**: Use `scope` instead." The workspaces
  doc:1010 says what the null means — "both report `null` for the Default Workspace, as API keys
  also do for all-workspaces keys" — which is the evidence behind §7.6's key-kinds entry.
  **None of this is a registry item**: the Admin API's `api_keys` resource is not a surface this
  platform mirrors, the console key routes being plan 32's own dialect in place of the Admin API
  §3 declines (`docs/DIVERGENCES.md:125`). Under decision 5
  every key minted here corresponds to the `workspace` variant, and the `organization` variant
  has no counterpart: a consequence of that decision, not a divergence.
- **A cross-organization/workspace mismatch *is* given a status by the SDK, three times, and
  it is 400** — not 404. `betamessage.go:8994`: "Either way: same workspace, same platform; a
  mismatch is a 400."; `betamessage.go:15698-15704` and `betamessagebatch.go:707-713`: "Must be
  redeemed by the same organization and workspace … a mismatch is a 400." **This is
  `fallback_credit_token`, an opaque credit code, not a resource-id reference**, so the
  extrapolation to a foreign `memory_store_id` is arguable either way — but the premise "no
  source records a status" is false, and §8-D10 must weigh it rather than ignore it.
- **The one explicit resource-id cross-reference rule states the requirement without a
  rejection**: "The memory store ID (memstore\_...). Must belong to the caller's organization
  and workspace." — three sites, `betasession.go:900-901`, `betasessionresource.go:319`,
  `betadeployment.go:1408`.
- **Environment `scope: account` is not tenancy.** "'organization' makes the environment
  visible to all accounts. 'account' restricts visibility to the owning account only. Only
  applicable for self-hosted environments. **If not specified, defaults based on organization
  type**." (`betaenvironment.go:733-736`). Per-principal visibility, orthogonal to
  org/workspace — but the default *is* tied to the organization, which nothing in the registry
  records (§7.6).
- **`project` has no counterpart anywhere.** No spec path matches `/project/i`; the SDK's
  non-test Go matches nothing that is a tenancy key — "projection", Kubernetes "projected", GCP
  project plumbing, and three memory-store path examples
  (`betamemorystorememory.go:208`, `:342`, `:418`).

### 4.3 What was NOT OBSERVED — and what the 2026-09-05 recording answered

Five gaps stood open when this plan was drafted. **§5's recording ran on 2026-09-05 and closed
all five** — the four it was scoped for, and the fifth as a by-product. Each is stated as the
question it was, then the answer and where the bytes are. None of them changed the
architecture; the plan changes they *did* force are listed in §5.

1. The status and message for a **cross-tenant resource id** on a managed-agents route (the
   SDK states the `memory_store_id` requirement without its rejection; the only recorded
   mismatch status is `fallback_credit_token`'s 400, on a different kind of value).
   → **404 `not_found_error`, and indistinguishable from an absent id.** Workspace B reading
   A's memory store and reading an id that exists nowhere produce the same status, the same
   error type and the same message template, differing only in the id the caller supplied
   (`batch8.json` idx 5, 7). A session create naming a foreign `environment_id` and one naming
   an absent one likewise agree (idx 18, 19). **CONFIRMED**, and §7.4's byte-identical-404 and
   D10 stand as written. **The per-field question is only half settled**: what was recorded is
   one *read* family and one *create-time reference* family, both answering the same way. A
   second create-time field was attempted and could not be reached — `memory_store_id` is not a
   session-create field at all, and the reference rejects it as an unknown field before any
   tenancy check (idx 20, 21). So "uniform across create-time reference fields" rests on one
   field, and a second one is worth recording whenever a route offers it.
2. Whether an **environment Bearer key resolves a workspace on the work routes**, and how a
   foreign `anthropic-workspace-id` answers there. The spec's `authorization` parameter on
   those paths has no description, and both header names are absent from the spec entirely. The
   2026-09-03 recording narrows this without closing it: that key is an **OAuth token carrying
   named scopes**, two of them workspace-level (`workspace:developer`, `workspace:skills`), and
   admission is per route rather than per API (`docs/DIVERGENCES.md:163`). So a workspace
   dimension exists on that credential; whether a *response* names it, and what a foreign header
   does there, were still unobserved.
   → **It does not, and the work lane ignores the header entirely.** The mint response grants
   `{"type": "ccr_env", "actions": ["poll","list","stats"], "environment_id": …}` with no
   workspace in it (`batch8.json` idx 37); a successful poll carries `request-id` and **neither**
   tenancy header (idx 27); and a foreign, own or malformed `anthropic-workspace-id` all return
   200 (idx 28-30) where the same malformed value is a 400 on the key lane. **CONFIRMED**, and
   it is the strongest possible support for §6.1's choice to derive that lane's scope from the
   `environments` row: the reference's own credential carries no workspace to derive from.
3. What a **single-workspace key sending a header naming another existing workspace** does —
   200-ignored ("always runs there") or 404 (the access clause). §4.2 names both readings.
   → **404**, the access clause, not 200-ignored (`batch9.curl.txt:24`). The other arms, in
   full bytes: malformed → 400 with the documented message verbatim (`:63`), an unknown but
   well-formed `wrkspc_` id → 404 (`:79`), an empty value → 200 ignored (`:111`), the key's own
   workspace → 200 (`:40`). **CONFIRMED**, and §6.2's narrow-only rule now mirrors an observed
   refusal rather than choosing between two readings.
4. Which **organization-id format** the response header carries **on the live wire** — the
   workspaces doc illustrates a bare UUID in its own example (§4.2), so §5 item 3 was expected
   to confirm that shape rather than discover it.
   → **A bare UUID**, on both the cookie and key lanes (`batch8.json` idx 0,
   `batch9.curl.txt:1`). Its companion `anthropic-workspace-id` is the `wrkspc_` form and names
   the workspace the request resolved to, not the one it asked for. **CONFIRMED**; §7.1 needs
   no INFERRED format fallback.
5. When **`anthropic-organization-id` is absent** — the docs give absence conditions for the
   workspace header only, so emitting the two on one schedule is a choice, not a mirrored rule.
   → **The reference has no single schedule. Two axes decide it, and route comes first.**
   `batch8.json`'s 38 entries fall into five header combinations:

   | headers | entries | what they are |
   |---|---|---|
   | rate-limit + `anthropic-organization-id` + `anthropic-workspace-id` | 0, 1, 3, 5, 6, 7, 12, 18, 19 | reached the route on `/v1/memory_stores`, `/v1/agents`, `/v1/sessions` — 2xx **and** 4xx alike |
   | the two tenancy headers only | 8, 9 | field-level validation on those same routes |
   | rate-limit + `anthropic-organization-id`, **no** workspace header | 31, 32, 35, 36 | the console dialect's 200s, which stamp the organization alone — even where the path names a workspace (31, 35 are workspace archives) |
   | `request-id` only | 4, 10, 11, 13-17, 20-30, 33, 34, 37 | three unlike things: body-parse failures on the tenancy-stamping routes (10, 11, 13-17, 20, 21); **every** response from three families that never stamp — `/v1/environments` (4, 23, 25, 26), the work-poll lane (24, 27-30), `/api/oauth/…` (22, 37); and the console dialect's 404s (33, 34) |
   | nothing at all | 2 | the pre-auth 401 |

   So **whether a route stamps at all is decided first** — `/v1/environments` returns 200 with
   none of them (idx 4, 23, 26) while `/v1/memory_stores` and `/v1/agents` on the same lane
   seconds apart carry the full set — and only *within* a stamping route does how far the
   request got decide which headers appear. Our one schedule remains **ours**: a documented
   simplification of a two-axis rule rather than an unmirrored invention, and strictly wider
   than it (see §6.2). The registry entry says so.

§5's two further items are answered in place: the console's own workspace administration (its
item 5, which gates slice 6) at §7.6, and the archived-workspace refusal (item 6) at §6.1.

### 4.4 Platform as-is

- **No tenant reaches any request context.** `authenticate` runs `SELECT id FROM api_keys WHERE
  key_hash = $1 AND status = 'active' AND (expires_at IS NULL OR expires_at > now())` and
  returns only the row id (`internal/api/auth.go:165` the func, `:167-171` the statement);
  `authenticateEnvironmentKey` returns only `environment_id` (`internal/api/envauth.go:18`,
  statement `:21-24`). The api package has exactly six context keys and none carries a tenant
  (`internal/api/errors.go:114` the type, `:117-122` the constants).
- **The identity lane has no tenant source at all.** `identity.Identity` has five fields —
  Issuer, Subject, Email, DisplayName, Role (`internal/identity/identity.go:145-151`); only
  `RolesClaim`/`EmailClaim`/`NameClaim` and `RoleMap` are configurable
  (`internal/identity/config.go:26-29`); `upsertPrincipal` never writes `principals`' reserved
  columns (`internal/api/principals.go:35` the func, `:37-44` the statement).
- **Exactly one statement writes scope**, and it is the house pattern:
  `internal/brain/delegate.go:362-367` copies the parent session's triple into a child
  `session_threads` row via `INSERT … SELECT`, with the reason at `:356-361`. Its sibling — the
  *primary* thread insert at `internal/api/sessions.go:753` — does not, so one session's thread
  rows would disagree with each other the moment a session lives outside `default`. Migration
  `0025_session_threads.sql:44-48`'s own backfill got it right; the live insert is the outlier.
- **Four more writes into scoped tables rely on the column default:**
  `internal/events/log.go:203` (`events`, a `strings.Builder` multi-row INSERT),
  **both** `work_items` inserts — `internal/queue/queue.go:224-229` (`EnqueueThread`) and
  `:258-263` (`EnqueueOutputsHarvest`, which plan 38's idle harvest added) — and
  `internal/executor/harvest.go:394-396` (`files`).
- **The scoped-table set is 14**: `agents`, `environments`, `sessions`, `events`, `work_items`,
  `api_keys`, `environment_keys` (`0001_init.sql:15,41,66,101,116,141,154`), `skills`
  (`0007:9`), `files` (`0008:11`), `vaults` (`0011:10`), `principals` (`0022:43`),
  `session_threads` (`0025:14`), `memory_stores` (`0028:10`), `deployments` (`0031:16`).
  *Counting rule: `grep -n org_id internal/store/migrations/*.sql` returns **19** lines (20 once
  slice 1's `0034_workspaces.sql` lands — §6.5 step 2) in 9
  files; five are not column definitions — `0001:6` (prose), `0007:25` (the partial index),
  `0013:43` (prose) and `0025:44`, `:46` (the backfill).* Eleven child tables inherit through a
  foreign key. **Two inherit nothing**: `deleted_sessions` (`0018:12-16`, no `REFERENCES` by
  design — the tombstone outlives its session) and `session_checkpoints` (`0019:11-16`,
  `session_id text PRIMARY KEY` at `:12` with no `REFERENCES`), so
  `internal/store/store.go:37-38`'s inheritance sentence is untrue for exactly these two.
- **No index is tenant-keyed any more.** The one that was — `skills_custom_display_title_uq` on
  `(org_id, workspace_id, display_title) WHERE source = 'custom'` (`0007_skills.sql:24-26`) —
  is **dropped** by `0033_skills_display_name.sql:28`, the reference's GA rename having taken
  the uniqueness with it (§7.5), so nothing in the schema keys on the three columns at all. Of
  the unique indexes left on scoped tables, two are keyed through a parent id and cannot collide
  across tenants (`work_items_live_session_thread_kind_idx`, `0026_work_thread.sql:21`;
  `session_threads_primary_idx`, `0025_session_threads.sql:35`) and two are not:
  `api_keys_one_live_unissued ON api_keys (name)` (`0024_api_keys_lifecycle.sql:77-78`), which
  §4.5 item 1 and slice 5 rebuild with the triple, and `files_scope_filename_idx ON files
  (scope_id, filename) WHERE scope_id IS NOT NULL` (`0017_outputs_harvest.sql:15-16`), safe only
  because `scope_id` holds a session id — exactly the property §3's #266 bullet guards, and the
  reason a workspace-scoped upload path would make harvested filenames collide across tenants.
  No trigger, function, RLS policy or CHECK anywhere references the three columns.
- **Ten list handlers build SQL on a literal `WHERE true` base**, but that literal is the
  insertion point for only **eight** of them: `agents.go:430`, `deployments.go:365`,
  `environments.go:572`, `files.go:215`, `memorystores.go:291`, `sessions.go:1123`,
  `skills.go:360`, `vaults.go:209`. The other two sit on tables that carry **no scope columns**.
  `deploymentruns.go:274` lists `deployment_runs` (`0031_deployments.sql:127`, no `org_id`) and
  its `deployment_id` filter is **optional** (`:276`), so an unfiltered
  `GET /v1/deployment_runs` would stay cross-tenant — it needs a
  `JOIN deployments d ON d.id = r.deployment_id` to carry the predicate. `memories.go:611`'s
  `WHERE true` is on the *derived* table `matched` (`:600-611`), whose projection is
  `memoryColumns`; the parent term `memory_store_id = $1` is at `:609` on `memories`
  (`0029_memories.sql:8`, no `org_id`), so its enforceable point is the parent memory-store read
  (`:135`, `:165`). An **eleventh** list builder is invisible to this count because it has no
  `WHERE true` base: `listThreads` (`threads.go:117`, query `:133`, keyset term `:138`), which
  scopes through the parent session its `sessionExists` gate at `:130` resolves. *Counting rule:
  `grep -n "WHERE true" internal/api/*.go` minus `_test.go` — ten lines, none in `threads.go`.*
- **Private id prefixes are an established family.** `PrefixEnvironmentKey` (`:49`),
  `PrefixPrincipal` (`:56`) and `PrefixAPIKey` (`:63`) in `internal/domain/id.go` are
  deliberately **outside** `knownPrefixes` (`:95`), with the reason written in place at
  `:42-48`: that set is what every `/v1` path accepts as an id shape, and widening it for a
  private identifier "would widen all of them". `internal/domain/docs_test.go:53-55` states the
  same exclusion, and `:56`'s test pins documents to `knownPrefixes` only, so a private prefix
  costs no documentation edit. The validator is `domain.ValidWithPrefix`, already used on a
  console path at `internal/api/consoleapikeys.go:202`.
- **No non-test Go composes a table name.** Fragment concatenation, by contrast, is pervasive:
  **77** lines in the target packages' non-test files place a backtick-quoted string
  adjacent to a `+`, including `internal/events/toolflow.go:72` (`func answeredBy` — a
  *function returning a SQL predicate*, spliced at **six** sites: `:89`, `:378`, `:403`, `:527`,
  `:610`, `:679`), the predicate
  variables `unansweredToolUse` (`:86`), `runnableToolUse` (`:101`) and `threadClause` (`:116`),
  and `internal/queue/lifecycle.go:47` (`workAPIScope`, spliced at **eleven**: ten in that file —
  `:79`, `:91`, `:109`, `:154`, `:186`, `:207`, `:283`, `:296`, `:343`, `:366` — plus the
  `/work/stats` depth read at `internal/queue/stats.go:60`). *Counting rules:
  `grep -cE "\` *\+|\+ *\`"` over those files, summed; `grep -n` on each identifier for its
  splice sites — the declaration and the comment mentions are not splices.*
  This is what §6.5's fail-closed rule must be written against.

### 4.5 Where the inherited sizing is wrong

`docs/plan/31_console-sso-rbac.md:99-107` and #56's body are stale in three places, and this
plan carries the corrections so the verifier's docs rung does not flag them:

1. **The index to rebuild is `api_keys_one_live_unissued`, not `api_keys_one_live`.**
   `0013_key_rotation_one_live.sql:43-46` pre-announced the rebuild — "api_keys' slot is bare
   `name`, not (org_id, workspace_id, project_id, name) … When the reserved tenancy columns
   become real scoping (principle 5), this index is rebuilt with them" — but
   `0024_api_keys_lifecycle.sql:76` already dropped it and `:77-78` created
   `api_keys_one_live_unissued ON api_keys (name) WHERE status = 'active' AND created_by IS
   NULL`, and `revoked_at` no longer exists. `environment_keys_one_live` was dropped by
   `0021_environment_keys_named.sql:25`; nothing to rebuild there.
2. **The worker-lane policy is at `internal/api/server.go:305` and `:559-569`**, not the cited
   `server.go:329-360`. The dispatcher arm is `:346-347`.
3. **The statement count is 211 across 32 of the 46 non-test `internal/api` files**, not 60-80
   across 13. *Counting rule: occurrences of the four statement-opening tokens `SELECT `,
   `INSERT INTO `, `UPDATE <ident> SET`, `DELETE FROM ` across those 46 files, counting a
   nested subquery's `SELECT` as its own occurrence and including statements on unscoped
   tables.* A second, narrower rule gives the guard's real workload: mentions of one of the 14
   scoped tables after `FROM`/`JOIN`/`INSERT INTO`/`UPDATE`/`DELETE FROM` number **143** in
   `internal/api`, **51** in `internal/events`, **31** in `internal/queue`, **22** in
   `internal/executor` and **26** in `internal/brain` — **273** in all, plus **2** in
   `internal/vaultresolve`, the guard's sixth target package (§6.5). These are **mention**
   counts — one statement can contribute several (`internal/api/threads.go:133-134` yields both
   `FROM session_threads` and `JOIN sessions`) — where 211 is a **statement** count; by the
   statement rule `internal/queue` holds 18, `internal/events` 59, and brain, executor and
   vaultresolve 77 together (29, 44 and 4), and later sections spend each figure in its own unit. *That rule as an
   expression: `grep -roE "(FROM|JOIN|INSERT INTO|UPDATE|DELETE FROM) (<the 14 names>)([^_a-z]|$)"
   --include=*.go <pkg>` minus `_test.go` lines.* Create-time
   cross-references are ~16 sites, not 6, and **three have no existing site at all** (§7.4).

## 5. The recording that gated slice 1 — taken 2026-09-05

> **Done. The gate is lifted** — §5.1 has the outcome and what it changed. What follows is the
> specification the recording was run against, kept because each item names what its
> observation converts, and §4.3's answers are keyed to those items.

Decision 6. This is an account action creating real credentials in a real organization, in the
shape of #378's 2026-08-13 console capture, so only the user can perform it. **Slice 1 does not
start until items 1-4 and 6 exist** — the same provision plan 31 made for #378, whose capture
still anchors four registry entries (`:125`-`:128`). Item 5 gates slice 6 instead, and item 6
rides on the archive item 5 performs. Capture request and response headers and full error bodies
throughout; a second workspace and one key bound to it are the whole setup.

What to capture, and what each observation converts:

1. **A cross-tenant resource id on a managed-agents route.** With workspace B's key, create a
   session naming a `memory_store_id` that exists in workspace A. Record **status, error
   `type` and the exact `message`**. Repeat for one more family (an `environment_id` on
   `POST /v1/sessions`) to see whether the answer is per-field or uniform.
   → converts §4.3 item 1: §7.4's entry for "a cross-tenant resource read and a cross-tenant
   create-time reference both answer the absent-id 404" lands **CONFIRMED** at whatever the
   reference actually answers rather than INFERRED — and if that is a 400, §8-D10 and §7.4's
   refusal shape change with it, which is why this is the recording's first item.
2. **Whether an environment Bearer key resolves a workspace on the work routes.** Poll
   `GET /v1/environments/{id}/work/poll` with an environment key and read the response's
   `anthropic-workspace-id` header (present or absent). Then send the same poll with an
   `anthropic-workspace-id` naming a *foreign* workspace and record the answer.
   → converts §4.3 item 2, and with it §6.1's decision to derive the environment lane's scope
   from the `environments` row.
3. **The organization-id format.** Read `anthropic-organization-id` off any Managed Agents
   route's response (a session create is enough) and record the literal value — bare UUID or
   `org_`-tagged.
   → converts §4.3 item 4: the CONFIRMED divergence for this platform emitting `default` gains
   the real shape it diverges from, and needs no INFERRED format fallback beside it (§7.1).
4. **A single-workspace key sending a header naming another workspace.** With workspace B's
   key (bound to B) send `anthropic-workspace-id: <A>` on a Managed Agents read. Record
   200-ignored versus 404, and the body if 404. Then repeat with a *malformed* value and with
   an *unknown* workspace id, to confirm the two documented rejections on this key class.
   → converts §4.3 item 3. §6.2's header rule and slice 6's acceptance step (d) are written
   against this observation; until it exists, "narrow only" is **ours**, and the 400-malformed
   and unknown-404 arms are the only CONFIRMED halves. If the account can mint a multi-workspace
   key, also record a header-less read with it to confirm the reference's documented omission 400
   (auth doc:310-320) — the model §6.2's SSO multi-member-no-header arm mirrors (finding: that
   arm is ours, tracked #78).
5. **The console's own workspace administration** — the one item that gates **slice 6**, not
   slice 1, and is captured in the same sitting because creating workspace B in the console is
   already step one of this setup. Record that create's request and response, the workspace
   listing, and an archive. `docs/DIVERGENCES.md:86` justifies the whole `/api/` dialect by its
   having been "mirrored segment-for-segment from the reference console's own private backend
   (observed live 2026-08-10)", and slice 6 adds two routes to that dialect (§7.6) with no such
   capture behind them.
   → if the *capture* proves unreachable, slice 6 registers the two new routes as ours in
   *shape* as well as in placement, rather than presenting them as mirrored. The *create* itself
   cannot fail without failing the recording: without workspace B there is no item 1-4 or 6, and
   slice 1 stays gated.
6. **An archived workspace's credentials**, last of all, because it destroys the setup: after
   item 5's archive of throwaway workspace B, call a Managed Agents read with B's management
   key and `GET /v1/environments/{id}/work/poll` with B's environment key, recording the
   **status and error `type`** for each.
   → converts §6.1's archived-workspace refusal from an alignment with the documented
   *expired*-key status (§4.2) into CONFIRMED at whatever the reference answers — a deliberate
   divergence if that is not a 401 `authentication_error`. It gates slice 1 because §6.1's rule
   lands there.

If item 1 comes back as a 400, that is a plan change, not a detail: §7.4's byte-identical-404
refusal, its per-statement-kind assertions and D10's split of refusal answers are all keyed to
it, and the slice-4 PR must carry the correction. Items 2-6 change registry entries and one
acceptance step, not the architecture.

### 5.1 Done, 2026-09-05 — the gate is lifted

**Items 1-5 were recorded in full and item 6 on one of its two lanes; slice 1 is unblocked and
slice 6's item 5 is answered too.** 07:13:03Z-07:43:18Z, at US$0 — every probe is a
control-plane call, and no model turn ran. Three throwaway workspaces
(`plan40-tenancy-recording-A/B/C` — names created on the wire while this plan was numbered
40, so they stay verbatim; renaming them would falsify the bytes) were created and archived
inside the half hour; three credential lanes were driven, the third of them by `curl`
because a browser cannot reach it (CORS preflight on `POST` to `api.anthropic.com` answers
405, and script may read only two response headers on a `GET`). Every credential minted —
three workspace API keys, two environment tokens — was revoked or archived and refused
afterwards; **the bytes for one of those five refusals are in the archive**
(`batch9.curl.txt:150`, workspace C's key). The other four are attested by `FINDINGS8.md`
alone: keys A and B from runs it records as header-filtered, and the two environment tokens
from post-revocation probes it does not describe. Both refusals are stated there as
observed; neither has bytes here. `batch8.json`, `batch9.json`, `batch9.curl.txt` and
`FINDINGS8.md`, cited as §4.1 sets out.

**Item 1 came back a 404**, so the architecture stands unchanged: §7.4's byte-identical-404
refusal, its per-statement-kind assertions and D10 are all as drafted, and no slice-4
correction is owed. What the recording *did* change:

- **§4.3's five open items are all CONFIRMED**, with the fifth — the header schedule — turning
  out to be answerable after all. Our one schedule stays ours, now as a stated simplification
  of a two-axis rule — route first, then how far the request got — rather than an unmirrored
  invention.
- **§6.1's archived-workspace refusal is CONFIRMED** at exactly the status it was aligned to.
- **§7.6's two console routes are confirmed in shape**, not merely in placement, so the
  contingency in item 5 above — registering them as ours in shape — does not fire.
- **One new deliberate divergence**, from item 4's bytes: the reference's foreign-workspace 404
  echoes a *decoded UUID* rather than the id it was sent, and produces one even for an id
  belonging to no workspace. Ours echoes what it was given. §6.2 states the rule; the registry
  carries the entry.
- **Three findings nobody asked for**, none of which changes a slice: rate-limit buckets are
  organization-wide rather than per workspace (which is decision 7's premise, now evidenced
  rather than assumed); `/v1/environments` stamps no tenancy headers even on 200; and the
  literal string `default` is *rejected* in `anthropic-workspace-id`, so this platform
  accepting it (§7.1) is genuinely ours and is registered as such.

**One half of item 6 is NOT OBSERVED and stays so**: an *environment* key belonging to an
archived workspace. The console's mint endpoint could not reach an environment in a
non-default workspace, so no environment token was ever bound to a workspace that could then
be archived. §6.1's rule covers both lanes; only the key lane is confirmed. Tracked as part of
#56's own recording debt rather than #78's, because it needs this plan's own two-workspace
setup to ask.

## 6. Architecture

### 6.1 The scope, and where it enters

`internal/api/errors.go:117-122` gains a seventh context key `ctxKeyScope` holding a
`domain.Scope`, with a `scopeFrom(ctx) (domain.Scope, bool)` accessor beside `principalFrom`
(`internal/api/auth.go:227`). **`!ok` is an internal error at every call site, never
"unscoped, proceed"** — a handler that forgets to fetch the scope cannot serve a request that
needs one. No handler ever computes a scope; five choke points derive one, each by widening a
query that already runs:

| Lane | Source | Change |
|---|---|---|
| Management `x-api-key` | the `api_keys` row | `internal/api/auth.go:167-171` selects the triple beside `id` |
| Environment key (Bearer) | the **`environments`** row | `internal/api/envauth.go:21-24` gains `JOIN environments` |
| Work token `wtk_` | the `sessions` row | `internal/worktoken/worktoken.go:102-112` already joins it |
| Gate token `gtk_` | the `sessions` row | `internal/gatetoken/gatetoken.go:103-107` already joins it |
| Human (SSO) | IdP claims | §6.2 |

The environment key derives from the **environment**, not from `environment_keys`' own reserved
columns, so a key can never disagree with the environment it serves — no copy at mint time, no
drift, no backfill. Those columns therefore stay unread and unstamped, said out loud so the
omission reads as deliberate. Tokens 3 and 4 never declare a tenant; they inherit.

**An archived workspace fails the resolution.** A credential resolving to a workspace with
`archived_at` set is refused exactly as an inactive key is: 401 `authentication_error` on the
key lanes, the fail-closed no-authority refusal on the identity lane. **CONFIRMED on the key
lane, 2026-09-05**: archiving workspace C and calling with its key answers `401`
`authentication_error` `"API key is invalid."` with `request_id: null` and no tenancy headers —
byte-for-byte what an unknown key gets, with and without the workspace header
(`batch9.curl.txt:150`, `:166`; reproduced on workspaces A and B). So the alignment this plan
chose is the reference's own answer, not merely a defensible reading of its expired-key rule,
and the message is a template worth matching too: the reference does not distinguish an
archived tenant from a bad credential, which is the right disclosure. **The environment-key
half stays NOT OBSERVED** (§5.1) — that lane's rule is ours until a recording can bind an
environment token to an archivable workspace. Slice 1 registers both halves (§7.1).

**Background paths have no credential and copy instead of deriving.** The deployment scheduler
is the only session-create path with no request principal — its own comment says so
(`internal/api/deploymentscheduler.go:504-505`, above the `createSessionInTx` call at `:506`) —
and its fire transaction already re-reads the deployment row `FOR SHARE` (`:444-451`), so the
three columns join that projection and pass into `createSessionInTx`. The manual-run twin does
the same on its own `FOR SHARE` re-read (`internal/api/deploymentruns.go:82-85`). The brain and
executor consumers already re-read the session under a row lock at the top of every item
(`internal/brain/brain.go:500-505`, `internal/executor/executor.go:945-950`), so scope joins
those column lists at zero extra round trip — which is why **none** of `queue.Item`
(`internal/queue/queue.go:94`),
`Claim`'s own `RETURNING` list (`internal/queue/queue.go:320-324`) or `workColumns` (`:170`,
the wire projection every Work-returning query selects) grows a field. Those are three
independent decisions and all three are "no" (§8-D6, which cites the same declaration).

### 6.2 The human lane, and the header rule

**Membership from claims, selection from the reference's own header.** Two new configuration
names, mirroring the existing `RolesClaim`/`RoleMap` pair exactly
(`internal/identity/config.go:26-29`): `IDENTITY_CLAIM_WORKSPACES` — a multi-valued claim
resolved through the existing machinery, `claimAt` (`internal/identity/claims.go:27`) for the
name and the shape of `roleValues` (`:86`) for the values, whose name is fixed at boot so no
token can switch it (the hazard `claims.go:19-26` already argues for roles) — and
`IDENTITY_WORKSPACE_MAP`, a claim value → workspace id map, so an operator binds IdP group
names rather than pushing platform-minted `wrkspc_` ids into the IdP. `identity.Identity`
gains the resolved set; nothing is persisted.

**The principal row stays unwritten**, and the schema already argues why:
`0022_principals.sql:12-15` says roles are deliberately not a column because "storing them
would make this table a second, stale authority beside the IdP: a human demoted at the provider
would keep the role until something wrote the row again." A stored tenant is the same mistake
with the same failure mode. `principals.org_id/workspace_id/project_id` (`0022:43-45`, under
the reservation comment at `:39-42`) stay reserved and unread — decision 3's one exception,
and the reason for it.

**The fail-closed arm, as a checkable condition rather than a hope** (decision 4): claim
unconfigured **and exactly one live workspace** (`archived_at IS NULL`) → that workspace, so
every existing single-tenant SSO deployment keeps working untouched. Claim unconfigured **and
more than one live workspace** → 403 naming the missing configuration. A configured claim
resolving to no live workspace → no authority. **Live rows, not rows**: archive deletes nothing
(§6.9), so counting rows would lock every human out of a deployment for ever the moment one
throwaway workspace had been created and archived.

**One header rule for every lane.** `anthropic-workspace-id` may only *narrow* to a workspace
the credential already covers.

- absent → the credential's bound workspace: a key's single workspace, or the human lane's set
  when it has exactly one live member. When the human lane's set has **more than one live
  member** and no header selects one → **400 `invalid_request_error`** naming that the identity
  spans multiple workspaces and none was selected, the `anthropic-workspace-id` header required
  to choose one. This **mirrors** the reference's rule that a multi-workspace key runs in the
  workspace its header names (§4.2); but the SSO lane has no exact reference counterpart, so the
  status is **INFERRED** (tracked #78), and it is **distinct from decision 4's 403** — the 403
  is a missing server-side *configuration*, this is a missing per-request *selection*. §5's
  recording did **not** reach the reference's own answer for a multi-workspace key sent with no
  header, the model this follows, so the status stays INFERRED. It did evidence that such a
  credential exists, at one remove: an org-level key create refusing both discriminators names
  all three forms — `Provide workspace_id (a workspace key), principal_id (an identity-linked
  key), or both (an identity-linked key bound to a workspace).` — so the unbound `principal_id`
  form is the multi-workspace class, and minting one needs a principal id this recording had no
  way to obtain. That message is the third of FINDINGS8's three byte-less claims (§4.1): it is
  recorded there as a session note, so this reading rests on the note rather than on retained
  bytes. Tracked #78.
- malformed → 400 `invalid_request_error`, the reference's message verbatim (§4.2). "Malformed"
  is a real check: `domain.ValidWithPrefix(v, domain.PrefixWorkspace)` or the literal
  `default`. **CONFIRMED** — the docs state this arm unconditionally, and 2026-09-05 records it
  (`batch9.curl.txt:63`). **Accepting the literal `default` is ours**: the reference answers
  that exact 400 for it (`:95`), as it does for a bare UUID (`:134`). We accept it because
  `default` is this deployment's own frozen workspace id (§7.1), which the reference has no
  equivalent of; registered.
- an **unknown** workspace → 404 `not_found_error` ``Workspace `<id>` not found.``
  **CONFIRMED** (`batch9.curl.txt:79`).
- a **real workspace the credential does not cover** → the identical 404 body, so existence
  never leaks. **CONFIRMED 2026-09-05** — it is a 404, not the inert 200 the contrary reading
  allowed ("always runs there", the header "Optional for other API keys"), and the covered and
  uncovered answers are indistinguishable on both sides (`batch9.curl.txt:24` against `:79`).
  **What differs is what the body echoes**, and that is a divergence rather than a detail: the
  reference prints a *decoded UUID* — it prints one even for `wrkspc_01AAAAAAAAAAAAAAAAAAAAAA`,
  an id belonging to no workspace, so it is decoding the input rather than looking it up. (That
  the decode of a live workspace's id equals that workspace's own `compartment_id` field was
  seen once and its bytes not retained; `FINDINGS8.md` marks it INFERRED, and nothing here
  rests on it.) Ours echoes the id it was given. Both keep existence from leaking, which is the
  property that matters; ours does it without echoing a value it did not receive, and without
  owing a decoder. Registered.
- The reference's "required with a multi-workspace API key" arm is **unreachable**, because
  decision 5 mints no such credential. Registered as out of scope rather than unimplemented.

**On the work lane this one rule is a deliberate divergence, and §5's recording is why we know.**
The reference ignores `anthropic-workspace-id` there entirely — foreign, own and malformed
values all answer 200 — and stamps neither tenancy header on the response (§4.3 item 2). We
apply the same rule we apply everywhere: an environment key resolves a scope from its
environment's row, so a foreign value is a 404 and both headers are stamped. **Two divergences,
both chosen rather than overlooked**, and slice 1 registers them together:

- **One rule beats five.** A header that narrows on four lanes and is silently ignored on the
  fifth is the shape a reader gets wrong, and getting it wrong on the work lane means a BYOC
  worker quietly polling the wrong tenant's queue. The rule is uniform because a scoping rule
  with an exception is the kind that leaks.
- **The credential *does* resolve a workspace here**, unlike the reference's, whose token
  grants one environment and three verbs and names no tenant at all (§4.3 item 2). So we have a
  workspace to stamp and a workspace to compare against; the reference does not, which is a
  sufficient explanation for its ignoring the header and stamping nothing, and not a reason for
  us to.

The cost is bounded and worth naming: a client that sends a *foreign* workspace header to a
work route gets 404 here and 200 there. The `ant` CLI can produce that request — it forwards
whatever `--workspace-id` it is given, on the worker lane as on the management one
(`anthropic-cli` v1.26.1 `pkg/cmd/workspace_test.go:18-74`) — but only when its operator names
a workspace other than the one the environment belongs to, which is precisely the request this
plan exists to refuse. A worker polling its own environment is unaffected whether it sends the
header or omits it.

Both response headers are stamped **inside each credential resolver** — `requireAPIKey`
(`internal/api/auth.go:181`), `resolveEnvironmentKey` (`envauth.go:44`), `requireWorkToken`
(`worktokenauth.go:99`), `requireGateToken` (`gateauth.go:21`) and `requireIdentity`
(`identitylane.go:61`) — immediately after the scope resolves and before the handler or an
authenticated rejection writes, so both are present on a 200 and on an authenticated 4xx and
absent on a pre-auth 401. The environment lane is stamped at the **shared resolver**, not at
`requireEnvironmentKey` (`envauth.go:67`), because a second middleware calls the same function:
`requireEnvironmentKeyForSession` (`:88`) is the worker half of the dual-auth session routes
(`dispatchSessionEventsAuth`, `server.go:460-461` — the events list and append, the SSE stream
and the bare session read). Stamping the middleware rather than the resolver would leave those
worker responses bare and make the sentence above false on exactly the routes a BYOC deployment
uses most. They cannot ride the `request-id` stamp:
`withRequestID` (`internal/api/server.go:699`, the header at `:702`) runs **outermost** — the
wrapper order is `withRequestID(withTracing(dispatchAuth(…)))` (`server.go:283`) — so it
executes before any lane resolves the scope, which each lane attaches only to the context it
passes to its `next` (e.g. `auth.go:207`); `withRequestID` keeps its request-id stamp alone. For
`anthropic-workspace-id` that schedule is the documented one; for
`anthropic-organization-id` no absence rule is *documented* anywhere, and the 2026-09-05
recording shows the reference does not run one schedule at all — whether a route stamps comes
first, and only within a stamping route does how far the request got decide which headers
appear (§4.3 item 5). **Our single schedule is therefore a deliberate simplification**, and a
strictly wider one: every authenticated response of every route carries both headers, so
wherever the reference stamps them we do too, and we stamp on responses where it does not — a
body-parse failure, every `/v1/environments` call, the work-poll lane, and the console dialect's
organization-level routes, where the reference stamps the organization alone. A client that
reads the headers when present is unaffected; one that infers *anything* from their absence
would be reading a rule the reference does not publish. Registered (§4.3 item 5).

### 6.3 The predicate, and where it goes

Two helpers, written once in `internal/api`:

- `scopeClause(alias string, argOffset int) string` → ` AND org_id = $N AND workspace_id =
  $N+1 AND project_id = $N+2`, with `alias` supplying a table qualifier for the JOIN cases.
- `scopeArgs(ctx) []any` → the three values, in that order, from `scopeFrom`.

List handlers append at their literal `WHERE true` base (§4.4). Single-row reads and updates
place the clause **before** any `FOR UPDATE`/`FOR SHARE` suffix. Child tables are scoped
through the predicate on their already-present parent id or an existing join — the rule
`internal/store/store.go:37-38` and `0001_init.sql:6-10` already state, tightened by §6.5's
rule (b) so it is a rule rather than a loophole.

Two carve-outs live inside the predicate itself:

- **Skills:** `(source = 'anthropic' OR <scope triple>)`. Catalog rows are platform-global *by
  construction* — `internal/api/skillsimport.go:102-104` inserts them with `id = name` (the
  bundle's own name, `:91`) and `source = 'anthropic'` at boot — and the schema's one
  tenant-keyed index exempted them the same way until `0033_skills_display_name.sql:28` dropped
  it along with the uniqueness itself (`0007_skills.sql:24-26`, §7.5). **The
  carve-out cannot be written on `skill_versions`**, which carries no scope columns at all
  (`0007:32-45`, and `:30-31` says why: "Scope columns are inherited from the parent skill
  row"), so the three version routes take a *parent* check instead (§7.5).
- **Credential authentication:** by hash, where the scope is *derived*. Filtering on it would
  be circular. Five statements, not four: `internal/api/auth.go:167-171` (`authenticate`),
  `internal/api/envauth.go:21-24`, `internal/worktoken/worktoken.go:102-112`,
  `internal/gatetoken/gatetoken.go:103-107`, and `internal/api/auth.go:115-117` —
  `EnsureAPIKey`'s adoption probe, `SELECT created_by, status FROM api_keys WHERE key_hash =
  $1`, which sits twelve lines above the archive-by-name §7.5 does fix. This class is why the
  guard is a scanner with an exemption list and not a grep.

### 6.4 The write floor

A new migration adds a **composite** `FOREIGN KEY (org_id, workspace_id) REFERENCES workspaces
(org_id, id)` to the **nine root tables** — the `UNIQUE (org_id, id)` such a key requires is
declared in the `workspaces` table itself, in slice 1's `0034_workspaces.sql` (§7.1), because a
merged migration cannot be edited to add it afterwards: `agents`, `environments`, `sessions`, `api_keys`, `skills`, `files`,
`vaults`, `memory_stores`, `deployments`. Composite, not `workspace_id` alone: a single-column
key would leave `org_id` on rows looking authoritative while nothing validated it — the exact
defect §8-D2 rejects the single-column *predicate* for — and it is what makes decision 2's
"activating org later is additive" true rather than hopeful. One extra column, same migration,
same lock.

**Validation is made safe rather than assumed.** Nothing in the schema, the code or the tests
establishes that every existing row holds `'default'`: the columns are `text NOT NULL DEFAULT
'default'` with no CHECK, and the pin that looks like proof reads one row from three of the
fourteen tables (`internal/store/store_test.go:636-651`, the loop over `{"agents",
"environments", "sessions"}` at `:641`). Because `Migrate` runs every pending file in one
transaction at every binary's startup (`internal/store/migrate.go:27-33`), a single
non-default value anywhere in the nine tables would make the deployment unbootable with no
repair path. So the migration **normalizes before it constrains**: an
`UPDATE <table> SET org_id = 'default', workspace_id = 'default', project_id = 'default'
WHERE (org_id, workspace_id, project_id) <> ('default','default','default')` per table, with
the upgrade note `0013:21-25` models ("the rows this revokes are credentials that were
authenticating until now"), then the keys. A `SELECT count(*) … WHERE org_id <> 'default'`
assertion per table in the migration test makes the claim true rather than assumed.

Deliberately excluded from the floor, with reasons:

- **`environment_keys`** and **`principals`** never stamp scope at all (§6.1, §6.2), so a key
  on their columns would assert nothing.
- **`events`**, **`work_items`** and **`session_threads`**. Two of the three copy from a
  `sessions` row whose own composite key already validated it, and the third —
  `session_threads` — has a `sessions` foreign key. But **the reason is weaker than it looks
  for `events`**, and the plan says so: `AppendInTx` reads the session row under `FOR UPDATE`
  (`internal/events/log.go:171-173`) and then binds values into a `strings.Builder` multi-row
  INSERT (`:203`) — a Go-side copy, where `internal/brain/delegate.go:362-367` is a genuine
  `INSERT … SELECT`. The sessions key validates the *session's* workspace, not that the Go code
  bound the right value into an events row, which is precisely §9's named residual left with no
  database check on the highest-volume table. It is excluded anyway, on cost: an
  `ADD CONSTRAINT … FOREIGN KEY` validation scan over a populated `events` table would hold its
  lock for the whole single-transaction migration. Slice 2 answers it with the guard's
  `INSERT … SELECT` rule (§6.5 rule (c)) and a propagation test instead, and the trade is
  recorded rather than presented as covered.

**The lock cost, named correctly.** `ALTER TABLE … ADD CONSTRAINT … FOREIGN KEY` takes
`SHARE ROW EXCLUSIVE` on both the referencing and the referenced table — not `ACCESS
EXCLUSIVE` — plus a validation scan; that still blocks writers, so the "brief write pause on a
populated `sessions` or `api_keys` table" conclusion stands. The in-repo precedent for shutting
writers out inside a single-transaction migrator is `0013:19`'s `LOCK TABLE api_keys,
environment_keys IN SHARE MODE`, argued at `:11-18` — **not** `0025:57-61`, which warns about
`CREATE INDEX`'s `SHARE` lock on `events`, a different statement with a different lock.

### 6.5 The completeness guard

No request-driven test can prove that 211 statements all filter. The repo already answers this
question, in the same package, for the same class of problem:
`internal/api/rolematrix_test.go:18-21` parses `server.go` with `go/parser` and states the
reasoning verbatim — "a request-driven test can only check the routes someone remembered to
list. This reads the registrations from the source instead, which is why a route added tomorrow
without a role fails here rather than silently inheriting slice 2's deny-everyone floor." A
scope predicate has exactly that shape. So `internal/api/scopematrix_test.go`:

1. **Walks every non-test Go file in `internal/api`, `internal/events`, `internal/queue`,
   `internal/executor`, `internal/brain` and `internal/vaultresolve` from its first landing** —
   the target set **from slice 2**, six packages. `internal/vaultresolve` is in it because its
   **four** statements would otherwise sit unclassified for four slices: the two `vaults` joins
   (`credentials.go:139`, `mcp.go:85`) and the two keyed on a credential row's own id
   (`mcprefresh.go:222`, `:418`). The rules divide them by *literal*, not by statement, which is
   how the two units come apart here: all four name `vault_credentials`, an unscoped child, so
   four literals ride rule (g); the first two additionally name `vaults` after `JOIN`, and those
   two literals — the package's whole scoped-table mention count of 2 (§4.5) — ride rule (b′).
   Reading the smaller number as the package's statement count is what hid `mcprefresh.go`'s
   two from every rule at once. Everything not yet scoped goes on the exemption list **with
   the slice that fixes it named**, so the guard passes between slices. Slice 5 widens the walk
   once, to every non-test file under `internal/` (§7.5) — which is what brings `internal/store`
   into view, and why the two no-FK writers on the exemption list below sit outside the six
   until then. That widening is a named step in a slice, which is the whole of the rule: **no
   package is added by memory**, and none is added silently.
2. **Derives the scoped-table set from `internal/store/migrations/*.sql`**, not from a
   hand-written list — the mechanism `internal/domain/docs_test.go` uses for the prefix set,
   and for the same reason (that kind of list has drifted twice). A bare `org_id` grep will not
   do: it returns 19 lines of which 5 are prose, an index and a backfill (§4.4) — 20 once slice
   1's `0034` lands, its `org_id` the one declaration the three-column rules read past. So the
   derivation is two rules, both stated in the test, and **both keyed on all three columns, not
   on `org_id` alone**: **(i)** a `CREATE TABLE <name> ( … )` block whose body declares
   `org_id`, `workspace_id` *and* `project_id`, and **(ii)** an `ALTER TABLE <name> ADD COLUMN`
   set declaring the same three — a new scoped table arrives as (i) in a new migration, and
   because a merged migration is immutable an *existing* table gaining scope columns can arrive
   only as (ii); both shapes must enrol automatically, which is the property this step promises.
   All three, because slice 1's own `workspaces` table declares `org_id` and nothing else
   (§7.1): the registry is the referent of every predicate, not a referrer, and an `org_id`-only
   rule would enrol it into a check it can never satisfy. Rule
   (ii) has **no instance in the merged tree** — all 14 declarations sit inside `CREATE TABLE`
   bodies and no migration carries an `ADD COLUMN` of these columns — so it is proved by a
   synthetic fixture rather than by the tree (§7.2). The same walk derives a **second set**: a
   table declaring none of the three while carrying a `REFERENCES` to one that does is an
   **unscoped child**. This is a derivation too, not a shortlist, and it returns **eleven**
   today: `memories` and `memory_versions` (`0029_memories.sql:8`, `:35`), `skill_versions`
   (`0007_skills.sql:32`), `deployment_runs` (`0031_deployments.sql:127`), `agent_versions`
   (`0001_init.sql:30`), `worker_polls` (`0006_worker_polls.sql:15`), `vault_credentials`
   (`0011_vaults.sql:20`), `session_gate_tokens` (`0012_session_gate_tokens.sql:12`),
   `session_resource_credentials` (`0020_session_resource_credentials.sql:14`), `mcp_catalogs`
   (`0023_mcp_catalogs.sql:35`) and `work_session_tokens` (`0030_work_session_tokens.sql:16`).
   No predicate rule can reach any of them, because they hold no column to predicate on. What
   keeps their rows from crossing a workspace is the parent's read, and rule (g) below
   **checks** that rather than trusting it — for all eleven, from slice 2, with the statements a
   later slice fixes riding the exemption list until it does. Naming a subset here and leaving
   the rest to inheritance would make step 2's promise — that both sets are derived, not
   remembered — false for the second one, and would drop the member carrying the worst blast
   radius: `vault_credentials`, which is why §7.5 enumerates its `internal/api` statements by
   hand instead of leaving them to slice 4's bulk (its four in `internal/vaultresolve` are named
   in step 1 above). **And sized, because eleven members reach much further than four did**: the
   eleven are named **92** times across the six packages — 67 in `internal/api`, 14 in
   `internal/executor`, 4 each in `internal/brain` and `internal/vaultresolve`, 3 in
   `internal/queue`, none in `internal/events`. *Counting rule: §4.5's mention rule with these
   eleven names substituted for the fourteen.* Outside `internal/api` they ride rule (g)'s
   stand-in arm. Inside it, (g) asks each for its parent's scoped read in the same function, and
   **which ones already have it is what the guard's first run against the current tree reports**,
   not what this plan lists by hand. Reading found **seven** in-api routes that fail (g) today,
   and they were found in two different places: `getMemoryVersion` and `getMemory` below, which
   §7.4 fixes in slice 4, and the five §7.5 already enumerates for slice 5 — `getSkillVersion`
   and `downloadSkillVersion`, which run no `skills` probe at all, and `getVaultCredential`,
   `updateVaultCredential` and `validateVaultCredential`, which read `vault_credentials` with
   no `vaults` read in the function. Seven found by reading, spread over two sections, is exactly
   the job the mechanism exists to take over. Two members have **no** statement
   in the six packages at all: `session_gate_tokens` and `work_session_tokens` live in
   `internal/gatetoken` and `internal/worktoken`, which the walk reaches only when slice 5 widens
   it to every non-test file under `internal/`.
3. For every SQL literal naming a scoped table after `FROM` / `JOIN` / `INSERT INTO` /
   `UPDATE` / `DELETE FROM`, requires one of:
   - **(a)** the three-column predicate;
   - **(b)** a read constrained by a scoped parent's id column **where that same statement, or
     a scoped read of that parent in the same function, puts the parent under scope**. Bare
     parent-id inheritance is *not* enough, and this is the tightening the current tree needs:
     `internal/api/threads.go:181` (`loadThread`, `:180`) joins `sessions` and constrains
     `t.session_id = $1` while selecting only `s.resolved_agent` from `s` (`threadColumns`,
     `:61-62`) and predicating nothing on it — so the rule is keyed on the **absence of a scope
     predicate on the parent alias**, never on the projection. Those reads are satisfied *in the
     same function*: they join `sessions` and take the scope predicate on that alias in the
     statement itself (slice 4). The skill-version reads (`internal/api/skills.go:641`, `:711`,
     `:868`) carry `skill_id` with no scope term and look like the same class, but they read
     `skill_versions`, which declares none of the three — an **unscoped child** (step 2), so it
     leaves rule (b) for rule (g), which asks the same thing of it: the `SELECT … FROM skills`
     parent probe each route runs, scoped, in the same function. `skills.go:634` for
     `listSkillVersions` and `:750` for `deleteSkillVersion` have one; `getSkillVersion` and
     `downloadSkillVersion` have none at all, which is why they are two of the seven routes (g)
     fails today and why slice 5 adds the same-shape probe to both. **The check is `*ast.FuncDecl`-local,
     not file-local**: the guard walks each function and requires the scoped parent read to be
     present in that same body. A file-local check let a compliant statement in one function
     mask an unsafe statement of the same shape in another — the false negative this rule
     closes. A rule the guard cannot evaluate statically is a comment, not a rule, so a
     statement whose parent is resolved in a **different function** — a sibling handler or a
     shared helper, and *a fortiori* a different file — is a rule-(d) exemption naming its
     resolver, never a (b) admission. In the current tree those are exactly the five cross-file
     resolutions (`internal/api/workapi.go:515`, `internal/api/envkeys.go:121`, `:132` and
     `:163`, and `internal/api/threadstate.go:38`), all on the exemption list below;
   - **(b′)** a read in a package with **no request scope** — `internal/brain`,
     `internal/executor` and `internal/vaultresolve` — constrained either **by the scoped
     table's own primary key** or **by the id of a scoped row its caller already resolved under
     scope**: a session id, a thread id, or a vault id. Both shapes, not just the first: the
     by-own-PK half covers `internal/brain/brain.go:500-505`,
     `internal/executor/executor.go:945-950`, `internal/executor/memory.go:87`, `:180`, `:430`,
     `:754`, `:785`, but at least eight statements in those two packages are keyed on
     `session_id` rather than on a primary key — `brain/brain.go:546`, `brain/delegate.go:326`,
     `:446`, `:985`, `brain/repos.go:92`, `executor/mcpwork.go:229`, `executor/packages.go:553`,
     `executor/repos.go:378`. A by-own-PK-only rule admits none of those eight, and no other rule
     reaches them: rule (f) has exactly the right shape and the same justification but names
     `internal/events` alone. So the rule is stated on the **property that makes it safe** — the
     package holds no request scope, and every id reaching it came from a caller who resolved it
     under rule (a) or (b) — rather than on the narrower syntax one sample of it happens to
     have. This rule exists because rules (a)-(c) admit none of them and rule (d) would
     otherwise swallow the ~50 scoped-table mentions those three packages hold as exemptions;
   - **(c)** an INSERT whose column list names all three, or an `INSERT … SELECT` copying them
     from a scoped parent — the `internal/brain/delegate.go:362-367` shape;
   - **(e)** a statement in `internal/queue` constrained on `environment_id`. The work API's
     tenant gate is the environment lane's scope resolution (§6.1), which happens in
     `internal/api` before the queue package is reached, and the **environment row is the
     authority** — `internal/queue` contains no scoped read of `environments`, so rule (b) can
     never admit these. `workAPIScope` (`internal/queue/lifecycle.go:47`, eleven splice sites —
     ten in that file and the `/work/stats` depth read at `internal/queue/stats.go:60`) is
     that constraint's carrier. Without this rule the whole work-API surface — 31 scoped-table
     mentions across `internal/queue`'s 18 statements — falls through to (d), putting the
     exemption list past the length a reviewer reads;
   - **(f)** a statement in `internal/events` constrained on a **session id or thread id** —
     `FROM sessions WHERE id = $1`, or `WHERE session_id = $1` on `events` and
     `session_threads`. Rule (e)'s shape for rule (e)'s reason: the package has no request
     scope, and a session id reaches it only from a caller who resolved it under rule (a) or
     (b′). `internal/events/log.go:171-173`'s `SELECT archived_at, resolved_agent->>'name' FROM
     sessions WHERE id = $1 FOR UPDATE` is the case to classify under it in the slice that lands
     the guard. Without this rule ~49 of `internal/events`' **51** scoped-table mentions across
     its **59** statements (§4.5) fall to (d) — everything but the `INSERT INTO events` that
     rule (c) admits and `outcomes.go:118`'s `FROM files`, which slice 4 gives a predicate;
   - **(d)** an entry on the **exemption list carrying a one-line reason**.

   And for every SQL literal naming an **unscoped child table** — step 2's second set, which no
   predicate rule can reach — requires:
   - **(g)** a scoped read of that child's **parent, in the same function**: rule (b)'s shape,
     applied where there is no column to predicate on. **Where a package holds no request scope
     there is no scope to read the parent under**, so whichever arm admits that package's
     *scoped-table* statements admits its unscoped-child ones too — (b′) in `internal/brain`,
     `internal/executor` and `internal/vaultresolve`, (e) in `internal/queue`, (f) in
     `internal/events`. Stated that way rather than by naming three packages, because the
     second set has eleven members and they do not all live where (b′) reaches: `worker_polls`
     is read and written in `internal/queue` (`stats.go:55`, `:84`, `:88`) under exactly rule
     (e)'s `environment_id` constraint, and a (b′)-only stand-in would leave those three
     admitted by nothing. The (b′) arm has to cover **all four** of
     `internal/vaultresolve`'s `vault_credentials` statements, not just the two that look like
     joins: `credentials.go:139` and `mcp.go:85` join `vaults` and constrain
     `c.vault_id = ANY($1)` on parent ids their caller resolved under scope, and
     `mcprefresh.go:222` and `:418` are keyed on a credential row's own id — an id those first
     two resolved. In `internal/api`, where the scope does reach the statement, only the scoped
     parent read will do. Seven in-api routes fail it today (step 2); slice 4 fixes the two here
     and slice 5 the five §7.5 enumerates. `getMemoryVersion` (`internal/api/memoryversions.go:98`) reads
     `memory_versions` with no store probe anywhere in its body, defaulting to full content
     (`viewFull`), so a store id and version id from another workspace would return that
     workspace's memory verbatim. `getMemory` (`memories.go:317`) probes only on the **miss**
     path (`:322`, inside the `ErrNoRows` branch), so a hit answers before the store is ever
     checked. Both were written under a parent obligation the plan stated in prose; this rule is
     the reason a third one cannot be written the same way.
4. **Fails closed on a dynamically composed table name**, and only on that. The stronger rule
   ("any SQL-looking literal it cannot parse") is not available: the **77** fragment-concatenation
   lines §4.4 counts, under the rule stated there, say otherwise — one of them a function
   returning a predicate. The table name after
   `FROM`/`JOIN`/`INSERT INTO`/`UPDATE`/`DELETE FROM` is always a
   literal today — nothing in non-test `internal/` composes one, verified — so this rule is
   satisfiable now and a later refactor that interpolates a table name fails here. The guard's
   own comment says so, and says the corollary: a move toward a query builder defeats the
   scanner outright.
5. Adds one rule the table-based check cannot express: the **jsonb snapshot reference columns**
   on `sessions` and `deployments` (§6.7).

**The exemption list is the deliverable, not a loophole** — a reviewer reads nineteen commented
entries instead of auditing 211 statements. Its permanent members, **each its own entry** except
where this paragraph says otherwise, each with its reason:
`queue.Claim` (`internal/queue/queue.go:300`) and the self_hosted gauge (the literal at
`internal/queue/metrics.go:92`); the **deployment scheduler's background lane** — one entry for
its **six** statements, none of which any rule can admit because the lane holds no credential
and the scope comes *out* of these reads rather than into them: on `deployments`, the due scan
(`internal/api/deploymentscheduler.go:211-218`), the fire's `FOR SHARE` re-read (`:444-451`)
and the failure `UPDATE` (`:534-536`); on `deployment_runs`, whose enrolment as an unscoped
child (§6.5 step 2) is what brings them in at all, the run claim (`:479`) and the two settles
(`:524`, `:546`); the retention sweep
(`internal/api/memoryretention.go:109-118`); the **five** credential resolvers of §6.3; the
boot catalog import (`internal/api/skillsimport.go:103-104`, `:108`, `:135-138` — the three
`skills` statements inside `importSkillDir`, which opens at `:82`); the reaper's by-id classification
reads (`internal/executor/reaper.go:255-266`) and the tombstone read beside them (`:269`); the
two no-FK tables' writers — `store.SessionTombstoneInsertSQL` (`internal/store/store.go:59-62`)
and `internal/executor/checkpoint.go:205-210`; the **five cross-file parent resolutions**
rule (b)'s same-function check cannot admit, each naming where its parent *is* resolved:
`internal/api/workapi.go:515` (`SELECT session_id, kind FROM work_items WHERE id = $1 AND
environment_id = $2` — that environment id is the env-key choke point's own, §6.1);
`internal/api/envkeys.go:121`, `:132` and `:163`, whose environment is gated by
`consoleEnvironment` (`internal/api/consoleapi.go:138`, statement `:145`, which slice 4 gives
the predicate — §7.4); and `internal/api/threadstate.go:38`, whose session is the one
`sendSessionEvents` (`internal/api/events.go:43`) locked at `:77-81` in the same transaction.
An entry is a commented site group as this paragraph groups them — the boot import's three
statements share one, each credential resolver has its own — and the test states that
granularity beside the list, since a pinned length means nothing without it.

**The terminal check is a pinned count, not a minimality claim.** "The list contains nothing
but the platform-wide sweepers" cannot hold against a target set that admits ~50 rule-(b′)
mentions, 31 rule-(e) ones and ~49 rule-(f) ones (mention counts, §4.5) on their own named
rules rather than on the predicate. So slice 6 asserts the list's **exact length and exact
membership** instead — the **nineteen** permanent members named above, and no other: adding an
entry then requires editing a number and a name in a test, which is a deliberate, reviewed edit
rather than a silent one. Between slices 2 and 5 the list is longer by the entries parked with
the slice that removes them, and each of those names that slice, so the count at any slice is
the nineteen plus what the not-yet-landed slices still owe.

**The guard's own TDD comes first.** Before any statement is fixed, the guard runs against the
*current* tree and must report the leaks §4.4 and §7.5 enumerate — the four worker-lane skill
routes, `ListManagementKeys` with no `WHERE` at all (`internal/api/apikeys.go:190` the func,
`:195` the statement), the five unscoped inserts. Without that, the first thing it ever does is
pass, and nobody learns whether it detects the failure mode it was built for. Synthetic
fixtures prove the matcher; the current tree proves the coverage.

**It is a lint, not a proof, and the residual is named.** `workspace_id = $1` bound to the
wrong variable — the environment id, the session id — passes every static check. Only behavior
catches that, which is why §7 prices the coverage at one cross-tenant case per *statement kind*
per resource family, plus a **two-tenant mirror test**: identical resources created in both
workspaces under the same logical names, asserting every route in the family answers only its
own. A wrong-variable binding then produces a visible cross-read.

### 6.6 Rejected: row-level security

Not preference — evidence, and the decisive fact is that **RLS would never be exercised by the
merge gate**. `internal/pgtest/pgtest.go:95` builds every test DSN as
`postgres://postgres:test@127.0.0.1:%s/postgres`, so the entire Postgres-backed suite runs as
the **superuser**, which bypasses row-level security unconditionally and is *not* fixed by
`FORCE ROW LEVEL SECURITY`. `make test` would report green over an enforcement mechanism that
never fired once, and an untested fail-closed mechanism is a belief, not a guarantee — worse
than none, because it invites skipping the predicate on its strength. Making it testable means
changing pgtest's role, which every Postgres suite in the tree depends on.

Three more, each independently sufficient:

- **No place for a tenant setting to live.** The only `SET LOCAL` in the tree is
  `lock_timeout` (`internal/api/deploymentscheduler.go:561`, inside `setDeploymentLockWait`
  at `:560`, whose comment at `:557-559` says it "bounds every deployment-row lock wait **in
  tx**" and that "SET cannot be parameterized"), i.e. the one precedent lives inside an
  explicit transaction. Hot-path statements run on the pool outside any transaction
  (`internal/api/principals.go:37`), and session-level `SET` on a pooled connection leaks
  across requests.
- **The owner is the querier.** All three DB-touching binaries read one `DATABASE_URL`
  (`deploy/compose/docker-compose.yml:23`) and `store.Open` runs `Migrate` on the handlers' own
  pool (`internal/store/store.go:67-81`, the `Migrate` call at `:76`), so the role that owns
  every table is the role that queries them — and an owner bypasses RLS without `FORCE` on all
  14 tables.
- **The background writers carry no request tenant.** Brain, executor, the scheduler and
  `Migrate` itself would force an "unset tenant means all rows" arm, demoting RLS from
  enforcement to decoration; a `BYPASSRLS` background role would contradict
  `deploy/gcp/dbinit.sql:248-249`, which raises `'FAILED: % has BYPASSRLS'` if the platform
  role has it.

RLS stays available as a later defense-in-depth slice with its own role and DSN design, in its
own issue — never as the primary mechanism, and never before pgtest connects as a
non-superuser.

### 6.7 The invariant the guard cannot see: jsonb snapshot references

Every by-id read in `internal/brain`, `internal/executor` and `internal/vaultresolve` is
unscoped **by design** (§6.5 rule (b′)), and there is no name-keyed resolution anywhere in that
lane to fix — so the ~16 create-time cross-reference checks are not a parallel work item, they
are that lane's *entire* read-side gate. But the ids those packages read come from three
**jsonb snapshot columns** on `sessions` — `resolved_agent`, `resources`, `vault_ids` — and
their writers are `UPDATE`/`INSERT` statements that carry a scope predicate on the *row* and
say nothing about the ids inside the payload. The same grep over non-test Go returns an
enumerable set of **five** writers across **two** tables — the sessions columns and
`deployments`' own `resources`, `vault_ids` and `initial_events`, which the scheduler later snapshots into a session:

- `internal/api/sessions.go:732-733` — the create, which writes all three at once;
- `internal/api/sessions.go:1052` — `UPDATE sessions SET resolved_agent = $2, …` (session
  update; `vault_ids` updates are explicitly refused at `:928`);
- `internal/api/sessionresources.go:803` — `UPDATE sessions SET resources = $2, updated_at =
  now() WHERE id = $1`, reached from `addSessionResource` (`:878`) via `addSessionResourceTx`
  (`:892`). A table-based guard passes it while it smuggles a foreign reference;
- `internal/api/deployments.go:301` (the create's column list) and `:622` (the update's SET
  list), which carry `vault_ids`, `resources` **and `initial_events`** on `deployments`. That payload is exactly what
  the deployment scheduler snapshots into a session with **no request credential** (§6.1), and
  the first two are stored unchecked today because both paths call `parseResourceInputs` for shape only
  (`:260`, `:473`). `initial_events` is the third reference and the worst-hidden: a
  `user.define_outcome` event with a `file` rubric embeds a `files` id, and
  `validateDeploymentInitialEvents` **stops checking that id the moment blob storage exists**
  (`internal/api/deploymentwire.go:79`'s early return when `s.blobs != nil`, the create's call at
  `:294`, the update's at `:582`), so a foreign-workspace rubric id is caught only at a fire's
  outcome read (`internal/events/outcomes.go:118`, `SELECT size_bytes FROM files WHERE id = $1`),
  never at create — the create-time check §7.4 adds.

So the plan states the invariant and gives it a mechanism:

> **Every writer of a snapshot reference column re-runs the same-tenant check on the ids it
> writes.**

All reference-id validation funnels through one `requireSameTenantIDs` helper, called at the
two session materialization points — `materializeResourceInputs`
(`internal/api/sessionresources.go:653`, called from `internal/api/sessions.go:694`) and
`addSessionResource` — at the two deployment parse points, and beside
`validateDeploymentInitialEvents` for the file-rubric id, which is §7.4's new
"deployment resources→file and memory store, and initial-events rubric→file" check. The guard
asserts that every SQL literal writing one of these columns, on **either** table, is reached
**only through a call path that runs the helper** — the writer's own function calls
`requireSameTenantIDs`, or every path that reaches it does — **never** merely that some function
in the same file calls it somewhere, which would let an unchecked writer ride beside a checked
one. The check is function/call-path-local, not file-local.

### 6.8 The credential paths that need their own attention

Three of them, none reachable by a predicate sweep.

**`EnsureAPIKey`'s upsert is a cross-workspace credential move.** `key_hash` is globally
`UNIQUE` (`0001_init.sql:145`) and the boot upsert is
`INSERT INTO api_keys … ON CONFLICT (key_hash) DO UPDATE SET status = 'active', name =
EXCLUDED.name, partial_key_hint = EXCLUDED.partial_key_hint, created_by = NULL, expires_at =
NULL` (`internal/api/auth.go:146-151`, with the adoption argument at `:104-112` and `:135-145`).
After tenancy, workspace B's operator configuring a value that already exists as workspace A's
key takes the `ON CONFLICT (key_hash)` arm, which cannot fail: it rebinds name, status and
expiry and leaves `workspace_id` untouched, so a B-configured bootstrap key authenticates into
A — silently. Slice 1 resolves it explicitly: **the `DO UPDATE` set gains `workspace_id`**, so
the row moves to B, and the log line at `:122-123` — which already warns when adopting a
console-issued key — gains the previous workspace when it differs. **The conflict target stays
`(key_hash)`, and the global `UNIQUE` stays with it**, deliberately: one secret must resolve to
exactly one workspace (§6.2), so two workspaces naming the same key value is a move, not a
coexistence. Re-targeting the conflict on `(org_id, workspace_id, key_hash)` is the tempting
alternative and the wrong one — with the global `UNIQUE` still in place B's insert would raise
a uniqueness violation instead of conflicting, failing the boot and turning a misconfiguration
into an existence oracle for A's key; and dropping that `UNIQUE` to avoid the violation would
let one secret authenticate into two workspaces, which is the premise this plan rests on. A
same-workspace re-configuration is unchanged; a cross-workspace one is a loud adoption, and the
test asserts both.

**The bootstrap marker must reach the request, and it is writable.** Slice 6's authority rule
distinguishes the env-var-managed key by `created_by IS NULL` — the same predicate `0024:77-78`
keys its one-live index on. But `authenticate` returns only the row id today, so slice 1's
"selects the triple beside `id`" must **also select `created_by`** and put a boolean
`bootstrapKey` on the context beside the scope; nothing else can reach it. And the marker is
not immutable: `EnsureAPIKey`'s adoption path sets `created_by = NULL` on any previously
console-issued key whose value is configured (the clause at `auth.go:150`), so "a console-issued key cannot
administer workspaces" is enforced by a column `CONTROLPLANE_API_KEY` can clear. That is
stated as the bound it is — whoever can set that variable already has deployment access — not
papered over, and it is why slice 6 pairs the marker with an SSO `admin` rather than resting on
it alone.

**`EnsureAPIKey`'s archive-by-name needs the predicate.** `UPDATE api_keys SET status =
'archived' WHERE name = $1 AND key_hash <> $2 AND status = 'active' AND created_by IS NULL`
(`auth.go:129-132`) — without a workspace term, a bootstrap rotation in one workspace archives
another's key of the same name. **This one lands in slice 5, not slice 1**, because its test
cannot be written before then: the fixture needs two live env-var-managed keys of the same name
in different workspaces, and `0024:77-78`'s `api_keys_one_live_unissued` is keyed on `name`
alone until slice 5's `0036` rescopes it, so the second key cannot be inserted at all. The other
two corrections above are testable on slice 1's schema and land there.

### 6.9 What archiving a workspace does, and does not do

Defined explicitly, because an `archived_at` column with locally-tested effects is how a
half-defined cascade ships:

- **The `default` workspace cannot be archived at all** (§7.6): the archive handler refuses
  `workspace_id == "default"` before any mutation, so the seeded workspace — and the bootstrap
  key scoped to it (§6.8) — can never be locked out. Everything below describes archiving a
  *non-default* workspace.
- **Credentials stop working**: any credential resolving to the workspace fails the scope
  resolution (§6.1) — the reference's documented behavior, and **observed 2026-09-05** on the
  key lane, down to the message an unknown key gets (`batch9.curl.txt:150`).
- **The workspace stays listed under `?include_archived=true`** and drops out of the plain
  listing (§7.6) — the reference's own listing behavior, and the reason the row survives.
- **Deployments pause** with `workspace_archived_error`; a run attempted in an archived
  workspace fails with the run-error twin. `0031_deployments.sql:98`, `:105`, `:176`, `:185`'s
  CHECK lists already admit both values, so no migration is needed — exactly as
  `docs/DIVERGENCES.md:153` predicted. The run error's `message` wording is **ours** and is
  registered **CONFIRMED** — a deliberate divergence rather than an inference, the reference
  giving only "Human-readable error description." for a recording to match (§4.2).
- **In-flight sessions and leased work items are not touched.** Existing machinery — lease
  expiry, the reaper — finishes or reclaims them. No new sweeper.
- **Nothing is deleted.** Archive is not deletion; tenant erasure is out of scope (§3).
- **`organization_disabled_error` stays unemitted**, on a new reason: with one organization per
  deployment there is no disabled-organization state, and inventing an organization lifecycle
  so an error type has a producer inverts the order of the work.

### 6.10 What row scoping does not isolate

Stated once, here, because calling this work "multi-tenant" invites the misreading: the shared
sandbox endpoint, the shared object-storage bucket, the single credential cipher key, the
single global work queue and the upstream prompt cache are all out of scope (§3), each with a
registered non-goal and an issue. Decision 1 makes that a defensible boundary — administrative
separation between teams that trust each other — and it is exactly the boundary that would have
to move if the driver ever became untrusted co-tenants.

## 7. Slices

Each lands one PR with its own `changelog.d/` fragment, its STATE.md movement, and its registry
entries.

### 7.1 Slice 1 — the workspace registry, and scope on the request

**Was gated on §5's recording; taken 2026-09-05, so this slice is clear to start** (§5.1).
**Goal.** Every credential resolves exactly one `domain.Scope`;
the registry holds the default workspace as a *recognised* row rather than a created one; both
response headers are emitted. Behavior changes only in the header surface.

**Changes.** Migration `0034_workspaces.sql` (next free number: the directory ends at
`0033_skills_display_name.sql`) — `workspaces (id text PRIMARY KEY, org_id text NOT NULL
DEFAULT 'default', name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
archived_at timestamptz, UNIQUE (org_id, id))` plus `INSERT … VALUES ('default', 'default',
'Default Workspace')`. Its header states the change and notes that `0001_init.sql:6-10`,
`0013:43-46` and `0022:39-42` are now historical text, because a merged migration is immutable.
· `domain.PrefixWorkspace = "wrkspc"`, joining the private family at
`internal/domain/id.go:42-63` with the same reasoning, **out of `knownPrefixes`** (`:95`) — so
CLAUDE.md's prefix line and `internal/domain/docs_test.go` are untouched. · `ctxKeyScope` +
`scopeFrom` · the five choke points of §6.1 · `internal/domain/session.go:43-49` `Scope`'s doc
rewritten (the `NOTE` at `:47-49` and all three `json:"-"` tags at `:51-53` stay — no scope
field becomes wire-visible) · `IDENTITY_CLAIM_WORKSPACES`, `IDENTITY_WORKSPACE_MAP`, the
fail-closed arm · the header rule (§6.2) · the two scope-derived response headers stamped inside
the five credential resolvers of §6.1 (`requireAPIKey`, `resolveEnvironmentKey` — the shared
function, so both env-key middlewares carry it, `requireWorkToken`, `requireGateToken`,
`requireIdentity`) after scope resolves, with
`withRequestID` (`internal/api/server.go:699`) keeping only its request-id stamp · the **two**
`EnsureAPIKey` corrections of §6.8 this slice can test — the workspace-bearing conflict target
and the `created_by` marker reaching the request; the third, the archive-by-name predicate,
lands in slice 5 beside the migration that makes it testable (§6.8, §7.5) — whose signature
gains the bootstrap key's workspace — `cmd/controlplane/main.go:128` is the only **production**
call site and passes `(ctx, pool, "bootstrap", bootKey)` today, but 24 further call sites live
in six `internal/api` test files and three more outside them (`acceptance/deployments_test.go:26`,
`acceptance/stack_test.go:105`, `evals/stack_test.go:64` — both directories are in the gate's
build set), so the signature change is **28 edits across ten files**, not one — unless a
workspace-defaulting wrapper spares the 27 test sites, which is the cheaper shape and the one
to prefer ·
`internal/pgtest`'s **six** fixture inserts into scoped tables, five of which gain a scope
(`pgtest.go:229` environments, `:253` agents, `:257` sessions, `:261` the primary
`session_threads`, and `:304` the **child** thread in `NewChildThreadWithAgent`, `:300` — the
pair slice 2's propagation fixture asserts on); `:255` inserts `agent_versions`, which has no
scope columns to stamp and inherits through its FK, as `0001_init.sql:9-10` says in as many
words. Scopes default to today's behavior · a second-workspace seam threaded
through `internal/api/apitest_test.go`: the constant at `:22`, `newPoolWithKey` at `:39`, and
above all `newTestServer` at `:60`, which **453** call sites use. *Counting rule:
`grep -rc "newTestServer(t)" internal/api/*_test.go`, summed.*

**Tests.** Per-lane scope derivation, five cases, through a test-only echo route — including
that the environment key tracks its *environment's* row and not `environment_keys`' own
columns. · The three header rejections, message-for-message, on every scope-bearing lane. ·
`--api-key` plus a matching `anthropic-workspace-id` is 200; another workspace answers with the
same body as an unknown one. · Both response headers on a 200 and on an authenticated 4xx; both
absent on a 401 that fails before authentication. · A credential resolving to an archived
workspace is refused. · Identity: one live workspace and no claim → that workspace; an archived
second workspace beside it changes nothing; two **live** workspaces and no claim → 403 naming
the configuration; a claim resolving to nothing → no authority. · A **configured** identity
resolving to **two live workspaces** with no header → 400 `invalid_request_error`; with a header
selecting one of the two → that workspace; with a header selecting a workspace outside the set
(foreign or nonexistent) → the identical 404; with a header selecting an **archived** member →
refused. ·
`EnsureAPIKey`: re-configuring a value that exists in another workspace adopts it loudly and
lands it in the configured workspace; a same-workspace rotation is unchanged. · Migration replay over a populated
database leaves every row at `default` and inserts exactly one workspace. ·
**`TestTenancyColumnsHaveSingleTenantDefaults` (`internal/store/store_test.go:636-651`) is
extended from 3 tables to all 14** — widening the table loop at `:641`, which is an edit to that
test and the only one it takes: its assertion is unchanged, because it is the
upgrade-compatibility pin and the pin that `org_id`/`project_id` stay constant. Enforcement is a
separate suite beside it.

**Docs.** `changelog.d/` · STATE.md (plan enters `in-progress`) · plan frontmatter →
`in-progress` · README.md's development/configuration notes and the `internal/identity` package
doc beside `config.go:26-29` for the two new `IDENTITY_*` names — **not**
`docs/ARCHITECTURE.md:466`, which mentions only `IDENTITY_MODE` and enumerates neither existing
name, so there is no list for them to join; if ARCHITECTURE is to carry them, `:460-465` is
*extended*. · Registry: **rewrite** `:47` — this plan's own PR already recast that entry to
cover **both** halves (no response header; the request header accepted and ignored, with
`server.go:699` cited and `Tracked: #56` naming this slice), so slice 1 rewrites it into the
*delivered* behavior: both response headers emitted wherever a scope resolves, the request
header honored under the narrow-only rule, and its #56 tracker pointer closes.
**What matches the reference is not a divergence and stays out of the divergence sections**: the
two unconditional request-header bodies — 400 on a malformed value, 404 on a workspace that does
not exist — are reproduced verbatim, as is the workspace response header's documented emission
schedule, so both belong in `:47`'s rewritten non-mismatch half, or in the registry's
architecture-and-compatibility notes, which exist for exactly this class
(`docs/DIVERGENCES.md:14-17`, against the CONFIRMED section's own charter at `:37`: "Mismatches
with the managed-agents wire taken by choice"). What is **added** as a divergence is only the
mismatch: `anthropic-organization-id` emitted on the workspace header's schedule, no absence
rule for it being recorded anywhere; the reference's "required with a multi-workspace API key"
arm registered as **unreachable**, decision 5 minting no such credential; and §6.2's fourth arm
as **CONFIRMED** — a real workspace the credential does not cover answering the identical 404,
which §5 item 4 observed directly. **Add** the work lane's two deliberate divergences (§6.2):
we honor `anthropic-workspace-id` and stamp both tenancy headers there, where the reference
ignores the header and stamps neither. **Add**
CONFIRMED that the default workspace keeps the literal id `default` where the reference's
Default Workspace is `wrkspc_`-prefixed, and that this platform additionally accepts the literal
`default` **in the header** — a lenient parse of ours, since `anthropic-sdk-go/config/federation.go:77-86` speaks
only to the token-exchange body (§4.2); rewriting the seeded id would be an `UPDATE` across
every scoped table including `events` for a cosmetic gain. **Add** CONFIRMED (deliberate
divergence, *not* INFERRED) that this platform emits `default` as its organization id, naming
**the bare UUID §5 item 3's recording observed** as the shape it diverges from — no INFERRED
format fallback is needed, that observation having succeeded. **Add** the archived-workspace
refusal (§6.1) as a **note in the non-mismatch half**: §5 item 6 recorded the reference
answering the same 401 `authentication_error`, so this is convergence, not divergence — with
the environment-key half named as the remaining unobserved lane. **Add** the two divergences the
recording itself produced: that our foreign-workspace 404 **echoes the id it was given** where
the reference echoes a decoded UUID (§6.2), and that we **accept the literal `default` in the
header** where the reference answers 400 (§6.2) — the second is the header half of the same
leniency `:19` records for the id, and belongs beside it rather than as its own entry.
**Add** a divergence for the human
lane's claim-plus-header membership: what has no reference counterpart is the *claim mechanism*,
not membership itself — the reference manages it through `POST /v1/organizations/workspaces/{id}/members`
and `ant beta:organization:workspaces:members add|update|remove` (workspaces doc:449-810). And
**add** as INFERRED (tracked #78) that a human identity spanning **more than one live workspace**
with no `anthropic-workspace-id` header is refused **400 `invalid_request_error`** — ours because
the SSO lane has no reference twin, modelled on the reference's multi-workspace-key header rule
(§4.2) and distinct from decision 4's unconfigured-claim 403. **§5's recording did not reach the
model** — minting a multi-workspace key needs a principal id it had no way to obtain (§6.2) — so
this one stays INFERRED with its #78 pointer intact.
**Amend** `:19` rather than adding a second entry for `project_id`: `:19` already records
org/workspace/project as the reserved keys with single-tenant defaults, so "platform-only,
frozen at `default`, no reference counterpart at any level" belongs *there*, and slice 4's
re-argument of the same parenthetical is the other half of one entry's rewrite — one divergence,
one entry, which is the registry's own discipline.

### 7.2 Slice 2 — the guard, and the five stamping inserts it catches

**Goal.** Land the mechanism that makes every later slice impossible to forget, and fix the
statements it fails on. Behavior-preserving: the values stamped are the ones the defaults
already supplied.

**Changes.** `internal/api/scopematrix_test.go` per §6.5 — full target set, both table-set
derivation rules, rules (a)-(g) including (b′), the table-name fail-closed rule, exemption list
shipped complete. · `internal/events/log.go:203` stamps the triple — the scope source is free,
because `AppendInTx` already holds the session row `FOR UPDATE` two statements earlier at
`:171-173`. · **Both** `work_items` inserts — `internal/queue/queue.go:224-229`
(`EnqueueThread`) and `:258-263` (`EnqueueOutputsHarvest`) — become `INSERT … SELECT` against
`sessions`
rather than growing a scope parameter, decided explicitly because a parameter would touch
callers in brain, executor and api for a value the statement can read itself — **and the return
contract is preserved deliberately**: `created := tag.RowsAffected() == 1` (`:233`) today
distinguishes a fresh enqueue from the `ON CONFLICT … DO NOTHING` dedupe, and a missing session
errors on the `session_id` foreign key (`0001_init.sql:120`). An `INSERT … SELECT` would turn
that error into zero rows, i.e. `created == false`, indistinguishable from the dedupe. So each
rewrite keeps an explicit "the session must exist" arm — the `SELECT` is
`FROM sessions WHERE id = $3` and the caller distinguishes the two zero-row causes — with the
reason in a comment beside it and a test for each cause. · `internal/executor/harvest.go:394-396`
stamps it; the transaction already reads the session at `:310-312`. ·
`internal/api/sessions.go:753`, the primary thread. · `internal/api/deploymentscheduler.go:444-451`
adds the three columns to the fire's existing `FOR SHARE` re-read and passes them into
`createSessionInTx` (`:506`); the manual-run twin at `internal/api/deploymentruns.go:82-85` does
the same. · The scheduler's **other two** background statements are keyed on the
`deployment_id` the fire's re-read resolved at `:444-451`, and the guard treats them
differently. The failure `UPDATE deployments` (`:534-536`) is on a scoped table, so the guard
sees it — and no rule can admit it, because the lane holds no credential and its parent read is
the credential-less re-read itself. It therefore joins that re-read in the scheduler's
background-lane exemption (§6.5). The run claim
`INSERT INTO deployment_runs …` (`deploymentscheduler.go:479`) **cannot** copy the deployment's
scope — no column in `deployment_runs` holds one (`0031_deployments.sql:127`, the
`CREATE TABLE`). That does **not** put it outside the target set: `deployment_runs` is one of
§6.5 step 2's eleven unscoped children, so rule (g) sees every statement naming it and asks
for the parent's scoped read in the same function. The manual-run twin
(`internal/api/deploymentruns.go:103`) passes, on the `FOR SHARE` re-read at `:82-85` that
slice 4 gives the predicate; the scheduler's claim does not, for the same credential-less
reason its `deployments` statements do not, so it **joins the background-lane exemption**
rather than sitting outside the rules. So do the two settle statements the earlier draft
missed entirely — `UPDATE deployment_runs …` at `:524` and `:546`, both keyed on the run id
the claim returned. That is six statements in one entry, not three — three on `deployments` and
three on `deployment_runs` — and it is the same fact
that makes §7.4 give the run list and the run get a join. Each carries the comment naming the
scoped re-read it rides on, so none is silently unaccounted for.
· `internal/store/store.go:34-42` corrected: the inheritance sentence gains its
two exceptions (`deleted_sessions`, `session_checkpoints`), and "does not yet enforce" is left
alone until slice 5 (§7.4).

**Tests.** The guard against the **current** tree reports the known leaks (§6.5). · Synthetic
fixtures: compliant statement, missing predicate, missing insert columns, a parent-id-only
read that must now fail rule (b), a root-table-by-own-PK read in `internal/brain` that must
pass rule (b′) **and a `session_id`-keyed one beside it that must pass it too** — the shape a
by-own-PK-only reading would have refused — **plus an unscoped-child read in
`internal/vaultresolve` keyed on a caller-resolved id, which rule (g)'s no-request-scope arm
must admit while the same read in `internal/api` must fail**, so a too-wide implementation of
either arm is caught and not only a too-narrow one; an `environment_id`-only read in
`internal/queue` that must pass rule (e), a
session-id-constrained read in `internal/events` that must pass rule (f), an `ALTER TABLE … ADD
COLUMN` of all three scope columns that must enrol a fifteenth table under derivation rule
(ii) — the rule with no instance in the merged tree (§6.5) — a `CREATE TABLE` declaring `org_id`
alone, the `workspaces` shape, which must **not** enrol, a dynamically composed table name, **a
same-file masking pair** (one safe statement and one unsafe statement of the same shape in one
file, where the unsafe one must still fail rather than be waved through by the safe one), and **a
rule-(b) cross-function pair** (a scoped parent read in one function and a parent-keyed read of
the same parent in a *different* function of the same file, which must fail because the read is
not in the resolving function — the file-local→same-function tightening of finding-fixed rule
(b)), and **an unscoped-child pair** (a read of a table declaring none of the three but
referencing one that does, once with its parent's scoped read in the same function and once
without, which must pass and fail respectively — rule (g)): seventeen verdicts. · **Two assertions, not
one, because five of the tables the sizing named carry no scope columns at all.** *Stamping* is
asserted on `events`, `work_items`, `session_threads` (primary and child) and harvested `files`
— the four tables that have columns to stamp. *Inheritance* is asserted behaviorally as well as
statically: rule (g) checks that each unscoped child's parent is read under scope, and the test
checks that the rows then land where the guard implies. A
workspace-B credential reads its own `mcp_catalogs` (`0023:35-36`, `session_id` FK, no scope
columns), its own `session_gate_tokens` (`0012:12-14`) and `work_session_tokens`
(`0030:16-19`), and answers absent for A's. `memories`/`memory_versions` leave the
session-create fixture entirely — they hang off `memory_stores` (`0029:8-10`, `:35-37`), not off
a session, so a session create writes neither; they are tested from a memory-store create. · A
scheduled fire and a manual run in workspace B create a session in B, not `default` (via the
existing `SchedulerTick` hook, `internal/api/export_test.go:87`, and the fire hook at `:129-131`).
· `queue.Enqueue` and `queue.EnqueueOutputsHarvest` alike: a dedupe and a missing session are
still distinguishable. · The exemption list
is asserted non-empty and every entry names a statement that still exists.

**Docs.** `changelog.d/` · STATE.md · `internal/store/store.go` package doc. No "now enforced"
claim yet — reads are still unfiltered, and that sentence stays true until slice 5.

### 7.3 Slice 3 — every insert stamps the scope; the write floor

**Goal.** No row can be created outside its creator's workspace, and the database refuses a row
naming a workspace that does not exist in its organization. Must precede the read predicates, or
a newly created resource would be invisible to its own creator.

**Changes.** The remaining `internal/api` inserts into scoped tables: `agents.go:159`,
`environments.go:420`, `sessions.go:732`, `deployments.go:300`, `memorystores.go:124`,
`vaults.go:82`, `files.go:129`, `skills.go:298`, `apikeys.go:135` — `IssueManagementKey`
(`:101`), reached only from the console route, so the *issuer's* scope and the `{workspace}`
segment's are the same value until slice 6 lets them differ (§7.6). ·
`envkeys.go:93` and `principals.go:37-44` deliberately leave their columns at the defaults, with
a comment each so the omission reads as deliberate (§6.1, §6.2). · `skillsimport.go:102-104` is
exempted, not changed. · Migration `0035_workspace_fk.sql`: the normalizing UPDATEs, then the
nine composite foreign keys of §6.4, with the lock name, the lock cost and the exclusion reasons
in its header. · The guard's target set does not grow here — it was complete at slice 2 — but
`internal/api`'s inserts come **off the exemption list**, admitted now by rule (c).

**Tests.** Per scoped table, a create by a second-workspace credential lands a row carrying that
workspace — table-driven over resource families, not one test per statement. · A console-issued
management key carries its issuer's workspace. · An environment key minted for a
second-workspace environment authenticates into that workspace (the derive-don't-copy decision).
· The composite key refuses an insert naming a nonexistent workspace, and one naming a real
workspace under the wrong `org_id`, on three representative tables. · Catalog skills stay at
`default` after a boot import in a multi-workspace database. · Migration `0035` replays over a
populated database, and the per-table `count(*) … WHERE org_id <> 'default'` assertions hold
after the normalizing UPDATEs.

**Docs.** `changelog.d/` · STATE.md.

### 7.4 Slice 4 — the read predicates and the cross-references (the behavior change)

**Goal.** Every by-id lookup, list builder and create-time cross-reference on a scoped table
filters on the request scope, and a resource in another workspace answers exactly as an absent
one does — **every one outside the set §7.5 enumerates and closes**. That residue is not an
oversight: it is the destructive and credential-bearing statements, pulled out of this slice's
bulk deliberately so each is named, and it is why the "does not yet enforce" sentences flip in
slice 5 rather than here.

**Changes.** The list builders, which do not take one uniform edit (§4.4): **eight** take the
predicate in place at their `WHERE true` base; `deploymentruns.go:274` gains a
`JOIN deployments d ON d.id = r.deployment_id` and the predicate on `d`, because
`deployment_runs` carries no scope columns and its `deployment_id` filter is optional (`:276`) —
**and its by-id twin takes the same join**, `getDeploymentRun` (`:245`, statement `:252`,
`SELECT runColumns FROM deployment_runs WHERE id = $1`), which rule (g) does see — step 2
enrols `deployment_runs` among the unscoped children — and which no bulk edit
reaches, because there is no column on it to predicate: a run id alone would otherwise read
across workspaces, and the join is what satisfies (g);
`memories.go:611`'s enforceable point is the parent memory-store read (`:135`, `:165`), its
`WHERE true` sitting on a derived table; and the **eleventh** builder, `listThreads`
(`threads.go:133`), which already joins `sessions s`, takes the scope predicate on that alias
in-statement — the same-function form rule (b) now requires (§6.5) — with its `sessionExists`
gate (`:130`) kept only for the missing-session 404, not for scope. · The by-id lookups, with
the clause before any lock suffix — including the ones a bulk count hides: **the events and SSE
lane's entire scope gate is `sessionExists`** (`internal/api/events.go:1158`, statement `:1160`,
`SELECT 1 FROM sessions WHERE id = $1`), which resolves the 404 for both the events list and the
stream by bare id; `sessionSelfHosted` (`:1171`, statement `:1173-1174`) is the second such read
and the append path's locked read (`:77-81`) the third; and in `threads.go`, `loadThread`
(`:180`, statement `:181`) and `liveChildThreads` (`:450`, statement `:451`, `FOR UPDATE OF t`)
are two more join-based reads that predicate nothing on the joined `sessions` row. The broker itself is sound —
subscriptions are keyed by session id (`internal/events/broker.go:24`) — but the LISTEN channels
are process-wide (`:155`, `internal/events/log.go:28-30`) and the frames channel carries content
fragments (`internal/events/preview.go`), so that one helper is the whole boundary and is named
here rather than counted.

· **The ~16 create-time cross-references**, each asserting the referenced row's scope equals the
caller's rather than only its existence: session→environment (`sessions.go:669`), session→agent
and pinned version (`:289`, `:299-301`), session→vaults (`:414`, via `validateAttachedVaults` at
`:410`, shared with `deployments.go:287` and `:560`), session→roster members (`roster.go:426`),
agent→roster (`roster.go:179`, `:221`), session resource→memory store
(`sessionresources.go:704`), session resource→file (`:768`), deployment→environment
(`deployments.go:774`), deployment→agent and version (`deploymentparse.go:184`, `:204`), skill
version→skill (`skills.go:559`, `:750`), vault credential→vault (`vaultcredentials.go:152`,
`:433`), memory→store (`memories.go:135`, `:165`), thread→session (`threads.go:189`, `:324` —
**not** `:181`, which is `loadThread`'s join-based by-id read, the rule-(b) case §6.5 names),
environment key→environment (`consoleapi.go:204`).

· **The console's environment existence check is a tenant gate too**, and the whole gate for two
routes: `consoleEnvironment` (`internal/api/consoleapi.go:138`, statement `:145`,
`SELECT true FROM environments WHERE id = $1`) is where `listEnvironmentKeys` (`:231`) and
`revokeEnvironmentKey` (`:267`) resolve `{environment_id}`, so without the predicate an admin in
one workspace lists — and revokes — another's worker credentials. It gains the predicate here,
beside `consoleapi.go:204`.

· **Three have no existing site and must be created**: agent spec→skills, because `parseSkills`
is shape-only (`internal/api/wire.go:565-569` the doc, `:570` the func) and `agents.go` has
**zero** `FROM skills` statements, so a foreign-workspace `skill_id` would resolve silently at
runtime in `internal/brain/skills.go:117`, `:128` and `internal/executor/skills.go:193`, `:233`;
deployment resources→file and memory store, stored unchecked because `deployments.go:260`
and `:473` call `parseResourceInputs` (shape only) and never `materializeResourceInputs`, so
validation happens at fire time — this is also the deployment half of §6.7's snapshot
invariant; and **deployment `initial_events`→file**, because a `user.define_outcome` event with a
`file` rubric embeds a `files` id that `validateDeploymentInitialEvents` stops checking once blob
storage exists (`deploymentwire.go:79`'s early return, the create's call `deployments.go:294`,
the update's `:582`), so a foreign-workspace rubric id reaches storage unchecked. Slice 4
extracts the embedded id and asserts its workspace equals the caller's at create *and* update,
the same `requireSameTenantIDs` posture (§6.7) the other snapshot references get, so rejection
happens **before** the row is stored. · **A fourth site exists and needs a predicate, not a new
check**: the outcome file rubric's `SELECT size_bytes FROM files WHERE id = $1 FOR SHARE`
(`internal/events/outcomes.go:118`) — the fire-time half of that same rubric check, kept as
defense in depth — whose comment at `:77-78` already names "the org scope
(v1's single-tenant boundary)" as a check the code does not compute. The scope it compares
against is the session's, not a request's — `internal/events` has no request scope, and
`AppendInTx` holds the session row `FOR UPDATE` in the same transaction (`log.go:171-173`).
Because `internal/events` is in the guard's target set from slice 2, this statement rides slice
2's exemption list with **slice 4 named as its fix**, so the guard passes between slices (§6.5
step 1).

· `requireSameTenantIDs` and the snapshot invariant of §6.7, plus its guard rule. · Refusal
shape: `errNotFound` with the message and status the absent id already gets — no new error type,
nothing distinguishing "another workspace's" from "does not exist". **§5's recording confirmed
this is the reference's own shape** (§4.3 item 1), so no correction is owed here. · `internal/api`'s
reads come **off the exemption list** — again a shrinking list, not a growing target — and rule
(b) starts refusing bare parent-id inheritance. · **The two rule-(g) failures this slice owns**
(§6.5 counts seven in all; the other five are §7.5's, in slice 5): `getMemoryVersion`
(`internal/api/memoryversions.go:98`) gains the scoped `memory_stores` probe it has never had,
and `getMemory`'s probe (`memories.go:322`) moves ahead of its read so a hit is checked as the
miss already is. The probe is the existing `checkMemoryStore`, which slice 4 has just given the
predicate, so neither is a new query shape — only a call the parent obligation always implied.

**Tests.** Per resource family and **per statement kind** (get / list / update / archive /
delete), the cross-workspace answer is byte-identical to the absent-id answer: status, error
type, message, envelope — the deployment-run **get** among them, not only the run list, since
both reach their scope solely through the `deployments` join. · The console environment-key
listing and **revoke** are each refused across workspaces, the gate being `consoleEnvironment`
alone. · A non-default workspace's key still lists the `anthropic`-source catalog through
`GET /v1/skills` (`skills.go:360`): the carve-out is asserted on the management list here, as
slice 5 asserts it on the worker lanes. · Per cross-reference, a create naming a foreign id is
refused with the absent-id answer — including the four new checks; the **cross-workspace
file-rubric** case asserts that a deployment create *and* update naming another workspace's
rubric `files` id is refused **at create, before storage**, not deferred to fire time. · **The
unscoped children answer as absent too**, which no predicate could have given them: reading
A's memory version and A's memory through B's key returns the absent-id answer, and the
version case asserts the **body**, since that route defaults to full content and is the one
place a missing probe would have returned another workspace's text rather than a bare id. · The
**two-tenant mirror test** (§6.5). · A
list in B never returns A's rows at any page boundary or filter combination, and a keyset cursor
minted in A is not a window into A. · The events list, the SSE stream and the append path each
answer 404 for another workspace's session. · The brain/executor by-id reads are exercised end to
end in a two-workspace database, proving the create-time gate closes statements that stay
unscoped by design. · Every existing single-workspace test still passes with a second workspace
populated in the same database.

**Docs.** `changelog.d/` · STATE.md · **the "does not yet enforce" sentences stay for one more
slice** — all seven coordinates: `internal/store/store.go:34` and `:37-38`,
`internal/domain/session.go:44-45`,
`docs/ARCHITECTURE.md:368` (the `store/` row's "reserves the multi-tenant columns it does not
yet enforce") and `:485`'s parenthetical, `CLAUDE.md:36`'s parenthetical and `:85`'s layout
line. They are still true here: §7.5's enumerated by-id reads on `vaults`, `vault_credentials`,
`skills` and `files` are unscoped until that slice closes them, and a doc asserting enforcement
one slice early is the overclaim the verifier's docs rung exists to catch. Slice 5 flips all
seven. The principle halves stay verbatim whenever they flip — scoping is org/workspace/project
and never a user, the adk `AppName`+`UserID` divergence, the `created_by`/`metadata` hooks.
· Registry: re-argue `:19`'s parenthetical; **add an entry** for a
cross-tenant resource read and a cross-tenant create-time reference both answering the absent-id
404 — **CONFIRMED, and a compatibility note rather than a divergence**, because §5 item 1
observed the reference's own answer before slice 1 ever started and **it is a 404 on both
families**, indistinguishable from an absent id (§4.3 item 1). The entry keeps **both** sides of
the evidence that shaped the choice — the reference's existence-hiding precedent ("the
same response as for any unknown workspace", authentication doc) against the only recorded
org/workspace mismatch status, a **400** on `fallback_credit_token` (§4.2), which the recording
now shows does not generalise — and cross-references `docs/DIVERGENCES.md:28`, this tree's one
recorded cross-scope refusal (403 on the work API). No INFERRED residual and no #78 pointer:
the observation succeeded, so an entry naming no open issue is correct here rather than a
`registry-check` finding (`tools/registrycheck/registrycheck.go:421-423` — the rule binds
INFERRED entries only).

### 7.5 Slice 5 — the leaks inheritance does not cover

**Goal.** Close the statements no parent scoping reaches, and complete the guard's target set to
every non-test file under `internal/`.

**Changes — the worker-lane policy reversal.** Four routes are readable today from any
environment key or `wtk_` token, and `internal/api/worktokenauth.go:134-135` is a bare
`case isSkillReadPath(p): // Workspace-global, as for the environment key.` with no check at
all. **The fix is not one predicate repeated four times**, because only one of the four queries
a scoped table:

- `getSkill` (`internal/api/skills.go:321`, statement `:333`, `FROM skills s WHERE s.id = $1`)
  gains `(source = 'anthropic' OR <scope>)` in place.
- `listSkillVersions` (`:620`) — its `skill_versions` query (`:641`) has nowhere to put the
  clause, so the enforceable point is the **parent probe it already runs**,
  `SELECT EXISTS (SELECT 1 FROM skills WHERE id = $1)` at `:634`. That statement is in this
  slice, not slice 4's cross-reference list, because it is the *lane's* gate rather than a
  create-time check.
- `getSkillVersion` (`:692`, statement `:711`) and `downloadSkillVersion` (`:840`, statement
  `:868`) have **no parent check at all** today; each gains one in the same shape as `:634`.
  These two and the three vault-credential routes below — `getVaultCredential`,
  `updateVaultCredential` (two reads) and `validateVaultCredential` — are five of the seven
  routes rule (g) fails against the current tree (§6.5); slice 4 owns the other two.

The in-repo remediation pattern is the adjacent file lane, which solved this and documents the
contrast: admit to the lane, narrow inside the handler, fail closed to 404
(`internal/api/files.go:335-343` the argument, `:344-352` the branch, `:385`
`fileMountedInEnvironment`). **Four doc sites flip in this same PR** or a comment asserts the
pre-change policy beside post-change code: `internal/api/server.go:559-568` (the
`isSkillReadPath` doc, whose `:563-565` states the policy verbatim),
`internal/api/worktokenauth.go:17` and `:134-135`, `internal/api/doc.go:55-56`, and — because
they *contrast* against skills' globality — `internal/api/files.go:339` and
`internal/api/server.go:598`.

**Changes — the key surface.** `ListManagementKeys` has no `WHERE` at all
(`internal/api/apikeys.go:190`, statement `:195`) and gains the predicate. **Two statements the
inherited sizing misses entirely are cross-tenant *writes* or reads by bare id**:
`updateManagementKey` (`internal/api/apikeys.go:157`, statement `:159-161`, `UPDATE api_keys SET
status = coalesce($2, status), name = coalesce($3, name) WHERE id = $1`) and the **console
patch**'s locked read (`internal/api/consoleapikeys.go:239`,
`FROM api_keys WHERE id = $1 FOR UPDATE`, inside `updateAPIKey` at `:193` — there is no console
*revoke* route: `server.go:192-194` registers POST and GET on the collection and POST on the
item only, so revocation is a `status` patch through this same handler) —
without them a workspace-bound key can rename or archive another workspace's management key by
id. · Migration `0036_api_keys_one_live_scoped.sql`: `LOCK TABLE api_keys IN SHARE MODE; DROP
INDEX IF EXISTS api_keys_one_live_unissued; CREATE UNIQUE INDEX api_keys_one_live_unissued ON
api_keys (org_id, workspace_id, project_id, name) WHERE status = 'active' AND created_by IS
NULL;` — 0024's index under 0024's predicate (§4.5). Widening a unique key cannot fail on
existing data, so unlike `0013` there is no duplicate-collapse repair; `0013:19` is the precedent
for shutting writers out, and `CREATE INDEX CONCURRENTLY` is impossible in a single-transaction
migrator (`internal/store/store.go:16-19`).

**Changes — the remaining by-id reads and, deliberately named, the destructive statements.**
§3 prices a predicate miss on the blob paths in *destroyed objects* and a miss on
`vault_credentials` in *disclosed credentials*, so the statements that carry that blast radius
are enumerated here rather than left to slice 4's bulk:

- **Vault credential reads by bare id, with `vault_id` compared in Go afterwards** —
  `internal/api/vaultcredentials.go:238` (comparison at `:241`), `:285`, `:317` (comparison at
  `:320`), and `internal/api/vaultvalidate.go:106`. Unlike memories, which carry
  `memory_store_id` in every `WHERE`, these four resolve the row first and check the parent
  second. Each gains a **`vaults` join carrying the scope predicate**, not a bare `vault_id`
  term: `vault_credentials` declares none of the three columns, so it is an unscoped child and
  rule (g) asks it for the parent's *scoped* read — the same thing rule (b) was tightened to
  require of a scoped table, for the same reason.
- **A vault credential *write* with no `vault_id` term at all** —
  `internal/api/vaultvalidate.go:171-173`, `UPDATE vault_credentials SET auth = $2,
  secret_ciphertext = $3, secret_key_id = $4, updated_at = now() WHERE id = $1 AND archived_at
  IS NULL AND secret_ciphertext = $5`. This belongs in the write class, not with the four reads,
  and it is the strongest single argument for **rule (g)**: an unscoped child written with no
  parent term at all, which no predicate rule could ever have caught.
- **The destructive and cascade statements**, each of which either deletes an object or a
  credential: `internal/api/files.go:290` (`DELETE FROM files WHERE id = $1`, whose orphan
  object delete follows at `:300`); `internal/api/skills.go:464` (`DELETE FROM skills`) and the
  version cascade `:456` beside it that plan 39 added (object deletes at `:478`), `:778`
  (`DELETE FROM skill_versions`, object delete at `:801`), `:602` and `:787` (`UPDATE skills SET
  latest_version …`), `:439` and `:495` (source reads by bare id feeding a delete and a create);
  `internal/api/vaults.go:324` (`DELETE FROM vaults`), `:185`, `:284` (`UPDATE vaults`), `:297`
  (`UPDATE vault_credentials`, the archive cascade); `internal/api/vaultcredentials.go:360`,
  `:511` (`UPDATE vault_credentials`) and `:540` (`DELETE … WHERE id = $1 AND vault_id = $2`,
  the one that already carries its parent).
- `internal/api/files.go:324-326`, the `SELECT filename, mime_type, downloadable FROM files
  WHERE id = $1` that the download handler runs before either lane branch, gains the `files`
  predicate. The **branch structure at `:344-352` is unchanged** — it is a Go-side lane test
  with no SQL in it — and the environment-key arm's `fileMountedInEnvironment` isolation
  (`:344-348`, `:385`) is left exactly as it is.
- `internal/api/auth.go:129-132`, `EnsureAPIKey`'s archive-by-name (§6.8).
- Guard target: every non-test file under `internal/`. `vault_credentials` needs no rule of its
  own — it is one of §6.5's eleven unscoped children and rule (g) has covered it since slice 2;
  what happens here is that `internal/api`'s twelve statements on it come **off the exemption
  list**: the nine this section names by hand, plus `vaultcredentials.go:164`, `:175` and
  `:440`, which already constrain `vault_id` and need only the scoped `vaults` read slice 4
  gives their handlers.

**Tests.** **The failing test comes first, and it does not exist today in either direction**:
`internal/api/skillsauth_test.go:14` (`TestSkillReadsEnvironmentKeyLane`) issues its environment
key inside the same fixture environment that created the skill (`_, envID := fixture(t, s)` at
`:16`; `wkey := issueKey(t, s.pool, envID, "skills-lane")` at `:17`), so it would pass unchanged
under scoping, and `internal/api/worktoken_test.go:194-197` does the same for the token. Write
"workspace B's environment key reading workspace A's custom skill answers 404" and its `wtk_`
twin, across all four routes including `/content`, before touching the lane. · An
`anthropic`-source skill stays readable from every workspace's key and token — the carve-out
asserted positively, or the reversal breaks every sandbox it was built for. · **No
`display_name` test is owed**: plan 39 dropped the uniqueness this slice would otherwise have
made per-workspace (`0033_skills_display_name.sql:28`), so there is nothing left to scope. ·
A management key lists, patches and revokes only its own workspace's keys. ·
Two workspaces may each hold a live env-var-managed key named `bootstrap`; rotating one leaves
the other active. · A vault credential read, a credential update, a vault delete, a skill
delete and a file download are each refused across workspaces with the absent-id answer, and the
corresponding blob object survives. · Migration `0036` replays over a database holding keys in
several workspaces. · **`TestKeyRotationMigrationRepairsExistingDuplicates`' rewind list
(`internal/store/store_test.go:533-545`) must gain `0036`** — it drops
`api_keys_one_live_unissued` *by name* (`:534`) and deletes 0013/0021/0024 from
`schema_migrations` (`:541-544`) before replaying, so without the addition its replay silently
ends on a schema no deployment reaches.

**Docs.** `changelog.d/` · STATE.md · **the "does not yet enforce" claims flip here**, where they
finally become false — seven coordinates across four files: `internal/store/store.go:34` and
`:37-38`, `internal/domain/session.go:44-45`, `docs/ARCHITECTURE.md:368` and `:485`,
`CLAUDE.md:36` and `:85` (§7.4 says why they wait for this slice). · Registry: **rewrite the two entries carrying the clause
"scopes nothing per environment (skills are workspace-global)"** — `:163`, which #575's
2026-09-03 recording promoted from INFERRED to CONFIRMED, and `:64`, plan 39's twin from the
2026-09-04 recording — to the per-workspace rule plus the `anthropic`-source carve-out. **Both
stay CONFIRMED, and the draft's reason for keeping them INFERRED is gone**: the reference's
server-side auth scope for these routes *is* recorded now — its environment key is an OAuth
token whose named scopes refuse `GET /v1/skills` and `GET /v1/skills/{id}` with 403 while the
whole version subtree answers 200 — so what this slice narrows is the workspace dimension no
recording has touched, and each entry keeps only its own remaining #78 pointer (whether a
management key is admitted on those routes). **`:60` needs nothing at all.** The draft
scheduled a rewrite there for the display field's derivation, cap, uniqueness and name; plan 39
settled all four ahead of this plan — the field is `display_name`, its derivation from the
SKILL.md frontmatter is the reference's own, the 255-character cap now matches
(`internal/api/skillsupload.go:27`, `maxDisplayNameChars`), and `0033_skills_display_name.sql:28`
dropped the uniqueness — and the entry already reads "Converged, no longer a divergence".
**Amend** `:162`, which stays
accurate but whose contrast against "skills (workspace-global shared assets)" no longer holds. ·
`docs/plan/08_files.md:188` and `docs/plan/36_memory-stores.md:629` restate the workspace-global
policy and are archived plans — cited as historical, with the correction recorded in the
registry.

### 7.6 Slice 6 — the operator surface, the acceptance run, close-out

**Goal.** Make a second workspace creatable, prove it with the real `ant` CLI, and close #56.
**Deliberately last**: a half-enforced multi-tenant deployment is worse than a single-tenant
one, so the surface that makes a second workspace possible lands only after every scoped
statement is enforced.

**Changes.** Workspace administration on the console dialect that already carries the segments,
not the `/v1/organizations/workspaces` Admin API §3 records as a non-goal against a known shape:
`POST|GET /api/console/organizations/{org}/workspaces` and a workspace archive **that rejects
`workspace_id == "default"` before any mutation**. **The path shapes are CONFIRMED, 2026-09-05**
(§5 item 5): the reference's own console answers `POST|GET
/api/console/organizations/{org}/workspaces` (`batch9.json` idx 0, `batch8.json` idx 32),
`POST …/workspaces/{ws}/archive` returning the workspace with `archived_at` set (`batch8.json`
idx 31), and `POST …/workspaces/{ws}/api_keys` (`batch9.json` idx 1) — so these two routes are
mirrored in shape as well as in placement, and item 5's contingency (registering them as ours in
shape) does not fire. Three details worth carrying. Its listing **omits archived workspaces** and
takes `?include_archived=true` to include them (`batch9.json` idx 4) — **we mirror that**, and
it is why the archive is a soft one (§6.9). Its key object carries `scope: {"type":
"workspace", "workspace_id": …}` beside a nullable `principal`, the same two discriminators §6.8
reasons about. And its listing **omits the Default Workspace entirely** — absent from the live
listing and from `?include_archived=true` alike, while that workspace is live and serving
requests (`batch8.json` idx 32, 36; `batch9.json` idx 4). **We diverge there and list it**:
under decision 2 this deployment's `default` is not a hidden bootstrap row but the workspace
nearly every operator will only ever have, and a listing that hides the single workspace in use
is a worse surface than a faithful one. Registered. The seeded Default Workspace cannot be
archived, mirroring the reference (it "cannot be renamed, archived, or deleted", workspaces doc,
§4.2); this keeps the bootstrap key's `default` resource scope (§6.8) and every `default`-bound
credential from ever being locked out by an archive (§6.9). The refusal is a **400
`invalid_request_error`** naming the default workspace — INFERRED status, since the reference
documents the prohibition but no code for it (registry below). Registered
`identity.RoleAdmin` in the shape `internal/api/server.go:171-198` already uses for the two
console sections. **The authority rule, stated once:** workspace administration is reachable by
an SSO `admin` (an org-wide role, per plan 31) and by the **env-var-managed bootstrap key** —
distinguished by the `created_by IS NULL` bit slice 1 put on the context (§6.8), the same
predicate `0024:77-78` keys on, with that marker's own writability stated as a bound. A
console-*issued* key cannot, and no credential gains a wider *resource* scope. · `consoleWorkspace`
(`internal/api/consoleapikeys.go:127-135`) resolves `{workspace}` against the registry instead
of comparing to `reservedWorkspace` (`:34`), which is deleted — **and the caller's own scope
constrains that resolution only for a credential without the administration capability**: a
console-issued key may address its own workspace and no other, while the bootstrap key and an
SSO admin may address any registered one. That is the difference between a capability and a
wider resource scope, and it is what makes `apikeys.go:135` stamp the **path's** workspace here
rather than the issuer's (§7.3). · **The bootstrap order, stated because nothing else in the
plan can mint workspace B's first credential.** The bootstrap key's *resource* scope is
`default`, so it cannot be the answer by scope; it is the answer by capability: (1) the
bootstrap key creates B on the console dialect and reads back B's minted `wrkspc_` id; (2) the
same key issues B's first management key at
`POST /api/console/organizations/default/workspaces/<B>/api_keys`, which stamps B; (3) from
there B's own key administers B's resources. The human lane is a **separate, later** step with
its own cliff: `IDENTITY_WORKSPACE_MAP` cannot name B before B exists, so an operator maps the
IdP group to B's id and restarts, and until then decision 4's 403 refuses every human — which
is the order the release note carries. · `consoleOrganization` (`internal/api/consoleapi.go:59-64`) is **unchanged** — `org_id`
is frozen, so its constant is still correct — but its **doc comment at `:53-58` is not**: it
argues the two console surfaces must answer alike "once #56 makes org a real tenancy key and
someone updates one of them", a future decision 2 cancels. That comment is on the doc-edit list;
leaving it would ship a document predicting a future the plan declined. · `renderAPIKey`
(`internal/api/consoleapikeys.go:108`, `WorkspaceID: nil` at `:113`) populates `WorkspaceID` from
the row, discharging the promise at `:48-50` ("so #56 can populate them without a shape change").
`Principal` stays null and is re-argued on principle 5. No `scope` field is invented: decision 5
mints only the reference's workspace-bound class, and the documented field belongs to the Admin
API's `api_keys` resource, which §3 declines (§4.2). ·
`parseScope` (`internal/api/environments.go:302-314`)
keeps refusing `account`, on the new reason of §8-D8; the message at `:311` loses "yet". · The
archive cascade of §6.9, including `workspace_archived_error`. · **OTel `org`/`workspace`
attributes at the same-source instrumentation point, which is two places, not one.** The span and
the `span.*` wire event are opened together in `internal/events/span.go:35`
(`StartModelRequest`) → `:42` (`StartModelRequestOn`), whose attribute list is built at `:44-49`
and today carries `session.id` and `session.thread_id`; the metrics half is
`internal/events/metrics.go:42` (`(m *ModelRequest).recordMetrics`), with the no-drift argument
stated at `:35-37`. `ModelRequest` carries no scope today, so this is scope plumbing into the
brain's request object plus one attribute pair at each end — priced as such, not as a one-liner —
and one more pair on the queue gauges (`internal/queue/metrics.go`, whose self_hosted scan is
the SQL literal at `:92`, under the `Query` call at `:91`), which is what makes
the queue-fairness non-goal diagnosable. **No tenant field is added to any `span.*` event
payload**: no reference field exists, and inventing one would be a wire invention at the very
point principle 3 requires the two views not to drift. · The exemption list's **pinned count and
membership** assertion (§6.5). · Close #56.

**Tests.** Create a workspace, issue a key in it, read back only its resources; a console-issued
key is refused at workspace administration; an archived workspace's credentials stop
authenticating and its deployments pause with `workspace_archived_error` while another
workspace's are untouched. · An attempted archive of the `default` workspace is refused (400
`invalid_request_error`) **before any mutation**, leaving the workspace, the bootstrap key's
authentication and its deployments unchanged. · **One test is redesigned, not three.** Of the three that pin "any
org or workspace other than `default` is 404", **two exercise the organization segment and stay
exactly as they are** — which is a point in decision 2's favour, not a cost:
`internal/api/consoleapikeys_test.go:653-659` aims a real key id at
`/api/console/organizations/other/workspaces/default/api_keys/{id}` under the comment "The real
id under the wrong scope is refused by the scope, not by the id", and
`internal/api/consoleapi_test.go:252` (`TestConsoleKeyRoutesRejectOtherOrganizations`, the org
substitution at `:258`) additionally asserts at `:272-274` that the message *names the
organization* — an assertion a workspace rewrite would break for nothing. The one workspace case
is `internal/api/consoleapikeys_test.go:603`
(`"unknown workspace": "/api/console/organizations/default/workspaces/other/api_keys"`, inside
`TestAPIKeyRoutesRejectUnknownScopesAndIDs` at `:597`); it keeps its meaning and gains a sibling
case for "a **real** workspace the credential does not cover", with the identical 404 shape that
preserves the enumeration-oracle argument `consoleapikeys.go:123-126` makes. ·
`internal/api/consoleapikeys_test.go`: `workspace_id` becomes the row's value in the **create**
response, where `:153-159` asserts it null today; the **listing** test asserts field presence
only (`wantExactFields` at `:252-254`, no value check), so it needs a value assertion added
rather than changed. `principal` and `expires_at` stay asserted null, and both
`wantExactFields` lists keep the recorded field set exactly. · The exemption-list count and
membership assertion. · `make registry-check` green with #56 closed. · **Acceptance with the
real client**, recorded in `docs/HISTORY.md` the way plan 31's run was
(`docs/HISTORY.md:2593`): build `ant` from the read-only checkout
(`go build -o <scratch>/ant ./cmd/ant`), then (a) create an agent in the default workspace with
the bootstrap key; (b) run the bootstrap order above — create workspace B with the bootstrap
key, read B's minted id from the response, and issue B's first key at B's own console path —
which is the step that proves administration is a capability rather than a resource scope; (c)
`ant --api-key <B key> beta:agents list` shows none of the default workspace's agents; (d)
`ant --api-key <bootstrap> --workspace-id <B>` is **404 — pinning *our* narrow-only rule, which
§5 item 4 showed is also the reference's**: a workspace-bound key sending a foreign header gets
a 404 there too (§4.3 item 3), so this step now pins a mirrored rule rather than a chosen one,
and the 200-ignored contingency that hung over it and §6.2's fourth bullet does not fire; (e) `ant --api-key <B key> --workspace-id B` succeeds and `--workspace-id default` is
404; (f) `ant beta:worker poll --environment-id <B env> --environment-key <B env key>
--workspace-id <B>` polls B's queue and reads B's skills but not the default workspace's — the
direct regression test for the leak this plan exists to close. (The pinned CLI's worker poll
requires `--environment-id` beside the key: `anthropic-cli/pkg/cmd/workspace_test.go:64-66`
passes both.)

**Docs.** `changelog.d/` · STATE.md back to none · plan frontmatter → `archived`;
`docs/HISTORY.md` receives the progress summary and the acceptance record · README.md:76 — the
multi-tenant bullet leaves "Deferred past v1 — seams reserved, not implemented" and its
capability joins what runs today · **AGENTS.md needs no tenancy edit**: it carries no tenancy
claim at all across its 12 lines and defers to CLAUDE.md · `docs/self-hosted-security.md` — the
literal `organizations/default` console runbook paths (`:765`, `:773`, `:777`, `:779` for
environment tokens; `:1092`, `:1099`, `:1103`, `:1106` for management keys — three curls each), plus
the now workspace-bounded "anyone holding the management `x-api-key` can mint worker keys"
(`:787`) and "a management key can mint management keys" (`:1085`); a **new requirement
beside `:926`** — one Casdoor organization even under platform multi-tenancy (§9); and a **new
"Reserved seams and tracked gaps" entry** for what row scoping still does not isolate, a section
that today names four gaps (`:1217`, `:1228`, `:1251`, `:1262`) and no tenancy entry at all.
· **The "single-tenant tampering residual" relabelling is two registry entries by that exact
phrase, plus a third entry that carries the same bound in different words.**
`grep -n "single-tenant tampering residual" docs/DIVERGENCES.md` returns `:245` and `:251` only;
`:58` states the identical bound on the identical class — it names "three tampering residuals"
and bounds them with "none reaches past the agent's own single-tenant session" — so it takes the
per-session/same-tenant word too, which is also what its place in the twelve-entry re-argument
set below already requires. The same phrase appears in code at
`internal/api/sessionresources.go:590` and in prose at `docs/self-hosted-security.md:528`.
All five sites get the word; the residual itself is unchanged.

**Registry, in one batch.** **Demote** `:153` to provenance — by slice 6 the *last* live tracker
naming #56 (`grep -c "Tracked: #56" docs/DIVERGENCES.md` → 2 today, `:47` and `:153`; slice 1
closes `:47`'s), so `make registry-check` reports live-tracker-open until it moves; its workspace
half retires and its `organization_disabled_error` half is re-argued (§6.9) and re-pointed at a
new issue.
**Re-argue the twelve entries that actually cite single-tenancy as their reason**, which is a
derived set, not a remembered one: `grep -nE "single-tenan|single organization|single-organization"
docs/DIVERGENCES.md` → `:19`, `:47` (slice 1's, already rewritten there), `:52`, `:58`, `:100`,
`:102` (the 500 GB per-org quota, whose clause about a fixed ceiling being arbitrary on a
single-tenant deployment gives way to the operator-owns-its-own-disk reason the same entry
already carries — the words are paraphrased here, not quoted),
`:128`, `:150`, `:153`, `:245`, `:251`, `:255`. **Separately**, each
with its own stated reason rather than a single-tenancy premise it does not contain, revisit
`:85` (environment worker-key *issuance*, which contains no tenancy word at all), `:86`, `:122`,
`:125`, `:147` (the 1,000-scheduled-deployments cap) and `:154` — for `:122` the divergence
**grows** rather than shifts, and its recorded count of **seven** org/account-level roles stands
unchanged: what the workspaces doc adds is a second axis, five *workspace-level* roles with
documented inheritance (§4.2), so three org-wide roles with no per-workspace granularity is a
wider gap under real tenancy, covered by §3's fine-grained exclusion. **Leave `:267`'s organization clause intact** —
"v1 answers only for `default` and 404s anything else *before* the environment lookup, naming
the organization in the message" describes the `{organization_id}` segment, which decision 2
freezes and `consoleOrganization` preserves, so striking it would delete an accurate record.
`:267` gains **nothing** about workspaces: its path is the environment-token dialect
(`/api/oauth/organizations/{organization_id}/environments/{environment_id}/tokens…`, `:86`),
which carries no `{workspace}` segment at all. The workspace half's new behavior — resolved
against the registry, constrained by the caller's scope only without the administration
capability, identical 404 — goes to `:125`, the entry whose path
(`/api/console/organizations/{org}/workspaces/{workspace}/api_keys`) actually has the segment;
`internal/api/server.go:171-173` and `:192-194` register the two dialects separately, which is
why the two entries stay separate. **Add** CONFIRMED: the reference's key kinds,
quoted with "legacy" intact, and the null `workspace_id` for Default-Workspace and
all-workspaces keys; the two workspace-administration divergences of §3 — (i) administration
lives on the `/api/` console dialect rather than `/v1/organizations/workspaces`, so the `ant`
organization verbs cannot drive this server, and (ii) it is authorized by a workspace-bound
`created_by IS NULL` key or an SSO admin where the reference requires an org-level credential
class ("an Admin API key, an `org:admin` OAuth token, or a personal or service account key that
isn't scoped to a specific workspace"); the 100-workspaces cap, unenforced with a reason; the
`workspace_archived_error` run-error message wording as ours; the environment `scope`
default's tie to organization type; and the Default Workspace's **archive prohibition** — the
reference documents it ("cannot be renamed, archived, or deleted", workspaces doc), so mirroring
it is CONFIRMED, with the refusal's 400 `invalid_request_error` status INFERRED because the
reference records no code for it (§7.6). **Add to the architecture-notes section, not INFERRED**: the
five shared substrates of §6.10 — they are platform architecture, not inferences a recording
could settle. **Add nothing to the INFERRED *section*.** The distinction is load-bearing rather than
pedantic: `tools/registrycheck/registrycheck.go:421-423` keys the open-issue-pointer rule on an
entry's **section**, so the archive-refusal status above — INFERRED inside a CONFIRMED-section
entry — is not covered by that rule and has to carry its own #78 pointer in the entry's prose. The two candidates a draft would put here are
**not** INFERRED by slice 6, because §5's recording closed them before slice 1: the environment
Bearer key's workspace resolution on the work routes (item 2) and the organization-id format
(item 3) both landed CONFIRMED at what was observed (§4.3) — the credential carries no workspace
at all, and the header is a bare UUID. Neither observation proved unreachable, so the demotion
path this sentence reserved is unused. A third candidate is gone
outright: the api-key `scope` field is fully documented and belongs to a resource this platform
does not mirror (§4.2). And per `docs/HISTORY.md:1095-1103` — #56's *last*
scope change silently rotted five registry pointers **while the issue stayed open**, a rot no
issue-state check can see — this PR re-reads every #56 reference by hand: **the registry's nine**
(`grep -nE "#56([^0-9]|$)" docs/DIVERGENCES.md` → `:28`, `:47`, `:121`, `:122`, `:123`, `:128`,
`:153`, `:181`, `:184`; the regex's trailing class is what keeps #565/#566/#567 out of the set,
and `:47` joined it in this plan's own PR),
`docs/self-hosted-security.md:851` and
`:1242`, `docs/ARCHITECTURE.md:466`, and **`README.md:76`**. Archived plans (31, 32, 37) and
released changelog sections (`docs/changelog/0.3.0.md`) are deliberately left as historical text.

## 8. Decisions, and the alternatives they beat

**D1 — Application-level predicates with an AST guard and a database write floor.** *Rejected:*
row-level security, on §6.6's evidence, chiefly that `internal/pgtest/pgtest.go:95` makes the
entire Postgres suite a superuser so policies would be inert under the whole merge gate.
*Rejected:* a store-layer repository every statement must call — it would move 211 statements out
of the handlers into a new package, contradicting `internal/store/store.go:6-9`'s stated
ownership ("Query SQL is not owned here — it belongs to the packages that issue it") and turning
a surgical change into a re-architecture. *Rejected:* schema-per-tenant — `queue.Claim` is a
single `SKIP LOCKED` scan across every tenant's work and would become an N-schema fan-out with no
way to keep its fairness or its one lease state machine, and the public docs allow 100 workspaces
per organization.

**D2 — The workspace is the isolation unit; org and project are enforced but constant.**
*Rejected:* a single-column `workspace_id` predicate — it leaves `org_id` on rows looking
authoritative when nothing validates it, and re-activating org later would re-sweep every
statement. Carrying all three costs one term in one helper; §6.4's write floor carries the pair
for the same reason, so "additive later" is a property the database holds, not a hope.
*Rejected:* dropping `project_id` from 14 tables — a larger, irreversible migration that
destroys a seam CLAUDE.md blesses.

**D3 — One key, one workspace** (decision 5). *Rejected:* a super-scope credential — every
predicate becomes conditional (`OR caller_is_super`), and a conditional predicate is the branch
that leaks when someone forgets it. *Rejected:* the reference's nullable-`workspace_id`
multi-workspace key, which would make the one secret every deployment holds
(`CONTROLPLANE_API_KEY`) a platform-wide credential. Note what this adopts: the reference's
**legacy** class — "A workspace key (a legacy key without an owner) belongs to the workspace it
was created in and always runs there", with the authentication doc adding that such keys "should
be considered legacy" (§4.2). That is deliberate — this platform mints no identity-linked
credential and has no user or service-account identity for a key to act as — and the key-kinds
registry entry records the qualifier rather than dropping it. *Rejected:* a separate admin key
class — plan 31 declined exactly that alongside the Admin API.

**D4 — Scope stays off the credential row where the row is not the authority.**
`environment_keys`' own columns stay unread because the environment is the authority (§6.1), and
`principals`' stay unwritten because `0022_principals.sql:12-15` already argues that storing
authority there makes the table "a second, stale authority beside the IdP". *Rejected:* copying
scope onto `environment_keys` at mint time — a copy can drift and needs a backfill; the join
costs nothing.

**D5 — Child rows copy their parent's scope; the two no-FK tables stay unscoped** (decision 3).
*Rejected:* dropping the three columns from `events`, `work_items`, `session_threads` and
`principals`. The diagnosis behind that proposal is correct and verified — those columns have no
reader and three of four writers already forgot them — but the drop is irreversible, contradicts
the reservation stated in three immutable migrations and `internal/store/store.go:34-42`, and
forecloses per-workspace event and work-item aggregates without a join. *Rejected:* scope
columns on `deleted_sessions` and `session_checkpoints` — neither is listed, enumerated or
rendered to a tenant, and the reaper that reads the tombstone is endpoint-local and
tenant-agnostic by design (`internal/executor/reaper.go:3-9`). *Considered and priced, not
dismissed:* `store.SessionTombstoneInsertSQL` (`internal/store/store.go:59-62`) is already an
`INSERT … SELECT` from a `sessions`-joined read, executed at `internal/api/sessions.go:1335`
inside `deleteSession`'s transaction, so a scope predicate on `s` would cost one term and would
close the one path by which a cross-workspace delete could write a tombstone. It stays exempt
because D5's *reader*-side argument does not cover the writer and the honest answer is that the
delete itself is already scoped by slice 4 — the tombstone can only be written for a session the
caller could delete. That is the reason on the exemption entry, in place of "not rendered to a
tenant".

**D6 — Propagation by re-read, not by carrying scope in the work item.** Three separate
"no"s, because the draft's single citation conflated them: *Rejected:* a scope field on
`queue.Item` (`internal/queue/queue.go:94`) and on `Claim`'s own `RETURNING` list (`:320-324`) —
both consumers already re-read the session under lock, so the columns join an existing
single-row SELECT at zero extra round trip. *Rejected:* adding the triple to `workColumns`
(`:170`), which is the projection every **Work-API** query selects for wire rendering and would
put a tenancy field on the wire. *Rejected:* a scope parameter on `queue.Enqueue` (`:204`) or on
`EnqueueOutputsHarvest` (`:257`) — it would touch callers in three packages for a value each
statement can read itself, which is why slice 2 rewrites both as `INSERT … SELECT` while
explicitly preserving `EnqueueThread`'s `created` contract.

**D7 — `wrkspc_`-minted ids, private-prefix family, default workspace keeps the literal
`default`.** *Rejected:* operator-chosen free-text ids, on the ground that a new prefix would
force a `knownPrefixes` widening and a documentation pin. **That cost does not exist**:
`internal/domain/id.go:42-63` already carries three private prefixes deliberately outside
`knownPrefixes` with the reasoning written in place, and `internal/domain/docs_test.go:53-56`
both states that exclusion and pins documents to `knownPrefixes` only. Minting matches the docs'
"Workspace identifiers use the `wrkspc_` prefix" for free. *Rejected:* rewriting the default
workspace's id to a `wrkspc_` value — an `UPDATE` across every scoped table including `events`
for a cosmetic gain. Accepting the literal `default` on *input* is registered as our lenient
parse, with no appeal to `anthropic-sdk-go/config/federation.go`, which speaks only to the token-exchange body.

**D8 — Environment `scope: "account"` stays refused, on a new reason.** The SDK and spec are the
only sources and they define `account` as visibility to the owning **account** — a principal —
self-hosted only. That is orthogonal to org/workspace, and this platform has no account identity
for a machine credential to own an environment with. Only `docs/DIVERGENCES.md:52`'s stated
reason ("single-tenant v1") expires. One thread stays live and is registered rather than
resolved: the field "defaults based on organization type"
(`anthropic-sdk-go/betaenvironment.go:735-736`), so if org ever activates, that default is
reference behavior nothing records today.

**D9 — Emit `workspace_archived_error`; leave `organization_disabled_error` unemitted.**
*Rejected:* inventing an organization lifecycle so the second error type has a producer.
`0031_deployments.sql`'s CHECK lists already admit both values, so the emitted half needs no
migration — exactly as `:153` predicted. The emitted half's `message` is ours and registered.

**D10 — Cross-tenant refusal follows the split the tree already decided on evidence, rather than
unifying it — and the 404 arm is CONFIRMED against what looked like real counter-evidence.** `/v1` resource routes
and create-time references answer **404**, byte-identical to absent. That is a choice, and the
entry records both sides: *for* it, the reference's own existence-hiding precedent ("the same
response as for any unknown workspace", authentication doc) and the fact that every cross-scope
refusal already in this tree outside the work API is a 404
(`internal/api/sessioneventsauth_test.go`, `internal/api/filesauth_test.go:64-68`); *against*
it, the SDK's three statements that an org/workspace mismatch is a **400**
(`betamessage.go:8994`, `:15698-15704`; `betamessagebatch.go:707-713`) — on
`fallback_credit_token`, an opaque credit code rather than a resource-id reference, which is why
the extrapolation was arguable and why §5 item 1 existed to settle it. **It did: the reference
answers 404 on both resource families** (§4.3 item 1), so the 400 does not generalise off that
opaque credit code and this decision keeps its 404 on the reference's own authority. The other answers stay as
the tree decided them: the work API's **403** `permission_error` "Token not authorized for this
environment" is untouched, because it is environment-level and pinned to a 2026-09-02 recording
(#78 — `internal/api/workapi.go:362`, `workapi_test.go:78-99`, which also pins that an unknown
environment id answers identically); the header cases answer 400/404 per the documented bodies
(CONFIRMED for all four arms now, the fourth from §5's recording rather than the docs — §6.2); the console segments
answer 404, preserving today's shape. Four answers each
tracing to a recording or to an existing surface's own behavior is not an inconsistency —
picking one globally would override a recording.

**D11 — The guard's completeness claim is a pinned exemption list, not a minimality assertion.**
*Rejected:* "the list contains nothing but the platform-wide sweepers", which cannot hold once
the target set includes the ~50 rule-(b′) mentions in brain, executor and vaultresolve — by
own primary key and by a caller-resolved parent id alike (§6.5) — the 31-mention (18-statement) `internal/queue` work-API surface, and the ~49
session-keyed mentions rule (f) admits in `internal/events` (51 mentions across 59 statements) —
without rules (b′), (e) and (f) each of those classes would have to enter through (d), and the
assertion would be false the day it landed. What is pinned instead is **nineteen** entries by
count and by name (§6.5), five of them the cross-file parent resolutions rule (b)'s
same-function check deliberately cannot admit. *Rejected:* shrinking the target set to
`api` + `events` + `queue` and admitting brain/executor later — same end state, but §7.2's five
stamping inserts, four of them outside any handler, would then be caught by the plan's memory
rather than by the mechanism.
Pinned count plus pinned membership gets both: brain and executor are in scope from slice 2, and
growth is a reviewed edit.

**D12 — The guard fails closed on a composed *table name* only.** *Rejected:* failing closed on
any unparseable SQL literal, which the tree does not satisfy — 77 fragment-concatenation lines
under §4.4's stated rule, one of them a function returning a predicate. The narrow rule is
satisfiable today and still catches the refactor that would defeat the scanner.

**D13 — The write floor normalizes before it constrains.** *Rejected:* asserting "every existing
row holds `'default'`" from the column default, which nothing establishes: no CHECK, and the pin
that looks like proof reads one row from three of fourteen tables
(`internal/store/store_test.go:641`). Because `Migrate` is one transaction at every binary's
startup (`internal/store/migrate.go:27-33`), a single stray value would make the deployment
unbootable with no repair path — the failure mode `0013` was written to avoid.

## 9. Risks

- **Row scoping is not compute isolation, and the security doc must get louder, not corrected.**
  One executor's daemon or namespace runs every tenant's containers
  (`internal/executor/reaper.go:3-9`); `docs/self-hosted-security.md:37` and `:1200-1210`
  already call it one trust domain. That statement stays true and now means one team's sandboxes
  sit beside another's. Bounded by decision 1 (administrative separation, not adversarial
  co-tenancy), §3's registered non-goal, and the new "Reserved seams" entry.
- **Multi-tenancy invites a second Casdoor organization, which re-opens an unpatched 9.x CVE.**
  `docs/self-hosted-security.md:926` voids CVE-2026-9094 (cross-organization escalation) purely
  because the bundled Casdoor seeds exactly one populated organization with public signup off —
  "there is no second tenant to escalate across". Nothing here falsifies that, but the natural
  next step an operator takes reopens it on a path the deployment cannot close. Bounded by an
  explicit requirement in slice 6: **platform workspaces are not Casdoor organizations**; the
  workspace claim is a claim within the one Casdoor org, and `IDENTITY_ROLE_MAP` keys stay
  spelled `map/<group>` (`:950-951`), so a tenancy-aware role map touches the same string. Plan
  31's fifth exclusion — "the platform never calls the IdP — it only verifies tokens"
  (`docs/plan/31_console-sso-rbac.md:124-126`) — is what keeps this a configuration risk rather
  than a code path.
- **A single missed predicate on `vault_credentials` is credential disclosure, not metadata
  disclosure**, because `secrets.Cipher.Encrypt` has no per-tenant key
  (`internal/secrets/secrets.go:38`) and there is no second envelope to fail. Bounded by rule (g),
  which covers the table from slice 2 because it is one of §6.5's eleven unscoped children; by
  §7.5 replacing the Go-side `row.vaultID != vaultID` comparison
  (`internal/api/vaultcredentials.go:241`, `:320`) with a scoped `vaults` join; by naming the
  one credential **write** that carries no parent term at all
  (`internal/api/vaultvalidate.go:171-173`), and by a registered non-goal that says so plainly.
- **The guard cannot check a predicate's argument.** `workspace_id = $1` bound to the environment
  id passes every static check. Bounded by the per-statement-kind coverage and the two-tenant
  mirror test (§6.5). A refactor toward a query builder would defeat the scanner outright, and
  the guard's own comment must say so.
- **The exemption list is a social control.** A reviewer who waves through a new entry reopens
  the whole failure mode. Bounded by requiring a written reason per entry and by slice 6's pinned
  count and membership (D11).
- **`events` has no database floor, by a trade rather than by a clean argument.** Slice 2's fix
  is a Go-side copy into a builder-composed multi-row INSERT (`internal/events/log.go:203`),
  which the `sessions` key cannot validate; the composite FK is excluded on lock cost. Bounded by
  the guard's `INSERT … SELECT`-or-explicit-columns rule, the propagation test, and this
  paragraph saying it out loud rather than claiming the FK covers it.
- **Coverage and gate runtime, in measured numbers.** The gate is total statement coverage ≥ 90%
  computed exactly — the Makefile's awk deliberately avoids `go tool cover`'s 0.1% rounding so
  ~89.95% cannot pass as "90.0" (`Makefile:94-96`, the recipe at `:98-105`) — over `./internal/...` minus twelve named
  test-support packages (`Makefile:85`). Every scope check adds a refusal branch needing a test,
  and it all lands in `internal/api`: 19,117 production against 28,564 test lines, and 520 test
  functions of the tree's **2,523**. *Counting rules: `wc -l` over `internal/api/*.go` split on
  the `_test.go` suffix; `grep -h "^func Test"` over `internal/api`'s test files for 520, and
  over every `*_test.go` in the tree **excluding `.claude/`** — whose worktree copies would
  otherwise double the denominator — for 2,523.* The suite already runs about ten minutes, which
  is why `go test`'s timeout was raised to 30m (#490, `Makefile:69`) and why a timed-out binary
  leaks pgtest fixtures into the next run (#499). Bounded by: extend existing tests with a second
  workspace rather than adding database-per-test cases; let the guard carry completeness so
  behavioral tests stay table-driven; and if a tenancy test-support package is added, edit
  **both** `Makefile:85`'s `grep -vE` exclusion and the prose that names the same packages.
- **The identity lane's claim is the weakest link.** An IdP that lets a user edit their own
  workspace attribute hands them tenant selection. `internal/identity/claims.go:19-26` already
  argues this hazard for roles — "the alternative shape … lets the TOKEN choose the
  interpretation" — and the same warning rides the two new variables.
- **Decision 4's cliff is an operational surprise.** Creating a second workspace refuses every
  human until `IDENTITY_CLAIM_WORKSPACES` is configured. Bounded by the release note and by the
  fail-closed arm being a *checkable* condition (exactly one **live** workspace row) rather than
  a configured default that can rot.
- **Slice 4 is one large PR by design** — the ten list builders, the by-id reads, ~16
  cross-reference checks — because a half-scoped read surface means a scoped session resolving an
  unscoped agent. It may be split by table family if review load demands it, and that is safe
  because no second workspace exists until slice 6; the plan should not lean on "nobody has
  configured it yet" across merges without saying so out loud, which is what the sequencing
  property in the opening does.
- **§5's recording could have invalidated a decision, not just an entry** — which is why slice 1
  was gated on it rather than merely informed by it. **Retired 2026-09-05**: the cross-tenant
  resource id answers 404, so §7.4's refusal shape and D10's split stand (§5.1). The residual
  risk this leaves is smaller and named at §5.1: one half of item 6, an environment key belonging
  to an archived workspace, is still unobserved, and §6.1's rule for that lane is ours.
- **#46 and #550 overlap** (decision 8). #46 (per-org rate limiting, priority medium, no
  `post-v1` marker) already specifies keying on the reserved org scope, so landing it on
  frozen-org credentials is cheap while re-keying later is not. #550 must split work-API
  admission by route on the very routes slice 5 touches; its 2026-09-02 recording is live
  evidence for the work API's **refusal statuses** (`docs/DIVERGENCES.md:28`, the 403 body and
  the `work.poll.wrong-env.cloud` fixture), and as far as that entry's citation of it shows it
  carries no tenancy-header observation — the recording lives outside this tree and was not
  re-read for this plan. So those semantics rested on the public docs, which is precisely why §5
  item 2 existed — and it has since answered: that credential carries no workspace at all, and
  the work lane ignores the workspace header (§4.3 item 2), which is a stronger footing for
  slice 5 than the docs gave. This plan
  assumes both issues stay independent and says so, so a later conflict is a known decision
  rather than a surprise.

## 10. Open questions

None on scope: the eight decisions of §1 settle appetite, the organization level, the write-only
columns, the SSO cliff, the credential model, the recording, the limits and the ordering. What
remained open was **evidence, not choice** — the **five** NOT OBSERVED items of §4.3 — and
**§5's recording of 2026-09-05 closed all five**, including the fifth
(`anthropic-organization-id`'s absence schedule), which turned out to be observable after all
even though it had been registered as ours; it stays ours, now as a stated simplification of a
two-axis rule (§4.3 item 5). Items 1-5 were captured in full and item 6 on its key lane. That
meets both gates, and item 6 is the one that needs the argument said out loud rather than
assumed: the slice-1 gate asked for it so §6.1's refusal could be aligned to something
observed rather than chosen, and the API-key lane is the lane that rule was aligned to. The
environment-key half stays unobserved, and §6.1 carries it as ours (§5.1). So neither the
slice-1 gate (items 1-4 and 6) nor the slice-6 one (item 5) is outstanding. **Nothing the
2026-09-03 and 2026-09-04 recordings settled closed any of those items** — they reached the
environment key's OAuth scopes and the skills wire shape, never a second workspace — which is
what 2026-09-05 was for, and the registry's second wave (#575) left `:47` and `:153` as the only
two entries still tracking #56.

**What is still open is one half of one item and two small threads.** An *environment* key
belonging to an archived workspace was not reachable (§5.1), so §6.1's rule for that lane is
ours rather than mirrored; and the reference's answer for a multi-workspace key sent with no
header was not reachable either, so §6.2's SSO-lane 400 keeps its INFERRED label and its #78
pointer. Riding along without blocking anything: the environment `scope` default's tie to
organization type (§8-D8). The skill display field's **name**, which an earlier draft carried as
a second such thread, is no longer one — plan 39 settled it as `display_name` (§4.2).
