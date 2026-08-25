package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
	"github.com/jackc/pgx/v5"
)

// memoryJSON is the BetaManagedAgentsMemory wire shape (anthropic-sdk-go
// v1.66.0 betamemorystorememory.go:180-240). Everything but content is
// api:"required"; content is api:"nullable" and populated only under
// view=full, so it is the one pointer here. content_sha256 and
// content_size_bytes are "Always populated, regardless of `view`" — that is
// what lets a sync client diff a whole store without fetching a byte of it.
type memoryJSON struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`
	MemoryStoreID    string    `json:"memory_store_id"`
	Path             string    `json:"path"`
	Content          *string   `json:"content"`
	ContentSizeBytes int64     `json:"content_size_bytes"`
	ContentSha256    string    `json:"content_sha256"`
	MemoryVersionID  string    `json:"memory_version_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// memoryPrefixJSON is the other arm of the list union (:248-367): "a
// rolled-up directory marker … a list-time rollup, not a stored resource; it
// has no ID and no lifecycle", produced only when depth is set.
type memoryPrefixJSON struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// The two projections a memory or a version renders under
// (betamemorystorememory.go:369-379). The default is per endpoint: retrieve
// defaults to full, list/create/update to basic.
const (
	viewBasic = "basic"
	viewFull  = "full"
)

// memoryFullViewLimit is the documented silent clamp: a list with view=full
// is "Capped at 20" however large a limit the caller asked for.
const memoryFullViewLimit = 20

const memoryColumns = `id, path, content, content_sha256, content_size_bytes,
	memory_version_id, created_at, updated_at`

type memoryRow struct {
	id, path, content, sha string
	size                   int64
	versionID              string
	createdAt, updatedAt   time.Time
}

func (m *memoryRow) scan(row pgx.Row, before ...any) error {
	dest := append(before, &m.id, &m.path, &m.content, &m.sha, &m.size,
		&m.versionID, &m.createdAt, &m.updatedAt)
	return row.Scan(dest...)
}

func renderMemory(storeID string, row memoryRow, full bool) memoryJSON {
	out := memoryJSON{
		ID: row.id, Type: "memory", MemoryStoreID: storeID, Path: row.path,
		ContentSizeBytes: row.size, ContentSha256: row.sha, MemoryVersionID: row.versionID,
		CreatedAt: row.createdAt.UTC(), UpdatedAt: row.updatedAt.UTC(),
	}
	if full {
		out.Content = &row.content
	}
	return out
}

func parseMemoryView(q url.Values, def string) (string, error) {
	switch v := q.Get("view"); v {
	case "":
		return def, nil
	case viewBasic, viewFull:
		return v, nil
	default:
		return "", errInvalid(`view must be %q or %q`, viewBasic, viewFull)
	}
}

// contentDigest is the wire's content_sha256: "Lowercase hex SHA-256 digest of
// the UTF-8 content bytes (64 characters). The server applies no
// normalization, so clients can compute the same hash locally."
func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// memoryActor is decision 6's attribution, and it is principalFrom's two lanes
// under the reference's own actor union: a machine key writes as api_actor
// carrying the key's row id, a human as user_actor carrying their principal id
// (the reference's user_ ids are its Console's; ours are principal_, and the
// difference is registered). session_actor is slice 4's, for a write that
// arrives through a mounted store; service_account_actor is never emitted.
// Nil when nothing authenticated the write, which the wire renders as null.
func memoryActor(ctx context.Context) map[string]string {
	if id, _ := ctx.Value(ctxKeyPrincipal).(string); id != "" {
		return map[string]string{"type": "api_actor", "api_key_id": id}
	}
	if p, ok := identityFrom(ctx); ok {
		return map[string]string{"type": "user_actor", "user_id": p.ID}
	}
	return nil
}

