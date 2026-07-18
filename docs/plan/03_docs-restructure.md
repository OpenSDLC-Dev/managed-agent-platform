---
status: in-progress
---

# Docs restructure: ARCHITECTURE.md, HISTORY slimming, STATE.md as work tracker, issue triage

## Context

Three pressures, surfaced while reviewing the doc system after the plan-management
convention (plan, in a sense, 02.5 — PR #100) landed:

1. **Duplication.** Every PR writes its story twice — a rich CHANGELOG entry and a
   HISTORY.md narrative — and the two have drifted into ~70% overlap. Meanwhile HISTORY
   holds three kinds of content that are *not* change records at all: 56 rows of
   per-package reference tables (current-state "what lives where"), acceptance-run
   records, and decisions-evaluated-and-rejected.
2. **No architecture document.** The system's as-built description is scattered across
   CLAUDE.md (compressed), HISTORY.md (per-package tables), and the archived plan 01
   (as-designed, not as-built). A contributor has no single place to read how the
   platform actually works.
3. **STATE.md still carries static content.** Its snapshot, "Where things live", and
   environment notes change rarely and duplicate other homes; what a resuming session
   actually needs is: what is being worked on, and how far along it is.

## Workstreams

### WS1 — `docs/ARCHITECTURE.md` (new)

The as-built architecture reference, consolidating:

- CLAUDE.md's architecture depth (three-piece decoupling, process topology, async
  execution flow, repo layout commentary) — **the behavioral guardrails stay in
  CLAUDE.md in compressed form** (non-negotiable design principles, wire-compatibility
  rules); ARCHITECTURE.md carries the descriptive depth they compress.
- HISTORY.md's per-package reference tables, migrated with a freshness pass (every
  referenced file verified to exist; stale claims corrected against the code).
- The system-overview content STATE.md's snapshot used to carry (release status stays
  in CHANGELOG).

Structure: overview → process topology → execution flow (incl. permission/HITL and
BYOC) → wire-compatibility model (pointing at DIVERGENCES.md) → package reference →
security invariants → observability → testing architecture (tiers, contract suites,
evals).

### WS2 — HISTORY.md slimming + a one-writer rule

- **Move**: the per-package tables → ARCHITECTURE.md.
- **Prune**: per-slice delivery narratives whose substance CHANGELOG already carries —
  each section verified against CHANGELOG before deletion; content found nowhere else
  is kept or rehomed. Git history is the backstop.
- **Keep**: the delivery-slices table, acceptance-run records (slice 8), decisions
  evaluated and rejected, and archived-plan summaries (the eval phase-1 section — plan
  02 links to it).
- **The rule going forward** (written into both file headers, CLAUDE.md, AGENTS.md, and
  the verifier's docs-consistency rung): a change's narrative is written **once**, in
  CHANGELOG.md; HISTORY.md receives only what a changelog structurally cannot hold —
  acceptance/verification run records, decisions evaluated and rejected, and archived
  plans' progress summaries.

### WS3 — STATE.md as a pure active-work tracker

STATE.md reduces to two sections: **Active work** (the current docs/plan file or GitHub
issue, one line of goal) and **Tasks** (the checklist decomposed from it, with progress
and evidence links). Everything else moves out: the snapshot's system description →
ARCHITECTURE.md; release status → CHANGELOG; "Where things live" → a short index in
CLAUDE.md; environment notes → CLAUDE.md's Development section (deduplicated — half of
them are already there). New size budget: ~30 lines. CLAUDE.md's STATE description,
AGENTS.md's mirror, and the verifier's rung 5 update in the same PR (STATE checks
become: active work and task progress agree with reality).

### WS4 — `issue-triage` subagent

`.claude/agents/issue-triage.md`: read-only tools (Bash, Read, Grep, Glob), **pinned to
Sonnet 5** (`model: claude-sonnet-5`). Given an issue number, it reads the issue
(`gh issue view`) and surveys the affected code, then returns **strict JSON only** —
`{issue, needs_plan, complexity, reasoning, plan_scope_suggestion?, direct_tasks?,
dependencies?, open_questions?}`. Its judgment criteria: multi-PR scope, architectural
decisions, ambiguity needing user input, or wire-schema verification → `needs_plan`;
single-PR mechanical work → `direct_tasks`.

Scope limits (deliberate): it is dispatched **only when work starts from a GitHub
issue**, and it returns **judgment only** — drafting a plan, or turning `direct_tasks`
into STATE.md's task list, stays with the main agent. CLAUDE.md's workflow gains the
trigger rule.

## PR slicing

- **PR A** — WS1 + WS2 (coupled: the tables move between the same two files), this plan
  file landing `in-progress`, STATE.md's Active plan section tracking it.
- **PR B** — WS3 (depends on A: the snapshot's destinations must exist first).
- **PR C** — WS4 (independent; may run parallel to B).

Every PR takes the full ritual (verifier + dual review) — each touches behavior-steering
markdown (CLAUDE.md / AGENTS.md / `.claude/`).

## Verification

- PR A: `make verify` green; every relative link in touched files resolves; no content
  deleted from HISTORY without verified CHANGELOG coverage (the prune report is the
  evidence); every file path in ARCHITECTURE's package reference exists; CLAUDE.md still
  carries the guardrails verbatim-or-compressed (verifier re-derives against them).
- PR B: STATE.md ≤ ~30 lines; the removed content demonstrably lives at its new homes;
  verifier's STATE checks updated in the same PR.
- PR C: the agent file parses (frontmatter, model pin); a dry-run dispatch on a real
  issue returns valid JSON matching the schema; CLAUDE.md trigger rule consistent with
  the agent's own scope statement.
