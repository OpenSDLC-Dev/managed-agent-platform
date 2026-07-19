---
status: in-progress
issue: "#27"
---

# Work Stop returns 204 No Content

The plan for [#27](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/27). It needs a
plan file rather than direct tasks because it **reverses a CONFIRMED entry in
[docs/DIVERGENCES.md](../DIVERGENCES.md)**: the registry currently asserts that `200` + a
`BetaSelfHostedWork` body is the reference wire behavior for
`POST /v1/environments/{environment_id}/work/{work_id}/stop`. Settling a wire-schema question that
the repo has already answered the other way is exactly the case CLAUDE.md says must not be
improvised mid-implementation.

## What the reference actually does

The pinned `anthropic-sdk-go` v1.56.0 answers the question directly, in a comment written by the
SDK's own maintainers at `lib/environments/poller.go:439-465`:

> Today the server returns 204 with no body / no Content-Type, and the strict Go decoder errors
> with `"expected destination type of 'string' or '[]byte' for responses with content-type '' that
> is not 'application/json'"` for what is actually a successful call. We work around it by
> rebinding the response destination to `**http.Response` via `WithResponseBodyInto` […]. TS and
> Python never hit this because their decoders handle 204/empty bodies gracefully; only Go is
> strict here.

The same file's `TODO` names the two ways the discrepancy could be retired — the OpenAPI spec
declaring `204 No Content` (it currently declares a `BetaSelfHostedWork` body "that the server never
sends"), or the Go SDK short-circuiting 204 the way the TypeScript SDK already does. Both framings
put the defect on the *client/spec* side and the `204` on the *server* side. The SDK's own test
fixture agrees: `lib/environments/worker_test.go:118-120` models a successful Stop as a bare
`w.WriteHeader(http.StatusNoContent)`.

## Why the registry got it backwards

The existing CONFIRMED entry reasons: the generated `Work.Stop` is typed `*BetaSelfHostedWork`, and
driving the generated client against a 204/empty-body server makes its decoder error — therefore
`204` "is not wire-compatible."

That inference conflates **a known Go-SDK-only decoder bug** with **the service's contract**. The
measurement behind it was real (the generated decoder does error on 204), but it measured the
client, not the server — and the very SDK release that exhibits the bug ships a helper whose comment
exists solely to say the server sends 204 anyway. Wire compatibility means matching the reference
*service*; a client workaround documented in the reference SDK is evidence *for* the 204, not
against it.

## Consequence for this platform's own worker

`internal/worker/lease.go`'s `forceStop` calls the generated `Work.Stop` directly, without the
`WithResponseBodyInto` bypass. Flipping the server to 204 without touching the worker would make
every *successful* force-stop return a decode error and log `worker: force-stop failed`. The BYOC
worker is the customer-hosted twin of the reference's own poller, so it adopts the reference's
documented workaround rather than inventing one.

## The counterargument, and why it loses

Adversarial review raised a real objection worth recording rather than burying: `200` + JSON
satisfies a **strict superset** of clients. Driving the pinned SDK against both shapes gives

| response | generated `Work.Stop` | helper / CLI (body bypass) |
| --- | --- | --- |
| `200` + JSON | ok | ok |
| `204` empty | decoder error | ok |

so — the objection runs — emitting `204` breaks a consumer that `200` would have served, for no
compatibility gain, and imports a defect Anthropic has an open `TODO` to remove.

It loses on three counts.

- **The consumer it "breaks" is already broken against the reference.** The naive generated method
  fails against Anthropic's own service today; no real client depends on it working. The clients
  that exist do not: the SDK's worker and poller apply the body bypass, and the real `ant` CLI binds
  `*[]byte` for *every* work command (`anthropic-cli/pkg/cmd/betaenvironmentwork.go:662-663`), so
  `ant beta:environments:work stop` is content with an empty body.
- **Being more permissive is the lock-in hazard this project exists to prevent.** A customer who
  develops against a self-hosted platform that answers `200` + JSON, using the generated method,
  gets working code that fails the moment it is pointed at Anthropic. Accepting more than the
  reference does not add compatibility; it silently teaches a contract the reference will not honor,
  which is precisely the migration trap a drop-in self-hostable platform must not set.
- **The `TODO` does not say what the objection needs it to say.** Its two branches are "update the
  spec to declare `204`" and "make the Go decoder short-circuit `204` as the TypeScript SDK does."
  Both take the server's `204` as given and differ only over whether the *spec* or the *decoder* is
  corrected. Neither restores a JSON body. TypeScript and Python decode `204` natively already; only
  Go is behind.

The typed schema does rank first in CLAUDE.md's resolution order, and it declares
`*BetaSelfHostedWork`. That ordering governs what to believe when the answer is unknown — not when
the schema's own authors ship a present-tense erratum naming this exact field as a body "the server
never sends."

## Approach

1. **Server** — `internal/api/server.go` gains a `handleNoContent` adapter beside `handle`: same
   error envelope, a bodiless `204` on success instead of `200` + JSON. `stopWork` drops to
   returning only `error`; `queue.Stop` keeps returning the updated work item for the state-machine
   tests. Route registration is the one-line swap. → verify: graceful and forced Stop each answer
   `204` with zero body bytes and no `Content-Type`.
2. **Worker** — `forceStop` adopts the reference's `WithResponseBodyInto(&raw)` bypass, with the
   comment pointing at `poller.go` rather than restating the workaround's mechanics. → verify: the
   worker lease tests, which drive a real in-process control plane, force-stop without an error.
3. **Registry** — the CONFIRMED `stop — response shape` entry is *replaced* (not deleted) by one
   recording the corrected contract and the reference evidence, so the reversal is auditable. The
   adjacent `graceful vs force semantics` INFERRED entry's `Tracked: #27` cross-reference is stale
   now that #27 explicitly scopes lifecycle convergence to #25; it is repointed. → verify: no
   registry entry still claims 204 is wire-incompatible.
4. **Tests and comments** — `TestWorkStopGracefulThenForce` asserts `204` + empty body for both
   modes and reads the resulting state back through `GET`; the `409` conflict path and the auth /
   scope tests are untouched. Comments in `internal/api/workapi.go` and `internal/queue/lifecycle.go`
   stop claiming 204 breaks the reference worker and instead document the SDK helper's body bypass.
   → verify: `make verify` green, coverage gate held.

## Out of scope

Graceful-stop lifecycle convergence (#25) and the identity-blind hung-worker race (#62) are
untouched; this plan changes only the HTTP response contract. `GET …/work/poll`'s empty-queue
response is a separate open question — the same SDK test fixture returns `204` for an empty poll,
which is worth revisiting against the INFERRED entry that currently pins it at `200` + `null`, but
it is not this issue's scope and gets an issue comment rather than a code change here.
