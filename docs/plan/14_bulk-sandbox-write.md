---
status: archived
issue: "#206"
---

# A bulk sandbox write — N files for one exec

> Archived on landing: the decisions below and the code that implements them are one PR. The
> narrative is in CHANGELOG.md and the delivery record in [docs/HISTORY.md](../HISTORY.md) §
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

```
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

### D4 — file entries only; the untar creates the parents

Measured, `debian:stable-slim`, docker 28 / GNU tar 1.35:

| | implicit parent dirs | file entry |
|---|---|---|
| docker daemon untar (`PUT /archive`) | 0755 | 0644 |
| in-container `tar -x` under `umask 022` | 0755 | 0644 |

The two agree, and they agree with what `mkdir -p` gives today, so nothing has to be said
about directories at all. The archive therefore carries **no directory entries** — measured,
an explicit dir entry chmods a directory that already exists (0700 → 0755 under both
untars), and a write must not change the mode of a directory it happens to pass through.

### D5 — both backends: deliver, then rename; classify only on failure

- **docker**: `putArchive(path=/)` with the whole archive, then one exec running the shared
  rename script. Two round trips, one exec.
- **k8s**: one exec, `tar -x -C /` reading the archive from the exec's stdin, then the same
  rename script inline. One round trip, one exec.

When delivery fails, both run the same recovery the docker `WriteFile` already runs for a
single file — `mkdir -p` the directories, which is also what classifies a path blocked by a
non-directory (`__map_path_fault` → `ErrNotDirectory`) — then retry the delivery once, then
rename. The manifest is the archive's first entry and both untars extract in order and abort
on the first error, so a delivery that failed on a later entry has already landed the
manifest the recovery exec needs. A recovery that cannot make the directories, or a retry
that fails too, sheds every temp the manifest lists and returns. Every path is O(1) round
trips.

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
bytes (`tee | wc -c`). A batch gets the same guarantee from its shape: the rename loop
refuses to move a temp file that is not there, so a stream that ended early fails as a short
write instead of landing a subset. `tar` fails on a truncated member as well; the existence
check is what the guarantee rests on.

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

## Acceptance

1. `sandbox.Sandbox` has `WriteFiles`; both backends implement it and pass the same new
   contract rows in `internal/sandbox/sandboxtest`: round trip creating parents, per-file
   atomicity, `ErrIsDirectory` naming the offending member, `ErrNotDirectory` for a blocked
   parent, an existing target's mode preserved, a created file at 0644 and a created
   directory at 0755, no temporary file left behind on either outcome, and an empty batch as
   a no-op.
2. Materializing a skill of N files costs O(1) execs, not O(N) — both materializers
   (`internal/executor/skills.go`, `internal/worker/skills.go`) write a skill's tree in one
   call, measured against a real daemon.
3. Per-file atomicity is unchanged: a failed batch leaves each target holding what it held,
   never a truncated file, and the temporary files are shed.
4. `make verify` green, coverage gate held.
