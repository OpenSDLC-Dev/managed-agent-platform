package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hashKey derives the stored form of an API key. Only this hash ever touches
// the database.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// IssuedKeyPrefix marks a management key this platform minted, beside plan 30's
// `sk-map-env01-` for worker credentials. It is public by construction: it is the
// same for every key, so showing it identifies the *kind* of credential without
// revealing anything about a particular one.
const IssuedKeyPrefix = "sk-map-api01-"

// partialKeyHint is the masked form a listing shows. It is computed from the
// plaintext because it cannot be recovered from the hash, and it is the only part
// of a key that outlives issuance.
//
// Two rules, because there are two kinds of value here and only one has a format
// this platform knows.
//
// A key we minted carries IssuedKeyPrefix, which is public, so the hint may show
// it in full and then three characters, an ellipsis, and the final four —
// mirroring the reference's `sk-ant-api03-R2D...igAA`. The split is taken at a
// fixed offset from the *known* prefix, never at the last separator in the value:
// the body is base64url (envkeys.go mints with base64.RawURLEncoding), whose
// alphabet includes `-`, so a last-separator rule would move the split to wherever
// a random dash happened to land and could leave a key almost entirely published.
//
// A CONTROLPLANE_API_KEY is operator-chosen and may be anything, so NOTHING in it
// may be assumed public — its leading run is not a prefix this platform knows, and
// reading it as one would render `secret-12345678` as `secret-123...5678`,
// fourteen of its fifteen characters. Such a value gets its last four characters
// only.
//
// Both paths refuse to produce a hint at all below a length floor, because "a
// masked value that is mostly the value" is worse than an empty column — and worse
// than it looks, since key_hash is an unsalted SHA-256 that a mostly-known
// plaintext makes trivially searchable offline.
//
// Slicing is by rune, not byte. A key is an arbitrary environment-variable string
// and may be non-ASCII; cutting mid-rune yields invalid UTF-8, which Postgres
// refuses on a text column — an EnsureAPIKey error at boot, i.e. a control plane
// that will not start because of the *hint*. A value that is not valid UTF-8 at
// all gets no hint for the same reason.
func partialKeyHint(key string) string {
	const lead, tail = 3, 4
	// An issued key's body must hide at least as much as it shows; an opaque one,
	// where nothing is public, at least three times as much. Minted bodies are 43
	// base64url characters, so the issued floor is slack rather than a constraint —
	// which is the point: the rule stays safe by construction, not by luck.
	const minIssuedBody, minOpaque = 2 * (lead + tail), 4 * tail
	if !utf8.ValidString(key) {
		return ""
	}
	if strings.HasPrefix(key, IssuedKeyPrefix) {
		body := []rune(key[len(IssuedKeyPrefix):])
		if len(body) < minIssuedBody {
			return ""
		}
		return IssuedKeyPrefix + string(body[:lead]) + "..." + string(body[len(body)-tail:])
	}
	r := []rune(key)
	if len(r) < minOpaque {
		return ""
	}
	return "..." + string(r[len(r)-tail:])
}

// EnsureAPIKey makes key the one live credential for the named logical key:
// it inserts (or reactivates) the hash and archives every other live key
// under the same name. That gives rotation-by-restart semantics — changing
// CONTROLPLANE_API_KEY and restarting cmd/controlplane retires the previous
// key instead of leaving it valid forever. All replicas must therefore share
// one key value per name; replicas booting with *different* values for one name
// race, and api_keys_one_live_unissued resolves that by failing the loser's
// transaction rather than leaving the name with two live credentials.
//
// It only ever writes rows with created_by NULL, which is what puts them under
// that index and marks them env-var-managed. A key issued over the console
// records its issuer and is deliberately outside the one-live rule (plan 32).
//
// This is EnsureAPIKeyInWorkspace in `default`, the workspace 0035 seeds and
// the only one a single-tenant deployment has — what a caller that never chose
// a workspace is asking for.
func EnsureAPIKey(ctx context.Context, pool *pgxpool.Pool, name, key string) error {
	return EnsureAPIKeyInWorkspace(ctx, pool, domain.DefaultWorkspaceID, name, key)
}

