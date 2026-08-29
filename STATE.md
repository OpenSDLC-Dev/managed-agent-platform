# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

None.

## Tasks

None in flight. Two streams closed at once. The GCP staging lane: CD now opens and
closes its own tracking issue for a failed deploy (#479) and for a parked-then-forgotten
environment (#504), and `environment/`'s Terraform state moved to a bucket, so a destroy
is no longer tied to the laptop that applied it (#478) — none of it production code, each
landing complete in one PR rather than passing through this file, as did the three
follow-ups those reviews spun off: neither notifier's `retry` hands a failed attempt's
output to its caller any more (#507), the Workload Identity Federation commands in the
deploy notes are runnable again, having asked `gcloud` for a project number it never takes
in `--project` (#508), and the operator's coordinates are out of `docs/`, with `make
identifiers-test` holding that rule now rather than memory (#514). And the three plan-36
leftovers that needed no decision from anyone: the test rows slice 4 deferred (#488), its
fifteen recording items folded into the issue tracking its twenty inferences (#78, which
stays open as that tracker), and memory-version retention (#476), where the count the
reference withholds is now this platform's own — the newest five. What is left of plan 36
needs owner decisions or a live run: #475 (dreams), #495 (the deferred `self_hosted`
acceptance transcript). Pick the next piece of work from the GitHub issue backlog.
