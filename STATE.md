# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**A Gemini upstream protocol** ([plan 62](./docs/plan/62_gemini-upstream-protocol.md),
#900): the model gateway answers a Messages request from a Gemini model through the
Gemini API with an API key, converting it to `generateContent` and back.

## Tasks

- [x] PR 1 — whole answers: migration 0052 and the `gemini` profile, the request and
      answer conversions, routing, thinking provenance's `gemini:` tag; a streamed
      request is refused on a Gemini-only alias
- [ ] PR 2 — streaming, the free `countTokens` live rows, the plan's close; generation
      live rows join #903
