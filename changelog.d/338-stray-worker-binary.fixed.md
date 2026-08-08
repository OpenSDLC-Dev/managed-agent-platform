- **The stray root build outputs are untracked and ignored** (#338). PR #333
  committed an 80 MB Mach-O arm64 `worker` — a dev-time
  `go build -o worker ./cmd/worker` swept in by `git add -A` — and it shipped
  inside the v0.2.0 tag. `.gitignore` already carried `/gate`, so the hazard was
  known and only the list was short, which is exactly how it recurred: every
  `cmd/` name (`brain`, `controlplane`, `executor`, `gate`, `worker`) and
  `tools/changelog` is now listed, root-anchored so the `cmd/<name>/`
  directories stay tracked. `.dockerignore` gains the same set, which git
  ignoring does not cover — a developer who builds locally still has the binary
  on disk, and the Dockerfile's `COPY . .` would ship ~80 MB of host-arch
  executable into the image and its layer cache, the same context-size concern
  that file's `.claude/worktrees` entry already documents. History rewriting
  stayed out of scope, so the blob remains in pack history: this shrinks fresh
  working trees, not clone size.
