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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// skillJSON is the GA Skill wire shape — exactly seven keys, every one
// api:"required" (plan 39 decision 2, from the 2026-09-04 recording). The
// pre-GA spellings are retired rather than served alongside: display_title
// became display_name, the bare `source` string became an object, and the
// numeric latest_version became latest_version_id holding the version row's id.
//
// latest_version_id is documented "Always set", and decision 6 keeps it so from
// here on: a skill can no longer reach zero versions through the API. A
// database upgraded from before it can still hold one, deleting the only
// version having been allowed then and 0033 dropping an index rather than
// rewriting rows; such a skill renders the empty string, and adding a version
// is the way back.
type skillJSON struct {
	Type            string          `json:"type"`
	ID              string          `json:"id"`
	DisplayName     string          `json:"display_name"`
	Source          skillSourceJSON `json:"source"`
	LatestVersionID string          `json:"latest_version_id"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// skillSourceJSON is the GA source object. The reference's type set is wider
// than the two this platform mints — it gained anthropic_example and plugin —
// which is why the list's source filter is validated against its own pair
// rather than against whatever a source object may carry.
type skillSourceJSON struct {
	Type string `json:"type"`
}

func renderSkill(id, displayName, latestVersionID, source string, createdAt, updatedAt time.Time) skillJSON {
	return skillJSON{
		Type: "skill", ID: id, DisplayName: displayName,
		Source: skillSourceJSON{Type: source}, LatestVersionID: latestVersionID,
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(),
	}
}

// skillVersionJSON is the GA SkillVersion wire shape — exactly six keys. Both
// `directory` and the Unix-epoch `version` left the object in the reference's
// 2026-08-27 migration: a version is addressed solely by its id. The numeric
// stays this platform's internal identity (the blob key, the materialization
// sentinel — decision 3); it simply stops appearing on the wire.
type skillVersionJSON struct {
	Type        string    `json:"type"`
	ID          string    `json:"id"`
	SkillID     string    `json:"skill_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// renderSkillVersion is the one place the six keys are assembled.
func renderSkillVersion(vid, skillID, name, description string, createdAt time.Time) skillVersionJSON {
	return skillVersionJSON{
		Type: "skill_version", ID: vid, SkillID: skillID,
		Name: name, Description: description, CreatedAt: createdAt.UTC(),
	}
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

// skillLatestAlias is the read-only version alias.
const skillLatestAlias = "latest"

// The {version} path slot's four accepted forms (plan 39 decision 4):
//
//   - a GA skver_ id, and the legacy skillver_ id this platform minted before
//     the convergence — old rows keep theirs, because agent configs pin them;
//   - "latest", on the read routes only;
//   - the numeric version, as a registered legacy alias. GA rejects it
//     outright, but agents created before this change hold numeric pins, and a
//     stored pin that silently stops resolving is worse than a divergence.
//
// The id form is matched by shape rather than through domain.ID so the slot
// stays independent of which prefix the platform currently mints; what both
// patterns really guarantee is that no unstorable byte reaches a bind
// parameter (checkSkillID's rationale).
var (
	skillVersionIDRe     = regexp.MustCompile(`^(?:skver|skillver)_[0-9a-z]{1,64}$`)
	skillVersionNumberRe = regexp.MustCompile(`^[0-9]{1,32}$`)
)

// checkSkillVersion validates a {version} path slot. aliasVerb is empty on the
// read routes, which accept "latest"; on the two routes that refuse the alias
// it is the verb naming the refusal — "deleting" and "downloading" — and the
// sentence is the reference's own, reproduced verbatim.
func checkSkillVersion(v, aliasVerb string) error {
	if v == skillLatestAlias {
		if aliasVerb == "" {
			return nil
		}
		return errInvalid("'latest' is not accepted when %s a skill version. "+
			"Address a specific version id: read the skill's latest_version_id, or "+
			"GET /v1/skills/{skill_id}/versions/latest, and use that id.", aliasVerb)
	}
	if skillVersionIDRe.MatchString(v) || skillVersionNumberRe.MatchString(v) {
		return nil
	}
	return errInvalid("Invalid version id: '%s'", v)
}

// resolveSkillVersion maps a validated {version} slot onto the numeric version
// the storage layer keys on: "latest" through the skill's latest_version, an id
// through its row, a numeric taken verbatim. A version row's id and its number
// are both immutable, so resolving outside a caller's transaction cannot go
// stale — the row can only vanish, and every caller's next statement reports
// that as the 404 it is.
func (s *server) resolveSkillVersion(ctx context.Context, skillID, slot string) (string, error) {
	switch {
	case slot == skillLatestAlias:
		var latest *string
		err := s.pool.QueryRow(ctx,
			`SELECT latest_version FROM skills WHERE id = $1`, skillID).Scan(&latest)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && latest == nil) {
			return "", errNotFound("skill %s version %s not found", skillID, slot)
		}
		if err != nil {
			return "", err
		}
		return *latest, nil
	case skillVersionIDRe.MatchString(slot):
		var version string
		err := s.pool.QueryRow(ctx,
			`SELECT version FROM skill_versions WHERE skill_id = $1 AND id = $2`,
			skillID, slot).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errNotFound("skill %s version %s not found", skillID, slot)
		}
		if err != nil {
			return "", err
		}
		return version, nil
	default:
		return slot, nil
	}
}

