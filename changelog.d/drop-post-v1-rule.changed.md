- **CLAUDE.md drops its two pre-v1 leftovers** (#715) — the `(post-v1)` marker fenced work
  off until v1 shipped, so now that it has, "do not build ahead of them" is wrong rather than
  merely stale, and a steering document is a thing contributors obey. The backlog bullet keeps
  only the rule that outlives v1 — GitHub issues are the only backlog — and the query listing
  the still-open deferrals moves to [README.md](./README.md), the one place that already
  defined them as seams reserved but not implemented, with the `--limit 100` bound and the
  false-positive caveat it was given in #445. Nothing now asks that a new deferral be marked;
  the marker stays on the titles already carrying it, so the query keeps working as an index
  of what those issues are. Beside it, Simplicity first stops naming vaults, deployments,
  memory, multi-agent threads and skills as reserved seams, all five having shipped since;
  what a reserved seam *is* still stands, because tenancy is still one. Draft plan 42's
  decision 8 loses the sentence that maintained the marker for #56, whose title had already
  lost it.
