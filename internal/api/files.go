package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
)

// maxFileListLimit is the GET /v1/files per-page cap: the SDK documents limit
// "1 to 1000" (anthropic-sdk-go betafile.go BetaFileListParams), unlike the
// managed-agents resource lists' 100.
const maxFileListLimit = 1000

// maxFileListIDs bounds ?ids[], counted on the de-duplicated set the way the
// docs count it — "at most 100 entries (after de-duplication)".
const maxFileListIDs = 100

// fileJSON is the BetaFileMetadata wire shape (anthropic-sdk-go betafile.go:178-218):
// id/created_at/filename/mime_type/size_bytes all api:"required"; type is the
// constant "file"; downloadable a plain bool.
//
// The last two fields are both api:"nullable", which says the value may be
// null and nothing about whether the server sends the key at all. The SDK does
// answer that, but at runtime rather than in the tags — respjson.Field.Raw()
// reads "null" for a null and "" for an omitted key — so the schema alone
// cannot decide which of these two we owe. The recorded bytes can, and they
// split: all eight file objects in the archive carry expires_at, and the six
// without a scope carry no scope key. Those readings check each other, because
// the six that omit scope still spell out expires_at: null — so the absence is
// the reference's and not the recorder's.
//
// The two halves are not evidenced equally, and the asymmetry is worth keeping
// in view. The omission is broad: six objects across upload, get and list, on
// the bare path and ?beta=true alike. The presence is one file read twice on
// GET /v1/files/{id}, bare path — no recorded list entry and no ?beta=true
// response ever held a scoped file. We render scope on every surface anyway,
// because renderFile is the only renderer and the reference gives no reason to
// think a lane strips it.
//
// omitempty is not what enforces "only when it has one" — it omits a nil
// pointer and nothing else, so a non-nil pointer to a zero-value scope would
// still marshal (as would omitzero: for a pointer the two behave alike).
// renderFile's both-non-nil guard is the actual contract; the tag only carries
// out what it decides.
//
// expires_at is the upload time plus expires_in_seconds, null when the upload
// asked for no lifetime (#655, plan 49). It is sent either way: the key is
// always present, and null is the value that says "does not expire". Like the
// skills registry, the shape is api-local (no domain.File) — the registry is
// metadata-only.
type fileJSON struct {
	ID           string         `json:"id"`
	CreatedAt    time.Time      `json:"created_at"`
	Filename     string         `json:"filename"`
	MimeType     string         `json:"mime_type"`
	SizeBytes    int64          `json:"size_bytes"`
	Type         string         `json:"type"`
	Downloadable bool           `json:"downloadable"`
	ExpiresAt    *time.Time     `json:"expires_at"`
	Scope        *fileScopeJSON `json:"scope,omitempty"`
}

// fileScopeJSON is BetaFileScope (betafile.go:226-237): the scoping resource id
// and its type ("session").
type fileScopeJSON struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func renderFile(id, filename, mimeType string, sizeBytes int64, downloadable bool, scopeType, scopeID *string, createdAt time.Time, expiresAt *time.Time) fileJSON {
	var scope *fileScopeJSON
	// Both-or-neither is enforced in the schema since 0036, so this is a
	// restatement rather than the only thing holding it (#659). It still has to
	// ask, because it is building a pointer.
	if scopeID != nil && scopeType != nil {
		scope = &fileScopeJSON{ID: *scopeID, Type: *scopeType}
	}
	if expiresAt != nil {
		utc := expiresAt.UTC()
		expiresAt = &utc
	}
	return fileJSON{
		ID: id, CreatedAt: createdAt.UTC(), Filename: filename, MimeType: mimeType,
		SizeBytes: sizeBytes, Type: "file", Downloadable: downloadable,
		ExpiresAt: expiresAt, Scope: scope,
	}
}

// errFilesUnavailable answers the storage-backed file routes on a deployment
// configured without object storage.
var errFilesUnavailable = &apiError{http.StatusInternalServerError, errTypeAPI,
	"object storage is not configured on this deployment; files are unavailable"}

