- **The last three `actions/checkout` call sites drop the job's git credential** (#558).
  `release.yml`, `claude.yml` and `claude-code-review.yml` never set `persist-credentials: false`,
  so `GITHUB_TOKEN` stayed wired into the job's git config — in the workflow that publishes the
  images, the chart and the Release, and in the two holding `id-token: write` and an OAuth token.
  It was not merely sitting there unused: `http.<server>.extraheader` REPLACES the header git
  derives from a remote's URL, so in the two Claude jobs it outranked the App token
  `claude-code-action` mints for itself, and was the credential every git request actually
  carried — the action's own attempt to remove it looks for a `.git/config` key that
  `actions/checkout` v6+ no longer writes. Dropping it is corrective rather than tidy, and for
  `claude.yml` it changes who git speaks as. It also corrects a released claim: v0.2.0's
  supply-chain entry said every checkout in the repository had gained the flag, and `git show
  v0.2.0:.github/workflows/release.yml` shows that one never did. That record is frozen, so the
  correction is written here and in #558 instead.
