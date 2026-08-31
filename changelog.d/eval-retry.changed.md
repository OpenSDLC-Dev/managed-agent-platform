- **The eval suite retries model non-compliance once, reported** — a trial
  whose failures are all Either/Model class earns one fresh attempt (new
  session, new nonce) before the run reds; any other class — Platform above
  all — still reds immediately and is never retried, per plan 02's classing
  line ("M model non-compliance (one retry, reported)", now built and extended
  to Either, whose evidence cannot separate model from platform any better).
  Nothing is silent: the summary headline says `(N retried)` on any run that
  needed one, the superseded attempt keeps its record (`attempt: 1` in
  report.json), its transcript and its failure detail, the table marks both
  rows (`FAIL (retried)`, `PASS (retry)`), the headline count judges final
  attempts only, and the token totals still charge both. This is what stops a
  single stochastic refusal from a live endpoint redding the nightly while
  keeping every genuine platform signal loud. Skill fixtures now upload with a
  per-attempt `display_title`, so a retried skill trial re-uploads cleanly
  instead of tripping the title-uniqueness rule. (#528)
