package vaultresolve

import (
	"net"
	"testing"
)

// AllowLoopbackTokenEndpointForTest lifts the address guard under the
// token-endpoint dial for one test, so the exchange can reach an httptest
// server. Without it every refresh test would be testing the guard.
func AllowLoopbackTokenEndpointForTest(t *testing.T) {
	t.Helper()
	old := refreshIPAllowed
	refreshIPAllowed = func(net.IP) error { return nil }
	t.Cleanup(func() { refreshIPAllowed = old })
}

// MatchesMCPServerForTest exposes the credential-matching rule to the package's
// external test, which is where the rest of this package is tested from.
func MatchesMCPServerForTest(credURL, serverURL string) bool {
	return matchesMCPServer(credURL, serverURL)
}

// NormalizeMCPURLForTest exposes the normalized comparison key, so a test can
// assert what a URL was reduced to and not only which pairs matched.
func NormalizeMCPURLForTest(raw string) (string, bool) {
	return normalizeMCPURL(raw)
}
