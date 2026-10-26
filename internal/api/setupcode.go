package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const consoleSetupCodePath = "/api/console/organizations/{org}/setup_code/redeem"

// MintSetupCode returns a replica's first-admin code only before bootstrap,
// unless the IdP can supply admins. Only its hash is stored; the caller may
// print the returned code once. Redeeming any replica's code spends them all.
func MintSetupCode(ctx context.Context, pool *pgxpool.Pool, now time.Time, mapGrantsAdmin bool) (string, error) {
	if mapGrantsAdmin {
		return "", nil
	}
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
	var bootstrapped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM install_bootstrap)`).Scan(&bootstrapped); err != nil {
		return "", err
	}
	if bootstrapped {
		return "", nil
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	sum := sha256.Sum256([]byte(code))
	if _, err := tx.Exec(ctx, `INSERT INTO setup_codes (code_hash, expires_at) VALUES ($1, $2)`,
		hex.EncodeToString(sum[:]), now.Add(time.Hour)); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return code, nil
}

func (s *server) redeemSetupCode(r *http.Request) (any, error) {
	p, ok := identityFrom(r.Context())
	if !ok {
		return nil, errForbidden("only a signed-in person can redeem a setup code")
	}
	if err := consoleOrganization(r); err != nil {
		return nil, err
	}
	// Do not hold database locks while reading a client body. Its validation
	// waits until after the permanent flag check, so even a spent code gets 409.
	obj, bodyErr := decodeObject(r)
	ctx := r.Context()
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
	var bootstrapped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM install_bootstrap)`).Scan(&bootstrapped); err != nil {
		return nil, err
	}
	if bootstrapped {
		return nil, errConflict("an admin already exists")
	}
	if bodyErr != nil {
		return nil, bodyErr
	}
	code, err := requiredString(obj, "code")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(code))
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM setup_codes
		WHERE code_hash = $1 AND expires_at > clock_timestamp())`, hex.EncodeToString(sum[:])).Scan(&valid); err != nil {
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
	return struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}{p.ID, "admin"}, nil
}
