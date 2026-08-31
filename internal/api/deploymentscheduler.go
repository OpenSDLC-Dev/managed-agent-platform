package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/cron"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// The deployment scheduler (plan 37 §4): a controlplane background sweep — a
// ticker calling one stateless function — not a fifth binary. Every replica
// runs it; there is no leader election, because the guarantee is at most one
// committed run per occurrence and that rests entirely on the partial unique
// index over (deployment_id, scheduled_at) — the reference's own published
// idempotency key. Liveness inside the catch-up window is best-effort;
// uniqueness is absolute.
//
// The clock is Postgres's: one SELECT now() per tick feeds the pure occurrence
// computation, never time.Now() — sourced from the replica's own clock, an
// hour-fast host would fire occurrences early and the unique index would then
// refuse the honest fires from the correct-clock replicas, silently shifting
// the whole fleet's schedule (§4.2).

// apiTracerName is this package's OTel trace instrumentation scope — the same
// string as its meter scope.
const apiTracerName = apiMeterName

const (
	// MetricDeploymentFires counts fire attempts by outcome: "created" (a run
	// row with a session), "failed" (a run row settled on a classified error,
	// sub-attributed by error.type) and "abandoned" (an unclassified failure
	// rolled the whole transaction back, so there is no run row and no error
	// type to label it with — without this outcome an infrastructure failure
	// would be invisible in the only place that counts fires). A lost claim is
	// not a fire — another replica's is — and counts nothing. Exported so the
	// test can assert the exact name.
	MetricDeploymentFires = "deployment.fires"
	// MetricDeploymentOccurrencesSkipped counts occurrences a catch-up
	// collapse passed over (§3.6) — the one number that makes a dropped
	// backlog debuggable. It rides the claim, not the tick: only the winner
	// adds, after its commit, so a rolled-back claim cannot leave a count the
	// retry would double.
	MetricDeploymentOccurrencesSkipped = "deployment.occurrences.skipped"
	// MetricDeploymentTickDuration times one whole sweep. A tick approaching
	// the tick interval is the signal that the fleet has outgrown one sweep —
	// and that deploymentFireConcurrency is wrong.
	MetricDeploymentTickDuration = "deployment.tick.duration"
)

const (
	fireOutcomeCreated   = "created"
	fireOutcomeFailed    = "failed"
	fireOutcomeAbandoned = "abandoned"
	// fireOutcomeLost is a claim another replica owns, or a deployment
	// archived or paused between the candidate scan and the fire's re-read.
	// Nothing is recorded: the fire that counts is the winner's.
	fireOutcomeLost = ""
)

// deploymentFireConcurrency bounds how many fires one tick runs at once. A
// fire is a full session creation, and thirty of them serialized behind one
// tick would overrun the 30-second interval (§4.4); the connection budget is
// one per in-flight fire plus one per replica briefly blocked on a competing
// claim, so this constant is also (most of) the scheduler's draw on the pool.
const deploymentFireConcurrency = 4

// deploymentTickInterval paces the sweep, and is therefore the fire latency:
// with no jitter (§3.4, registered), an occurrence fires at the first tick
// that sees it due — 0-30 seconds after its instant. A var so the test binary
// can drive the ticker without a wall clock; export_test.go holds the setter.
var deploymentTickInterval = 30 * time.Second

// deploymentCatchupWindow bounds how far back a tick will fire. A tick fires
// at most the single most recent due occurrence per deployment, and only if
// it falls inside this window — a day-long outage on a */5 schedule would
// otherwise spawn 288 sessions at once, and the unique index would admit
// every one (§3.6, registered: no source describes the reference's own
// behavior here). A package constant, not a knob: an env variable would have
// exactly two useful values, and its zero would drop a run on every rolling
// upgrade. A var only for the test setter.
var deploymentCatchupWindow = time.Hour

// deploymentSkipScanCap bounds the walk that counts a collapse. The count's
// floor is deliberately not the window (§3.6 — a backlog spanning months is
// still the operator's loss to see), but enumerating it must be bounded, or
// the count itself becomes the cost the window exists to avoid. Past the cap
// the counter saturates: at that magnitude the signal is the size class, not
// the digit.
const deploymentSkipScanCap = 1000

