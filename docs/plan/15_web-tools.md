---
status: in-progress
issue: "#47"
---

# web_fetch + web_search built-in tools (plan 15)

This plan lifts **#47** out of its deferral: the last two `agent_toolset_20260401` tools,
executed **in the executor process on both deployment modes** (cloud *and* self_hosted),
backed by **Tavily** (search) and **Jina Reader** (fetch). Enabling `web_fetch` or
`web_search` stops resolving to nothing; the model gets the tool, the executor runs it,
and the result lands in the event log as the wire's typed blocks.

Deliberately **not** in this plan, each with its own tracker or a follow-up to file:

- **MCP toolset** (#45): the executor-executes-platform-tools routing this plan builds is
  the same spine #45's `tools/call` will ride, but the MCP client itself stays #45's.
- **Tool-level domain allowlisting**: the reference has per-tool "allowed domains" for the
  web tools (its environments doc names them; no wire field configures them — see Ground
  truth). v1 ships without a filter; a follow-up issue will add an operator-side allowlist
  config. Its INFERRED registry entry lands with slice 4.
- **The 100k-character spill-to-sandbox-file behavior**: the reference writes oversized
  tool output to a sandbox file and hands the model a preview + path. That couples web
  results back into the sandbox; we keep today's `capOutput` truncation, and slice 4
  records the divergence. Applies to all tools, so it is its own issue, not a web-tools
  rider.
- **Delegating to model endpoints that support server-side web tools**: rejected for v1 —
  design principle 4 requires any Anthropic-protocol endpoint to work, so a
  server-tool passthrough could only ever be an optimization, and it would bypass the
  permission-policy machinery besides.

## Ground truth (verified 2026-07-30/31)

Resolved per CLAUDE.md's order — public docs first, then the SDK checkout's typed schema;
`claude-code-source` consulted as **harness design reference only** (its shapes are
Claude-Code-internal and were not copied; see below).

- **Public docs.** The environments page (platform.claude.com → managed-agents →
  environments, Networking): *"The `networking` field controls the sandbox's outbound
  network access. It does not affect the allowed domains for the `web_search` or
  `web_fetch` tools."* The self-hosted-sandboxes page mentions the web tools **zero**
  times and its worker helper *"returns the standard tool implementations (bash, read,
  write, edit, glob, grep)"* — six. The MCP connector page's analog is explicit about the
  locus of platform-side tools: it *"connects to MCP servers from Anthropic's side."*
- **SDK (anthropic-sdk-go, pinned v1.59.0).** The tool-config enum names all eight tools
  and carries `permission_policy` for each (`betaagent.go` `BetaManagedAgentsAgentToolConfig`);
  the client toolset helper `tools/agenttoolset` implements **six** ("returns the six
  built-in agent_toolset_20260401 implementations") — the official worker cannot run web
  tools. `betasessionevent.go` contains **no** `web_search_tool_result` /
  `web_fetch_tool_result` / `server_tool_use` types: the Messages API's server-tool
  blocks do not appear in the managed-agents event schema. The `agent.tool_result`
  content union is closed over four variants — `text | image | document | search_result`
  — and `BetaManagedAgentsSearchResultBlock` is documented as *"A block containing a web
  search result"*: `{type: "search_result", title, source /* URL */, content: []text,
  citations}`.
- **claude-code-source (design reference only).** Its `WebSearchTool` wraps the Messages
  API server tool (`web_search_20250305`) in a sub-model call and flattens the response
  into an internal zod shape; its `WebFetchTool` output is `{bytes, code, codeText,
  result, durationMs, url}` with `result` post-processed by a sub-model. Both shapes are
  process-internal to another product — evidence for *why* claude-code-source is never a
  wire source, and a design mine for fetch behavior (redirect handling, HTML→markdown,
  HTTPS upgrade) and tool descriptions.

What only a real `ant` recording can settle (a paid managed-agents account; if
impractical, the INFERRED entries below carry the assumptions):

1. Whether `web_fetch` results ride `text` or `document` blocks (this plan assumes `text`).
2. Mixed-turn ordering on self_hosted: how the reference keeps an unanswered web
   `agent.tool_use` invisible to a polling worker (this plan holds the worker's work item
   back — see Routing).
3. The model-facing input schemas (assembled server-side in the reference, unobservable
   from the SDK; ours below are minimal and recorded INFERRED).

## The decision: executor process, both modes

Web tools execute **in the executor's own process** — never in the sandbox, never on the
BYOC worker, never through the per-session egress gate. Four load-bearing reasons:

1. **Wire-compat is a hard constraint, not a preference.** The official worker implements
   six tools. Routing web calls into the self_hosted work queue would hand the real
   `ant beta:worker` calls it cannot run — sessions that work against the reference would
   degrade to error results against us. Platform execution keeps official tooling
   working unchanged, and keeps web results in the `agent.tool_result` family the
   reference (by inference) uses, not `user.tool_result`.
2. **The reference's locus is platform-side, and our platform side is inside the customer
   boundary.** The reference executes web tools from Anthropic's network — outside its
   customers' boundary. Our entire platform deploys in the customer's VPC, so
   executor-side execution keeps the self-hosting promise the reference cannot make.
   Deliberate divergence (better, and forced by our deployment model); DIVERGENCES entry.
3. **Brain-vs-executor is wire-invisible, so our own principles decide — and they pick
   the executor.** `permission_policy` is wire-legal on web tools (`always_ask` included),
   which requires the suspend → `user.tool_confirmation` → resume pipeline that lives on
   the queue+executor spine; the lease-keeper/reclaim machinery for long calls lives
   there too; and "the brain only produces intent" stays intact.
4. **The environment's networking policy must NOT constrain web tools** (the docs
   sentence above). Executor-side execution bypasses the session gate structurally;
   sandbox-side execution would wrongly subject web tools to `limited`/`allowed_hosts`.

## Wire shapes

- **Definitions offered to the model** (new entries in `internal/toolset/definitions.go`):
  `web_fetch {url: string (required)}`, `web_search {query: string (required)}`. Minimal
  by design; INFERRED (the reference's model-facing schemas are unobservable).
  Descriptions authored fresh with claude-code-source as design input only.
- **Results**: `web_search` → one `search_result` block per hit (`title`, `source` URL,
  `content` as text blocks, `citations`), matching `BetaManagedAgentsSearchResultBlock`
  field-for-field; `web_fetch` → a `text` block carrying the fetched content as markdown.
- **Config**: the enum names are already accepted; `enabled` now resolves into the
  definitions; `permission_policy` defaults to the toolset's `always_allow` and an
  `always_ask` flows through the existing HITL machinery unchanged (web tools are
  ordinary `agent.tool_use` events).

## Backends: Tavily + Jina behind a seam

New package `internal/webtool`: `Searcher` and `Fetcher` interfaces, `tavily/` and
`jina/` adapters, and a shared contract suite per house convention (every future backend
passes the same suite). Config is env-driven and never hard-codes an endpoint
(self-hosting: enterprises proxy):

| Env | Meaning | Default |
|---|---|---|
| `TAVILY_API_KEY` | Tavily key for `web_search` | — (unset ⇒ backend unconfigured) |
| `JINA_API_KEY` | Jina Reader key for `web_fetch` | — (unset ⇒ backend unconfigured) |
| `WEBSEARCH_BASE_URL` | Tavily-protocol endpoint | `https://api.tavily.com` |
| `WEBFETCH_BASE_URL` | Jina-Reader-protocol endpoint | `https://r.jina.ai` |

