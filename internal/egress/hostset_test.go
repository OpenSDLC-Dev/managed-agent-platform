package egress_test

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

func TestHostSetCoversEntry(t *testing.T) {
	env := egress.NewHostSet([]string{"api.example.com", "*.wild.example.com", "192.0.2.1"})
	cases := []struct {
		entry string
		want  bool
	}{
		{"api.example.com", true},      // exact entry, exact match
		{"API.Example.Com", true},      // case-insensitive
		{"192.0.2.1", true},            // IPv4 literal
		{"blocked.example.com", false}, // exact entry outside the set
		{"a.wild.example.com", true},   // exact entry under the set's wildcard
		{"*.wild.example.com", true},   // wildcard entry == the set's wildcard
		{"*.a.wild.example.com", true}, // narrower wildcard, family within the set's
		{"*.example.com", false},       // broader wildcard: names hosts the set refuses
		{"*.api.example.com", false},   // exact set entry cannot cover a wildcard's family
		{"wild.example.com", false},    // the apex is never matched by the set's wildcard
		{"*.", false},                  // malformed wildcard entry
		{"", false},                    // malformed entry
	}
	for _, c := range cases {
		if got := env.CoversEntry(c.entry); got != c.want {
			t.Errorf("CoversEntry(%q) = %v, want %v", c.entry, got, c.want)
		}
	}
	var nilSet *egress.HostSet
	if nilSet.CoversEntry("api.example.com") {
		t.Error("nil set covers an entry")
	}
}

