package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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

	// FOR SHARE, not loadDeployment's FOR UPDATE: manual runs have no
	// occurrence to claim, so two may fire concurrently — while a concurrent
	// archive (FOR UPDATE on this row) still cannot slip between this read
	// and the settlement. Paused is deliberately not checked: "manual runs
	// through the run endpoint are still allowed while paused" (§8.1 entry
	// 11). The raw resources column rides along because scanDeployment's
	// stored form is the credential-free echo, and the fire copies ciphertext
	// (§5.1) — the cipher is never dialed here.
	var rawResources []byte
	d, err := scanDeployment(tx.QueryRow(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE id = $1 FOR SHARE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("deployment %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if d.ArchivedAt != nil {
		return nil, errInvalid("deployment %s is archived", id)
	}
	if err := tx.QueryRow(ctx,
		`SELECT resources FROM deployments WHERE id = $1`, id).Scan(&rawResources); err != nil {
		return nil, err
	}

	run := domain.DeploymentRun{
		ID:             domain.NewID(domain.PrefixDeploymentRun),
		DeploymentID:   d.ID,
		TriggerContext: domain.TriggerContext{Type: "manual"},
		Agent:          d.Agent,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO deployment_runs (id, deployment_id, trigger_type, agent_id, agent_version)
		 VALUES ($1, $2, 'manual', $3, $4)
		 RETURNING created_at`,
		run.ID, d.ID, string(d.Agent.ID), d.Agent.Version).Scan(&run.CreatedAt); err != nil {
		return nil, err
	}

	in, err := deploymentSessionIn(d, rawResources)
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
		run.Error = &domain.RunError{Type: re.typ, Message: re.err.Error()}
		// The run row survives the savepoint rollback — it was inserted
		// before the savepoint — so this settlement updates a row this
		// transaction proved exists.
		if _, err := tx.Exec(ctx,
			`UPDATE deployment_runs SET error_type = $1, error_message = $2 WHERE id = $3`,
			re.typ, re.err.Error(), run.ID); err != nil {
			return nil, err
		}
	} else {
		sid := domain.ID(created.row.id)
		run.SessionID = &sid
		if _, err := tx.Exec(ctx,
			`UPDATE deployment_runs SET session_id = $1, succeeded_at = now() WHERE id = $2`,
			created.row.id, run.ID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if fireErr == nil {
		// The post-commit halves createSession keeps for itself, because a
		// fired session is the same session: the status metric and the
		// resource-mutation metric observe the committed state.
		if created.initialEvents > 0 {
			events.RecordSessionStatus(ctx, domain.SessionRunning)
		}
		if created.resources > 0 {
			recordResourceMutation(ctx, resourceOutcomeOK, created.resources)
			slog.InfoContext(ctx, "session created with resources",
				"session_id", created.row.id, "resource_count", created.resources)
		}
	}
	return run, nil
}

// deploymentSessionIn hydrates a session create from the deployment row: the
// fire validates exactly what POST /v1/sessions validates and no more, the
// parse stage having run at deployment create/update. The metadata bag is
// deliberately empty and the title unset — session metadata is the
// application layer's hook, not the schedule's (§8.1 entry 24) — and
// created_by stays NULL by construction, createSessionInTx reading the
// principal from a ctx that carries none of the deployment creator's.
func deploymentSessionIn(d domain.Deployment, rawResources []byte) (createSessionIn, error) {
	var stored []deploymentResource
	if err := json.Unmarshal(rawResources, &stored); err != nil {
		return createSessionIn{}, err
	}
	inputs, sealed := sessionInputsFrom(stored)
	vaultIDs := make([]string, 0, len(d.VaultIDs))
	for _, vid := range d.VaultIDs {
		vaultIDs = append(vaultIDs, string(vid))
	}
	deplID := string(d.ID)
	return createSessionIn{
		envID: string(d.EnvironmentID),
		agentRaw: mustJSON(map[string]any{
			"type": "agent", "id": string(d.Agent.ID), "version": d.Agent.Version,
		}),
		metadata:       map[string]string{},
		resourceInputs: inputs,
		sealedTokens:   sealed,
		vaultIDs:       vaultIDs,
		rawInitial:     d.InitialEvents,
		deploymentID:   &deplID,
	}, nil
}

// sessionInputsFrom is deploymentResourcesFrom's inverse at fire time: the
// stored elements become the validated inputs a session create takes, and a
// repository's sealed token is copied as ciphertext. The positional pairing is
// deploymentResourcesFrom's own, walked back the other way.
func sessionInputsFrom(stored []deploymentResource) ([]resourceInput, []sealedToken) {
	var inputs []resourceInput
	var sealed []sealedToken
	for _, r := range stored {
		switch r.Type {
		case "github_repository":
			inputs = append(inputs, resourceInput{
				kind: resourceKindRepo, url: r.URL,
				checkout: r.Checkout, mountPath: r.MountPath,
			})
			sealed = append(sealed, sealedToken{ciphertext: r.Token.Ciphertext, keyID: r.Token.KeyID})
		case "memory_store":
			inputs = append(inputs, resourceInput{
				kind: resourceKindMemory, memoryStoreID: r.MemoryStoreID,
				access: r.Access, instructions: r.Instructions,
			})
		default:
			inputs = append(inputs, resourceInput{
				kind: resourceKindFile, fileID: r.FileID, mountPath: r.MountPath,
			})
		}
	}
	return inputs, sealed
}
