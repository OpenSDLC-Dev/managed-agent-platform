---
status: approved
issue: "#722"
---

# Bind to the SDK by symbol, date the check, and let a bump report itself (plan 51)

This repository is bound to `anthropic-sdk-go`, and that binding is the point:
wire compatibility is judged against the pinned SDK, and a claim about the
reference that cites nothing is not checkable. What is not the point is that a
pin bump reads as though it invalidates every citation at once —
`docs/DIVERGENCES.md` names an SDK version 184 times and the Go comments 54 more
— when the work a bump actually creates is proportional to what Anthropic
changed, not to how many times we cited them — and a machine's share of that is
narrower still, since a guard can see a symbol removed or renamed but not one
whose meaning moved underneath it.

The reason a bump looks that large is that a citation is **two independent
things** and one syntax carries both:

| | what it says | what a bump does to it |
| --- | --- | --- |
| **the temporal claim** | when the reference behaved this way — `since v1.63.0`, or `at the pinned v1.66.0` | `since` must not be touched; `at the pinned` must be re-checked |
| **the locator** | where to look — `betamemorystorememory.go:207-218` | the numbers move whether or not the claim did |

These are orthogonal axes, not a list of kinds: a citation is a locator plus,
usually, a temporal claim, and `docs/DIVERGENCES.md:47` fuses the two into one
clause — "At the pinned v1.70.1
**both** file shapes carry `expires_at` (`file.go:185`, `betafile.go:200`)".
Because nothing separates the axes, a bump offers two bad options: edit
everything, which turns true `since` sentences false and produces a green diff
proving nothing was re-checked; or edit nothing, which is what happened.
`internal/events/inbound.go:164` still says "at the pinned v1.66.0" while
`go.mod` pins v1.70.1, and `.claude/agents/verifier.md:25` — a file that steers
the verifier — still says "Judge against the SDK version pinned in
`go.mod` (v1.66.0)". No rung in the gate notices either.

That is the disease #452 diagnosed for `Tracked: #N` — *written once and
falsified later, elsewhere, by an event the file cannot see* — with the SDK bump
in place of the issue closure. #452's cure was to make the assertion executable.

## The convention half-exists already

This is not a scheme invented from nothing. Under #612 and #667 the registry
began reaching for it by hand. `docs/DIVERGENCES.md:60` already writes a dated
claim and a bare symbol side by side:

```
anthropic-sdk-go v1.70.1 tools/agenttoolset/skills.go:102 (the retrieve) and
:116-118 (the comment and the download by the id it returned), against v1.66.0
tools/agenttoolset/skills.go:159-182 (resolveSkillVersion, absent at the pin —
a statement about that tag, which is where it is read); internal/api/skills.go
getSkillVersion and resolveSkillVersion, …
```

Three things are worth taking from that entry. A human found that
`resolveSkillVersion` had been deleted from the reference between v1.66.0 and
v1.70.1, and recorded it — so the rot this plan attacks is real and is currently
caught by per-entry human reading, at the cost #660 is still paying. The last
clause is already a symbol anchor with no line number. And "absent at the pin" is
a third temporal form, which the table above does not name. The plan's job
is to generalise and enforce what the registry already reaches for, not to invent
a convention.

## The shape of the fix

**Name the axes separately, so a bump touches one of them.**

Three temporal forms, and a bump changes none of them:

- `since <source> vX.Y.Z` — the reference has behaved this way from that tag on.
  Twelve Go comments already carry a `since` and a version, though only two name
  the source; the rest need the source added, not the version changed.
- `checked against <source> vX.Y.Z` — a human verified this against that tag.
  Not "the pin is v1.66.0", which today can falsify, but "this was verified
  against v1.66.0", which is permanently true.
- `absent at <source> vX.Y.Z` — the thing cited is gone as of that tag. Without
  this form, `resolveSkillVersion` is either stamped `checked against v1.66.0`
  and lags forever, or re-checked against every new pin to re-discover the same
  intentional absence.

