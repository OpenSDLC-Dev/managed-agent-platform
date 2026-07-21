# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#54](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/54) — skills distribution +
execution, per [docs/plan/06_skills.md](./docs/plan/06_skills.md) (approved; this PR lands the
plan and the DIVERGENCES.md correction it carries — implementation starts with slice 1).

## Tasks

- [ ] Slice 1 — blob store foundation: `internal/blob` + `blob/s3` (minio-go) + contract
      suite/MinIO test harness; compose MinIO; helm `minio.yaml` + `externalObjectStorage`.
- [ ] Slice 2 — `/v1/skills` registry: migration 0007, `skillver_` ids, multipart create (both
      forms), nine endpoints, per-resource list limits; CI compose skills round-trip;
      `ant beta:skills` transcript.
- [ ] Slice 3 — anthropic prebuilt import: run-once importer, date versions, self-authored CI
      fixtures (license red lines per plan).
- [ ] Slice 4 — runtime materialization: env-key lane for skills reads, executor post-Provision
      + worker twin, 500-cap validation; `ant beta:worker` transcript.
- [ ] Slice 5 — brain Level-1 injection, evals E2E task, remaining DIVERGENCES entries, docs
      closure, archive the plan, close #54.
