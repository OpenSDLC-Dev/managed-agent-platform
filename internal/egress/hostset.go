// Package egress is the egress-time credential-injection subsystem: the shared
// substitution engine that rewrites vault placeholders into their secret values
// on outbound requests, and the host matcher both it and the per-session gate
// use to decide which hosts a request — or a credential — may reach. Beside the
// matcher sits the one host list that is neither an operator's nor a
// credential's: the curated package-registry set allow_package_managers opens
// (registries.go), which lives here because the gate and the control plane both
// have to be told it. It holds no I/O, and that is the invariant to preserve:
// internal/gate drives it against real HTTP requests — constructing the
// substitution engine (gate.go) and calling Substitute for header and body
// locations — while internal/gate/policy.go reuses NewHostSet and
// CanonicalLookup for the environment's allowed-host policy, and
// internal/vaultresolve supplies the credentials read from the store. CanonicalHost is also what
// internal/vaultresolve and internal/mcp compare their own hosts with, so all
// three answer "are these one host?" with one function.
package egress

import (
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/idna"
)

// HostSet matches a request host against an allowed_hosts list in the grammar
// the vault API validates (internal/api/vaultcredauth.go): a bare hostname, an
// IPv4 literal, or a "*."-prefixed wildcard. It is the one matcher shared by a
// credential's allowed_hosts (may this secret be used for this host?) and an
// environment's networking allow-list (may this request leave at all?).
//
// A wildcard "*.example.com" matches any subdomain but never the apex
// (example.com) — the reference's recorded behavior (anthropic-sdk-go
// betavaultcredential.go: "a `*.`-prefixed entry matches any subdomain of the
// named domain but not the domain itself"). "Any subdomain" is read as any
// label depth (a.example.com, a.b.example.com), the one residual the SDK wording
// does not pin (recorded in DIVERGENCES).
type HostSet struct {
	exact    map[string]struct{} // hostnames and IPv4 literals, canonical (see CanonicalHost)
	suffixes []string            // wildcard suffixes, canonical, no leading "*."
}

// NewHostSet builds a matcher from allowed_hosts entries. Entries are assumed to
// have passed CanonicalEntry below. One that did not is not rejected here — it
// is keyed as it canonicalizes, and what that costs depends on the entry. Every
// one of them matches exactly the string it was written as and nothing else, so
// the question is whether a request host can ever be that string:
// "https://x.com" and "x.com:443" cannot be — net/http parses neither into a
// host — while "::1" and "_acme.example.com" can, and match. A nil or empty list
// matches nothing.
func NewHostSet(entries []string) *HostSet {
	s := &HostSet{exact: make(map[string]struct{}, len(entries))}
	for _, e := range entries {
		// The entry is trimmed once, here, and canonicalDeRooted is what runs
		// on the halves — not CanonicalLookup, which would trim again. That
		// second trim is not harmless: it lands *inside* the entry, so
		// "*. example.com" would key the live suffix "example.com" where it
		// used to key " example.com" and match nothing. Measured, and the
		// reason the trim cannot live below the cut.
		//
		// Running the whole lookup form before the cut as well is wrong for a
		// second reason, and only measurement separates the two. Converting
		// early would be harmless — an asterisk is a rune idna refuses, so the
		// error fallback returns the same folded string — but de-rooting does
		// not survive being repeated: it takes one trailing dot off per pass,
		// so a second pass turns the entry "example.com.." — whose last label
		// is empty — into the live key "example.com". Converting *after* the
		// cut is what makes a Unicode wildcard work at all. Mutation testing
		// sorted the three apart.
		e = strings.TrimSpace(e)
		if rest, ok := strings.CutPrefix(e, "*."); ok {
			// Judged after canonicalization for the reason Match's is: IDNA
			// creates label boundaries, so an entry arrives here holding no
			// empty label and can leave holding one. Such a key could never
			// match — Match refuses an empty label on the request side first —
			// so dropping it changes no answer. It keeps a dead key out of the
			// set rather than leaving the other side to make it harmless.
			if rest = canonicalDeRooted(rest); rest != "" && !hasEmptyLabel(rest) {
				s.suffixes = append(s.suffixes, rest)
			}
			continue
		}
		if e = canonicalDeRooted(e); e != "" && !hasEmptyLabel(e) {
			s.exact[e] = struct{}{}
		}
	}
	return s
}

