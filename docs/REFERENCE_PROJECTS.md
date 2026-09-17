# Reference projects

Read-only local reference sources used as ground truth and design reference. One line per
project — `<github-url>, <relative-local-path>` (paths relative to this repo's root):

```
https://github.com/anthropics/anthropic-sdk-go, ../../anthropic-sdk-go
https://github.com/anthropics/anthropic-cli, ../../anthropic-cli
https://github.com/anthropics/claude-code, ../../claude-code-source
https://github.com/google/adk-go, ../../adk-go
https://github.com/deepseek-ai/deepseek-harness, ../../deepseek-harness
https://github.com/openai/codex, ../../codex
```

## Roles and authority order

For wire-schema questions, resolve in this order — never guess a wire shape:

1. **Public docs.**
2. **`anthropic-sdk-go`** — the typed wire schema for everything managed-agents:
   `betasessionevent.go` (full event taxonomy, both directions), `betaagent.go` /
   `betaenvironment.go` / `betasession.go` (resources), `betaenvironmentwork.go` (work
   API). Also `tools/agenttoolset` — the reference host-side toolset the `ant` worker
   runs (`pkg/cmd/worker.go` hands it to the SDK's `EnvironmentWorker`), which makes it
   the behavior-and-wording authority for `agent_toolset_20260401` tools: every tool
   failure there is a model-visible `is_error` tool_result (never an infrastructure
   fault), worded by `fsErrorMessage`'s four-entry normalization table with raw-text
   passthrough — the basis of [plan 23](./plan/23_classified-unwritable-write.md). Also
   this repo's primary dependency.
3. **`anthropic-cli`** — the real `ant` CLI source; client-side behavior (polling,
   SSE/stream handling, defaults, headers): `pkg/cmd/beta*.go`, `pkg/cmd/worker.go`.
4. **Recording a real `ant` CLI stream** — for behavior the types can't capture
   (ordering, SSE framing, defaults).

`claude-code-source` is a **harness design reference only** (agent loop, tool
orchestration, permission flow) — never a wire-schema source; never copy code from it.
Provenance caveat: unlike the others it is not a git checkout — it is a local source
snapshot with no git remote; the URL above is the upstream project it corresponds to, not
where the snapshot was cloned from.
`adk-go` is a source of **ideas only**, governed by CLAUDE.md design principle 2 — never a
foundation; where it conflicts with the Anthropic model, it loses by rule.
`deepseek-harness` (a TypeScript monorepo) and `openai/codex` (a Rust workspace under
`codex-rs/`) are **harness design references**, like `claude-code-source` — agent loop,
tool orchestration, permission flow, child-agent lifecycle and message passing, whatever
the work at hand needs: never a wire-schema source, never copy code from them, and where
either conflicts with the Anthropic model, the Anthropic model wins.

## Caveats

The SDK and CLI checkouts track the API's tip and can run ahead of the pin (whatever
lands next). Wire-compat is judged against the SDK version pinned in `go.mod` —
and because the pin moves, **a registry citation's tag is the one its form names — the tag
its anchor was checked against, arrived at (`since`) or was missing at (`absent at`) — not
necessarily the pin**: read the anchor at that tag, with `git show <tag>:<path>` or the
module cache, never in a working tree that has moved on;
new surface in a checkout is not an invitation to build ahead of the backlog, and pinned
surface the platform deliberately leaves unbuilt — memory stores, the advisor, budgets —
is registered in docs/DIVERGENCES.md rather than built. (Session threads were on that
list until plan 35 built them.)

## Bumping a pin

A pull request that moves a pin in `go.mod` — the SDK's or go-jose's — or edits a citation
runs `make sdk-bump-report` through [`sdk-bump.yml`](../.github/workflows/sdk-bump.yml), and
fails while a **transition** awaits a disposition: an anchor the pin no longer holds, or an
`absent at` anchor it holds again. Each is a deletion, a rename or a reinstatement in the
reference, and the report names the line that dispositions it — except for an anchor
stamped at the pin, which no bump has passed, and whose claim is simply wrong
([plan 51](./plan/51_sdk-reference-binding.md)).

1. **An anchor gone at the pin** takes `absent at <source> <pin> — <file> <what went>` in
   the same registry line or comment paragraph, if the claim held at its stamp; each entry
   citing the symbol takes its own. Check the name is spelt right first: the tool never
   opens the stamp's tag, so a misspelt name reads as gone as surely as a deleted one. An
   earlier tag after the stamp is accepted too, for someone who read when the symbol went —
   name one only if you read it, since nothing checks a tag before the pin. Under a
   pre-release or pseudo-version pin, which the grammar cannot name, write the release it
   precedes. If the entry's argument rested on what went, the disposition is not enough:
   rewrite the entry, and cite what replaced it at the tag you read.
2. **An `absent at` anchor resolving again** drops that clause, and the claim it was part
   of is re-read.
3. **Neither moves a `since` or `checked against` stamp.** A stamp moves only when someone
   has re-read the claim at the new tag. The guard sees what the reference deleted or
   renamed, not what it changed underneath a surviving name, so the report's list of stamps
   behind the pin is the prompt to look for the rest — judgment, never a failure.

A citation stamped at the pin is held by the gate as any other: a positive anchor must
resolve there, and an `absent at` anchor must not.
