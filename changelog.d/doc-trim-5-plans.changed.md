- **Archived plans drop the scaffolding their delivery records replaced** (#413) — Once a plan
  archives, its delivery record is [docs/HISTORY.md](./docs/HISTORY.md) and the changelog, so the
  pre-run scaffolding it still carried — how it intended to verify, what it planned to accept,
  which slices it planned to cut the work into — was a second, staler home for what those records
  already hold. 33 such sections go from 25 plans; every frontmatter block, title, Ground truth
  section and numbered Decision stays, because those are the part a plan exists to hold. Facts
  that survived nowhere else moved first rather than going with them: the transcript-redaction
  rule, which is wider than credentials and enforced by a grep that blocks a record, is now in
  HISTORY's header; plan 21's cross-language SDK pin joins the bump record it belongs to; and
  plan 12's *undelivered* per-substitution observability is now a comment on `egress.Substitute`,
  where an operator asking why a credential was not substituted actually arrives. Two corrections
  fell out of the review: plan 20 claimed every in-cluster credential was rotatable and exactly
  one unscoped, which had gone stale when #240 retired the GCS HMAC key, and plan 21's acceptance
  asked that every `api:"required"` field be present — a criterion `assertNoExtras` never
  implemented, now recorded as the gap it is.
