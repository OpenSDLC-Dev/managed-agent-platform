package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// maxResourceListLimit caps GET /v1/sessions/{id}/resources: the SDK documents
// limit "1 to 1000" and, uniquely, "if omitted, returns all"
// (anthropic-sdk-go betasessionresource.go). parseResourceLimit maps an omitted
// limit to -1 (all), not the managed-agents default of 20.
const maxResourceListLimit = 1000

// defaultMountRoot is the container mount location for an uploaded file resource
// when the caller gives no mount_path: /mnt/session/uploads/<file_id>
// (betasession.go:693-717 documents the default).
const defaultMountRoot = "/mnt/session/uploads/"

// maxMountPathBytes bounds a caller-supplied mount_path so a pathological value
// never reaches the sandbox layer or the jsonb column.
const maxMountPathBytes = 1024

// fileResourceJSON is the materialized session file resource
// (BetaManagedAgentsSessionResource file variant, betasessionresource.go:176-209):
// every field is api:"required", so the server resolves the default mount_path
// and both timestamps at create/add and renders them. Stored verbatim as one
// element of the sessions.resources jsonb array; session GET echoes the array.
type fileResourceJSON struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	FileID    string    `json:"file_id"`
	MountPath string    `json:"mount_path"`
	Type      string    `json:"type"`
	UpdatedAt time.Time `json:"updated_at"`
}

// resourceInput is a validated-but-not-yet-materialized resource: its file has
// not been proven to exist and it has no sesrsc_ id or timestamps.
type resourceInput struct {
	fileID    string
	mountPath string
}

// parseResourceInputs validates the create-time resources[] union without
// touching the database: each element must be a supported resource (only
// type:"file" in v1; github_repository/memory_store keep the union seam open —
// the git half of #55 lands there — but are rejected), a valid file_id, and an
// absolute, unique, storable mount_path. Existence of the referenced file is
// checked later, inside the create transaction (materializeResourceInputs).
func parseResourceInputs(obj map[string]json.RawMessage) ([]resourceInput, error) {
	raw, ok := obj["resources"]
	if !ok || isNull(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errInvalid("resources must be an array")
	}
	out := make([]resourceInput, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		in, err := parseResourceItem(item)
		if err != nil {
			return nil, err
		}
		if seen[in.mountPath] {
			return nil, errInvalid("mount_path %q is used by more than one resource", in.mountPath)
		}
		seen[in.mountPath] = true
		out = append(out, in)
	}
	return out, nil
}

func parseResourceItem(raw json.RawMessage) (resourceInput, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return resourceInput{}, errInvalid("each resource must be an object")
	}
	return parseResourceObject(obj)
}

// parseResourceObject dispatches on the resource union's type discriminator. It
// backs both the create-time array elements and the add endpoint's single body.
func parseResourceObject(obj map[string]json.RawMessage) (resourceInput, error) {
	typ, err := requiredString(obj, "type")
	if err != nil {
		return resourceInput{}, err
	}
	switch typ {
	case "file":
		return parseFileResource(obj)
	case "github_repository", "memory_store":
		return resourceInput{}, errInvalid("%s resources are not supported yet", typ)
	default:
		return resourceInput{}, errInvalid("resource type %q is not supported", typ)
	}
}

func parseFileResource(obj map[string]json.RawMessage) (resourceInput, error) {
	if err := rejectUnknownKeys(obj, "type", "file_id", "mount_path"); err != nil {
		return resourceInput{}, err
	}
	fileID, err := requiredString(obj, "file_id")
	if err != nil {
		return resourceInput{}, err
	}
	if !domain.ID(fileID).HasPrefix(domain.PrefixFile) || !domain.ID(fileID).Valid() {
		return resourceInput{}, errInvalid("file_id must be a valid file id")
	}
	mountPath, set, null, err := stringField(obj, "mount_path")
	if err != nil {
		return resourceInput{}, err
	}
	if !set || null || mountPath == "" {
		mountPath = defaultMountRoot + fileID
	} else if err := validateMountPath(mountPath); err != nil {
		return resourceInput{}, err
	}
	return resourceInput{fileID: fileID, mountPath: mountPath}, nil
}

