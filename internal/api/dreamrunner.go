package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/transcript"
)

// The dream runner (plan 41 §4.1): a controlplane background sweep beside the
// deployment scheduler, for the reason that one gives — the controlplane holds
// the pool, the blob store and the cipher a session create needs, and a dream
// is created by a request and runs later.
//
// A fire is one transaction; a dream is minutes to hours. Nothing here holds a
// claim across ticks, so the runner is stateless per tick and resumable from
// rows: one plain candidate scan, then one transaction per candidate that
// re-reads the row under FOR UPDATE SKIP LOCKED and takes the FIRST arm that
// matches, and only that one. A replica that crashes mid-arm rolls back to the
// last committed position and the next tick, on any replica, continues.
//
// The one arm with network I/O — the start — splits across three transactions
// (dreamstart.go), because no row lock may be held across it: a cancel landing
// during the render must find the row free.

const (
	// MetricDreamTransitions counts dream status transitions by the status
	// moved to. A closing pass moves no status and counts nothing.
	MetricDreamTransitions = "dream.transitions"
	// MetricDreamStageTurns records the model turns a running dream's current
	// stage has spent — every thread's counted (§3.3) — so the turn cap has a
	// distribution behind it rather than only a tripwire.
	MetricDreamStageTurns = "dream.stage_turns"
	// MetricDreamTickDuration times one whole sweep, the scheduler's twin.
	MetricDreamTickDuration = "dream.tick_duration"
)

// dreamStartLease is how long a claimed but unfinished start keeps its dream
// out of every replica's candidate scan (§4.2): the soft lease — one constant
// against updated_at, no owner column and no renewal, because the phase it
// covers is seconds and a crashed claimant costs one attempt and one lease,
// never a stuck dream. A var for the test setter (memoryretention.go:48-52's
// idiom), as the three below are.
var dreamStartLease = 5 * time.Minute

// dreamStartAttempts bounds the start claims one dream may burn. A dream has
// no successor occurrence to supersede it, so the retry is bounded here: the
// claim that exhausts this settles failed{internal_error} with the last
// error's text.
var dreamStartAttempts = 5

// dreamConcurrency bounds the arms one tick runs at once. Two is plenty for a
// job measured in hours, and a start arm is seconds rather than a fire's
// milliseconds.
var dreamConcurrency = 2

// dreamStageTurnCap is the model turns a stage may spend — every thread's
// counted — before the runner interrupts the session and fails the dream. One
// var while the pipeline is one stage (slice 2); slice 3 turns it into the
// four caps of §3.3.
var dreamStageTurnCap = 300

// dreamLockWait is the lock_timeout every dream-row transaction sets: the
// tick's arm, the start's write transaction, and the two lifecycle handlers on
// the other side of the same contention. A cancel arriving while a start's
// write holds the row waits at most this long and then fails the request,
// which the SDK retries against whatever the write left (§4.1) — a failed
// request rather than a hang.
var dreamLockWait = 2 * time.Second

// DreamRunnerConfig is the operator's three knobs (§4.7), read from the
// controlplane's environment.
type DreamRunnerConfig struct {
	// TickInterval paces the sweep, and is therefore a dream's start latency.
	TickInterval time.Duration
	// Timeout is a dream's runtime budget from creation, in pending as in
	// running → error.type "timeout".
	Timeout time.Duration
	// MaxInputBytes caps the input store's content → "input_memory_store_too_large".
	MaxInputBytes int64
}

// StartDreamRunner ticks until ctx ends. Safe on every replica at once: each
// arm re-reads its dream under FOR UPDATE SKIP LOCKED, so a second replica
// skips the row rather than duplicating the arm. blobs and cipher are the
// handler's own — the start arm puts the rendered transcripts and creates a
// session, which needs whatever POST /v1/sessions needs.
func StartDreamRunner(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, cipher secrets.Cipher, cfg DreamRunnerConfig) {
	s := newServer(pool, blobs, cipher)
	t := time.NewTicker(cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// The database's clock, as the scheduler reads it: the timeout and
		// the lease are compared against columns Postgres stamped, so a
		// replica's own clock must never enter the comparison.
		var now time.Time
		if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "dream tick skipped: reading the database clock failed", "error", err)
			}
			continue
		}
		if err := s.dreamTick(ctx, now, cfg); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "dream tick incomplete; the next interval retries", "error", err)
		}
	}
}

