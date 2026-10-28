package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/unknownkey"
)

const (
	consoleMembersPath = "/api/console/organizations/{org}/members"
	consoleMemberPath  = consoleMembersPath + "/{member_id}"
)

// Native user fields plus our private role provenance and tombstone status.
// Internal principal IDs, issuer/subject and credentials never enter this DTO.
type memberJSON struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	AddedAt    time.Time `json:"added_at"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	RoleSource string    `json:"role_source"`
	Status     string    `json:"status"`
	LoginName  string    `json:"login_name,omitempty"`
}

type membersPageJSON struct {
	Data    []memberJSON `json:"data"`
	HasMore bool         `json:"has_more"`
	FirstID *string      `json:"first_id"`
	LastID  *string      `json:"last_id"`
}

func requireHumanAdmin(r *http.Request) (login.Session, error) {
	session, err := requireCookieSession(r)
	if err != nil {
		return login.Session{}, err
	}
	if session.Role != identity.RoleAdmin {
		return login.Session{}, errForbidden("an admin browser session is required")
	}
	if err := consoleOrganization(r); err != nil {
		return login.Session{}, err
	}
	return session, nil
}

// All manual principal writes share setup's principal-first lock order. READ
// COMMITTED is explicit: a database default must not freeze a pre-wait snapshot.
// Recheck the caller inside this transaction, including service-account writes.
func (s *server) beginPrincipalMutation(r *http.Request) (pgx.Tx, login.Session, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, login.Session{}, err
	}
	ctx := r.Context()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, login.Session{}, err
	}
	fail := func(err error) (pgx.Tx, login.Session, error) { tx.Rollback(ctx); return nil, login.Session{}, err }
	if _, err := tx.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fail(err)
	}
	session, err := s.login.AuthenticateInTx(ctx, tx, r)
	if err != nil {
		switch {
		case errors.Is(err, login.ErrUnauthenticated):
			err = &apiErrorWithHeaders{err: errAuth("invalid or expired browser session"), headers: map[string]string{"x-platform-signed-out": "1"}}
		case errors.Is(err, principal.ErrDeactivated):
			err = &apiErrorWithHeaders{err: errDeactivated, headers: map[string]string{"x-platform-signed-out": "1"}}
		}
		return fail(err)
	}
	if session.Role != identity.RoleAdmin {
		return fail(errForbidden("an admin browser session is required"))
	}
	return tx, session, nil
}

func managementBody(r *http.Request, fields ...string) (map[string]json.RawMessage, error) {
	if err := loginJSON(r); err != nil {
		return nil, err
	}
	raw, err := readBody(r)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errInvalid("request body must be a JSON object")
	}
	obj, err := decodeBodyObject(raw)
	if err != nil {
		return nil, err
	}
	if _, ok := unknownkey.Least(obj, fields...); ok {
		return nil, errInvalid("request contains an unknown field")
	}
	return obj, nil
}

// Unlike identity.ParseRole, new management bodies never accept the legacy
// viewer alias. The SDK's other organization roles are not platform roles.
func managementRole(role string, human bool) bool {
	return role == "developer" || role == "admin" || human && role == "user"
}

func managementQuery(r *http.Request, singletons ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errInvalid("invalid query")
	}
	for _, key := range singletons {
		if len(q[key]) > 1 || len(q[key]) == 1 && !storableText(q[key][0]) {
			return nil, errInvalid("invalid %s query parameter", key)
		}
	}
	return q, nil
}

func managementLimit(q url.Values, max int) (int, error) {
	limit := 20
	if values, ok := q["limit"]; ok {
		var err error
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > max {
			return 0, errInvalid("limit must be an integer between 1 and %d", max)
		}
	}
	return limit, nil
}

func privatePrincipalID(value, wantKind string) (string, error) {
	id, kind, ok := principal.ParsePublicID(value)
	if !ok || kind != wantKind {
		return "", errNotFound("Not found")
	}
	return id, nil
}

const memberColumns = `id,created_at,email,display_name,role,role_source,status,
	CASE WHEN issuer='urn:managed-agent-platform:local' THEN subject ELSE '' END`

func scanMember(row pgx.Row) (memberJSON, error) {
	var m memberJSON
	err := row.Scan(&m.ID, &m.AddedAt, &m.Email, &m.Name, &m.Role, &m.RoleSource, &m.Status, &m.LoginName)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, errNotFound("Not found")
	}
	if err != nil {
		return m, err
	}
	m.ID, m.Type, m.AddedAt = principal.PublicID(m.ID, "user"), "user", m.AddedAt.UTC()
	return m, nil
}

func readMember(r *http.Request, db principal.Querier, id string) (memberJSON, error) {
	return scanMember(db.QueryRow(r.Context(), `SELECT `+memberColumns+` FROM principals WHERE id=$1 AND kind='user' AND org_id='default'`, id))
}

func (s *server) getMember(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	id, err := privatePrincipalID(r.PathValue("member_id"), "user")
	if err != nil {
		return nil, err
	}
	return readMember(r, s.pool, id)
}

func (s *server) listMembers(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	q, err := managementQuery(r, "limit", "after_id", "before_id", "email")
	if err != nil {
		return nil, err
	}
	limit, err := managementLimit(q, 1000)
	if err != nil {
		return nil, err
	}
	if q.Get("after_id") != "" && q.Get("before_id") != "" {
		return nil, errInvalid("use only one member cursor")
	}
	roles := q["roles[]"]
	for _, role := range roles {
		if !managementRole(role, true) {
			return nil, errInvalid("unsupported role")
		}
	}
	var email *string
	if q.Has("email") {
		value := q.Get("email")
		email = &value
	}
	args := []any{email, roles}
	where := `kind='user' AND status='active' AND org_id='default' AND ($1::text IS NULL OR email=$1) AND (coalesce(cardinality($2::text[]),0)=0 OR role=ANY($2))`
	order := "DESC"
	cursorID := q.Get("after_id")
	before := q.Get("before_id") != ""
	if before {
		cursorID = q.Get("before_id")
		order = "ASC"
	}
	if cursorID != "" {
		id, err := privatePrincipalID(cursorID, "user")
		if err != nil {
			return nil, errInvalid("invalid member cursor")
		}
		m, err := readMember(r, s.pool, id)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && ae.status == 404 {
				return nil, errInvalid("invalid member cursor")
			}
			return nil, err
		}
		comparison := "<"
		if before {
			comparison = ">"
		}
		where += ` AND (created_at,id) ` + comparison + ` ($3,$4)`
		args = append(args, m.AddedAt, id)
	}
	// limit is validated as an integer, not client SQL.
	rows, err := s.pool.Query(r.Context(), `SELECT `+memberColumns+` FROM principals WHERE `+where+` ORDER BY created_at `+order+`,id `+order+` LIMIT `+strconv.Itoa(limit+1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := membersPageJSON{Data: []memberJSON{}}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		page.Data = append(page.Data, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	if before {
		slices.Reverse(page.Data)
	}
	if len(page.Data) > 0 {
		page.FirstID = &page.Data[0].ID
		page.LastID = &page.Data[len(page.Data)-1].ID
	}
	return page, nil
}

func (s *server) createMember(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	if s.login.Mode() != login.ModePassword {
		return nil, errInvalid("local members require password mode")
	}
	obj, err := managementBody(r, "login_name", "password", "name", "role")
	if err != nil {
		return nil, err
	}
	name, password, err := localCredentials(obj)
	if err != nil {
		return nil, err
	}
	role, err := requiredString(obj, "role")
	if err != nil {
		return nil, err
	}
	if !managementRole(role, true) {
		return nil, errInvalid("unsupported role")
	}
	display, _, _, err := stringField(obj, "name")
	if err != nil {
		return nil, err
	}
	// Expensive password hashing never holds a principal lock.
	hash, err := login.HashPassword(password)
	if err != nil {
		return nil, err
	}
	tx, actor, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.Context())
	id := domain.NewID(domain.PrefixPrincipal).String()
	_, err = tx.Exec(r.Context(), `INSERT INTO principals(id,issuer,subject,display_name,role,created_by)
		VALUES ($1,$2,$3,$4,$5,$6)`, id, login.LocalIssuer, name, display, role, actor.PrincipalID)
	if err := principalNameConflict(err); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO local_accounts(principal_id,password_hash) VALUES ($1,$2)`, id, hash); err != nil {
		return nil, err
	}
	m, err := readMember(r, tx, id)
	if err != nil {
		return nil, err
	}
	return m, tx.Commit(r.Context())
}

