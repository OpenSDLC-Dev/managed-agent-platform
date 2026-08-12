- **The `repo-answer` eval runs again, against a fixture that now exists** (issue
  #358; the parking entry in this same section is the other half of the story).
  Plan 25's E2E-2 trial came out of `tasks()` because the private GitHub
  repository and the read-only token it needs had never been created, which made
  it mandatory-and-impossible for the scheduled job and reddened three weeks of
  nightlies. Both exist now: a private fixture repository holding a single
  root-level `PASSPHRASE.txt`, and a fine-grained token scoped to that one
  repository at Contents: Read-only, stored with the fixture's URL as secrets on
  the `evals` deployment environment beside the four `MODEL_*`. The repository's
  privacy is load-bearing rather than cautious — `repo-answer` is the only
  answer-style trial with no per-trial nonce, so the fixture's privacy does that
  job — and it is measured rather than assumed: anonymous `git-upload-pack`
  against the fixture answers 401 and anonymous `raw.githubusercontent.com`
  answers 404, so the eval sandbox's unrestricted egress cannot reach the
  passphrase unless the platform really materialised the checkout. The passphrase
  is sized like the harness's own nonce (`newNonce`, 8 random bytes rendered
  hex) rather than longer: entropy enough that no model guesses it, short enough
  that none mistranscribes it into a false red.

  **The two names are renamed** — `GITHUB_EVAL_REPO_URL` / `GITHUB_EVAL_REPO_TOKEN`
  become `EVAL_GITHUB_REPO_URL` / `EVAL_GITHUB_REPO_TOKEN`. GitHub refuses to
  store a secret or a variable whose name begins with `GITHUB_` at any scope, so
  the old spelling could never have been configured in CI under the name the code
  reads; the alternative was a second name plus a mapping in the workflow's
  `env:` block whose only job would be to stay correct forever. The rename was
  free exactly while nothing anywhere was configured, and that window closed the
  moment the environment took the secrets.

  **The registered trial set is pinned** by `TestTaskSetIsPinned` and
  `TestDocsSpellTheTrialCount`, both offline, so they run on every `make verify`
  rather than only when someone pays for a live run. Nothing tied the suite to
  the two documents that count it: #331 took the set from fourteen to fifteen
  without touching either, and README.md and `docs/ARCHITECTURE.md` read
  "fourteen" for three weeks until the parking made them accidentally true again.
  The count is fifteen once more, and adding or dropping a trial now fails the
  merge gate until the pinned list moves with it, with the failure naming both
  documents. The second check is a tripwire rather than a proof — it asserts the
  spelled count appears in each file at all, which survives a reflow of the
  paragraph but could be fooled by the same word arriving for an unrelated
  reason. Both reviewers on the parking PR asked for this, and it is deliberately
  landing with the restore rather than after it.

  **The fixture's token and its passphrase both join the artifact scrub.** A
  failed trial's whole transcript is written to `evals/artifacts/`, and
  `evals.yml` uploads that directory unconditionally from a public repository,
  whose workflow artifacts anyone can download. For the token that closes the
  gap `TranscriptCarriesNoRepoToken` cannot: the run where that grader fires is
  precisely the run whose artifacts would carry what it caught. The passphrase
  matters more. On a trial that fails for any *other* reason — a grader the model
  tripped, a flaky turn — the passphrase is sitting in the agent's own final
  message, and every other answer-style trial can publish its answer harmlessly
  because it planted a fresh one for that trial, while this fixed fixture would
  be burned permanently and every later run would silently prove nothing. Two
  smaller holes on the same theme close with it: the trial's clone oracle now
  removes the token from a failing clone's error before that error reaches
  `t.Fatalf` and a public step log — go-git copies a refusing host's response
  body into its error, which is the exposure `internal/executor` already scrubs
  at the production clone's boundary — and that clone is now deadline-bounded,
  so a blackholed route fails the trial instead of hanging the run past the point
  where `go test -timeout` panics and the report is never written.

  **The `.env` reader in `evals/repo_test.go` has tests.** Its comment claimed it
  reads a line "exactly as modeltest and webtooltest read the same file" — a
  cross-package invariant nothing asserted, and one that only the restore would
  ever have exercised. `modeltest`'s own value table is now asserted against this
  parser too, alongside its key filtering (it must never become a second reader
  of the model key) and the environment-wins-even-when-empty rule that
  `evals.yml`'s "an unset secret reads as unset" comment rests on.
