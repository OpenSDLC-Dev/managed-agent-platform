---
status: in-progress
issue: "#566"
---

# Skills converged onto the GA wire shape (plan 39)

On 2026-08-27 the reference collapsed its beta Skills namespace onto the GA shapes in one
commit (anthropic-sdk-go `7990249e14473d2eacb9bf959e279dc714b5a643`, released v1.68.0):
`display_title` became `display_name`, `latest_version` became `latest_version_id` holding a
`skver_…` id, `source` became a `{type}` object, and the version object lost both `directory`
and its Unix-epoch `version` — a version is now addressed solely by its id. This platform
pins v1.66.0, which predates that commit, and mirrors the pre-migration types faithfully:
`skillJSON` renders `display_title` / `latest_version` / a bare `source`, `parseSkillUpload`
accepts only the form field `display_title`, `internal/domain/id.go` mints `skillver_`, the
`{version}` path slot admits only `^[0-9]{1,32}$`, and `DELETE /v1/skills/{id}` refuses while
any version exists.

The break is total for creates and it is symmetric: a current SDK sends `display_name`, which
this platform rejects with `unknown form field "display_name"`, while this platform wants
`display_title`, which no current SDK offers. Neither field name lets a released client create
a skill here (#566 reproductions (a) and (b)).

Underneath sits a second, quieter defect the issue does not name. All three execution halves
decide "already concrete versus resolve the alias" with the same test — `skillDigitsRe`,
`^[0-9]+$` — in `internal/brain/skills.go`, `internal/executor/skills.go` and
`internal/worker/skills.go`. Anything non-numeric is treated as the alias `latest`. Once a
client pins a version the GA way, by its `skver_` id, this platform does not reject it: it
silently serves the newest version instead of the pinned one. That is a wrong answer, not a
refusal, and it is the reason the convergence cannot stop at the render layer.

## 1. Scope and goal

Serve the GA Skills shape, and only it. Every field name, id prefix, addressing form, delete
semantic and limit that the 2026-09-04 recording observed becomes this platform's behavior;
the pre-migration shape is retired rather than kept alongside. A current `anthropic-sdk-go`,
`anthropic` (python) or `ant` CLI must be able to create, list, read, version, download and
delete a skill against this platform without a shim, and an agent must be able to pin a
version by its id and actually get that version.

Out of the four shapes #566 offered — full convergence, dated-header dual shape, a
`display_name` alias on create only, and register-only — this plan is the first. The dual
shape is rejected in decision 11; the alias-only and register-only options are rejected in
the issue's own terms, because a client that creates successfully and then cannot parse the
response is worse off than one that fails at the request.

## 2. Ground truth (pinned 2026-09-04)

Everything in this section is a recorded byte, not a reading of the SDK types. The session is
`2026-09-04/` in the private `managed-agents-wire-recordings` repository: 163 entries in two
batches, taken through the console's same-origin proxy against the live endpoint, US$0 (no
model turns). Bracketed numbers are entry indices there.

**The Skill object has exactly seven keys** — `type`, `id`, `display_name`, `source` (an
object `{"type": "custom"|"anthropic"}`), `latest_version_id` (a `skver_…` id), `created_at`,
`updated_at` [0, 2, 16]. `display_title`, `latest_version` and a bare-string `source` appear
nowhere in the GA lane.

**The SkillVersion object has exactly six keys** — `type`, `id`, `skill_id`, `name`,
`description`, `created_at` [30, 143]. There is no `version` and no `directory`. `name` is the
skill's immutable slug, identical on every version of a skill.

**`display_name` is optional, derived, capped and not unique.** Omitted, it becomes the
SKILL.md frontmatter `name` [2]. Two skills may carry the same `display_name` [3, 4]. At 255
characters it is accepted [6]; at 256 it is `display_name must be at most 255 characters
long` [7]. A `display_title` form part is **ignored** rather than rejected — the create
succeeds and the name is derived from the frontmatter as if the part were absent [5].

**`files[]` is the part name and it is required.** A part named bare `files` is
`files[]: Field required` [8], as is a form carrying no file part at all [11]. An unqualified
`SKILL.md` with no directory prefix is accepted [9], as is a zip [10] and a multi-file
upload [15].

**A version is addressed by its id, or by `latest` on read.** `GET …/versions/{id}` and
`GET …/versions/latest` both answer 200 [30, 32]. A numeric timestamp is
`Invalid version id: '1759178010641129'` [34], and so is a `skillver_`-prefixed id [38].
`DELETE …/versions/latest` is refused: `'latest' is not accepted when deleting a skill
version. Address a specific version id: read the skill's latest_version_id, or GET
/v1/skills/{skill_id}/versions/latest, and use that id.` [60].