// keyAdoptionLockWait bounds the wait for the adoption lock below — and, since
// SET LOCAL runs for the whole transaction, every row lock it takes afterwards.
// A fleet of replicas booting together all hash the SAME configured value, so
// they all queue on one lock, and the boot context has no deadline of its own:
// an unbounded wait is a control plane that hangs before it serves, which the
// Helm liveness probe turns into a CrashLoopBackOff carrying no diagnostic at
// all. Bounded, a wedged adopter fails loudly and names the lock. A var only
// for the test setter.
var keyAdoptionLockWait = 10 * time.Second

// EnsureAPIKeyInWorkspace is EnsureAPIKey binding the credential to a named
// workspace, which is what a management key resolves its scope from (plan 42
// §6.1).
//
// A value configured here that already exists as another workspace's key MOVES
// to this one: the upsert's conflict arm carries workspace_id across, so the
// credential cannot go on authenticating into the workspace it was first
// configured in. That is the deliberate reading of one secret, one workspace
// (§6.8) — the conflict target stays (key_hash) and its global UNIQUE stays
// with it. Re-targeting the conflict on (org_id, workspace_id, key_hash) is the
// tempting alternative and the wrong one: with the global UNIQUE still in place
// the second workspace's insert would raise a uniqueness violation rather than
// conflict, failing the boot and turning a misconfiguration into an existence
// oracle for the first workspace's key; and dropping that UNIQUE to avoid the
// violation would let one secret authenticate into two workspaces, which is the
// premise tenancy rests on.
func EnsureAPIKeyInWorkspace(ctx context.Context, pool *pgxpool.Pool, workspace, name, key string) error {
	hash := hashKey(key)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Adopters of one value serialize here, so the SELECT below sees whatever a
	// concurrent boot committed. Without it two replicas configuring the same
	// never-seen value into different workspaces would each find no row, neither
	// would warn, and the later upsert would pick the tenant silently — the one
	// move this function promises to be loud about. Transaction-scoped, so the
	// commit or the deferred rollback releases it; keyed in SQL so a test can
	// hold the same lock without repeating a Go-side derivation.
	//
	// SET LOCAL takes no bind parameter, so the milliseconds are formatted into
	// the statement text; the value is this package's, never a caller's.
	if _, err := tx.Exec(ctx,
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", keyAdoptionLockWait.Milliseconds())); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, hash); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
			return fmt.Errorf("another replica is adopting the management key %q and has held the adoption lock past %s: %w",
				name, keyAdoptionLockWait, err)
		}
		return err
	}
	// The destination has to be a workspace this deployment still runs, checked
	// after the lock and before anything is written. The upsert writes
	// workspace_id exactly as given, and authenticate folds a missed registry
	// join into the unknown-key 401 by design (§6.1) — so without this check a
	// typo, or a destination archived since the deployment was configured,
	// boots clean and then answers every request `invalid x-api-key`, with
	// nothing anywhere naming the workspace at fault.
	var live int
	switch err := tx.QueryRow(ctx,
		`SELECT 1 FROM workspaces WHERE id = $1 AND archived_at IS NULL`, workspace).Scan(&live); {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("workspace %q is not a live workspace in this deployment", workspace)
	case err != nil:
		return err
	}
	// Adopting a key somebody issued from the console is the right outcome (see the
	// ON CONFLICT clause below) but it must not be a silent one. An operator who
	// pasted an archived console key out of an old runbook has just brought a
	// retired credential back, and since this runs once at boot there is nothing
	// else that would ever mention it. Refusing to boot instead was considered and
	// rejected: it would strand the control plane behind a state only a direct
	// database edit could clear, and it protects nobody — setting the variable at
	// all requires the deployment access that could equally configure a fresh
	// value. So: adopt, and say so.
	//
	// A cross-workspace move is loud for the same reason and independently of
	// the issuer: the row is about to stop answering for the workspace it was
	// configured in, and boot is the only moment anything could say so.
	var priorIssuer *string
	var priorStatus, priorWorkspace string
	switch err := tx.QueryRow(ctx,
		`SELECT created_by, status, workspace_id FROM api_keys WHERE key_hash = $1`, hash).
		Scan(&priorIssuer, &priorStatus, &priorWorkspace); {
	case err == pgx.ErrNoRows: // a value this deployment has never seen
	case err != nil:
		return err
	default:
		if priorIssuer != nil {
			slog.WarnContext(ctx, "configured management key already existed as a console-issued key; adopting it as env-var-managed",
				"name", name, "issued_by", *priorIssuer, "previous_status", priorStatus)
		}
		if priorWorkspace != workspace {
			slog.WarnContext(ctx, "configured management key already existed in another workspace; moving it",
				"name", name, "previous_workspace", priorWorkspace, "workspace", workspace)
		}
	}
	// Archive before inserting: api_keys_one_live_unissued admits one active
	// unissued row per name and Postgres enforces it per statement, so
	// registering the replacement while the incumbent is still live would fail
	// every rotation.
	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET status = 'archived'
		 WHERE name = $1 AND key_hash <> $2 AND status = 'active' AND created_by IS NULL`,
		name, hash); err != nil {
		return err
	}
	// created_by and expires_at are RESET, not preserved, when an existing row is
	// adopted. The conflict path fires when the configured value already exists as
	// a row, and that row may have been issued over the console — carrying an
	// issuer and possibly an expiry. Leaving either in place would break both
	// halves of this function's contract: an issued row sits outside
	// api_keys_one_live_unissued, so the archive above would skip it on the next
	// rotation and leave two live credentials under one name; and an inherited
	// expiry would let EnsureAPIKey report success over a key authenticate then
	// refuses, i.e. a control plane that starts without a working bootstrap
	// credential. Naming a value in CONTROLPLANE_API_KEY makes it env-var-managed,
	// whatever it was before.
	// workspace_id is set on both arms, which is what makes a value configured in
	// a second workspace a move rather than a silent cross-tenant credential.
	// org_id and project_id are not: they are frozen at 'default' (plan 42
	// decision 3), so naming them would be a column this platform cannot vary.
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, partial_key_hint, workspace_id) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (key_hash) DO UPDATE
		 SET status = 'active', name = EXCLUDED.name, partial_key_hint = EXCLUDED.partial_key_hint,
		     workspace_id = EXCLUDED.workspace_id, created_by = NULL, expires_at = NULL`,
		domain.NewID(domain.PrefixAPIKey).String(), name, hash, partialKeyHint(key), workspace); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// apiKeyPrincipal is what an x-api-key resolves to: the row's id (the audit
