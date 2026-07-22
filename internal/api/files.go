package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// maxFileListLimit is the GET /v1/files per-page cap: the SDK documents limit
// "1 to 1000" (anthropic-sdk-go betafile.go BetaFileListParams), unlike the
// managed-agents resource lists' 100.
const maxFileListLimit = 1000

// fileJSON is the FileMetadata wire shape (anthropic-sdk-go betafile.go:186-221):
// id/created_at/filename/mime_type/size_bytes all api:"required"; type is the
// constant "file"; downloadable a plain bool; scope api:"nullable" — a
// {id, type:"session"} object for files created in a scoping resource's context,
// null for a plain upload. Like the skills registry, the shape is api-local
// (no domain.File) — the registry is metadata-only.
type fileJSON struct {
	ID           string         `json:"id"`
	CreatedAt    time.Time      `json:"created_at"`
	Filename     string         `json:"filename"`
	MimeType     string         `json:"mime_type"`
	SizeBytes    int64          `json:"size_bytes"`
	Type         string         `json:"type"`
	Downloadable bool           `json:"downloadable"`
	Scope        *fileScopeJSON `json:"scope"`
}

// fileScopeJSON is BetaFileScope (betafile.go:133-151): the scoping resource id
// and its type ("session").
type fileScopeJSON struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func renderFile(id, filename, mimeType string, sizeBytes int64, downloadable bool, scopeType, scopeID *string, createdAt time.Time) fileJSON {
	var scope *fileScopeJSON
	if scopeID != nil && scopeType != nil {
		scope = &fileScopeJSON{ID: *scopeID, Type: *scopeType}
	}
	return fileJSON{
		ID: id, CreatedAt: createdAt.UTC(), Filename: filename, MimeType: mimeType,
		SizeBytes: sizeBytes, Type: "file", Downloadable: downloadable, Scope: scope,
	}
}

// errFilesUnavailable answers the storage-backed file routes on a deployment
// configured without object storage.
var errFilesUnavailable = &apiError{http.StatusInternalServerError, errTypeAPI,
	"object storage is not configured on this deployment; files are unavailable"}

// fileBlobKey is the object-storage key layout for a file's bytes — the second
// consumer of the namespace internal/blob reserved for the Files API.
func fileBlobKey(id string) string { return "files/" + id }

// checkFileID rejects a path id that is not a well-formed file_ id with the 404
// an unknown id already gets (checkID's rationale: shape-reject before an
// unstorable byte reaches a bind parameter).
func checkFileID(id string) error {
	if !domain.ID(id).HasPrefix(domain.PrefixFile) || !domain.ID(id).Valid() {
		return errNotFound("file %s not found", id)
	}
	return nil
}

// orphanCleanupTimeout bounds the detached best-effort object delete.
const orphanCleanupTimeout = 5 * time.Second

// deleteOrphanedFile best-effort-removes an object whose database row never
// landed (or just left). A failure here leaves a rare orphaned object, accepted
// and documented in the plan — GC is a non-goal. The cleanup runs on a context
// detached from the request's cancellation (but keeping its trace/log values):
// the most likely commit-failure cause is that very cancellation, and reusing
// the dead context would make the cleanup fail immediately every time. (The
// skills registry's deleteOrphanedObject reuses the request context and has the
// same latent gap; left as-is here, only files is hardened.)
func (s *server) deleteOrphanedFile(ctx context.Context, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orphanCleanupTimeout)
	defer cancel()
	if err := s.blobs.Delete(cleanupCtx, key); err != nil {
		slog.WarnContext(cleanupCtx, "file orphaned in object storage", "key", key, "err", err)
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
	createdAt, err := s.insertFile(ctx, id, up)
	if err != nil {
		recordFileUpload(ctx, fileOutcomeError, 0)
		return nil, err
	}
	recordFileUpload(ctx, fileOutcomeOK, int64(len(up.data)))
	slog.InfoContext(ctx, "file uploaded", "file_id", id, "filename", up.filename,
		"mime_type", up.mimeType, "bytes", len(up.data))
	// A fresh upload is never downloadable and carries no scope (public docs).
	return renderFile(id, up.filename, up.mimeType, int64(len(up.data)), false, nil, nil, createdAt), nil
}

