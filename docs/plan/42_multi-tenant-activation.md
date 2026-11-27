---
status: in-progress
issue: "#56"
---

# One organization with multiple workspaces (plan 42)

The owner approved the workspace model and Console URL cutover on 2026-11-26.
Each deployment serves one organization. Teams in that organization share infrastructure
and use workspaces to isolate resources, model activity and access. The organization and
project storage keys stay `default`. Each resource request resolves exactly one workspace.
[Issue #56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)

The first delivery migrated the Console's Default page URLs. Stage 2 registers
Default, binds credentials, migrates existing grants and enables independent
Default roles. The owner selected a maintenance cutover on 2026-11-27. Stop the
old authority readers before migration 0071. See the [upgrade procedure](../ARCHITECTURE.md#default-workspace-upgrade).
Stage 3 binds resource storage, background execution and worker reads to their owning workspace.
The owner approved coordinated upgrades. Migration 0072 retires the old writer initialization trigger.
Stage 4 binds browser and OAuth clients to a selected workspace and accounts for every Gateway request in its originating workspace. Drain and stop previous Gateway replicas before migration 0073 changes the rollup key. Stage 5 enables the private workspace lifecycle and Console management.
[Plan 69](./69_principal-bound-auth.md) and [Playground grants](./73_console-playground.md)

## Identity, roles and membership

Store explicit membership against the stable pair of workspace and principal IDs, with
one assigned role per pair. A principal is an active organization user or service account.
Resolve effective access at the platform, for password, OIDC and trusted-proxy sign-ins
alike. Existing IdP mappings retain their organization-role semantics. The workspace
registry and membership records supply workspace authority. An org-wide request, such as
changing the role of an organization member, checks organization authority separately.

| Effective workspace role | Workspace and Console operations | Public resource API |
| --- | --- | --- |
| User | Playground in this workspace | No API key or resource management |
| Limited Developer | Developer operations except opening or starting sessions, opening memory stores, managing deployments, or downloading or deleting files in Console | Developer operations on resources and inference in this workspace |
| Developer | Use Playground, operate workspace resources, issue and manage own keys within platform policy | Same resource and inference operations as Limited Developer |
| Admin | Developer operations plus workspace settings, membership, and workspace-scoped administration | Developer operations plus actions the endpoint grants to a workspace administrator |
| Billing | Read workspace billing and usage summaries | Billing reads only, with the appropriate credential and endpoint |

Organization Admin inherits Workspace Admin everywhere. Organization Billing inherits
Workspace Billing everywhere and may be explicitly granted Workspace Admin in a particular
workspace. Billing is inherited rather than directly assignable. Organization users and
developers get the roles granted in their individual workspaces. Preserve explicit grants
across promotion or demotion and recompute inheritance on each authority check. Losing an
organization role removes its inherited access. An inactive principal has no effective role.
Scope an administrator's member changes to existing organization principals. Organization
membership, organization roles, gateway configuration and organization-wide limits each
require their own organization-level permission.

| Organization role | Organization-level authority |
| --- | --- |
| User | Own account and setup. Resource and Playground access require a workspace grant. |
| Developer | Own account, own key management and the existing read-only gateway configuration. Workspace grants scope resource and request-usage reads. |
| Billing | Organization-wide cost and usage summaries, including workspace breakdowns. Request payloads, member administration and gateway configuration require separate authority. |
| Admin | Manage organization members and roles, service accounts, workspace lifecycle, credential administration, gateway configuration and organization limits. Read organization totals. |

Use explicit capabilities for billing, resource and Console actions. Adding Billing to a
linear role ranking would grant unrelated operations. Preserve the Console origin/lane when
dispatching through private proxies so the Limited Developer restrictions also apply to
direct private requests. Public API clients retain the documented developer operations.
On 2026-11-27 the owner approved workspace Admins issuing keys for active service accounts with resource access in the same workspace. The service account's identity and organization role remain under organization administration.
A credential narrows the authority of its holder. Organization administration through the
Console requires its signed-in human session and CSRF on writes.
[Workspace roles, inheritance and service accounts](https://platform.claude.com/docs/en/manage-claude/workspaces#workspace-roles-and-permissions)

## Workspace identifiers and upgrade

- Seed one protected workspace named Default. Its page and private Console path segment is
  `default`, its existing storage key is `default`, and its public ID is
  `wrkspc_00000000000000000000000001`, the ID already issued by CLI OAuth.
  Resolve both representations through a single registry row. Emit the public ID on
  workspace-bearing public responses. The private key representation can retain its
  recorded null Default field. Other workspaces get unique immutable public IDs.
  A workspace hint selects among authorized workspaces. Keep the reference's validation
  of wire IDs and preserve any explicitly supported client compatibility through the registry.
  [Existing CLI grant](../../internal/clioauth/oauth.go) and [wire ID contract](https://platform.claude.com/docs/en/manage-claude/workspaces)
- Migrate active developers, including developer service accounts, to explicit Default
  Developer membership. Admins receive inherited access. Give an active user with the
  existing Playground grant Default User access. An ungranted user retains account and
  setup access. Preserve explicit Playground grants for users through later organization
  role changes. Removed users, archived accounts, disabled/expired keys and historical
  unowned keys gain no access. New organization Users, Developers and ordinary service
  accounts need an explicit workspace grant, including for Default. Inherited Billing
  access creates no explicit Default grant. Explicit membership in Default is a self-hosted
  policy. Claude derives Default access from the organization role and gives it no separate
  member list. Record this divergence when membership ships.
  [Default workspace FAQ](https://platform.claude.com/docs/en/manage-claude/workspaces#whats-the-default-workspace)
  Keep legacy Playground grant and revoke statements atomic with Default membership
  changes after the maintenance cutover.
- Preserve continuation for a still-active initiator whose role was reduced to User
  before upgrade. Record eligibility on existing Default sessions, dreams and deployments
  and propagate it only to work they spawn. This covers an initiator with no Playground
  grant. It authorizes subsequent turns and scheduled runs of that work alone. Removing
  the initiator from the organization or explicitly revoking its workspace authority
  ends continuation, as does workspace archival. A role-only change preserves continuation.
- Bind existing owned keys, resources, sessions, deployments and usage history to Default,
  preserving IDs, owners, issuers and immutable initiators. Apply the same initialization
  to legacy-format writes after cutover. Replay the backfill without changing
  an operator's later grant or resurrecting a revoked one. Inventory unexpected historical
  scope triples before adding constraints and report affected rows for reconciliation.
  Assign new migration numbers when implementing the slice.
- Seed registry uniqueness and add composite organization/workspace foreign keys on resource
  roots. Stamp each scoped insert explicitly. Derive child scope from its validated parent,
  including session events, threads, queued work and deployment runs. Validate populated
  tables and measure writer locks before removing reliance on single-workspace defaults.

## Authorization at each entry point

| Entry point | Source of workspace authority and required checks |
| --- | --- |
| Browser resource proxy and private Console routes | Resolve the signed-in principal, selected workspace and effective role. Path and forwarded header must agree. Validate CSRF on writes. Revalidate membership on stream delivery using the existing authority watcher. |
| Public Managed Agents API | Keep the SDK's `/v1` resource paths. Resolve the owner's live grant and the key's single bound workspace. An optional `anthropic-workspace-id` may only narrow access. Reject a foreign or unknown well-formed workspace identically. |
| CLI OAuth | Resolve the grant's principal, scopes and workspace, including the existing Default ID on token responses and requests. Keep token audience and route restrictions. Bind a new workspace during consent and recheck membership on redemption, refresh and use. |
| Existing public organization audit reads | Preserve the consented CLI `org:admin` gate on `GET /v1/organizations/workspaces` and the API-key list/detail reads. List accessible registry rows using the documented Default inclusion option. Render each key's actual public workspace ID and bind filters and cursors to the authorized organization and requested workspace. |
| Environment keys, worker and gate tokens | Derive authority from the environment or leased item and its session. Scope poll, completion, memory, skills and downloads to that authority. Preserve lane-specific route and lease restrictions. |
| Brain, scheduler, dreams and delegated inference | Propagate the session or deployment workspace into child sessions, work and model calls. Recheck the immutable initiator's active membership when authorizing the next turn or scheduled run. Preserve already-leased completion semantics and the delegated token's gateway route and claim lifetime. |
| Gateway, Playground and usage | Resolve workspace before inference, record workspace with the existing principal/key attribution and retain organization-level limit enforcement. Keep local workspace selection out of upstream provider headers. Enforce workspace filters before aggregation, pagination or detail lookup. Billing summaries expose aggregate usage. Organization totals require organization authority. |
| Agents overview, search and private reports | Apply the authorized workspace to tables, joins, chart windows and detail filters. Key browser caches by principal and workspace and cancel old reads/streams on a switch before displaying results. |

For a browser request the selected workspace comes from the page and is conveyed using
the private route's workspace segment or a validated workspace header on the existing
resource proxy. The private Playground uses its recorded `x-workspace-id` selector;
public requests use `anthropic-workspace-id`. Resolve each within its own protocol.
Two tabs can select different workspaces in the same signed-in session.
An absent selector on a workspace-bound credential uses its binding. A request requiring a
choice from several grants must select one explicitly. Scope the error and response header
contracts by credential lane and route using the recordings.
[Workspace recording](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/tree/main/2026-09-05)
and [gateway follow-up](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56#issuecomment-6245317786)

The first activation issues only workspace-bound API keys. Claude also supports
unscoped keys that select an authorized workspace per request. Supporting that key type
would require its own issuance and selection contract. Keep existing unowned keys disabled
under Plan 69. [Claude API key scoping](https://platform.claude.com/docs/en/manage-claude/workspaces#how-workspaces-work)

Scope each read, write, list, cursor and download using its table's scope triple or a
parent resolved under that scope. In the same transaction validate every referenced
resource before saving a session, deployment, dream, file binding or tool configuration.
Include IDs inside snapshots and JSON, including initial outcome rubric files, vaults,
resolved agents and nested skills. Verify ownership before reading blob bytes or enqueueing
object deletion. Keep built-in Anthropic skills readable through an explicit catalog
exception. Bind custom skills and their versions to their owner workspace. For resource
lookups, a foreign ID has the same status and public error body as a missing ID.

Keep the application-level SQL completeness guard and a reviewed list of parent-derived
or global operations. Scan embedded SQL as well as Go statements. Include the gateway
usage store, private reports and background writers. A hash lookup may derive scope for
authentication. A scheduler may scan globally to discover work, then must act under the
owning workspace. Test JSON reference writers at the call path that performs validation.
The [earlier SQL inventory and guard design](https://github.com/OpenSDLC-Dev/managed-agent-platform/blob/1b1c368fa5074a62e182d81c5473263e38d07675/docs/plan/42_multi-tenant-activation.md#65-the-completeness-guard)
preserves the discovery procedure and historical recording coordinates for re-auditing current code.

## Console rollout and staged acceptance

1. **Default pages and design.** The Console registers workspace pages under
   `/workspaces/default/`, including `agents-overview`, `agent-quickstart`,
   `messages-overview`, `logs` and `resources-overview`. Organization keys, members
   and service accounts live under `/settings/`. Dashboard and organization usage
   keep their top-level routes. Retired flat pages and unknown workspace URLs return
   HTTP 404 before any Default resource read. Exercise deep links, reload, Back/Forward,
   filtered lists, drawers and the Playground-only landing with the Console fixture.
2. **Registry, grants and Default upgrade.** Add the registry, membership, billing
   capabilities, credential bindings and compatible writers. Exercise pre-upgrade
   and current writer statements, repeat migration, inactive principals, service accounts, old CLI tokens
   and both states of the legacy Playground grant. Verify the same effective access
   before and after upgrade, including admin demotion and continuation of a demoted
   ungranted User's existing work without permission to start new work.
3. **Scoped storage and execution.** Introduce the SQL guard, scope stamps, foreign keys,
   list predicates and cross-reference validation. In seeded A/B fixtures compare
   absent and foreign IDs across CRUD, downloads, copied snapshots and worker routes.
   Exercise races between archive/removal and a new grant, enqueue or write. Check
   event replay, retries, leased completion and the next initiated turn.
4. **Gateway and clients.** Exercise cookie, key, OAuth, environment and delegated
   credentials. Attribute fixture calls, Playground and Agents reports in A and B.
   Check two simultaneous browser tabs, caches and streams after membership changes.
   Verify billing summaries, organization rollups, pagination and individual request
   visibility. Exercise CLI organization audit reads with A/B keys, their public IDs,
   filtered cursors and Default inclusion. Run a real SDK/CLI workspace contract against
   the local platform.
5. **Enable additional workspaces.** After every serving replica and worker enforces
   scope, enable the private create/list/archive and membership surfaces and connect
   the Console picker. Require the cross-process A/B matrix to pass before activation.
   Workspace Admin can manage its workspace, while organization administrators create
   and archive workspaces. Protect Default against renaming and archival. Archive is
   terminal for other workspaces. Preserve historical reporting, retire workspace
   keys, deny new work, pause deployments and finish or reclaim already-leased work.
   Use the workspace-aware binaries for rollback once a second workspace exists.

The acceptance matrix uses the same human as Developer in A and User in B, a Limited
Developer on Console and public API, an organization Admin, an inherited Billing member
with a single Workspace Admin override, a service account and a removed member. Verify
that organization role changes update inherited rights without rewriting explicit grants.
Test distinct workspace keys, identical object names across workspaces, archived credentials,
foreign references and the absence of another workspace's rows or totals.

The reference recording establishes the cross-workspace refusal and the fate of an
archived workspace's API key. The environment-key archive behavior and lower-role private
error details still need recorded evidence. Track those observations separately from
platform policy in the [wire registry](../DIVERGENCES.md) when the corresponding behavior
ships. Organization quotas, billing settlement, additional public Admin operations and separate sandbox,
blob or database infrastructure have their own scope.
[Recording tracker](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/78)
