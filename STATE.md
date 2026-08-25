# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 36](./docs/plan/36_memory-stores.md) (#52) — memory stores: a workspace-scoped
collection of text documents, attached through `resources[]`, mounted at
`/mnt/memory/<slug>`, read and written with the ordinary file tools, every write an
attributed immutable version, in sync across the sessions that share it. Cloud and
`self_hosted` both, the latter needing the per-item sessions token this plan starts
issuing. All seven scope decisions are settled; the design is the plan's nineteen decisions.

## Tasks

- [x] **Slice 0 — SDK bump v1.63.1 → v1.66.0** (#482): pin and pairwise diffs,
      `acceptance/dcf_test.go`'s one type rename, `type` accepted and echoed on built-in
      tool configs, the web tools' wire domain keys refused and filed as #481, the live
      `v1.63.1` labels advanced with 85 registry citations re-read, HISTORY record; plan
      → `in-progress`.
- [x] **Slice 1 — stores** (#484): migration 0028, the three `mem*` prefixes, the six
      `/v1/memory_stores` routes on the vault idiom, `created_by`, the six registry items.
- [x] **Slice 2 — memories and versions** (#TBD): migration 0029, `internal/memsync`'s
      path/content validation and slug, the five memory routes (`view`, preconditions,
      occupancy, prefix rollups), the three version routes, actors, eight registry items.
- [ ] Slice 3 — session attachment and the `memory_store_id` filter.
- [ ] Slice 4 — cloud materialization, run-end sync, the brain's block. Cloud acceptance.
- [ ] Slice 5 — the sessions token: migration 0030, the `wtk_` lane and its matrix.
- [ ] Slice 6 — BYOC memory: the worker's decode, `SetupMemory` and sync, the
      `self_hosted` 400 lifted. Meets #52's `self_hosted` acceptance.
- [ ] Slice 7 — close-out: HISTORY, ARCHITECTURE, README, plan → `archived`, #52 closed.
