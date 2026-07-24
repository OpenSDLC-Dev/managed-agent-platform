// Package egress is the egress-time credential-injection subsystem: the shared
// substitution engine that rewrites vault placeholders into their secret values
// on outbound requests, and the host matcher both it and the per-session gate
// use to decide which hosts a request — or a credential — may reach. It holds
// no I/O: the gate (a later slice) drives it against real HTTP requests, and
// resolution supplies it credentials read from the store.
package egress

import "strings"

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
		e = normalizeHost(e)
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
	host = normalizeHost(host)
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

// normalizeHost lowercases and strips a single trailing FQDN dot so
// "Example.com." and "example.com" compare equal.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ".")
}

// hasEmptyLabel reports whether host contains an empty DNS label — a leading dot,
// a trailing dot (beyond the one normalizeHost strips), or a ".." run. Such a
// string is not a valid hostname and must not match, least of all a wildcard.
func hasEmptyLabel(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return true
		}
	}
	return false
}