// lockMemoryStore takes the store row's write lock and reports whether the
// store is archived. Every memory write in a store serializes behind this
// lock, which is what makes the occupancy check and the 2,000 cap decisions
// rather than races.
func lockMemoryStore(ctx context.Context, tx pgx.Tx, storeID string) (archived bool, err error) {
	var archivedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT archived_at FROM memory_stores WHERE id = $1 FOR UPDATE`, storeID).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound("memory store %s not found", storeID)
	}
	if err != nil {
		return false, err
	}
	return archivedAt != nil, nil
}

// lockMemoryStoreForWrite is lockMemoryStore plus decision 3's refusal: an
// archived store takes no new content. Redaction deliberately does not use it
// — a compliance erasure is not a new write, and archiving is one-way.
func lockMemoryStoreForWrite(ctx context.Context, tx pgx.Tx, storeID string) error {
	archived, err := lockMemoryStore(ctx, tx, storeID)
	if err != nil {
		return err
	}
	if archived {
		return errInvalid("memory store %s is archived", storeID)
	}
	return nil
}

// checkMemoryStore is the read half: the store must exist for its collections
// to answer, so a list or a get under an unknown store is that store's 404
// rather than an empty page.
func (s *server) checkMemoryStore(ctx context.Context, storeID string) error {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT true FROM memory_stores WHERE id = $1`, storeID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("memory store %s not found", storeID)
	}
	return err
}

// occupiedBy answers decision 4's occupancy question: a path is occupied when
// a memory sits at it OR at an ancestor or a descendant of it — /a and /a/b
// cannot both be files. The unique index catches only the equality arm, and
// LIKE is deliberately not used: `_` and `%` are legal path bytes, so a
// pattern would make /acb/x occupy /a_b. except is the memory the write is
// updating, so a rename does not conflict with itself; "" for a create.
func occupiedBy(ctx context.Context, tx pgx.Tx, storeID, path, except string) (id, conflicting string, err error) {
	err = tx.QueryRow(ctx,
		`SELECT id, path FROM memories
		  WHERE memory_store_id = $1 AND id <> $2
		    AND (path = $3
		      OR left(path, length($3) + 1) = $3 || '/'
		      OR left($3, length(path) + 1) = path || '/')
		  LIMIT 1`, storeID, except, path).Scan(&id, &conflicting)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return id, conflicting, err
}

