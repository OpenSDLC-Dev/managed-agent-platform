- **v0.2.0's published release body links into the repository from anywhere** — its notes were
  rendered before #423 and left 59 repo-root-relative targets across 39 paths relative. The body
  has been re-rendered from the tagged section by today's tool and replaced in place: identical
  bar those targets, clamped at the same group boundary, flags and assets untouched. A re-run of
  the tag could not have done it — every step runs the tooling as of the tagged commit — and
  would undo it, since a re-run reverts what was edited in place afterwards. The rationale #423
  recorded was itself wrong: it said such a target 404s off the release page, where github.com's
  renderer in fact resolves it against the repository at the tag, so the page always read
  correctly. What carried it unresolved is the raw body — the REST API, `gh release view`,
  mirrors — and the root-relative `body_html`. [docs/RELEASING.md](./docs/RELEASING.md),
  [changelog.d/README.md](./changelog.d/README.md), #423's own entry and the tool's own comments
  now say that. (#425)
