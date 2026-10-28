package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

const consoleSetupCodePath = "/api/console/organizations/{org}/setup_code/redeem"

// MintSetupCode returns a replica's first-admin code only before bootstrap.
// An admin-granting map invalidates outstanding codes instead. Only the hash
// is stored; the caller must deliver the code through a restricted file,
// never a log. Redeeming any replica's code spends them all.
func MintSetupCode(ctx context.Context, pool *pgxpool.Pool, mapGrantsAdmin bool) (string, error) {
	return mintSetupCode(ctx, pool, mapGrantsAdmin, "identity")
}

// MintPasswordSetupCode is development-only setup. An active local human admin
// closes it even if their password row was lost. First-admin history in another
// mode does not close this exception. Startup validates the development config.
func MintPasswordSetupCode(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	return mintSetupCode(ctx, pool, false, "password")
}

const localAdminExists = `EXISTS (SELECT 1 FROM principals WHERE issuer='urn:managed-agent-platform:local'
	AND kind='user' AND status='active' AND role='admin')`

func mintSetupCode(ctx context.Context, pool *pgxpool.Pool, mapGrantsAdmin bool, purpose string) (string, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Match redemption's lock order. Principal writes record the first admin
	// through 0056's triggers, so neither mint nor redemption may overtake one.
	if _, err := tx.Exec(ctx, `LOCK TABLE principals, setup_codes IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return "", err
	}
	if mapGrantsAdmin {
		if _, err := tx.Exec(ctx, `DELETE FROM setup_codes`); err != nil {
			return "", err
		}
		return "", tx.Commit(ctx)
	}
	var bootstrapped bool
	closedQuery := `SELECT EXISTS (SELECT 1 FROM install_bootstrap)`
	if purpose == "password" {
		closedQuery = `SELECT ` + localAdminExists
	}
	if err := tx.QueryRow(ctx, closedQuery).Scan(&bootstrapped); err != nil {
		return "", err
	}
	if bootstrapped {
		return "", nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM setup_codes WHERE expires_at <= clock_timestamp()`); err != nil {
		return "", err
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	sum := sha256.Sum256([]byte(code))
	// Use the database's redemption clock after lock waits, once for both
	// timestamps. Replica clock skew and transaction start do not shorten TTL.
	if _, err := tx.Exec(ctx, `WITH minted AS (SELECT clock_timestamp() AS created_at)
		INSERT INTO setup_codes (code_hash, created_at, expires_at, purpose)
		SELECT $1, created_at, created_at + interval '1 hour', $2 FROM minted`, hex.EncodeToString(sum[:]), purpose); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return code, nil
}

func (s *server) redeemSetup(w http.ResponseWriter, r *http.Request) {
	if s.nativeLogin() && s.login.Mode() == login.ModePassword {
		s.redeemPasswordSetup(w, r)
		return
	}
	s.handle(identity.RoleUser, s.redeemSetupCode)(w, r)
}

func (s *server) redeemSetupCode(r *http.Request) (any, error) {
	if s.nativeLogin() {
		if _, err := requireCookieSession(r); err != nil {
			return nil, err
		}
		if err := loginJSON(r); err != nil {
			return nil, err
		}
	}
	p, ok := identityFrom(r.Context())
	if !ok {
		return nil, errForbidden("only a signed-in person can redeem a setup code")
	}
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	ctx := r.Context()
	// The flag never clears. A closed install can refuse without a table lock
	// that would serialize unrelated sign-ins, or reading an irrelevant body.
	var bootstrapped bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM install_bootstrap)`).Scan(&bootstrapped); err != nil {
		return nil, err
	}
	if bootstrapped {
		return nil, errConflict("an admin already exists")
	}
	if s.setupMapGrantsAdmin {
		return nil, errConflict("the identity provider's mapping supplies administrators")
	}
	obj, bodyErr := decodeObject(r)
	var code string
	if bodyErr == nil {
		code, bodyErr = requiredString(obj, "code")
	}
	sum := sha256.Sum256([]byte(code))
	hash := hex.EncodeToString(sum[:])
	// Reject bad inputs without taking locks that block principal writers.
	// Read both facts in one snapshot after the body: a completed bootstrap
	// takes priority over body errors and codes spent while the client wrote.
	// These prechecks only reject; promotion still needs every locked recheck.
	var valid bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM install_bootstrap),
		EXISTS (SELECT 1 FROM setup_codes WHERE code_hash = $1 AND purpose = 'identity' AND expires_at > clock_timestamp())`,
		hash).Scan(&bootstrapped, &valid); err != nil {
		return nil, err
	}
	if bootstrapped {
		return nil, errConflict("an admin already exists")
	}
	if bodyErr != nil {
		return nil, bodyErr
	}
	if !valid {
		return nil, errInvalid("invalid or expired setup code")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Principal writers take their table lock before the admin trigger writes
	// install_bootstrap. Take principals first too, then setup_codes. This short
	// transaction excludes mapping, deactivation and SQL recovery until commit;
	// a row lock on a code cannot serialize different replicas' codes.
	if _, err := tx.Exec(ctx, `LOCK TABLE principals, setup_codes IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM install_bootstrap)`).Scan(&bootstrapped); err != nil {
		return nil, err
	}
	if bootstrapped {
		return nil, errConflict("an admin already exists")
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM setup_codes
		WHERE code_hash = $1 AND purpose = 'identity' AND expires_at > clock_timestamp())`, hash).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, errInvalid("invalid or expired setup code")
	}
	var kind, status, source string
	err = tx.QueryRow(ctx, `SELECT kind, status, role_source FROM principals WHERE id = $1`, p.ID).Scan(&kind, &status, &source)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (kind != "user" || status != "active")) {
		return nil, errForbidden("only an active user principal can redeem a setup code")
	}
	if err != nil {
		return nil, err
	}
	if source == "idp" {
		return nil, errConflict("your role comes from the identity provider's mapping")
	}
	// The promotion's trigger permanently records bootstrap, in this same
	// transaction. Demoting or deactivating this person never reopens setup.
	if _, err := tx.Exec(ctx, `UPDATE principals SET role = 'admin', role_source = 'console' WHERE id = $1`, p.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM setup_codes`); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	id := p.ID
	if s.nativeLogin() {
		id = principal.PublicID(p.ID, "user")
	}
	return struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}{id, "admin"}, nil
}

