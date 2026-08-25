package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// memoryVersionJSON is the BetaManagedAgentsMemoryVersion wire shape
// (anthropic-sdk-go v1.66.0 betamemorystorememoryversion.go:234-310): "one
// immutable, attributed row in a memory's append-only history … Versions
// belong to the store (not the individual memory) and persist after the memory
// is deleted."
//
// Four fields are nullable for two separate reasons the row itself already
// carries, so the renderer only ever applies `view` on top: a `deleted`
// version has no content, digest or size, and a redacted one has none of those
// and no path either — "`null` if and only if `redacted_at` is set". A client
// is told to "branch on `redacted_at`, not HTTP status", which is why a
// redacted version still answers 200.
type memoryVersionJSON struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	MemoryStoreID    string            `json:"memory_store_id"`
	MemoryID         string            `json:"memory_id"`
	Operation        string            `json:"operation"`
	Path             *string           `json:"path"`
	Content          *string           `json:"content"`
	ContentSizeBytes *int64            `json:"content_size_bytes"`
	ContentSha256    *string           `json:"content_sha256"`
	CreatedBy        map[string]string `json:"created_by"`
	CreatedAt        time.Time         `json:"created_at"`
	RedactedAt       *time.Time        `json:"redacted_at"`
	RedactedBy       map[string]string `json:"redacted_by"`
}

const memoryVersionColumns = `id, memory_id, operation, path, content, content_sha256,
	content_size_bytes, created_by, created_at, redacted_at, redacted_by`

type memoryVersionRow struct {
	id, memoryID, operation string
	path, content, sha      *string
	size                    *int64
	createdBy               []byte
	createdAt               time.Time
	redactedAt              *time.Time
	redactedBy              []byte
}

func (v *memoryVersionRow) scan(row pgx.Row) error {
	return row.Scan(&v.id, &v.memoryID, &v.operation, &v.path, &v.content, &v.sha,
		&v.size, &v.createdBy, &v.createdAt, &v.redactedAt, &v.redactedBy)
}

