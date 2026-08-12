- **The `repo-answer` eval is parked out of the task list** (issue #358). Plan
  25's E2E-2 trial landed with #331 on 2026-08-07 and has never run: it needs a
  private GitHub fixture repository and a token only an operator can mint, and
  neither exists — not in CI, and not in the repo-root `.env` on the machine it
  landed from. #331 said so at the time ("It has not been run") and
  `docs/HISTORY.md` recorded it as "wired but unrun". What nobody connected is
  that *unrun* and *harmless* are not the same thing here. `repoAnswer()` was
  registered in `tasks()` unconditionally, while the scheduled job runs
  `make eval`, whose `eval` target sets `RUN_EVALS=1` unconditionally, and
  `repoConfig` fails rather than skips once opted in — so for that job the trial
  was not opt-in at all, it was mandatory. On every nightly after the merge it
  aborted at its own start with `RUN_EVALS opted into the eval suite but
  GITHUB_EVAL_REPO_URL is unset`, reddening the run: green on 2026-08-07 (the
  trial did not exist at that ref), red on 08-08, 08-09 and 08-10 — the last two
  with all fourteen other trials passing, 08-08 with thirteen, since
  `journal-multiturn` failed there independently on model behaviour. A nightly
  that reds every night for a reason nobody can act on is one people stop
  reading, and that costs more than the coverage this trial was going to add — so
  the trial comes out of the list until the fixture exists. Nothing else moves:
  `evals/repo_test.go` keeps `repoAnswer()`, its two repo-specific graders and
  its clone oracle (the third grader, `ReadsFile`, lives in `grade_test.go` and
  is shared with the still-registered `file-answer`), and restoring it is putting
  one call back in `tasks()`. Two documentation claims became true again as a
  side effect rather than needing an edit: the task count was fourteen once more
  (README.md, `docs/ARCHITECTURE.md`), and the four `MODEL_*` secrets really were
  the scheduled job's whole configuration again. Three claims did need
  correcting, all of them the same defect — a rule stated as a status.
  `docs/ARCHITECTURE.md` said the eval job "is red until a maintainer configures"
  its secrets and `.github/workflows/evals.yml` said it "is red by design" until
  then; all four were configured on 2026-07-25 with the workflow's first run, and
  the job was red for an unrelated reason. `CLAUDE.md` described
  `GITHUB_EVAL_REPO_URL` / `GITHUB_EVAL_REPO_TOKEN` as live `.env` configuration
  when nothing read them while the trial was parked. #358 carried what restoring
  it needed, including two findings that would otherwise have to be rediscovered:
  the fixture must be **private**, because `repo-answer` is the only answer-style
  trial with no per-trial nonce and its fixture's privacy *is* the nonce — made
  public, the passphrase is reachable from the sandbox over the network while
  `ReadsFile` (which matches the tool_use input and never the result) stays green
  with an empty mount; and the two names cannot be stored as GitHub secrets under
  that spelling at all, since GitHub forbids the `GITHUB_` prefix for both
  secrets and variables at every scope. Both findings were acted on inside this
  same release: the fixture was created and the trial restored, under the renamed
  `EVAL_GITHUB_REPO_*` — see the restore entry. Read this one as the record of why
  the trial was gone for a fortnight, not as the state of the suite today.
