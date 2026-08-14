package vaultresolve

import (
	"net/url"
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
	host := lowerHost(u.Host)
	if port := u.Port(); (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		host = strings.TrimSuffix(host, ":"+port)
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

// lowerHost lowercases a host the way DNS allows and an IPv6 zone identifier
// does not: everything up to a "%" is folded, the zone after it is left exactly
// as written.
func lowerHost(host string) string {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		return strings.ToLower(host[:i]) + host[i:]
	}
	return strings.ToLower(host)
}

// matchesMCPServer reports whether a credential registered for credURL is the
// credential for an agent's server at serverURL. A string neither side can
// normalize matches nothing: `mcp_server_url` is validated at create and an
// agent's `url` at dial, so a URL that fails here is one the platform would not
// dial anyway, and matching it to a token would be the wrong direction to guess.
func matchesMCPServer(credURL, serverURL string) bool {
	a, aok := normalizeMCPURL(credURL)
	b, bok := normalizeMCPURL(serverURL)
	return aok && bok && a == b
}
