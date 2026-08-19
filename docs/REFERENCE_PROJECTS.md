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
lands next). Wire-compat is judged against the SDK version pinned in `go.mod` (v1.63.1);
new surface in a checkout is not an invitation to build ahead of the backlog, and pinned
surface the platform deliberately leaves unbuilt — memory stores, the advisor, budgets,
threads until plan 35 lands them — is registered in docs/DIVERGENCES.md rather than built.