// principal), the tenant the row binds, and whether it is the env-var-managed
// bootstrap key. A zero ID means the key did not resolve at all.
type apiKeyPrincipal struct {
	ID        string
	Scope     domain.Scope
	Bootstrap bool
}

// authenticate resolves an x-api-key value to its principal, or the zero
// principal if the key is unknown, not active, past its expiry, or bound to a
// workspace that is no longer live.
//
// Expiry is evaluated here rather than swept: a key whose expires_at has passed
// stops authenticating the moment it passes, with no background job to be down.
// The comparison is against the database's clock, the same one that stamped
// created_at, so a control-plane replica with a skewed clock cannot extend or
// shorten a credential's life.
//
// The workspace join is the same kind of condition and deliberately shares the
// branch: a key whose workspace has been archived — or that names no registry
// row at all — is refused with the message an unknown key gets, so archiving a
// tenant discloses nothing about which of its credentials existed (plan 42
// §6.1, and the reference's own answer, §6.9). created_by rides along because
// nothing else can reach it: the bootstrap marker is a column, and this is the
// only query that reads the row (§6.8).
//
// That join is COMPOSITE — workspace and org both — because org has exactly one
// authority, the registry. A key row whose org_id drifted from its workspace's
// names a tenant no workspace agrees with, so it resolves to nothing rather
// than to whichever half the query happened to read; 0035's UNIQUE (org_id, id)
// is the index it lands on.
func authenticate(ctx context.Context, pool *pgxpool.Pool, key string) (apiKeyPrincipal, error) {
	var p apiKeyPrincipal
	err := pool.QueryRow(ctx,
		`SELECT k.id, k.org_id, k.workspace_id, k.project_id, k.created_by IS NULL
		   FROM api_keys k
		   JOIN workspaces w ON w.id = k.workspace_id AND w.org_id = k.org_id AND w.archived_at IS NULL
		 WHERE k.key_hash = $1 AND k.status = 'active'
		   AND (k.expires_at IS NULL OR k.expires_at > now())`,
		hashKey(key)).Scan(&p.ID, &p.Scope.OrgID, &p.Scope.WorkspaceID, &p.Scope.ProjectID, &p.Bootstrap)
	if err == pgx.ErrNoRows {
		return apiKeyPrincipal{}, nil
	}
	return p, err
}

