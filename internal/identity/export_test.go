package identity

import (
	"net/http"
	"time"
)

// The tuning constants, so an external test advances a clock by exactly the bound
// production uses rather than by a number copied out of the source.
const (
	KeySetTTLForTest       = keySetTTL
	RefreshCooldownForTest = refreshCooldown
	MaxIdPBytesForTest     = maxIdPBytes
	MaxKeysForTest         = maxKeys
	MaxTokenBytesForTest   = maxTokenBytes
	MaxSubjectBytesForTest = maxSubjectBytes
	ClockSkewLeewayForTest = clockSkewLeeway
	MaxRoleValuesForTest   = maxRoleValues
	MaxClaimDepthForTest   = maxClaimDepth
	MaxProfileBytesForTest = maxProfileBytes
)

// SetFetchTimeoutForTest shortens one verifier's key-fetch deadline, retiring the
// only branch a fake clock cannot reach: a real context deadline. Per-verifier
// rather than package-global, so subtests keep t.Parallel().
func SetFetchTimeoutForTest(v *Verifier, d time.Duration) (restore func()) {
	prev := v.keys.timeout
	v.keys.timeout = d
	return func() { v.keys.timeout = prev }
}

// ProductionClientForTest exposes the client a Config with no HTTPClient selects,
// so a test can prove the dial guard is wired rather than aspirational.
func ProductionClientForTest() *http.Client { return productionClient }
