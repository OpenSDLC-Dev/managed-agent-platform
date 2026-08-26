# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

None.

## Tasks

None in flight. The last deliveries were the two things CD could not say for itself: a
failed `deploy` now opens and closes a tracking issue (#479), and a staging environment
parked and then forgotten opens one of its own (#504). Both are GitHub Actions and
`.github/scripts/` shell with no production code behind them, and both landed complete in
one PR each rather than passing through this file. Pick the next piece of work from the
GitHub issue backlog.