**Delete is the mirror image of ours.** `DELETE /v1/skills/{id}` cascades, answering
`{"type":"skill_deleted","id":"skill_…"}` and taking every version with it — the skill, its
version list and each version all answer `Skill not found: skill_…` afterwards [79-82].
Deleting a skill's **only** version is refused instead: `cannot delete a Skill's only version.
Delete the Skill, or create another version first` [111]. So a skill always holds at least one
version, which is why `latest_version_id` is documented "Always set".

**An agent pins a version by its id, and the id survives verbatim.** `version` omitted is
stored as the literal `"latest"` [106]; a `skver_` id is stored and echoed unchanged [101,
107]. A numeric [102], a `skillver_` id [103] and another skill's version id [105] are each
``Agent has invalid configuration: `skill_id` `…` version `…` not found``. Deleting a skill an
agent references succeeds and leaves the agent holding a dangling reference [108, 109].

**Versions carry a name-consistency rule.** A new version whose frontmatter `name` differs
from the skill's is `Skill name 'rec81-renamed' in SKILL.md must be consistent across all
versions for a given `skill_id`. Expected 'rec81-probe-a'.` [54].

**List parameters.** `limit` outside 1..1000 is `limit must be between 1 and 1000` [24, 25];
1000 is accepted [23]. `source` outside the pair is `source must be one of custom, anthropic`
[21, 22] — the filter did not widen when the `source` object gained `anthropic_example` and
`plugin`. A malformed cursor is `page is not a valid page token.` [28]. The envelope is
`{data, next_page}` with an opaque `page_`-prefixed cursor [26, 27].

**`?beta=true` selects nothing.** Five read pairs are byte-identical across the bare and
`?beta=true` paths [16/17, 30/31, 32/33, 0/1, 43/44], and the write pairs behave identically.

**The dated header is a full dual shape, fields and behavior.** With `anthropic-beta:
skills-2025-10-02` the skill object returns `display_title`, a bare `source` and a numeric
`latest_version` [18, 19]; the version object returns `version` and `directory` and an id
prefixed **`skill_version_`** [142]; the `{version}` slot takes only the numeric form
(`Invalid version format: 'skver_…'. Version must be a numeric timestamp (e.g.,
'1759178010641129').` [36, 37]); and `DELETE /v1/skills/{id}` refuses with `Cannot delete
skill with existing versions. Delete all versions first.` [87] where the same verb without
the header cascades [79]. Note the id prefix: the reference's pre-migration prefix was
`skill_version_`, and its GA prefix is `skver_`. This platform's `skillver_` matches neither
lane, so that divergence is two-sided and always was.

**The content download.** `GET …/versions/{id}/content` serves the archive on both the bare
path and `?beta=true` [134, 135], so the route is not beta-only server-side. `latest` is
refused there with the download-worded twin of the delete message [136]. The numeric version
**is** accepted by `/content` even on the GA lane [150] while version metadata rejects it
[34] — an asymmetry in the reference, recorded and not designed around. The console cookie is
refused with `Downloading skill content is not supported with this credential type. Use a
workspace API key, an environment credential, or a Session credential.` [44], and an
environment key is refused on the skill object GET with an `any_of(org:skills, …)` scope
message [146] while the version list, version GET and `/content` all succeed on it [143-145].

## 3. Design decisions

**Decision 1 — GA only; the dated lane is not implemented.** The platform serves one shape.
`anthropic-version` and `anthropic-beta` stay accepted-and-ignored per CLAUDE.md's wire rule,
and `?beta=true` keeps selecting nothing, which the recording shows is exactly what the
reference does with it. A client that still sends `skills-2025-10-02` gets the GA shape here
and the beta shape there; that is a deliberate divergence and it is registered.
*Rejected:* the dual shape (decision 11).

**Decision 2 — the wire shapes are adopted verbatim.** `skillJSON` becomes the seven GA keys
and `skillVersionJSON` the six; `directory` and `version` leave the version object.
`renderSkill` stops emitting `""` for a null latest version, because decision 6 makes a
skill without versions unreachable.

**Decision 3 — the numeric version stays the internal identity; the wire addresses ids.**
`skill_versions` already holds both the `skillver_`/`skver_` id (primary key) and the numeric
`version` (unique per skill) on one row, so translating an id to a version is one indexed
lookup. Keeping the numeric internally means the blob key `skills/{skill_id}/{version}.zip`
(`internal/skills/BlobKey`), the materialization sentinel and `skills.SentinelVersion` are all
untouched: no object-storage remap, no sentinel generation bump, no data migration. The
numeric simply stops appearing on the wire.
This holds for the platform-managed half only, and the asymmetry is forced rather than chosen:
the BYOC worker owns no database and sees only the wire, where the GA version object no longer
carries a numeric at all. Its resolved token is therefore the version id whenever the pin was
an id or the alias, and the numeric only when the pin itself was numeric. The two halves never
share a sandbox, so the sentinel each writes stays internally consistent; the one visible
effect is that a worker's first pass after this change sees a sentinel mismatch and re-extracts
each skill once.
*Rejected:* making the id the sole identity. It changes the blob layout at seven non-test call
sites, forces a `SentinelVersion` bump, and buys nothing a client can observe.

**Decision 4 — the `{version}` slot accepts an id, `latest`, and the numeric as a registered
legacy alias.** GA rejects the numeric outright [34]; this platform accepts it, because agents
created before this change may hold a numeric pin and a stored pin that silently stops
resolving is worse than a divergence. The slot therefore takes `skver_…`, `skillver_…`,
`latest` (read routes only), and `^[0-9]{1,32}$`. `latest` on DELETE and on `/content` is
refused with the reference's own two messages, verbatim. The operator importer's version
validation in `internal/api/skillsimport.go` gets its own regex so widening the slot does not
loosen the importer.

**Decision 5 — all three resolvers learn to resolve an id.** `resolveSkillMeta` (brain),
`resolveSkillVersion` (executor) and its BYOC twin (worker) replace the `skillDigitsRe`
two-way test with an explicit three-way one: `latest` resolves through the skill's latest
version; an id resolves that version row; digits are taken verbatim. The silent-wrong-answer
bug dies here, and it is the reason this plan is one PR rather than two — between a merged
render change and a merged resolver change, a client that round-trips `latest_version_id`
into an agent pin would be served the wrong version.

**Decision 6 — delete semantics are inverted to match.** `DELETE /v1/skills/{id}` cascades in
one transaction — versions then skill — with the existing per-version archive cleanup running
for each, and answers `{id, type: "skill_deleted"}`. `DELETE …/versions/{version}` refuses to
remove a skill's only version with the reference's message. Both directions are enforced in
the handler, not by the schema: migration `0007_skills.sql` deliberately has no `ON DELETE
CASCADE`, and a database-level cascade would orphan every stored archive. Migrations are
immutable, so `0007`'s now-stale comment is corrected by the new code and a new migration,
never by editing it.

**Decision 7 — `display_name` replaces `display_title` on input and output, and uniqueness
goes.** The create form accepts `display_name`, caps it at 255 characters rather than 4096
(runes, not bytes: the recorded refusal sentence says characters, so a multi-byte name
under the cap must be accepted), and
derives it from the frontmatter `name` when omitted. The partial unique index
`skills_custom_display_title_uq` is dropped in a new migration: the recording shows two
skills sharing a `display_name` [3, 4], and refusing a create the reference accepts is a
harder break than accepting one it refuses. A `display_title` part is ignored rather than
rejected, matching [5].

**Decision 8 — unknown create-form parts are ignored rather than rejected.** This follows from
decision 7 rather than standing on its own: `display_title` is only "ignored" if unknown parts
in general are. The observed evidence covers exactly one field name, so the entry registering
this says so. `files[]` stays required, with the reference's `files[]: Field required`.
*Rejected:* special-casing `display_title` alone, which would leave the platform rejecting
parts the reference tolerates while claiming to have converged.

**Decision 9 — `skver_` is minted; `skillver_` is accepted on input forever.** Existing rows
keep their `skillver_` ids and are not rewritten, because agent configs pin those ids and a
rewrite would dangle every pin. Both prefixes join `knownPrefixes`, in the mould of the
existing `session_` input alias, and CLAUDE.md's prefix list changes in the same commit —
`TestDocsEnumerateTheWirePrefixSet` fails the build otherwise. Old rows therefore still render
a `skillver_` id on the wire, which a GA client round-trips correctly because the id is opaque
to it; the entry registering this says so plainly.

**Decision 10 — the list ceiling rises to 1000 and the `source` filter does not widen.** Both
are recorded reference behavior [23-25, 21-22]. The version list already caps at 1000 and is
unchanged.

**Decision 11 — the dated-header dual shape is rejected.** It is the only option that would
require `internal/api` to select a response shape from a request header, which contradicts
CLAUDE.md's own "accept and ignore" rule and the doc comment in `internal/api/doc.go`; it
permanently doubles the wire surface and its tests; and the single consumer it would spare is
one workshop example that a two-line edit also fixes. Were it ever adopted, the CLAUDE.md and
`doc.go` edits would be a policy amendment landing in the same PR, not a registry entry — the
reference genuinely serves both lanes, so this is a divergence from our own rule, not from the
reference.

**Decision 12 — the SDK pin moves to v1.70.1 in this PR.** A scratch-copy probe bumped
`go.mod` from v1.66.0 and found exactly one compile break in the whole module —
`internal/worker/skills.go` reading `iter.Current().Version`, a field v1.68.0 deletes — with
no transitive dependency changes. The bump cannot compile without the worker rewrite and the
rewrite has nothing to compile against without the bump, so they are atomic. The worker is
also where the reference's own alias rule lands: pass a possibly-aliased value to
`Versions.Get`, then download by the concrete id it returns.

**Decision 13 — `/content` gains the reference's `Content-Disposition`.** Every recorded
download carries `attachment; filename*=utf-8''<name>.zip`, the version's slug plus `.zip`,
RFC 5987 encoded; this platform sent no such header. It is a field-level mismatch in the exact
surface being converged and costs three lines, so it lands here rather than as a follow-up.
Nothing in the repo consumed it before — the SDK's `Download` treats the body as opaque and the
BYOC worker ignores it — so the only behavior that changes is a browser save landing a
correctly named file. The additive `x-skill-archive-sha256` digest header stays as it is.

**Decision 14 — the only-version refusal changes what "empty" means.** Because a skill can no
longer reach zero versions through the API, `skills.latest_version` is never NULL for a live
skill, and the three writers that maintain it keep their length-then-lexical numeric-max
guard unchanged.

## 4. Out of scope

- The **Files** side of the same 2026-08-27 migration (#544, `docs/DIVERGENCES.md`'s GA-shape
  entry). Nothing here touches `internal/api/files.go`.
- **Deleting a built-in skill.** The recording deliberately never issued `DELETE
  /v1/skills/xlsx`, so the reference's behavior there is unobserved and this plan does not
  guess at it.
- The **500-skills-per-session cap** reject shape, still unprobed and still tracked under #78.
- The reference's **credential rules** for `/content` (a console cookie is refused there). This
  platform has no console-cookie lane; its dual-auth read set is unchanged.

## 5. Slices

One PR, four commits, in this order — each compiles and keeps `make verify` green.

1. **SDK bump and the worker rewrite.** `go.mod` to v1.70.1; `internal/worker/skills.go`
   resolves by id through `Versions.Get`/`Versions.Download` instead of reading the deleted
   `Version` field. Contract tests for the worker's three-way resolution.
2. **The wire shapes and the create form.** `skillJSON`, `skillVersionJSON`, `parseSkillUpload`
   (`display_name`, 255-byte cap, unknown parts ignored), the list ceiling, `renderSkill`.
   A migration dropping `skills_custom_display_title_uq`.
3. **Version addressing and delete semantics.** The `{version}` slot's four accepted forms and
   the two verbatim alias-refusal messages; the importer's own regex; the skill-delete cascade
   in one transaction with per-version archive cleanup; the only-version refusal.
4. **The resolvers and the prefix.** Brain, executor and worker resolve `latest` / id / digits
   explicitly; `PrefixSkillVersion` mints `skver_` with `skillver_` accepted on input;
   CLAUDE.md's prefix list; `docs/DIVERGENCES.md`; `changelog.d/`; STATE.md.

Each slice is TDD: the contract test for the recorded behavior lands before the code, and
every new guard is mutation-tested against the pre-fix code — a regression test that never saw
the broken behavior proves nothing.

## 6. Acceptance (recorded into docs/HISTORY.md by slice 4)

- `make verify` green, coverage gate held.
- The real `ant` CLI, built from the reference checkout, drives create / list / get / version /
  download / delete against a local stack with `--base-url`, with no legacy-shape shim engaged.
- Two `anthropic-cwc-workshops` examples that upload skills — `agent-decomposition` and
  `agent-battle` — run against the local stack on a current `anthropic` release with their
  call sites moved from `display_title` to `display_name`, with no `anthropic==0.97.0` pin.
- `research-desk`, which posts `display_title` and reads `latest_version` through a raw fetch,
  is migrated to the GA field names in both its source and its `solutions/` copy.
- A skill pinned by `skver_` id in an agent's `skills[]` materializes **that** version, proven
  by a test that fails against the pre-change resolver.
