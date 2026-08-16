---
status: archived
issue: "#206"
---

# A bulk sandbox write — N files for one exec

> Archived on landing: the decisions below and the code that implements them are one PR. The
> narrative is in CHANGELOG.md and the delivery record in [docs/history/2026-07.md](../history/2026-07.md) §
> "A bulk sandbox write (plan 14)". **"The gap" describes the state of the repository *before*
> that PR** — read it as the argument for the design, not a description of the result. "Design"
> and "Acceptance" are what was *planned*; what shipped is CHANGELOG.md.

The plan for [#206](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/206).

## Why this needs a plan file

Not for its size — it is one PR — but because it adds a method to `sandbox.Sandbox`, one of
the three swappable pieces the architecture is built on, and every backend must implement it
and pass the same contract rows. The issue itself names the seam and then declines to build
it ("a new interface method and a new contract row, deliberately out of scope for a bug
fix"). What the batch guarantees, and what it deliberately does *not* guarantee, is a
decision a reviewer should be able to read rather than reconstruct from a diff.

## The gap

Since [#71](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/71) (PR #203) the
docker backend's `WriteFile` lands the bytes under a temporary name and renames them into
place. The rename is a second round trip — one container exec per write — and it dominates
the cost of a small file. Measured on this branch against a real daemon, warm directory,
`debian:stable-slim`:

```text
WriteFile: 20 calls in 409ms (20.4ms each)
bare Exec: 20 calls in 276ms (13.8ms each)
```

So ~68% of a buffered write is the rename exec. The k8s backend absorbs the rename into the
script it already runs and pays nothing extra per write — but it pays one exec per file all
the same, because its `WriteFile` *is* one exec.

Nothing batches, and one caller writes a whole tree a file at a time:
`internal/executor/skills.go` and `internal/worker/skills.go` both loop
`sb.WriteFile` over `skills.Extract`'s members. `internal/skills` accepts an archive with up
to `MaxMembers` = 10,000 files, so a large-but-valid skill costs ~10,000 execs — about 138
seconds of pure exec overhead at the rate measured above, on top of the writes themselves.

## Design

### D1 — the seam is a buffered batch, not a stream

```go
// FileWrite is one member of a bulk write.
type FileWrite struct {
	Path string // absolute, cleaned
	Data []byte
}

WriteFiles(ctx context.Context, files []FileWrite) error
```

In memory, because the only caller already holds the whole tree in memory —
`skills.Extract` returns `[]skills.File` — so a streaming multi-entry source would buy
nothing and cost a protocol. `WriteFileStream` stays exactly what it is: the one-big-file
path (a mount up to the Files API's 500 MB cap), which is a different problem and already
solved. The batch's own size is the caller's to bound; on the skills path
`skills.ExtractMaxBytes` already does.

### D2 — per-file atomicity, no batch transaction

Each member lands atomically, by the same temp-file-and-rename every single write uses. The
*batch* is not a transaction: the first failure stops the run, the members that already
landed stay landed, and the rest are never written. That is a deliberate choice to be a drop-in
for the loop it replaces — `materializeSkill` stops at its first `WriteFile` error today and
leaves the partial tree behind, and the sentinel records only what landed, so the next pass
re-runs the whole skill. Rolling a batch back would be new behavior wearing a bug fix's
clothes, and there is no caller asking for it.

### D3 — one archive: a manifest, then one temp entry per file

The temp file must live in its target's own directory — that is what keeps the rename
inside one filesystem and therefore atomic, and it is the whole reason writes are done this
way. So the archive carries, for each member, one entry named
`{dir(target)}/.map-write-{nonce}`, and a **manifest** listing `tmp\0target\0` pairs. The
manifest is the archive's **first** entry and lands at `{workdir}/.map-write-{nonce}`. A
single exec then walks it: check the target is not a directory, carry the target's mode over
(`__map_preserve_mode`), `mv -f`, check again. One shared script, embedded by both backends,
next to the shared path-fault and mode-preserving shells in `internal/sandbox/filefault.go`.

The manifest travels as a file rather than in the script because it cannot travel in the
script: Linux caps a single `execve` argument at `MAX_ARG_STRLEN` = 128 KiB, and a
10,000-entry batch's generated script is on the order of 800 KB.

### D4 — file entries only; the sandbox makes the parents

Measured, `debian:stable-slim`, docker 28 / GNU tar 1.35:

| | implicit parent dirs | file entry |
|---|---|---|
| docker daemon untar (`PUT /archive`) | 0755 | 0644 |
| in-container `tar -x` under `umask 022` | 0755 | 0644 |

The archive carries **no directory entries** — measured, an explicit dir entry chmods a directory
that already exists (0700 → 0755 under both untars), and a write must not change the mode of a
directory it happens to pass through.

But *who* makes a missing parent turns out to decide whether the write can finish at all, and it
is not the untar. The docker daemon extracts on the **host, as root**, so the directories it
creates for the members belong to root — and a sandbox whose image runs as anyone else can then
rename nothing into them. Measured on an image with `USER app` and a user-owned workdir: a
file-at-a-time loop succeeds (its own `mkdir -p` runs inside the sandbox) and a batch that let the
daemon make the directories fails outright with `mv: Permission denied`. Skill materialization
would have stopped working on every non-root docker image.

So the batch makes its own directories, inside the sandbox, in a pass of its own — which is what
the single write always did. The k8s backend needs no such pass: its `tar` runs in the pod, so
everything it creates is already the sandbox user's.

Those directories land 0755, which is *not* what the single write's `mkdir -p` gives — that takes
the image's umask, so a hardened image gets 0700 there. The prepare pass sets `umask 022`
deliberately, to match what a host-side untar gives on the other backend: cross-backend agreement
is the property the shared contract suite exists to hold, and the difference it costs is one only
a second UID inside the same sandbox can see. (The k8s write script's own `umask 022` is
load-bearing besides: measured, a **non-root** user extracting under a 077 umask lands the *file*
0600, where #212 requires 0644.)

### D5 — deliver, prepare, deliver, rename

- **docker**: `putArchive` the bookkeeping (it lands in the workdir, which exists), one exec to
  make the members' directories from it, `putArchive` the members, one exec to rename them all.
  Four round trips of which **two are execs** — O(1), against one exec *per member* before.
- **k8s**: one exec, `tar -x -C /` reading the whole archive from the exec's stdin, then the same
  rename script inline. One round trip, one exec, because the extraction is already inside the
  sandbox and needs nothing pre-created.

A failure after the bookkeeping has landed sheds what the batch put in the sandbox
(`__map_bulk_discard`) on the way out. On k8s an extraction that fails runs the same prepare pass
— which is also what classifies a path blocked by a non-directory — and delivers once more. The
bookkeeping is the archive's first entries and lands in the workdir, so an extraction that failed
on a later member has already delivered what that pass reads.

The bookkeeping is written 0644 rather than 0600, and that is not cosmetic: on docker it is
extracted by the daemon and therefore owned by root, and the sandbox user has to be able to read
it. It names paths the sandbox can already list.

### D6 — the k8s sandbox image must carry `tar`

The docker backend needs nothing new (the daemon extracts). The k8s backend extracts inside
the pod, so `tar` joins `/bin/bash`, `mkdir`, `mv`, `rm`, `stat`, `chmod`, `tee`, `wc` and
`cat` in the image contract, and is documented as such in the contract-suite harness and
`docs/ARCHITECTURE.md`. Like every other one of those, it comes off the agent's own PATH and
carries the trust model the write path already has: a planted `tar` chooses what a write
writes, and reaches nothing the agent could not reach with its own hands.

### D7 — the short-stream guard, restated for a batch

The k8s write path cannot see a truncated stdin stream — client-go hands a failed stdin copy
to `runtime.HandleError` and never to the caller — which is why a single write counts its
bytes (`tee | wc -c`). A batch gets the same guarantee from its shape: the rename script walks the manifest **twice**,
and the first pass moves nothing — it only checks that every member the manifest names is
actually there. So a stream that ended early is refused whole, before any member is renamed,
which is the one failure in this design that lands nothing at all (every other one is D2's
ordinary non-transactional stop). The same pass is what stops a manifest that is missing or
empty from reading as a batch that succeeded and wrote nothing. `tar` fails on a truncated
member as well; the existence check is what the guarantee rests on.

### D8 — the failing member is named

The script reports the failing member's manifest index on stderr as `map-bulk-fail {i}`, and
the caller turns it back into the path, so a batch that hit a directory says *which* path is
a directory. A marker rather than the path itself because stderr may carry an image's own
noise, and an index cannot be confused with it.

### Out of scope

`internal/sandbox/shell/shell.go`'s two-or-three writes per bash call, which the issue also
counts. They bracket an exec — the command file before it, the head pointer after it — so
they cannot be batched; only the restart pair could, saving one exec on restart calls alone.
Not worth a second shape.