// skillLatestVersionIDExpr renders latest_version_id for a skills query: the id
// of the row the skill's numeric latest_version names. The empty string is what
// a skill with no versions renders, the field being required on the wire —
// unreachable through the API since decision 6, but not in a database upgraded
// from before it, where deleting the only version was allowed.
const skillLatestVersionIDExpr = `COALESCE(
	(SELECT v.id FROM skill_versions v
	 WHERE v.skill_id = s.id AND v.version = s.latest_version), '')`

// skillBlobKey is the object-storage key layout documented in internal/blob,
// shared with the executor's materialization via skills.BlobKey.
func skillBlobKey(skillID, version string) string {
	return skills.BlobKey(skillID, version)
}

// maxSkillLimit is the skills list's ceiling. Unlike page.go's maxEventLimit
// this is a recorded reference number rather than a compatible bound of ours:
// the 2026-09-04 recording accepts limit=1000 and refuses 1001 and 0 with the
// message below.
const maxSkillLimit = 1000

// parseSkillsPage is parsePageMax for both skills lists — the collection and a
// skill's versions — whose out-of-range message is the reference's own rather
// than parsePageWith's shared wording (recorded on each list separately). The
// limit is re-parsed here only to say it differently, so the cursor half stays
// in one place.
func parseSkillsPage(q url.Values) (pageParams, error) {
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxSkillLimit {
			return pageParams{}, errInvalid("limit must be between 1 and %d", maxSkillLimit)
		}
	}
	return parsePageMax(q, maxSkillLimit)
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
	// display_name is optional and derives from the SKILL.md frontmatter name
	// when omitted; it is capped at 255 characters by the form parser and,
	// since the GA rename, not unique.
	displayName := bundle.Name
	if up.displayNameSet {
		if up.displayName == "" || !storableText(up.displayName) {
			recordSkillUpload(ctx, skillOutcomeInvalid, 0)
			return nil, errInvalid("display_name must be non-empty storable text")
		}
		displayName = up.displayName
	}

	id := domain.NewID(domain.PrefixSkill).String()
	version := mintSkillVersion()
	vid := domain.NewID(domain.PrefixSkillVersion).String()
	created, err := s.insertSkill(ctx, id, vid, displayName, version, bundle)
	if err != nil {
		recordSkillUpload(ctx, skillOutcomeError, 0)
		return nil, err
	}
	recordSkillUpload(ctx, skillOutcomeOK, int64(len(bundle.Zip)))
	slog.InfoContext(ctx, "skill created", "skill_id", id, "version", version,
		"files", len(up.files), "bytes", len(bundle.Zip))
	return renderSkill(id, displayName, vid, "custom", created, created), nil
}

