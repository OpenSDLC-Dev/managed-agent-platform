---
status: in-progress
issue: "#942"
---

# A status the image's startup chose is no answer (plan 67)

An image's startup file (`BASH_ENV`) runs first in every shell a sandbox
starts. A startup that exits ends the shell before a platform script begins,
and the exec then reports the startup's status. Many platform scripts read
their exit status as their answer without asking whether the script ran. A
forged `0` then reports work that never happened:

- a checkpoint marked consumed with nothing restored;
- a package recorded as installed;
- a repository counted as landed with no checkout;
- a file write reported as done;
- a memory deletion whose baseline drops files still on disk, so the next
  sync pushes them back.

A forged refusal does harm too. A read refused as "not found" makes the
memory sync take its marker or its baseline as absent, so deleted memories
come back. #941 fixed the one case that destroyed data, the repository
presence probe, and left the rest to #942.

A survey of `internal/` on 2026-10-17 found these sites. The backend sites
are in `internal/sandbox/docker` and `internal/sandbox/k8s`:

| Site | Statuses read | Framed today |
|---|---|---|
| Docker `pathFault`, `unreplaceable`, `rename` | 0, 15, 16, 19 | no |
| Docker `notWritable`, `mkdirAll` | 0, 1, 15, 20 | yes, but the status is trusted unframed |
| Docker bulk write, `prepareBulk`, `refusedBulk` | 0, 16, 19, 20 | no |
| Kubernetes read (`readAnswer`) | 0, 10–13, 15 | yes; an unframed refusal carries `ErrUnframedStatus` beside its sentinel |
| Kubernetes `WriteFileStream` | 0, 14–16, 19, 20 | yes, but the status is trusted unframed |
| Kubernetes bulk write, `prepareBulk`, `refusedBulk` | 0, 16, 18–20 | no |
| Kubernetes `readExit` | 0 | yes; an unframed, silent `0` reads as "no record" and becomes 137 |

The callers that run a script of their own, or act on a backend's refusal:

| Caller | What a forged status does |
|---|---|
| `executor/repos.go` `extractRepo` | `0` counts the repository as landed |
| `executor/checkpoint.go` `restoreCheckpoint` | `0` marks the checkpoint consumed |
| `executor/packages.go` install pass | `0` records the packages as installed |
| `executor/memory.go`, `worker/memory.go` | a refused marker or baseline read reads as absent; `0` on a deletion writes the baseline |
| `executor/harvest.go` | a refused read drops an output from the snapshot |
| `executor/reposkills.go` | a refused read lists a skill without its description |
| `toolset/file.go` `fileFault` | a refusal or `0` reaches the model as the path's answer |
| `toolset/grep.go` `installRipgrep` | `0` takes ripgrep as installed |
| `sandbox/shell` `Run`, the `done` marker | a refusal other than "not found" is a backend fault, and the reclaim runs the command again |

## Decisions

1. **Every exit-status script runs framed** (the owner, 2026-10-16). A
   status that came with no frame in an output the cap did not cut is no
   answer, on both backends.
2. **A tool call answers a tool error** (the owner). A file read or write,
   or ripgrep's install, whose script a startup ended answers a tool error
   naming the image's startup, as `sandbox.StartupOutputError` does, never a
   backend fault. At the bash tool's `done` marker it reads as "not saved":
   the result is returned, `head` stays where it was, and nothing replays the
   command.
3. **A lost stream is told apart from a startup's exit** (the owner). An exec
   that returned no exit status is a transport failure and stays a backend
   fault that a reclaim retries. An exec that returned a status with no
   frame had its script ended by the startup.
4. **The begin line on either stream shows the script began.** `Frame.Open`
   prints the begin line on stdout and on stderr. client-go does not report a
   failed stdout copy (#105), so on Kubernetes an exec can return a status
   with its stdout lost. Where stderr carries the begin line, the script
   began and the stream was lost: a backend fault, as in decision 3. Where
   neither stream carries it, the startup ended the shell. Losing both
   streams whole reads as the startup's: a tool error, not a retry. The
   Kubernetes exit record is the one exception: its command has run, and a
   retry would run it again, so a stream lost there still reads as no record,
   which the liveness probes settle, as before.
5. **One error says the startup ended the script.** `sandbox`'s new
   `StartupExitError` names what was asked and the status the startup left.
   `errors.Is` matches it to `ErrUnframedStatus`, so #941's `StatPresence`
   is unchanged. It replaces the refusal sentinel that an unframed
   Kubernetes read carries today. A reader that takes `ErrFileNotExist` for
   "absent" then meets an error, not an absence.
6. **One helper runs every exit-status script.** It frames the script, returns
   Exec's own error as it is, and answers a `StartupExitError`, a lost
   stream or a `StartupOutputError` where the frame is missing. `ExecScript`
   stays only for scripts whose status no one reads. Where the cap cut
   stdout before the frame, a stderr that carries the whole frame still
   answers the status (`Frame.UnframeStatus`), so writes keep working under
   a startup that floods stdout (#860). The Kubernetes read still trusts a
   refusal whose stdout the cap cut before the frame: #941's `StatPresence`
   answers through that flood.
7. **Each caller treats the error as a failure of its step.** None takes it
   for an answer:
   - the repository does not land (`errExtraction`);
   - the checkpoint stays unconsumed;
   - the packages are not recorded;
   - the memory sync stops for this pass, before it pushes, pulls or writes
     a baseline;
   - the harvest fails rather than drop an output;
   - the skill is skipped as for any read that failed.

## Pull requests

1. **The sandbox layer.** `StartupExitError`, the helper, and every backend
   site in the table on both backends, each with a contract row: an image
   whose startup exits with each status the site reads answers the error,
   never the status. The toolset answers the error as a tool error where it
   answers `StartupOutputError`, so no reclaim retries it before the second
   pull request.
2. **The toolset.** `fileFault`, `shell.Run`'s `done` marker and
   `installRipgrep`, as decision 2 says.
3. **The executor and worker callers**, as decision 7 says.

## Verification

- Each backend site, on Docker and on kind: a startup that exits with each
  status the site reads gives `StartupExitError`.
- On Kubernetes, a status with stdout lost and stderr framed is a backend
  fault.
- Under a startup that floods stdout past the cap, a write, a batch and a
  refused write still answer, on Docker and on kind.
- Each caller in the table: a forged `0` and a forged refusal leave its state
  as it was.
- The bash tool: a startup that ends the `done` read leaves `head` as it was,
  and the call is answered, not reclaimed.
