package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/jackc/pgx/v5"
)

// dreamJSON is the BetaDream wire shape (anthropic-sdk-go v1.70.1
// betadream.go:150-178). All fourteen fields are api:"required" and the spec's
// BetaDream forbids extra keys, so every one renders on every response —
// nullable ones as null rather than by omission.
//
// The runner behind them (dreamrunner.go) is what fills `outputs`,
// `session_id`, `usage` and `error`; a create on a deployment that runs none
// is refused rather than left pending forever (plan 41 §4.7).
type dreamJSON struct {
	ID             string            `json:"id"`
	Type           string            `json:"type"`
	Status         string            `json:"status"`
	Inputs         []json.RawMessage `json:"inputs"`
	Outputs        []json.RawMessage `json:"outputs"`
	Model          domain.Model      `json:"model"`
	Instructions   *string           `json:"instructions"`
	OutputBehavior json.RawMessage   `json:"output_behavior"`
	SessionID      *string           `json:"session_id"`
	CreatedAt      time.Time         `json:"created_at"`
	EndedAt        *time.Time        `json:"ended_at"`
	ArchivedAt     *time.Time        `json:"archived_at"`
	Usage          dreamUsageJSON    `json:"usage"`
	Error          *dreamErrorJSON   `json:"error"`
}

// dreamUsageJSON is BetaDreamUsage (betadream.go:594-612): the four counters
// flat, unlike a session's nested cache_creation split — the dream's
// cache_creation_input_tokens is the "sum of all TTL tiers".
type dreamUsageJSON struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// dreamErrorJSON is BetaDreamError: failure detail for a failed dream, written
// by the tick's arms and by the start's classified failures (§5.2).
type dreamErrorJSON struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// The create bounds the guide publishes: "1 to 100 sessions" and instructions
// of "1-4,096 characters". Lengths count runes, not bytes — the repo's reading
// of a documented "character" (validateMetadataCaps).
const (
	dreamSessionsMax     = 100
	dreamInstructionsMax = 4096
)

var validDreamStatuses = map[string]bool{
	"pending": true, "running": true, "completed": true, "failed": true, "canceled": true,
}

// dreamColumns is the stored dream row in scanDream's order; every handler
// reads through it, so no route can render a dream differently from another.
const dreamColumns = `id, status, inputs, outputs, model, instructions, output_behavior,
	session_id, usage, error, created_at, ended_at, archived_at`

func scanDream(row pgx.Row) (dreamJSON, error) {
	var (
		d                                          dreamJSON
		inputs, outputs, model, behavior, usageRaw []byte
		errRaw                                     []byte
	)
	if err := row.Scan(&d.ID, &d.Status, &inputs, &outputs, &model, &d.Instructions,
		&behavior, &d.SessionID, &usageRaw, &errRaw, &d.CreatedAt, &d.EndedAt,
		&d.ArchivedAt); err != nil {
		return d, err
	}
	for _, f := range []struct {
		raw []byte
		dst any
	}{
		{inputs, &d.Inputs}, {outputs, &d.Outputs}, {model, &d.Model},
		{behavior, &d.OutputBehavior}, {usageRaw, &d.Usage},
	} {
		if err := json.Unmarshal(f.raw, f.dst); err != nil {
			return d, err
		}
	}
	if errRaw != nil {
		if err := json.Unmarshal(errRaw, &d.Error); err != nil {
			return d, err
		}
	}
	d.Type = "dream"
	d.CreatedAt = d.CreatedAt.UTC()
	d.EndedAt, d.ArchivedAt = utcPtr(d.EndedAt), utcPtr(d.ArchivedAt)
	return d, nil
}

// dreamSessionID normalizes the session_ wire spelling onto sesn_, so the
// stored input_session_ids and the existence check name rows the way the
// sessions table does. inputs[] keeps the client's own spelling: it is echoed
// as sent (§2.3). Only ever called on an id already known to carry one of the
// two session prefixes.
func dreamSessionID(id string) string {
	_, token, _ := strings.Cut(id, "_")
	return domain.PrefixSession + "_" + token
}

