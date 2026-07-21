package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// skillJSON is the BetaSkill wire shape (anthropic-sdk-go betaskill.go):
// every field api:"required". latest_version renders as the empty string once
// every version is deleted — the SDK types it as a required plain string, and
// what the reference echoes there is unrecorded (docs/DIVERGENCES.md).
type skillJSON struct {
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
	DisplayTitle  string    `json:"display_title"`
	LatestVersion string    `json:"latest_version"`
	Source        string    `json:"source"`
	Type          string    `json:"type"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func renderSkill(id, displayTitle string, latestVersion *string, source string, createdAt, updatedAt time.Time) skillJSON {
	latest := ""
	if latestVersion != nil {
		latest = *latestVersion
	}
	return skillJSON{
		ID: id, CreatedAt: createdAt.UTC(), DisplayTitle: displayTitle,
		LatestVersion: latest, Source: source, Type: "skill", UpdatedAt: updatedAt.UTC(),
	}
}

// skillVersionJSON is the BetaSkillVersion wire shape
// (anthropic-sdk-go betaskillversion.go).
type skillVersionJSON struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	Description string    `json:"description"`
	Directory   string    `json:"directory"`
	Name        string    `json:"name"`
	SkillID     string    `json:"skill_id"`
	Type        string    `json:"type"`
	Version     string    `json:"version"`
}

// skillShortNameRe is the id shape of the imported anthropic catalog ("xlsx",
// "pdf"): short names, not skill_-prefixed ids. The skills path slots accept
// both spellings.
var skillShortNameRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// checkSkillID rejects a path id that is neither a valid skill_ id nor a
// catalog short name, with the 404 an unknown id already gets (checkID's
// rationale: shape-reject before an unstorable byte reaches a bind parameter).
func checkSkillID(id string) error {
	if domain.ID(id).HasPrefix(domain.PrefixSkill) && domain.ID(id).Valid() {
		return nil
	}
	if skillShortNameRe.MatchString(id) {
		return nil
	}
	return errNotFound("skill %s not found", id)
}

// skillVersionRe is the {version} path slot: the epoch-timestamp string only
// (dates for imported anthropic skills are digits too). Aliases such as
// "latest" are rejected here, matching the documented reference behavior.
var skillVersionRe = regexp.MustCompile(`^[0-9]{1,32}$`)

func checkSkillVersion(v string) error {
	if !skillVersionRe.MatchString(v) {
		return errInvalid("version %q is not a version identifier (aliases such as \"latest\" are not accepted here)", v)
	}
	return nil
}

// skillBlobKey is the object-storage key layout documented in internal/blob,
// shared with the executor's materialization via skills.BlobKey.
func skillBlobKey(skillID, version string) string {
	return skills.BlobKey(skillID, version)
}

// errSkillsUnavailable answers the storage-backed skill routes on a
// deployment configured without object storage.
var errSkillsUnavailable = &apiError{http.StatusInternalServerError, errTypeAPI,
	"object storage is not configured on this deployment; skills are unavailable"}

// mintSkillVersion returns a new Unix-epoch-microseconds version string, the
// reference's version format.
func mintSkillVersion() string {
	return strconv.FormatInt(time.Now().UnixMicro(), 10)
}

// isUniqueViolation reports whether err is a Postgres unique violation on the
// named constraint.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// deleteOrphanedObject best-effort-removes an archive whose database row
// never landed (or just left). A failure here leaves a rare orphaned object,
// accepted and documented in the plan — GC is a non-goal.
func (s *server) deleteOrphanedObject(ctx context.Context, key string) {
	if err := s.blobs.Delete(ctx, key); err != nil {
		slog.WarnContext(ctx, "skill archive orphaned in object storage", "key", key, "err", err)
	}
}

func (s *server) createSkill(r *http.Request) (any, error) {
	ctx := r.Context()
	if s.blobs == nil {
		return nil, errSkillsUnavailable
	}
	up, err := parseSkillUpload(r, true)
	if err != nil {
		recordSkillUpload(ctx, skillOutcomeInvalid, 0)
		return nil, err
	}
	bundle, err := up.bundle()
	if err != nil {
		recordSkillUpload(ctx, skillOutcomeInvalid, 0)
		slog.InfoContext(ctx, "skill upload rejected", "files", len(up.files),
			"bytes", up.totalBytes(), "reason", err)
		return nil, err
	}
	displayTitle := bundle.Name
	if up.displayTitleSet {
		if up.displayTitle == "" || !storableText(up.displayTitle) {
			recordSkillUpload(ctx, skillOutcomeInvalid, 0)
			return nil, errInvalid("display_title must be non-empty storable text")
		}
		displayTitle = up.displayTitle
	}

	id := domain.NewID(domain.PrefixSkill).String()
	version := mintSkillVersion()
	created, err := s.insertSkill(ctx, id, displayTitle, version, bundle)
	if err != nil {
		if isUniqueViolation(err, "skills_custom_display_title_uq") {
			recordSkillUpload(ctx, skillOutcomeInvalid, 0)
			return nil, errInvalid("display_title %q is already used by another custom skill", displayTitle)
		}
		recordSkillUpload(ctx, skillOutcomeError, 0)
		return nil, err
	}
	recordSkillUpload(ctx, skillOutcomeOK, int64(len(bundle.Zip)))
	slog.InfoContext(ctx, "skill created", "skill_id", id, "version", version,
		"files", len(up.files), "bytes", len(bundle.Zip))
	return renderSkill(id, displayTitle, &version, "custom", created, created), nil
}

// insertSkill lands the skill row, its first version, and the archive in one
// transaction: rows first (a display_title conflict rejects before any
// storage traffic), the blob put before commit (the object exists before the
// rows become visible — a version row can never dangle), commit last. The
// only orphan window is a failed commit after a successful put, cleaned
// best-effort.
func (s *server) insertSkill(ctx context.Context, id, displayTitle, version string, bundle *skills.Bundle) (time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version)
		 VALUES ($1, 'custom', $2, $3) RETURNING created_at`,
		id, displayTitle, version).Scan(&createdAt); err != nil {
		return time.Time{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		domain.NewID(domain.PrefixSkillVersion).String(), id, version,
		bundle.Name, bundle.Description, bundle.Directory); err != nil {
		return time.Time{}, err
	}
	key := skillBlobKey(id, version)
	if err := s.blobs.Put(ctx, key, bytes.NewReader(bundle.Zip), int64(len(bundle.Zip)), "application/zip"); err != nil {
		return time.Time{}, fmt.Errorf("store skill archive: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		s.deleteOrphanedObject(ctx, key)
		return time.Time{}, err
	}
	return createdAt, nil
}

func (s *server) getSkill(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	var (
		displayTitle, source string
		latestVersion        *string
		createdAt, updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT display_title, latest_version, source, created_at, updated_at FROM skills WHERE id = $1`, id).
		Scan(&displayTitle, &latestVersion, &source, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("skill %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return renderSkill(id, displayTitle, latestVersion, source, createdAt, updatedAt), nil
}

func (s *server) listSkills(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	page, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	source := q.Get("source")
	if source != "" && source != "custom" && source != "anthropic" {
		return nil, errInvalid(`source must be "custom" or "anthropic"`)
	}

	query := `SELECT id, display_title, latest_version, source, created_at, updated_at FROM skills WHERE true`
	var args []any
	if source != "" {
		args = append(args, source)
		query += fmt.Sprintf(` AND source = $%d`, len(args))
	}
	if page.cur != nil {
		if page.cur.versioned || page.cur.seqKeyed || page.cur.dir != dirNext {
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

	var data []any
	var lastT time.Time
	var lastID string
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		var (
			id, displayTitle, src string
			latestVersion         *string
			createdAt, updatedAt  time.Time
		)
		if err := rows.Scan(&id, &displayTitle, &latestVersion, &src, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		data = append(data, renderSkill(id, displayTitle, latestVersion, src, createdAt, updatedAt))
		lastT, lastID = createdAt, id
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

func (s *server) deleteSkill(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	// The imported anthropic catalog is not API-manageable (its versions
	// already refuse create); an accidental DELETE must not empty it.
	var source string
	err := s.pool.QueryRow(ctx, `SELECT source FROM skills WHERE id = $1`, id).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("skill %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if source != "custom" {
		return nil, errInvalid("anthropic skills are managed by the platform, not this API")
	}
	var versions int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM skill_versions WHERE skill_id = $1`, id).Scan(&versions); err != nil {
		return nil, err
	}
	if versions > 0 {
		return nil, errInvalid("cannot delete skill %s: %d version(s) still exist; delete every version first", id, versions)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM skills WHERE id = $1`, id)
	if err != nil {
		// A version created between the count and the delete trips the FK
		// (23503): same client answer as the counted case.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return nil, errInvalid("cannot delete skill %s: version(s) still exist; delete every version first", id)
		}
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("skill %s not found", id)
	}
	slog.InfoContext(ctx, "skill deleted", "skill_id", id)
	return map[string]string{"id": id, "type": "skill_deleted"}, nil
}

func (s *server) createSkillVersion(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	if s.blobs == nil {
		return nil, errSkillsUnavailable
	}
	// Resolve the skill before touching the (potentially large) body.
	var source string
	err := s.pool.QueryRow(ctx, `SELECT source FROM skills WHERE id = $1`, id).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("skill %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if source != "custom" {
		return nil, errInvalid("versions of anthropic skills are managed by the platform, not this API")
	}
	up, err := parseSkillUpload(r, false)
	if err != nil {
		recordSkillUpload(ctx, skillOutcomeInvalid, 0)
		return nil, err
	}
	bundle, err := up.bundle()
	if err != nil {
		recordSkillUpload(ctx, skillOutcomeInvalid, 0)
		slog.InfoContext(ctx, "skill upload rejected", "skill_id", id,
			"files", len(up.files), "bytes", up.totalBytes(), "reason", err)
		return nil, err
	}

	version := mintSkillVersion()
	vid := domain.NewID(domain.PrefixSkillVersion).String()
	createdAt, err := s.insertSkillVersion(ctx, id, vid, version, bundle)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound("skill %s not found", id)
		}
		if isUniqueViolation(err, "skill_versions_skill_id_version_key") {
			// The row was claimed before any storage traffic, so a
			// same-microsecond loser cannot touch the winner's archive.
			return nil, errConflict("a version with the same identifier was minted concurrently; retry")
		}
		recordSkillUpload(ctx, skillOutcomeError, 0)
		return nil, err
	}
	recordSkillUpload(ctx, skillOutcomeOK, int64(len(bundle.Zip)))
	slog.InfoContext(ctx, "skill version created", "skill_id", id, "version", version,
		"files", len(up.files), "bytes", len(bundle.Zip))
	return skillVersionJSON{
		ID: vid, CreatedAt: createdAt.UTC(), Description: bundle.Description,
		Directory: bundle.Directory, Name: bundle.Name, SkillID: id,
		Type: "skill_version", Version: version,
	}, nil
}

// insertSkillVersion lands the version row and its archive with the same
// ordering as insertSkill: parent row locked (serializing the latest_version
// maintenance against concurrent creates and deletes), row claimed before the
// blob put — so a same-microsecond version collision 409s without any storage
// traffic and can never overwrite or delete the winner's archive — put before
// commit, commit last.
func (s *server) insertSkillVersion(ctx context.Context, id, vid, version string, bundle *skills.Bundle) (time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var have string
	if err := tx.QueryRow(ctx, `SELECT id FROM skills WHERE id = $1 FOR UPDATE`, id).Scan(&have); err != nil {
		return time.Time{}, err // pgx.ErrNoRows when the skill vanished
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
		vid, id, version, bundle.Name, bundle.Description, bundle.Directory).Scan(&createdAt); err != nil {
		return time.Time{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE skills SET latest_version = $2, updated_at = now() WHERE id = $1`,
		id, version); err != nil {
		return time.Time{}, err
	}
	key := skillBlobKey(id, version)
	if err := s.blobs.Put(ctx, key, bytes.NewReader(bundle.Zip), int64(len(bundle.Zip)), "application/zip"); err != nil {
		return time.Time{}, fmt.Errorf("store skill archive: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		s.deleteOrphanedObject(ctx, key)
		return time.Time{}, err
	}
	return createdAt, nil
}

func (s *server) listSkillVersions(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	// The versions list's documented cap is 1000, like the session-events
	// list — not the resource lists' 100.
	page, err := parsePageMax(r.URL.Query(), maxEventLimit)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM skills WHERE id = $1)`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errNotFound("skill %s not found", id)
	}

	query := `SELECT id, version, name, description, directory, created_at FROM skill_versions WHERE skill_id = $1`
	args := []any{id}
	if page.cur != nil {
		if page.cur.versioned || page.cur.seqKeyed || page.cur.dir != dirNext {
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

	var data []any
	var lastT time.Time
	var lastID string
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > page.limit {
			break
		}
		var (
			vid, version, name, description, directory string
			createdAt                                  time.Time
		)
		if err := rows.Scan(&vid, &version, &name, &description, &directory, &createdAt); err != nil {
			return nil, err
		}
		data = append(data, skillVersionJSON{
			ID: vid, CreatedAt: createdAt.UTC(), Description: description,
			Directory: directory, Name: name, SkillID: id, Type: "skill_version", Version: version,
		})
		lastT, lastID = createdAt, vid
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

func (s *server) getSkillVersion(r *http.Request) (any, error) {
	ctx := r.Context()
	id, version := r.PathValue("id"), r.PathValue("version")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	if err := checkSkillVersion(version); err != nil {
		return nil, err
	}
	var (
		vid, name, description, directory string
		createdAt                         time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, description, directory, created_at FROM skill_versions
		 WHERE skill_id = $1 AND version = $2`, id, version).
		Scan(&vid, &name, &description, &directory, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("skill %s version %s not found", id, version)
	}
	if err != nil {
		return nil, err
	}
	return skillVersionJSON{
		ID: vid, CreatedAt: createdAt.UTC(), Description: description,
		Directory: directory, Name: name, SkillID: id, Type: "skill_version", Version: version,
	}, nil
}