// sweepBudgets holds one connection budget per pool, shared by every
// controlplane sweep in the process (§4.1: "one semaphore for both sweeps").
// The scheduler's own MaxConns-2 clamp became an acquisition from this, so the
// reservation its comment protects — two connections for the rest of the
// process, one of them the SSE broker's LISTEN loop — holds with both sweeps
// running rather than with either alone. Keyed by the pool because a test
// binary runs several; a process runs one.
var sweepBudgets sync.Map // *pgxpool.Pool -> chan struct{}

// sweepBudget returns the pool's budget, creating it once. An arm costs
// exactly one connection at a time — inside a transaction every read uses that
// transaction's connection, and the start arm's render phase runs with no
// transaction open — so a slot is a connection.
func sweepBudget(pool *pgxpool.Pool) chan struct{} {
	if b, ok := sweepBudgets.Load(pool); ok {
		return b.(chan struct{})
	}
	b, _ := sweepBudgets.LoadOrStore(pool, make(chan struct{}, max(1, int(pool.Config().MaxConns)-2)))
	return b.(chan struct{})
}

// dreamRow is what step reads: the dream's own columns, and the pipeline
// session's status, archive mark and usage where one exists (§4.1).
type dreamRow struct {
	id, status       string
	stage, attempts  int
	createdAt        time.Time
	sessionID        *string
	inputStoreID     string
	inputSessionIDs  []string
	instructions     *string
	model            []byte
	outputs          []byte
	sessionFound     bool
	sessionStatus    string
	sessionArchived  *time.Time
	sessionUsage     []byte
	sessionCreatedAt time.Time
}

// dreamStepResult is what an arm leaves behind for the commit and after it.
type dreamStepResult struct {
	// to is the status the arm moved the dream to, empty when it moved none.
	// Counted after the commit, because a metric observes committed state.
	to string
	// after runs once the transaction has committed, still on the arm's
	// budget slot: the start arm's render-and-write, the closing arm's blob
	// deletes.
	after func(context.Context)
}

