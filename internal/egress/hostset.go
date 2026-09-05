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
// NormalizeHost for the environment's allowed-host policy, and
// internal/vaultresolve supplies the credentials read from the store.
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
	exact    map[string]struct{} // hostnames and IPv4 literals, lowercased
	suffixes []string            // wildcard suffixes, lowercased, no leading "*."
}

// NewHostSet builds a matcher from allowed_hosts entries. Entries are assumed to
// have passed CanonicalEntry below; malformed entries simply never match. A nil
// or empty list matches nothing.
func NewHostSet(entries []string) *HostSet {
	s := &HostSet{exact: make(map[string]struct{}, len(entries))}
	for _, e := range entries {
		// NormalizeHost first, for the trim that makes the prefix visible;
		// CanonicalHost only after the "*." is off, because an asterisk is a
		// disallowed rune to IDNA and would fail every wildcard entry.
		e = NormalizeHost(e)
		if rest, ok := strings.CutPrefix(e, "*."); ok {
			if rest = CanonicalHost(rest); rest != "" {
				s.suffixes = append(s.suffixes, rest)
			}
			continue
		}
		if e = CanonicalHost(e); e != "" {
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
	// In this order, and not the other one: IDNA maps U+3002, U+FF0E and U+FF61
	// onto ".", so canonicalizing *creates* label boundaries. A host led by an
	// ideographic full stop carries no empty label as written and carries one
	// once converted, and the converted string is exactly what this check exists
	// to keep away from the "*.example.com" suffix test below.
	host = CanonicalHost(NormalizeHost(host))
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
	entry = NormalizeHost(entry)
	if rest, ok := strings.CutPrefix(entry, "*."); ok {
		rest = CanonicalHost(rest)
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

// NormalizeHost folds a host's case and strips a single trailing FQDN dot so
// "Example.com." and "example.com" compare equal. It folds **ASCII only**, the
// way DNS does, and it never rejects anything.
//
// It is not the comparison: CanonicalHost is, and callers deciding whether two
// spellings name one host want that one. This is the trim CanonicalHost opens
// with, the answer it gives for everything that is not a name, and the answer it
// falls back to when canonicalization fails. It stays exported because the
// gate's MCP endpoint set has to answer a host exactly the way this package's
// HostSet does (internal/gate, policy.go) while staying an exact-match map.
//
// It folds its whole input, a port, a "*." prefix and an IPv6 zone identifier
// included, where internal/vaultresolve's lowerHost and internal/mcp's sameHost
// leave a zone exactly as written because it selects a local interface. Read
// that as unaddressed rather than impossible: an entry in an environment's
// networking.allowed_hosts could carry one, and so could a CONNECT authority
// naming a scoped address, and folding it would call two interfaces one (#609).
func NormalizeHost(h string) string {
	return foldASCII(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// foldASCII lowercases the ASCII letters of h and copies every other byte. No
// non-ASCII code point's UTF-8 encoding contains a byte in 0x41-0x5A — lead
// bytes are 0xC2-0xF4 and continuation bytes 0x80-0xBF — so the fold cannot
// reach inside a multi-byte character, and it preserves length.
func foldASCII(h string) string {
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

// maxCanonicalHost bounds what CanonicalHost will hand to IDNA. A sandbox writes
// the CONNECT authority itself and net/http bounds that only by MaxHeaderBytes,
// so a megabyte-long U-label is reachable: canonicalizing one costs tens of
// milliseconds where the fold costs milliseconds. Nothing is lost by refusing to
// spend that — a name this long cannot resolve.
const maxCanonicalHost = 255

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
// "k", U+017F and "s". Comparing A-labels gets both directions right, and it is
// injective on registrable names: sweeping all 1,112,064 code points through
// idna.Lookup.ToASCII produced 140,873 distinct A-labels, none of which moved or
// collided when fed back.
//
// What it must not do is rewrite strings that are not names. The Lookup profile
// validates, and it refuses shapes this platform accepts today: ":" and "["
// (an authority carrying its port, a bracketed or bare IPv6 literal), "_" (an
// internal-DNS spelling), and — the surprise — a plain-ASCII label with hyphens
// in positions 3 and 4, so "s3--us-west-2.example.com" errors while
// ValidateHostEntry accepts it. Every one of those takes the fold, by the error
// path below rather than by a guard of its own: a guard would have been dead
// code, which mutation testing is how we found out. An IPv4 literal needs no arm
// either way — idna returns it unchanged, measured.
//
// The error path is the sharp edge. ToASCII returns a value *and* an error, and
// the value can be a different real hostname: "xn--pypi-.org" comes back as
// ("pypi.org", err), so an implementation that canonicalizes and drops the error
// makes an entry admit a domain nobody typed. The returned string is therefore
// discarded whenever the error is non-nil, on the entry side and the lookup side
// alike, so the two can never disagree about which alphabet they are in.
//
// The ASCII branch is a cost guard and nothing more, which is worth saying
// plainly: an all-ASCII host either passes idna unchanged or is refused by it,
// so removing that branch changes no output and no test can tell — it is there
// because idna costs 128ns against the fold's 43ns on a path the gate runs for
// every request. net/http has the same branch for the same reason, but its one
// does not case-fold, so copying the function rather than the shape would undo
// the fold #611 landed. Plan 43 argues the whole design.
//
// It folds but does not trim or de-root, so each caller keeps the normalization
// its own callers need: this package pairs it with NormalizeHost, while
// internal/mcp's sameHost deliberately does not, because a trailing dot there is
// a difference that drops the token rather than sends it.
func CanonicalHost(h string) string {
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
// store. An entry written as a Unicode name is stored as its A-label, so the
// stored list holds one spelling per host and a later comparison needs no
// conversion; an ASCII entry is returned exactly as written, because rewriting
// those would change what every existing environment reads back for no gain.
//
// The wildcard prefix is cut before conversion and restored after, because an
// asterisk is a rune IDNA refuses — which is also why CanonicalHost cannot be
// handed a wildcard.
func CanonicalEntry(h string) (string, error) {
	e := h
	if !isASCII(e) {
		rest, wildcard := strings.CutPrefix(e, "*.")
		a, err := idna.Lookup.ToASCII(rest)
		if err != nil {
			return "", fmt.Errorf("allowed_hosts entry %q is not a hostname: %w", h, err)
		}
		if wildcard {
			a = "*." + a
		}
		e = a
	}
	if err := ValidateHostEntry(e); err != nil {
		return "", err
	}
	return e, nil
}

// isASCII reports whether s is entirely ASCII. It selects CanonicalHost's cost
// guard, never a different answer: for a valid ASCII name ToASCII returns the
// fold's own string, and for the shapes it refuses the fold is what the error
// path would have returned anyway.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// hasEmptyLabel reports whether host contains an empty DNS label — a leading dot,
// a trailing dot (beyond the one NormalizeHost strips), or a ".." run. Such a
// string is not a valid hostname and must not match, least of all a wildcard.
func hasEmptyLabel(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return true
		}
	}
	return false
}

// ValidateHostEntry checks one allowed-hosts entry against the grammar this
// package matches: a bare hostname, an IPv4 literal, or a "*."-prefixed
// wildcard on a hostname. It is the single source for the grammar — the vault
// API's allowed_hosts validation wraps it, and the executor's
// WEBTOOL_ALLOWED_DOMAINS fails startup on the first bad entry, because an
// out-of-grammar entry silently matches nothing (a typo would read as the
// operator's fence when it is really a hole in it, or a deny-all).
func ValidateHostEntry(h string) error {
	badf := func() error {
		return fmt.Errorf("allowed_hosts entry %q is not a hostname, IPv4 address, or *.-wildcard", h)
	}
	wildcard := strings.HasPrefix(h, "*.")
	host := strings.TrimPrefix(h, "*.")
	if host == "" || strings.Contains(host, "*") {
		return fmt.Errorf("allowed_hosts entry %q: a wildcard must be a \"*.\" prefix on a hostname", h)
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
			return fmt.Errorf("allowed_hosts entry %q: a \"*.\" wildcard applies to hostnames, not IP addresses", h)
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