// insertSkill lands the skill row, its first version, and the archive in one
// transaction: rows first, the blob put before commit (the object exists
// before the rows become visible — a version row can never dangle), commit
// last. The only orphan window is a failed commit after a successful put,
// cleaned best-effort.
func (s *server) insertSkill(ctx context.Context, id, vid, displayName, version string, bundle *skills.Bundle) (time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var createdAt time.Time
	// display_title is the 0007 column name; only the wire field was renamed
	// to display_name (migration 0033 says why the column keeps its spelling).
	if err := tx.QueryRow(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version)
		 VALUES ($1, 'custom', $2, $3) RETURNING created_at`,
		id, displayName, version).Scan(&createdAt); err != nil {
		return time.Time{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, sha256)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		vid, id, version,
		bundle.Name, bundle.Description, bundle.Directory, bundle.SHA256); err != nil {
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
		displayName, latestVersionID, source string
		createdAt, updatedAt                 time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT s.display_title, `+skillLatestVersionIDExpr+`, s.source, s.created_at, s.updated_at
		 FROM skills s WHERE s.id = $1`, id).
		Scan(&displayName, &latestVersionID, &source, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("skill %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return renderSkill(id, displayName, latestVersionID, source, createdAt, updatedAt), nil
}

func (s *server) listSkills(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	page, err := parseSkillsPage(q)
	if err != nil {
		return nil, err
	}
	// The filter's pair did not widen when the source OBJECT's type set gained
	// anthropic_example and plugin: the reference still refuses anything but
	// these two here, with this message.
	source := q.Get("source")
	if source != "" && source != "custom" && source != "anthropic" {
		return nil, errInvalid("source must be one of custom, anthropic")
	}

	query := `SELECT s.id, s.display_title, ` + skillLatestVersionIDExpr +
		`, s.source, s.created_at, s.updated_at FROM skills s WHERE true`
	var args []any
	if source != "" {
		args = append(args, source)
		query += fmt.Sprintf(` AND s.source = $%d`, len(args))
	}
	if page.cur != nil {
		if page.cur.versioned || page.cur.seqKeyed || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
		args = append(args, page.cur.t, page.cur.id)
		query += fmt.Sprintf(` AND (s.created_at, s.id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, page.limit+1)
	query += fmt.Sprintf(` ORDER BY s.created_at DESC, s.id DESC LIMIT $%d`, len(args))

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
			id, displayName, latestVersionID, src string
			createdAt, updatedAt                  time.Time
		)
		if err := rows.Scan(&id, &displayName, &latestVersionID, &src, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		data = append(data, renderSkill(id, displayName, latestVersionID, src, createdAt, updatedAt))
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

// deleteSkill cascades: versions then skill, in one transaction, with each
// version's archive swept from object storage afterwards (plan 39 decision 6).
// The cascade is enforced here rather than by the schema on purpose — an
// ON DELETE CASCADE would drop the version rows behind this handler's back and
// orphan every archive they name.
func (s *server) deleteSkill(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock the parent row before reading its versions, as insertSkillVersion
	// and deleteSkillVersion do: a concurrent version create then blocks here,
	// so the set this delete sweeps cannot grow behind it and strand an archive
	// whose row is gone.
	//
	// The imported anthropic catalog is not API-manageable (its versions
	// already refuse create); an accidental DELETE must not empty it.
	var source string
	if err := tx.QueryRow(ctx, `SELECT source FROM skills WHERE id = $1 FOR UPDATE`, id).Scan(&source); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound("skill %s not found", id)
		}
		return nil, err
	}
	if source != "custom" {
		return nil, errInvalid("anthropic skills are managed by the platform, not this API")
	}
	// The sweep needs somewhere to sweep — asked after shape and existence, so
	// that a deployment without object storage still answers an unknown or
	// unmanaged id the way it did before the cascade gave this route archives
	// to remove at all.
	if s.blobs == nil {
		return nil, errSkillsUnavailable
	}
	rows, err := tx.Query(ctx,
		`DELETE FROM skill_versions WHERE skill_id = $1 RETURNING version`, id)
	if err != nil {
		return nil, err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM skills WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// The rows are gone; the archives follow best-effort, the same ordering a
	// single version delete uses (rare orphans are accepted; GC is a non-goal).
	// The sweep outlives the request on purpose: it is N sequential deletes
	// with nothing left to answer the client with, so on the request's own
	// context a proxy timeout would orphan not the odd archive the plan
	// accepts but every one the skill still had.
	sweep := context.WithoutCancel(ctx)
	for _, v := range versions {
		s.deleteOrphanedObject(sweep, skillBlobKey(id, v))
	}
	slog.InfoContext(ctx, "skill deleted", "skill_id", id, "versions", len(versions))
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
		var rejected *apiError
		if errors.As(err, &rejected) {
			// The name-consistency refusal: a bad SKILL.md like any other, so
			// it counts as a rejected upload rather than a platform failure.
			recordSkillUpload(ctx, skillOutcomeInvalid, 0)
			return nil, err
		}
		recordSkillUpload(ctx, skillOutcomeError, 0)
		return nil, err
	}
	recordSkillUpload(ctx, skillOutcomeOK, int64(len(bundle.Zip)))
	slog.InfoContext(ctx, "skill version created", "skill_id", id, "version", version,
		"files", len(up.files), "bytes", len(bundle.Zip))
	return renderSkillVersion(vid, id, bundle.Name, bundle.Description, createdAt), nil
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
	// The frontmatter name is one skill's forever, and a version that renames
	// it is refused with the reference's own sentence (plan 39 §2, recording
	// 54): two names on one skill would materialize into two directories, so a
	// pin's landing place would depend on which version answered it. Every
	// version shares the name by this very rule, so any row answers — except in
	// a database upgraded across this change, where nothing enforced it before
	// and a skill may hold several. The newest version's name is the one that
	// wins there, by the same length-then-lexical order the delete path
	// recomputes latest_version with, so the answer is at least deterministic
	// and is the name a client last chose; deleting the odd version is the
	// recovery. Reading it under the parent lock is what stops two concurrent
	// creates on a skill left with no versions from each establishing a
	// different one.
	var named string
	switch err := tx.QueryRow(ctx,
		`SELECT name FROM skill_versions WHERE skill_id = $1
		 ORDER BY length(version) DESC, version DESC LIMIT 1`, id).Scan(&named); {
	case errors.Is(err, pgx.ErrNoRows): // nothing to be consistent with yet
	case err != nil:
		return time.Time{}, err
	case named != bundle.Name:
		return time.Time{}, errInvalid(
			"Skill name '%s' in SKILL.md must be consistent across all versions "+
				"for a given `skill_id`. Expected '%s'.", bundle.Name, named)
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, sha256)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		vid, id, version, bundle.Name, bundle.Description, bundle.Directory,
		bundle.SHA256).Scan(&createdAt); err != nil {
		return time.Time{}, err
	}
	// latest_version advances only to a numerically newer version (length-then-
	// lexical, matching the importer and the delete path's recompute). The
	// version is minted before the FOR UPDATE lock above, so two concurrent
	// creates can serialize in the opposite order to their mint times; an
	// unconditional assignment would let the loser's older version clobber the
	// winner's newer latest_version.
	if _, err := tx.Exec(ctx,
		`UPDATE skills SET latest_version = $2, updated_at = now()
		 WHERE id = $1 AND (latest_version IS NULL
		   OR length($2::text) > length(latest_version)
		   OR (length($2::text) = length(latest_version) AND $2 > latest_version))`,
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
	// Both skills lists cap at 1000 and phrase an out-of-range limit the
	// reference's way, which is not parsePageWith's shared wording.
	page, err := parseSkillsPage(r.URL.Query())
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

	query := `SELECT id, name, description, created_at FROM skill_versions WHERE skill_id = $1`
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
			vid, name, description string
			createdAt              time.Time
		)
		if err := rows.Scan(&vid, &name, &description, &createdAt); err != nil {
			return nil, err
		}
		data = append(data, renderSkillVersion(vid, id, name, description, createdAt))
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
	id, slot := r.PathValue("id"), r.PathValue("version")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	// A read route: "latest" is one of the accepted forms here.
	if err := checkSkillVersion(slot, ""); err != nil {
		return nil, err
	}
	version, err := s.resolveSkillVersion(ctx, id, slot)
	if err != nil {
		return nil, err
	}
	var (
		vid, name, description string
		createdAt              time.Time
	)
	err = s.pool.QueryRow(ctx,
		`SELECT id, name, description, created_at FROM skill_versions
		 WHERE skill_id = $1 AND version = $2`, id, version).
		Scan(&vid, &name, &description, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("skill %s version %s not found", id, slot)
	}
	if err != nil {
		return nil, err
	}
	return renderSkillVersion(vid, id, name, description, createdAt), nil
}

func (s *server) deleteSkillVersion(r *http.Request) (any, error) {
	ctx := r.Context()
	id, slot := r.PathValue("id"), r.PathValue("version")
	if err := checkSkillID(id); err != nil {
		return nil, err
	}
	if err := checkSkillVersion(slot, "deleting"); err != nil {
		return nil, err
	}
	if s.blobs == nil {
		return nil, errSkillsUnavailable
	}
	version, err := s.resolveSkillVersion(ctx, id, slot)
	if err != nil {
		return nil, err
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
			return nil, errNotFound("skill %s version %s not found", id, slot)
		}
		return nil, err
	}
	if source != "custom" {
		return nil, errInvalid("versions of anthropic skills are managed by the platform, not this API")
	}
	// Under the parent lock, so a concurrent create cannot turn a refused
	// only-version delete into an allowed one or the reverse.
	var total, target int
	if err := tx.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE version = $2) FROM skill_versions WHERE skill_id = $1`,
		id, version).Scan(&total, &target); err != nil {
		return nil, err
	}
	if target == 0 {
		return nil, errNotFound("skill %s version %s not found", id, slot)
	}
	// A skill can never reach zero versions through the API (decision 6): the
	// way to remove the last one is to delete the skill, which cascades.
	if total == 1 {
		return nil, errInvalid("cannot delete a Skill's only version. " +
			"Delete the Skill, or create another version first")
	}
	var vid string
	if err := tx.QueryRow(ctx,
		`DELETE FROM skill_versions WHERE skill_id = $1 AND version = $2 RETURNING id`,
		id, version).Scan(&vid); err != nil {
		return nil, err
	}
	// The new latest_version is the numerically greatest remaining version
	// (length-then-lexical, the rule used everywhere else), not merely the
	// most-recently-created — the two can diverge for imported or backfilled
	// versions. At least one always remains, by the refusal above.
	if _, err := tx.Exec(ctx,
		`UPDATE skills SET latest_version = (
		    SELECT version FROM skill_versions WHERE skill_id = $1
		    ORDER BY length(version) DESC, version DESC LIMIT 1
		 ), updated_at = now() WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// The row is gone; the archive follows best-effort (plan: rare orphans
	// are accepted, GC is a non-goal), and outside the request's cancellation
	// for the same reason the cascade's sweep is: the row cannot come back, so
	// a client that gave up after the commit must not decide whether the
	// object it named goes with it.
	s.deleteOrphanedObject(context.WithoutCancel(ctx), skillBlobKey(id, version))
	slog.InfoContext(ctx, "skill version deleted", "skill_id", id, "version", version)
	// The deleted object's id, like every other id on this surface since the GA
	// convergence, is the version row's — the numeric no longer appears on the
	// wire at all (decision 3), whichever form the caller addressed it by.
	return map[string]string{"id": vid, "type": "skill_version_deleted"}, nil
}

// skillContentDisposition renders the archive download's Content-Disposition
// in the reference's exact form — an attachment named for the version's own
// immutable slug, carried only as an RFC 5987 filename* with no bare filename
// fallback beside it. mime.FormatMediaType is not usable here: it emits the
// extended form only for a value it finds non-ASCII, and the reference sends it
// unconditionally.
func skillContentDisposition(name string) string {
	return "attachment; filename*=utf-8''" + rfc5987Escape(name+".zip")
}

// rfc5987Escape percent-encodes every byte outside RFC 5987's attr-char set.
// A skill name is validated kebab-case at upload, so in practice nothing is
// escaped; encoding rather than interpolating is what keeps a name that did
// need it from breaking the header.
func rfc5987Escape(s string) string {
	const attrChar = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(attrChar, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// downloadSkillVersion streams the stored archive. Not a typed handler: the
// body is the object, not JSON.
func (s *server) downloadSkillVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, slot := r.PathValue("id"), r.PathValue("version")
	if err := checkSkillID(id); err != nil {
		writeError(w, r, err)
		return
	}
	if err := checkSkillVersion(slot, "downloading"); err != nil {
		writeError(w, r, err)
		return
	}
	if s.blobs == nil {
		writeError(w, r, errSkillsUnavailable)
		return
	}
	version, err := s.resolveSkillVersion(ctx, id, slot)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The existence probe also fetches the recorded archive digest and the
	// version's name — the same round trip, and the only place the wire-only
	// BYOC worker can learn the digest.
	var (
		name string
		sha  *string
	)
	err = s.pool.QueryRow(ctx,
		`SELECT name, sha256 FROM skill_versions WHERE skill_id = $1 AND version = $2`,
		id, version).Scan(&name, &sha)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, errNotFound("skill %s version %s not found", id, slot))
		return
	}
	if err != nil {
		writeError(w, r, err)
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
	// Content-Type and Content-Length are an inference (docs/DIVERGENCES.md):
	// the SDK sends Accept: application/binary and treats the body as opaque
	// bytes. Content-Disposition is not — it is the recorded reference header,
	// on every observed content download.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", skillContentDisposition(name))
	// The archive's recorded digest, so a wire-only consumer can verify what it
	// downloaded (the SDK's version object carries no checksum field). Additive
	// and ignored by reference clients — the traceparent-on-/work/poll pattern.
	// Omitted for a version that predates the column: no digest was recorded,
	// and an absent header says so where a wrong one would fail closed.
	if sha != nil {
		w.Header().Set(skills.ArchiveDigestHeader, *sha)
	}
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
