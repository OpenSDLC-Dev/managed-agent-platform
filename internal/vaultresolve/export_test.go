package vaultresolve

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
