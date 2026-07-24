package api

import (
	"net"
	"time"
)

// AllowLoopbackProbeForTest relaxes the validate probe's SSRF guard to permit
// loopback, so a test can point the probe at an httptest server (which listens
// on 127.0.0.1) — link-local and the other blocked classes stay refused so the
// guard's real targets remain covered. Test binary only.
func AllowLoopbackProbeForTest() (restore func()) {
	prev := probeIPAllowed
	probeIPAllowed = func(ip net.IP) error {
		if ip.IsLoopback() {
			return nil
		}
		return prev(ip)
	}
	return func() { probeIPAllowed = prev }
}

// ProbeIPAllowedForTest exposes the production SSRF predicate so a test can
// assert which addresses it refuses. Test binary only.
func ProbeIPAllowedForTest(ip net.IP) error { return productionProbeIPAllowed(ip) }

// SetUpdateCredentialResealHookForTest installs a hook fired between the
// unlocked re-seal read and the locked compare-and-set write in
// updateVaultCredential, so a test can rotate the stored ciphertext in that
// exact window and drive the CAS-conflict 409. Test binary only.
func SetUpdateCredentialResealHookForTest(f func()) (restore func()) {
	updateCredentialResealHook = f
	return func() { updateCredentialResealHook = nil }
}

// SetPingIntervalForTest shortens the SSE keepalive cadence so contract tests
// can observe ping frames without real-time waits. Test binary only.
func SetPingIntervalForTest(d time.Duration) (restore func()) {
	prev := ssePingInterval
	ssePingInterval = d
	return func() { ssePingInterval = prev }
}

// SetMaxFileBytesForTest lowers the Files per-file cap so the 413 path can be
// exercised without streaming half a gigabyte through a test. Test binary only.
func SetMaxFileBytesForTest(n int64) (restore func()) {
	prev := maxFileBytes
	maxFileBytes = n
	return func() { maxFileBytes = prev }
}