func TestHostSetMatch(t *testing.T) {
	set := egress.NewHostSet([]string{
		"example.com",   // exact hostname
		"10.1.2.3",      // exact IPv4
		"*.api.test",    // wildcard: any subdomain of api.test, not the apex
		"MixedCase.org", // stored uppercase; matching is case-insensitive
	})

	cases := []struct {
		host string
		want bool
	}{
		// Exact hostname.
		{"example.com", true},
		{"EXAMPLE.COM", true},    // request host case-insensitive
		{"example.com.", true},   // trailing FQDN dot tolerated
		{"a.example.com", false}, // exact entry is not a wildcard
		{"example.org", false},

		// Exact IPv4.
		{"10.1.2.3", true},
		{"10.1.2.4", false},

		// Wildcard *.api.test — subdomains at any depth, never the apex.
		{"v1.api.test", true},
		{"a.b.api.test", true},
		{"api.test", false},  // apex is excluded
		{"xapi.test", false}, // must be a label boundary, not a suffix
		{"api.test.evil.com", false},

		// Case-insensitive on both stored entry and query.
		{"mixedcase.org", true},
		{"MIXEDCASE.ORG", true},

		// A host with an empty label never matches — not a wildcard (the
		// boundary a naive HasSuffix would leak) and not an exact entry.
		{".api.test", false},
		{"a..api.test", false},
		{"..api.test", false},
		{".example.com", false},

		// Unknown host.
		{"other.net", false},
		{"", false},
	}
	for _, c := range cases {
		if got := set.Match(c.host); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestHostSetEmpty(t *testing.T) {
	// A nil/empty set matches nothing (a credential with no allowed_hosts, or an
	// environment allow-list that admits nothing, never admits a request).
	if egress.NewHostSet(nil).Match("example.com") {
		t.Error("empty host set must match nothing")
	}
	// A nil *HostSet (a credential resolved without a host list) must not panic.
	var nilSet *egress.HostSet
	if nilSet.Match("example.com") {
		t.Error("nil host set must match nothing")
	}
}

// ValidateHostEntry is the grammar's single source: the vault API wraps it and
// the executor's WEBTOOL_ALLOWED_DOMAINS fails startup on it, because an
// out-of-grammar entry stored in a HostSet silently matches nothing.
func TestValidateHostEntry(t *testing.T) {
	for _, ok := range []string{"example.com", "sub.example.com", "*.example.com", "10.0.0.1", "localhost"} {
		if err := egress.ValidateHostEntry(ok); err != nil {
			t.Errorf("ValidateHostEntry(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"https://example.com", "example.com:443", "example.com/docs",
		"*example.com", ".example.com", "*.", "*", "999.999.999.999",
		"::1", "::ffff:10.0.0.1", "*.10.0.0.1", "*.1", "ex ample.com",
	} {
		if err := egress.ValidateHostEntry(bad); err == nil {
			t.Errorf("ValidateHostEntry(%q) = nil, want an error", bad)
		}
	}
}

// Case folding a hostname is an ASCII operation; anything else is a different
// name. Unicode maps U+0130 onto plain "i", so a Unicode fold would collapse
// these onto their ASCII twins — and Go's HTTP stack would then dial the IDNA
// form, a name anyone may register. Every entry on the matching side is ASCII by
// ValidateHostEntry's grammar, so the alias can only ever be the request's, and
// refusing to match it is the whole job (#606).
func TestAnIDNAliasIsNotItsASCIITwin(t *testing.T) {
	set := egress.NewHostSet([]string{"github.com", "pypi.org", "api.example.com"})
	for _, alias := range []string{
		"gİthub.com", // U+0130 -> "i" under a Unicode fold
		"pypİ.org",
		"apİ.example.com",
	} {
		if egress.NormalizeHost(alias) == egress.NormalizeHost("github.com") ||
			set.Match(alias) {
			t.Errorf("%q matched an ASCII entry: NormalizeHost gave %q", alias, egress.NormalizeHost(alias))
		}
	}
}

// ...while the ASCII folding the grammar does need keeps working, trailing dot
// and all.
func TestASCIIFoldingStillNormalizes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"API.Example.Com", "api.example.com"},
		{"example.com.", "example.com"},
		{"  Example.COM.  ", "example.com"},
		{"GITHUB.COM", "github.com"},
	} {
		if got := egress.NormalizeHost(tc.in); got != tc.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Two spellings of one name are one host. Each pair here is a single domain
// after IDNA and was two under the ASCII fold this replaces, so each is a
// credential the platform used to withhold from the server it was resolved for.
func TestCanonicalHostJoinsSpellingsOfOneName(t *testing.T) {
	for _, tc := range []struct{ a, b, why string }{
		{"B\u00dcCHER.example", "b\u00fccher.example", "a capital in a non-ASCII label is still that label"},
		{"b\u00fccher.example", "xn--bcher-kva.example", "a U-label and its own A-label"},
		{"\u00c4.example", "\u00e4.example", "both punycode to xn--4ca.example"},
		{"\u0414.example", "\u0434.example", "both punycode to xn--d1a.example"},
		{"\u212a.example", "k.example", "UTS46 maps U+212A (KELVIN SIGN) onto k"},
		{"\u017ftrasse.example", "strasse.example", "UTS46 maps U+017F (LONG S) onto s"},
	} {
		if egress.CanonicalHost(tc.a) != egress.CanonicalHost(tc.b) {
			t.Errorf("CanonicalHost(%q) = %q but CanonicalHost(%q) = %q — %s",
				tc.a, egress.CanonicalHost(tc.a), tc.b, egress.CanonicalHost(tc.b), tc.why)
		}
		if !egress.NewHostSet([]string{tc.b}).Match(tc.a) {
			t.Errorf("a set holding %q did not match %q — %s", tc.b, tc.a, tc.why)
		}
	}
}

// ...and the pairs IDNA keeps apart stay apart, which is the direction that
// leaks rather than withholds.
func TestCanonicalHostKeepsTwoNamesApart(t *testing.T) {
	for _, tc := range []struct{ a, b, why string }{
		{"\u03c3.example", "\u03c2.example", "the sigmas punycode to xn--4xa and xn--3xa"},
		{"\u0130.example", "i.example", "U+0130 punycodes to xn--i-9bb, which strings.ToLower would have merged"},
		{"\u0430.example", "a.example", "a Cyrillic a is xn--80a, not the Latin one"},
		{"\u0435xample.com", "example.com", "a Cyrillic e makes xn--xample-2of.com"},
	} {
		if egress.CanonicalHost(tc.a) == egress.CanonicalHost(tc.b) {
			t.Errorf("CanonicalHost merged %q and %q onto %q — %s",
				tc.a, tc.b, egress.CanonicalHost(tc.a), tc.why)
		}
		if egress.NewHostSet([]string{tc.b}).Match(tc.a) {
			t.Errorf("a set holding %q matched %q — %s", tc.b, tc.a, tc.why)
		}
	}
}

// IDNA's Lookup profile validates, and it refuses shapes this platform admits.
// They take the fold instead, so what worked before still works.
func TestCanonicalHostLeavesWhatIsNotANameToTheFold(t *testing.T) {
	for _, tc := range []struct{ in, want, why string }{
		{"::1", "::1", "a bare IPv6 literal: idna refuses U+003A"},
		{"[::1]", "[::1]", "a bracketed one: idna refuses U+005B"},
		{"FE80::1%Eth0", "fe80::1%eth0", "a scoped address, folded whole as this package always has"},
		{"Example.com:8080", "example.com:8080", "an authority still carrying its port"},
		{"_acme.Example.com", "_acme.example.com", "idna refuses U+005F, internal DNS does not"},
		{"S3--us-west-2.example.com", "s3--us-west-2.example.com", "idna's CheckHyphens refuses \"--\" in positions 3-4"},
		{"192.168.1.1", "192.168.1.1", "an IPv4 literal needs no special arm: idna returns it unchanged"},
	} {
		if got := egress.CanonicalHost(tc.in); got != tc.want {
			t.Errorf("CanonicalHost(%q) = %q, want %q — %s", tc.in, got, tc.want, tc.why)
		}
		if !egress.NewHostSet([]string{tc.in}).Match(tc.in) {
			t.Errorf("a set holding %q no longer matches it — %s", tc.in, tc.why)
		}
	}
}

// idna.Lookup.ToASCII returns a value *and* an error, and the value can be a
// different real name: it answers "a\u200db.example" with "xn--ab-m1t.example".
// Acting on that would put a host in the set that nobody wrote.
func TestACanonicalizationErrorNeverBecomesAnotherHost(t *testing.T) {
	const refused = "a\u200db.example" // a zero-width joiner: idna's CheckJoiners refuses it
	if got := egress.CanonicalHost(refused); got != refused {
		t.Errorf("CanonicalHost(%q) = %q — the refused conversion's value was used", refused, got)
	}
	set := egress.NewHostSet([]string{refused})
	if !set.Match(refused) {
		t.Errorf("a set holding %q stopped matching it", refused)
	}
	if set.Match("ab.example") || set.Match("xn--ab-m1t.example") {
		t.Errorf("a set holding %q admitted the name idna would have made of it", refused)
	}
	// The ASCII fast path keeps the same shape from ever reaching idna: an
	// A-label with a trailing hyphen inside converts to a real domain
	// ("xn--pypi-.org" -> "pypi.org"), and this entry must not admit it.
	pypi := egress.NewHostSet([]string{"xn--pypi-.org"})
	if pypi.Match("pypi.org") {
		t.Error("an entry of \"xn--pypi-.org\" admitted pypi.org")
	}
	if !pypi.Match("xn--pypi-.org") {
		t.Error("an entry of \"xn--pypi-.org\" stopped matching itself")
	}
}

// The wildcard prefix is cut before the remainder is canonicalized, because an
// asterisk is a disallowed rune to IDNA and would fail every wildcard entry.
func TestAWildcardIsCanonicalizedAfterItsPrefix(t *testing.T) {
	idn := egress.NewHostSet([]string{"*.b\u00fccher.example"})
	for _, h := range []string{"a.xn--bcher-kva.example", "a.B\u00dcCHER.example"} {
		if !idn.Match(h) {
			t.Errorf("*.b\u00fccher.example did not match %q", h)
		}
	}
	if idn.Match("xn--bcher-kva.example") {
		t.Error("a wildcard matched its own apex")
	}
	if ascii := egress.NewHostSet([]string{"*.example.com"}); !ascii.Match("a.example.com") {
		t.Error("a plain ASCII wildcard stopped matching, which is what canonicalizing before the cut would do")
	}
}

// IDNA maps U+3002, U+FF0E and U+FF61 onto ".", so canonicalizing *creates*
// label boundaries. The empty-label check has to run on the canonical value or
// a leading fullwidth stop walks straight through a wildcard's boundary.
func TestAnEmptyLabelIsJudgedAfterCanonicalization(t *testing.T) {
	set := egress.NewHostSet([]string{"*.example.com"})
	for _, h := range []string{"\u3002example.com", "\u3002\u3002example.com"} {
		if set.Match(h) {
			t.Errorf("%q matched *.example.com; it canonicalizes to %q",
				h, egress.CanonicalHost(egress.NormalizeHost(h)))
		}
	}
	// A fullwidth stop between two real labels is a real boundary, and matching
	// it is correct IDNA rather than the same bug.
	if !set.Match("evil\u3002example.com") {
		t.Error("a fullwidth stop between two labels is a label boundary and should match")
	}
}

// A Unicode entry is stored as its A-label so the stored list holds one
// spelling per host; an ASCII entry is stored exactly as written.
func TestCanonicalEntryStoresOneSpellingPerHost(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"b\u00fccher.example", "xn--bcher-kva.example", true},
		{"*.b\u00fccher.example", "*.xn--bcher-kva.example", true},
		{"example.com", "example.com", true},
		{"*.example.com", "*.example.com", true},
		{"s3--us-west-2.example.com", "s3--us-west-2.example.com", true},
		{"192.0.2.1", "192.0.2.1", true},
		{"example.com:8080", "", false},
		{"_acme.example.com", "", false},
		{"\u00e4-.example", "", false}, // a trailing hyphen, caught by idna rather than by the label loop
	} {
		got, err := egress.CanonicalEntry(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("CanonicalEntry(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("CanonicalEntry(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A host longer than a name can be is folded and left alone: a sandbox writes
// the CONNECT authority itself, and canonicalizing a megabyte of U-labels costs
// orders of magnitude more than folding it.
func TestCanonicalHostLeavesAnOverlongNameToTheFold(t *testing.T) {
	long := strings.Repeat("\u00e4", 200) + ".EXAMPLE"
	want := strings.Repeat("\u00e4", 200) + ".example"
	if got := egress.CanonicalHost(long); got != want {
		t.Errorf("a %d-byte host was canonicalized rather than folded", len(long))
	}
}
