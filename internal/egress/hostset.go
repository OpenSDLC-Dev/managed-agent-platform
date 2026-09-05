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
// have passed the API's validateAllowedHost; malformed entries simply never
// match. A nil or empty list matches nothing.
func NewHostSet(entries []string) *HostSet {
	s := &HostSet{exact: make(map[string]struct{}, len(entries))}
	for _, e := range entries {
		e = NormalizeHost(e)
		if rest, ok := strings.CutPrefix(e, "*."); ok {
			if rest != "" {
				s.suffixes = append(s.suffixes, rest)
			}
			continue
		}
		if e != "" {
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
	host = NormalizeHost(host)
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
// "Example.com." and "example.com" compare equal. Exported because the gate's
// MCP endpoint set has to answer a host exactly the way this package's HostSet
// does (internal/gate, policy.go) while staying an exact-match map.
//
// It folds **ASCII only**, the way DNS does, and that is the security-relevant
// half. strings.ToLower folds by Unicode, and Unicode maps several non-ASCII
// letters onto ASCII ones — it lowercases U+0130 (LATIN CAPITAL LETTER I WITH
// DOT ABOVE) to plain "i", so "gİthub.com" and "github.com" would share one key.
// They are not one host: Go's HTTP stack resolves the first through IDNA to
// "xn--github-qyd.com", a name anyone may register. Folding by Unicode therefore
// let a request to the IDN pass an admission check written for the ASCII name,
// and — because Engine.Substitute selects a credential through this same
// matcher — collect that name's secret on the way out.
//
// Nearly every entry on the matching side is ASCII by construction:
// ValidateHostEntry accepts only letters, digits and "-" per label, and it
// guards a credential's allowed_hosts, the MCP endpoints the control plane
// forwards, WEBTOOL_ALLOWED_DOMAINS and this platform's own
// PackageRegistryHosts. An environment's networking.allowed_hosts is the
// exception — parseNetworking stores that list as arbitrary strings — so an
// operator can write a U-label there, and a differently-cased spelling of it
// stops matching. That is a real narrowing, not none: it errs toward a refusal,
// which is the direction this gate must err in, but the honest claim is
// "narrows only into refusal", never "narrows nothing" (#609).
//
// internal/vaultresolve's lowerHost and internal/mcp's sameHost fold this way,
// for this reason; sameHost joined them in #609's first half. Only the folding
// is shared — each also normalizes what its own callers need, and this one is
// alone in trimming space and a trailing dot. The folds differ past a "%":
// those two leave a scoped address's zone identifier exactly as written,
// because it selects a local interface and is case-sensitive, while this one
// folds its whole input. Nothing here selects an interface — a host reaching
// this matcher is a name to be resolved.
func NormalizeHost(h string) string {
	h = strings.TrimSuffix(strings.TrimSpace(h), ".")
	var b strings.Builder
	for i := range len(h) {
		c := h[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
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
