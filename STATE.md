# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#90](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/90) — both provider adapters
always sent a non-nil `Chunk.Usage`, so an endpoint that reported no usage was indistinguishable
from a model that spent nothing, and the token metric recorded a false zero. No plan file (triage
returned `needs_plan: false`: single-PR scope, no wire-schema question).

## Tasks

- [x] Both adapters send `Usage: nil` when no usage object arrived — anthropic judges presence by
      the SDK's `respjson.Field.Valid()` on `message_start`/`message_delta`, openai by its existing
      per-frame `fr.Usage != nil`.
- [x] `turnResult.usage` is `*domain.ModelUsage` and `streamUsage` returns it directly; settlement
      substitutes zeroes so `span.model_request_end` keeps its `model_usage` object and the session
      usage fold is unchanged.
- [x] Verifier PASS — mutation-checked the new tests against `main` and probed both real adapters
      against fake endpoints; confirmed the SDK tags `model_usage` `api:"required"`, so keeping the
      zeroed object on the wire is correct rather than a divergence.
- [x] Reviews: Codex (`gpt-5.6-sol`, `ultra`) found that the decoder marks a field valid whatever
      its JSON kind, so `"usage": "bad"` still produced a false zero — confirmed by probe and fixed
      with an object-kind check. Claude-side adversarial review (Opus 4.8, four lenses) raised nine
      findings and confirmed none; its coverage note (the delta-only presence branch) is now fenced.
      `/code-review` itself is `disable-model-invocation`, so a workflow stood in for it.
- [ ] PR green on CI.