`.env` (gitignored, already in `.worktreeinclude`) carries the two keys for local runs
and the live test tier. **An enabled tool whose backend is unconfigured returns an
`is_error` tool result naming the missing variable** — a deployment misconfiguration
surfaces where an operator can see it. (Considered and rejected: hiding the tool from
the definitions when the key is absent — the brain and executor are separate processes,
and availability decided by one process's env silently masking the other's misconfig is
exactly the drift that rule would create.)

Live-tier tests follow the `modeltest` contract: `RUN_LIVE_WEB_TESTS=1` is consent,
`.env` supplies the keys, and consent without configuration **fails** rather than skips.
`make test` never spends money; the default suites run against stub servers.

## Routing: a platform-executed work kind

- **Queue**: a new internal kind (`web_exec`) alongside `model_turn`/`tool_exec` — the
  kind taxonomy is internal, not wire. `web_exec` is claimed by the **platform executor
  for both environment kinds**; the self_hosted `Poll` never serves it.
- **Executor**: a `web_exec` item runs with **no sandbox Provision** — the driver scans
  the session's unanswered web-tool uses, runs them through `webtool`, and posts
  `agent.tool_result`. The existing `tool_exec` driver (and the worker's) filters its
  scan to the six sandbox tool names, so neither path ever answers the other's calls.
- **Mixed turns on self_hosted** (`bash` + `web_fetch` in one turn): the worker's work
  item is **held back until every web call is answered** — a polling real `ant` worker
  must never see an unanswered web `agent.tool_use` (it would answer unknown-tool).
  Extends the trigger logic that already computes `HasUnansweredPlatformToolUse`.
  Reference ordering unobserved; INFERRED.
- **Result plumbing**: `toolset.Result{Content string}` cannot carry `search_result`
  blocks; it grows an optional structured-blocks field (nil ⇒ single text block, today's
  shape byte-identical), with a domain `SearchResultBlock` type round-trip-tested against
  the SDK's JSON.
- **Telemetry**: web executions record through the same tool-run instrumentation point,
  keeping the one-metric-one-meaning invariant.

## Slices

1. **`internal/webtool`** — interfaces, tavily/jina adapters, contract suite, and the
   consent-gated live tier. The env table above is this slice's *naming* contract only —
   the production processes read nothing yet; the executor consumes it in slice 3.
   → verify: `make verify`; live tier manually with filled keys.
2. **Wire surface** — domain `SearchResultBlock` + `Result` blocks field + definitions +
   brain offering/policies; its INFERRED DIVERGENCES entries (input schemas, fetch-block
   choice) land in the same PR — the registry rule is same-PR, never batched to slice 4.
   → verify: round-trip tests against the SDK types; enabling `web_search` puts it in
   the model request.
3. **Routing + execution** — `web_exec` kind, trigger logic (hold-back + name filters),
   executor driver wiring the env table above, telemetry; the mixed-turn hold-back
   INFERRED entry lands in the same PR. → verify: queue/executor contract tests;
   end-to-end acceptance: a
   local-stack session (real keys) runs "search X, fetch the top hit" and the event log
   shows `agent.tool_use` → `agent.tool_result{search_result}` / `{text}`.
4. **Docs** — the remaining DIVERGENCES entries (egress origin: deliberate;
   allowed-domains config: INFERRED; spill-file: deferred),
   docs/self-hosted-security.md (web egress originates in the executor, not the gate;
   the "still deferred" line goes), README status, CHANGELOG. STATE.md flips this plan
   `in-progress` in slice 1's PR; slice 1 already updated CLAUDE.md's `.env` sentence
   and docs/ARCHITECTURE.md's package reference with the seam.

## Acceptance (from #47)

An agent with the web tools enabled can call `web_fetch` and `web_search`; results land
in the event log as the typed blocks above; a `limited`-networking environment does not
constrain them; and a self_hosted session driven by the real `ant beta:worker` completes
a mixed turn without the worker ever seeing a web call.