Two locator forms:

- **A symbol anchor** is the default: `betaagent.go BetaManagedAgentsWebFetchToolConfig`,
  a method qualified by its receiver (`betaagent.go BetaAgentNewParams.MarshalJSON`),
  a field by its struct (`betaenvironment.go BetaEnvironment.Description` — which
  matters, because that file declares four `Description` fields and the entry at
  `docs/DIVERGENCES.md:54` is about two of them, the response field and the
  update parameter).
- **A stamped line span is the named fallback**, not a defeat. At an immutable
  tag a line span is perfectly stable, and some citations have no single symbol
  to name: `betasessionevent.go:2931-2980` crosses nine declarations, generated
  union registration puts fifteen `init` functions in `betaagent.go` alone, and
  a claim can be about a file's shape rather than any declaration in it.

  **The fallback is earned, not chosen**, and it has to be *checkable* without
  the tag it was written against, because most stamped tags are not fetchable
  offline. So a span carries a reason from a closed set, written by the person
  migrating it: `crosses-declarations` for a range covering more than one;
  `no-unique-name` where the enclosing declaration's name repeats in the file,
  which is the generated-`init` case and not "the file is generated", since every
  file in the SDK is; `whole-file` for a claim about the file's shape; and
  `non-go` for a source with no Go declarations to name.

  ```
  checked against anthropic-sdk-go v1.66.0 — betasessionevent.go:2931-2980
  (span: crosses-declarations)
  ```

  Rung 1 checks the reason is present and from the set, which is pure syntax and
  always decidable. Where the stamped source *is* available — the pin, and
  whatever else the cache happens to hold — the guard goes further and falsifies
  the reason: a `crosses-declarations` span that resolves to one declaration is a
  finding. Without both halves, "symbols are the default" would be prose only —
  a migration could stamp all 126 coordinates, convert none, and pass every rung,
  which would make slice 4 a ceremony. The measurement below is what makes the
  rule affordable: 104 of 104 resolve, so a span is the rare case and its reason
  is worth typing.

A live citation therefore reads:

```
checked against anthropic-sdk-go v1.70.1 — betaagent.go
BetaManagedAgentsWebFetchToolConfig and BetaManagedAgentsWebSearchToolConfig
```

The stamp says when a human last looked, which is the fact #452 and #612 were
both about losing. The symbol says where to look, and unlike a line number it is
still there after the bump.

### Symbols are a workable anchor, measured rather than assumed

The standing objection is that the SDK's managed-agents files are generated
union boilerplate where no unique symbol exists to cite. The registry holds 126
named `file.go:NNN` coordinates, 122 of them distinct. Nineteen are set aside:
11 point into this repository, 5 name an `anthropic-cli` path, 2 sit in an entry
naming no SDK tag this machine's cache holds, and 1 is an SDK `examples/` file
the published module does not ship. That leaves **103 distinct coordinates**,
one of which two entries cite at different tags — so 104 (coordinate, tag) pairs
to resolve with the Go parser, each at a tag its own entry names:

| | |
| --- | --- |
| resolve to an enclosing top-level declaration | 104 / 104 |
| that declaration's name is unique in its file, receiver included | 104 / 104 |
| that name still exists at the pin, v1.70.1 | 103 / 104 |

Rows count pairs; the single failure is one coordinate, not two.

With the receiver included, not one name in the tested corpus is ambiguous —
the generated-union objection does not survive contact with the citations we
actually made. Three caveats belong with the table rather than in a footnote.
19 of the 104 resolve only because the resolver attributes a doc-comment line to
the declaration it documents; the rule is right, but a fifth of the corpus rests
on it. An entry naming several tags is resolved newest-first, which makes the
survival row trivially true for those entries. And the one name that does not
survive is `resolveSkillVersion`, which the registry already records as absent —
so the table's last row measures that a symbol anchor would have surfaced
mechanically what a human sweep in fact caught by hand, not that anything is
currently hidden.

## What the guard does, and what it deliberately does not

