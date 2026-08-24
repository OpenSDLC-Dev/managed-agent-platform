# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**None.** The two follow-ups plan 35's closeout left are delivered: #445's sweep over
[docs/DIVERGENCES.md](./docs/DIVERGENCES.md)'s pointers, and #447's session delegation
bound — the one two agents messaging each other cannot escape. Eight issues that work filed
along the way are delivered too, in five PRs: #450 and #451 (the sections state their
test), #452 (the pointer invariant made executable), #458 (two mirrored entries
converged), #457 with #462 (the work API's enqueue trigger is a divergence, and says
so), and #463 with #465 (the session-id mirror was never observed on the reference at
all, so it is an inference now; four entries name the recording that settles them).

The bound's record is the registry entry; the three designs it beat, and why each was
rejected, are in [docs/HISTORY.md](./docs/HISTORY.md).
