- **The eval suite retries model non-compliance once, reported** — a trial
  whose failures are all Either/Model class earns one fresh attempt (new
  session, new nonce) before the run reds; any Platform-class failure still
  reds immediately and is never retried, per plan 02's classing line ("M model
  non-compliance (one retry, reported)", now built and extended to Either,
  whose evidence cannot separate model from platform any better). Nothing is
  silent: the superseded attempt keeps its record (`attempt: 1` in
  report.json), its transcript and its failure detail in summary.md — the
  table marks both rows (`FAIL (retried)`, `PASS (retry)`), the headline count
  judges final attempts only, and the token totals still charge both. This is
  what stops a single stochastic refusal from a live endpoint redding the
  nightly while keeping every genuine platform signal loud.
