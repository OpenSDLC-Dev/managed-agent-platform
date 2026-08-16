---
status: archived
issue: "#226"
---

# Spill oversized tool output to a sandbox file

Archived: completed — implemented by the PR that landed this file (a
single-PR plan, the plan-16 precedent; the delivery record is the CHANGELOG
entry and docs/HISTORY.md).

## Problem

The reference writes tool output past ~100k characters to a file in the
sandbox and hands the model a truncated preview plus the file path — publicly
documented ([#226](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/226)
quotes the tools-doc sentence). We truncate at `toolset.MaxOutputBytes`
(100 KiB) with a trailer saying so; the truncated tail is gone. For sandbox
tools the tail is *irreplaceable* — the command ran once, the giant grep hit
what it hit — which is exactly where the reference's spill earns its keep.

## Decisions

1. **The five eligible sandbox tools spill (`read` excepted — decision 8);
   the web tools keep truncating.** The sandbox tools always have a sandbox
   by construction (the `toolset.Runner` holds it), so the spill is one
   `Sandbox.WriteFile` call. The web tools
   run in the executor with **no sandbox provisioned** — plan 15's deliberate
   design ("no sandbox and no gate") — and provisioning one solely to store a
   spilled answer would couple the web pass back into sandbox lifecycle for
   marginal value: web content, unlike a command's output, is *re-fetchable*
   (the model can fetch again, narrower). `web_fetch` keeps the truncation
   trailer; `web_search` keeps its budget pruning. Recorded as the deliberate
   half of the DIVERGENCES entry (revisitable if a real need appears).
2. **Spill at the two existing capping points, sanitized bytes, full body.**
   The generic dispatch arm (every tool whose content reaches dispatch
   uncapped, bash's success arm included — except `read`, decision 8) and
   bash's two `capWithTrailer` failure arms (which cap before dispatch so
   the exit-code/timeout trailer survives — the spill must hook before that
   cap or the tail is already gone). The spilled file holds the complete
   NUL-sanitized output the toolset received — for an exec-driven tool, what
   `Sandbox.Exec` retained and returned under its pre-existing 1 MiB
   per-stream memory guard; the preview is the same head the truncation
   keeps today.
3. **Threshold = the existing cap.** The spill fires when the body alone
   exceeds `MaxOutputBytes` — the same boundary that triggers truncation
   today and the same order of magnitude as the reference's documented
   100,000 characters. (Edge, accepted: a bash body within the cap whose
   status trailer pushes the join over it still truncates without spilling —
   the loss is bounded by the trailer plus the truncation notice, a few
   dozen bytes.)
4. **Path convention (ours, INFERRED):** `/tmp/tool_outputs/<tool_use_id>.txt`
   — outside the workdir so project-relative `glob`/`grep` never match spill
   files, keyed by the tool-use event id (unique per call). The tail is
   reached with `read` + `view_range` or bash slicing (`head -c`/`tail -c`/
   `sed`); a whole-file `read` of a spill file truncates plainly and never
   re-spills (decision 8). The reference documents the spill but neither its
   path nor its preview shape; both are recorded as INFERRED.
5. **Preview shape (ours, INFERRED):** the truncation notice extended to name
   the file — `[output truncated; full output written to
   /tmp/tool_outputs/<id>.txt]` — in place of today's `[output truncated]`,
   for the generic arm and bash's trailer arms alike.
6. **A failed spill write falls back to plain truncation.** The write runs
   under the tool's own context; if it errors the result caps exactly as
   today — the spill is an enhancement, never a new failure mode for the
   call.
7. **The BYOC worker inherits the behavior for free** — the spill lives in
   the shared `toolset.Runner`, which both the cloud executor and the worker
   drive, writing into whichever sandbox ran the tool.
8. **`read` never spills** (a review-round decision — both reviewers caught
   the chain the first cut invited). A read's full content already sits in
   the sandbox at the very path the model just named, so a spill would only
   copy a file to a second file — and because every spill file exceeds the
   cap by construction, reading one back would mint another copy under a
   fresh id on every attempt: a chain with no fixed point, burning turns and
   sandbox disk while the changing path disguises the lack of progress. An
   oversized read truncates plainly instead. Accepted residual: a spill file
   holding one giant line has no line breaks for `view_range` to cut, so
   only bash slicing reaches its tail — bash can be disabled per-agent, in
   which case that tail is unreachable; the pre-change behavior lost it
   outright, so this is still strictly better.

## Tasks

1. `internal/toolset/toolset.go`: `Runner.spill(ctx, id, full)` (WriteFile
   to the spill path; "" on fit-or-failure) + the dispatch arm using it;
   `capWithTrailer` takes the notice so bash's arms can carry the spill path.
   `internal/toolset/bash.go`: the two failure arms spill before capping.
   TDD with the package's fakeSandbox: spill content + path, no-spill under
   the cap, write-failure fallback, bash trailer survival — mutation-checked.
2. Docs in the same PR: CHANGELOG entry; the DIVERGENCES truncation entry
   rewritten (sandbox tools now spill — path/preview INFERRED; web answers
   deliberately keep truncation/pruning); ARCHITECTURE `toolset.go` row;
   HISTORY progress summary; STATE.md ticks #226 and closes the four-issue
   batch.