// deploymentLockWait is the lock_timeout the fire's transaction sets, and the
// one archive, pause and unpause set (§4.1 steps 2-3). The fire's side reads
// a 55P03 as a lost claim: a competing replica's uncommitted claim blocks the
// loser's insert for the winner's whole fire, and without the timeout the
// loser would hold a pool connection all that time. The handlers' side
// surfaces a wedged fire as a failed request rather than an indefinite hang.
// A var only for the test setter.
var deploymentLockWait = 5 * time.Second

// deploymentFireHookAfterBegin, when non-nil, runs after the fire's
// transaction opens and before the deployment re-read — the seam that proves
// the re-read happens under this transaction (an archive committed in that
// window must stop the fire). Always nil in production.
var deploymentFireHookAfterBegin func()

// deploymentFireHookInFire, when non-nil, runs under SAVEPOINT fire before
// createSessionInTx; its error is handled exactly as one from
// createSessionInTx — which is what lets a test drive the unclassified
// whole-rollback arm, and hold a winner's claim uncommitted while a competing
// caller runs. Always nil in production.
var deploymentFireHookInFire func() error

// deploymentPausingErrorTypes is the paused-reason union: the fourteen run
// error types whose recording on a scheduled fire auto-pauses the deployment.
// The run union's other two members — session_rate_limited_error and
// session_creation_rejected_error — are absent deliberately; a pause carrying
// either is unrepresentable on the wire. Seven of the fourteen are produced
// by no path in this platform (§5.2 does the accounting; each is registered
// in docs/DIVERGENCES.md) and are listed anyway: the test asserts this map
// against the migration's CHECK, so the two cannot drift.
var deploymentPausingErrorTypes = map[string]bool{
	"agent_archived_error":                    true,
	"environment_archived_error":              true,
	"environment_not_found_error":             true,
	"file_not_found_error":                    true,
	"mcp_egress_blocked_error":                true,
	"memory_store_archived_error":             true,
	"organization_disabled_error":             true,
	"self_hosted_resources_unsupported_error": true,
	"session_resource_not_found_error":        true,
	"skill_not_found_error":                   true,
	"unknown_error":                           true,
	"vault_archived_error":                    true,
	"vault_not_found_error":                   true,
	"workspace_archived_error":                true,
}

// StartDeploymentScheduler ticks until ctx ends. Safe on every replica at
// once: the occurrence claim is a unique-index insert, so N replicas cost
// N-1 briefly-blocked losers per due occurrence and never a duplicate run.
// blobs and cipher are the handler's own — a fire is a full session creation
// and needs whatever POST /v1/sessions needs (the cipher is still never
// dialed at fire time: repository tokens were sealed at deployment
// create/update, and the fire copies ciphertext).
func StartDeploymentScheduler(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, cipher secrets.Cipher) {
	s := newServer(pool, blobs, cipher)
	t := time.NewTicker(deploymentTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// The one SELECT now() per tick (§4.2): the database's clock, shared
		// by every replica, is the only one the occurrence math may see.
		var now time.Time
		if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "deployment tick skipped: reading the database clock failed", "error", err)
			}
			continue
		}
		if err := s.deploymentTick(ctx, now); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "deployment tick incomplete; the next interval retries", "error", err)
		}
	}
}

// deploymentFire is one due occurrence a tick decided to fire: the schedule
// pair it was computed from rides along so the fire's re-read can tell a
// replaced schedule from the one this occurrence belongs to.
type deploymentFire struct {
	deploymentID string
	expr, tz     string
	occ          time.Time
	skipped      int64
}

