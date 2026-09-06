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

The residual is in fact **wider** than the search list, which matters for
whoever takes options 1 to 3: the same private answer arrives from split-horizon
DNS, or from a controlled zone publishing an RFC 1918 record for the declared
name absolutely. Rooting the lookup narrows the residual to those; it does not
end it. Both reviewers reached this independently, and the documents were
corrected to say the wider thing.

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
	// Lookup resolves a name to addresses. Nil selects
	// net.DefaultResolver.LookupIPAddr — LookupIPAddr rather than LookupIP,
	// because only the former keeps IPAddr.Zone. That is also why a "tcp4"
	// dial drops the AAAA answers here rather than asking the resolver for
	// one family: the network is not in this signature.
	Lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
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

1. A network with no host and port in its address — a raw `ip:proto`, a unix
   path — is **refused**. This type resolves a name and judges addresses, so
   there is nothing for it to do with one, and handing it to the standard
   dialler would open a socket the floor never saw. Every caller here dials TCP,
   so nothing loses reach; it is the one deliberate narrowing in the change.
2. The timeout starts here, before the lookup, which is where `net.Dialer`
   starts its own. A negative value is already expired, as `net.Dialer` reads
   one.
3. `net.SplitHostPort`. What it refuses goes to step 5 rather than to the
   standard dialler: that was the one route on which a socket could open
   without the floor having been asked anything, and it was safe only for as
   long as the standard dialler's parser stayed exactly as strict as
   `SplitHostPort`.
4. An address literal skips the lookup and is judged directly. This is not an
   optimisation: it is what keeps a literal's refusal identical to today's.
5. An authority step 3 could not split, or a host that is neither a name nor
   an address `net.ParseIP` reads — a zone-scoped literal, the empty host of
   `:443`, a bracketed `[foo:bar]` — is judged as an *unreadable* address and
   dialled unchanged if the caller admits it anyway.
   None of these could reach a socket for a floored class before either: a
   zone-scoped literal and an empty host errored inside the `Control` hook
   before its predicate ran, while an authority the parser or the resolver
   rejects never reached the hook at all. For a class the gate exempts from the
   floor no hook was installed, and all of them went to the standard dialler
   unchanged — which is what still happens, so each still fails, or connects,
   exactly as it did. Both halves have to survive or the change moves what a
   session can reach. The one difference is the kind of error a floored class
   gets for the shapes that used to fail below the hook: `ErrRefused` rather
   than a parse or lookup failure.
6. Otherwise one `Lookup`, whose answer is unmapped — `net.IP` carries an A
   record as sixteen bytes with the IPv4-mapped prefix, and dialling that
   literally would spell `198.51.100.1` as `[::ffff:198.51.100.1]`.
7. `Allow` runs on **every** returned address before any connect, in the
   resolver's order. A refused address is dropped, not attempted. If every
   address is refused, the first refusal is returned — so the message a caller
   surfaces is still `dialguard.ErrRefused`, which
   `internal/vaultresolve/mcprefresh.go` already tests with `errors.Is`, and it
   still names the address.
8. The survivors are dialled (next section).

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
  left, floored at two seconds, so one blackholed address cannot consume the
  whole `Timeout` and a long list does not reduce each attempt to a slice too
  short to finish a handshake in.
- **Happy Eyeballs (RFC 6555/8305)** — the other family started after
  `FallbackDelay`, first success winning, so a broken IPv6 path costs 300ms
  rather than a connect timeout. For network `"tcp"` alone, which is
  `net.Dialer`'s own condition (`d.dualStack() && network == "tcp"`): `tcp4` and
  `tcp6` have one family by construction, and it does not race udp.
- **The resolver's ordering, twice over.** Which family leads is decided by the
  *whole* answer, before the floor filters it — `net.Dialer` partitions before
  its `Control` hook runs, so a refused first address must not hand the lead to
  the other family. And with the race off, addresses are tried in the resolver's
  own order rather than regrouped by family.

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

Nothing above changes what any of them may reach, with one measured exception,
found by the re-verification and recorded here rather than smoothed over. The
deleted `Control` hook refused **every resolved address carrying a zone**:
`net.ParseIP` answers nil for one, so the hook errored before its predicate ran.
A zoned answer the floor admits is now dialled, zone kept. Observing it takes an
operator's own resolver answering a non-link-local name with a zone — link-local
is refused either way — and it is what `net.Dialer` does with the same answer,
but it is a difference and not nothing.

The three non-gate callers have no admitted-name-versus-dialled-name divergence
to close — they dial a URL with no host allowlist above it — and they are
converted so that one dialler is the whole answer, not because they are
broken.

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
6. **Literals and malformed addresses keep their old answers**, and this is
   the item two review passes rewrote: the first draft asserted it and tested
   only part of it. `[::1]` and a bare IPv4 literal are judged and dialled as
   before. Every shape the old hook refused *without consulting its
   predicate* — a zone-scoped literal, the empty host of `:443`, a bracketed
   `[foo:bar]`, and an authority that cannot be split at all — is driven on
   both sides: refused for a floored class, and handed to the standard dialler
   unchanged for an exempt one, where `[[::1]]:443` still fails exactly as it
   always did (`TestAnAddressTheFloorCannotReadKeepsItsOldAnswer`,
   `TestAnAddressThisCannotSplitFailsAsBefore`).
7. **The class still decides.** In `internal/gate`, an `admitOperator` dial
   reaches an address the floor refuses and an `admitMCP` one does not — the
   existing assertions, re-pointed at the new dialler.
8. **Mutation testing**, per the repo rule: every guard above gets a mutant
   that removes it, and each must fail a **named test** — checked, rather than
   assumed, by reading which test each kill came from: none dies on a build
   failure, and none dies by hanging the package until its own timeout (one
   did, and the test was bounded rather than the mutant retired, because a test
   that hangs on a regression reports it as a timeout instead of as itself).
   Twenty-nine mutants, twenty-nine killed. Four survived a pass and all four
   were real: an all-refused answer
   was still reporting `ErrRefused` through a generic fallback rather than the
   refusal that names the offending address, and `netip.Addr.WithZone("")` before
   `AsSlice()` turned out to be a no-op — netip keeps a zone beside the address
   rather than in it — so a call that read like a guard was removed rather than
   given a test it could never fail. The third came later: once an authority
   that cannot be split was folded into the floor, the default `Allow` refused
   the port-less networks anyway, so nothing was left holding `portCarrying`
   itself. Its own property is that the refusal does not depend on the caller's
   floor — a class the caller exempts still cannot dial a network with no
   address to judge — and that is what the test drives now. The fourth was
   written by the re-verification rather than by this suite: dropping the colon
   from the test for a host that is not an address left the package green,
   because every case reaching that branch also carried a percent sign or an
   empty host. A bracketed `[foo:bar]` reaches it by its colon alone, and is
   now one of the shapes item 6 above drives.

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
