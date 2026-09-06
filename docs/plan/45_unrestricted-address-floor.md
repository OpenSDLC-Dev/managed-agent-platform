---
status: archived
issue: 570
---

# Plan 45 — `unrestricted` gets the address floor, and says so in the refusal

A recording of the reference on 2026-09-03 (#78, probes
`session.events.list.blocklist-unrestricted` and `.blocked-status`) answered the
question `internal/gate/policy.go` had been deferring in a comment. On an
environment with `networking: {"type":"unrestricted"}`:

| target | the reference's answer |
|---|---|
| `http://169.254.169.254/latest/meta-data/` | **403**, body `Destination IP is in a private/reserved range` |
| `https://example.com/` | 200 |

So `unrestricted` is unrestricted in its **hosts** and not in its **addresses**:
every name is admitted, and a private or reserved address is refused underneath
it. This platform admits both — `newPolicy` returns `admitAll` for
`domain.NetUnrestricted`, and `admission.floored()` then exempts that class from
the address floor as well.

Plan 44 is what makes this a small change: the gate's dialler now resolves a
name once and holds every resolved address to `admissionOf(ctx).floored()`
before any connect, so the floor is already wired to the class — it is only
being asked the wrong question for one of them.

## What the owner settled

Three decisions, taken 2026-09-06 against the alternatives named beside them:

1. **The floor lands in the gate, for the sessions that have one.** A
   vault-less `unrestricted` session is provisioned no gate at all
   (`gateSpec`: `wantsGate := Limited || len(vaultIDs) > 0`) and the Docker
   backend networks it directly (`networkMode` returns `"bridge"`), so nothing
   in `internal/gate` can reach it. That arm is an executor and sandbox-backend
   question — always provision a gate, or enforce at the network layer — and it
   becomes **its own issue** rather than being decided here. The alternatives
   were provisioning a gate for every session and enforcing in both backends.
2. **The refusal matches the recording**: 403 with the reference's own body,
   rather than the 502 `cannot reach host` a failed dial surfaces today. What
   an agent reads should say it was refused by policy, not that the host was
   unreachable.
3. **The empty-authority bypass rides along.** Found in #596's review and
   deferred here deliberately: `CONNECT :443` gives `hostOnly` an empty host,
   and `admit` short-circuits on `admitAll` before any host is examined, so the
   class came back `admitUnrestricted` rather than `admitNone`. `":443"` is
   Go's documented "local system" form, so the gate dialled loopback in the
   namespace it shares with the sandbox.

## The change

**One line of policy.** `floored()` keeps `admitOperator` exempt and drops
`admitUnrestricted`:

```go
func (a admission) floored() bool { return a != admitOperator }
```

`admitAll` **stays**. It governs which *hosts* the policy admits, and the
reference admits every host — `example.com` answered 200 on the same
environment that refused the metadata address. Removing it would turn
`unrestricted` into something the recording does not show. The two questions the
`admission` type exists to keep apart are exactly the two the reference answers
differently, which is the argument for that type restated by evidence.

The remaining exemption is the one that was always the point: a host in
`allowed_hosts` is an operator naming a destination, and naming a private
address there is the vouching. `floored()`'s doc comment already says it is
written as the exemptions rather than the members so a class added later is
floored by default; it now has one exemption instead of two, and the sentence
about both widening flags becomes a sentence about the one class that is not a
widening at all.

**The refusal's shape.** `dialguard.ErrRefused` already travels out of the
dialler; both handlers currently flatten every dial error into
502 `cannot reach host`. They separate the two:

- `errors.Is(err, dialguard.ErrRefused)` → **403**, body
  `Destination IP is in a private/reserved range`
- anything else → 502, unchanged

This moves the floor's refusal for the **other** floored classes too — an MCP
endpoint or a package registry that resolves to a refused address answered 502
before and answers 403 now. That is the same correction: a dial the policy
stopped was being reported as a host that did not answer. The tests that used
the two status codes to prove *which* mechanism refused keep the distinction and
read the body for it, which is what the reference's own 403 carries.

`handleConnect` reads its error from `g.dial` directly. `handlePlain` reads it
from `transport.RoundTrip`, which wraps it — the test asserts `errors.Is`
survives that wrapping rather than assuming it, because if it does not the
plain-HTTP path silently keeps answering 502.

**The empty authority** is refused before `admit` is asked, in both handlers,
because `admit` is the thing that cannot refuse it: `admitAll` answers before
any host is examined. Under `limited` it was already closed — an empty host
matches no set — so what this adds is the `unrestricted` arm, where the dial
went to the local system. Flooring `unrestricted` would also have caught it,
and the explicit check is kept anyway: it answers 403 rather than depending on
what an empty address happens to resolve to, and it holds for a class the floor
exempts. `handlePlain` carries the same shape through `http://:80/x`, and there
it carried more — a credential whose own arm is `unrestricted` ignores its
`Hosts` list, so its secret would have been substituted into a request
delivered to that loopback listener.

## What this does not do, stated rather than implied

A vault-less `unrestricted` session has **no gate in its egress path**, and this
plan does not give it one. For those sessions the metadata endpoint stays
reachable, and the new issue carries the decision. Both the changelog fragment
and `docs/DIVERGENCES.md` say which sessions are covered, so nobody reads
"`unrestricted` now has an address floor" as covering the deployment shape where
it does not.

The gate is also an environment-variable forward proxy, where the reference
intercepts transparently — a process that clears its own proxy variables leaves
through neither ours nor theirs identically. That is a separate recorded
divergence and #570 explicitly is not about it.

## Tests

1. **The class decides, and now decides differently.** In `internal/gate`, an
   `admitUnrestricted` dial to a private address is refused where it was
   admitted; `admitOperator` still is not. The existing per-class assertions
   move with it.
2. **Every host still leaves.** `unrestricted` admits a public name and dials
   it — the half of the recording that is not a refusal.
3. **The refusal's shape**, on both handlers: 403 and the reference's body for
   `ErrRefused`, 502 for an ordinary dial failure. The plain-HTTP case drives a
   real `RoundTrip` so the wrapping is tested rather than assumed.
4. **The empty authority** under `unrestricted` is refused rather than dialled,
   driven through a real listener the way #596's review drove it.
5. **Mutation testing**, per the repo rule: every guard above gets a mutant that
   removes it, and each must die by a *named* test — not by a build failure and
   not by a hang. Seven mutants, seven killed, no survivors: the exemption put
   back on `unrestricted` and taken off `allowed_hosts`, the empty-authority
   check removed from each handler, the refusal flattened back into a 502, and
   the 403 stripped of first its reason and then its status.

## Docs

- `changelog.d/` — one fragment, naming which sessions gain the floor.
- `docs/DIVERGENCES.md` — the egress row's `unrestricted` line becomes the
  narrower statement, and the dial-address-floor row gains the gate.
- `internal/gate/policy.go` — the comment that deferred to #570 is replaced by
  what the recording answered.
- `STATE.md`, and this file's status.
