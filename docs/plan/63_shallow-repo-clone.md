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

## Ground truth (verified 2026-10-12 and 2026-10-13)

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
    write;
  - a clone left at go-git's default tag mode fetches every tag, each with the
    tree of its commit, at depth 1 too (found in review, from
    `CloneOptions.Validate` and a scratch probe). A fetch's default follows
    tags only for a wildcard refspec, which an exact SHA is not.
- **Real `git clone --depth 1`** (git 2.54, measured):
  - `--branch <b>` leaves no `origin/HEAD` over protocol v2, the default. Over
    v0 it writes one when `<b>` is the default branch;
  - a default branch named `HEAD/<x>` clones fine, to
    `refs/remotes/origin/HEAD/<x>` and no `origin/HEAD`, whose name the
    tracking ref needs as a directory. One named `HEAD` leaves `origin/HEAD`
    naming itself;
  - a tag that points at the fetched commit is kept; other tags are not.
- **GitHub serves any commit in a repository's fork network by SHA, through
  the repository's own URL** (measured 2026-10-13 on `anthropics/skills`). Two
  depth-1 fetches succeeded: a fork's pull request head (`refs/pull/2004/head`,
  one commit ahead of `main`) and a commit only a fork's branch holds
  (`TheAwesomeAray/skills`, in no pull request GitHub's search finds).
- **go-git's ref-name check is git's, plus one rule:** it refuses a path
  component that is just `@`, which git accepts. go-git's clone refuses such a
  branch anyway, when it creates the local branch.
- **go-git cuts some names git admits.** `ReferenceName.Short()` scans with
  `fmt.Sscanf`, which stops at any space, a non-breaking one included, and
  go-git builds a single-branch refspec from it. Its `[branch]` section name
  stops at the next `refs/heads/` in the ref, so a branch name that holds that
  text loses its end, and the checkout gets no upstream (found in review, from
  go-git's source; this predates the plan).
- **The test fixture could not serve a shallow request.** It was go-git's own
  server-side transport, which refuses one outright and cannot truncate history.
  `git http-backend` can. `make test` already needs git (`tools/sdkref` asks it
  which files are tracked), and now its `http-backend` helper too.

## Decisions

1. **Depth 1 for every form, and no tags.** Default branch and branch use
   `Depth: 1, SingleBranch: true, Tags: NoTags`. A commit no longer needs the
   full history. A branch name go-git's ref-name check refuses is refused
   before the remote is asked, with reason `checkout`: one with a wildcard
   would fetch every branch's tree. So is a name holding a space, which
   go-git's `Short()` would cut, fetching another branch. git admits that
   name, so the reason is `internal`.
2. **A commit is fetched by its exact SHA** into a scratch ref directly under
   `refs/`, checked out detached, and the ref removed, so nothing stands beside
   HEAD. There is no fallback to a full clone: the URL grammar admits only
   `github.com`, which serves any commit in the fork network this way. A commit
   checkout can therefore mount a fork's code under the repository's name, as
   `git fetch <sha>` can; the owner decided on the exact-SHA fetch before this
   was measured. The null SHA is still refused before the remote is asked.
3. **The recorded `.git/config`**, written after the checkout:
   - `origin` and `github`, both at the URL's `.git` spelling, each fetching
     `+refs/heads/*` into its own namespace;
   - `gh-resolved = base` on `github`, and `gc.auto = 0`;
   - the `core` keys `git init` writes.

   Section order is go-git's. No credential is written: the token rides a
   request header only.
4. **Refs as `git clone --depth 1` leaves them over protocol v2.** A branch
   checkout keeps the `[branch]` upstream and `refs/remotes/origin/<branch>`
   go-git's clone gives it, so `git status` has an upstream. go-git cuts a
   branch name that holds `refs/heads/` at that text when it names the
   upstream, as it did before this plan; such a branch gets no upstream. A default-branch
   checkout also gets `origin/HEAD` naming its tracking ref, unless the branch
   is named `HEAD/<x>` or the remote's HEAD names no branch. No other branch is
   fetched: the recording does not show which the reference's holds. A commit
   checkout has no refs. Two differences are left:
   - the tag at the fetched commit, which git keeps and decision 1 drops:
     go-git follows tags only for a wildcard refspec;
   - a default branch named `HEAD`, which gets neither ref here, where git
     leaves `origin/HEAD` naming itself.
5. **An unknown SHA reports `checkout`.** The commit fetch wraps the remote's
   `not our ref` as `plumbing.ErrObjectNotFound`. That keeps `cloneReason`
   sentinel-based. The match is on text, so a host that words it otherwise, or
   does not admit a SHA want at all, reports a reason other than `checkout`: a
   failed clone either way, with a less exact reason.
6. **The fixture serves through `git http-backend`** behind `net/http/cgi`, with
   `uploadpack.allowReachableSHA1InWant` set the way GitHub behaves. Its
   handler-level failure modes stay in front of it.

## Not settled here

Clone timing, and the recorded cached-checkout anomaly, stay with #78
(`docs/DIVERGENCES.md`, the `github_repository` materialization entry).