// validateMountPath enforces the mount-path shape: absolute, bounded, and
// storable. An unstorable byte (U+0000, invalid UTF-8) would otherwise fail as a
// 500 when the resources array binds into the jsonb column (see #135).
func validateMountPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return errInvalid("mount_path must be an absolute path")
	}
	if len(p) > maxMountPathBytes {
		return errInvalid("mount_path must be at most %d bytes", maxMountPathBytes)
	}
	if !storableText(p) {
		return errInvalid("mount_path must be storable text")
	}
	return nil
}

// materializeResourceInputs verifies each referenced file exists in the same
// transaction as the create (cheaper failure locality than an unvalidated
// reference — an INFERRED divergence, docs/DIVERGENCES.md) and stamps each input
// with a fresh sesrsc_ id and the create timestamp. A file deleted between this
// check and a later materialization is tolerated by design (plan decision 2).
func materializeResourceInputs(ctx context.Context, db querier, inputs []resourceInput, now time.Time) ([]fileResourceJSON, error) {
	out := make([]fileResourceJSON, 0, len(inputs))
	for _, in := range inputs {
		if err := fileMustExist(ctx, db, in.fileID); err != nil {
			return nil, err
		}
		out = append(out, fileResourceJSON{
			ID:        domain.NewID(domain.PrefixResource).String(),
			CreatedAt: now, FileID: in.fileID, MountPath: in.mountPath,
			Type: "file", UpdatedAt: now,
		})
	}
	return out, nil
}

// fileMustExist reports whether a file row exists, mapping absence to the wire
// 404. The referencing session's transaction holds no lock on the file row, so a
// concurrent delete may still leave a dangling reference — accepted (decision 2).
func fileMustExist(ctx context.Context, db querier, fileID string) error {
	var exists bool
	err := db.QueryRow(ctx, `SELECT true FROM files WHERE id = $1`, fileID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("file %s not found", fileID)
	}
	return err
}

// sessionResourceRows loads a session's stored resources array and archived
// state. forUpdate takes the session row lock so a concurrent add/delete/archive
// serializes; reads pass false and may use the pool directly. A missing session
// surfaces as pgx.ErrNoRows for the caller to map to a 404.
func sessionResourceRows(ctx context.Context, db querier, id string, forUpdate bool) (resources []json.RawMessage, archivedAt *time.Time, err error) {
	q := `SELECT resources, archived_at FROM sessions WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var raw []byte
	if err = db.QueryRow(ctx, q, id).Scan(&raw, &archivedAt); err != nil {
		return nil, nil, err
	}
	if err = json.Unmarshal(raw, &resources); err != nil {
		return nil, nil, err
	}
	return resources, archivedAt, nil
}

// updateSessionResources rewrites the resources array and bumps updated_at. No
// session.updated event is emitted: the taxonomy has no session_resource.* event
// and the documented session.updated payload carries only title/metadata/agent.
func updateSessionResources(ctx context.Context, tx pgx.Tx, id string, resources []json.RawMessage) error {
	raw, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE sessions SET resources = $2, updated_at = now() WHERE id = $1`, id, raw)
	return err
}

func (s *server) getSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	rid := r.PathValue("rid")
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	if err := checkResourceID(rid); err != nil {
		return nil, err
	}
	resources, _, err := sessionResourceRows(ctx, s.pool, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if raw := findResource(resources, rid); raw != nil {
		return raw, nil
	}
	return nil, errNotFound("session resource %s not found", rid)
}

func (s *server) listSessionResources(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	q := r.URL.Query()
	limit, err := parseResourceLimit(q)
	if err != nil {
		return nil, err
	}
	resources, _, err := sessionResourceRows(ctx, s.pool, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}

	start := 0
	if cur := q.Get("page"); cur != "" {
		last, err := decodeResourceCursor(cur)
		if err != nil {
			return nil, err
		}
		// An unknown cursor yields an empty page — the files-list convention.
		idx := indexOfResource(resources, last)
		if idx < 0 {
			return pageJSON{Data: []any{}}, nil
		}
		start = idx + 1
	}
	end := len(resources)
	if limit >= 0 && start+limit < end {
		end = start + limit
	}
	page := resources[start:end]
	out := pageJSON{Data: make([]any, 0, len(page))}
	for _, raw := range page {
		out.Data = append(out.Data, raw)
	}
	if end < len(resources) && len(page) > 0 {
		c := encodeResourceCursor(resourceID(page[len(page)-1]))
		out.NextPage = &c
	}
	return out, nil
}

