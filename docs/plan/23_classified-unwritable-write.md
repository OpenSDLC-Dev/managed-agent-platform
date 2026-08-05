---
status: draft
issue: "#306"
---

# A write the sandbox cannot land is the model's error, not the platform's fault (plan 23)

A write to a **replaceable** path whose temporary file cannot be created fails
unclassified today: k8s surfaces a raw `k8s: write <path>: exit 1`, docker the daemon's
raw `http 400 ... container rootfs is marked read-only`. An unclassified error is the
failure mode #71/#205/#303 removed for their cases — the executor stops the tool set and
abandons the work item to lease reclaim, retrying a doomed call until the lease runs out,
instead of handing the model an error it can act on. #304 fixed the hang on this branch
(the drain) and deliberately left the error's shape to this plan; the reachable cases are
a read-only root outside the writable mounts, a root-owned parent under `RunAsUser`, and
ENOSPC. Issue #306 tracks it.

## What the reference implementation does

Resolved from the SDK checkout (authority rung 2 — no recording needed; behavior is in
shipped source): `tools/agenttoolset` is the toolset the real `ant` worker runs
(`anthropic-cli` `pkg/cmd/worker.go` builds `BetaAgentToolset20260401` and hands it to the
SDK's `EnvironmentWorker`), i.e. the reference behavior of `agent_toolset_20260401` at the
self-hosted deployment point. Three findings decide this plan's shape:

1. **Every write failure is a model-visible tool error, never an infrastructure fault.**
   Each failure path in the write tool returns `errorf(...)` — path resolution, the
   `os.MkdirAll` (`fs.go`: `write %s: mkdir: %s`), and every step of `atomicWriteFile`
   (temp creation via `os.CreateTemp` — exactly #306's case — write, chmod, close,
   rename: `write %s: %s`). `errorf` returns `(message, true)`; `funcTool.Execute`
   (`agenttoolset.go`) turns `(msg, true)` into a Go error its comment says the tool
   runner "surfaces to the model as an error result" — an `is_error: true` tool_result.
   The work item completes normally. Nothing is abandoned, nothing retries.
2. **Classification is a wording-normalization table, not an error-code taxonomy.**
   `fsErrorMessage` (`agenttoolset.go`) maps exactly four errnos to stable strings —
   ENOENT → `no such file or directory`, EPERM/EACCES → `permission denied`, ENOTDIR →
   `not a directory`, EISDIR → `is a directory` — and everything else falls through to
   the raw error text (EROFS → `... read-only file system`, ENOSPC → `... no space left
   on device`). Its comment states the purpose: consistent, language-independent wording
   across host runtimes. Our `internal/toolset/file.go` `fileFault` already mirrors this
   table's wording for the sentinels it classifies.
3. **The wire carries no structured file-fault codes.** A tool failure is text plus
   `is_error` — so wire compatibility constrains the *message wording*, not a code
   taxonomy, and the raw-passthrough messages embed random temporary names, so exact-text
   matching is neither possible nor expected. Our sentinels are and remain internal
   routing (classified → tool_result the model reads; unclassified → executor fault).

## Design

**One new sentinel, reason text carried from the sandbox's own shell.** The reference
classifies by errno of the failed create; our scripts' equivalent of errno is bash's
`strerror` text on the failed redirect (`Permission denied`, `Read-only file system`,
`No space left on device` — stable under the C locale the sandbox images run). The
refusal carries that text out, and the toolset normalizes it the way `fsErrorMessage`
does, so the model sees reference wording from both backends.

- `internal/sandbox/filefault.go`: sentinel `ErrNotWritable` (a write refused because the
  temporary cannot be created — or a missing parent cannot be made — next to the target),
  exit code `ExitPathNotWritable = 20` (14–19 are taken), and a small typed wrapper
  (`PathNotWritableError{Reason string}`, `Is(ErrNotWritable)`) so the reason survives to
  the toolset without string-splitting on the way.
- **k8s** (`internal/sandbox/k8s/k8s.go`, `writeScript`): the `: >` failure branch
  captures the redirect's message, drains, and exits classified —
  `msg=$({ : > "$4"; } 2>&1) || { cat >/dev/null; printf '%s' "${msg##*: }"; exit 20; }`
  — with the write exec's stdout collected into a small capped buffer (today it is
  discarded) so exit 20's stdout is the reason. The `mkdir -p` branch is the same defect
  by another route (a new parent under a read-only root fails EROFS, `__map_path_fault`
  answers 0, and the script exits a raw 1 today): #304 v2's subshell capture extends —
  when the path fault answers 0, the branch reports the mkdir failure's own message and
  exits 20 instead of 1. The drains land before either classification per #304.