// dreamTick runs one sweep at the given instant: scan the candidates, then run
// each one's arm in its own transaction. A tick with no candidate exports no
// span — 2,880 empty root traces a day per replica would bury the ones an
// operator looks for — while the duration histogram records either way; a tick
// with candidates roots its own trace (there is no HTTP request, so no inbound
// traceparent) with a child span per dream. The scheduler's rule, including
// its edge: a candidate another replica has already claimed still gets a span.
func (s *server) dreamTick(ctx context.Context, now time.Time, cfg DreamRunnerConfig) error {
	start := time.Now()
	defer func() { recordDreamTickDuration(ctx, time.Since(start)) }()

	budget := sweepBudget(s.pool)
	ids, err := scanDreamCandidates(ctx, s.pool, budget, now)
	if err != nil || len(ids) == 0 {
		return err
	}

	ctx, span := otel.GetTracerProvider().Tracer(apiTracerName).Start(ctx, "dream.tick")
	defer span.End()
	sem := make(chan struct{}, dreamConcurrency)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		budget <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() { <-budget }()
			if err := s.runDreamArm(ctx, id, now, cfg); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("dream %s: %w", id, err))
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// scanDreamCandidates is the tick's plain, lockless scan (§4.1): every dream
// with something left to do, minus the ones under a live start claim. It holds
// a budget slot like an arm, because it is one query on one connection.
func scanDreamCandidates(ctx context.Context, pool *pgxpool.Pool, budget chan struct{}, now time.Time) ([]string, error) {
	select {
	case budget <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-budget }()

	rows, err := pool.Query(ctx, `
		SELECT id FROM dreams
		 WHERE closed_at IS NULL
		   AND NOT (status = 'pending' AND attempts > 0 AND updated_at > $1)
		 ORDER BY created_at`, now.Add(-dreamStartLease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// runDreamArm is one candidate's transaction: re-read the row under the scan's
// own predicate and FOR UPDATE SKIP LOCKED — so a candidate list taken before
// another replica's claim finds the row excluded rather than claiming it again
// — take one arm, commit, and only then record what the commit made true.
func (s *server) runDreamArm(ctx context.Context, id string, now time.Time, cfg DreamRunnerConfig) error {
	ctx, span := otel.GetTracerProvider().Tracer(apiTracerName).Start(ctx, "dream.step",
		trace.WithAttributes(attribute.String("dream.id", id)))
	defer span.End()

	res, err := s.dreamArmTx(ctx, id, now, cfg)
	if err != nil {
		return err
	}
	if res.to != "" {
		recordDreamTransition(ctx, res.to)
	}
	if res.after != nil {
		res.after(ctx)
	}
	return nil
}

func (s *server) dreamArmTx(ctx context.Context, id string, now time.Time, cfg DreamRunnerConfig) (dreamStepResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return dreamStepResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setDreamLockWait(ctx, tx); err != nil {
		return dreamStepResult{}, err
	}

	d, found, err := lockDream(ctx, tx, id, now)
	if err != nil || !found {
		// No row: another replica holds it, or claimed or closed it since the
		// scan. Not this replica's arm.
		return dreamStepResult{}, err
	}
	res, err := s.dreamStep(ctx, tx, d, now, cfg)
	if err != nil {
		return dreamStepResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return dreamStepResult{}, err
	}
	return res, nil
}

// lockDream re-reads the candidate under the scan's predicate, then the
// pipeline session's own state. The session read is deliberately the second
// statement and the status is read before any ask (§3.3): an ask's creation is
// committed together with the flip to idle, so a status read that returns idle
// is followed by an ask read that sees every ask there will be.
func lockDream(ctx context.Context, tx pgx.Tx, id string, now time.Time) (dreamRow, bool, error) {
	var d dreamRow
	err := tx.QueryRow(ctx, `
		SELECT id, status, stage, attempts, created_at, session_id, input_memory_store_id,
		       input_session_ids, instructions, model, outputs
		  FROM dreams
		 WHERE id = $1 AND closed_at IS NULL
		   AND NOT (status = 'pending' AND attempts > 0 AND updated_at > $2)
		 FOR UPDATE SKIP LOCKED`, id, now.Add(-dreamStartLease)).
		Scan(&d.id, &d.status, &d.stage, &d.attempts, &d.createdAt, &d.sessionID,
			&d.inputStoreID, &d.inputSessionIDs, &d.instructions, &d.model, &d.outputs)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	if d.sessionID == nil {
		return d, true, nil
	}
	err = tx.QueryRow(ctx,
		`SELECT status, archived_at, usage, created_at FROM sessions WHERE id = $1`, *d.sessionID).
		Scan(&d.sessionStatus, &d.sessionArchived, &d.sessionUsage, &d.sessionCreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unreachable through the API — the gate refuses a delete while the
		// dream owns the session, and the foreign key nulls session_id at the
		// database level — but a null-vs-missing distinction the arms would
		// otherwise have to carry twice.
		d.sessionID = nil
		return d, true, nil
	}
	if err != nil {
		return d, false, err
	}
	d.sessionFound = true
	return d, true, nil
}

// dreamStep is §4.1's decision table: the arms in order, the first that
// matches and only that one. Arm 9 ("stage k < 4 complete") is slice 3's — the
// pipeline is one stage here, so a session that idles has finished it, and arm
// 10 runs the end-of-stage checks.
func (s *server) dreamStep(ctx context.Context, tx pgx.Tx, d dreamRow, now time.Time, cfg DreamRunnerConfig) (dreamStepResult, error) {
	switch {
	case d.status != "pending" && d.status != "running": // 1. closing
		return s.dreamClosingArm(ctx, tx, d)
	case now.Sub(d.createdAt) > cfg.Timeout: // 2. timed out
		return s.dreamFail(ctx, tx, d, "timeout",
			fmt.Sprintf("dream exceeded its %s runtime budget", cfg.Timeout))
	case d.status == "pending": // 3. the start
		return s.dreamClaim(ctx, tx, d, cfg)
	}

	// Arms 4-10, all of them a running dream's.
	errType, msg, err := s.dreamUnavailable(ctx, tx, d) // 4. an input or the output gone
	if err != nil {
		return dreamStepResult{}, err
	}
	if errType != "" {
		return s.dreamFail(ctx, tx, d, errType, msg)
	}
	if d.sessionStatus == string(domain.SessionTerminated) { // 5. ended badly
		last, err := lastSessionError(ctx, tx, *d.sessionID)
		if err != nil {
			return dreamStepResult{}, err
		}
		return s.dreamFail(ctx, tx, d, "internal_error", "pipeline session terminated: "+last)
	}
	turns, err := dreamStageTurns(ctx, tx, *d.sessionID)
	if err != nil {
		return dreamStepResult{}, err
	}
	recordDreamStageTurns(ctx, d.stage, turns)
	if turns > dreamStageTurnCap { // 6. over budget
		return s.dreamFail(ctx, tx, d, "internal_error",
			fmt.Sprintf("stage %d exceeded its budget of %d model turns", d.stage, dreamStageTurnCap))
	}
	if d.sessionStatus == string(domain.SessionRunning) || // 7. busy
		d.sessionStatus == string(domain.SessionRescheduling) {
		return dreamStepResult{}, mirrorDreamUsage(ctx, tx, d)
	}
	asks, err := events.UnconfirmedAskEvents(ctx, tx, domain.ID(*d.sessionID), nil)
	if err != nil {
		return dreamStepResult{}, err
	}
	if len(asks) > 0 { // 8. idle with an open ask
		// Unreachable under the internal agent's always_allow policy, so it
		// is a bug rather than a human's to answer (§3.3) — and §4.4 refuses
		// every send that could answer it.
		return s.dreamFail(ctx, tx, d, "internal_error", "pipeline session asked for confirmation")
	}
	return s.dreamCompleteArm(ctx, tx, d) // 10. idle, the stage complete
}

// dreamClosingArm is arm 1: the terminal dream's session is wound down, its
// transcript rows and objects go, and closed_at is stamped. A session still
// running is interrupted again (idempotent) and the close waits for the next
// tick — a turn ends, so the wait is bounded by the turn.
func (s *server) dreamClosingArm(ctx context.Context, tx pgx.Tx, d dreamRow) (dreamStepResult, error) {
	if d.sessionFound {
		if d.sessionStatus == string(domain.SessionRunning) {
			// interruptSessionInTx records no post-commit session-status
			// metric — that observation belongs to whoever commits, and this
			// arm's own metric is the dream transition, which has not moved.
			if err := s.interruptSessionInTx(ctx, tx, *d.sessionID); err != nil {
				return dreamStepResult{}, err
			}
			return dreamStepResult{}, mirrorDreamUsage(ctx, tx, d)
		}
		if d.sessionArchived == nil {
			// Every other status the set requireNotRunning admits — idle,
			// terminated and rescheduling alike — is archivable.
			if _, err := s.archiveSessionInTx(ctx, tx, *d.sessionID); err != nil {
				return dreamStepResult{}, err
			}
		}
	}
	keys, err := deleteDreamFileRows(ctx, tx, d.id)
	if err != nil {
		return dreamStepResult{}, err
	}
	usage, err := dreamUsageOf(d)
	if err != nil {
		return dreamStepResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE dreams SET closed_at = now(), usage = COALESCE($2, usage), updated_at = now()
		  WHERE id = $1`, d.id, usage); err != nil {
		return dreamStepResult{}, err
	}
	// The objects follow the rows after the commit, best-effort, the order
	// deleteFile gives the two (rare orphans accepted, GC a non-goal).
	return dreamStepResult{after: func(ctx context.Context) { s.deleteDreamBlobs(ctx, keys) }}, nil
}

// dreamCompleteArm is arm 10: the end-of-stage checks of §3.3, then completed.
// The output store's liveness is arm 4's, taken in this same transaction, so
// what is left here is the secret scan — the cheap half of the secret defence.
func (s *server) dreamCompleteArm(ctx context.Context, tx pgx.Tx, d dreamRow) (dreamStepResult, error) {
	storeID, err := dreamOutputStoreID(d.outputs)
	if err != nil {
		return dreamStepResult{}, err
	}
	// The time bound keeps the clone out of the scan: the start arm attributed
	// every cloned version to this same actor and stamped them and the session
	// row with one now(), so the caller's pre-existing content cannot fail a
	// dream that wrote nothing wrong.
	memoryID, err := dreamSecretInVersions(ctx, tx, storeID, *d.sessionID, d.sessionCreatedAt)
	if err != nil {
		return dreamStepResult{}, err
	}
	if memoryID != "" {
		return s.dreamFail(ctx, tx, d, "internal_error",
			"memory "+memoryID+" matches a credential shape the renderer redacts")
	}
	return s.dreamSettle(ctx, tx, d, "completed", nil)
}

// dreamClaim is arm 3's first phase: count the attempt, release the row, and
// let the start run its rendering with no lock held (§4.2).
func (s *server) dreamClaim(ctx context.Context, tx pgx.Tx, d dreamRow, cfg DreamRunnerConfig) (dreamStepResult, error) {
	if _, err := tx.Exec(ctx,
		`UPDATE dreams SET attempts = attempts + 1, updated_at = now() WHERE id = $1`, d.id); err != nil {
		return dreamStepResult{}, err
	}
	d.attempts++
	return dreamStepResult{after: func(ctx context.Context) { s.startDream(ctx, d, cfg) }}, nil
}

// dreamFail settles a failed dream and interrupts its session where one is
// still live; arm 1 archives and closes on a later tick. A dream with no
// session has nothing to wind down, so it closes in the same commit (§4.6).
func (s *server) dreamFail(ctx context.Context, tx pgx.Tx, d dreamRow, errType, msg string) (dreamStepResult, error) {
	if d.sessionFound && d.sessionArchived == nil && d.sessionStatus != string(domain.SessionTerminated) {
		if err := s.interruptSessionInTx(ctx, tx, *d.sessionID); err != nil {
			return dreamStepResult{}, err
		}
	}
	return s.dreamSettle(ctx, tx, d, "failed", &dreamErrorJSON{Type: errType, Message: msg})
}

// dreamSettle writes one terminal state, mirrors the session's usage a last
// time, and closes the dream outright when it has no session to wind down —
// one it never had, or one deleted at the database level. The close takes
// the transcript rows with it (§4.6): their ownership is the dream's, and the
// closing arm, which would otherwise delete them, never runs on a dream this
// commit closes.
func (s *server) dreamSettle(ctx context.Context, tx pgx.Tx, d dreamRow, status string, failure *dreamErrorJSON) (dreamStepResult, error) {
	var errJSON []byte
	if failure != nil {
		errJSON = mustJSON(failure)
	}
	usage, err := dreamUsageOf(d)
	if err != nil {
		return dreamStepResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE dreams
		   SET status = $2, ended_at = now(), error = $3, usage = COALESCE($4, usage),
		       closed_at = CASE WHEN $5 THEN now() ELSE closed_at END, updated_at = now()
		 WHERE id = $1`, d.id, status, errJSON, usage, !d.sessionFound); err != nil {
		return dreamStepResult{}, err
	}
	res := dreamStepResult{to: status}
	if !d.sessionFound {
		keys, err := deleteDreamFileRows(ctx, tx, d.id)
		if err != nil {
			return dreamStepResult{}, err
		}
		if len(keys) > 0 {
			res.after = func(ctx context.Context) { s.deleteDreamBlobs(ctx, keys) }
		}
	}
	return res, nil
}

// dreamUnavailable is arm 4's four reads, in §4.1's order. The executor
// tolerates a missing or archived memory store — it skips or goes pull-only —
// so a session whose store vanished would run to the end writing nowhere; this
// arm is what notices.
func (s *server) dreamUnavailable(ctx context.Context, tx pgx.Tx, d dreamRow) (errType, msg string, err error) {
	live, err := memoryStoreLive(ctx, tx, d.inputStoreID)
	if err != nil {
		return "", "", err
	}
	if !live {
		return "input_memory_store_unavailable",
			"input memory store " + d.inputStoreID + " is missing or archived", nil
	}
	missing, err := missingSessions(ctx, tx, d.inputSessionIDs)
	if err != nil {
		return "", "", err
	}
	if missing != "" {
		return "input_session_unavailable", "input session " + missing + " is missing", nil
	}
	storeID, err := dreamOutputStoreID(d.outputs)
	if err != nil {
		return "", "", err
	}
	if storeID != "" {
		if live, err := memoryStoreLive(ctx, tx, storeID); err != nil {
			return "", "", err
		} else if !live {
			return "internal_error", "output memory store " + storeID + " is missing or archived", nil
		}
	}
	if d.sessionID == nil {
		return "internal_error", "the pipeline session was deleted", nil
	}
	return "", "", nil
}

func memoryStoreLive(ctx context.Context, db querier, id string) (bool, error) {
	var archivedAt *time.Time
	err := db.QueryRow(ctx, `SELECT archived_at FROM memory_stores WHERE id = $1`, id).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return archivedAt == nil, nil
}

// missingSessions names the first input session that is gone, in the dream's
// own order so the message points at a stable one.
func missingSessions(ctx context.Context, db querier, ids []string) (string, error) {
	rows, err := db.Query(ctx, `SELECT id FROM sessions WHERE id = ANY($1)`, ids)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	found := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, id := range ids {
		if !found[id] {
			return id, nil
		}
	}
	return "", nil
}

// dreamStageTurns counts the stage's model turns: the span.model_request_end
// events — one per settled turn on every thread, a child's carrying its
// thread_id — after the stage's opening user.message, which is the latest one
// on the primary thread, the runner being its only author while the dream is
// open (§4.4). No column tracks it.
func dreamStageTurns(ctx context.Context, db querier, sessionID string) (int, error) {
	var openedAt int64
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(MAX(seq), 0) FROM events
		 WHERE session_id = $1 AND type = $2 AND thread_id IS NULL`,
		sessionID, string(domain.EventUserMessage)).Scan(&openedAt); err != nil {
		return 0, err
	}
	var turns int
	err := db.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE session_id = $1 AND type = $2 AND seq > $3`,
		sessionID, string(domain.EventSpanModelRequestEnd), openedAt).Scan(&turns)
	return turns, err
}

// lastSessionError renders the message a terminated session last recorded, for
// the failed dream's error text. A session that terminated without one — the
// shape no emitter writes today — reads as "no error was recorded".
func lastSessionError(ctx context.Context, db querier, sessionID string) (string, error) {
	var msg *string
	err := db.QueryRow(ctx, `
		SELECT payload->'error'->>'message' FROM events
		 WHERE session_id = $1 AND type = $2 ORDER BY seq DESC LIMIT 1`,
		sessionID, string(domain.EventSessionError)).Scan(&msg)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && msg == nil) {
		return "no error was recorded", nil
	}
	if err != nil {
		return "", err
	}
	return *msg, nil
}

// dreamSecretInVersions returns the memory named by the first version this
// session wrote whose content still matches a redaction pattern, or "". It is
// the cheap half of the secret defence (§3.3): the pipeline reads redacted
// text, and this reads back what it wrote.
func dreamSecretInVersions(ctx context.Context, db querier, storeID, sessionID string, after time.Time) (string, error) {
	rows, err := db.Query(ctx, `
		SELECT memory_id, content FROM memory_versions
		 WHERE memory_store_id = $1 AND content IS NOT NULL AND created_at > $3
		   AND created_by->>'type' = 'session_actor' AND created_by->>'session_id' = $2
		 ORDER BY created_at, id`, storeID, sessionID, after)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var memoryID, content string
		if err := rows.Scan(&memoryID, &content); err != nil {
			return "", err
		}
		if transcript.Redact(content) != content {
			return memoryID, nil
		}
	}
	return "", rows.Err()
}

