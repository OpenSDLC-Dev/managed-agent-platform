- **The Claude review workflow skips Dependabot's PRs instead of failing them** (#608).
  `claude-code-review.yml`'s job now carries `if: github.actor != 'dependabot[bot]'`.
  `claude-code-action` refuses a bot actor it was not told to allow, so the `claude-review`
  check went red on #603 and #604 before reading a line of either diff, and allow-listing would
  not have helped: a run Dependabot triggers sees only Dependabot secrets, so the review's OAuth
  token is empty there. The check is now skipped on Dependabot's PRs; the job still runs for a
  human actor, and ci.yml runs on Dependabot PRs as before. Branch protection did not require
  the check when those two merged, so nothing was blocked; the red X was noise.
