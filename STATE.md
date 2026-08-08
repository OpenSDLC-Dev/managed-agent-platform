# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

Two unrelated things in flight.

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset`
wired end to end (#45). Seven slices; #45's acceptance criterion is met at the
end of slice 4.

**GCP continuous delivery** — plan-less, single PR. Build → push → deploy →
smoke on every push to `main`, in **mode 2**. Infrastructure stays human-driven;
CD runs no Terraform (plan 20, Decision 9). Proven by hand against the real
project first, then the repo made to match what ran — the run, its numbers and
what it does *not* cover: [docs/HISTORY.md](./docs/HISTORY.md).

## Tasks

- [x] **Slice 1 — wire correctness.** Dangling `mcp_toolset` rejected on all
      four spec-resolving surfaces (the public MCP-connector guide pins both
      reference directions, superseding #66's one-way reading); `mcp_toolset`
      nested-shape validation, the #26 fail-open class closed on the MCP arm;
      resolved-config echo for both kinds at render, resolving only entries
      naming a tool the toolset has. Both rungs bind stored specs, not just the
      request that writes one. #59's doubt retired — the permission-policies
      guide states both toolset defaults outright. Verifier PASS WITH FINDINGS
      (note-severity only). Evidence:
      [materialize.go](./internal/toolset/materialize.go),
      [wire.go](./internal/api/wire.go),
      [mcptoolset_test.go](./internal/api/mcptoolset_test.go).
- [ ] Slice 2 — `internal/mcp` client + `mcp_catalogs` + the discovery driver.
- [ ] Slice 3 — MCP confirmation gating and denial synthesis.
- [ ] Slice 4 — brain expansion, execution driver, settlement chaining, spill.
- [ ] Slice 5 — vault credentials, injection, OAuth refresh.
- [ ] Slice 6 — networking polish and retry semantics.
- [ ] Slice 7 — evals, live tier, `ant` CLI acceptance, archive.
- [x] **CD landed** — `cloudbuild.yaml` (`DOCKER_BUILDKIT=1`, a pre-existing bug
      the `FROM --platform=$BUILDPLATFORM` line needs, plus `CLOUD_LOGGING_ONLY`),
      `deploy/gcp/staging-values.yaml`, `.github/workflows/deploy.yml`, two CI
      steps that render the values file (the sandbox pair, and the seven-key
      Secret contract in both directions), and the runbook split in
      `deploy/gcp/README.md`.
- [ ] CD's first **green** run from the workflow itself. It has now run: WIF auth
      and `setup-gcloud` succeeded, and it died at `gcloud builds submit`, which
      stages source as the *caller* — so the manual runs, called by a human with
      Owner, had never exercised that path. #349 builds on the runner instead;
      its merge is the next attempt. The console's pipeline is already green
      end to end, including its selector-recreation guard firing for real.
- [x] WIF ref gap closed — the provider now asserts `ref == refs/heads/main` and
      `ref_type == branch` as well as `repository_owner`, so a dispatched branch
      can no longer mint the deploy identity. Command and read-back in
      `deploy/gcp/README.md`; unexercised until a dispatch from a throwaway
      branch is seen to fail at auth rather than at the workflow's own guard.
- [ ] Replace the `model-providers` placeholder (real endpoint, fake key) with a
      live route before anything runs a session.