// Match reports whether host is admitted by the set. Matching is
// case-insensitive and tolerant of a trailing FQDN dot. A malformed host — empty,
// or carrying an empty label (a leading dot or a ".." run) — never matches: the
// API validates entries against the same grammar, so admitting an out-of-grammar
// request host would let ".example.com" slip past the "*.example.com" boundary.
// A nil set (a credential resolved without a host list) matches nothing.
func (s *HostSet) Match(host string) bool {
	if s == nil {
		return false
	}
	// A host over the cost cap is refused rather than compared. CanonicalHost
	// hands back the raw bytes for one, and raw bytes can carry a label that is
	// empty only after UTS46 deletes what fills it: measured, 507 SOFT HYPHENs
	// before ".example.com" is 1026 bytes, carries no ASCII empty label, and
	// satisfies the "*.example.com" suffix test that the same host seven soft
	// hyphens long is refused by. Refusing is what the cap's own comment
	// promises for a lookup, and no host a request can resolve is written this
	// long — the A-label of a resolvable name holds 253 bytes.
	if len(host) > maxCanonicalHost {
		return false
	}
	// Judged on the canonical form, not the written one: IDNA maps U+3002,
	// U+FF0E and U+FF61 onto ".", so canonicalizing *creates* label boundaries.
	// A host led by an ideographic full stop carries no empty label as written
	// and carries one once converted, and the converted string is exactly what
	// this check exists to keep away from the "*.example.com" suffix test below.
	host = CanonicalLookup(host)
	if host == "" || hasEmptyLabel(host) {
		return false
	}
	if _, ok := s.exact[host]; ok {
		return true
	}
	for _, suf := range s.suffixes {
		// ".suf" requires a label boundary, which also excludes the apex (suf
		// itself lacks the leading dot) and a mere suffix collision (xsuf).
		if strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
}

// CoversEntry reports whether every host an allowed_hosts entry names is
// admitted by the set — the config-conflict probe behind the reference's
// credential_host_unreachable_error ("a credential's allowed_hosts includes a
// host the environment's network policy does not permit"). An exact entry is
// covered iff the set matches it. A wildcard entry names a whole subdomain
// family, which only a wildcard of the set can cover: "*.D" is covered iff the
// set has a suffix S with D == S or D under S — a set's exact host can never
// cover a family, and a broader wildcard is not covered by a narrower one.
// A malformed entry covers nothing and is reported uncovered (fail-closed:
// it names something the policy cannot admit).
func (s *HostSet) CoversEntry(entry string) bool {
	if s == nil {
		return false
	}
	entry = strings.TrimSpace(entry)
	if rest, ok := strings.CutPrefix(entry, "*."); ok {
		rest = canonicalDeRooted(rest)
		if rest == "" || hasEmptyLabel(rest) {
			return false
		}
		for _, suf := range s.suffixes {
			if rest == suf || strings.HasSuffix(rest, "."+suf) {
				return true
			}
		}
		return false
	}
	return s.Match(entry)
}

// CanonicalLookup is the form this package's matcher, and the gate's MCP
// endpoint map beside it, compare two hosts in: surrounding space trimmed, the
// host canonicalized, and then a single trailing FQDN dot stripped so
// "Example.com." and "example.com" are one host. It never rejects anything.
//
// The order is the whole point, and it is not the order this started with. IDNA
// maps U+3002, U+FF0E and U+FF61 onto ".", so a host written "example.com。" is
// rooted — but only once it has been converted. De-rooting first, as a fold-then
// -trim helper did, could not see that dot and the empty label it leaves behind
// was then refused: measured, all three spellings stopped matching "example.com"
// and an entry written that way was dropped from the set outright. Stripping
// again afterwards is not the fix either — "example.com.." would lose both dots
// and match. One strip, after.
//
// It is exported because the gate's MCP endpoint set has to answer a host
// exactly the way this package's HostSet does (internal/gate, policy.go) while
// staying an exact-match map. Two names for one comparison is how those two
// drift apart.
//
// It trims and de-roots where CanonicalHost does neither, and like that
// function it leaves a scoped address's zone identifier byte for byte, because
// the zone names a local interface and is case-sensitive. Folding it — which the
// fold-then-trim helper this replaced did — called two interfaces one, and #609
// recorded that as unaddressed; it is addressed now, on the admission side as
// well as at the dial. internal/vaultresolve's lowerHost and internal/mcp's
// sameHost do not use this function at all: they keep their own normalization
// around CanonicalHost, because a trailing dot there is a difference that
// withholds a token rather than sends it.
func CanonicalLookup(h string) string {
	return canonicalDeRooted(strings.TrimSpace(h))
}

// canonicalDeRooted is CanonicalLookup without the trim, for the callers that
// have already trimmed and must not trim again — NewHostSet and CoversEntry,
// which trim the whole entry before cutting a "*." off it, and where trimming
// the remainder instead would turn "*. example.com" into a live wildcard.
//
// It repeats CanonicalHost's split rather than wrapping it, because the dot it
// removes belongs to the name: TrimSuffix over the whole string would take a dot
// off a zone identifier, and two zones that differ only in a trailing dot are two
// interfaces.
func canonicalDeRooted(h string) string {
	if i := strings.IndexByte(h, '%'); i >= 0 {
		return strings.TrimSuffix(canonicalName(h[:i]), ".") + h[i:]
	}
	return strings.TrimSuffix(canonicalName(h), ".")
}

// foldASCII lowercases the ASCII letters of h and copies every other byte. No
// non-ASCII code point's UTF-8 encoding contains a byte in 0x41-0x5A — lead
// bytes are 0xC2-0xF4 and continuation bytes 0x80-0xBF — so the fold cannot
// reach inside a multi-byte character, and it preserves length.
func foldASCII(h string) string {
	if !hasUpperASCII(h) {
		return h
	}
	var b strings.Builder
	b.Grow(len(h))
	for i := range len(h) {
		c := h[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// maxCanonicalHost bounds what CanonicalHost and CanonicalEntry will hand to
// IDNA. A sandbox writes the CONNECT authority itself and net/http bounds that
// only by MaxHeaderBytes, so a megabyte-long U-label is reachable:
// canonicalizing one costs tens of milliseconds where the fold costs
// milliseconds.
//
// It is a cost bound, not a proof, and the difference is worth stating because
// the arithmetic looks like one. A resolvable name's A-label holds 253 bytes and
// UTF-8 spends at most 4 on a code point, so 1012 bytes covers every input whose
// code points survive UTS46 one for one — 1024 rounds that up. What it does not
// cover is deletion: UTS46 *removes* the ignorable code points outright, so an
// input of any length at all can map to a short name. Measured, 10,000 SOFT
// HYPHENs before "example.com" canonicalize to "example.com", and this cap
// refuses to find that out.
//
// So each caller above the cap has to say what it does with a host it cannot
// convert, and each says the fail-closed thing. CanonicalHost returns the fold,
// which is what the dial wants: the authority goes out as the sandbox wrote it.
// CanonicalEntry refuses. HostSet.Match refuses too, and separately, because
// comparing the raw bytes is not fail-closed — a label that UTS46 would empty
// out is not empty as written, and the raw form walked a wildcard boundary the
// converted one is refused at.
//
// What the number must not be is small. A 255-byte cap was measurably wrong:
// three 45-rune "ä" labels are 284 bytes of UTF-8 and a 167-byte A-label, an
// ordinary resolvable name that such a cap declined to canonicalize while the
// entry side stored its A-label, so the entry could not match its own spelling.
const maxCanonicalHost = 1024

// CanonicalHost returns the form of a host that two spellings of one name agree
// on. It is the comparison this package, internal/vaultresolve and internal/mcp
// share, and the reason it is not a case fold is that case folding is not what
// makes two hostnames one name — IDNA is.
//
// Folding by Unicode is unsafe: strings.EqualFold merges the two Greek sigmas,
// which Go's non-transitional lookup punycodes to "xn--4xa" and "xn--3xa", two
// names that can belong to two people. Folding by ASCII is safe but withholds
// far more than #611 claimed: it separates every orbit EqualFold merged that
// IDNA also collapses, so "Ä.example" and "ä.example" are called two hosts when
// they are one ("xn--4ca.example" either way), and so are "Д"/"д", U+212A and
// "k", U+017F and "s". Comparing A-labels gets both directions right. The sweep
// behind that: all 1,112,064 code points through idna.Lookup.ToASCII produced
// 140,873 distinct A-labels, none of which moved or collided when fed back. Read
// it for what it is — one code point per label, fed back once. It shows no two
// code points merge and that the conversion is stable on its own output, which is
// what the orbits above needed answering; it is not a proof over arbitrary
// multi-code-point names, and idna.Lookup checks neither DNS lengths nor whether
// anyone could register the result.
//
// What it must not do is rewrite strings that are not names. The Lookup profile
// validates, and it refuses shapes this platform accepts today: ":" and "["
// (an authority carrying its port, a bracketed or bare IPv6 literal), "_" (an
// internal-DNS spelling), and — the surprise — a plain-ASCII label with hyphens
// in positions 3 and 4, so "s3--us-west-2.example.com" errors while
// ValidateHostEntry accepts it. Every one of those comes back folded, and none
// needs an arm of its own: each is ASCII, so the branch below returns before
// idna runs, and each would reach the same string by the error path if that
// branch were deleted. A guard for them would have been dead code, which
// mutation testing is how we found out. An IPv4 literal is the same story from
// the other side — idna returns it unchanged, measured. That equivalence is not
// general, though; the branch's own paragraph names the family where it fails.
//
// The error path is the sharp edge. ToASCII returns a value *and* an error, and
// the value can be a different real hostname: "xn--pypi-.org" comes back as
// ("pypi.org", err), so an implementation that canonicalizes and drops the error
// makes an entry admit a domain nobody typed. The returned string is therefore
// discarded whenever the error is non-nil, on the entry side and the lookup side
// alike, so the two can never disagree about which alphabet they are in.
//
// The ASCII branch was written as a cost guard — idna costs 128ns against the
// fold's 43ns, on a path the gate runs for every request — and it turns out to
// carry a correctness rule as well, which is worth stating because the cost
// reading alone invites deleting it. Nearly every all-ASCII host either passes
// idna unchanged or is refused by it. One family does neither: a label of "xn--"
// whose punycode payload is empty is REWRITTEN, and no error is returned.
// Measured, ToASCII answers "xn--.example" with (".example", nil) and "a.xn--.z"
// with ("a..z", nil). What the branch protects is the destination: the gate dials
// this function's answer, so without it a CONNECT to "xn--.example" would leave
// for ".example" — a name the sandbox never wrote. Be exact about the direction,
// because the guard reads like a matcher rule and is not one. Relative to
// converting, keeping the raw form is *broader*: the converted string carries an
// empty label and Match would refuse it, while the raw one reaches the suffix
// test, so "xn--.example.com" does match "*.example.com" here. That is the
// compatibility net/http keeps too — its idnaASCII has the same branch, though
// its one does not case-fold, so copying the function rather than the shape would
// undo the fold #611 landed. Plan 43 argues the whole design.
//
// A scoped address keeps its zone identifier byte for byte. The zone names a
// local interface, it is case-sensitive, and folding it would call two
// interfaces one — so this splits at the first "%" and returns the remainder
// untouched. CanonicalLookup delegates here and inherits that, which is why the
// zone stays out of the fold on the admission side too, and it is what lets the
// gate dial the canonical name of a scoped address without changing which
// interface it means.
//
// It folds but does not trim or de-root, so each caller keeps the normalization
// its own callers need: this package wraps it as CanonicalLookup, while
// internal/mcp's sameHost deliberately does not, because a trailing dot there is
// a difference that drops the token rather than sends it.
func CanonicalHost(h string) string {
	if i := strings.IndexByte(h, '%'); i >= 0 {
		return canonicalName(h[:i]) + h[i:]
	}
	return canonicalName(h)
}

// canonicalName is CanonicalHost on a string already known to carry no zone
// identifier.
func canonicalName(h string) string {
	h = foldASCII(h)
	if h == "" || len(h) > maxCanonicalHost {
		return h
	}
	if isASCII(h) {
		return h
	}
	a, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return h
	}
	return a
}

// CanonicalEntry validates one allowed_hosts entry and returns the form to
// store. An entry written as a Unicode name is stored as its A-label, so a later
// comparison needs no conversion; an ASCII entry is returned exactly as written,
// case included, because rewriting those would change what every existing
// environment reads back for no gain. The store therefore holds one *alphabet*
// per host, not one spelling — "API.example" and "api.example" are both kept as
// typed, and it is the matcher that folds them into one host.
//
// The wildcard prefix is cut before conversion and restored after, because an
// asterisk is a rune IDNA refuses — which is also why CanonicalHost cannot be
// handed a wildcard.
//
// The length cap CanonicalHost keeps applies to the conversion, and that is
// exactly where it is needed: without it an operator could store an entry
// converted from a U-label the matcher would never convert, so the entry could
// not match the spelling it was written as. An ASCII entry over the cap needs no
// such arm — both sides fold it and neither converts it, so they still meet —
// and is stored as typed.
//
// It is not a DNS length check, and nothing here is. An entry can still name a
// label over 63 bytes or a name over 253: measured, 58 "ä" runes convert to a
// 64-byte A-label, and ValidateHostEntry has never bounded an ASCII label
// either. Such an entry is stored and matches nothing, which is the pre-existing
// behaviour rather than something this front end introduced.
//
// A failure names the entry as the operator wrote it, never the A-label made of
// it: a 400 quoting punycode nobody typed cannot be found in the config it came
// from.
func CanonicalEntry(h string) (string, error) {
	e := h
	if !isASCII(e) {
		// The cap is measured on the string that gets converted, which is the
		// remainder after the "*." cut — the same string NewHostSet converts.
		// Measuring it on the whole entry instead put the two sides a prefix
		// apart: a 1025-byte wildcard entry was refused here while NewHostSet
		// converted its 1023-byte remainder and installed a live wildcard.
		rest, wildcard := strings.CutPrefix(e, "*.")
		if len(rest) > maxCanonicalHost {
			return "", fmt.Errorf("allowed_hosts entry %q is too long to canonicalize", h)
		}
		a, err := idna.Lookup.ToASCII(rest)
		if err != nil {
			return "", fmt.Errorf("allowed_hosts entry %q is not a hostname: %w", h, err)
		}
		if wildcard {
			a = "*." + a
		}
		e = a
	}
	if err := validateHostEntry(e, h); err != nil {
		return "", err
	}
	return e, nil
}

// hasUpperASCII reports whether folding s would change it, so a host already
// written in lower case — the common case on a path the gate runs for every
// request — keeps its own string instead of an identical copy.
func hasUpperASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// isASCII reports whether s is entirely ASCII. It selects CanonicalHost's ASCII
// branch, which is not merely a shortcut — see that function's comment for the
// "xn--" family idna rewrites without returning an error.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// idnaFullStops are the three code points UTS46 maps onto "." beside "." itself.
// hasEmptyLabel counts them as separators because it also runs on hosts
// CanonicalHost left alone — one over the length cap, or one whose conversion
// failed — where that mapping has not happened. Measured on a host over the cap
// and led by U+3002: it carried no ASCII empty label and so walked straight
// through the "*.example.com" boundary this check exists to hold. The fixture in
// the test is sized against the cap for that reason, and has to be resized with
// it.
var idnaFullStops = strings.NewReplacer("\u3002", ".", "\uff0e", ".", "\uff61", ".")

// hasEmptyLabel reports whether host contains an empty DNS label — a leading dot,
// a trailing dot (beyond the one CanonicalLookup strips), or a ".." run. Such a
// string is not a valid hostname and must not match, least of all a wildcard.
func hasEmptyLabel(host string) bool {
	// The three are all non-ASCII, so an ASCII host cannot hold one and the
	// replacer has nothing to do — and it is not free: strings.Replacer builds a
	// fresh buffer and string on every call it is given, never returning its
	// input. Measured on an exact match, which is the gate's per-request path:
	// 92.71 ns/op and 4 allocations without this guard, 54.42 and 1 with it.
	// After a successful conversion the host is always ASCII, so what remains is
	// exactly the case that needs it — a host CanonicalHost declined to convert.
	if !isASCII(host) {
		host = idnaFullStops.Replace(host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return true
		}
	}
	return false
}

// ValidateHostEntry checks one allowed-hosts entry against the grammar this
// package matches: a bare hostname, an IPv4 literal, or a "*."-prefixed
// wildcard on a hostname. It is the single source for the grammar — every
// allowed_hosts writer reaches it through CanonicalEntry, and the executor's
// WEBTOOL_ALLOWED_DOMAINS fails startup on the first bad entry, because an
// out-of-grammar entry matches exactly the string it was written as and nothing
// else, and no request host can be that string — net/http parses neither a URL
// nor a bracketed authority carrying a colon into a host — so a typo reads as the
// operator's fence when it is really a hole in it.
func ValidateHostEntry(h string) error { return validateHostEntry(h, h) }

// validateHostEntry is ValidateHostEntry with the spelling to report split from
// the spelling to check, so CanonicalEntry can hold an entry to the grammar in
// its A-label form and still quote the operator's own.
func validateHostEntry(h, display string) error {
	badf := func() error {
		return fmt.Errorf("allowed_hosts entry %q is not a hostname, IPv4 address, or *.-wildcard", display)
	}
	wildcard := strings.HasPrefix(h, "*.")
	host := strings.TrimPrefix(h, "*.")
	if host == "" || strings.Contains(host, "*") {
		return fmt.Errorf("allowed_hosts entry %q: a wildcard must be a \"*.\" prefix on a hostname", display)
	}
	// A ":" is never part of the grammar — it is a port or an IPv6 literal,
	// including IPv4-mapped forms like "::ffff:10.0.0.1" that net.ParseIP
	// would otherwise accept as IPv4.
	if strings.Contains(host, ":") {
		return badf()
	}
	// A dotted-numeric entry must be a valid IPv4 literal (so 999.999.999.999
	// is rejected), and a wildcard applies to hostnames only — never an IP.
	if ip := net.ParseIP(host); ip != nil {
		if wildcard {
			return fmt.Errorf("allowed_hosts entry %q: a \"*.\" wildcard applies to hostnames, not IP addresses", display)
		}
		return nil
	}
	allNumeric := true
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return badf()
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
				allNumeric = false
			case r >= '0' && r <= '9':
			default:
				return badf()
			}
		}
	}
	// An all-numeric dotted string that net.ParseIP rejected is a malformed IP
	// (e.g. 999.999.999.999), not a hostname.
	if allNumeric {
		return badf()
	}
	return nil
}
