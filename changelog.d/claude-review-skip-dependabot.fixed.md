- **The Claude review workflow skips Dependabot's PRs instead of failing them** (#608).
  `claude-code-review.yml` ran on every pull request, but a Dependabot-triggered run sees only
  Dependabot secrets, so its `CLAUDE_CODE_OAUTH_TOKEN` was empty, and `claude-code-action`
  refuses a bot actor unless allow-listed — so the `claude-review` check went red on #603 and
  #604 before reading a line of either diff, and would have on every Dependabot PR after them.
  The job now carries `if: github.actor != 'dependabot[bot]'`: the check is skipped rather than
  failed, a SHA bump's real check stays `make pins-test`, and a maintainer's own push to a
  Dependabot branch still gets reviewed, since `github.actor` is then the human. It is not a
  required check, so nothing was ever blocked; the red X was noise.
