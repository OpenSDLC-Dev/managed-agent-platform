package api

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
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

// SetDeleteSessionAfterCommitHookForTest installs a hook fired between a
// session delete's commit and its terminal broadcasts, so a test can hang up
// on the request in that exact window and assert the frames still reach the
// subscribers holding a stream open. Test binary only.
func SetDeleteSessionAfterCommitHookForTest(f func()) (restore func()) {
	deleteSessionAfterCommitHook = f
	return func() { deleteSessionAfterCommitHook = nil }
}

// SetDeleteSessionBeforeCommitHookForTest installs a hook fired after a session
// delete has enqueued its orphaned object keys and before it commits, so a test
// can read the queue from another connection in that exact window and assert
// the rows are not there yet — which is what distinguishes an enqueue that
// rides the transaction from one that merely runs beside it. Test binary only.
func SetDeleteSessionBeforeCommitHookForTest(f func() error) (restore func()) {
	deleteSessionBeforeCommitHook = f
	return func() { deleteSessionBeforeCommitHook = nil }
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

// SetObjectDeleteIntervalForTest shortens the object-delete sweep's cadence so
// the one rung that must watch the interval itself fire — a key another replica
// enqueued, which raises no wake here — need not spend a minute doing it. Test
// binary only.
func SetObjectDeleteIntervalForTest(d time.Duration) (restore func()) {
	prev := objectDeleteInterval
	objectDeleteInterval = d
	return func() { objectDeleteInterval = prev }
}

// SetObjectDeleteBackoffForTest shortens the wait a refused key gets before it
// is tried again, so the recovery rung can watch the store come back without
// spending the production backoff. Test binary only.
// SetObjectDeleteCallBudgetForTest shortens the bound on one store call, so a
// rung can reach a store that hangs without waiting out the production budget.
// Test binary only.
func SetObjectDeleteCallBudgetForTest(d time.Duration) (restore func()) {
	prev := objectDeleteCallBudget
	objectDeleteCallBudget = d
	return func() { objectDeleteCallBudget = prev }
}

func SetObjectDeleteBackoffForTest(base, max time.Duration) (restore func()) {
	prevBase, prevMax := objectDeleteBackoffBase, objectDeleteBackoffMax
	objectDeleteBackoffBase, objectDeleteBackoffMax = base, max
	return func() { objectDeleteBackoffBase, objectDeleteBackoffMax = prevBase, prevMax }
}

// SchedulerTick runs exactly one deployment-scheduler tick against the pool
// at the given instant. The production loop is a ticker calling this with the
// database's own clock (SELECT now()), so a test that drives now covers every
// branch without a wall clock. blobs and cipher are nil: the cipher is never
// dialed by a fire (tokens are ciphertext copied as-is), and blobs only by
// one shape — initial_events carrying a file-rubric define_outcome, whose
// snapshot dials the store — so a test firing that shape through this seam
// gets the unclassified blobs-unconfigured arm, not a snapshot. Test binary
// only.
func SchedulerTick(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	return newServer(pool, nil, nil).deploymentTick(ctx, now)
}

// PurgeExpiredFilesForTest drives one sweep with a window the caller chooses,
// so the 30-day rule can be exercised in a test without 30 days or a fake
// clock. The production window is the package's own; only the statement is
// parameterized.
func PurgeExpiredFilesForTest(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int, error) {
	return purgeExpiredFiles(ctx, pool, retention)
}

// SetFilePurgeBeforeCommitHookForTest installs a hook fired after the sweep has
// enqueued the batch's object keys and before it commits; the file sweep's twin
// of SetDeleteSessionBeforeCommitHookForTest, and the only window from which
// "the row and the debt commit together" can be watched from outside the
// transaction — or failed, to watch the rollback.
func SetFilePurgeBeforeCommitHookForTest(f func() error) (restore func()) {
	filePurgeBeforeCommitHook = f
	return func() { filePurgeBeforeCommitHook = nil }
}

// SetFilePurgeAfterCommitHookForTest installs a hook fired between the sweep's
// commit and its return — the window where an enqueue that had moved after the
// commit is visible as an empty queue, and the only window in which it is.
func SetFilePurgeAfterCommitHookForTest(f func()) (restore func()) {
	filePurgeAfterCommitHook = f
	return func() { filePurgeAfterCommitHook = nil }
}

// SetFilePurgeBatchForTest shrinks one sweep's batch, so a test can see which
// rows a batch takes — at the production size every expired row fits in one and
// the order is unobservable.
func SetFilePurgeBatchForTest(n int) (restore func()) {
	prev := filePurgeBatch
	filePurgeBatch = n
	return func() { filePurgeBatch = prev }
}

// SetFilePurgeIntervalForTest shortens the expired-file sweep's cadence so a
// test can drive a tick without waiting an hour (SetMemoryPruneIntervalForTest's
// reason, for the sibling sweep).
func SetFilePurgeIntervalForTest(d time.Duration) (restore func()) {
	prev := filePurgeInterval
	filePurgeInterval = d
	return func() { filePurgeInterval = prev }
}

// SetDeploymentTickIntervalForTest sets the scheduler's cadence: short, so a
// wall-clock test can watch the ticker fire, or long, so one can watch the pass
// the loop takes before its first wait. Test binary only.
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
	prev := deploymentFireHookAfterBegin
	deploymentFireHookAfterBegin = f
	return func() { deploymentFireHookAfterBegin = prev }
}

