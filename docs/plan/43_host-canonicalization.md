---
status: archived
issue: "#609"
---

# Plan 43 — one host comparison, canonicalized

Close the second half of [#609](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/609):
replace three hand-rolled ASCII case folds with one canonicalizer that compares
IDN hosts by their A-label, and validate an environment's
`networking.allowed_hosts`, which today is stored as arbitrary strings.

The first half merged as `766878f` (#611): `internal/mcp`'s `sameHost` stopped
folding by Unicode. That fix is correct and closes a real leak, but it buys the
leak with a withholding cost this plan removes.

## Why a plan and not a patch

Three of the decisions here are wire-observable or change behaviour for every
egress caller, and one of them contradicts the rationale the first half shipped
with. Everything below was **measured** against the pinned `golang.org/x/net`
v0.57.0 and the pinned SDK v1.70.1, not reasoned about.

## What is actually true

### `strings.EqualFold` merges what IDNA separates, and separates what IDNA merges

| pair | `EqualFold` | today's ASCII fold | IDNA A-labels | one domain? |
|---|---|---|---|---|
| `σ.example` / `ς.example` | same | different | `xn--4xa` / `xn--3xa` | **no** — the leak |
| `Ä.example` / `ä.example` | same | different | both `xn--4ca.example` | yes |
| `Д.example` / `д.example` | same | different | both `xn--d1a.example` | yes |
| U+212A (the Kelvin sign) before `.example` / `k.example` | same | different | both `k.example` | yes |
| `ſtrasse.example` / `strasse.example` | same | different | both `strasse.example` | yes |
| `İ.example` (U+0130) / `i.example` | different | different | `xn--i-9bb` / `i` | no |

The ASCII fold gets row 1 right and rows 2–5 wrong. A-label comparison gets all
six right.

### No two code points merge, and the conversion is stable on its own output

Every one of the 1,112,064 Unicode code points was swept through
`idna.Lookup.ToASCII("a"+cp+"z.example")`; the 140,873 distinct A-labels
produced were fed back through. **Zero** moved, and **zero** collided with
another A-label. Every merge the sweep found is a UTS46 equivalence class — one
name, several spellings.

That is the claim, and it is narrower than "injective". One code point sits in
one label position, and each output is fed back once, so what the sweep answers
is the question the orbits above raise — whether two code points can be brought
together — and not whether two arbitrary multi-code-point names can be. It also
says nothing about registrability: `idna.Lookup` verifies neither DNS lengths nor
that anyone could register what it returns.

Four of those A-labels — the ones made from U+2135–U+2138 — *error* on the way
back rather than returning cleanly. The string they return is the string they
were given, so nothing moves and `CanonicalHost`'s error fallback answers the
same either way; "zero moved" is the exact claim, and it is not the same claim as
"the round trip is clean".

### But `idna.Lookup.ToASCII` refuses inputs this platform accepts today

The `Lookup` profile validates. It errors on eight shapes that reach this code
now and are normalized silently: `*`, `:`, `[`, `_`, a space, a leading- or
trailing-hyphen label, and — the one that surprises — **a plain-ASCII label with
hyphens in positions 3 and 4**, such as `ab--cd.example.com` or
`s3--us-west-2.example.com`, which `egress.ValidateHostEntry` accepts today.

A differential matrix over 30 entry/lookup pairs scored four designs: today, 0
violations; strict IDNA everywhere with fail-closed, **11**; strict IDNA with an
ASCII fallback, 1; the hybrid this plan adopts, 1. All eleven regressions are
plain-ASCII or IP shapes — not one is an IDN.

### `ToASCII` returns a *different real hostname* along with its error

`idna.Lookup.ToASCII("xn--pypi-.org")` returns `("pypi.org", err)`. Likewise
`xn--paypal-.com` → `paypal.com` and `api.xn--corp-.internal` →
`api.corp.internal`. An implementation that canonicalizes and drops the error
makes a stored entry admit a domain nobody typed. `NewHostSet` and
`CanonicalLookup` have no error channel, which is exactly the shape that invites
this mistake.

### net/http does not do what #611's commit message says it does

`766878f` cites `net/http` as comparing A-labels on both sides. It does not.
`idnaASCII` (`request.go:781`) opens with `if ascii.Is(v) { return v, nil }` — an
all-ASCII host is returned unchanged, **without even case folding** — and
`idnaASCIIFromURL` (`transport.go:3024`) discards the error and keeps the
original host. Measured end to end: a redirect from `Example.test` to
`example.test` **strips** `Authorization`, while every IDNA orbit keeps it. The
shape to copy is "ASCII fast path, else canonicalize, fall back on error"; the
function itself must not be copied, because its fast path would undo the fold
#611 just landed.

### What the reference publishes

Read from the pinned SDK's bundled OpenAPI spec (`scripts/mock-spec.json.gz`,
v1.70.1) — the two `allowed_hosts` fields are constrained very differently:

- **Environment** (`BetaLimitedNetwork`): `{"items": {"type": "string"}, "type":
  "array"}`. No `pattern`, no `maxItems`, no `format`; the description is only
  "Specifies domains the container can reach." **The reference publishes no
  grammar and no cap here, and documents no rejection.**
- **Credential** (`BetaManagedAgentsLimitedCredentialNetworking`): the grammar is
  published — "Each entry is a bare hostname (`api.example.com`), an IPv4 address
  (`192.0.2.1`), or a `*.`-prefixed wildcard (`*.example.com`). URLs, ports,
  paths, and IPv6 addresses are not accepted. At most 16 entries." The response
  schema adds the matching rule: "An entry matches the request host exactly."

Neither says anything about a Unicode hostname. Whether the reference accepts a
U-label, and what it echoes back, is **unobserved**.

## Decisions

Settled by the repository owner, 2026-09-05.

1. **The comparator becomes A-label comparison**, unified behind one helper, in
   `internal/egress`, `internal/vaultresolve` and `internal/mcp`.
2. **The implementation is the hybrid**, not strict IDNA everywhere — because
   strict costs eleven working configurations and buys nothing the hybrid does
   not.
3. **`networking.allowed_hosts` is validated at create and update**, rejecting
   invalid entries.
4. **Stored rows are not migrated.** Only new writes are gated.
5. **A U-label entry is accepted** and its A-label is stored, on **both** the
   environment list and the credential list.
6. **The gate dials the canonical name**, not the raw one.

## The design

### One helper, `egress.CanonicalHost`

```go
// CanonicalHost returns the form of h that two spellings of one host agree on.
func CanonicalHost(h string) string
```

Its rules, in order. Each exists because a measurement demanded it.

1. **Fold to ASCII lowercase first, and do nothing else to the string.**
   `CanonicalHost` does not trim whitespace and does not strip a trailing dot:
   that is `CanonicalLookup`'s job, which wraps it for the callers wanting both.
   `internal/mcp` is why — it treats a trailing dot as a deliberate
   fail-closed difference ("drop the token rather than send it"), so de-rooting
   inside the canonicalizer would silently widen the one path that sends a
   bearer token. The fold must lowercase: `net/http`'s equivalent does not, and
   copying the function rather than the shape would undo #611.
2. **Cap the length, at 1024 bytes.** A host over the cap is returned folded,
   uncanonicalized, and a *Unicode entry* over it is refused — an ASCII one needs
   no arm, since neither side converts it and both fold it, so they still meet.
   A sandbox writes the
   CONNECT authority and `net/http` bounds it only by `MaxHeaderBytes`; a 1 MB
   U-label authority costs 22.6 ms through IDNA against 2.1 ms through the fold.
   It is a cost bound, not a proof. 4 × 253 is 1012 — UTF-8's widest code point
   against the bytes an A-label holds — which covers every input whose code
   points survive UTS46 one for one, and 1024 rounds it up; but UTS46 *deletes*
   the ignorable code points, so an input of any length can map to a short name
   (measured: 10,000 soft hyphens before `example.com` canonicalize to
   `example.com`, and the cap declines to find that out). What the number must
   not be is small: a 255-byte cap was measurably wrong, because three 45-rune
   `ä` labels are 284 bytes of UTF-8 and a 167-byte A-label — an ordinary name
   the matcher then could not produce while the entry side stored it. Above the
   cap the empty-label check still has to hold on its own,
   which is why `hasEmptyLabel` counts U+3002, U+FF0E and U+FF61 as separators —
   measured, an over-cap host led by U+3002 carried no ASCII empty label and
   walked straight through a `*.example.com` boundary.
3. **ASCII branch — a cost guard that turned out to carry a rule.** If every
   byte is below 0x80, return the fold. It was written for cost: IDNA costs
   127.7 ns against the fold's 42.6 ns on a path the gate runs for every
   request, and nearly every all-ASCII host either passes `ToASCII` unchanged or
   is refused by it and reaches the same folded string through rule 4. One
   family does neither. A label of `xn--` whose punycode payload is empty is
   **rewritten, with no error**: measured, `ToASCII("xn--.example")` is
   `(".example", nil)` and `ToASCII("a.xn--.z")` is `("a..z", nil)`. Without this
   branch a malformed A-label would become a string carrying an empty label —
   the one shape rule 4's own reordering exists to keep away from a wildcard
   boundary. The branch is therefore load-bearing, and the code says so.
4. **Otherwise canonicalize.** `idna.Lookup.ToASCII`. **On error, discard the
   returned string entirely** and fall back to the ASCII fold of the original —
   on both the entry side and the lookup side, so the two can never desynchronize.

This is one rule shorter than the design started with. A guard routing `:`,
`[`, `%` and IP literals away from IDNA was written, and then mutation testing
showed deleting it changed no output: every shape it caught, `ToASCII` refuses,
and rule 4 returns the same string the guard would have. Dead code a test cannot
distinguish is worse than no code, so it went, and the comment beside rule 4
says where those shapes are handled instead.

### `NormalizeHost` becomes `CanonicalLookup`, and the order inside it flips

The design started with `NormalizeHost` untouched — fold, then strip one
trailing dot — and every caller composing it with `CanonicalHost` by hand. That
composition has an order, and the obvious one is wrong. `NormalizeHost` stripped
the dot *before* anything converted the host, so it could only see a dot
somebody wrote as U+002E; IDNA maps U+3002, U+FF0E and U+FF61 onto `.` as well,
so `example.com。` is a rooted spelling that the strip could not recognise. What
it left behind was a trailing empty label, and `hasEmptyLabel` — running, per
`Match`'s rule, on the canonical form — then refused the host outright. Measured:
all three non-ASCII full stops stopped matching `example.com`, and an entry
written with one was dropped from the set entirely.

So the two collapse into one exported function, `CanonicalLookup`, that trims,
canonicalizes, and *then* de-roots. Stripping again afterwards is not the fix —
`example.com..` would lose both dots and match — so it is one strip, after.
Three consequences follow, and all three are wanted: a rooted Unicode spelling
is now the same host as its bare form; there is one name for the comparison
instead of two that can drift apart; and a scoped address keeps its zone
identifier at admission too, because `CanonicalLookup` delegates to
`CanonicalHost`, which holds the zone out of the fold. That last one closes a
gap this plan had listed as out of scope (below).

`CanonicalHost` is a new function either way, so every call site opts in
deliberately rather than inheriting a changed meaning; the rename makes the same
true of the callers that only ever wanted the lookup form.

### Where it is called

- **`NewHostSet` and `CoversEntry`: convert the remainder after
  `strings.CutPrefix(e, "*.")`.** `ToASCII("*.example.com")` errors on U+002A,
  so the conversion has to see the entry with its prefix off or a Unicode
  wildcard never becomes an A-label at all. Mutation testing sorted out three
  claims here, and the middle one was wrong the first time it was argued.
  *Converting* the whole entry before the cut as well would indeed be harmless,
  because the asterisk's error sends `CanonicalHost` back to the fold and
  `CutPrefix` sees the same string. Running the whole **lookup** form there is
  not, because `CanonicalLookup` also de-roots and de-rooting does not survive
  repetition: it takes one trailing dot off per pass, so a second pass turns the
  entry `example.com..`, whose last label is empty, into the live key
  `example.com`. And dropping the conversion after the cut breaks a Unicode
  wildcard outright. A named row fails for each of the two mistakes.
- **`Match`: refuse a host over the cap, then canonicalize the whole of what is
  left and run `hasEmptyLabel` on the canonical value, not the raw one.** IDNA
  maps U+3002, U+FF0E and U+FF61 to `.`, so it *creates* label boundaries:
  `。example.com` canonicalizes to `.example.com`, which would then satisfy
  `HasSuffix(host, ".example.com")` and slip past the `*.example.com` boundary
  that `Match`'s doc comment exists to guard.

  The refusal above the cap is not a cost guard but the same rule again, and it
  was missing until the review found it. Above the cap nothing is converted, so
  the comparison ran on raw bytes — and raw bytes carry a label that is empty
  only after UTS46 deletes what fills it. Measured: 507 SOFT HYPHENs before
  `.example.com` is 1026 bytes, carries no ASCII empty label, and matched
  `*.example.com`, while the same host seven soft hyphens long is refused. Since
  no host a request can resolve is written that long — an A-label holds 253
  bytes — refusing outright is both the fail-closed answer and the one the cap's
  own comment had been promising.
- **The trim belongs to the whole entry, and the de-rooting stops at the zone.**
  Both are `canonicalDeRooted`, the lookup form without the trim.
  `CanonicalLookup` trims and then calls it; `NewHostSet` and `CoversEntry` trim
  the entry once and call it on the halves. Trimming below the `*.` cut instead
  lands *inside* the entry: measured, `*. example.com` became a live wildcard
  over `example.com` where it used to key ` example.com` and match nothing. And
  the de-rooting splits at `%` the way `CanonicalHost` does, because the dot it
  removes belongs to the name — otherwise `fe80::1%eth0.` and `fe80::1%eth0`
  become one interface.
- **`vaultresolve.lowerHost` and `mcp.sameHost`** delegate to it, keeping their
  own extra normalization — the empty-port trim, the port split — around it.
  Neither splits the zone identifier itself, though both did at first: review
  showed the split was the function they were wrapping. `CanonicalHost` splits at
  the first `%` and appends the remainder byte for byte, and `canonicalName`
  cannot emit a `%`, so the first `%` of the result is exactly that boundary and
  comparing the whole strings says what comparing the halves would. Three copies
  of the zone rule became one.
- **`endpointKey`, in both spellings** (`internal/gate/policy.go` and
  `internal/api/gateconfig.go`), moves with them, and the two spellings are not
  the same kind of change. The gate's is **observable**: `policy.admit` hands it
  the host the sandbox wrote, which never passed any entry grammar, so a session
  declaring `xn--bcher-kva.example:8443` and a request written `bücher.example`
  meet only because of it — measured, and covered by a test. The control plane's
  is a no-op today, because `mcpEndpoint` runs `ValidateHostEntry` two lines
  above and that refuses a non-ASCII host; it moves anyway so the two sides agree
  by construction rather than by both happening to see ASCII, and its comment
  says exactly that.

### Validation

`ValidateHostEntry` is left alone; a new `egress.CanonicalEntry` wraps it as a
canonicalizing front end. A non-ASCII entry has its `*.` prefix cut, the rest
converted to its A-label, the prefix restored, and the result validated as an
ASCII entry; an ASCII entry goes straight to the grammar it passes today,
including the `ab--cd` shape `ToASCII` would refuse — and is stored exactly as
typed, case included, because the matcher folds. A conversion error is the
entry's rejection, never a fallback: an entry is written once and matched
forever, so `xn--pypi-.org` must not become a `pypi.org` nobody typed.

All three writers call it. The environment's is `parseNetworking`, which create
and update share, so one call covers both write paths; the credential's is
`vaultcredauth.go`'s `canonicalAllowedHost` (was `validateAllowedHost`, renamed
because it now returns the form to store); the third is `cmd/executor`'s
`splitDomains` over `WEBTOOL_ALLOWED_DOMAINS`, which otherwise would have been
the one list that refused at startup the Unicode name the matcher canonicalizes.
Per decision 4 the environment check runs on the entries the patch newly supplies
rather than on the merged list, and carries through an entry the row already
holds so the ordinary read-modify-write is not refused on a value this API
returned one call earlier — refusing
an update over what an earlier one stored would take a working environment's
egress away for a field nobody is touching.

