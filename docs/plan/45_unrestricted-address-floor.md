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
every name is admitted, and an address underneath it can still be refused. What
the probe actually demonstrates is the **link-local** case, which is the one
that matters most — `169.254.169.254` is the cloud metadata endpoint — and the
reference's *message* names a wider class than its probe showed. This platform
refuses loopback, link-local, the unspecified address and multicast, and
**admits RFC 1918 by design** (the self-hosted premise `internal/dialguard`
argues in place), so `http://10.0.0.1/` is still dialled here. Whether the
reference refuses it is unrecorded; if it does, that is a divergence to keep
rather than a gap to close, and the registry's row says so.

This platform admits both halves today — `newPolicy` returns `admitAll` for
`domain.NetUnrestricted`, and `admission.floored()` then exempts that class from
the address floor as well.

Plan 44 is what makes this a small change: the gate's dialler now resolves a
name once and holds every resolved address to `admissionOf(ctx).floored()`
before any connect, so the floor is already wired to the class — it is only
being asked the wrong question for one of them.

## What the owner settled

Three decisions, taken 2026-09-06 against the alternatives named beside them:

1. **The floor lands in the gate, for the sessions that have one.** `gateSpec`
   provisions one only when a session *wants* it — `wantsGate := Limited ||
   len(vaultIDs) > 0` — **and** the deployment configured one: the same
   condition returns nil when `GateImage` or `ControlplaneURL` is empty, which
   is the chart's default. Everything else gets `networkMode`'s `"bridge"` and
   networks directly, so nothing in `internal/gate` can reach it. The uncovered
   set is therefore wider than "vault-less": it is every session no gate is
   provisioned for. That arm is an executor and sandbox-backend
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

**The refusal's shape.** Both handlers currently flatten every dial error into
502 `cannot reach host`. They separate the floor's refusal from the rest, keyed
on a marker the gate sets rather than on `dialguard.ErrRefused`: that sentinel is
wider than the floor, also wrapping an authority the dialler could not split and
a lookup that returned no address of a usable family — and the second is raised
*before* `Allow` runs at all, so matching it would tell an `admitOperator` dial,
the one class this floor never judges, that its destination was private or
reserved. The gate's `Allow` wraps `errFloorRefused` around a refusal of an
address it actually read, and around nothing else:

- `errors.Is(err, errFloorRefused)` → **403**, body
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

**The empty authority** is refused by `admit` itself, at the top, before any
set is consulted: `admitAll` answering before a host is examined at all is the
bypass, so the one line that fixes it belongs there, where it covers every
caller rather than each handler separately. Under `limited` it was already closed — an empty host
matches no set — so what this adds is the `unrestricted` arm, where the dial
went to the local system. Flooring `unrestricted` would also have caught it,
and the explicit check is kept anyway: it answers 403 rather than depending on
what an empty address happens to resolve to, and it holds for a class the floor
exempts. `handlePlain` carries the same shape through `http://:80/x`, and there
it carried more — a credential whose own arm is `unrestricted` ignores its
`Hosts` list, so its secret would have been substituted into a request
delivered to that loopback listener.

## What this does not do, stated rather than implied

A session the executor provisions no gate for has **no gate in its egress
path**, and this plan does not give it one — any `unrestricted` session with no
vault attached, and every session at all where the deployment configured no gate
image. For those sessions the metadata endpoint stays reachable, and the new
issue carries the decision. Both the changelog fragment
and `docs/DIVERGENCES.md` say which sessions are covered, so nobody reads
"`unrestricted` now has an address floor" as covering the deployment shape where
it does not.

One reachability change inside the gated shape is worth naming rather than
leaving to be discovered: a sandbox that curls its **own** loopback through the
proxy now gets the 403. `NO_PROXY` is forced empty by design, so a
proxy-honouring client's request to `127.0.0.1:<port>` goes to the gate, which
refuses the address like any other. Direct loopback is untouched — the firewall
does not stop a sandbox talking to itself without the proxy — and the reference
answers its own recorded probe of `127.0.0.1:1` with a connection failure rather
than a tunnel, so this is closer to it rather than further away. It is still a
shape that worked before and does not now.

The gate is also an environment-variable forward proxy, where the reference
intercepts transparently — a process that clears its own proxy variables leaves
through neither ours nor theirs identically. That is a separate recorded
divergence and #570 explicitly is not about it.

## Tests

1. **The class decides, and now decides differently.** In `internal/gate`, an
   `admitUnrestricted` dial to a private address is refused where it was
   admitted; `admitOperator` still is not. The existing per-class assertions
   move with it.
2. **Every host still leaves.** `unrestricted` admits a host no list names and
   dials it — the half of the recording that is not a refusal. It reaches an
   httptest origin, so what it drives is admission and the dial, not name
   resolution: an origin's authority is an address literal. The resolver half of
   the same dialler is held in `internal/dialguard`, where
   `TestTheProductionResolverIsWiredUp` drives a real lookup through to the
   floor's refusal.
3. **The refusal's shape**, on both handlers: 403 and the reference's body for
   the floor's own refusal, 502 for every other dial failure — the rest of
   `dialguard.ErrRefused` included. The plain-HTTP case drives a real
   `RoundTrip` so the wrapping is tested rather than assumed.
4. **The empty authority** under `unrestricted` is refused rather than dialled,
   driven through a real listener the way #596's review drove it.
5. **Mutation testing**, per the repo rule: every guard above gets a mutant that
   removes it, and each must die by a *named* test — not by a build failure and
   not by a hang. Eight mutants, eight killed: the exemption put back on
   `unrestricted` and taken off `allowed_hosts`, the empty-host check removed
   from `admit`, the refusal flattened back into a 502, the 403 stripped of
   first its wording and then its status, the floor's marker widened to every
   refusal `dialguard` produces, and the marker's own `ip != nil` half removed.
   The last of those survived its first pass and was real: nothing held the half
   that stops an address the dialler never read from being called
   private/reserved.

## Docs

- `changelog.d/` — two fragments: the floor and its refusal shape, and the
  empty-authority bypass, which is a security fix and is filed as one.
- `docs/DIVERGENCES.md` — the egress row's `unrestricted` line becomes the
  narrower statement, and the dial-address-floor row gains the gate.
- `internal/gate/policy.go` — the comment that deferred to #570 is replaced by
  what the recording answered.
- This file's status. **Not STATE.md**: it tracks active work, and by the time
  this landed its Active work was plan 41's, with the archived-plans sentence
  this would have joined already removed. An archived plan's record is its own
  frontmatter and docs/HISTORY.md.
