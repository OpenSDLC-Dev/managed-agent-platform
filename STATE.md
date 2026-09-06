# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 41 — dreams** ([docs/plan/41_dreams.md](./docs/plan/41_dreams.md), [#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)), five slices: a
dream consolidates one memory store over 1–100 session transcripts. Slice 3 (this PR) lands the
pipeline's **four stages** and the digest threads that read the transcripts in batches, each stage
under its own turn cap; `update_existing` waits for slice 4. Slice 0's recording waits on preview
enrollment; no slice waits on it, each landing its entries INFERRED.

**#78 — confirming documented wire assumptions against a real managed-agents endpoint.** Four
recording days, 1,346 request/response pairs for about US$1.12, in a private archive. It stays open:
129 registry entries name it as their live tracker, and the debt is now analysis, not recording.

## Tasks

- [ ] Record plan 41's `/v1/dreams` checklist (its §8.2) — attempted 2026-09-05, 45 pairs,
      US$0, every item unanswered; waits on preview enrollment
- [x] Slice 1, the surface and the storage — the five routes, `drm_`, migration `0034`,
      create-time validation, the list, and archive and cancel over the state machine
- [x] Slice 2 — the runner, with a one-stage pipeline
- [x] Slice 3 — the full pipeline: four stages, batched digest threads, per-stage turn caps
- [ ] Slice 4 — `update_existing`
- [x] Reconcile 2026-09-02 and 2026-09-03's second wave into the registry — three entries
      to CONFIRMED, four registered readings refuted, 23 mismatches now their own issues
- [x] Record the multi-workspace capture plan 42 (#56) gated slice 1 on — items 1-5 in
      full, item 6 on its key lane; the gate lifts, its environment-key half unobserved
- [ ] Read the 72 un-analysed comparison rows from 2026-09-02