A new `tools/sdkref` owns the syntax of SDK references wherever they appear —
`docs/DIVERGENCES.md`, Go comments, and the steering documents that also carry
version claims. `tools/registrycheck` keeps its own job, the `Tracked:` pointers;
the two invariant families stay in separate tools because they fail for
unrelated reasons.

**Fails the gate** (offline, free, inside `make verify`):

1. **Shape.** A citation into an external source carries one of the three
   temporal forms and a locator, and a bare `file.go:NNN` with no stamp is not a
   locator. A span additionally carries a reason from the closed set above —
   always checked for shape, and falsified against the source wherever the guard
   can reach it.
2. **Resolution at the pin, with the polarity the form asks for.** For a citation
   stamped at the version `go.mod` pins, a `since` or `checked against` anchor
   must resolve and an `absent at` anchor must **not** — a negative claim that
   silently starts resolving again is as wrong as a positive one that stops.
   Only the pin is guaranteed present: the module graph contains v1.70.1 and
   nothing else, while the registry cites v1.63.0, v1.64.0 and v1.65.0, none of
   which are in this machine's module cache and none of which a cold CI runner
   would have. Restricting the failing rung to pin-stamped citations is what
   keeps the gate offline.

That restriction is narrower than it sounds. 95 of the registry's 184 version
mentions are stamped at the pin today and 89 name an older tag, so rung 2 can
fail on about half the corpus — and on the day of the next bump, on none of it,
until stamps start moving again. Rung 2 is therefore the weaker of the two
resolution rungs, and rung 3 below is what actually watches the corpus.

**Reports, and does not fail:**

3. **Every symbol anchor, resolved against the pin — whatever its stamp, in
   both polarities.** This is the rung that produces the bump report. A positive
   anchor stamped v1.66.0 that no longer resolves at v1.70.1, and an `absent at`
   anchor that has started resolving again, are the two transitions this plan
   exists to surface. It is possible offline for the anchors that live in a
   module `go.mod` pins and for bundled-spec schema paths, because the pin is the
   one tag always present; it does not apply to `anthropic-cli` anchors, to
   in-repo coordinates, or to line spans, whose numbers mean nothing at a tag
   they were not written for. Those are listed as uncheckable rather than
   silently skipped — a rung that emits nothing for input it never read is a
   clean bill of health it did not earn.

   It reports rather than fails, because at gate time a transition is not yet
   known to be a defect: a symbol may be gone precisely because the entry
   describes a version where it was. Alongside it, the lag list — stamp distance
   from the pin — as judgment rather than obligation. A gate that reddened until
   fifty claims were re-verified would recreate the very thing this plan removes:
   it converts "re-check what changed" back into "edit everything now".

`docs/REFERENCE_PROJECTS.md` already says as much, in the sentence #667 added —
a stamp "names the tag its coordinates were last checked against, not
necessarily the pin". The guard enforces that reading instead of contradicting
it.

**Three limits stated rather than papered over.** The guard detects deletion and
renaming, not semantic drift: a struct whose field changes meaning, or a constant
whose value moves, passes every rung while the enclosing symbol survives. So the
report is proportional to what Anthropic *removed*, and the lag list remains the
only prompt to look for what Anthropic *changed* — which is why lag is printed at
every bump rather than filed away. `anthropic-cli` citations take the same syntax
but only rung 1: it is a tagged local checkout, not a module dependency, so CI
cannot resolve its symbols at all. And the grammar is **scoped to citations into
a Go module `go.mod` pins — `anthropic-sdk-go`, and the three go-jose coordinates
in `internal/identity` that work the same way — plus the SDK's bundled spec and
`anthropic-cli`** — the registry also
cites unversioned public documentation by page and fetch date, dated recordings
by archive path, and makes comparative claims across three tags at once. Those
are real citations and they are not this plan's; forcing them into a
tag-and-symbol grammar would be the category error described below for steering
documents.

### The observation point a report-only rung needs