func (s *server) createDream(r *http.Request) (any, error) {
	ctx := r.Context()
	// Nothing would ever run this dream: the runner owns the timeout too, so a
	// create on a runner-less deployment would leave a row pending forever
	// (plan 41 §4.7). The other four routes keep answering.
	if !s.dreamRunner {
		return nil, errDreamRunnerDisabled
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "inputs", "model", "instructions", "output_behavior"); err != nil {
		return nil, err
	}

	rawInputs, ok := obj["inputs"]
	if !ok {
		return nil, errInvalid("inputs is required")
	}
	storeID, sessionIDs, err := parseDreamInputs(rawInputs)
	if err != nil {
		return nil, err
	}

	rawModel, ok := obj["model"]
	if !ok || isNull(rawModel) {
		return nil, errInvalid("model is required")
	}
	// The spec puts additionalProperties:false on the model object, and
	// parseModel decodes with a plain json.Unmarshal that would drop an unknown
	// key silently — so the object form is key-checked before it is parsed.
	var modelObj map[string]json.RawMessage
	if json.Unmarshal(rawModel, &modelObj) == nil && modelObj != nil {
		if err := rejectUnknownKeys(modelObj, "id", "speed"); err != nil {
			return nil, err
		}
	}
	model, err := parseModel(rawModel)
	if err != nil {
		return nil, err
	}

	// Absent and explicitly null are both unset, the repo's create-route rule
	// (stringField / parseMetadata).
	var instructions *string
	if v, set, null, err := stringField(obj, "instructions"); err != nil {
		return nil, err
	} else if set && !null {
		if n := utf8.RuneCountInString(v); n == 0 || n > dreamInstructionsMax {
			return nil, errInvalid("instructions must be 1 to %d characters", dreamInstructionsMax)
		}
		instructions = &v
	}

	behavior, target, err := parseDreamOutputBehavior(obj)
	if err != nil {
		return nil, err
	}
	// The EAP rule (§5.3), and the last of the create's body checks: an
	// in-place dream consolidates its own input store, so a target naming any
	// other store is a 400 — INFERRED, the reference publishing the rule and
	// not its code.
	if target != nil && *target != storeID {
		return nil, errInvalid("output_behavior.memory_store_id must be the job's own memory_store input")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkDreamInputsExist(ctx, tx, storeID, sessionIDs); err != nil {
		return nil, err
	}
	// inputs[] is stored as the client sent it and rendered back from the
	// column, so the response echoes the values — including the session_ id
	// spelling, which input_session_ids normalizes away. jsonb is not
	// byte-preserving (it reorders object keys and drops insignificant
	// whitespace), so "as sent" is by value, not by bytes.
	d, err := scanDream(tx.QueryRow(ctx,
		`INSERT INTO dreams (id, status, inputs, input_memory_store_id, input_session_ids,
			model, instructions, output_behavior, target_memory_store_id, created_by)
		 VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING `+dreamColumns,
		domain.NewID(domain.PrefixDream).String(), rawInputs, storeID, sessionIDs,
		mustJSON(model), instructions, behavior, target, principalPtr(ctx)))
	if err != nil {
		// The hold (§5.3). dreams_target_hold_idx is the enforcement — one
		// live in-place dream per target store, "live" being any status not
		// yet closed — and this turns the unique violation it raises into the
		// spec's 409 rather than the 500 an unhandled one would be.
		if isUniqueViolation(err, "dreams_target_hold_idx") {
			_ = tx.Rollback(ctx)
			return nil, s.dreamTargetHeld(ctx, storeID)
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// dreamTargetHeld names the dream holding the target store, for the 409 above.
// The read comes AFTER the failed insert has rolled back, because the insert
// is what serializes two concurrent creates and a read before it could only
// race; the price is the one case the spec allows the name to be missing from,
// a holder that closed in between — which is also the case where the retry the
// message asks for is about to succeed. Any other read failure is answered the
// same way: the insert already proved the hold, so a 409 without the name
// beats a 500.
func (s *server) dreamTargetHeld(ctx context.Context, storeID string) error {
	var holder string
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM dreams WHERE target_memory_store_id = $1 AND closed_at IS NULL`,
		storeID).Scan(&holder); err != nil {
		return errTargetStoreHeld("memory store %s is still held by a prior update_existing "+
			"dream; retry", storeID)
	}
	return errTargetStoreHeld("memory store %s is still held by dream %s; wait for it to finish "+
		"or cancel it, then retry", storeID, holder)
}

// parseDreamInputs validates the inputs[] union and returns the denormalized
// store id and the normalized session ids. Both arms are key-checked before
// they are read: the spec puts additionalProperties:false on each, so a
// session_ids on the memory_store arm is a 400 rather than a silently ignored
// field.
func parseDreamInputs(raw json.RawMessage) (storeID string, sessionIDs []string, err error) {
	// A null inputs decodes to the empty list, which then fails the cardinality
	// rule below — the same 400, naming what was actually found.
	items, err := rawList(raw, "inputs")
	if err != nil {
		return "", nil, err
	}
	var stores, sessions int
	for _, item := range items {
		var arm map[string]json.RawMessage
		if err := json.Unmarshal(item, &arm); err != nil || arm == nil {
			return "", nil, errInvalid("inputs entries must be objects")
		}
		var typ string
		if t, ok := arm["type"]; ok {
			_ = json.Unmarshal(t, &typ)
		}
		switch typ {
		case "memory_store":
			if err := rejectUnknownKeys(arm, "type", "memory_store_id"); err != nil {
				return "", nil, err
			}
			stores++
			id, err := requiredString(arm, "memory_store_id")
			if err != nil {
				return "", nil, err
			}
			if !domain.ID(id).HasPrefix(domain.PrefixMemoryStore) || !domain.ID(id).Valid() {
				return "", nil, errInvalid("memory_store_id %q is not a memory store id", id)
			}
			storeID = id
		case "sessions":
			if err := rejectUnknownKeys(arm, "type", "session_ids"); err != nil {
				return "", nil, err
			}
			sessions++
			if sessionIDs, err = parseDreamSessionIDs(arm); err != nil {
				return "", nil, err
			}
		default:
			return "", nil, errInvalid(`inputs entries must have a type of "memory_store" or "sessions"`)
		}
	}
	// The cardinality rule is the guide's prose ("a pre-existing memory store"
	// and "1 to 100 sessions"), not the schema's — the spec bounds neither the
	// array nor the arms (§2.4, recorded in docs/DIVERGENCES.md).
	if stores != 1 || sessions != 1 {
		return "", nil, errInvalid("inputs must carry exactly one memory_store input and one "+
			"sessions input (found %d and %d)", stores, sessions)
	}
	return storeID, sessionIDs, nil
}

func parseDreamSessionIDs(arm map[string]json.RawMessage) ([]string, error) {
	raw, ok := arm["session_ids"]
	if !ok || isNull(raw) {
		return nil, errInvalid("session_ids is required")
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, errInvalid("session_ids must be an array of session ids")
	}
	if len(ids) == 0 || len(ids) > dreamSessionsMax {
		return nil, errInvalid("session_ids must carry 1 to %d session ids, not %d",
			dreamSessionsMax, len(ids))
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !domain.ID(id).HasPrefix(domain.PrefixSession) || !domain.ID(id).Valid() {
			return nil, errInvalid("session_ids entry %q is not a session id", id)
		}
		// The two spellings of one session are one input: compared after
		// normalizing, so sesn_x and session_x cannot both be listed.
		norm := dreamSessionID(id)
		if seen[norm] {
			return nil, errInvalid("session_ids must not repeat %q", id)
		}
		seen[norm] = true
		out = append(out, norm)
	}
	return out, nil
}

// parseDreamOutputBehavior returns the stored output_behavior and, under
// update_existing, the target store — nil under create_new, which holds
// nothing. Absent is the documented default, {type: create_new}; an explicit
// null is read the same way — the repo's create-route rule (stringField,
// parseMetadata), not the spec's, which types the field non-nullable, so
// docs/DIVERGENCES.md registers it.
//
// The target is returned rather than checked here: the rule it must satisfy
// (§5.3) compares it against the memory_store input, which only the create
// knows.
func parseDreamOutputBehavior(obj map[string]json.RawMessage) (json.RawMessage, *string, error) {
	raw, ok := obj["output_behavior"]
	if !ok || isNull(raw) {
		return json.RawMessage(`{"type":"create_new"}`), nil, nil
	}
	var ob map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ob); err != nil || ob == nil {
		return nil, nil, errInvalid("output_behavior must be an object")
	}
	var typ string
	if t, ok := ob["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	var target *string
	switch typ {
	case "create_new":
		if err := rejectUnknownKeys(ob, "type"); err != nil {
			return nil, nil, err
		}
	case "update_existing":
		if err := rejectUnknownKeys(ob, "type", "memory_store_id"); err != nil {
			return nil, nil, err
		}
		// BetaOutputBehaviorUpdateExisting lists memory_store_id as required
		// with a minLength of 1, so absent, null, empty and non-string are all
		// 400s here rather than targets the rule downstream would then refuse
		// for the wrong reason. The message names the field's full path: an
		// unqualified one reads as the memory_store input's own id, which this
		// body also carries.
		v, set, null, err := stringField(ob, "memory_store_id")
		if err != nil {
			// stringField formats with the bare key, which is the one wording
			// this field must not take, so the non-string arm is re-spelled
			// here rather than passed through.
			return nil, nil, errInvalid("output_behavior.memory_store_id must be a string")
		}
		if !set || null || v == "" {
			return nil, nil, errInvalid("output_behavior.memory_store_id is required")
		}
		target = &v
	default:
		return nil, nil, errInvalid(`output_behavior.type must be "create_new" or "update_existing"`)
	}
	return raw, target, nil
}

// checkDreamInputsExist refuses a create naming inputs that are not there. Each
// is a 400, the platform's precedent for a missing resource named in a create
// body (snapshotMemoryStore). The store is read FOR SHARE so a concurrent
// archive or delete cannot slip between this check and the INSERT; the sessions
// are not, because an archived session is an accepted input and a deleted one
// only makes a live dream fail later, which slice 2's tick already handles.
func checkDreamInputsExist(ctx context.Context, db querier, storeID string, sessionIDs []string) error {
	var archivedAt *time.Time
	err := db.QueryRow(ctx,
		`SELECT archived_at FROM memory_stores WHERE id = $1 FOR SHARE`, storeID).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return errInvalid("memory store %s not found", storeID)
	}
	if err != nil {
		return err
	}
	if archivedAt != nil {
		return errInvalid("memory store %s is archived", storeID)
	}

	rows, err := db.Query(ctx, `SELECT id FROM sessions WHERE id = ANY($1)`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := make(map[string]bool, len(sessionIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Named in request order, so the message points at the first bad id rather
	// than at whichever the database happened to return.
	for _, id := range sessionIDs {
		if !found[id] {
			return errInvalid("session %s not found", id)
		}
	}
	return nil
}

func (s *server) getDream(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "dream"); err != nil {
		return nil, err
	}
	// No archived filter: retrieve answers an archived dream too (§5.1).
	d, err := scanDream(s.pool.QueryRow(ctx,
		`SELECT `+dreamColumns+` FROM dreams WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("dream %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (s *server) listDreams(r *http.Request) (any, error) {
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
	// Bracketed and bare, the sessions-list precedent: the SDK and the CLI send
	// statuses[], the public list reference names it statuses. An empty set
	// applies no filter, which the guide says in as many words.
	statuses := append(q["statuses[]"], q["statuses"]...)
	for _, st := range statuses {
		if !validDreamStatuses[st] {
			return nil, errInvalid("invalid dream status %q", st)
		}
	}

	query := `SELECT ` + dreamColumns + ` FROM dreams WHERE true`
	var args []any
	if !includeArchived {
		query += ` AND archived_at IS NULL`
	}
	if len(statuses) > 0 {
		args = append(args, statuses)
		query += fmt.Sprintf(` AND status = ANY($%d)`, len(args))
	}
	// Both bounds are EXCLUSIVE, unlike the agent, memory-store and deployment
	// lists' [gte]/[lte] pair — the dream list publishes only these two
	// (betadream.go:923-946).
	for _, f := range []struct{ key, op string }{{"created_at[gt]", ">"}, {"created_at[lt]", "<"}} {
		t, err := parseTimeParam(q, f.key)
		if err != nil {
			return nil, err
		}
		if t != nil {
			args = append(args, *t)
			query += fmt.Sprintf(` AND created_at %s $%d`, f.op, len(args))
		}
	}
	if page.cur != nil {
		// Every non-time cursor kind is rejected, not just the version one: a
		// seq or path cursor decodes with a zero time and an empty id, and
		// binding those would render an empty 200 page — end of history — where
		// the reference publishes a 400 (the deployment-runs list, #534).
		if page.cur.foreignToTime() || page.cur.dir != dirNext {
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

	out := pageJSON{Data: []any{}}
	var lastT time.Time
	var lastID string
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		d, err := scanDream(rows)
		if err != nil {
			return nil, err
		}
		out.Data = append(out.Data, d)
		lastT, lastID = d.CreatedAt, d.ID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if fetched > page.limit {
		c := encodeTimeCursor(dirNext, lastT, lastID)
		out.NextPage = &c
	}
	return out, nil
}

func (s *server) archiveDream(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "dream"); err != nil {
		return nil, err
	}
	return s.dreamAction(ctx, id, func(ctx context.Context, tx pgx.Tx, status string, archivedAt *time.Time, _ *string) error {
		// Idempotent: archived_at is set once and never cleared, so a second
		// archive answers 200 with the first call's timestamp. status is left
		// alone — "There is no unarchive" and no status change either (§2.6).
		if archivedAt != nil {
			return nil
		}
		if status == "pending" || status == "running" {
			return errInvalid("dream %s is %s; cancel it first", id, status)
		}
		_, err := tx.Exec(ctx,
			`UPDATE dreams SET archived_at = now(), updated_at = now() WHERE id = $1`, id)
		return err
	})
}

func (s *server) cancelDream(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkID(id, "dream"); err != nil {
		return nil, err
	}
	// The two arms below both move the status, so both are counted — after
	// dreamAction commits, because a metric observes committed state (the
	// runner's own transitions are counted the same way). sessionMoves is the
	// running arm's interrupt, counted at the same moment for the same reason.
	moved := false
	var sessionMoves []domain.SessionStatus
	d, err := s.dreamAction(ctx, id, func(ctx context.Context, tx pgx.Tx, status string, _ *time.Time, sessionID *string) error {
		switch status {
		case "canceled":
			// "Canceling an already-canceled dream is an idempotent no-op":
			// ended_at keeps the first call's timestamp.
			return nil
		case "pending":
			// A dream with no session has nothing to wind down, so it ends and
			// closes in the same commit (§5.3).
			moved = true
			_, err := tx.Exec(ctx,
				`UPDATE dreams SET status = 'canceled', ended_at = now(), closed_at = now(),
				        updated_at = now() WHERE id = $1`, id)
			return err
		case "running":
			// The interrupt runs in this transaction, not as a bare event
			// append, which would stop nothing: it settles the outstanding
			// calls, cancels the queued work and idles the threads (§4.1). It
			// hands back the status moves it made rather than counting them,
			// because this handler is the one that commits — and a session
			// already gone at the database level leaves nothing to interrupt.
			// closed_at stays for the runner's closing arm, which archives the
			// session and deletes the transcripts once it is no longer running.
			if sessionID != nil {
				moves, err := s.interruptSessionInTx(ctx, tx, *sessionID)
				if err != nil {
					return err
				}
				sessionMoves = moves
			}
			moved = true
			_, err := tx.Exec(ctx,
				`UPDATE dreams SET status = 'canceled', ended_at = now(), updated_at = now()
				  WHERE id = $1`, id)
			return err
		default:
			return errInvalid("dream %s is %s; only a pending or running dream can be canceled", id, status)
		}
	})
	if err == nil && moved {
		recordDreamTransition(ctx, "canceled")
		for _, st := range sessionMoves {
			events.RecordSessionStatus(ctx, st)
		}
	}
	return d, err
}

// dreamAction is the shared body of the two lifecycle routes: lock the row,
// let the caller decide what the transition is, and render the result through
// the one column list every other handler reads.
func (s *server) dreamAction(ctx context.Context, id string,
	apply func(context.Context, pgx.Tx, string, *time.Time, *string) error) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The one transaction a cancel can wait on is a start's write transaction,
	// bounded by the clone's size; without this bound a wedged start would
	// hang the request instead of failing it, and a failed request is what the
	// SDK retries (§4.1).
	if err := setDreamLockWait(ctx, tx); err != nil {
		return nil, err
	}

	var status string
	var archivedAt *time.Time
	var sessionID *string
	err = tx.QueryRow(ctx,
		`SELECT status, archived_at, session_id FROM dreams WHERE id = $1 FOR UPDATE`, id).
		Scan(&status, &archivedAt, &sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("dream %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if err := apply(ctx, tx, status, archivedAt, sessionID); err != nil {
		return nil, err
	}
	d, err := scanDream(tx.QueryRow(ctx, `SELECT `+dreamColumns+` FROM dreams WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}
