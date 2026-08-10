# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

Two unrelated things in flight.

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset`
wired end to end (#45). Seven slices; #45's acceptance criterion is met at the
end of slice 4.

**GCP continuous delivery** — plan-less, delivered. Build → push → deploy →
smoke on every push to `main`, in **mode 2**; infrastructure stays human-driven,
CD runs no Terraform (plan 20, Decision 9). Green from the workflow itself and
verified against the live cluster from outside the job; the by-hand proof, the
run, and what it does *not* cover: [docs/HISTORY.md](./docs/HISTORY.md). One task
remains, and it blocks every session.

## Tasks

- [x] **Slice 1 — wire correctness.** Dangling `mcp_toolset` rejected on all four
      spec-resolving surfaces and on stored specs, `mcp_toolset` nested-shape
      validation (#26's fail-open class closed on the MCP arm), resolved-config
      echo for both toolset kinds at render. #66's one-way reading corrected,
      #59 settled and closable. Evidence: [materialize.go](./internal/toolset/materialize.go),
      [wire.go](./internal/api/wire.go),
      [mcptoolset_test.go](./internal/api/mcptoolset_test.go).
- [ ] Slice 2 — `internal/mcp` client + `mcp_catalogs` + the discovery driver.
      Client landed: streamable HTTP, bearer injection, self-driven paging, a
      listing that survives a hostile server, and `internal/dialguard` extracted
      as one guard under it and the vault probe. Still open: `mcp_catalogs`
      migration + store, `Conn.CallTool` (needed by slice 4), the `mcp_exec`
      driver and its egress check. Evidence: [mcp.go](./internal/mcp/mcp.go),
      [dialguard.go](./internal/dialguard/dialguard.go).
- [ ] Slice 3 — MCP confirmation gating and denial synthesis.
- [ ] Slice 4 — brain expansion, execution driver, settlement chaining, spill.
- [ ] Slice 5 — vault credentials, injection, OAuth refresh.
- [ ] Slice 6 — networking polish and retry semantics.
- [ ] Slice 7 — evals, live tier, `ant` CLI acceptance, archive.
- [x] **CD delivered and green** — #347 landed it, #349 moved the build off Cloud
      Build onto the runner after the caller-vs-build-identity trap, and the WIF
      provider now asserts `ref`/`ref_type` as well as `repository_owner`. First
      green run from the workflow itself: 31260884425 on `0c01e14`, 3m41s, build
      through smoke, all three components on its images at `ready 1/1`. The
      console's pipeline is green too. Detail in `deploy/gcp/README.md` and
      [docs/HISTORY.md](./docs/HISTORY.md).
- [x] **[#355](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/355)
      — the deployment's identifiers are Actions variables**, not literals in a
      public repository; `staging-values.yaml` is now a neutral, self-rendering
      reference deployment the workflow overrides. The eleven variables were
      created first, on purpose: an unset one renders empty and would deploy with
      no project.
- [ ] Replace the `model-providers` placeholder (real endpoint, fake key) with a
      live route before anything runs a session.