// checkFileID rejects a path id that is not a well-formed file_ id with the 404
// an unknown id already gets (checkID's rationale: shape-reject before an
// unstorable byte reaches a bind parameter).
func checkFileID(id string) error {
	if !domain.ID(id).HasPrefix(domain.PrefixFile) || !domain.ID(id).Valid() {
		return errNotFound("file %s not found", id)
	}
	return nil
}

// deleteOrphanedFile best-effort-removes an object whose database row never
// landed (or just left). A failure here leaves a rare orphaned object, accepted
// and documented in the plan — GC is a non-goal.
//
// That is still true, and the expired-file sweep beside it (fileretention.go)
// is not the exception it looks like: this note is about objects whose row is
// gone, which nothing can enumerate, while the sweep removes objects their own
// row names, on a lifecycle the client asked for at upload.
//
// deleteOrphanedFile deliberately runs on the request context (like the skills
// registry's deleteOrphanedObject): when
// insertFile's commit fails ambiguously — a cancelled or dropped context, where
// Postgres may in fact have committed — that same cancelled context makes this
// delete a no-op, so a possibly-live object is preserved rather than deleted out
// from under a committed row. A detached context would "fix" the benign orphan
// leak at the cost of that data-loss risk; preserving the object is the correct
// trade (a definite commit rejection leaves the context live, so the orphan is
// still cleaned).
func (s *server) deleteOrphanedFile(ctx context.Context, key string) {
	if err := s.blobs.Delete(ctx, key); err != nil {
		slog.WarnContext(ctx, "file orphaned in object storage", "key", key, "err", err)
	}
}

func (s *server) createFile(r *http.Request) (any, error) {
	ctx := r.Context()
	if s.blobs == nil {
		return nil, errFilesUnavailable
	}
	up, err := parseFileUpload(r)
	if err != nil {
		recordFileUpload(ctx, fileOutcomeInvalid, 0)
		slog.InfoContext(ctx, "file upload rejected", "reason", err)
		return nil, err
	}
	id := domain.NewID(domain.PrefixFile).String()
	createdAt, expiresAt, err := s.insertFile(ctx, id, up)
	if err != nil {
		recordFileUpload(ctx, fileOutcomeError, 0)
		return nil, err
	}
	recordFileUpload(ctx, fileOutcomeOK, int64(len(up.data)))
	slog.InfoContext(ctx, "file uploaded", "file_id", id, "filename", up.filename,
		"mime_type", up.mimeType, "bytes", len(up.data))
	// A fresh upload is never downloadable and carries no scope (public docs).
	return renderFile(id, up.filename, up.mimeType, int64(len(up.data)), false, nil, nil, createdAt, expiresAt), nil
}