// insertMemoryVersion appends the immutable row every non-no-op mutation
// writes. A `deleted` version carries its path and nothing else: "content …
// `null` when … `operation` is `deleted`", and the same for the digest and the
// size — so content is a pointer, nil for that one operation.
func insertMemoryVersion(ctx context.Context, tx pgx.Tx, storeID, memoryID, operation, path string,
	content *string, actor map[string]string) (string, error) {
	id := domain.NewID(domain.PrefixMemoryVersion).String()
	var body, sha, size, createdBy any
	if content != nil {
		body, sha, size = *content, contentDigest(*content), len(*content)
	}
	if actor != nil {
		createdBy = actor
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO memory_versions
		   (id, memory_store_id, memory_id, operation, path, content, content_sha256,
		    content_size_bytes, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, storeID, memoryID, operation, path, body, sha, size, createdBy)
	return id, err
}

func (s *server) createMemory(r *http.Request) (any, error) {
	ctx := r.Context()
	storeID := r.PathValue("id")
	if err := checkID(storeID, "memory store"); err != nil {
		return nil, err
	}
	view, err := parseMemoryView(r.URL.Query(), viewBasic)
	if err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "path", "content"); err != nil {
		return nil, err
	}
	path, err := requiredString(obj, "path")
	if err != nil {
		return nil, err
	}
	if err := memsync.ValidatePath(path); err != nil {
		return nil, errInvalid("%s", err)
	}
	// content is required but "" is a legal value — "Required; pass `""`
	// explicitly to create an empty memory" — so requiredString's non-empty
	// rule is the wrong one here.
	content, set, null, err := stringField(obj, "content")
	if err != nil {
		return nil, err
	}
	if !set || null {
		return nil, errInvalid("content is required")
	}
	if err := memsync.ValidateContent(content); err != nil {
		return nil, errInvalid("%s", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMemoryStoreForWrite(ctx, tx, storeID); err != nil {
		return nil, err
	}
	// "Create never overwrites; to modify an existing memory, use Update."
	conflictID, conflictPath, err := occupiedBy(ctx, tx, storeID, path, "")
	if err != nil {
		return nil, err
	}
	if conflictID != "" {
		return nil, errMemoryPathConflict(conflictID, conflictPath,
			"path %s is occupied by memory %s at %s", path, conflictID, conflictPath)
	}
	var held int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM memories WHERE memory_store_id = $1`, storeID).Scan(&held); err != nil {
		return nil, err
	}
	if held >= memsync.MaxMemoriesPerStore {
		return nil, errInvalid("memory store %s holds %d memories", storeID, memsync.MaxMemoriesPerStore)
	}

	row := memoryRow{
		id: domain.NewID(domain.PrefixMemory).String(), path: path,
		content: content, sha: contentDigest(content), size: int64(len(content)),
	}
	row.versionID, err = insertMemoryVersion(ctx, tx, storeID, row.id, "created", path, &content, memoryActor(ctx))
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO memories
		   (id, memory_store_id, path, content, content_sha256, content_size_bytes, memory_version_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at, updated_at`,
		row.id, storeID, path, content, row.sha, row.size, row.versionID).
		Scan(&row.createdAt, &row.updatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderMemory(storeID, row, view == viewFull), nil
}

func (s *server) getMemory(r *http.Request) (any, error) {
	ctx := r.Context()
	storeID, memoryID := r.PathValue("id"), r.PathValue("mid")
	if err := checkID(storeID, "memory store"); err != nil {
		return nil, err
	}
	if err := checkID(memoryID, "memory"); err != nil {
		return nil, err
	}
	// Retrieve is the one memory endpoint that defaults to full.
	view, err := parseMemoryView(r.URL.Query(), viewFull)
	if err != nil {
		return nil, err
	}
	var row memoryRow
	err = row.scan(s.pool.QueryRow(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE id = $1 AND memory_store_id = $2`,
		memoryID, storeID))
	if errors.Is(err, pgx.ErrNoRows) {
		// Read the store second, not first: the hit path is one query, and
		// the miss still names the store when that is what is missing.
		if err := s.checkMemoryStore(ctx, storeID); err != nil {
			return nil, err
		}
		return nil, errNotFound("memory %s not found", memoryID)
	}
	if err != nil {
		return nil, err
	}
	return renderMemory(storeID, row, view == viewFull), nil
}

func (s *server) updateMemory(r *http.Request) (any, error) {
	ctx := r.Context()
	storeID, memoryID := r.PathValue("id"), r.PathValue("mid")
	if err := checkID(storeID, "memory store"); err != nil {
		return nil, err
	}
	if err := checkID(memoryID, "memory"); err != nil {
		return nil, err
	}
	view, err := parseMemoryView(r.URL.Query(), viewBasic)
	if err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "content", "path", "precondition"); err != nil {
		return nil, err
	}
	content, contentSet, contentNull, err := stringField(obj, "content")
	if err != nil {
		return nil, err
	}
	path, pathSet, pathNull, err := stringField(obj, "path")
	if err != nil {
		return nil, err
	}
	// Both fields are anyOf [T, null] with no stated meaning for the null, and
	// the SDK's omitzero never sends one; a null reads as "leave it alone",
	// which is what omitting it already means.
	contentSet, pathSet = contentSet && !contentNull, pathSet && !pathNull
	if !contentSet && !pathSet {
		// Spec: "At least one of `content` or `path` must be provided".
		return nil, errInvalid("update requires content or path")
	}
	if contentSet {
		if err := memsync.ValidateContent(content); err != nil {
			return nil, errInvalid("%s", err)
		}
	}
	if pathSet {
		if err := memsync.ValidatePath(path); err != nil {
			return nil, errInvalid("%s", err)
		}
	}
	precondition, err := parsePrecondition(obj["precondition"])
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMemoryStoreForWrite(ctx, tx, storeID); err != nil {
		return nil, err
	}
	var row memoryRow
	err = row.scan(tx.QueryRow(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE id = $1 AND memory_store_id = $2 FOR UPDATE`,
		memoryID, storeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("memory %s not found", memoryID)
	}
	if err != nil {
		return nil, err
	}

	next := row
	if contentSet {
		next.content, next.sha, next.size = content, contentDigest(content), int64(len(content))
	}
	if pathSet {
		next.path = path
	}
	// An update that changes nothing is a no-op whether or not a precondition
	// came with it: "If the precondition fails but the stored state already
	// exactly matches the requested `content` and `path`, the server returns
	// 200 instead of 409", and "An update where every supplied field already
	// matches the stored value … returns 200 with the existing memory and
	// writes no new version". Same answer, so one branch serves both.
	unchanged := next.content == row.content && next.path == row.path
	if precondition != nil && *precondition != row.sha && !unchanged {
		return nil, errMemoryPrecondition(
			"memory %s has content_sha256 %s, not %s", memoryID, row.sha, *precondition)
	}
	if unchanged {
		return renderMemory(storeID, row, view == viewFull), nil
	}

	if next.path != row.path {
		// "Renaming onto a path occupied by a different memory returns
		// memory_path_conflict_error … Rename never overwrites; delete or
		// rename the blocking memory first."
		conflictID, conflictPath, err := occupiedBy(ctx, tx, storeID, next.path, memoryID)
		if err != nil {
			return nil, err
		}
		if conflictID != "" {
			return nil, errMemoryPathConflict(conflictID, conflictPath,
				"path %s is occupied by memory %s at %s", next.path, conflictID, conflictPath)
		}
	}
	next.versionID, err = insertMemoryVersion(ctx, tx, storeID, memoryID, "modified",
		next.path, &next.content, memoryActor(ctx))
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx,
		`UPDATE memories
		    SET path = $2, content = $3, content_sha256 = $4, content_size_bytes = $5,
		        memory_version_id = $6, updated_at = now()
		  WHERE id = $1 RETURNING updated_at`,
		memoryID, next.path, next.content, next.sha, next.size, next.versionID).
		Scan(&next.updatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderMemory(storeID, next, view == viewFull), nil
}

// parsePrecondition reads the optimistic-concurrency precondition
// (betamemorystorememory.go:381-412). type is the union's discriminator and
// carries one value; content_sha256 is param.Opt in the SDK but required here,
// since a precondition with nothing to compare against would silently become
// an unconditional write.
func parsePrecondition(raw json.RawMessage) (*string, error) {
	if !present(raw) {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errInvalid("precondition must be an object")
	}
	if err := rejectUnknownKeys(obj, "type", "content_sha256"); err != nil {
		return nil, err
	}
	kind, _, _, err := stringField(obj, "type")
	if err != nil {
		return nil, err
	}
	if kind != "content_sha256" {
		return nil, errInvalid(`precondition.type must be "content_sha256"`)
	}
	sha, set, null, err := stringField(obj, "content_sha256")
	if err != nil {
		return nil, err
	}
	if !set || null || sha == "" {
		return nil, errInvalid("precondition.content_sha256 is required")
	}
	return &sha, nil
}

func (s *server) deleteMemory(r *http.Request) (any, error) {
	ctx := r.Context()
	storeID, memoryID := r.PathValue("id"), r.PathValue("mid")
	if err := checkID(storeID, "memory store"); err != nil {
		return nil, err
	}
	if err := checkID(memoryID, "memory"); err != nil {
		return nil, err
	}
	// The delete precondition rides the query string, not a body
	// (betamemorystorememory.go:145). storableText, not a digest shape check:
	// any value that is not the stored one is a mismatch, and the only byte
	// that must not reach the comparison is one Postgres cannot store (#135).
	expected := r.URL.Query().Get("expected_content_sha256")
	if !storableText(expected) {
		return nil, errInvalid("expected_content_sha256 must be valid text")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMemoryStoreForWrite(ctx, tx, storeID); err != nil {
		return nil, err
	}
	var row memoryRow
	err = row.scan(tx.QueryRow(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE id = $1 AND memory_store_id = $2 FOR UPDATE`,
		memoryID, storeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("memory %s not found", memoryID)
	}
	if err != nil {
		return nil, err
	}
	if expected != "" && expected != row.sha {
		return nil, errMemoryPrecondition(
			"memory %s has content_sha256 %s, not %s", memoryID, row.sha, expected)
	}
	// The tombstone's own promise — "The memory's version history persists and
	// remains listable … until the store itself is deleted" — is why the
	// versions table carries no foreign key to this row.
	if _, err := insertMemoryVersion(ctx, tx, storeID, memoryID, "deleted",
		row.path, nil, memoryActor(ctx)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memories WHERE id = $1`, memoryID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]string{"id": memoryID, "type": "memory_deleted"}, nil
}

func (s *server) listMemories(r *http.Request) (any, error) {
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
	page, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	// "Capped at 20 when view=full" — silently, as documented, since the
	// caller's own limit is otherwise a legal one.
	if view == viewFull && page.limit > memoryFullViewLimit {
		page.limit = memoryFullViewLimit
	}
	prefix := "/"
	if v := q.Get("path_prefix"); v != "" {
		if !storableText(v) {
			return nil, errInvalid("path_prefix must be valid text")
		}
		prefix = v
	}
	depth := 0
	if v := q.Get("depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 0 && n != 1) {
			return nil, errInvalid("depth must be 0 or 1")
		}
		depth = n
	}
	if err := s.checkMemoryStore(ctx, storeID); err != nil {
		return nil, err
	}

	// One query for both depths (decision 5). The remainder after the prefix
	// decides each row: no "/" in it and the row is a memory at this level;
	// a "/" and — under depth=1 — the row rolls up to the first segment below
	// the prefix, which DISTINCT ON collapses to one memory_prefix item. The
	// prefix match is a literal left(), never LIKE, for occupiedBy's reason.
	//
	// COLLATE "C" is spelled once, on the key the subquery builds, and the
	// outer ORDER BY and cursor comparison inherit it. The column carries the
	// same collation, so the clause restates a derivation Postgres makes on
	// its own; TestMemoryPathsSortUnderTheCCollation pins the column and an
	// expression built over it, not this string. Without the collation a
	// deployment whose default is not byte order would serve the page in a
	// different order than it pages by.
	args := []any{storeID, prefix, depth == 1}
	query := `SELECT DISTINCT ON (key) key, is_prefix, ` + memoryColumns + `
	  FROM (
	    SELECT (CASE WHEN $3::boolean AND position('/' in substr(path, length($2::text) + 1)) > 0
	                 THEN $2::text || substr(path, length($2::text) + 1,
	                                         position('/' in substr(path, length($2::text) + 1)))
	                 ELSE path END) COLLATE "C" AS key,
	           $3::boolean AND position('/' in substr(path, length($2::text) + 1)) > 0 AS is_prefix,
	           ` + memoryColumns + `
	      FROM memories
	     WHERE memory_store_id = $1 AND left(path, length($2::text)) = $2::text
	  ) matched
	 WHERE true`
	if page.cur != nil {
		if !page.cur.pathKeyed || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
		args = append(args, page.cur.path)
		query += fmt.Sprintf(` AND key > $%d`, len(args))
	}
	args = append(args, page.limit+1)
	query += fmt.Sprintf(` ORDER BY key LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []any
	var lastKey string
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		var key string
		var isPrefix bool
		var row memoryRow
		if err := row.scan(rows, &key, &isPrefix); err != nil {
			return nil, err
		}
		if isPrefix {
			data = append(data, memoryPrefixJSON{Type: "memory_prefix", Path: key})
		} else {
			data = append(data, renderMemory(storeID, row, view == viewFull))
		}
		lastKey = key
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := pageJSON{Data: data}
	if out.Data == nil {
		out.Data = []any{}
	}
	if fetched > page.limit {
		c := encodePathCursor(lastKey)
		out.NextPage = &c
	}
	return out, nil
}