### The gate dials what it admitted

`internal/gate` today calls `admit(hostOnly(target))` and then
`g.dial(ctx, "tcp", target)`. Once admission canonicalizes, those two strings can
differ by a whole name — `exa<U+00AD>mple.com` and `example<U+3002>com` both
canonicalize onto `example.com`, and U+212A before `.example` onto `k.example`,
none of them with an error. The dial takes the canonical
name. The CONNECT tunnel is opaque and the client inside it sends its own SNI, so
nothing above the socket observes the change; and without it the widening is a
false promise, since `net.Dialer` on a raw U-label answers "no such host".

The substitution declines one authority, and review is how that was found. UTS46
deletes the ignorable code points outright, so an authority written as a single
SOFT HYPHEN canonicalizes to the empty string, and `net.JoinHostPort("", "443")`
is `":443"` — which Go's resolver reads as the *unspecified* address, a
destination the policy never admitted and, on the gate's own host, a local
service. An empty canonical name therefore leaves the authority as written, which
fails to resolve exactly as it did before there was a canonical dial at all.

## What this costs, stated plainly

- **Accepted narrowing.** An IDN wildcard entry whose lookup fails
  canonicalization stops matching: entry `*.bücher.example`, lookup
  `_acme.bücher.example`, true today and false after. The entry canonicalizes and
  the lookup falls back, so the two land in different alphabets. It errs toward
  refusal, which is the direction this gate must err in.
