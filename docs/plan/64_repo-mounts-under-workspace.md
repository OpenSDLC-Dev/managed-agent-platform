---
status: archived
issue: "#922"
---

# Repository mounts rooted under /workspace (plan 64)

A `github_repository` resource's `mount_path` was used as the literal container
path. Four targets were refused, and the paths below `/mnt/memory` too, and so
was the skills directory when the session had skills (#584). The reference
refuses none of these: it roots every `mount_path` under `/workspace` (#922).
The owner decided on 2026-10-13:

- adopt the reference's rooting;
- record its unrecorded edges first, not infer them. They were recorded the
  same day (managed-agents-wire-recordings#22 and #23);
- let a repository share `/workspace/skills` with attached skills, as the
  reference does.

Delivered in one PR, which archives this plan.

## Ground truth

The docs (`managed-agents/github`, read 2026-10-13): `mount_path` is "the
directory under `/workspace` to clone the repository into. Defaults to
`/workspace/<repo-name>`."

Every recorded `mount_path`, all on session create:

| Sent | Echoed | Recording |
|---|---|---|
| omitted, `/`, `/workspace`, `/workspace/` | `/workspace/<repo-name>` | 2026-09-03 batch1 idx 95; 2026-10-13 setup[2], [4] |
| `/tmp`, `/mnt/memory` | `/workspace/tmp`, `/workspace/mnt/memory` | batch1 idx 94, 93 |
| `/opt/repo`, `/workspaceX/repo` | `/workspace/opt/repo`, `/workspace/workspaceX/repo` | setup[10], [8] |
| `/workspace/x/../y`, `/workspace/../etc` | `/workspace/y`, `/workspace/etc` | batch1 idx 96; setup[12] |
| `workspace/rel` | `/workspace/rel` | batch1 idx 97 |
| `workspace` | `/workspace/workspace` | setup[6] |
| `../x` | 400 `Invalid` `` `github_repository` `` `resource: mount path "../x" escapes /workspace/` | setup[14] |
| `/tmp` beside `/workspace/tmp`, or beside `/workspace/tmp/sub` | 400, the overlap sentence on the rooted paths | setup[15], [16] |
| `/workspace/skills`, the agent carrying the `pdf` skill | `/workspace/skills`; the skill and the checkout share the directory | setup[17–21] |

## Decisions

1. **The rule** (`resolveRepoMountPath`). Clean the path. If it is
   `/workspace` or below it, it keeps its place. A relative path led by
   `workspace/` drops that component. Any other path is rooted under
   `/workspace`. A cleaned relative path still led by `..` escapes, and is
   refused. A path that lands on `/workspace` itself takes the default. This
   fits all the recorded inputs; what it does with any other input is ours.
2. **Refusals.** Session create refuses an escape in the reference's words.
   The overlap sentence it already had now names the rooted paths, as
   recorded. The deployment routes keep this platform's words: no deployment
   recording holds either refusal (#540). A deployment also bounds the
   spelling it stores, as the file arm does (#849).
3. **No reserved targets.** Every rooted path is strictly below `/workspace`,
   so `/`, `/tmp`, `/mnt/memory` and `/workspace` are out of reach. A file
   mount stays under `/mnt/session/uploads` and a memory store under
   `/mnt/memory`, so neither can meet a repository on create any more. The
   add endpoint keeps its ancestor check for rows stored before this plan.
4. **Skills and a repository share `/workspace/skills`.** Create no longer
   refuses the pair (#584), and the executor's guard drops its skills clause.
   The executor clones repositories before it writes skills, so a clone never
   removes a skill written in the same pass. In a run with skills, a clone at
   or below the skills directory drops the skills sentinel inside the
   extraction, after the new tree is staged and before the old one goes:
   - a clone that fails before the extraction keeps the sentinel, and so the
     agent's edits to its skills;
   - an executor that dies after the drop leaves the skills to be written
     again;
   - a drop that fails stops the extraction with nothing lost, and the
     client sees a clone failure;
   - a checkout at the skills directory itself has its own `.materialized`
     removed before it lands, so the skills step never reads it as its marker.
     In a run without skills the file lands, but an idle checkpoint does not
     keep it: capture strips the sentinel's path whatever the file holds.

   Where both write one file, the skill's wins. Where a checkout puts a
   directory at a path a skill writes as a file, that skill fails part way
   and is skipped, and every skill is written again on each pass, as after
   any skill that cannot be written. The executor refuses a mount at or
   below either materialization sentinel, which create accepts. The
   reference's behaviour there is unrecorded, so that part is ours.
5. **Stored deployments keep the path they resolved.** A fire never
   re-resolves a stored path (#849). A deployment stored before this plan
   mounts its literal path until its resources are sent again.
6. **`/workspace` stays writable and checkpointed.** Every repository mounts
   there whatever the executor's workdir. So a read-only root filesystem
   keeps it writable beside the workdir (`sandbox.WritablePaths`), and an
   idle checkpoint keeps it as a root (`checkpointRoots`). A workdir below it
   is captured within it, and mounted after it.

## Not settled here

The repository cap stays eight, though the reference accepted nine (2026-09-03
batch1 idx 98). Repository skill discovery is #921.