func principalNameConflict(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return errConflict("this name is unavailable")
	}
	return err
}

func protectLastHumanAdmin(r *http.Request, tx pgx.Tx, m memberJSON) error {
	if m.Role != "admin" || m.Status != "active" {
		return nil
	}
	var count int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM principals WHERE org_id='default' AND kind='user' AND status='active' AND role='admin'`).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return errConflict("the last active human admin cannot be demoted or removed")
	}
	return nil
}

func (s *server) updateMember(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	id, err := privatePrincipalID(r.PathValue("member_id"), "user")
	if err != nil {
		return nil, err
	}
	obj, err := managementBody(r, "role")
	if err != nil {
		return nil, err
	}
	role, err := requiredString(obj, "role")
	if err != nil {
		return nil, err
	}
	if !managementRole(role, true) {
		return nil, errInvalid("unsupported role")
	}
	tx, actor, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.Context())
	m, err := readMember(r, tx, id)
	if err != nil {
		return nil, err
	}
	// This is the target's state, never a sign-out response for the caller.
	if m.Status != "active" {
		return nil, errInvalid("removed members cannot be updated")
	}
	if m.RoleSource == "idp" {
		return nil, errConflict("this role comes from the identity provider's mapping")
	}
	if role != "admin" {
		if err := protectLastHumanAdmin(r, tx, m); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE principals SET role=$2,role_source='console',updated_at=clock_timestamp(),updated_by=$3 WHERE id=$1`, id, role, actor.PrincipalID); err != nil {
		return nil, err
	}
	m, err = readMember(r, tx, id)
	if err != nil {
		return nil, err
	}
	return m, tx.Commit(r.Context())
}

func (s *server) removeMember(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	id, err := privatePrincipalID(r.PathValue("member_id"), "user")
	if err != nil {
		return nil, err
	}
	tx, actor, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.Context())
	m, err := readMember(r, tx, id)
	if err != nil {
		return nil, err
	}
	if err := protectLastHumanAdmin(r, tx, m); err != nil {
		return nil, err
	}
	if m.Status == "active" {
		if _, err := tx.Exec(r.Context(), `UPDATE principals SET status='deactivated',deactivated_at=clock_timestamp(),deactivated_by=$2,updated_at=clock_timestamp(),updated_by=$2 WHERE id=$1`, id, actor.PrincipalID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		return nil, err
	}
	return struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}{m.ID, "user_deleted"}, nil
}
