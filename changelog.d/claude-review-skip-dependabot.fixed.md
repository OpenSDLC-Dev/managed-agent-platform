- **The Claude review workflow skips Dependabot's PRs instead of failing them** (#608).
  `claude-code-review.yml` ran on every pull request, but a Dependabot-triggered run sees only
  Dependabot secrets, so its `CLAUDE_CODE_OAUTH_TOKEN` was empty, and `claude-code-action`
  refuses a bot actor unless allow-listed — so the `claude-review` check went red on #603 and
  #604 before reading a line of either diff, and would have on every Dependabot PR after them.
  The job now carries `if: github.actor != 'dependabot[bot]'`: the check is skipped rather than
  failed, and a maintainer's own push to a Dependabot branch is still reviewed, since
  `github.actor` is then the human. A Dependabot PR keeps `make pins-test`, which checks the
  pin's shape, not that the SHA is the tagged commit — that stays the merger's to confirm.
  Branch protection never required the check (only `ci` and `coverage` are), so nothing was
  blocked; the red X was noise.