// deploymentTick runs one sweep at the given instant: scan the candidates,
// compute each one's most recent due occurrence, and fire the ones inside the
// catch-up window. A tick that fires roots its own trace — there is no HTTP
// request, so no inbound traceparent — with each fire's span a child; an idle
// sweep exports no span at all (2,880 empty root traces a day per replica
// would bury the fires an operator looks for), while the duration histogram
// records either way.
func (s *server) deploymentTick(ctx context.Context, now time.Time) error {
	start := time.Now()
	defer func() { recordDeploymentTickDuration(ctx, time.Since(start)) }()

	// The watermark — MAX(scheduled_at) over committed runs, served by the
	// partial unique index via the MIN/MAX rewrite — is derived, never
	// stored: a denormalized copy would be a second source of truth that any
	// forgetful writer could desynchronize from the run history (§4.2).
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.schedule_expression, d.schedule_timezone, d.schedule_resumed_at,
		       (SELECT MAX(r.scheduled_at) FROM deployment_runs r
		         WHERE r.deployment_id = d.id AND r.scheduled_at IS NOT NULL)
		  FROM deployments d
		 WHERE d.schedule_expression IS NOT NULL
		   AND d.archived_at IS NULL
		   AND d.paused_at IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		fires    []deploymentFire
		scanErrs []error
	)
	for rows.Next() {
		var (
			id, expr, tz string
			resumedAt    time.Time
			watermark    *time.Time
		)
		if err := rows.Scan(&id, &expr, &tz, &resumedAt, &watermark); err != nil {
			return err
		}
		// The floor is exclusive on both contributors: the watermark is the
		// last occurrence some fire claimed, and schedule_resumed_at is what
		// makes "missed triggers are not backfilled" implementable — without
		// it, no run row moves the watermark during a pause, so the first
		// tick after an unpause would fire the occurrence that fell due
		// inside it (§4.2). The same floor stops a new deployment firing
		// occurrences that predate it.
		lower := resumedAt
		if watermark != nil && watermark.After(lower) {
			lower = *watermark
		}
		if !lower.Before(now) {
			continue
		}
		// The fire lookup is clamped to the catch-up window: an occurrence at
		// or below now-window can never fire, so walking a months-old backlog
		// to find the one candidate would be pure cost — a cold */1 schedule
		// would otherwise enumerate every minute since its creation, on every
		// tick, until its first commit establishes a watermark. Due's lower
		// bound is exclusive, so everything returned is strictly inside the
		// window and the most recent element is the fire — the same occurrence
		// the unclamped walk would have chosen, found without the walk.
		fireFloor := lower
		if w := now.Add(-deploymentCatchupWindow); w.After(fireFloor) {
			fireFloor = w
		}
		due, err := cron.Due(expr, tz, fireFloor, now)
		if err != nil {
			// Unreachable for a stored schedule — create and update refuse
			// an expression Upcoming cannot walk — but joined rather than
			// returned: one poisoned row (a zone a tzdata bump dropped, a
			// hand-written migration) must cost that deployment its fires,
			// never the whole fleet's.
			scanErrs = append(scanErrs, fmt.Errorf("deployment %s: %w", id, err))
			continue
		}
		if len(due) == 0 {
			continue
		}
		// At most the single most recent due occurrence fires; the ones it
		// passed over are the skipped count the winner will carry, bounded
		// below by the unclamped floor (§3.6). An occurrence that aged out of
		// the window with no later fire at all is never selected and never
		// counted — the best-effort boundary §4.1 draws around liveness.
		occ := due[len(due)-1]
		skipped := int64(len(due) - 1)
		if fireFloor.After(lower) {
			if skipped, err = occurrencesSkipped(expr, tz, lower, occ); err != nil {
				scanErrs = append(scanErrs, fmt.Errorf("deployment %s: %w", id, err))
				continue
			}
		}
		fires = append(fires, deploymentFire{deploymentID: id, expr: expr, tz: tz, occ: occ, skipped: skipped})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if len(fires) == 0 {
		return errors.Join(scanErrs...)
	}

	// The fire concurrency is clamped to the pool: pgxpool's default MaxConns
	// is max(4, NumCPU), so on a small host the constant alone could pin
	// every connection for up to a contended fire's lock_timeout, queueing
	// each HTTP handler behind the sweep (§4.4's budget). Two are always left
	// for the rest of the process — the SSE broker holds one for its LISTEN
	// loop whenever a subscriber exists.
	concurrency := deploymentFireConcurrency
	if m := int(s.pool.Config().MaxConns) - 2; m < concurrency {
		concurrency = max(1, m)
	}

	ctx, span := otel.GetTracerProvider().Tracer(apiTracerName).Start(ctx, "deployment.tick")
	defer span.End()
	sem := make(chan struct{}, concurrency)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, f := range fires {
		wg.Add(1)
		sem <- struct{}{}
		go func(f deploymentFire) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.fireScheduled(ctx, f); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(f)
	}
	wg.Wait()
	return errors.Join(append(scanErrs, errs...)...)
}