- **A claim in `766878f` must be retired.** Its `asciiEqualFold` comment counts
  separating two invalid UTF-8 bytes as a win. Measured: five distinct invalid
  byte strings all canonicalize to `xn--host-w70y.example` with no error, and
  `net/http` dials that A-label for all five. The socket merges them; the ASCII
  fold's separation was the anomaly, not a protection.
- **Cost.** `NormalizeHost` 42.6 ns/op, `ToASCII` on an ASCII host 127.7 ns/op, on
  a U-label 163.1 ns/op; the ASCII fast path puts a typical request back at
  47.5 ns. `admit` normalizes at most three times per request. Not an argument
  either way, which is why rule 2's length cap is the only performance rule here.

## Divergences to register

Three, all INFERRED, in `docs/DIVERGENCES.md`:

1. **Environment `allowed_hosts` gains a 400.** The reference's schema constrains
   the field to `array<string>` and its docs document no rejection for it, while
   documenting one for `allow_package_managers` on the same page — so it knows
   how to say "rejected" and does not say it here.
2. **A U-label entry is echoed back as its A-label** on both lists. The operator
   reads back punycode they did not type. Unobserved on the reference.
3. **The registered MCP credential-matching reading changes.**
   `docs/DIVERGENCES.md`'s existing entry says "host case is folded as ASCII only,
   because Unicode folding maps `İ` onto `i` while Go resolves the two to
   different DNS names". That reason survives — `İ` and `i` stay apart under IDNA
   — but the rule becomes A-label comparison, and the published "matches the
   request host exactly" is now read as "matches its canonical form".

