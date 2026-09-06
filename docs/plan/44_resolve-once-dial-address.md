---
status: archived
issue: 601
---

# Plan 44 — resolve once, floor the result, dial the address

#601 records a residual #596 left open: the gate admits a *name*, egress
substitution injects a credential on that *name*, and the socket then goes
wherever the resolver points at dial time. #596 closed one way those disagree —
the `search` list — for one of four admission classes, by rooting the dial
address for a package registry. The other three classes were left as they were,
and the executor's own MCP dial was never in that fix at all.

The owner settled on the fourth of the issue's six options:

> Resolve the name once in the handler, run the address floor on that result,
> and dial the resolved IP.

This plan is that option, and nothing else. The cost the issue names for it —
re-implementing the multi-address and dual-stack fallback dialling `net.Dialer`
provides today — is paid here rather than avoided.

## The invariant

**One dial, one resolution.** Every outbound connection this platform makes on a
customer-supplied or agent-declared name resolves that name exactly once, judges
the addresses that came back, and connects to those addresses — never to a name
a lower layer resolves again on its own.

Today the resolution happens inside `net.Dialer`, below every decision the
platform makes. `admit` sees a name; `Substitute` sees a name; the address
appears for the first time in the `Control` hook, one syscall before `connect`,
where the floor is the only thing that can read it. After this change the
resolution is the platform's: it happens where the class is known, its result is
the floor's input, and it is what the socket uses.

## What this closes, and what it does not

It is worth being exact, because #601's own text overclaims on this point and
the decision was taken partly on it.

**Closed.**

- *The decision and the socket refer to one resolution.* Nothing below the
  gate re-resolves. A name that answers differently on a second lookup cannot
  send the connection somewhere the floor never judged.
- *The address becomes an input the request path holds.* This is the enabling
  half. `admit` and the floor are today separated by a resolution neither can
  see; afterwards the class and the addresses are known in the same place, which
  is what #570 needs to give `unrestricted` a floor at all.
- *One dial shape.* The five callers of `dialguard.Control` become five callers
  of one dialler, so "what does this platform do before it connects" has a
  single answer rather than five copies of a hook.

**Not closed, and this plan does not pretend otherwise.**

#601 writes that resolving once "closes the search-list gap … uniformly for
every class". It does not. A single resolution of `api.example.com` under
`ndots:5` still consults the `search` list first, still answers from an internal
zone, and `dialguard.IPAllowed` still admits the RFC 1918 address it returns —
deliberately, because on-prem MCP servers live there (see the package comment).
The credential is still chosen by name and still delivered to that address.

No rule about the *address* can separate the two cases, and this is the reason
the issue needed a policy decision rather than a patch:

| | declared host | class | resolved address | credential |
|---|---|---|---|---|
| legitimate | `nexus.infra:8080` | MCP | 10.0.0.7 (search-completed) | matched by name |
| the leak | `api.example.com:80` | MCP | 10.0.0.5 (search-completed) | matched by name |

The rows are identical in everything the gate can observe. What separates them
is *which name answered* — absolute or search-completed — which is the question
the issue's options 1, 2 and 3 are about and which the owner did not pick. That
half stays open; the "Left open" section below says how it is tracked.

## The primitive

A new file in `internal/dialguard`, the package that already owns the floor and
that all five call sites already import.

```go
// Dialer connects by resolving a name once, refusing the addresses the floor
// refuses, and dialling what survives.
type Dialer struct {
	Timeout       time.Duration
	FallbackDelay time.Duration // 0 selects net.Dialer's own 300ms
	// Lookup resolves a name to addresses. network is "ip", "ip4" or "ip6" —
	// the spelling net.Resolver.LookupIP takes — so nil selects
	// net.DefaultResolver.LookupIP and a "tcp4" dial still resolves only A
	// records.
	Lookup func(ctx context.Context, network, host string) ([]net.IP, error)
	// Allow judges every resolved address before any connect. Nil selects
	// IPAllowed. The context is passed because a caller's answer can depend on
	// it — the gate's floor is per admission class, and the class travels in
	// the request context.
	Allow func(ctx context.Context, ip net.IP) error
}

func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error)
```

`DialContext` is a drop-in for `(&net.Dialer{…}).DialContext`, so every call site
keeps the shape it has.

Behaviour, in order:

1. `net.SplitHostPort`. What it refuses is dialled unchanged — the same
   fail-as-before rule `rootedName` already argues in place.
2. An address literal skips the lookup and is judged directly. This is not an
   optimisation: it is what keeps a literal's refusal identical to today's.
3. Otherwise one `Lookup`. An empty answer is the resolver's own error.
4. `Allow` runs on **every** returned address before any connect. A refused
   address is dropped, not attempted. If every address is refused, the first
   refusal is returned — so the message a caller surfaces is still
   `dialguard.ErrRefused`, which `internal/vaultresolve/mcprefresh.go` already
   tests with `errors.Is`.
5. The survivors are dialled (next section).

`Control` is deleted once the last caller stops using it. It is ours, it was
introduced by this line of work, and leaving two mechanisms that answer the same
question is exactly the drift this plan exists to remove.

### Multi-address and dual-stack dialling

`net.Dialer` gives three things a bare `DialContext` to a literal does not, and
all three have to be reproduced or they are a regression on every dual-stack
deployment:

- **The timeout's scope** — `net.Dialer` applies its `Timeout` at the top of
  `DialContext`, *before* it resolves, so the bound covers the lookup as well as
  the connects. Applying it any later leaves a hanging resolver bounded only by
  the caller's context, which for the gate is the sandbox's own request.
