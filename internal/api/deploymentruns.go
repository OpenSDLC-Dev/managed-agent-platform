package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// runError tags an apiError with the run-error type a deployment run records
// when that failure settles a fire (plan 37 §5.2) — typed at the source so the
// settlement never classifies by matching message strings. Transparent
// everywhere else: writeError unwraps to the apiError beneath, so
// POST /v1/sessions answers exactly as it did before the tag existed.
type runError struct {
	typ string
	err error
}

func (e *runError) Error() string { return e.err.Error() }
func (e *runError) Unwrap() error { return e.err }

// classified wraps err with the run-error type a deployment fire records for
// it. An error nothing wraps is unclassified: the fire rolls the whole
// transaction back instead of settling a run (§5.2's last row).
func classified(typ string, err error) error { return &runError{typ: typ, err: err} }

// runDeployment is POST /v1/deployments/{id}/run — the reference's manual
// trigger: "the run was started manually by creating a session directly
// against the deployment". One transaction inserts the run row, creates the
// session under a savepoint, and settles exactly one of session_id and error:
//
//   - success: the session commits, the run carries session_id, and
//     succeeded_at is stamped as the durable marker (#520 — the session link
//     is ON DELETE SET NULL and may go stale; the marker may not).
//   - a classified failure (§5.2): the savepoint rollback discards the
//     half-made session, the run settles on its error arm, and the response
//     is still a 200 — the endpoint's only success shape is the run object,
//     and its error member is where failure lives. A manual run never pauses:
//     "only a scheduled fire auto-pauses".
//   - anything unclassified: the whole transaction rolls back, no run row is
//     recorded, and the request answers with the underlying HTTP error
//     (§8.1 entry 14's shape).
func (s *server) runDeployment(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "deployment"); err != nil {
		return nil, err
	}
	// The request body is neither read nor refused: the reference declares no
	// requestBody on any deployment action (§5.1).

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// One FOR SHARE read of exactly what the fire consumes — the raw
	// resources column included, because scanDeployment's stored form is the
	// credential-free echo, and the fire copies ciphertext (§5.1): the cipher
	// is never dialed here. FOR SHARE, not loadDeployment's FOR UPDATE:
	// manual runs have no occurrence to claim, so two may fire concurrently,
	// while a concurrent archive (FOR UPDATE on this row) still cannot slip
	// between this read and the settlement. Paused is deliberately not
	// checked: "manual runs through the run endpoint are still allowed while
	// paused" (§8.1 entry 11).
	var (
		envID, agentID        string
		agentVersion          int
		vaultIDs              []string
		initial, rawResources []byte
		archivedAt            *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT environment_id, agent_id, agent_version, vault_ids, initial_events, resources, archived_at
		   FROM deployments WHERE id = $1 FOR SHARE`, id).
		Scan(&envID, &agentID, &agentVersion, &vaultIDs, &initial, &rawResources, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("deployment %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		return nil, errInvalid("deployment %s is archived", id)
	}

	run := domain.DeploymentRun{
		ID:             domain.NewID(domain.PrefixDeploymentRun),
		DeploymentID:   domain.ID(id),
		TriggerContext: domain.TriggerContext{Type: "manual"},
		Agent:          domain.AgentReference{Type: "agent", ID: domain.ID(agentID), Version: agentVersion},
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO deployment_runs (id, deployment_id, trigger_type, agent_id, agent_version)
		 VALUES ($1, $2, 'manual', $3, $4)
		 RETURNING created_at`,
		run.ID, id, agentID, agentVersion).Scan(&run.CreatedAt); err != nil {
		return nil, err
	}

	in, err := deploymentSessionIn(id, envID, agentID, agentVersion, vaultIDs, initial, rawResources)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT fire`); err != nil {
		return nil, err
	}
	created, fireErr := s.createSessionInTx(ctx, tx, in)
	if fireErr != nil {
		var re *runError
		if !errors.As(fireErr, &re) {
			return nil, fireErr
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT fire`); err != nil {
			return nil, err
		}
		// The run row survives the savepoint rollback — it was inserted
		// before the savepoint.
		run.Error = &domain.RunError{Type: re.typ, Message: re.err.Error()}
		if err := settleRun(ctx, tx,
			`UPDATE deployment_runs SET error_type = $1, error_message = $2 WHERE id = $3`,
			re.typ, re.err.Error(), run.ID); err != nil {
			return nil, err
		}
	} else {
		sid := domain.ID(created.row.id)
		run.SessionID = &sid
		if err := settleRun(ctx, tx,
			`UPDATE deployment_runs SET session_id = $1, succeeded_at = now() WHERE id = $2`,
			created.row.id, run.ID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if fireErr == nil {
		created.recordCreated(ctx)
	}
	return run, nil
}

// settleRun runs one settlement UPDATE and holds it to plan §4.1's rule: the
// settlement must affect exactly one row, because zero would commit a run
// with neither arm set — the shape the reference forbids. Unreachable here,
// where the row was inserted before the savepoint in this same transaction,
// and checked anyway: slice 4's scheduler settles an ON CONFLICT-claimed row,
// where zero is a real outcome, and its settlement must not drift from this
// one.
func settleRun(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("run settlement affected %d rows, want exactly 1", n)
	}
	return nil
}

// deploymentSessionIn hydrates a session create from the deployment's stored
// columns: the fire validates exactly what POST /v1/sessions validates and no
// more, the parse stage having run at deployment create/update. The metadata
// bag is deliberately empty and the title unset — session metadata is the
// application layer's hook, not the deployment's (§8.1 entry 24). created_by
// is left to createSessionInTx's ctx read: a manual run therefore attributes
// the session to the caller who fired it — the request is authenticated, and
// created_by is the audit answer to who caused a row to exist — while a
// scheduled fire, whose ticker ctx carries no principal, creates
// unattributed (plan §9's NULL is the schedule's, not this path's).
func deploymentSessionIn(deplID, envID, agentID string, agentVersion int,
	vaultIDs []string, initial, rawResources []byte) (createSessionIn, error) {
	var stored []deploymentResource
	if err := json.Unmarshal(rawResources, &stored); err != nil {
		return createSessionIn{}, err
	}
	inputs, sealed, err := sessionInputsFrom(stored)
	if err != nil {
		return createSessionIn{}, err
	}
	var rawInitial []json.RawMessage
	if err := json.Unmarshal(initial, &rawInitial); err != nil {
		return createSessionIn{}, err
	}
	return createSessionIn{
		envID: envID,
		agentRaw: mustJSON(map[string]any{
			"type": "agent", "id": agentID, "version": agentVersion,
		}),
		metadata:       map[string]string{},
		resourceInputs: inputs,
		sealedTokens:   sealed,
		vaultIDs:       vaultIDs,
		rawInitial:     rawInitial,
		deploymentID:   &deplID,
	}, nil
}