// occurrencesSkipped counts the occurrences in (lower, occ) — what a fire at
// occ passed over — walking at most deploymentSkipScanCap of them. Only the
// clamped path calls it; when the floor is inside the window the due list
// already holds the exact count.
func occurrencesSkipped(expr, tz string, lower, occ time.Time) (int64, error) {
	batch, err := cron.Upcoming(expr, tz, lower, deploymentSkipScanCap)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, t := range batch {
		if !t.Before(occ) {
			break
		}
		n++
	}
	return n, nil
}

// fireScheduled wraps one fire in its span and settles the post-commit
// observability: the fires counter by outcome, and — only when the claim
// committed — the skipped count it carried. An abandoned fire is the one red
// span: a run the platform recorded, error and all, is a fire it handled
// correctly.
func (s *server) fireScheduled(ctx context.Context, f deploymentFire) error {
	ctx, span := otel.GetTracerProvider().Tracer(apiTracerName).Start(ctx, "deployment.fire",
		trace.WithAttributes(
			attribute.String("deployment.id", f.deploymentID),
			attribute.String("trigger_type", "schedule"),
			attribute.String("scheduled_at", f.occ.UTC().Format(time.RFC3339)),
		))
	defer span.End()

	outcome, errType, err := s.fireScheduledTx(ctx, f)
	switch {
	case err != nil && isDeadlockVictim(err):
		// A deadlock victim from any statement of the fire — the auto-pause
		// UPDATE is the reachable one — had its whole transaction rolled
		// back: nothing was committed, and the occurrence is retried or
		// already another actor's. Quiet, like a lost claim. A 55P03 is
		// deliberately NOT caught here: outside the two claim-phase
		// statements (which read it locally), a lock timeout deep in the
		// session create is a diagnosable contention fault, and swallowing
		// it would make a wedged environment row indistinguishable from
		// "nothing was due" — it falls through to abandoned, counted and
		// logged, exactly as §4.1 scopes the quiet reading to the claim.
		return nil
	case err != nil:
		if ctx.Err() != nil {
			// A shutdown mid-fire aborts the transaction; the occurrence
			// stays unclaimed and the next replica's tick retries it.
			// Counting that as abandoned would redden every deploy.
			return err
		}
		span.SetStatus(codes.Error, err.Error())
		recordDeploymentFire(ctx, fireOutcomeAbandoned, "")
		slog.ErrorContext(ctx, "deployment fire abandoned: no run row was recorded",
			"deployment_id", f.deploymentID, "scheduled_at", f.occ, "error", err)
		return err
	case outcome == fireOutcomeLost:
		return nil
	default:
		recordDeploymentFire(ctx, outcome, errType)
		recordDeploymentOccurrencesSkipped(ctx, f.skipped)
		return nil
	}
}

