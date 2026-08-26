- **Six registry entries stopped naming a closed issue as their live tracker** (#502) —
  archiving plan 36 closed
  [#52](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/52), but six
  [docs/DIVERGENCES.md](docs/DIVERGENCES.md) entries still cited it as *live*, so each
  named an issue that could no longer settle it — the nightly pointer guard's
  `live-tracker-open` finding, and a red `registry` check on any PR touching the registry,
  the guard or the Makefile. All six are now provenance (`#52 (delivered)`): the work
  landed, so no issue is owed. The `self_hosted` resource-superset entry additionally
  names the still-open
  [#322](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/322) for the
  repository arm it does not settle. No divergence text changed.
