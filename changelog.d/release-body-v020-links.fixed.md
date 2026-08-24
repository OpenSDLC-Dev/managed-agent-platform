- **v0.2.0's published release body links into the repository from anywhere** — its notes were
  rendered before #423 and left 59 repo-root-relative targets across 39 paths relative. The body
  has been re-rendered from the tagged section by today's tool and replaced in place: identical
  bar those targets, clamped at the same group boundary, assets and flags untouched. A re-run of
  the tag cannot do this, because every step runs the tooling as of the tagged commit. Three
  statements justifying #423 said such a target 404s off the release page, and they were wrong:
  github.com's renderer resolves it against the repository at the tag, so the page always read
  correctly. What carried it unresolved was the raw body — the REST API, `gh release view`,
  mirrors — and the root-relative `body_html`. [docs/RELEASING.md](./docs/RELEASING.md),
  [changelog.d/README.md](./changelog.d/README.md) and #423's own entry now say that, and
  RELEASING.md records the one thing re-running a pre-fix tag reverts rather than converges.
  (#425)
