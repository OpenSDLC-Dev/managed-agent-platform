# Reference projects

Read-only local checkouts used as ground truth and design reference. One line per
project — `<github-url>, <relative-local-path>` (paths relative to this repo's root):

```
https://github.com/anthropics/anthropic-sdk-go, ../../anthropic-sdk-go
https://github.com/anthropics/anthropic-cli, ../../anthropic-cli
https://github.com/anthropics/claude-code, ../../claude-code-source
https://github.com/google/adk-go, ../../adk-go
```

## Roles and authority order

For wire-schema questions, resolve in this order — never guess a wire shape:

1. **Public docs.**
2. **`anthropic-sdk-go`** — the typed wire schema for everything managed-agents:
   `betasessionevent.go` (full event taxonomy, both directions), `betaagent.go` /
   `betaenvironment.go` / `betasession.go` (resources), `betaenvironmentwork.go` (work
   API). Also this repo's primary dependency.
3. **`anthropic-cli`** — the real `ant` CLI source; client-side behavior (polling,
   SSE/stream handling, defaults, headers): `pkg/cmd/beta*.go`, `pkg/cmd/worker.go`.
4. **Recording a real `ant` CLI stream** — for behavior the types can't capture
   (ordering, SSE framing, defaults).

`claude-code-source` is a **harness design reference only** (agent loop, tool
orchestration, permission flow) — never a wire-schema source; never copy code from it.
`adk-go` is a source of **ideas only**, governed by CLAUDE.md design principle 2 — never a
foundation; where it conflicts with the Anthropic model, it loses by rule.

## Caveats

The checkouts track the API's tip and contain post-plan surface (`agent.thread_*` events,
memory-store betas). Wire-compat is judged against the SDK version pinned in `go.mod`
(v1.56.0); new surface in a checkout is not an invitation to build ahead of the backlog.