## Out of scope

- **#601** — that a declared MCP endpoint resolves through the resolver's search
  list. The gate dialing the canonical name narrows the admitted-versus-dialled
  gap to the name; #601 closes the name-versus-address half by resolving once.
- **A zone identifier folded into the comparison** — listed here as out of
  scope, and then closed anyway. `CanonicalHost` splits at the first `%` and
  returns the remainder untouched, and `CanonicalLookup` inherits that, so
  `fe80::1%eth0` and `fe80::1%ETH0` are now two hosts everywhere: at admission,
  at the dial, and in `internal/mcp` and `internal/vaultresolve`, which already
  kept them apart. It falls out of the same reordering that made a trailing
  U+3002 a rooted spelling, and it errs toward refusal, so it lands here rather
  than waiting for its own change. #609's note that this was unaddressed is
  retired with it.
- **Migrating stored rows** (decision 4), and any cap on the number of entries —
  the reference publishes one for credentials (16) and none for environments, and
  neither is this issue's subject.

## Definition of done

`make verify` green; the shared helper and every call site covered by mutation
tests that each fail for the row that names them — one per rule above, including
the conversion after the wildcard cut, the post-canonicalization empty-label
check, the refusal above the cap, the trim that stays above the cut, the
de-rooting that stops at the zone, the discarded error on the mangling input, the
lowercasing fast path, the length cap in both directions, the zone identifier,
and the canonical dial; the three divergences registered; a `changelog.d/`
fragment; and the verifier plus both reviewers run per the repository's ritual.

Thirty-three mutants across the two suites, thirty-one killed.

Two mutants survive, and each is stated rather than hidden. Dropping
`NewHostSet`'s empty-label check removes only keys that could never have matched,
since `Match` refuses an empty label on the request side first; it is defence in
depth, and no test can distinguish it. The other is the control plane's
`endpointKey`, argued where it is called.

A third was claimed as a survivor here and the claim was false — recorded rather
than quietly dropped, because the error is the instructive part. Running the
whole lookup form before the wildcard cut as well as after was argued to be
output-equivalent; it is not, because de-rooting does not survive repetition, and
the entry `example.com..` becomes the live key `example.com` under it. What hid
the difference was a test gap on the entry side, not an inert mutant. Both are
closed.
