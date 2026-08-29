package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/cron"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// upcomingRunsCount is the reference's "up to 5 timestamps of upcoming cron
// occurrences". Five whenever five exist inside internal/cron's search bound,
// which is what makes the published "non-empty for active and paused
// deployments" hold even for the sparsest satisfiable expression.
const upcomingRunsCount = 5

// deploymentInitialEventsNote names the three types a deployment admits — one
// more than a session's, because a fired session has no client standing by to
// send a system.message afterwards.
const deploymentInitialEventsNote = "initial_events supports user.message, user.define_outcome and system.message events only"

// The documented create-time bounds no column enforces.
const (
	deploymentNameMax        = 256
	deploymentDescriptionMax = 2048
	deploymentEnvironmentMax = 128
	deploymentVaultIDsMax    = 50
	deploymentResourcesMax   = 500
)

// deploymentCreateKeys and deploymentUpdateKeys are the allow-lists. `budget`
// is on neither: this platform has no session budgets, so a request carrying
// one is a 400 rather than an unenforced ceiling an operator would believe in
// (plan 37 §8.1 entry 2, #432).
var (
	deploymentCreateKeys = []string{"name", "description", "agent", "environment_id",
		"vault_ids", "initial_events", "resources", "metadata", "schedule"}
	deploymentUpdateKeys = deploymentCreateKeys
)

// deploymentColumns is the select list every read path shares, in the order
// scanDeployment reads it.
const deploymentColumns = `id, name, description, agent_id, agent_version, environment_id,
	vault_ids, initial_events, resources, metadata, schedule_expression, schedule_timezone,
	paused_at, paused_kind, paused_error_type, created_at, updated_at, archived_at`

// scanDeployment reads one row into the domain type. The schedule's two
// computed members are left unset; fillScheduleTimestamps needs a query of its
// own and runs once per page rather than once per row.
func scanDeployment(row pgx.Row) (domain.Deployment, error) {
	var (
		d                         domain.Deployment
		description               string
		agentID                   string
		agentVersion              int
		vaultIDs                  []string
		initial, resources, meta  []byte
		expr, tz                  *string
		pausedKind, pausedErrType *string
	)
	if err := row.Scan(&d.ID, &d.Name, &description, &agentID, &agentVersion, &d.EnvironmentID,
		&vaultIDs, &initial, &resources, &meta, &expr, &tz,
		&d.PausedAt, &pausedKind, &pausedErrType, &d.CreatedAt, &d.UpdatedAt, &d.ArchivedAt); err != nil {
		return d, err
	}

	// The column is NOT NULL with an empty-string default while the wire field
	// is nullable, and null is how the reference spells unset. A description
	// explicitly set to "" therefore reads back as null — the one asymmetry
	// the two representations cannot both hold (docs/DIVERGENCES.md).
	if description != "" {
		d.Description = &description
	}
	d.Agent = domain.AgentReference{Type: "agent", ID: domain.ID(agentID), Version: agentVersion}
	d.VaultIDs = deploymentVaultIDs(vaultIDs)
	if err := json.Unmarshal(initial, &d.InitialEvents); err != nil {
		return d, err
	}
	var stored []deploymentResource
	if err := json.Unmarshal(resources, &stored); err != nil {
		return d, err
	}
	d.Resources = echoDeploymentResources(stored)
	if err := json.Unmarshal(meta, &d.Metadata); err != nil {
		return d, err
	}
	if pausedKind != nil {
		d.PausedKind = domain.PausedKind(*pausedKind)
	}
	if pausedErrType != nil {
		d.PausedErrorType = *pausedErrType
	}
	if expr != nil && tz != nil {
		d.Schedule = &domain.Schedule{Type: domain.ScheduleCron, Expression: *expr, Timezone: *tz}
	}
	return d, nil
}

