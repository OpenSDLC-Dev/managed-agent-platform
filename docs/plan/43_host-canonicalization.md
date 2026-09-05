---
status: in-progress
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
| `K.example` (U+212A) / `k.example` | same | different | both `k.example` | yes |
| `ſtrasse.example` / `strasse.example` | same | different | both `strasse.example` | yes |
| `İ.example` (U+0130) / `i.example` | different | different | `xn--i-9bb` / `i` | no |

The ASCII fold gets row 1 right and rows 2–5 wrong. A-label comparison gets all
six right.

### A-label comparison is injective on registrable names

Every one of the 1,112,064 Unicode code points was swept through
`idna.Lookup.ToASCII("a"+cp+"z.example")`; the 140,873 distinct A-labels
produced were fed back through. **Zero** moved, and **zero** collided with
another A-label. Every merge the sweep found is a UTS46 equivalence class — one
name, several spellings. So canonicalizing cannot introduce a leak.

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
`NormalizeHost` have no error channel, which is exactly the shape that invites
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
   that is `NormalizeHost`'s job, and a caller composes the two when it wants
   both. `internal/mcp` is why — it treats a trailing dot as a deliberate
   fail-closed difference ("drop the token rather than send it"), so de-rooting
   inside the canonicalizer would silently widen the one path that sends a
   bearer token. The fold must lowercase: `net/http`'s equivalent does not, and
   copying the function rather than the shape would undo #611.
2. **Cap the length.** A host over 255 bytes is returned folded, uncanonicalized.
   A sandbox writes the CONNECT authority and `net/http` bounds it only by
   `MaxHeaderBytes`; a 1 MB U-label authority costs 22.6 ms through IDNA against
   2.1 ms through the fold. A longer name cannot resolve anyway.
3. **ASCII fast path — a cost guard, and nothing more.** If every byte is below
   0x80, return the fold. This branch changes no output and no test can tell:
   an all-ASCII host either passes `ToASCII` unchanged or is refused by it and
   takes rule 4's fallback to the same folded string. It is there because IDNA
   costs 127.7 ns against the fold's 42.6 ns on a path the gate runs for every
   request, and it is commented as claiming cost rather than correctness.
4. **Otherwise canonicalize.** `idna.Lookup.ToASCII`. **On error, discard the
   returned string entirely** and fall back to the ASCII fold of the original —
   on both the entry side and the lookup side, so the two can never desynchronize.

This is one rule shorter than the design started with. A guard routing `:`,
`[`, `%` and IP literals away from IDNA was written, and then mutation testing
showed deleting it changed no output: every shape it caught, `ToASCII` refuses,
and rule 4 returns the same string the guard would have. Dead code a test cannot
distinguish is worse than no code, so it went, and the comment beside rule 4
says where those shapes are handled instead.

`NormalizeHost` stays exactly as it is and keeps its name. `CanonicalHost` is a
new function, so every call site opts in deliberately rather than inheriting a
changed meaning.

### Where it is called

- **`NewHostSet` and `CoversEntry`: after `strings.CutPrefix(e, "*.")`, never
  before.** Both call `NormalizeHost` first today, and `ToASCII("*.example.com")`
  errors on U+002A. Canonicalizing inside `NormalizeHost` would break every
  wildcard entry in all eight lists that feed a `HostSet`. This is the single
  most likely implementation mistake in the whole change.
- **`Match`: canonicalize the whole host, then run `hasEmptyLabel` on the
  canonical value, not the raw one.** IDNA maps U+3002, U+FF0E and U+FF61 to
  `.`, so it *creates* label boundaries: `。example.com` canonicalizes to
  `.example.com`, which would then satisfy `HasSuffix(host, ".example.com")` and
  slip past the `*.example.com` boundary that `Match`'s doc comment exists to
  guard.
- **`vaultresolve.lowerHost` and `mcp.sameHost`** delegate to it, keeping their
  own extra normalization (the empty-port trim, the exact zone comparison) around
  it. Both split the zone identifier off before calling, so a scoped address's
  `%eth0` is compared exactly and only the address half is canonicalized.
- **`endpointKey`, in both spellings** (`internal/gate/policy.go` and
  `internal/api/gateconfig.go`), moves with them. It is a no-op beyond the fold
  today — `ValidateHostEntry` refuses a non-ASCII host on that path, so no
  mutation test can catch either one, and both are commented as such — but a
  `HostSet` comparing canonical names beside a map comparing folded ones would
  answer one request two ways depending on which list held the host, which is
  exactly what `endpointKey`'s own comment promises cannot happen.

### Validation

`ValidateHostEntry` is left alone; a new `egress.CanonicalEntry` wraps it as a
canonicalizing front end. A non-ASCII entry has its `*.` prefix cut, the rest
converted to its A-label, the prefix restored, and the result validated as an
ASCII entry; an ASCII entry goes straight to the grammar it passes today,
including the `ab--cd` shape `ToASCII` would refuse — and is stored exactly as
typed, case included, because the matcher folds. A conversion error is the
entry's rejection, never a fallback: an entry is written once and matched
forever, so `xn--pypi-.org` must not become a `pypi.org` nobody typed.

Both API lists call it. The environment's is `parseNetworking`, which create and
update share, so one call covers both write paths; the credential's is
`vaultcredauth.go`'s `canonicalAllowedHost` (was `validateAllowedHost`, renamed
because it now returns the form to store). Per decision 4 the environment check
runs on the entries the patch supplies rather than on the merged list — refusing
an update over what an earlier one stored would take a working environment's
egress away for a field nobody is touching.

### The gate dials what it admitted

`internal/gate` today calls `admit(hostOnly(target))` and then
`g.dial(ctx, "tcp", target)`. Once admission canonicalizes, those two strings can
differ by a whole name — `exa<U+00AD>mple.com`, `K.example` and `example<U+3002>com`
all canonicalize onto `example.com` with no error. The dial takes the canonical
name. The CONNECT tunnel is opaque and the client inside it sends its own SNI, so
nothing above the socket observes the change; and without it the widening is a
false promise, since `net.Dialer` on a raw U-label answers "no such host".

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
- **`NormalizeHost` folding a zone identifier**, so that `fe80::1%eth0` and
  `fe80::1%ETH0` are one host in `internal/egress` but two in the other. Rule 3
  routes zoned addresses to the fold, so this plan neither fixes nor worsens it;
  it stays recorded on #609.
- **Migrating stored rows** (decision 4), and any cap on the number of entries —
  the reference publishes one for credentials (16) and none for environments, and
  neither is this issue's subject.

## Definition of done

`make verify` green; the shared helper covered by mutation tests that each fail
for the row that names them, including one per rule above (the wildcard cut, the
post-canonicalization empty-label check, the discarded error on the mangling
input, the lowercasing fast path); the three divergences registered; a
`changelog.d/` fragment; and the verifier plus both reviewers run per the
repository's ritual.
