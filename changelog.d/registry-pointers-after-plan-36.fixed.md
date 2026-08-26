- **Six registry pointers left orphaned when plan 36 closed** — archiving the plan closed
  [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52), but six
  [docs/DIVERGENCES.md](docs/DIVERGENCES.md) entries still cited it as a *live* tracker, so
  each named a closed issue that could never settle it — the nightly pointer guard's
  `live-tracker-open` finding, and a red `registry` check on any PR touching the registry,
  the guard or the Makefile. All six are demoted to provenance (`#52 (delivered; …)`), and
  the `self_hosted` resource-superset entry now names the still-open
  [#322](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/322) for the
  repository arm it does not settle. No divergence text changed — only which issue each
  entry claims will settle it.