Reporting without an enforced moment to read the report is how
`.claude/agents/verifier.md:25` stayed wrong, and "we will run the report" is a
ritual, not a mechanism. So the pin gets a trigger: a workflow on any pull
request whose diff touches the `anthropic-sdk-go` line in `go.mod`, running
`make sdk-bump-report` and rendering both lists into the step summary.

It fails on **undispositioned transitions, and only those.** A transition is
dispositioned when the citation says the bump was seen: an entry whose symbol
went away takes `absent at <source> <new pin>` beside the stamp it already has,
and one whose symbol came back drops that clause. Neither edit advances the
`checked against` stamp, so neither is a claim to have re-verified anything —
which is why this is not the "edit everything now" failure in a smaller hat. A
bump that deletes twenty symbols costs twenty one-line acknowledgements, each of
them true when written, and nothing at all for the rest of the corpus. Lag stays
advisory in the summary; it never fails.

That is also what makes rung 3's report-only stance and this job's exit code
consistent rather than contradictory: at gate time a transition is unread, and
the plan will not redden a build over it; on the PR that moves the pin, it has
been read or it has not. And it keeps the promise `registry.yml` sets — that
workflow runs the pointer guard on any PR touching the registry and **preserves
its exit code**, failing on a stale pointer rather than merely printing one. A
job that passed whatever it found would enforce running a command, not reading
its answer.

## What a bump looks like afterwards

`make sdk-bump-report` prints two lists:

- **Transitions: positive anchors that no longer resolve at the new pin, and
  `absent at` anchors that resolve again.** Every one is a deletion, a rename, or
  a reinstatement in the SDK, and each needs a one-line disposition from a human.
  On the v1.66.0 → v1.70.1 bump already taken, this list would have held exactly
  one line, and named the entry that a per-entry human sweep eventually found.
  Beneath it, the anchors the rung could not check — `anthropic-cli`, in-repo,
  and line spans — named rather than omitted.
- **Claims lagging the pin.** Judgment, not obligation: an entry last checked
  four versions ago may be perfectly true, and re-stamping it without re-reading
  it would be the green-diff failure in a new costume. A stamp moves only when
  someone actually looked.

The history files, the archived plans and the changelogs carry several hundred
more SDK version mentions. A bump does not touch any of them, because each
records what was true when it was written — that is what those files are for.

## Not every citation is a Go symbol

The registry cites `anthropic-openapi.yml` in 11 named coordinates and 12 bare
continuations. No Go symbol can anchor a YAML schema, and #660 already carries
those citations as a separate open problem — v1.66.0 shipped no spec at all, so
they name a source nobody can open at the tag they give, while v1.70.1 bundles
one as `scripts/mock-spec.json.gz`. Slice 2 therefore gives spec citations their
own locator — a schema path such as `components.schemas.BetaSession` — resolved
against the bundled spec at the stamped tag, and rung 2 covers them exactly when
that tag is the pin. Anything the spec cannot answer stays a stamped line span.

## Slices

1. **`tools/sdkref` with all three rungs, and `make sdk-bump-report` as rung 3's
   two-list front end.** Its own test runs the real files inside `make verify`,
   the way `tools/registrycheck`'s does. The tool lands first and clean: it must
   tolerate the existing corpus, so rungs 1 and 2 start by reporting rather than
   failing, and the exemption is the corpus itself rather than a hand-written
   list that would become its own debt. Rung 3 reports from the start, which is
   its permanent behaviour.
2. **Migrate `docs/DIVERGENCES.md`** — 126 named Go coordinates and 76 bare
   `:NNN` continuations into Go source, across 74 of its 259 entries, plus the 23
   spec citations and two `api.md` coordinates at `docs/DIVERGENCES.md:91` that
   rot the same way. The bare continuations are #660's blocker and they disappear
   here, because a symbol needs no filename inherited from the surrounding prose.
   Closes #660.
