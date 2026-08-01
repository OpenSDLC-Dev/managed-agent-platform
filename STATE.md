# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 15 — web_fetch + web_search built-in tools** ([docs/plan/15_web-tools.md](./docs/plan/15_web-tools.md), [#47](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/47)): executor-executed on both deployment modes, Tavily/Jina backends.

## Tasks

- [x] Slice 1 — `internal/webtool` seam: Searcher/Fetcher, tavily/jina adapters, contract suite, consent-gated live tier (PR #221; live tier green against real Tavily/Jina 2026-07-31; the production env wiring is slice 3's)
- [x] Slice 2 — wire surface: domain `SearchResultBlock` (SDK round-trip), `Result.SearchResults`, eight-tool definitions + `IsWebTool`, brain offering, openai search_result flattening (with slice 3, one PR)
- [x] Slice 3 — routing + execution: `web_exec` kind (migration 0015), web-first hold-back (brain settlement + confirmation resume), executor web driver (no sandbox, both env kinds), worker/sandbox-pass filters, env wiring (with slice 2, one PR)
- [ ] Slice 4 — docs: remaining DIVERGENCES entries (allowed-domains, spill-file), README status, plan archive + HISTORY summary (self-hosted-security.md landed with slices 2+3)