- **Multi-address failover** — try the next address when one fails.
- **Per-address deadline** — the remaining budget divided by the addresses
  left, so one blackholed address cannot consume the whole `Timeout`.
- **Happy Eyeballs (RFC 6555/8305)** — the other family started after
  `FallbackDelay`, first success winning, so a broken IPv6 path costs 300ms
  rather than a connect timeout.

The implementation mirrors Go's own: partition by the family of the first
address, run each partition serially, race the two with the fallback delay, and
return the primary partition's error when both fail. It is the largest single
piece of the change, and it is the named cost of this option.

One unexported seam goes with it. `dialOne` stands in for the socket, is unset
in production, and exists because the case the fallback delay is *for* — an
address that neither answers nor refuses — cannot be driven against a real
network deterministically, and driving it against a real IPv6 address would make
the test depend on whether the machine running it has IPv6 at all.

## Per-class semantics stay exactly as they are

This plan changes *where* the resolution happens, not *who is floored* and not
*what is rooted*.

- `admission.floored()` keeps its two exemptions. Flooring `allowed_hosts` or
  `unrestricted` is #570's decision, not this one, and doing it here would
  reverse a plan 12 argument silently.
- `admission.rooted()` keeps the registry class alone. `rootedName` still
  rewrites the dial address before the dialler sees it, so the single lookup is
  of the rooted name for that class. #596's fix is unchanged, and the doc
  comment's argument for the other three classes now points at this plan for the
  half it does not answer.
- `handleConnect`'s canonical dial address, `endpointKey`, and everything plan
  43 settled about host comparison are untouched.

## Call sites

| site | today | after |
|---|---|---|
| `internal/gate/gate.go` | `net.Dialer{ControlContext: floor-if-floored}` | `dialguard.Dialer{Allow: floor-if-floored}`, still wrapped by `rootedDial` |
| `internal/mcp/mcp.go` | `net.Dialer{Control: …}` in `guardedClient` | the primitive, both clients |
| `internal/identity/fetch.go` | `net.Dialer{Control: …}` | the primitive |
| `internal/api/vaultvalidate.go` | `net.Dialer{Control: …}` over `probeIPAllowed` | the primitive, seam kept |
| `internal/vaultresolve/mcprefresh.go` | `net.Dialer{Control: …}` over `refreshIPAllowed` | the primitive, seam kept |

Nothing above changes what any of them may reach. The three non-gate callers
have no admitted-name-versus-dialled-name divergence to close — they dial a URL
with no host allowlist above it — and they are converted so that one dialler is
the whole answer, not because they are broken.

TLS server names and `Host` headers are untouched by construction: the rewrite
happens inside `DialContext`, which receives the address and returns a
connection, and neither `http.Transport` nor the CONNECT tunnel consults it for
anything else. That is the same reason #596 put the rooting in the dialler
rather than the handlers.

## Tests

The resolution seam is what makes this testable at all, so the suite drives it
rather than the network.

1. **One lookup per dial.** A `Lookup` counting its calls: one connection, one
   call. This is the invariant; a mutant that resolves again inside the dial
   loop dies here.
2. **The floor judges every returned address.** Two addresses, one refused: the
   refused one is never connected to, and with both refused the error is
   `ErrRefused`.
3. **The address dialled is the address judged.** A `Lookup` returning a
   loopback address for an innocent name is refused, where the same name
   dialled through `net.Dialer` today would be refused only by `Control` — the
   point being that the refusal now precedes the socket.
4. **Failover.** First address refuses connections, second answers: the dial
   succeeds on the second.
5. **Happy Eyeballs.** A dual-family answer whose primary family blackholes
   completes within a small multiple of `FallbackDelay`, not of `Timeout` — and
   not *before* it either, which is what catches a race started too early. The
   per-address share of the budget is driven directly, either side of its floor.
6. **Literals and malformed addresses are unchanged.** `[::1]`, `:443`,
   `[[::1]]:443` and a bare IPv4 literal behave as they do today.
7. **The class still decides.** In `internal/gate`, an `admitOperator` dial
   reaches an address the floor refuses and an `admitMCP` one does not — the
   existing assertions, re-pointed at the new dialler.
8. **Mutation testing**, per the repo rule: every guard above gets a mutant
   that removes it, and each must fail a named test. Nineteen mutants, nineteen
   killed. Two survived the first pass and both were real: an all-refused answer
   was still reporting `ErrRefused` through a generic fallback rather than the
   refusal that names the offending address, and `netip.Addr.WithZone("")` before
   `AsSlice()` turned out to be a no-op — netip keeps a zone beside the address
   rather than in it — so a call that read like a guard was removed rather than
   given a test it could never fail.

## Docs

- `docs/DIVERGENCES.md` — the egress row records the current narrowing and
  #601; it gains what this closes and keeps the residual, still pointing at
  #601.
- `docs/self-hosted-security.md` — the paragraph on the search list and the
  residual (around line 662) gains the same distinction.
- `changelog.d/` — one fragment.
- `STATE.md`, and this file's status.

## Left open

The credential half of #601 — a name-matched secret reaching a search-completed
internal address — is **not** closed by this plan, and cannot be by any rule
about the resolved address. Closing it requires one of the issue's options 1–3:
rooting the MCP class outright, rooting it with an opt-out, or preferring the
absolute answer and falling back. Each trades reachability for the boundary in
a different place, and each is the owner's call. #601 stays open for it, with
this plan's "What this closes" section as the record of what the mechanical
half already does.