- **docker** (`internal/sandbox/docker/docker.go`): when the archive PUT is refused and
  the #303 probe answered *replaceable*, a second probe exec attempts the same create the
  daemon-side extraction would need (`: >` a `TempName()` in the target's directory,
  removing it on success): failure classifies exit 20 with the same stripped bash
  message; success keeps the raw daemon error (the refusal was something else — no
  daemon-text parsing anywhere). Both backends thus derive the reason from in-sandbox
  bash `strerror`, which is what keeps the #205 identical-answers invariant without
  coordinating on error strings.
- **toolset** (`internal/toolset/file.go`): a `fileFault` row for `ErrNotWritable` —
  `failf("%s %s: %s", verb, display, reason)` — with the reason normalized by a table
  mirroring `fsErrorMessage`'s casing for the mapped case (`Permission denied` →
  `permission denied`) and passed through otherwise, exactly the reference's mechanism.
  No advice sentence: the reference's message for this class is the reason alone.

## Docs moved by this plan

The reference-implementation findings become repo documentation, in two steps:

- **With the plan PR** (this document): docs/REFERENCE_PROJECTS.md's `anthropic-sdk-go`
  role gains `tools/agenttoolset` — the reference host-side toolset the `ant` worker
  runs, the behavior-and-wording authority for `agent_toolset_20260401` tools (the
  research above is the record of why).
- **With the implementing PR**: docs/DIVERGENCES.md gains the convergence entry (write
  failures used to fault the platform where the reference answers the model — closed by
  this plan, source-anchored to `tools/agenttoolset`), and has the `ErrNotReplaceable`
  advice sentence checked against the registry while there (our added "Use bash
  redirection" hint has no reference counterpart); docs/ARCHITECTURE.md's `toolset` row
  names the `fsErrorMessage` mirror as the wording's provenance, and the `sandbox.go` /
  contract-suite rows take the new sentinel and cells; CHANGELOG.md carries the entry;
  #306 closes.

## Testing (TDD — red observed first)

- Host-bash (`k8s_internal_test.go`): `TemporaryCannotBeCreated` asserts exit 20 and the
  reason on stdout (red: today exit 1, empty stdout); a mkdir-EROFS row is not stageable
  on a host filesystem, so the mkdir route is pinned by the script-literal test and the
  contract row below. The drain assertions stay as they are.
- Contract (`sandboxtest/contract.go`): #304's read-only-root large-stream cell tightens
  from `err != nil` to `ErrNotWritable` (its comment already names this plan's issue); a
  buffered small-body cell joins it; both backends must answer the same sentinel. The
  non-root row's uid-route bound is unchanged (`RunAsUser` travels with `ReadOnlyRootfs`).
- docker unit (`api_test.go`): PUT refusal on a replaceable target runs
  probe-then-classify (exec sequence pinned, as `TestWriteFileShedsItsTempWhenThePutFails`
  pins removal-then-probe); a probe that succeeds keeps the daemon's error.
- toolset unit: the `ErrNotWritable` row's wording, mapped and passthrough cases both.

## Out of scope

Bulk writes (workdir-only by design), the read path (unaffected by parent writability),
wire-level error codes (none exist to be compatible with), and retry semantics for
transient ENOSPC (the reason text reports it; the model decides).

## Delivery

One PR, full ritual (code change → dual review). The plan flips to `in-progress` in that
PR and `archived` when it lands; STATE.md tracks the tasks while active.