func (s *server) deleteSkillVersion(r *http.Request) (any, error) {
	ctx := r.Context()
	id, version := r.PathValue("id"), r.PathValue("version")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	if err := checkSkillVersion(version); err != nil {
		return nil, err
	}
	if s.blobs == nil {
		return nil, errSkillsUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock the parent row before touching versions, mirroring
	// insertSkillVersion: without it, a delete blocked behind a concurrent
	// version create would recompute latest_version on a pre-create snapshot
	// (READ COMMITTED evaluates the subquery against the statement snapshot)
	// and could blank latest_version while a live version exists.
	var source string
	if err := tx.QueryRow(ctx, `SELECT source FROM skills WHERE id = $1 FOR UPDATE`, id).Scan(&source); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound("skill %s version %s not found", id, version)
		}
		return nil, err
	}
	if source != "custom" {
		return nil, errInvalid("versions of anthropic skills are managed by the platform, not this API")
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM skill_versions WHERE skill_id = $1 AND version = $2`, id, version)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("skill %s version %s not found", id, version)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE skills SET latest_version = (
		    SELECT version FROM skill_versions WHERE skill_id = $1
		    ORDER BY created_at DESC, id DESC LIMIT 1
		 ), updated_at = now() WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// The row is gone; the archive follows best-effort (plan: rare orphans
	// are accepted, GC is a non-goal).
	s.deleteOrphanedObject(ctx, skillBlobKey(id, version))
	slog.InfoContext(ctx, "skill version deleted", "skill_id", id, "version", version)
	// The wire's asymmetry, reproduced deliberately: the deleted object's id
	// is the version timestamp, not the skillver_ id.
	return map[string]string{"id": version, "type": "skill_version_deleted"}, nil
}

// downloadSkillVersion streams the stored archive. Not a typed handler: the
// body is the object, not JSON.
func (s *server) downloadSkillVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, version := r.PathValue("id"), r.PathValue("version")
	if err := checkSkillID(id); err != nil {
		writeError(w, r, err)
		return
	}
	if err := checkSkillVersion(version); err != nil {
		writeError(w, r, err)
		return
	}
	if s.blobs == nil {
		writeError(w, r, errSkillsUnavailable)
		return
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM skill_versions WHERE skill_id = $1 AND version = $2)`,
		id, version).Scan(&exists); err != nil {
		writeError(w, r, err)
		return
	}
	if !exists {
		writeError(w, r, errNotFound("skill %s version %s not found", id, version))
		return
	}
	rc, size, err := s.blobs.Get(ctx, skillBlobKey(id, version))
	if err != nil {
		// A version row whose object is gone is an operator incident, not a
		// client 404: report it as such and say so in the logs.
		slog.ErrorContext(ctx, "skill archive missing from object storage",
			"skill_id", id, "version", version, "err", err)
		writeError(w, r, fmt.Errorf("read skill archive: %w", err))
		return
	}
	defer rc.Close()
	// Response headers are an inference (docs/DIVERGENCES.md): the SDK sends
	// Accept: application/binary and treats the body as opaque bytes.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are gone; nothing to do but log the broken stream.
		slog.WarnContext(ctx, "skill archive download interrupted",
			"skill_id", id, "version", version, "err", err)
		return
	}
	recordSkillDownload(ctx, size)
	slog.DebugContext(ctx, "skill archive downloaded", "skill_id", id, "version", version, "bytes", size)
}