func (s *server) addSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	res, err := s.addSessionResourceTx(ctx, id, r)
	if err != nil {
		recordResourceMutation(ctx, resourceOutcomeFor(err), 1)
		return nil, err
	}
	recordResourceMutation(ctx, resourceOutcomeOK, 1)
	slog.InfoContext(ctx, "session resource added", "session_id", id,
		"resource_id", res.ID, "file_id", res.FileID, "mount_path", res.MountPath)
	return res, nil
}

func (s *server) addSessionResourceTx(ctx context.Context, id string, r *http.Request) (fileResourceJSON, error) {
	obj, err := decodeObject(r)
	if err != nil {
		return fileResourceJSON{}, err
	}
	in, err := parseResourceObject(obj)
	if err != nil {
		return fileResourceJSON{}, err
	}
	if err := checkID(id, "session"); err != nil {
		return fileResourceJSON{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fileResourceJSON{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	resources, archivedAt, err := sessionResourceRows(ctx, tx, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return fileResourceJSON{}, errNotFound("session %s not found", id)
	}
	if err != nil {
		return fileResourceJSON{}, err
	}
	if archivedAt != nil {
		return fileResourceJSON{}, errInvalid("session %s is archived", id)
	}
	if mountPathTaken(resources, in.mountPath) {
		return fileResourceJSON{}, errInvalid("mount_path %q is already in use by this session", in.mountPath)
	}
	if err := fileMustExist(ctx, tx, in.fileID); err != nil {
		return fileResourceJSON{}, err
	}
	now := time.Now().UTC()
	res := fileResourceJSON{
		ID: domain.NewID(domain.PrefixResource).String(), CreatedAt: now,
		FileID: in.fileID, MountPath: in.mountPath, Type: "file", UpdatedAt: now,
	}
	resources = append(resources, mustJSON(res))
	if err := updateSessionResources(ctx, tx, id, resources); err != nil {
		return fileResourceJSON{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return fileResourceJSON{}, err
	}
	return res, nil
}

func (s *server) deleteSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	rid := r.PathValue("rid")
	if err := s.deleteSessionResourceTx(ctx, id, rid); err != nil {
		recordResourceMutation(ctx, resourceOutcomeFor(err), 1)
		return nil, err
	}
	recordResourceMutation(ctx, resourceOutcomeOK, 1)
	// Deletion removes the reference only; it never reaches into a live sandbox
	// to unmount (a non-goal, INFERRED divergence).
	slog.InfoContext(ctx, "session resource deleted", "session_id", id, "resource_id", rid)
	return map[string]string{"id": rid, "type": "session_resource_deleted"}, nil
}

func (s *server) deleteSessionResourceTx(ctx context.Context, id, rid string) error {
	if err := checkID(id, "session"); err != nil {
		return err
	}
	if err := checkResourceID(rid); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	resources, archivedAt, err := sessionResourceRows(ctx, tx, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("session %s not found", id)
	}
	if err != nil {
		return err
	}
	if archivedAt != nil {
		return errInvalid("session %s is archived", id)
	}
	idx := indexOfResource(resources, rid)
	if idx < 0 {
		return errNotFound("session resource %s not found", rid)
	}
	resources = append(resources[:idx], resources[idx+1:]...)
	if err := updateSessionResources(ctx, tx, id, resources); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// updateSessionResource handles POST …/resources/{rid}. The reference accepts
// only an authorization_token here and rotates it for github_repository
// resources; every resource this platform stores is a file, so the operation is
// always rejected (INFERRED error shape, docs/DIVERGENCES.md).
func (s *server) updateSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	rid := r.PathValue("rid")
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "authorization_token"); err != nil {
		return nil, err
	}
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	if err := checkResourceID(rid); err != nil {
		return nil, err
	}
	resources, _, err := sessionResourceRows(ctx, s.pool, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if findResource(resources, rid) == nil {
		return nil, errNotFound("session resource %s not found", rid)
	}
	return nil, errInvalid("only github_repository resources support token rotation")
}

// checkResourceID rejects a path id that is not a well-formed sesrsc_ id with the
// 404 an unknown resource already gets (checkID's shape-reject rationale).
func checkResourceID(id string) error {
	if !domain.ID(id).HasPrefix(domain.PrefixResource) || !domain.ID(id).Valid() {
		return errNotFound("session resource %s not found", id)
	}
	return nil
}

// findResource returns the stored resource object whose id equals rid, or nil.
func findResource(resources []json.RawMessage, rid string) json.RawMessage {
	if i := indexOfResource(resources, rid); i >= 0 {
		return resources[i]
	}
	return nil
}

func indexOfResource(resources []json.RawMessage, rid string) int {
	for i, raw := range resources {
		if resourceID(raw) == rid {
			return i
		}
	}
	return -1
}

func resourceID(raw json.RawMessage) string {
	var o struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.ID
}

func mountPathTaken(resources []json.RawMessage, path string) bool {
	for _, raw := range resources {
		var o struct {
			MountPath string `json:"mount_path"`
		}
		if json.Unmarshal(raw, &o) == nil && o.MountPath == path {
			return true
		}
	}
	return false
}

// parseResourceLimit parses the resources-list limit. Unlike every other list,
// an omitted limit means "return all" (SDK: "if omitted, returns all"), reported
// as -1; a present value must be 1..1000.
func parseResourceLimit(q url.Values) (int, error) {
	s := q.Get("limit")
	if s == "" {
		return -1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxResourceListLimit {
		return 0, errInvalid("limit must be an integer between 1 and %d", maxResourceListLimit)
	}
	return n, nil
}

// encodeResourceCursor/decodeResourceCursor are the resources-list page cursor:
// the resources live in one jsonb array, so a page is a slice and the cursor is
// simply the last id returned. Self-contained (not the base64 keyset cursor the
// keyset lists use) because there is no (created_at, id) table walk here.
func encodeResourceCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("r1|" + id))
}

func decodeResourceCursor(s string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", errInvalid("invalid page cursor")
	}
	rest, ok := strings.CutPrefix(string(raw), "r1|")
	if !ok || rest == "" {
		return "", errInvalid("invalid page cursor")
	}
	return rest, nil
}

// MetricSessionResources counts session file-resource mutations (create-attach,
// add, delete) by outcome. Outcome-only labels: session/resource/file ids ride
// the structured logs, never the metric (plan decision 9). Exported so the
// integration test can assert the name and labels.
const MetricSessionResources = "session.resources"

const (
	resourceOutcomeOK       = "ok"
	resourceOutcomeInvalid  = "invalid"
	resourceOutcomeNotFound = "not_found"
	resourceOutcomeError    = "error"
)

// resourceOutcomeFor maps a handler error to its mutation outcome label.
func resourceOutcomeFor(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.status {
		case http.StatusNotFound:
			return resourceOutcomeNotFound
		case http.StatusBadRequest:
			return resourceOutcomeInvalid
		}
	}
	return resourceOutcomeError
}

// recordResourceMutation counts resource mutations by outcome: 1 for a single
// add/delete (or a failed attempt), the resource count for a create that
// attaches several. The meter is resolved per call so it never pins a
// MeterProvider installed after startup; telemetry failure never fails a request.
func recordResourceMutation(ctx context.Context, outcome string, count int) {
	meter := otel.GetMeterProvider().Meter(apiMeterName)
	c, err := meter.Int64Counter(MetricSessionResources,
		metric.WithDescription("Session file-resource mutations by outcome."))
	if err != nil {
		return
	}
	c.Add(ctx, int64(count), metric.WithAttributes(attribute.String("outcome", outcome)))
}