func (s *server) redeemPasswordSetup(w http.ResponseWriter, r *http.Request) {
	if s.login.CheckOrigin(r) != nil {
		writeError(w, r, errForbidden("invalid browser origin"))
		return
	}
	if err := loginJSON(r); err != nil {
		writeError(w, r, err)
		return
	}
	if err := consoleOrganization(r); err != nil {
		writeError(w, r, err)
		return
	}
	id, err := s.createSetupLocalAdmin(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.issueLogin(w, r, id)
}

func (s *server) createSetupLocalAdmin(r *http.Request) (string, error) {
	ctx := r.Context()
	var closed bool
	if err := s.pool.QueryRow(ctx, `SELECT `+localAdminExists).Scan(&closed); err != nil {
		return "", err
	}
	if closed {
		return "", errConflict("an active local admin already exists")
	}
	obj, bodyErr := decodeObject(r)
	var code, name, password string
	if bodyErr == nil {
		code, bodyErr = requiredString(obj, "code")
	}
	if bodyErr == nil {
		name, password, bodyErr = localCredentials(obj)
	}
	sum := sha256.Sum256([]byte(code))
	hash := hex.EncodeToString(sum[:])
	var valid bool
	// Reject-only prechecks do not serialize unrelated sign-ins or hash work.
	if err := s.pool.QueryRow(ctx, `SELECT `+localAdminExists+`, EXISTS (SELECT 1 FROM setup_codes
		WHERE code_hash=$1 AND purpose='password' AND expires_at>clock_timestamp())`, hash).Scan(&closed, &valid); err != nil {
		return "", err
	}
	if closed {
		return "", errConflict("an active local admin already exists")
	}
	if bodyErr != nil {
		return "", bodyErr
	}
	if err := s.throttleLogin(r, name); err != nil {
		return "", err
	}
	if !valid {
		return "", errInvalid("invalid or expired setup code")
	}
	encoded, err := login.HashPassword(password)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// Hashing is outside this short transaction. Preserve principal → code lock
	// order, then recheck eligibility and expiry with post-wait database time.
	if _, err := tx.Exec(ctx, `LOCK TABLE principals, setup_codes IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `SELECT `+localAdminExists).Scan(&closed); err != nil {
		return "", err
	}
	if closed {
		return "", errConflict("an active local admin already exists")
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM setup_codes
		WHERE code_hash=$1 AND purpose='password' AND expires_at>clock_timestamp())`, hash).Scan(&valid); err != nil {
		return "", err
	}
	if !valid {
		return "", errInvalid("invalid or expired setup code")
	}
	id := domain.NewID(domain.PrefixPrincipal).String()
	_, err = tx.Exec(ctx, `INSERT INTO principals(id,issuer,subject,role,role_source)
		VALUES ($1,$2,$3,'admin','console')`, id, login.LocalIssuer, name)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return "", errConflict("this login name is unavailable")
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO local_accounts(principal_id,password_hash) VALUES ($1,$2)`, id, encoded); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM setup_codes`); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}
