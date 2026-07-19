# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#105 — K8s sandbox: `ReadFile` can return a short read as a success](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/105).
Plan-less, single-PR scope (issue-triage: `needs_plan` false); the read-side mirror of #103.

## Tasks

- [x] Clusterless tests first, red before the fix: `TestReadStdoutRequiresTheMarker` (9 subtests) and
      `TestReadScriptMarksWhatItSent` (9 subtests, host bash, `stat -c` shimmed on BSD hosts)
- [x] `readScript` marks the end of what it sent (`cat "$f" || exit 1`, then `printf %s "$3"` with a
      per-call `nonce()`); `ReadFile` requires and strips it via `readStdout`
- [x] Read buffer's room becomes cap+marker exactly, so `truncated` alone decides oversize and a file
      at `MaxFileBytes` still succeeds
- [x] `ReadFileAtTheCap` in the shared suite; docker passes unchanged (4/4 file subtests green)
- [x] Docs: CHANGELOG entry, HISTORY design record + amended #103 deferral bullet, ARCHITECTURE row
- [ ] Verifier verdict
- [ ] Dual code review, PR green, review threads settled, squash merge

K8s contract suite stays environmentally red locally (docs/HISTORY.md § "Local verification
blocker"); the file subtests passed 2 of 3 full runs on kind, CI is the gate.