// SetDeploymentFireHookInFireForTest installs a hook under SAVEPOINT fire
// whose error is handled exactly as a session-create failure — the seam for
// the unclassified whole-rollback arm, and for holding a winner's claim
// uncommitted while a competing caller runs. Test binary only.
func SetDeploymentFireHookInFireForTest(f func() error) (restore func()) {
	prev := deploymentFireHookInFire
	deploymentFireHookInFire = f
	return func() { deploymentFireHookInFire = prev }
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

// InsertAgentForTest writes an agent through the parse-and-insert body the
// dream runner shares with POST /v1/agents (insertAgentInTx): the request JSON,
// a caller-chosen id, and the internal flag no request can carry. It commits
// its own transaction and reports whether the id was new. Test binary only.
func InsertAgentForTest(ctx context.Context, pool *pgxpool.Pool, body, id string, internal bool) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inserted, err := newServer(pool, nil, nil).insertAgentInTx(ctx, tx, json.RawMessage(body), id, internal)
	if err != nil {
		return false, err
	}
	return inserted, tx.Commit(ctx)
}

// InsertEnvironmentForTest is InsertAgentForTest for environments
// (insertEnvironmentInTx). Test binary only.
func InsertEnvironmentForTest(ctx context.Context, pool *pgxpool.Pool, body, id string, internal bool) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inserted, err := newServer(pool, nil, nil).insertEnvironmentInTx(ctx, tx, json.RawMessage(body), id, internal)
	if err != nil {
		return false, err
	}
	return inserted, tx.Commit(ctx)
}

// CreateSessionForTest creates a session the way the dream runner will (plan 41
// §4.2 step 6): with the pre-minted id it must know before the row exists, and
// with the internal bypass that admits the runner's own hidden agent and
// environment. An empty id mints one, as a wire create does. Returns the
// created session's id. Test binary only.
func CreateSessionForTest(ctx context.Context, pool *pgxpool.Pool, id, envID, agentRaw string, internal bool) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := newServer(pool, nil, nil).createSessionInTx(ctx, tx, createSessionIn{
		id: id, internal: internal, envID: envID, agentRaw: json.RawMessage(agentRaw),
	})
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return created.row.id, nil
}