// requireAPIKey is the management-auth middleware: every /v1 route needs a
// valid, unrevoked x-api-key. The authenticated key's ID is stored in the
// request context as the audit principal (sessions.created_by), beside the
// scope it resolved and the bootstrap marker.
//
// A management key covers exactly one workspace, so the header can only name
// that one; selectWorkspace answers the other cases. Both tenancy headers are
// stamped the moment the scope resolves and before anything writes, so they
// are present on a 200 and on whatever 4xx the route answers, and absent on
// the 401s above — which is the whole of the schedule (plan 42 §6.2).
func requireAPIKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A repeated field is refused before the value is read. HTTP allows one,
		// no real client sends one, and Header.Get would silently pick the first —
		// so without this the answer to "which key authenticated?" would depend on
		// header order, and it would differ from the answer apiKeyOffered gives
		// when choosing the lane. One rule in both places: a duplicate credential
		// is ambiguous, and ambiguous is a 401.
		if len(r.Header.Values("x-api-key")) > 1 {
			writeError(w, r, errAuth("multiple x-api-key headers"))
			return
		}
		key := r.Header.Get("x-api-key")
		if key == "" {
			writeError(w, r, errAuth("missing x-api-key header"))
			return
		}
		principal, err := authenticate(r.Context(), pool, key)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if principal.ID == "" {
			writeError(w, r, errAuth("invalid x-api-key"))
			return
		}
		scope, err := selectWorkspace(r, []domain.Scope{principal.Scope})
		if err != nil {
			writeError(w, r, err)
			return
		}
		stampScope(w, scope)
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, principal.ID)
		ctx = context.WithValue(ctx, ctxKeyScope, scope)
		ctx = context.WithValue(ctx, ctxKeyBootstrapKey, principal.Bootstrap)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalFrom is the audit answer to "who made this request" — the value
// sessions.created_by records. It resolves either lane's principal: the api key's
// row id on the machine lane — `authenticate` returns `id`, and the comment here
// said "name" until plan 32 gave the console a resource that renders the value as
// an actor and made the difference visible — and the human's `principal_` id on
// the identity lane (plan 31 slice 2, #56).
//
// Reading only ctxKeyPrincipal was the pre-plan-31 shape, when a machine key was
// the only thing that could reach a mutation. Left that way, the moment slice 3
// lets a human create a session the row would record NO creator at all — silently,
// since created_by is nullable and nothing checks it. The whole point of a stable
// `principal_` id is that an audit trail can name the human it belongs to.
//
// The machine lane wins when both are somehow set, matching dispatch: only one is
// ever populated today, and if that ever changed, the credential that
// authenticated the request is the machine one.
func principalFrom(ctx context.Context) string {
	if p, _ := ctx.Value(ctxKeyPrincipal).(string); p != "" {
		return p
	}
	if p, ok := identityFrom(ctx); ok {
		return p.ID
	}
	return ""
}

// scopeFrom is the tenancy answer to "whose data may this request touch" — the
// scope the credential resolved to, attached by whichever resolver
// authenticated it. Every scoped query reads it from here; no handler ever
// computes one (plan 42 §6.1).
//
// !ok IS AN INTERNAL ERROR at every call site, never "unscoped, proceed". A
// handler reaching for a scope it was not given has been dispatched behind a
// resolver that does not set one, which is a wiring defect: serving the request
// anyway would serve it across every tenant. Fail the request instead.
func scopeFrom(ctx context.Context) (domain.Scope, bool) {
	s, ok := ctx.Value(ctxKeyScope).(domain.Scope)
	return s, ok
}

// bootstrapKeyFrom reports whether this request authenticated with the
// env-var-managed management key — the api_keys row with created_by IS NULL,
// the same predicate api_keys_one_live_unissued keys on (plan 42 §6.8). Absent
// means false, which is the safe answer: every other credential, and every
// unauthenticated request, is not the bootstrap key.
func bootstrapKeyFrom(ctx context.Context) bool {
	b, _ := ctx.Value(ctxKeyBootstrapKey).(bool)
	return b
}
