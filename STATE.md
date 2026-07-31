# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 15 — web_fetch + web_search built-in tools** ([docs/plan/15_web-tools.md](./docs/plan/15_web-tools.md), [#47](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/47)): executor-executed on both deployment modes, Tavily/Jina backends.

## Tasks

- [x] Slice 1 — `internal/webtool` seam: Searcher/Fetcher, tavily/jina adapters, contract suite, consent-gated live tier (PR #221; live tier green against real Tavily/Jina 2026-07-31; the production env wiring is slice 3's)
- [ ] Slice 2 — wire surface: domain `SearchResultBlock`, `Result` blocks field, definitions, brain offering
- [ ] Slice 3 — routing + execution: `web_exec` queue kind, trigger hold-back, executor driver
- [ ] Slice 4 — docs: DIVERGENCES entries, self-hosted-security.md, README
