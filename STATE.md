# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#103](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/103) — K8s sandbox `WriteFile`
reported success on a truncated write. Plan-less (issue-triage: single-PR scope). Closes #86, the
same subtest and assertion.

## Tasks

- [x] Reproduce: new contract subtest `FileRoundTripLargePayload` fails on the live cluster at 1 MiB —
      `read back 32768 bytes, want 1048576; first difference at 32768`, `WriteFile` having returned nil.
- [x] Pin the script's exit-code contract deterministically, no cluster:
      `TestWriteScriptVerifiesDeliveredLength` (7 subtests, host bash).
- [x] Fix `writeScript` — drop `exec` (removes the trigger) and verify the delivered byte count
      (exit 14 → an error from `WriteFile`).
- [x] Green: both new tests pass, plus `ProvisionIsIdempotentPerSession` (the #103 subtest) and the
      docker backend on the same shared subtest.
- [x] Verifier: PASS with findings (reproduced the pre-fix loss 5/5, post-fix green 5/5); dual review
      landed, both reviewers converging on the re-stat flaw now fixed.
- [ ] Full `make verify` green — delegated to CI: the local K8s suite fails on unmodified `main` too,
      so the branch cannot be gated here (diagnosis in docs/HISTORY.md).
- [ ] PR to green CI.
