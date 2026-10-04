package admin

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

const maxNameLen = 256

// checkName requires a non-empty value of at most maxNameLen characters with
// no control character.
func checkName(field, s string) error {
	switch {
	case s == "":
		return invalid("%s is required", field)
	case utf8.RuneCountInString(s) > maxNameLen:
		return invalid("%s is longer than %d characters", field, maxNameLen)
	case strings.IndexFunc(s, unicode.IsControl) >= 0:
		return invalid("%s contains a control character", field)
	}
	return nil
}

// checkOptionalName is checkName for a field that may be empty.
func checkOptionalName(field, s string) error {
	if s == "" {
		return nil
	}
	return checkName(field, s)
}

// checkToken requires a name a caller sends verbatim — an alias, or a key in
// a header — so no whitespace either.
func checkToken(field, s string, max int) error {
	switch {
	case s == "":
		return invalid("%s is required", field)
	case utf8.RuneCountInString(s) > max:
		return invalid("%s is longer than %d characters", field, max)
	case strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return invalid("%s contains whitespace or a control character", field)
	}
	return nil
}

// checkAliasName is checkToken for an alias name, which may hold slashes
// ("Qwen/Qwen3-Coder") but no segment the router would rewrite: an empty one,
// "." or "..", which ServeMux answers by redirecting to another path — a
// DELETE of "x/../y" sent to "y".
func checkAliasName(s string) error {
	if err := checkToken("name", s, maxNameLen); err != nil {
		return err
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return invalid("name %q has an empty, \".\" or \"..\" path segment", s)
		}
	}
	return nil
}

// checkEndpoint returns a base URL without its trailing slash, refusing one
// that could carry a secret: userinfo, a query or a fragment. A secret is a
// credential. http is allowed: an in-cluster model server often serves no TLS,
// and the console marks such a provider.
func checkEndpoint(proto, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", invalid("endpoints.%s must be an absolute http or https URL", proto)
	}
	// url.Parse checks a port is digits, not that it is one: an endpoint is
	// fixed at creation, so a typo it accepts outlives every dial it fails.
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", invalid("endpoints.%s has port %s; a port is 1 to 65535", proto, port)
		}
	}
	switch {
	case u.User != nil:
		return "", invalid("endpoints.%s carries userinfo; a secret belongs in a credential", proto)
	case u.RawQuery != "" || u.ForceQuery:
		return "", invalid("endpoints.%s carries a query; a secret belongs in a credential", proto)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", invalid("endpoints.%s carries a fragment", proto)
	}
	return strings.TrimRight(raw, "/"), nil
}

// checkHeaders refuses a header the redactor treats as a credential (a secret
// belongs in a credential), a name that is not an HTTP token, a value that
// could split the header, and two names alike but for case, which are one
// HTTP header whose value would depend on map order.
func checkHeaders(h map[string]string) error {
	seen := make(map[string]bool, len(h))
	for name, value := range h {
		if name == "" || strings.IndexFunc(name, func(r rune) bool { return !isTokenChar(r) }) >= 0 {
			return invalid("header %q is not a valid HTTP header name", name)
		}
		if provider.IsCredentialName(name) {
			return invalid("header %q would carry a credential; a secret belongs in a credential", name)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return invalid("header %q has a value with a line break or NUL", name)
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			return invalid("header %q is set more than once, in different cases", lower)
		}
		seen[lower] = true
	}
	return nil
}

// isTokenChar reports an RFC 9110 tchar.
func isTokenChar(r rune) bool {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

// The bounds on a weight and a priority: enough to express any split, small
// enough that a sum of them cannot overflow.
const (
	maxWeight   = 1000
	maxPriority = 1000
)

func checkWeight(field string, w int) error {
	if w < 1 || w > maxWeight {
		return invalid("%s must be between 1 and %d", field, maxWeight)
	}
	return nil
}