// insertFile lands the metadata row and the object in one transaction with the
// same ordering as insertSkill: the row is claimed in the tx, the blob put
// before commit (the object exists before the row becomes visible — a metadata
// row can never point at a missing object), commit last. The only orphan window
// is a failed commit after a successful put, cleaned best-effort.
func (s *server) insertFile(ctx context.Context, id string, up *fileUpload) (time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable)
		 VALUES ($1, $2, $3, $4, false) RETURNING created_at`,
		id, up.filename, up.mimeType, int64(len(up.data))).Scan(&createdAt); err != nil {
		return time.Time{}, err
	}
	key := fileBlobKey(id)
	if err := s.blobs.Put(ctx, key, bytes.NewReader(up.data), int64(len(up.data)), up.mimeType); err != nil {
		return time.Time{}, fmt.Errorf("store file: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		s.deleteOrphanedFile(ctx, key)
		return time.Time{}, err
	}
	return createdAt, nil
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
	)
	err := s.pool.QueryRow(ctx,
		`SELECT filename, mime_type, size_bytes, downloadable, scope_type, scope_id, created_at
		 FROM files WHERE id = $1`, id).
		Scan(&filename, &mimeType, &sizeBytes, &downloadable, &scopeType, &scopeID, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("file %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return renderFile(id, filename, mimeType, sizeBytes, downloadable, scopeType, scopeID, createdAt), nil
}

func (s *server) listFiles(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	afterID, beforeID := q.Get("after_id"), q.Get("before_id")
	if afterID != "" && beforeID != "" {
		return nil, errInvalid("after_id and before_id are mutually exclusive")
	}
	limit, err := parseFileLimit(q)
	if err != nil {
		return nil, err
	}
	scopeID := q.Get("scope_id")
	// A query-parameter value binds straight into Postgres; an unstorable byte
	// (U+0000, invalid UTF-8) would fail as a 500 rather than filter (see #135).
	// An unknown-but-storable scope_id simply matches nothing.
	if scopeID != "" && !storableText(scopeID) {
		return nil, errInvalid("scope_id must be storable text")
	}

	// Resolve the after_id/before_id cursor to its (created_at, id) keyset
	// position. An unknown cursor id yields an empty page — the reference's
	// behavior here is unrecorded (docs/DIVERGENCES.md).
	cursorID := afterID
	if beforeID != "" {
		cursorID = beforeID
	}
	var curCreatedAt time.Time
	var curID string
	haveCursor := false
	if cursorID != "" {
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

	query := `SELECT id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id, created_at FROM files WHERE true`
	var args []any
	if scopeID != "" {
		args = append(args, scopeID)
		query += fmt.Sprintf(` AND scope_id = $%d`, len(args))
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
		)
		if err := rows.Scan(&id, &filename, &mimeType, &sizeBytes, &downloadable, &scopeType, &sID, &createdAt); err != nil {
			return nil, err
		}
		files = append(files, renderFile(id, filename, mimeType, sizeBytes, downloadable, scopeType, sID, createdAt))
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
	s.deleteOrphanedFile(ctx, fileBlobKey(id))
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
	)
	err := s.pool.QueryRow(ctx,
		`SELECT filename, mime_type, downloadable FROM files WHERE id = $1`, id).
		Scan(&filename, &mimeType, &downloadable)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, errNotFound("file %s not found", id))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !downloadable {
		writeError(w, r, errInvalid(
			"file %s is not downloadable; only files created by skills or the code execution tool can be downloaded", id))
		return
	}
	rc, size, err := s.blobs.Get(ctx, fileBlobKey(id))
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

// parseFileLimit parses the GET /v1/files limit (1–1000, default 20). The
// Files list paginates by object id, not the managed-agents keyset cursor, so
// it does not share parsePage.
func parseFileLimit(q url.Values) (int, error) {
	limit := defaultLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxFileListLimit {
			return 0, errInvalid("limit must be an integer between 1 and %d", maxFileListLimit)
		}
		limit = n
	}
	return limit, nil
}
