package vaultresolve

import (
	"net"
	"testing"
)

// AllowLoopbackTokenEndpointForTest lifts the address guard under the
// token-endpoint dial for one test, so the exchange can reach an httptest
// server. Without it every refresh test would be testing the guard.
//
// The guard is a package-level var, so the lift is process-wide for as long as
// the test runs: this package's tests must not call t.Parallel(). One of them
// asserts that the guard *is* installed, and a lift leaking in from a
// concurrent test would let its dial through — and under -race the swap itself
// would be a data race.
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