// fillScheduleTimestamps computes the schedule's two derived members for a
// whole page in one query rather than one per row.
//
// upcoming_runs_at is [] once archived_at is set — "empty once the deployment
// is archived" — while a paused deployment keeps its list, which "reflects
// what the schedule would do if unpaused".
//
// last_run_at is the created_at of the run with the greatest scheduled_at. The
// field is "the most recent scheduled run actually started" and the row is
// inserted as the fire begins, so created_at is the instant it names; ordering
// by scheduled_at rather than taking MAX(created_at) reports the most recent
// *occurrence*, which is what the sentence says, and the two disagree whenever
// a fire queues past the next occurrence. Three published rules fall out of
// the one expression rather than needing three branches: manual runs do not
// update the field (the trigger filter), it survives archiving (archive
// touches no run row), and it is null until one completes (no rows, no
// maximum).
//
// The `scheduled_at IS NOT NULL` conjunct is redundant against trigger_type
// and load-bearing anyway: the occurrence index is partial on exactly that
// predicate, and Postgres will not use a partial index unless the query's own
// WHERE implies it. Without the conjunct spelled out the planner refuses the
// index even with enable_seqscan off.
func fillScheduleTimestamps(ctx context.Context, db querier, ds []*domain.Deployment, now time.Time) error {
	var ids []string
	for _, d := range ds {
		if d.Schedule == nil {
			continue
		}
		ids = append(ids, string(d.ID))
		if d.ArchivedAt != nil {
			d.Schedule.UpcomingRunsAt = []time.Time{}
			continue
		}
		next, err := cron.Upcoming(d.Schedule.Expression, d.Schedule.Timezone, now, upcomingRunsCount)
		if err != nil {
			return err
		}
		d.Schedule.UpcomingRunsAt = next
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := db.Query(ctx,
		`SELECT DISTINCT ON (deployment_id) deployment_id, created_at
		   FROM deployment_runs
		  WHERE deployment_id = ANY($1)
		    AND trigger_type = 'schedule'
		    AND session_id IS NOT NULL
		    AND scheduled_at IS NOT NULL
		  ORDER BY deployment_id, scheduled_at DESC`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	last := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return err
		}
		last[id] = at
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range ds {
		if d.Schedule == nil {
			continue
		}
		if at, ok := last[string(d.ID)]; ok {
			d.Schedule.LastRunAt = &at
		}
	}
	return nil
}

// finishDeployment is the single-row form of fillScheduleTimestamps.
func finishDeployment(ctx context.Context, db querier, d domain.Deployment) (domain.Deployment, error) {
	err := fillScheduleTimestamps(ctx, db, []*domain.Deployment{&d}, time.Now().UTC())
	return d, err
}

// loadDeployment reads one row for a mutating action, locking it FOR UPDATE so
// two concurrent pauses cannot interleave, and refuses an archived one.
// Archive is terminal for every mutating action — update, pause, unpause and
// run all answer 400 — while GET and the list still return the row.
func loadDeployment(ctx context.Context, tx pgx.Tx, id string) (domain.Deployment, error) {
	d, err := scanDeployment(tx.QueryRow(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errNotFound("deployment %s not found", id)
	}
	if err != nil {
		return d, err
	}
	if d.ArchivedAt != nil {
		return d, errInvalid("deployment %s is archived", id)
	}
	return d, nil
}

func (s *server) createDeployment(r *http.Request) (any, error) {
	ctx := r.Context()
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, deploymentCreateKeys...); err != nil {
		return nil, err
	}

	name, err := requiredString(obj, "name")
	if err != nil {
		return nil, err
	}
	description, _, _, err := nullableString(obj, "description")
	if err != nil {
		return nil, err
	}
	envID, err := requiredString(obj, "environment_id")
	if err != nil {
		return nil, err
	}
	agentRaw, ok := obj["agent"]
	if !ok || isNull(agentRaw) {
		return nil, errInvalid("agent is required")
	}
	initial, err := parseInitialEvents(obj, true)
	if err != nil {
		return nil, err
	}
	vaultIDs, err := parseVaultIDs(obj)
	if err != nil {
		return nil, err
	}
	metadata, err := parseMetadata(obj)
	if err != nil {
		return nil, err
	}
	if err := validateMetadataCaps(metadata); err != nil {
		return nil, err
	}
	resourceInputs, err := parseResourceInputs(obj)
	if err != nil {
		return nil, err
	}
	if err := validateDeploymentBounds(name, description, envID, vaultIDs, len(initial), len(resourceInputs)); err != nil {
		return nil, err
	}
	expr, tz, _, _, err := parseDeploymentSchedule(obj)
	if err != nil {
		return nil, err
	}

	sealed, err := s.sealDeploymentRepoTokens(ctx, resourceInputs)
	if err != nil {
		return nil, err
	}

	id := domain.NewID(domain.PrefixDeployment).String()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := requireLiveEnvironment(ctx, tx, envID); err != nil {
		return nil, err
	}
	if err := validateAttachedVaults(ctx, tx, vaultIDs); err != nil {
		return nil, err
	}
	agent, err := resolveDeploymentAgent(ctx, tx, agentRaw)
	if err != nil {
		return nil, err
	}
	if err := s.validateDeploymentInitialEvents(initial); err != nil {
		return nil, err
	}

	stored := deploymentResourcesFrom(resourceInputs, sealed)
	d, err := scanDeployment(tx.QueryRow(ctx,
		`INSERT INTO deployments (id, name, description, agent_id, agent_version, environment_id,
			vault_ids, initial_events, resources, metadata, schedule_expression, schedule_timezone, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		 RETURNING `+deploymentColumns,
		id, name, derefOr(description, ""), string(agent.ID), agent.Version, envID,
		vaultIDs, mustJSON(initial), mustJSON(stored), metadata,
		nullableText(expr), nullableText(tz), principalPtr(ctx)))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return finishDeployment(ctx, s.pool, d)
}

func (s *server) getDeployment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "deployment"); err != nil {
		return nil, err
	}
	d, err := scanDeployment(s.pool.QueryRow(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("deployment %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return finishDeployment(ctx, s.pool, d)
}

func (s *server) listDeployments(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	page, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	includeArchived, err := parseBoolParam(q, "include_archived")
	if err != nil {
		return nil, err
	}
	status := q.Get("status")
	// The two filters are mutually exclusive on the wire: an archived
	// deployment reports status "active", so combining them would ask for a
	// set whose membership rule contradicts itself.
	if status != "" && includeArchived {
		return nil, errInvalid("status cannot be combined with include_archived")
	}
	if status != "" && status != string(domain.DeploymentActive) && status != string(domain.DeploymentPaused) {
		return nil, errInvalid("status must be %q or %q", domain.DeploymentActive, domain.DeploymentPaused)
	}
	gte, err := parseTimeParam(q, "created_at[gte]")
	if err != nil {
		return nil, err
	}
	lte, err := parseTimeParam(q, "created_at[lte]")
	if err != nil {
		return nil, err
	}

	query := `SELECT ` + deploymentColumns + ` FROM deployments WHERE true`
	var args []any
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	switch status {
	case string(domain.DeploymentPaused):
		query += ` AND paused_at IS NOT NULL`
	case string(domain.DeploymentActive):
		query += ` AND paused_at IS NULL`
	}
	if agentID := q.Get("agent_id"); agentID != "" {
		// "Filter by agent ID." Shape first, as the sessions list does: a
		// malformed agent_id can never name a stored agent, and rejecting it
		// here keeps an unstorable byte from reaching the bind parameter as a
		// 500 (#135). A well-formed but absent one filters to an empty page.
		if !domain.ID(agentID).Valid() {
			return nil, errInvalid("agent_id must be a valid agent id")
		}
		args = append(args, agentID)
		query += fmt.Sprintf(` AND agent_id = $%d`, len(args))
	}
	if gte != nil {
		args = append(args, *gte)
		query += fmt.Sprintf(` AND created_at >= $%d`, len(args))
	}
	if lte != nil {
		args = append(args, *lte)
		query += fmt.Sprintf(` AND created_at <= $%d`, len(args))
	}
	if page.cur != nil {
		if page.cur.versioned || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
		args = append(args, page.cur.t, page.cur.id)
		query += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, page.limit+1)
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ds []*domain.Deployment
	var lastT time.Time
	var lastID string
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		ds = append(ds, &d)
		lastT, lastID = d.CreatedAt, string(d.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := fillScheduleTimestamps(ctx, s.pool, ds, time.Now().UTC()); err != nil {
		return nil, err
	}

	out := pageJSON{Data: []any{}}
	for _, d := range ds {
		out.Data = append(out.Data, *d)
	}
	if fetched > page.limit {
		c := encodeTimeCursor(dirNext, lastT, lastID)
		out.NextPage = &c
	}
	return out, nil
}

func (s *server) updateDeployment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "deployment"); err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, deploymentUpdateKeys...); err != nil {
		return nil, err
	}

	// Everything that can be judged without the row, judged before the
	// transaction opens — and the repo tokens sealed there too, because the
	// cipher's network round trip must not run under the row locks.
	initial, err := parseInitialEvents(obj, true)
	if err != nil {
		return nil, err
	}
	resourceInputs, err := parseResourceInputs(obj)
	if err != nil {
		return nil, err
	}
	// Key presence, not present(): "send empty array or null to clear".
	// parseResourceInputs reads a null as no inputs, so the clear falls out.
	_, resourcesSet := obj["resources"]
	var sealed []sealedToken
	if resourcesSet {
		if sealed, err = s.sealDeploymentRepoTokens(ctx, resourceInputs); err != nil {
			return nil, err
		}
	}
	expr, tz, scheduleSet, scheduleNull, err := parseDeploymentSchedule(obj)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	d, err := loadDeployment(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	// "Omit a field to preserve its current value." vault_ids,
	// initial_events, resources and schedule are full replacements; metadata
	// alone is a patch.
	name := d.Name
	if v, set, _, err := stringField(obj, "name"); err != nil {
		return nil, err
	} else if set {
		name = v
	}
	description := d.Description
	if v, set, null, err := nullableString(obj, "description"); err != nil {
		return nil, err
	} else if set {
		description = nil
		if !null {
			description = v
		}
	}
	envID := string(d.EnvironmentID)
	if v, set, null, err := stringField(obj, "environment_id"); err != nil {
		return nil, err
	} else if set {
		// "Cannot be cleared" — answered here rather than left to collapse
		// onto the empty string, which would look up an environment named ""
		// and report a 404 for a request that is malformed, not missing.
		if null {
			return nil, errInvalid("environment_id cannot be cleared")
		}
		envID = v
		if err := requireLiveEnvironment(ctx, tx, envID); err != nil {
			return nil, err
		}
	}
	vaultIDs := make([]string, 0, len(d.VaultIDs))
	for _, v := range d.VaultIDs {
		vaultIDs = append(vaultIDs, string(v))
	}
	// "Omit to preserve; send empty array or null to clear" — so the test is
	// key presence, not present(): an explicit null clears rather than
	// preserves, and parseVaultIDs already reads it as the empty list.
	if _, ok := obj["vault_ids"]; ok {
		if vaultIDs, err = parseVaultIDs(obj); err != nil {
			return nil, err
		}
		if err := validateAttachedVaults(ctx, tx, vaultIDs); err != nil {
			return nil, err
		}
	}
	agent := d.Agent
	if raw, ok := obj["agent"]; ok {
		// "Omit to preserve. Cannot be cleared."
		if isNull(raw) {
			return nil, errInvalid("agent cannot be cleared")
		}
		if agent, err = resolveDeploymentAgent(ctx, tx, raw); err != nil {
			return nil, err
		}
	}
	storedInitial := d.InitialEvents
	if raw, ok := obj["initial_events"]; ok {
		// "Omit to preserve. Cannot be cleared." — the one collection of the
		// three that null does not clear, so null is refused rather than
		// silently preserving, the way agent's is.
		if isNull(raw) {
			return nil, errInvalid("initial_events cannot be cleared")
		}
		if err := s.validateDeploymentInitialEvents(initial); err != nil {
			return nil, err
		}
		storedInitial = initial
	}
	resourcesJSON := mustJSON(deploymentResourcesFrom(resourceInputs, sealed))
	if !resourcesSet {
		// Preserve: re-read the column rather than round-tripping the echo,
		// which has had the sealed tokens stripped out of it.
		if err := tx.QueryRow(ctx, `SELECT resources FROM deployments WHERE id = $1`, id).
			Scan(&resourcesJSON); err != nil {
			return nil, err
		}
	}
	metadata := d.Metadata
	if raw, ok := obj["metadata"]; ok {
		if metadata, err = patchMetadata(metadata, raw, false); err != nil {
			return nil, err
		}
		if err := validateMetadataCaps(metadata); err != nil {
			return nil, err
		}
	}
	newExpr, newTZ := "", ""
	if d.Schedule != nil {
		newExpr, newTZ = d.Schedule.Expression, d.Schedule.Timezone
	}
	if scheduleSet {
		newExpr, newTZ = expr, tz
		if scheduleNull {
			newExpr, newTZ = "", ""
		}
	}
	if err := validateDeploymentBounds(name, description, envID, vaultIDs, len(storedInitial), len(resourceInputs)); err != nil {
		return nil, err
	}

	updated, err := scanDeployment(tx.QueryRow(ctx,
		`UPDATE deployments SET
		   name = $2, description = $3, agent_id = $4, agent_version = $5, environment_id = $6,
		   vault_ids = $7, initial_events = $8, resources = $9, metadata = $10,
		   schedule_expression = $11, schedule_timezone = $12, updated_at = now()
		 WHERE id = $1
		 RETURNING `+deploymentColumns,
		id, name, derefOr(description, ""), string(agent.ID), agent.Version, envID,
		vaultIDs, mustJSON(storedInitial), resourcesJSON, metadata,
		nullableText(newExpr), nullableText(newTZ)))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return finishDeployment(ctx, s.pool, updated)
}

// archiveDeployment is idempotent and one-way: the first call stamps
// archived_at, later calls change nothing. It leaves the pause columns exactly
// as it found them — an archived deployment reports "active" whatever they
// say, and nothing unarchives, so nothing can observe them again.
func (s *server) archiveDeployment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "deployment"); err != nil {
		return nil, err
	}
	d, err := scanDeployment(s.pool.QueryRow(ctx,
		`UPDATE deployments SET
		   updated_at  = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END,
		   archived_at = COALESCE(archived_at, now())
		 WHERE id = $1
		 RETURNING `+deploymentColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("deployment %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return finishDeployment(ctx, s.pool, d)
}

// pauseDeployment records a manual pause. Idempotent in the sense that matters:
// pausing an already-paused deployment leaves the existing reason alone, so a
// manual pause never overwrites the error that auto-paused it — the operator
// would lose the only rendering of why it stopped.
func (s *server) pauseDeployment(r *http.Request) (any, error) {
	return s.setDeploymentPause(r, true)
}

// unpauseDeployment clears the pause and stamps schedule_resumed_at, which is
// what makes the published "unpause resumes the schedule from the next
// scheduled occurrence; missed triggers are not backfilled" implementable: no
// run row advances the derived watermark while a deployment is paused, so
// without the stamp the first tick after an unpause would fire the occurrence
// that fell due during it.
func (s *server) unpauseDeployment(r *http.Request) (any, error) {
	return s.setDeploymentPause(r, false)
}

func (s *server) setDeploymentPause(r *http.Request, pause bool) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "deployment"); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := loadDeployment(ctx, tx, id); err != nil {
		return nil, err
	}

	stmt := `UPDATE deployments SET
	           paused_at = NULL, paused_kind = NULL, paused_error_type = NULL,
	           schedule_resumed_at = now(), updated_at = now()
	         WHERE id = $1 RETURNING ` + deploymentColumns
	if pause {
		stmt = `UPDATE deployments SET
		          paused_at   = COALESCE(paused_at, now()),
		          paused_kind = COALESCE(paused_kind, 'manual'),
		          updated_at  = now()
		        WHERE id = $1 RETURNING ` + deploymentColumns
	}
	d, err := scanDeployment(tx.QueryRow(ctx, stmt, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return finishDeployment(ctx, s.pool, d)
}

// sealDeploymentRepoTokens runs the repository tokens through the cipher before
// any transaction opens — the cipher's network round trip must not run under
// the row locks — and refuses a repository outright on a cipher-less
// deployment, since a token is never stored unencrypted.
func (s *server) sealDeploymentRepoTokens(ctx context.Context, inputs []resourceInput) ([]sealedToken, error) {
	if !resourceInputsHaveRepo(inputs) {
		return nil, nil
	}
	if s.cipher == nil {
		return nil, errRepoSecretsUnavailable
	}
	return sealRepoTokens(ctx, s.cipher, inputs)
}

// requireLiveEnvironment holds the environment row FOR SHARE so a concurrent
// delete or archive cannot slip between the check and the write, the discipline
// createSession applies to the same row.
func requireLiveEnvironment(ctx context.Context, tx pgx.Tx, envID string) error {
	var archivedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT archived_at FROM environments WHERE id = $1 FOR SHARE`, envID).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("environment %s not found", envID)
	}
	if err != nil {
		return err
	}
	if archivedAt != nil {
		return errInvalid("environment %s is archived", envID)
	}
	return nil
}
