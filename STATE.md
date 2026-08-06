# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[#316](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/316)** —
docker bulk writes leave a root-owned residue the sandbox user cannot shed (the
#310 gap, for batches). No plan file: single-PR scope, triaged `needs_plan:
false`. Branch `fix/316-bulk-write-root-residue`.

## Tasks

- [x] Reproduce: a two-member batch into a root-owned `/etc` on a non-root image
      left 8192 bytes — `TestBulkWriteIntoARootOwnedParentOnANonRootImage`, red
      before the fix, green after.
- [x] Both bulk sheds name on stdout what their own `rm` could not take
      (`__map_bulk_left`); docker empties exactly those in one archive
      (`reclaimBulk`), including at the reported-fault return. The rename's
      exec-error branch still sheds with `rm` alone.
- [x] k8s `discardBulk` detached from the caller's context (`WithoutCancel` + a
      10s budget), with an API-server row — no clientset fake can see an exec.
- [x] Docs: ARCHITECTURE.md, self-hosted-security.md, CHANGELOG.md.
- [ ] Verifier, dual review, PR.
