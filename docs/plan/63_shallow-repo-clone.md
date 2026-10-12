---
status: archived
issue: "#582"
---

# A shallow repository clone in the reference's shape (plan 63)

A `github_repository` resource is cloned by the executor (`cloneToTar`,
`internal/executor/repoclone.go`). Before this plan the clone was full, and its
`.git/config` held one remote. The 2026-09-03 recording found the reference's
checkout shallow, with two remotes (#582). The user decided on 2026-10-12:

- every checkout form is depth 1, and a commit checkout fetches the exact SHA;
- the `.git/config` takes the recorded shape, whatever the checkout form.

Delivered in one PR, which archives this plan.

## Ground truth (verified 2026-10-12)

- **The recorded checkout** (2026-09-03 batch2 `session.events.list.repo-inspect`,
  `https://github.com/anthropics/skills`, default branch): `git rev-parse
  --is-shallow-repository` is `true`, and `.git/config` reads:

  ```
  [core]
  	repositoryformatversion = 0
  	filemode = true
  	bare = false
  	logallrefupdates = true
  [gc]
  	auto = 0
  [remote "origin"]
  	url = https://github.com/anthropics/skills.git
  	fetch = +refs/heads/*:refs/remotes/origin/*
  [branch "main"]
  	remote = origin
  	merge = refs/heads/main
  [remote "github"]
  	url = https://github.com/anthropics/skills.git
  	fetch = +refs/heads/*:refs/remotes/github/*
  	gh-resolved = base
  ```

  The recording does not show the refs, the depth, or a branch or commit form.
- **go-git v5.19.2 against GitHub**, measured with a scratch program:
  - a depth-1 clone of the default branch works. For `anthropics/skills` it
    carried 409 objects, 3.7 MB. go-git writes the refspec
    `+HEAD:refs/remotes/origin/HEAD`, creates only `refs/remotes/origin/HEAD`,
    and writes the `[branch]` block;
  - a depth-1 fetch of an exact SHA works through `Init`, `CreateRemote` and
    `Fetch`. `git.CloneOptions` has no field for a SHA;
  - an unknown SHA fails as the text `upload-pack: not our ref <sha>`, with no
    sentinel, so `cloneReason` called it `internal`;
  - go-git rebuilds a remote's subsection from its typed fields on every
    config write. A key it has no field for, like `gh-resolved`, needs a second
    write.
- **The test fixture could not serve a shallow request.** It was go-git's own
  server-side transport, which refuses one outright and cannot truncate history.
  `git http-backend` can, and `make test` already needs a git binary
  (`tools/sdkref` asks git which files are tracked).

## Decisions

1. **Depth 1 for every form.** Default branch and branch use `Depth: 1,
   SingleBranch: true`. A commit no longer needs the full history.
2. **A commit is fetched by its exact SHA** into a scratch ref, checked out
   detached, and the ref removed, so no ref stands beside HEAD. There is no
   fallback to a full clone: the URL grammar admits only `github.com`, which
   answers a want for any commit its refs reach. The null SHA is still refused
   before the remote is asked.
3. **The recorded `.git/config`**, written after the checkout:
   - `origin` and `github`, both at the URL's `.git` spelling, each fetching
     `+refs/heads/*` into its own namespace;
   - `gh-resolved = base` on `github`, and `gc.auto = 0`;
   - the `core` keys `git init` writes.

   Section order is go-git's. No credential is written: the token rides a
   request header only.
4. **Refs as `git clone` leaves them.** A branch checkout keeps its `[branch]`
   upstream and gets `refs/remotes/origin/<branch>`, with `origin/HEAD` naming
   it, so `git status` has an upstream. No other branch is fetched: the
   recording does not show which the reference's holds. A commit checkout has
   no refs.
5. **An unknown SHA reports `checkout`.** The commit fetch wraps the remote's
   `not our ref` as `plumbing.ErrObjectNotFound`. That keeps `cloneReason`
   sentinel-based.
6. **The fixture serves through `git http-backend`** behind `net/http/cgi`, with
   `uploadpack.allowReachableSHA1InWant` set the way GitHub behaves. Its
   handler-level failure modes stay in front of it.

## Not settled here

Clone timing, and the recorded cached-checkout anomaly, stay with #78
(`docs/DIVERGENCES.md`, the `github_repository` materialization entry).