// dreamOutputStoreID reads the output store from outputs[], which the start
// arm wrote in the same commit as `running`. Empty before that commit.
func dreamOutputStoreID(outputs []byte) (string, error) {
	var out []struct {
		MemoryStoreID string `json:"memory_store_id"`
	}
	if err := json.Unmarshal(outputs, &out); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", nil
	}
	return out[0].MemoryStoreID, nil
}

// dreamUsageOf converts the session's usage column to the dream's flat four
// (cache_creation_input_tokens is the sum of the TTL tiers), or nil when there
// is no session to mirror — the COALESCE at every call site then leaves the
// stored value alone.
func dreamUsageOf(d dreamRow) ([]byte, error) {
	if !d.sessionFound {
		return nil, nil
	}
	var u domain.Usage
	if err := json.Unmarshal(d.sessionUsage, &u); err != nil {
		return nil, err
	}
	return mustJSON(dreamUsageJSON{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreation.Ephemeral1h + u.CacheCreation.Ephemeral5m,
	}), nil
}

func mirrorDreamUsage(ctx context.Context, tx pgx.Tx, d dreamRow) error {
	usage, err := dreamUsageOf(d)
	if err != nil || usage == nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE dreams SET usage = $2, updated_at = now() WHERE id = $1`, d.id, usage)
	return err
}

// deleteDreamFileRows removes the dream's transcript rows and returns the
// object keys the caller deletes after the commit (§4.5). Their ownership is
// the dream's, not the session's, so they go whether or not a session remains.
func deleteDreamFileRows(ctx context.Context, tx pgx.Tx, dreamID string) ([]string, error) {
	rows, err := tx.Query(ctx, `DELETE FROM files WHERE dream_id = $1 RETURNING id`, dreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		keys = append(keys, blob.FilesKey(id))
	}
	return keys, rows.Err()
}

func (s *server) deleteDreamBlobs(ctx context.Context, keys []string) {
	for _, k := range keys {
		s.deleteOrphanedFile(ctx, k)
	}
}

// setDreamLockWait bounds every dream-row lock wait in tx. SET cannot be
// parameterized; the value is this package's (setDeploymentLockWait's rule).
func setDreamLockWait(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", dreamLockWait.Milliseconds()))
	return err
}

// errDreamRunnerDisabled answers POST /v1/dreams where DREAM_TICK_INTERVAL is
// "0" (§4.7): with no runner, a created dream would never reach a terminal
// state — the timeout is the runner's too — so the create refuses rather than
// accept work nothing will do. The other four routes keep answering, so
// existing dreams stay readable, cancelable and archivable.
var errDreamRunnerDisabled = &apiError{http.StatusInternalServerError, errTypeAPI,
	"the dream runner is disabled on this deployment"}

func recordDreamTransition(ctx context.Context, to string) {
	c, err := otel.GetMeterProvider().Meter(apiMeterName).Int64Counter(
		MetricDreamTransitions,
		metric.WithDescription("Dream status transitions, by the status moved to."))
	if err != nil {
		return
	}
	c.Add(ctx, 1, metric.WithAttributes(attribute.String("to", to)))
}

func recordDreamStageTurns(ctx context.Context, stage, turns int) {
	h, err := otel.GetMeterProvider().Meter(apiMeterName).Int64Histogram(
		MetricDreamStageTurns,
		metric.WithDescription("Model turns spent in a running dream's current stage."))
	if err != nil {
		return
	}
	h.Record(ctx, int64(turns), metric.WithAttributes(attribute.Int("stage", stage)))
}

func recordDreamTickDuration(ctx context.Context, d time.Duration) {
	h, err := otel.GetMeterProvider().Meter(apiMeterName).Float64Histogram(
		MetricDreamTickDuration,
		metric.WithDescription("One dream sweep, end to end."),
		metric.WithUnit("s"))
	if err != nil {
		return
	}
	h.Record(ctx, d.Seconds())
}
