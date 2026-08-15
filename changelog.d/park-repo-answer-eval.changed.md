- **The `repo-answer` eval was parked, then restored within this release** (issue #358,
  PR #359). The repository-mounting trial was registered unconditionally, but `make eval`
  sets `RUN_EVALS=1` and the trial's fixture configuration fails rather than skips once
  opted in — so with no fixture repository anywhere, the nightly eval job aborted before
  its first session, reddening every run from 2026-08-08 while its other trials passed.
  Taking it out of the registered set returned the nightly to green and the task count to
  the fourteen the docs spell; `evals/repo_test.go` kept the trial and its graders intact.
  It rejoined the suite later in this same release, once the fixture existed — see the
  restore entry; the full account is in docs/HISTORY.md.