// insertFile lands the metadata row and the object in one transaction with the
// same ordering as insertSkill: the row is claimed in the tx, the blob put
// before commit (the object exists before the row becomes visible — a metadata
// row can never point at a missing object), commit last. The only orphan window
// is a failed commit after a successful put, cleaned best-effort.
//
// expires_at is computed by Postgres from the same now() that defaults
// created_at, so the two are the one instant the docs describe — "the upload
// time plus that value" — rather than two clocks agreeing to the millisecond.
// An upload that requested no lifetime needs no branch: make_interval is STRICT
// (pg_proc.proisstrict), so a NULL argument makes a NULL interval and now()
// plus NULL is NULL, which is exactly "does not expire".
func (s *server) insertFile(ctx context.Context, id string, up *fileUpload) (time.Time, *time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var createdAt time.Time
	var expiresAt *time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, expires_at)
		 VALUES ($1, $2, $3, $4, false, now() + make_interval(secs => $5::bigint))
		 RETURNING created_at, expires_at`,
		id, up.filename, up.mimeType, int64(len(up.data)), up.expiresIn).
		Scan(&createdAt, &expiresAt); err != nil {
		return time.Time{}, nil, err
	}
	key := blob.FilesKey(id)
	if err := s.blobs.Put(ctx, key, bytes.NewReader(up.data), int64(len(up.data)), up.mimeType); err != nil {
		return time.Time{}, nil, fmt.Errorf("store file: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		s.deleteOrphanedFile(ctx, key)
		return time.Time{}, nil, err
	}
	return createdAt, expiresAt, nil
}

func (s *server) getFile(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkFileID(id); err != nil {
		return nil, err
	}
	var (
		filename, mimeType string
		sizeBytes          int64
		downloadable       bool
		scopeType, scopeID *string
		createdAt          time.Time
		expiresAt          *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT filename, mime_type, size_bytes, downloadable, scope_type, scope_id, created_at, expires_at
		 FROM files WHERE id = $1`, id).
		Scan(&filename, &mimeType, &sizeBytes, &downloadable, &scopeType, &scopeID, &createdAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("file %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return renderFile(id, filename, mimeType, sizeBytes, downloadable, scopeType, scopeID, createdAt, expiresAt), nil
}

func (s *server) listFiles(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	afterID, beforeID := q.Get("after_id"), q.Get("before_id")
	if afterID != "" && beforeID != "" {
		return nil, errInvalid("after_id and before_id are mutually exclusive")
	}

	// ?ids[] restricts the result set to the files named. listParam reads both
	// wire spellings; the bracketed one is what the SDK sends, since
	// BetaFileListParams.URLQuery pins ArrayQueryFormatBrackets and that encoder
	// appends "[]" to the key once per element.
	//
	// An empty value is not an id, so ?ids= reads as no filter at all rather than
	// as a filter matching nothing — the same way every other empty parameter on
	// this route reads.
	idsParam := listParam(q, "ids")
	idsSupplied := false
	for _, id := range idsParam {
		if id != "" {
			idsSupplied = true
			break
		}
	}
	var ids []string
	if idsSupplied {
		// "Mutually exclusive with page and limit", and the docs name the id
		// cursors in the same breath: before_id/after_id are "not combinable with
		// page or ids[]". Ask whether a value was sent rather than reading the
		// first one: parsePageWith cannot tell a defaulted limit from a sent one,
		// and q.Get would let ?limit=&limit=5 past a guard that ?limit=5 trips.
		sent := func(key string) bool {
			for _, v := range q[key] {
				if v != "" {
					return true
				}
			}
			return false
		}
		if sent("page") || sent("limit") || sent("after_id") || sent("before_id") {
			return nil, errInvalid("ids is not combinable with page, limit, after_id or before_id")
		}
		// The cap is checked as the set grows, so a caller sending far more
		// entries than it allows pays for the rejection rather than for all of
		// them. It counts entries the caller sent, de-duplicated — which is what
		// the docs bound — so a malformed entry still consumes cap, and 100 good
		// ids plus one typo is a 400 rather than a silently-shortened page.
		seen := make(map[string]bool, min(len(idsParam), maxFileListIDs+1))
		for _, id := range idsParam {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			if len(seen) > maxFileListIDs {
				return nil, errInvalid("ids accepts at most %d entries", maxFileListIDs)
			}
			// A malformed id is dropped rather than rejected. The scalar filters
			// beside this one 400 on a bad shape (#135's "shape first"), but this
			// parameter is documented to tolerate misses — "IDs that do not resolve
			// to a visible File — including deleted Files — are silently omitted" —
			// and a malformed id resolves to no visible file by definition.
			//
			// Valid is the half that carries weight: it is what keeps an unstorable
			// byte out of the bind parameter, which #135 requires whatever the
			// status code would be. The prefix half changes no response — a
			// well-formed id of another resource simply matches no file row — and
			// is here to say what this parameter takes, matching checkFileID's
			// identical pair.
			if !domain.ID(id).HasPrefix(domain.PrefixFile) || !domain.ID(id).Valid() {
				continue
			}
			ids = append(ids, id)
		}
	}
	// The shared parser reads ?page= and ?limit= together, so this list inherits
	// the keyset cursor's guards — the grammar check and #135's unstorable-id
	// reject — rather than growing a second spelling of them. Its limit bounds
	// are this route's own (1–1000, default 20), not the resource lists' 100.
	page, err := parsePageWith(q, defaultLimit, maxFileListLimit)
	if err != nil {
		return nil, err
	}
	limit := page.limit
	if page.cur != nil {
		if afterID != "" || beforeID != "" {
			return nil, errInvalid("page and after_id/before_id are mutually exclusive")
		}
		// Unidirectional list: only forward time cursors are valid here (#534).
		if page.cur.foreignToTime() || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
	}
	scopeID := q.Get("scope_id")
	// A query-parameter value binds straight into Postgres; an unstorable byte
	// (U+0000, invalid UTF-8) would fail as a 500 rather than filter (see #135).
	// An unknown-but-storable scope_id simply matches nothing.
	if scopeID != "" && !storableText(scopeID) {
		return nil, errInvalid("scope_id must be storable text")
	}

	// Resolve the after_id/before_id cursor to its (created_at, id) keyset
	// position — the position a ?page= cursor already carries, which is why that
	// lane needs no lookup and cannot 404. An unknown cursor id yields an empty
	// page — the reference's behavior here is unrecorded (docs/DIVERGENCES.md).
	cursorID := afterID
	if beforeID != "" {
		cursorID = beforeID
	}
	var curCreatedAt time.Time
	var curID string
	haveCursor := false
	if page.cur != nil {
		curCreatedAt, curID, haveCursor = page.cur.t, page.cur.id, true
	} else if cursorID != "" {
		if !storableText(cursorID) {
			return nil, errInvalid("invalid page cursor")
		}
		err := s.pool.QueryRow(ctx, `SELECT created_at, id FROM files WHERE id = $1`, cursorID).
			Scan(&curCreatedAt, &curID)
		if errors.Is(err, pgx.ErrNoRows) {
			return filePageJSON{Data: []any{}}, nil
		}
		if err != nil {
			return nil, err
		}
		haveCursor = true
	}

	// Every entry dropped is a filter that can match nothing, so answer it here
	// rather than asking Postgres to agree. Leaving it to the query would work —
	// pgx binds a nil slice as a NULL array and `id = ANY(NULL)` is NULL for every
	// row — but that makes the empty page depend on the driver's nil mapping and
	// on three-valued logic, where the terminal-page argument below ("at most
	// len(ids) rows can match") says nothing at all for a set of size zero.
	if idsSupplied && len(ids) == 0 {
		return filePageJSON{Data: []any{}}, nil
	}

	query := `SELECT id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id, created_at, expires_at FROM files WHERE true`
	var args []any
	if scopeID != "" {
		args = append(args, scopeID)
		query += fmt.Sprintf(` AND scope_id = $%d`, len(args))
	}
	if idsSupplied {
		// scope_id is not on the exclusivity list, so the two filters intersect.
		// The page size becomes the set's own size, which is what makes this page
		// terminal without a second rule: id is the primary key and the set is
		// de-duplicated, so at most len(ids) rows can match, the limit+1 probe
		// below never sees an extra, has_more is false, and the next_page branch
		// mints nothing — "the response is always a single page". The empty set is
		// not an exception to that argument but a case it cannot speak to, which
		// is why it returned above instead.
		args = append(args, ids)
		query += fmt.Sprintf(` AND id = ANY($%d)`, len(args))
		limit = len(ids)
	}
	// Default and after_id fetch newest-first; before_id fetches the nearest
	// newer rows ascending, reversed to newest-first before rendering.
	orderDir, reversed := "DESC", false
	if haveCursor {
		args = append(args, curCreatedAt, curID)
		if beforeID != "" {
			query += fmt.Sprintf(` AND (created_at, id) > ($%d, $%d)`, len(args)-1, len(args))
			orderDir, reversed = "ASC", true
		} else {
			query += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
		}
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(` ORDER BY created_at %s, id %s LIMIT $%d`, orderDir, orderDir, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []fileJSON
	for rows.Next() {
		var (
			id, filename, mimeType string
			sizeBytes              int64
			downloadable           bool
			scopeType, sID         *string
			createdAt              time.Time
			expiresAt              *time.Time
		)
		if err := rows.Scan(&id, &filename, &mimeType, &sizeBytes, &downloadable, &scopeType, &sID, &createdAt, &expiresAt); err != nil {
			return nil, err
		}
		files = append(files, renderFile(id, filename, mimeType, sizeBytes, downloadable, scopeType, sID, createdAt, expiresAt))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasMore := len(files) > limit
	if hasMore {
		files = files[:limit]
	}
	if reversed {
		for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
			files[i], files[j] = files[j], files[i]
		}
	}

	out := filePageJSON{Data: make([]any, 0, len(files)), HasMore: hasMore}
	for _, f := range files {
		out.Data = append(out.Data, f)
	}
	if len(files) > 0 {
		first, last := files[0].ID, files[len(files)-1].ID
		out.FirstID, out.LastID = &first, &last
		// next_page is the position after this page in the list's own
		// newest-first order — "an opaque page cursor returned in a prior list
		// response's next_page", passed back as ?page= (SDK v1.70.1 betafile.go
		// BetaFileListParams.Page). One meaning on every arm, so the two lanes
		// read as one walk: on a forward page null is exactly has_more, while a
		// non-empty before_id page always carries a cursor, continuing into the
		// row its own cursor named — which is why has_more, answering whether
		// rows remain the way that page was fetched, does not decide it there.
		// Under scope_id that continuation can be one further request that comes
		// back empty, the boundary row being resolved unfiltered.
		//
		// An empty page still sends the key, null: this branch is inside
		// len(files) > 0, so no value is minted, not that none is sent — the
		// reference's own empty page is next_page:null too. That reads as
		// end-of-list on the one arm where it is
		// not: an empty before_id page sits at the TOP of the list. No cursor
		// client can act on the difference either way, because
		// pagination.PageCursor.GetNextPage stops on an empty data array before
		// it looks at the cursor at all.
		//
		// A cursor handed out on an id-seeded page cannot be replayed the way
		// that pager replays one: GetNextPage clones the seed request and only
		// adds ?page=, so a client that seeded with after_id sends
		// after_id=…&page=… and meets the refusal above. That is the reference's
		// shape too — it sends next_page on every page while refusing the same
		// combination — so the cursor is emitted here rather than withheld on
		// the arms a legacy client seeds.
		if hasMore || beforeID != "" {
			c := encodeTimeCursor(dirNext, files[len(files)-1].CreatedAt, last)
			out.NextPage = &c
		}
	}
	return out, nil
}

func (s *server) deleteFile(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkFileID(id); err != nil {
		return nil, err
	}
	if s.blobs == nil {
		return nil, errFilesUnavailable
	}
	// A dream's transcript rows are the runner's while the dream is open (plan
	// 41 §4.5): the executor tolerates a deleted mount rather than failing the
	// run, so a caller could otherwise blank a transcript mid-dream. The
	// closing arm deletes them itself. A dream that closes between this read
	// and the delete below simply lets the delete through, which is the right
	// answer either way.
	var dreamID *string
	err := s.pool.QueryRow(ctx,
		`SELECT d.id FROM files f
		   LEFT JOIN dreams d ON d.id = f.dream_id AND d.closed_at IS NULL
		  WHERE f.id = $1`, id).Scan(&dreamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("file %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if dreamID != nil {
		return nil, errInvalid("file %s is owned by dream %s", id, *dreamID)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM files WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("file %s not found", id)
	}
	// The row is gone; the object follows best-effort (rare orphans accepted,
	// GC a non-goal). A deleted file cannot be recovered — the reference has no
	// file archival (unlike sessions).
	s.deleteOrphanedFile(ctx, blob.FilesKey(id))
	slog.InfoContext(ctx, "file deleted", "file_id", id)
	return map[string]string{"id": id, "type": "file_deleted"}, nil
}

// downloadFile streams a file's bytes. Not a typed handler: the body is the
// object, not JSON. Uploaded files carry downloadable=false and are refused with
// the reference's 400 — only files created by skills or the code execution tool
// are downloadable, none of which this slice produces.
func (s *server) downloadFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkFileID(id); err != nil {
		writeError(w, r, err)
		return
	}
	if s.blobs == nil {
		writeError(w, r, errFilesUnavailable)
		return
	}
	var (
		filename, mimeType string
		downloadable       bool
		expired            bool
	)
	// Postgres answers whether the file has expired, rather than this process
	// comparing a scanned timestamp: expires_at was computed from the database's
	// now() at upload, and no replica's clock may decide when it arrives.
	err := s.pool.QueryRow(ctx,
		`SELECT filename, mime_type, downloadable, NOT `+store.FileLiveSQL+`
		   FROM files WHERE id = $1`, id).
		Scan(&filename, &mimeType, &downloadable, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, errNotFound("file %s not found", id))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	// "Downloading its content (GET /v1/files/{file_id}/content) returns a 404
	// error" once expires_at has passed (public docs, plan 49). It is answered
	// ahead of both gates below, so the two lanes agree about a file that no
	// longer has content: without this, the management lane would keep saying
	// "not downloadable" and the worker lane would keep serving bytes the wire
	// contract says are gone. That the reference orders its own checks this way
	// is unrecorded — the docs give the status and not the precedence
	// (docs/DIVERGENCES.md).
	//
	// The metadata route and the list deliberately do not gain this check: an
	// expired file keeps answering there for the documented 30 days, with
	// expires_at in the past, until the retention sweep removes it.
	//
	// "Before both gates" means the two that decide whether this caller may have
	// this file. The storage-availability 500 above is prior to all three and
	// stays there: it answers that the deployment serves no content at all,
	// which is true of a live file and an expired one alike.
	if expired {
		writeError(w, r, errNotFound("file %s not found", id))
		return
	}
	// Lane-aware authorization. On the worker environment-key lane, a mount's
	// bytes must be readable regardless of the downloadable flag (a user upload is
	// downloadable=false), but the key is scoped to files a session in its own
	// environment actually mounts — file content can be sensitive, unlike
	// workspace-global skills (decision 10). A file no session in the env mounts is
	// answered as absent (404), so a leaked environment key can neither read
	// arbitrary files nor probe their existence across environments. On the
	// management x-api-key lane (env == ""), the reference's downloadable gate
	// stands and there is no session scoping.
	if env := environmentFrom(ctx); env != "" {
		if !s.fileMountedInEnvironment(ctx, env, id) {
			writeError(w, r, errNotFound("file %s not found", id))
			return
		}
	} else if !downloadable {
		writeError(w, r, errInvalid(
			"file %s is not downloadable; only files created by skills or the code execution tool can be downloaded", id))
		return
	}
	rc, size, err := s.blobs.Get(ctx, blob.FilesKey(id))
	if err != nil {
		// A row whose object is gone is an operator incident, not a client 404.
		slog.ErrorContext(ctx, "file missing from object storage", "file_id", id, "err", err)
		writeError(w, r, fmt.Errorf("read file: %w", err))
		return
	}
	defer rc.Close()
	// Content-Disposition carries the original filename so the CLI names the
	// local file (anthropic-cli cmdutil.go); the exact header shape is an
	// inference (docs/DIVERGENCES.md).
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already sent; nothing to do but log the broken stream.
		slog.WarnContext(ctx, "file download interrupted", "file_id", id, "err", err)
		return
	}
	recordFileDownload(ctx, size)
	slog.DebugContext(ctx, "file downloaded", "file_id", id, "bytes", size)
}

// fileMountedInEnvironment reports whether some session in the environment
// references the file in its resources[] — the environment-scope check that gates
// the worker environment-key download lane (decision 10). One jsonb containment
// lookup on `sessions.resources`, filtered on environment_id (the scope is the
// environment, not the one session a worker is servicing). A store error reads
// as "not mounted": the lane fails closed to a 404, so a transient database blip
// denies rather than leaks.
func (s *server) fileMountedInEnvironment(ctx context.Context, envID, fileID string) bool {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions
		   WHERE environment_id = $1
		     AND resources @> jsonb_build_array(jsonb_build_object('file_id', $2::text)))`,
		envID, fileID).Scan(&exists); err != nil {
		slog.WarnContext(ctx, "file mount scope check failed",
			"file_id", fileID, "environment_id", envID, "err", err)
		return false
	}
	return exists
}