func renderMemoryVersion(storeID string, row memoryVersionRow, full bool) (memoryVersionJSON, error) {
	out := memoryVersionJSON{
		ID: row.id, Type: "memory_version", MemoryStoreID: storeID, MemoryID: row.memoryID,
		Operation: row.operation, Path: row.path, ContentSizeBytes: row.size,
		ContentSha256: row.sha, CreatedAt: row.createdAt.UTC(), RedactedAt: utcPtr(row.redactedAt),
	}
	if full {
		out.Content = row.content
	}
	for _, actor := range []struct {
		raw  []byte
		into *map[string]string
	}{{row.createdBy, &out.CreatedBy}, {row.redactedBy, &out.RedactedBy}} {
		if len(actor.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(actor.raw, actor.into); err != nil {
			return memoryVersionJSON{}, err
		}
	}
	return out, nil
}

func (s *server) getMemoryVersion(r *http.Request) (any, error) {
	ctx := r.Context()
	storeID, versionID := r.PathValue("id"), r.PathValue("vid")
	if err := checkID(storeID, "memory store"); err != nil {
		return nil, err
	}
	if err := checkID(versionID, "memory version"); err != nil {
		return nil, err
	}
	view, err := parseMemoryView(r.URL.Query(), viewFull)
	if err != nil {
		return nil, err
	}
	var row memoryVersionRow
	err = row.scan(s.pool.QueryRow(ctx,
		`SELECT `+memoryVersionColumns+` FROM memory_versions WHERE id = $1 AND memory_store_id = $2`,
		versionID, storeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("memory version %s not found", versionID)
	}
	if err != nil {
		return nil, err
	}
	return renderMemoryVersion(storeID, row, view == viewFull)
}

func (s *server) listMemoryVersions(r *http.Request) (any, error) {
	ctx := r.Context()
	storeID := r.PathValue("id")
	if err := checkID(storeID, "memory store"); err != nil {
		return nil, err
	}
	q := r.URL.Query()
	view, err := parseMemoryView(q, viewBasic)
	if err != nil {
		return nil, err
	}
	// No default limit is published for this list; 20 is the sibling lists'
	// and the one this platform applies (registered as an inference).
	page, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	// "Listing with view=full caps limit at 20" — the view enum documents that
	// for a memory_version as much as for a memory, so the clamp listMemories
	// applies is this list's too, and just as silent.
	if view == viewFull && page.limit > memoryFullViewLimit {
		page.limit = memoryFullViewLimit
	}
	gte, err := parseTimeParam(q, "created_at[gte]")
	if err != nil {
		return nil, err
	}
	lte, err := parseTimeParam(q, "created_at[lte]")
	if err != nil {
		return nil, err
	}

	query := `SELECT ` + memoryVersionColumns + ` FROM memory_versions WHERE memory_store_id = $1`
	args := []any{storeID}
	// A malformed id can never name a stored row, so it is rejected on shape
	// rather than bound into the query (#135).
	for _, filter := range []struct{ param, column string }{
		{"memory_id", "memory_id"},
		{"session_id", "created_by->>'session_id'"},
	} {
		v := q.Get(filter.param)
		if v == "" {
			continue
		}
		if !domain.ID(v).Valid() {
			return nil, errInvalid("%s must be a valid id", filter.param)
		}
		args = append(args, v)
		query += fmt.Sprintf(` AND %s = $%d`, filter.column, len(args))
	}
	// The other two actor lanes take storableText instead of the prefix check:
	// neither apikey_ nor svac_ is in knownPrefixes — both are off-wire
	// identifiers — so the shape check would refuse every real one. This
	// platform emits no service_account_actor, which is exactly why the lane
	// has to exist: an ignored filter would hand a client that asked for one
	// service account's writes the store's whole history.
	for _, filter := range []struct{ param, column string }{
		{"api_key_id", "created_by->>'api_key_id'"},
		{"service_account_id", "created_by->>'service_account_id'"},
	} {
		v := q.Get(filter.param)
		if v == "" {
			continue
		}
		if !storableText(v) {
			return nil, errInvalid("%s must be valid text", filter.param)
		}
		args = append(args, v)
		query += fmt.Sprintf(` AND %s = $%d`, filter.column, len(args))
	}
	if v := q.Get("operation"); v != "" {
		if v != "created" && v != "modified" && v != "deleted" {
			return nil, errInvalid(`operation must be "created", "modified" or "deleted"`)
		}
		args = append(args, v)
		query += fmt.Sprintf(` AND operation = $%d`, len(args))
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
		// Unidirectional list: only forward time cursors are valid here.
		if page.cur.versioned || page.cur.pathKeyed || page.cur.seqKeyed || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
		args = append(args, page.cur.t, page.cur.id)
		query += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, page.limit+1)
	// "ordered by created_at descending (newest first), with id as tiebreak".
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))

	if err := s.checkMemoryStore(ctx, storeID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []any
	var lastT time.Time
	var lastID string
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		var row memoryVersionRow
		if err := row.scan(rows); err != nil {
			return nil, err
		}
		version, err := renderMemoryVersion(storeID, row, view == viewFull)
		if err != nil {
			return nil, err
		}
		data = append(data, version)
		lastT, lastID = row.createdAt, row.id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := pageJSON{Data: data}
	if out.Data == nil {
		out.Data = []any{}
	}
	if fetched > page.limit {
		c := encodeTimeCursor(dirNext, lastT, lastID)
		out.NextPage = &c
	}
	return out, nil
}

// redactMemoryVersion is the only statement that ever updates a memory_versions
// row, and the immutability the append-only log promises means exactly that.
// It nulls content, content_sha256, content_size_bytes and path, and records
// who did it; the attribution of the original write is preserved through it.
//
// Two rules the reference states without a status, both registered as
// inferences. "A version that is the current head of a live memory cannot be
// redacted" — a 400 here, since a redaction that left the memory serving the
// same bytes would erase nothing. And an archived store still admits one
// (decision 3): archived means no new content, not no compliance erasure, and
// archiving is one-way, so a refusal would leave the whole-store delete as the
// only lever. On an archived store the head is redactable too — nothing can
// supersede it — and the memory row is emptied so the bytes stop being served.
func (s *server) redactMemoryVersion(r *http.Request) (any, error) {
	ctx := r.Context()
	storeID, versionID := r.PathValue("id"), r.PathValue("vid")
	if err := checkID(storeID, "memory store"); err != nil {
		return nil, err
	}
	if err := checkID(versionID, "memory version"); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	archived, err := lockMemoryStore(ctx, tx, storeID)
	if err != nil {
		return nil, err
	}
	var row memoryVersionRow
	err = row.scan(tx.QueryRow(ctx,
		`SELECT `+memoryVersionColumns+`
		   FROM memory_versions WHERE id = $1 AND memory_store_id = $2 FOR UPDATE`,
		versionID, storeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("memory version %s not found", versionID)
	}
	if err != nil {
		return nil, err
	}
	// Already redacted: there is nothing left to erase, so the call returns the
	// row it would have produced rather than moving redacted_at (the archive
	// idempotence rule).
	if row.redactedAt != nil {
		return renderMemoryVersion(storeID, row, true)
	}

	var head string
	err = tx.QueryRow(ctx,
		`SELECT id FROM memories WHERE memory_store_id = $1 AND memory_version_id = $2 FOR UPDATE`,
		storeID, versionID).Scan(&head)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if head != "" && !archived {
		return nil, errInvalid(
			"memory version %s is the current head of memory %s and cannot be redacted", versionID, head)
	}
	if head != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE memories
			    SET content = '', content_sha256 = $2, content_size_bytes = 0, updated_at = now()
			  WHERE id = $1`, head, contentDigest("")); err != nil {
			return nil, err
		}
	}

	var redactedBy any
	if actor := memoryActor(ctx); actor != nil {
		redactedBy = actor
	}
	err = row.scan(tx.QueryRow(ctx,
		`UPDATE memory_versions
		    SET content = NULL, content_sha256 = NULL, content_size_bytes = NULL, path = NULL,
		        redacted_at = now(), redacted_by = $2
		  WHERE id = $1
		  RETURNING `+memoryVersionColumns, versionID, redactedBy))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderMemoryVersion(storeID, row, true)
}