3. **Migrate the Go comments, then the steering documents under a separate
   rule.** 50 lines carry a coordinate — 49 citations (40 into the SDK, 3 into
   go-jose, 6 into this repository) and one fixture path that is not one — and 12
   carry a `since` whose source is usually unnamed. The comments are a pure
   migration. The steering documents are not: `.claude/agents/verifier.md:25` is
   an *operational instruction* to judge against the current pin, not evidence
   dated to a tag, so rewriting it as `checked against v1.66.0` would be a
   category error. Its rule is narrow — no literal SDK version beside "pinned in
   `go.mod`" — and applying it does change what the verifier is told to do, which
   is the point (#724 corrects that instance now; this slice keeps it from
   recurring).
4. **Turn on failing.** Rungs 1 and 2 become errors, the corpus tolerance goes
   away, the `go.mod`-triggered workflow lands, and the bump ritual is written
   into `docs/REFERENCE_PROJECTS.md`.

Slices 2 and 3 are independent and can land in either order once 1 is in.

## Non-goals

- **Not a re-verification pass.** Migrating a citation to a symbol anchor does
  not re-check the claim, and the stamp keeps whatever version it already names.
  Moving a stamp forward without reading the code is exactly what this plan is
  designed to prevent, and doing it wholesale here would poison the corpus
  before the guard exists to protect it.
- **Not semantic drift detection.** Comparing a symbol's *content* — a stored
  normalised-AST digest checked against the pinned source — would catch the
  changes the resolution rungs miss, and would work offline, since only the
  digest and the current pin are needed. It is out of scope because it is a
  larger design than this one: choosing a normalisation that survives generated-
  code churn, storing a digest per citation, and giving a human a way to accept a
  diff. The gap is named here so the next plan can take it, not because the
  offline constraint forbids it.
- **Not a change to what is cited.** Which claims the registry makes, and which
  divergences it records, are untouched.
- **Not a rule for in-repo coordinates.** Eleven registry coordinates and six Go
  comments cite this repository's own source by line; git holds our history, so
  they take a symbol with no stamp. No rung enforces that — rung 1 reads
  citations into an external source, and rungs 2 and 3 are explicitly excluded —
  so it stays a convention a reviewer applies, not a check. Guarding our own
  coordinates is a separate job with a separate answer, and this plan does not
  pretend to have done it.
- **No automatic re-stamping.** There is no tool that advances a `checked
  against` version, by design.

## Decisions

1. **Present-tense claims become past-tense, rather than dropping the version or
   being pinned to `go.mod` by a guard.** Dropping it loses when-checked, which
   #452 and #612 both show is unrecoverable once gone. Guarding it against
   `go.mod` makes every bump produce a mandatory mechanical diff whose green
   light falsely reads as "re-verified".
2. **A line span stays legal as a stamped fallback, but only where a symbol
   does not resolve.** Banning spans outright would force a false symbol onto
   multi-declaration spans, generated `init`, and spec citations; allowing them
   freely would let the migration stamp everything and convert nothing. What was
   wrong with a coordinate was never the coordinate — it was the missing stamp
   that let it be read as current — so the stamp is mandatory and the span is
   conditional.
3. **The failing rung resolves only at the pin.** Resolving at arbitrary stamped
   tags would need those modules, and the registry cites three tags no cache
   here holds. Older tags are reported when available and never required.
4. **Both the registry and the Go comments migrate**, and the steering documents
   after them, under their own rule. Two citation conventions in one repository
   is a worse cost than one migration. Steering documents earn their place
   because measuring the corpus turned up a live falsehood there
   (`.claude/agents/verifier.md:25`) as well as the one already known in a Go
   comment (`internal/events/inbound.go:164`) — the rot is not confined to the
   two obvious surfaces.
5. **This plan is sliced rather than landing as one PR.** The corpus is 276
   coordinates — 139 named and 88 bare continuations in the registry, 49 in Go
   comments — across three surfaces plus a new tool, and the evidence for
   slicing is recent: a three-entry registry change landed with a tail of Go
   comments that cited the entries it changed.
