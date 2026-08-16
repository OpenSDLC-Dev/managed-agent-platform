# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 34](./docs/plan/34_doc-trim.md) (#413) — trimming the documentation to what code cannot say.
Tracked markdown was 2,665,735 bytes at plan start against 2,364,778 bytes of non-test Go;
the target is ~719 KB, cut from restatement and staleness only.

## Tasks

- [x] Slice 1 — `changelog.d/` 196 KB → 50 KB: a 1,500-byte cap and every fragment recut to it.
- [x] Slice 2 — twelve comments rewritten from the code (package docs, plus field and symbol
      comments; the plan's "seven" counted package docs alone); three documented lists pinned by
      offline tests, each of which found real drift on its first run.
- [x] Slice 3 — `docs/ARCHITECTURE.md` 325 KB → 39 KB: the per-file tables were 92% of the file
      and the derivative (nearly every package carries more comment than the document spent on
      it, most 3–10×), so they became a map. The subsystems that span packages — session
      resources, the sandbox lifecycle — moved into Execution flow rather than going with them.
- [x] Slice 4 — the steering layer (CLAUDE.md gains the "documents say what code cannot"
      rule and two invariants AGENTS.md held alone; README stops narrating delivery the
      changelog owns) plus the two deployment docs that did restate their config: the
      eight documents it covers went 307 KB → 291 KB. The two GCP docs name their
      Terraform variables only inside procedures, never as a reference, so they were kept;
      `self-hosted-security.md` grew instead — see the plan's checklist for both. Three gaps
      closed: the identity claim-name rule, MCP egress leaving the executor rather than the
      sandbox, and `runAsUser`, which `0` makes root rather than off.
- [ ] Slice 5 — `DIVERGENCES.md` 313 KB → 227 KB: the 24 entries grown into design essays
      became rows, each attacked by an independent skeptic; 25 losses and 6 accuracy defects
      repaired, not waived. Archived plans next. Three byte targets retired — the plan says why.
- [ ] Slice 6 — `HISTORY.md` and `docs/history/`; `docs/changelog/` is byte-frozen and out.
