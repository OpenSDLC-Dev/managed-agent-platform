package api

import (
	"context"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
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
func ProbeIPAllowedForTest(ip net.IP) error { return dialguard.IPAllowed(ip) }

// SetUpdateCredentialResealHookForTest installs a hook fired between the
// unlocked re-seal read and the locked compare-and-set write in
// updateVaultCredential, so a test can rotate the stored ciphertext in that
// exact window and drive the CAS-conflict 409. Test binary only.
func SetUpdateCredentialResealHookForTest(f func()) (restore func()) {
	updateCredentialResealHook = f
	return func() { updateCredentialResealHook = nil }
}

// ScrubberCleanForTest builds a scrubber from the given literal needles (in
// order) and runs its redaction over text, so a test can assert the
// longest-first ordering without reaching into unexported internals. Test
// binary only.
func ScrubberCleanForTest(needles []string, text string) string {
	s := &scrubber{}
	for _, n := range needles {
		s.add(n)
	}
	return s.clean(text)
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

// SetMemoryPruneIntervalForTest shortens the retention sweep's cadence so a
// test can observe the loop actually sweeping rather than only returning.
// Test binary only.
func SetMemoryPruneIntervalForTest(d time.Duration) (restore func()) {
	prev := memoryPruneInterval
	memoryPruneInterval = d
	return func() { memoryPruneInterval = prev }
}

// SchedulerTick runs exactly one deployment-scheduler tick against the pool
// at the given instant. The production loop is a ticker calling this with the
// database's own clock (SELECT now()), so a test that drives now covers every
// branch without a wall clock. blobs and cipher are nil: nothing a fire does
// dials either — tokens are ciphertext copied as-is, and file resources
// reference rows, not blobs — and the arms that would need them are driven
// through the HTTP surface instead. Test binary only.
func SchedulerTick(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	return newServer(pool, nil, nil).deploymentTick(ctx, now)
}

// SetDeploymentTickIntervalForTest shortens the scheduler's cadence so the
// one wall-clock test can watch the ticker actually fire. Test binary only.
func SetDeploymentTickIntervalForTest(d time.Duration) (restore func()) {
	prev := deploymentTickInterval
	deploymentTickInterval = d
	return func() { deploymentTickInterval = prev }
}

// SetDeploymentCatchupWindowForTest narrows the catch-up window so the
// aged-out branch can be driven with near-now instants. Test binary only.
func SetDeploymentCatchupWindowForTest(d time.Duration) (restore func()) {
	prev := deploymentCatchupWindow
	deploymentCatchupWindow = d
	return func() { deploymentCatchupWindow = prev }
}

// SetDeploymentLockWaitForTest shortens the fire's lock_timeout so the
// lost-claim-by-timeout branch resolves in test time. Test binary only.
func SetDeploymentLockWaitForTest(d time.Duration) (restore func()) {
	prev := deploymentLockWait
	deploymentLockWait = d
	return func() { deploymentLockWait = prev }
}

// SetDeploymentFireHookAfterBeginForTest installs a hook between the fire
// transaction's open and its deployment re-read, so a test can archive or
// pause the deployment in that exact window. Test binary only.
func SetDeploymentFireHookAfterBeginForTest(f func()) (restore func()) {
	deploymentFireHookAfterBegin = f
	return func() { deploymentFireHookAfterBegin = nil }
}

// SetDeploymentFireHookInFireForTest installs a hook under SAVEPOINT fire
// whose error is handled exactly as a session-create failure — the seam for
// the unclassified whole-rollback arm, and for holding a winner's claim
// uncommitted while a competing caller runs. Test binary only.
func SetDeploymentFireHookInFireForTest(f func() error) (restore func()) {
	deploymentFireHookInFire = f
	return func() { deploymentFireHookInFire = nil }
}

// DeploymentSkipScanCapForTest exposes the saturation bound of the skipped
// count so the cold-backlog test asserts against the constant rather than a
// copy of its value. Test binary only.
func DeploymentSkipScanCapForTest() int { return deploymentSkipScanCap }

// DeploymentPausingErrorTypesForTest exposes the paused-reason union mapping
// so the test can assert it against the migration's CHECK constraint. Test
// binary only.
func DeploymentPausingErrorTypesForTest() []string {
	types := make([]string, 0, len(deploymentPausingErrorTypes))
	for t := range deploymentPausingErrorTypes {
		types = append(types, t)
	}
	return types
}
