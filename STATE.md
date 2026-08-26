# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

None.

## Tasks

None in flight. The last deliveries closed out the GCP staging lane: the two things CD
could not say for itself — a failed `deploy` opens and closes a tracking issue (#479), and
a parked-then-forgotten environment opens one of its own (#504) — and then `environment/`'s
Terraform state moved to a bucket, so a destroy is no longer tied to the laptop that
applied it (#478). None of it is production code; all three landed complete in one PR each
rather than passing through this file. Pick the next piece of work from the GitHub issue
backlog.