// fireScheduledTx is §4.1's one transaction: re-read the deployment under
// lock, claim the occurrence through the unique index, create the session
// under a savepoint, and settle exactly one of session_id and error — with
// the fourteen pausing types stopping the schedule in the same commit. The
// savepoint is what makes the classified arm possible without a second
// transaction, and a second transaction is what would let a crash leave a
// run row with both columns null — the shape the reference forbids.
func (s *server) fireScheduledTx(ctx context.Context, f deploymentFire) (outcome, errType string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Transaction-scoped, so it covers both the FOR SHARE below and the
	// claim insert — a loser blocking on the winner's uncommitted claim
	// would otherwise hold its connection for the winner's whole fire.
	if err := setDeploymentLockWait(ctx, tx); err != nil {
		return "", "", err
	}
	if h := deploymentFireHookAfterBegin; h != nil {
		h()
	}

	// The candidate scan and the fire are separate statements, so without
	// this re-read a fire could create a session for a deployment whose
	// archive had already returned 200 — and archive is terminal. Zero rows
	// means archived or paused since the scan: not this replica's fire.
	// FOR SHARE: archive, pause and unpause write this row, so the two
	// cannot interleave; a competing fire's FOR SHARE is compatible, which
	// is what lets a manual run fire concurrently. The row also supplies
	// everything the fire consumes — raw resources included, because the
	// stored form is what carries the sealed tokens: the cipher is never
	// dialed here.
	var (
		envID, agentID        string
		agentVersion          int
		vaultIDs              []string
		initial, rawResources []byte
		curExpr, curTZ        *string
		curResumedAt          time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT environment_id, agent_id, agent_version, vault_ids, initial_events, resources,
		       schedule_expression, schedule_timezone, schedule_resumed_at
		  FROM deployments
		 WHERE id = $1 AND archived_at IS NULL AND paused_at IS NULL
		 FOR SHARE`, f.deploymentID).
		Scan(&envID, &agentID, &agentVersion, &vaultIDs, &initial, &rawResources,
			&curExpr, &curTZ, &curResumedAt)
	if errors.Is(err, pgx.ErrNoRows) || isLostRace(err) {
		return fireOutcomeLost, "", nil
	}
	if err != nil {
		return "", "", err
	}
	// The scheduling state may have moved in the same window: an update can
	// have replaced or removed the schedule this occurrence was computed
	// from, and a pause/unpause round-trip restamps the resume floor above
	// it — an occurrence behind that floor is exactly what "missed triggers
	// are not backfilled" forbids. Any of those makes the occurrence stale,
	// not this fire's to run; the next tick recomputes under the row as it
	// now stands.
	if curExpr == nil || curTZ == nil || *curExpr != f.expr || *curTZ != f.tz || !f.occ.After(curResumedAt) {
		return fireOutcomeLost, "", nil
	}

	// The claim. The conflict target is named on purpose: a bare DO NOTHING
	// would swallow every unique violation — an id collision, a constraint a
	// later migration adds — and report each as a lost race, silently
	// dropping a fire that should have been a 500. Zero rows against a
	// committed conflicting row returns at once; against an uncommitted one
	// Postgres waits on the winner's transaction until lock_timeout fires,
	// and that 55P03 is read as the same lost claim by a different route.
	runID := domain.NewID(domain.PrefixDeploymentRun)
	var claimed int
	err = tx.QueryRow(ctx, `
		INSERT INTO deployment_runs (id, deployment_id, trigger_type, scheduled_at, agent_id, agent_version)
		VALUES ($1, $2, 'schedule', $3, $4, $5)
		ON CONFLICT (deployment_id, scheduled_at) WHERE scheduled_at IS NOT NULL DO NOTHING
		RETURNING 1`,
		runID, f.deploymentID, f.occ, agentID, agentVersion).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) || isLostRace(err) {
		return fireOutcomeLost, "", nil
	}
	if err != nil {
		return "", "", err
	}

	in, err := deploymentSessionIn(f.deploymentID, envID, agentID, agentVersion, vaultIDs, initial, rawResources)
	if err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT fire`); err != nil {
		return "", "", err
	}
	var created createdSession
	fireErr := error(nil)
	if h := deploymentFireHookInFire; h != nil {
		fireErr = h()
	}
	if fireErr == nil {
		// The ticker's ctx carries no principal, so the session is created
		// unattributed — plan §9's NULL created_by is the schedule's own.
		created, fireErr = s.createSessionInTx(ctx, tx, in)
	}
	if fireErr != nil {
		var re *runError
		if !errors.As(fireErr, &re) {
			// Unclassified: roll the whole transaction back (the deferred
			// Rollback). The claim is released, and a later tick retries the
			// occurrence while it is still the most recent due one. The
			// reference's unknown_error is deliberately not recorded — it is
			// one of the fourteen pausing types, and emitting it would
			// auto-pause a healthy deployment on a transient database blip,
			// with no auto-resume shipped to undo it (§8.1 entry 28).
			return "", "", fireErr
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT fire`); err != nil {
			return "", "", err
		}
		if err := settleRun(ctx, tx,
			`UPDATE deployment_runs SET error_type = $1, error_message = $2 WHERE id = $3`,
			re.typ, re.err.Error(), runID); err != nil {
			return "", "", err
		}
		// Only a scheduled fire auto-pauses, and only the fourteen types the
		// paused-reason union can carry. Unconditional on the pause columns:
		// this transaction holds the row FOR SHARE, so no concurrent pause
		// or archive can have written them since the re-read above.
		if deploymentPausingErrorTypes[re.typ] {
			if _, err := tx.Exec(ctx, `
				UPDATE deployments
				   SET paused_at = now(), paused_kind = 'error', paused_error_type = $1, updated_at = now()
				 WHERE id = $2`, re.typ, f.deploymentID); err != nil {
				return "", "", err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", err
		}
		return fireOutcomeFailed, re.typ, nil
	}
	if err := settleRun(ctx, tx,
		`UPDATE deployment_runs SET session_id = $1, succeeded_at = now() WHERE id = $2`,
		created.row.id, runID); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	created.recordCreated(ctx)
	return fireOutcomeCreated, "", nil
}

// setDeploymentLockWait bounds every deployment-row lock wait in tx — the
// fire's own transaction, and archive/pause/unpause on the other side of the
// same contention. SET cannot be parameterized; the value is this package's.
func setDeploymentLockWait(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", deploymentLockWait.Milliseconds()))
	return err
}

// isLostRace reports the two shapes a lost claim takes at the claim phase —
// the FOR SHARE re-read and the claim insert, the two statements §4.1 names:
// 55P03 (lock_not_available — this transaction's lock_timeout gave up waiting
// on the winner) and 40P01 (deadlock_detected — Postgres chose this
// transaction as the victim and rolled it back). Both mean nothing was
// committed and the occurrence is retried or already owned.
func isLostRace(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40P01")
}

// isDeadlockVictim reports a 40P01 alone — the one lost-race shape that can
// surface past the claim phase. The deadlock is reachable, not theoretical: a
// loser blocked on the winner's uncommitted claim still holds its FOR SHARE
// on the deployment row, so a winner whose classified failure reaches the
// auto-pause UPDATE waits on that share lock — a cycle the deadlock detector
// resolves in about a second, choosing either side.
func isDeadlockVictim(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func recordDeploymentFire(ctx context.Context, outcome, errType string) {
	c, err := otel.GetMeterProvider().Meter(apiMeterName).Int64Counter(
		MetricDeploymentFires,
		metric.WithDescription("Deployment fire attempts, by outcome."))
	if err != nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("outcome", outcome)}
	if errType != "" {
		attrs = append(attrs, attribute.String("error.type", errType))
	}
	c.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func recordDeploymentOccurrencesSkipped(ctx context.Context, n int64) {
	if n == 0 {
		return
	}
	c, err := otel.GetMeterProvider().Meter(apiMeterName).Int64Counter(
		MetricDeploymentOccurrencesSkipped,
		metric.WithDescription("Occurrences a catch-up collapse passed over."))
	if err != nil {
		return
	}
	c.Add(ctx, n)
}

func recordDeploymentTickDuration(ctx context.Context, d time.Duration) {
	h, err := otel.GetMeterProvider().Meter(apiMeterName).Float64Histogram(
		MetricDeploymentTickDuration,
		metric.WithDescription("One scheduler sweep, end to end."),
		metric.WithUnit("s"))
	if err != nil {
		return
	}
	h.Record(ctx, d.Seconds())
}
