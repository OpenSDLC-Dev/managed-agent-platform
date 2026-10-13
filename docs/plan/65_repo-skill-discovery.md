---
status: in-progress
issue: "#921"
---

# Skills discovered in a mounted repository (plan 65)

The reference loads skills from a mounted `github_repository`'s root
`.claude/skills` directory and announces them to the agent (#921). This
platform announces only the agent's own `skills[]`, and in a prose template
where the reference uses XML (#581). The owner decided on 2026-10-14:

- **discover before the first model request**, as the reference does: the
  sandbox is provisioned, the repositories cloned and their skills scanned
  before the brain's first request in a session that mounts one;
- **adopt the recorded XML template for both skills blocks**, closing #581 in
  this plan;
- **discover nested skills on read** as well: a `read` of a file inside a
  repository's subdirectory announces the skills on its path;
- **trust as the reference does**: a mounted repository's skills load without
  a warning or a consent step, and the operator guide says so.

## Ground truth

The docs (`managed-agents/skills`, "Load skills from a GitHub repository"):

- at session start the platform scans the repository's root `.claude/skills`
  and finds skills at exactly `.claude/skills/<skill-name>/SKILL.md`, one
  level deep;
- each is available to the agent with no upload and no `skills[]` entry; the
  agent sees its name, description and path;
- discovery follows the checked-out state and runs once per session;
- it needs the agent's `read` tool;
- a repository skill that shares a name with another skill replaces nothing:
  both are announced, each with its own path;
- cloud sandboxes only. This platform already refuses `github_repository` on
  `self_hosted` (#322).

The recordings, as the agent quoted its own prompt (agent-reported text; the
API exposes no raw prompt):

- **The attached block** (2026-09-03 batch2, `session.events.list.skill-prompt`):
  a `**Skills usage policy**` lead, a `<system-reminder>` line, the sentence
  "Skills are available under `/workspace/skills/`.", then
  `<available_skills>` with one
  `<skill><name>…</name><description>…</description><location>…</location></skill>`
  per skill. Descriptions are XML-escaped (`&#34;`).
- **The repository block** (2026-10-13, managed-agents-wire-recordings#23,
  `setup[234–239]`): `<available_skills source="repo">` with the same entries,
  located in the mount (`/workspace/<repo>/.claude/skills/<name>/SKILL.md`).
  - A skill without frontmatter, or without a description, is listed with an
    empty `<description></description>`.
  - A lowercase `skill.md` is not found.
  - Same-named attached and repository skills are both listed.
  - The location follows the rooted mount (`/tmp` → `/workspace/tmp/...`).
- **Nested skills** (`setup[41–50]`): a skill under
  `packages/x/.claude/skills/` was announced only after the agent read
  `packages/x/probe.txt`. The agent described it as a separate user message
  carrying another `<available_skills source="repo">` block; that message is
  not in the events.
- **`read` disabled** (`setup[56–59]`, `[240–245]`): no repository skill was
  announced.
- **No trace**: no event and no session or resource field names a discovered
  skill (`setup[36–37]`, `[51–52]`).

## Decisions

1. **The attached block takes the recorded template** (#581). The XML list
   renders as recorded. Names, descriptions and locations are escaped as
   `html.EscapeString` does, which writes `"` as `&#34;`, as recorded.
   - The lead gives the recorded lead's directions and its reminder in our own
     words, not its text, then the "installed under" sentence. This is ours,
     registered.
   - A cloud session's location is absolute: `<workdir>/skills/<dir>/SKILL.md`.
     The brain reads the workdir from `EXECUTOR_WORKDIR`, which Helm now sets
     on the brain from the same `executor.sandboxWorkdir` value.
   - A self-hosted session's location stays relative, `skills/<dir>/SKILL.md`,
     and the sentence names `skills/`: the worker's workdir is the customer's
     (`WORKER_WORKDIR`) and the brain cannot know it. This is ours, registered.

2. **Discovery runs before the first request, as a run with no tool.** Before
   building a request, the brain suspends the turn when all of these hold:
   the environment is cloud, the session mounts a repository, the thread's
   agent has `read` enabled, and the session has no scan record. The
   suspension is MCP discovery's (`suspendForDiscovery`): in one commit it
   completes the model turn and enqueues the session's `tool_exec`. It writes
   no event, and the session stays `running`.
   - The executor runs that item as any run: it provisions, installs packages,
     materializes repositories, skills, files and memory, finds no tool to run,
     and commits. `settleDrain` then wakes the thread.
   - After the repositories land, the executor scans each one whose checkout
     is present, and writes a scan record and the skills found, once per
     session. A later re-clone does not scan again: the reference scans once.
   - A repository whose clone failed contributes no skill; the repositories
     block already marks it unavailable. The scan record is still written, so
     the brain does not wait again.
   - A sandbox that cannot provision holds the first turn, retried each lease
     period, as it already holds a tool call.

3. **What a scan finds.** One sandbox command per repository lists
   `<mount>/.claude/skills/*/SKILL.md`, matching the filename exactly and one
   level deep, and prints each file's frontmatter. The executor then applies
   three rules:
   - **Name:** the directory's name. A file with no frontmatter was listed under
     its directory's name. Whether a frontmatter `name` would win is
     unrecorded, so this is ours.
   - **Description:** the frontmatter `description`, or empty when the
     frontmatter is missing, invalid or has no description.
   - **Bounds** (both ours): at most 500 repository skills a session, the
     attached cap; a description cut at 1,024 bytes, the upload cap.

4. **The repository block** follows the attached block in the system prompt,
   in the thread whose agent has `read` enabled.
   - Entries are ordered by resource, then by name, as recorded within one
     repository.
   - Same-named skills are all listed.
   - With no attached skill, the lead and the reminder precede the repository
     block without the "available under" sentence. The reference's text there
     is unrecorded, so this is ours.
   - The block is rendered from Postgres on every request, as the other blocks
     are, so a replayed brain builds the same one. It is present from the
     first request, so it never moves the prompt prefix that kept thinking is
     keyed on.

5. **Nested skills are discovered on `read`.** When the executor runs a `read`
   of a file below a repository's root, it scans each directory between the
   file's and the root, excluding the root, for `.claude/skills/*/SKILL.md`.
   - It records each skill not already recorded, keyed by that read's
     `tool_use` id, in the commit that writes the read's result.
   - When building history, the brain appends one text block after that
     result, in the same user turn: an `<available_skills source="repo">`
     block of those skills. It renders the block from Postgres, so replay
     rebuilds it, and it writes no event.
   - Only the thread whose read found the skills sees them.
   - The reference's mechanism is the agent's account, so the placement and
     the text are inferred and registered.

6. **No API surface.** No event, field or endpoint carries a discovered skill,
   as recorded.

7. **Trust.** A repository's skills are instructions anyone who can push to
   its branch can change. The reference loads them without a warning, and so
   does this platform; `docs/self-hosted-security.md` says that mounting a
   repository trusts its skills.

## Slices

Each slice is one PR, each through the full review ritual.

1. **The template** (#581): decision 1, with Helm's brain workdir wiring and
   the registry entry. Closes #581.
2. **Root discovery**: decisions 2, 3, 4, 6 and 7, with a migration for the
   scan record and the skills.
3. **Nested discovery**: decision 5. It archives this plan and closes #921.

## Not settled here

What the recordings leave open is ours and registered, and
managed-agents-wire-recordings#26 asks for it:

- the lead text when only repository skills exist;
- the name a frontmatter `name` would give;
- the reference's bounds on skills and descriptions;
- the nested announcement's exact text.