// InterruptSessionForTest runs the whole-session interrupt the dream runner's
// cancel and tick call (interruptSessionInTx), committing its transaction.
// Test binary only.
func InterruptSessionForTest(ctx context.Context, pool *pgxpool.Pool, sessionID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := newServer(pool, nil, nil).interruptSessionInTx(ctx, tx, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DreamTickForTest runs exactly one dream-runner tick against the pool at the
// given instant. The production loop is a ticker calling this with the
// database's own clock (SELECT now()), so a test that drives now covers the
// timeout and the start lease without a wall clock. Test binary only.
func DreamTickForTest(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, now time.Time, cfg DreamRunnerConfig) error {
	return newServer(pool, blobs, nil).dreamTick(ctx, now, cfg)
}

// DreamInternalIDsForTest exposes the fixed ids of the runner's hidden agent
// and environment, so a test asserts against the constants rather than a copy
// of their values. Test binary only.
func DreamInternalIDsForTest() (agentID, envID string) { return dreamAgentID, dreamEnvID }

// DreamInternalBodiesForTest exposes the request bodies the runner hands the
// two insert helpers, so a test can write the same bodies through the public
// create routes and compare what the handlers store. Test binary only.
func DreamInternalBodiesForTest() (agentBody, envBody string) { return dreamAgentBody, dreamEnvBody }

// SetDreamStartAttemptsForTest lowers the start-claim bound so the exhaustion
// arm resolves in a couple of ticks. Test binary only.
func SetDreamStartAttemptsForTest(n int) (restore func()) {
	prev := dreamStartAttempts
	dreamStartAttempts = n
	return func() { dreamStartAttempts = prev }
}

// SetDreamStartLeaseForTest shortens the soft lease so a crashed claimant's
// dream re-enters the candidate scan in test time. Test binary only.
func SetDreamStartLeaseForTest(d time.Duration) (restore func()) {
	prev := dreamStartLease
	dreamStartLease = d
	return func() { dreamStartLease = prev }
}

// DreamStageCount is §3.3's four, for the tests that walk the pipeline end to
// end rather than repeating the number. Test binary only.
const DreamStageCount = dreamStageCount

// DreamStageMessageForTest renders the message the runner posts to open a
// stage, so an api_test case compares the log against what the code says
// rather than against prose of its own. The in-place flag is a parameter
// rather than a fixed false because the two variants say different things
// about deletion, and a test that walked a create_new dream's log against an
// in-place rendering would pass on prose neither stage ever posted. Test
// binary only.
func DreamStageMessageForTest(stage int, storeMount string, transcripts int, instructions string, inPlace bool) string {
	return dreamStageMessage(stage, storeMount, transcripts, instructions, inPlace)
}

// SetDreamStageTurnCapForTest lowers one stage's turn cap so the over-budget
// arm can be driven with a handful of planted span.model_request_end rows.
// Per stage, because the caps are: a case must be able to lower the stage it
// drives and leave the others where §3.3 put them. Test binary only.
func SetDreamStageTurnCapForTest(stage, n int) (restore func()) {
	prev := dreamStageTurnCaps[stage]
	dreamStageTurnCaps[stage] = n
	return func() { dreamStageTurnCaps[stage] = prev }
}

// SetDreamCloneBatchForTest lowers the clone's multi-row insert width so a
// store of a handful of memories still crosses the batch boundary the
// production width (500) never reaches under test. Test binary only.
func SetDreamCloneBatchForTest(n int) (restore func()) {
	prev := dreamCloneBatch
	dreamCloneBatch = n
	return func() { dreamCloneBatch = prev }
}

// SetDreamStartHookAfterRenderForTest installs a hook in the window between
// the render's blob puts and the write transaction — the one §4.2 leaves
// unlocked, where a cancel lands and wins; a returned error drives the
// unclassified rollback instead. Test binary only.
func SetDreamStartHookAfterRenderForTest(f func() error) (restore func()) {
	prev := dreamStartHookAfterRender
	dreamStartHookAfterRender = f
	return func() { dreamStartHookAfterRender = prev }
}

// SetDreamStartHookInWriteForTest installs a hook inside the start's write
// transaction, with the dream row locked FOR UPDATE and nothing written yet —
// the seam for holding the row while a cancel waits at it. Test binary only.
func SetDreamStartHookInWriteForTest(f func()) (restore func()) {
	prev := dreamStartHookInWrite
	dreamStartHookInWrite = f
	return func() { dreamStartHookInWrite = prev }
}

// SetDreamHookAfterLockForTest installs a hook in the arm's window between the
// pipeline session's status read and its ask read, so a test can commit an
// ask-and-idle exactly there (§3.3). Test binary only.
func SetDreamHookAfterLockForTest(f func()) (restore func()) {
	prev := dreamHookAfterLock
	dreamHookAfterLock = f
	return func() { dreamHookAfterLock = prev }
}

// SetDreamLockWaitForTest shortens the lock_timeout every dream-row
// transaction sets, so the contention between a start's write and a cancel
// resolves in test time. Test binary only.
func SetDreamLockWaitForTest(d time.Duration) (restore func()) {
	prev := dreamLockWait
	dreamLockWait = d
	return func() { dreamLockWait = prev }
}

// FillSweepBudgetForTest saturates the pool's shared sweep budget and returns
// the release that hands every slot back, so a test can drive a tick that finds
// no connection to spend. Test binary only.
func FillSweepBudgetForTest(pool *pgxpool.Pool) (release func()) {
	b := sweepBudget(pool)
	held := 0
	for {
		select {
		case b <- struct{}{}:
			held++
		default:
			return func() {
				for range held {
					<-b
				}
				held = 0
			}
		}
	}
}

// SweepBudgetHeldForTest is how many slots of the pool's shared sweep budget
// are taken right now — zero once every arm of a finished tick has handed its
// slot back, however the arm ended. Test binary only.
func SweepBudgetHeldForTest(pool *pgxpool.Pool) int { return len(sweepBudget(pool)) }

// DreamCandidatesForTest runs the tick's candidate scan alone, so a test can
// hold a candidate list across another tick's claim and then run the arm on the
// stale id. Test binary only.
func DreamCandidatesForTest(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]string, error) {
	return scanDreamCandidates(ctx, pool, sweepBudget(pool), now)
}

// DreamArmForTest runs one candidate's arm — the transaction that re-reads the
// row under the scan's own predicate — without a scan in front of it. Test
// binary only.
func DreamArmForTest(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, id string,
	now time.Time, cfg DreamRunnerConfig) error {
	return newServer(pool, blobs, nil).runDreamArm(ctx, id, now, cfg)
}
