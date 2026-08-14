package vaultresolve

import (
	"net/url"
	"strconv"
	"strings"
)

// normalizeMCPURL renders one URL into the form two of them are compared in, or
// reports false for a string that is not an http(s) URL with a host.
//
// The rule is the reference's, quoted whole because every clause of it is a
// decision: "Both URLs are normalized before matching (scheme and host
// lowercased, default ports and trailing slashes stripped), so differences in
// host casing, a default port, or a trailing slash don't prevent a match; a
// different path, subdomain, or non-default port does."
//
// So four operations and no others. Everything the sentence does not name is
// compared as it was given — the path (case included: only the scheme and the
// host are lowercased), the query, the fragment, and any userinfo, which
// `mcp_server_url` validation admits. The query in particular is left alone
// deliberately rather than by omission: an MCP endpoint may carry one, and two
// URLs differing in it are as different as two differing in their path.
//
// Two readings the sentence leaves open, both settled toward *not* matching,
// because a false match sends a session's bearer token to a server the
// credential was not registered for, while a false miss connects unauthenticated
// and surfaces as an authentication error the operator can see:
//
//   - "trailing slashes stripped" strips exactly one, so "https://x//" and
//     "https://x/" stay different.
//   - a host's IPv6 zone identifier is not folded. It is locally significant and
//     may distinguish two interfaces that differ only in case, and folding it is
//     the one way this could equate two hosts that are not the same one — the
//     same reasoning internal/mcp's sameHost is written on.
//
// Both are recorded in docs/DIVERGENCES.md.
func normalizeMCPURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	// The reference's "scheme lowercased" is url.Parse's own doing (net/url
	// lowercases Scheme as it parses), so this compares against the lower-case
	// spellings and adds no fold of its own.
	scheme := u.Scheme
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if u.Host == "" {
		return "", false
	}
	// A trailing colon with no port after it is not a port: net/http removes it
	// before the request goes out, so "host:" and "host" are one origin on the
	// wire and have to be one key here.
	host := strings.TrimSuffix(lowerHost(u.Host), ":")
	// The port is compared as a number, not as text. Go accepts a zero-padded
	// port and dials it as the number it spells, so ":0443" is the https default
	// written differently — and leaving it unstripped would put a credential and
	// an agent that spell one endpoint two ways into different keys.
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err == nil &&
			((scheme == "http" && n == 80) || (scheme == "https" && n == 443)) {
			host = strings.TrimSuffix(host, ":"+port)
		}
	}
	// The key is assembled rather than rendered through url.URL.String(), which
	// re-encodes from the decoded Path and would equate two URLs this must keep
	// apart: "/a%2Fb" and "/a/b" are one Path once parsed, and only the escaped
	// form still tells them apart.
	key := scheme + "://"
	if u.User != nil {
		key += u.User.String() + "@"
	}
	key += host + strings.TrimSuffix(u.EscapedPath(), "/")
	if u.ForceQuery || u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		key += "#" + u.EscapedFragment()
	}
	return key, true
}

// lowerHost folds a host's case the way DNS does — ASCII only — leaving an IPv6
// zone identifier alone.
//
// ASCII only is the security-relevant half. strings.ToLower folds by Unicode, and
// Unicode maps several non-ASCII letters onto ASCII ones: it lowercases U+0130
// (LATIN CAPITAL LETTER I WITH DOT ABOVE) to plain "i", so "İ.example" and
// "i.example" would share one key. They are not one host — Go's HTTP stack
// resolves the first through IDNA to "xn--i-9bb.example" — so a credential
// registered for the IDN would be selected for the ASCII domain and its token
// sent there. Case folding a hostname is an ASCII operation; anything else is a
// different name.
//
// The zone identifier is left as written for the same reason internal/mcp's
// sameHost leaves it: it is locally significant and may distinguish two
// interfaces that differ only in case.
func lowerHost(host string) string {
	end := len(host)
	if i := strings.IndexByte(host, '%'); i >= 0 {
		end = i
	}
	var b strings.Builder
	b.Grow(len(host))
	for i := 0; i < end; i++ {
		c := host[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	b.WriteString(host[end:])
	return b.String()
}

// matchesMCPServer reports whether a credential registered for credURL is the
// credential for an agent's server at serverURL. A string neither side can
// normalize matches nothing: `mcp_server_url` is validated at create and an
// agent's `url` at dial, so a URL that fails here is one the platform would not
// dial anyway, and matching it to a token would be the wrong direction to guess.
func matchesMCPServer(credURL, serverURL string) bool {
	wanted, ok := normalizeMCPURL(serverURL)
	return ok && matchesNormalized(credURL, wanted)
}

// matchesNormalized is the half the scan loop runs per candidate row, against a
// server key normalized once for the whole query rather than once per row.
func matchesNormalized(credURL, wanted string) bool {
	got, ok := normalizeMCPURL(credURL)
	return ok && got == wanted
}
