// Package store is the model gateway's queries over the modelgateway schema
// (migration 0049; docs/plan/59_model-gateway.md, "Configuration model").
//
// Every configuration write runs in one transaction that ends with a
// notification on NotifyChannel, so each replica's catalog reloads once the
// write is visible; a write that fails commits neither. The ledger and the
// rate windows (usage.go, limits.go) are not configuration, and announce
// nothing. The invariants that span rows are held
// here, inside that transaction, rather than by each caller: a credential
// names only protocols its provider has an endpoint for; an alias's targets
// share one kind, which is the alias's from creation on; an embedding alias
// keeps the one deployment it was created with; a key policy names only
// aliases that exist, and an alias a policy names cannot be deleted. The rows
// those checks read are locked, so a concurrent write cannot slip between the
// check and the write.
//
// An update takes a function that edits a copy of the locked row; only the
// fields the plan lets change are written back, whatever the function did to
// the rest.
package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotifyChannel is where every committed write is announced.
const NotifyChannel = "modelgateway_config"

// Kind is what a deployment serves, and so what an alias over it serves.
type Kind string

const (
	KindChat      Kind = "chat"
	KindEmbedding Kind = "embedding"
	KindRerank    Kind = "rerank"
)

// The three ways a call fails on what it was asked, as distinct from a
// database failure: the row it names does not exist, the write would break a
// row that depends on it, or what it was given is not acceptable.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

type storeError struct {
	kind error
	msg  string
}

func (e *storeError) Error() string { return e.msg }
func (e *storeError) Unwrap() error { return e.kind }

func fail(kind error, format string, args ...any) error {
	return &storeError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// Provider is one vendor account behind fixed endpoints.
type Provider struct {
	ID           string
	Name         string
	Profile      string
	Endpoints    map[profile.Protocol]string // fixed at creation
	Headers      map[string]string
	StallTimeout time.Duration // zero: the gateway's default
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Credential is one key of a provider's account, sealed by internal/secrets.
type Credential struct {
	ID         string
	ProviderID string
	Kind       string // "api_key", the only kind
	Ciphertext []byte // fixed at creation
	KeyID      string // fixed at creation
	LastFour   string // fixed at creation
	Protocols  []profile.Protocol
	Weight     int
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Capabilities are what /v1/models answers for a deployment.
type Capabilities struct {
	Tools          bool  `json:"tools"`
	Thinking       bool  `json:"thinking"`
	Vision         bool  `json:"vision"`
	MaxInputTokens int64 `json:"max_input_tokens,omitempty"`
	MaxTokens      int64 `json:"max_tokens,omitempty"`
}

// Prices are per million tokens; nil is unpriced.
type Prices struct {
	Input      *float64
	Output     *float64
	CacheWrite *float64
	CacheRead  *float64
}

// Deployment is one upstream model on one provider.
type Deployment struct {
	ID            string
	ProviderID    string // fixed at creation
	UpstreamModel string // fixed at creation
	Kind          Kind   // fixed at creation
	DisplayName   string
	Capabilities  Capabilities
	Prices        Prices
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Target is one deployment an alias routes to: groups are tried in ascending
// Priority, and within a group a deployment is chosen by Weight.
type Target struct {
	DeploymentID string
	Priority     int
	Weight       int
}

// Alias is a model name a caller sends.
type Alias struct {
	Name        string
	DisplayName string
	Kind        Kind // its targets', fixed at creation
	Targets     []Target
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// KeyPolicy grants one platform API key the right to call models.
type KeyPolicy struct {
	APIKeyID  string
	Aliases   []string // nil: every alias
	RPM       *int32   // nil: no limit
	TPM       *int64   // nil: no limit
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Config is every row, read in one snapshot.
type Config struct {
	Providers   []Provider
	Credentials []Credential
	Deployments []Deployment
	Aliases     []Alias
	KeyPolicies []KeyPolicy
}

// Store runs the gateway's queries.
type Store struct{ pool *pgxpool.Pool }

// New returns a Store over pool, whose database the platform's migrations
// have already brought to the current schema.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// write runs fn in a transaction that announces itself on commit.
func (s *Store) write(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT pg_notify($1, '')`, NotifyChannel)
		return err
	})
}

// read runs fn in a read-only repeatable-read transaction: one snapshot for
// a read that takes more than one statement.
func (s *Store) read(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

// idEncoding is the platform's id alphabet (internal/domain's idAlphabet,
// Crockford base32 lowercased), so a gateway id looks like every other id;
// the prefixes are the gateway's own and outside the wire list.
var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func newID(prefix string) string {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		panic("modelgateway/store: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + idEncoding.EncodeToString(b)
}

// isForeignKeyViolation reports a write a concurrent insert raced: the row it
// would delete gained a dependant after the check that looked for one.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func protocolStrings(ps []profile.Protocol) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

// sortedProtocols orders protocols as the profiles do, anthropic first.
func sortedProtocols(ps []profile.Protocol) []profile.Protocol {
	out := slices.Clone(ps)
	slices.SortFunc(out, func(a, b profile.Protocol) int { return strings.Compare(string(a), string(b)) })
	return slices.Compact(out)
}
