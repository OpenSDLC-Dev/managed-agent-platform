// Package modelgateway is the model gateway's HTTP surface
// (docs/plan/59_model-gateway.md): Anthropic Messages, OpenAI's Chat
// Completions and Embeddings, and rerank for a platform API key, routed to
// the deployments an alias names, beside the admin API it mounts under
// /admin/v1/.
//
// Inference routes, the Anthropic ones also under an /anthropic prefix and
// the OpenAI ones under /openai:
//
//	POST /v1/messages               streamed and not
//	POST /v1/messages/count_tokens
//	POST /v1/chat/completions       OpenAI's; streamed and not
//	POST /v1/embeddings             OpenAI's, to an embedding alias
//	POST /v1/rerank                 the Jina/Cohere shape, to a rerank alias;
//	                                answered in OpenAI's envelope
//	GET  /v1/models                 Anthropic's shape for a request that
//	                                carries anthropic-version or the prefix,
//	                                OpenAI's otherwise
//	GET  /v1/models/{id}
//
// A request authenticates with a platform API key in x-api-key or as a
// Bearer, checked by internal/apikey exactly as the control plane checks it,
// and calls a model only under the key policy an administrator wrote for the
// key. The bootstrap key — the value the control plane registers as
// "bootstrap" — needs no policy, being known by its configured value; a policy
// written for it still applies, and like any key it calls nothing once its row
// is archived or expired.
//
// Each inference path is a passthrough to an upstream speaking its protocol:
// the body is read only as far as its top-level keys, model becomes the
// deployment's upstream id, and everything else goes upstream as sent —
// anthropic-* headers included on the Anthropic path, no caller header on
// the OpenAI one (chat.go). The answer comes back event by event as it
// arrives, with its model rewritten to the name the caller sent. Errors
// answer in the route's protocol's envelope; an upstream's own error keeps
// its status and body, the call's credential removed from it.
package modelgateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/upstream"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// Config is what the gateway serves from.
type Config struct {
	Catalog      *catalog.Catalog
	Store        *store.Store   // the ledger and the rate windows
	Keys         apikey.Querier // the platform database, whose api_keys a caller's key is checked against
	Cipher       secrets.Cipher // opens the credentials the catalog holds sealed
	BootstrapKey string         // CONTROLPLANE_API_KEY
	Admin        http.Handler   // served under /admin/v1/; nil serves none

	// Client calls upstreams; nil takes upstream.NewClient.
	Client *http.Client
	// MaxAttempts bounds the calls one request makes to each deployment
	// before it falls back to the next; zero takes DefaultMaxAttempts.
	MaxAttempts int
	// Backoff is the first retry's ceiling, doubling per attempt up to
	// MaxBackoff; zero takes DefaultBackoff.
	Backoff time.Duration
}

// The retry defaults: three attempts per deployment, a first wait of at most
// 200ms, and no wait longer than two seconds, all before the caller has seen
// a byte.
const (
	DefaultMaxAttempts = 3
	DefaultBackoff     = 200 * time.Millisecond
	MaxBackoff         = 2 * time.Second
)

type handler struct {
	cfg       Config
	bootstrap [32]byte
	client    *http.Client
	fresh     *http.Client // client keeping no connection (profile.CloseConnections)
	draw      func() float64

	mu         sync.Mutex
	opened     map[string]openedKey // by credential id
	prunedFrom *catalog.Snapshot    // the snapshot opened was last pruned against
}

type openedKey struct {
	providerID string
	key        []byte
}

// open returns a credential's key, opening it once: under openbao or gcpkms
// every Decrypt is a round trip to the key service, which a model call should
// not wait on, nor pay for, each time. A credential's sealed value never
// changes — a new key is a new credential — so its id names the key for good,
// until a snapshot no longer holds it: the first open after a new snapshot
// prunes the keys of the credentials it lost, so a deleted credential's key
// stays in memory until the next request at most, and serves no request
// routed after the deletion, which no longer names it.
func (h *handler) open(ctx context.Context, c store.Credential) ([]byte, error) {
	snap := h.cfg.Catalog.Snapshot()
	h.mu.Lock()
	if snap != h.prunedFrom {
		for id, k := range h.opened {
			if !slices.ContainsFunc(snap.Credentials(k.providerID), func(c store.Credential) bool { return c.ID == id }) {
				delete(h.opened, id)
			}
		}
		h.prunedFrom = snap
	}
	k, ok := h.opened[c.ID]
	h.mu.Unlock()
	if ok {
		return k.key, nil
	}
	key, err := h.cfg.Cipher.Decrypt(ctx, c.Ciphertext, c.KeyID)
	if err != nil {
		return nil, err
	}
	// A request routed on an older snapshot can open a credential after a
	// later request pruned it: the key serves that request, and is kept only
	// while the current snapshot holds the credential. A snapshot that drops
	// it after this check is pruned by the next open.
	h.mu.Lock()
	if slices.ContainsFunc(h.cfg.Catalog.Snapshot().Credentials(c.ProviderID), func(k store.Credential) bool { return k.ID == c.ID }) {
		h.opened[c.ID] = openedKey{providerID: c.ProviderID, key: key}
	}
	h.mu.Unlock()
	return key, nil
}

// New returns the gateway's handler.
func New(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Catalog == nil:
		return nil, errors.New("modelgateway: a catalog is required")
	case cfg.Store == nil:
		return nil, errors.New("modelgateway: a store is required for the ledger and the limits")
	case cfg.Keys == nil:
		return nil, errors.New("modelgateway: the platform database is required")
	case cfg.Cipher == nil:
		return nil, errors.New("modelgateway: a secrets cipher is required")
	case cfg.BootstrapKey == "":
		return nil, errors.New("modelgateway: the bootstrap key is required")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = DefaultBackoff
	}
	h := &handler{cfg: cfg, bootstrap: sha256.Sum256([]byte(cfg.BootstrapKey)), client: cfg.Client,
		draw: func() float64 { return 1 - mrand.Float64() }, opened: map[string]openedKey{}}
	if h.client == nil {
		h.client = upstream.NewClient()
	}
	h.fresh = upstream.NoReuse(h.client)
	if h.fresh == h.client {
		slog.Warn("modelgateway: the client's transport is not an *http.Transport, so a profile that closes its connections reuses them")
	}
	return h, nil
}

type requestIDKey struct{}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := serverSpan(r)
	sw := &statusWriter{ResponseWriter: w}
	defer func() { endServerSpan(span, sw.status) }()
	w = sw
	rid := newRequestID()
	w.Header().Set("request-id", rid)
	r = r.WithContext(context.WithValue(ctx, requestIDKey{}, rid))
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		if h.cfg.Admin == nil {
			writeError(w, r, notFound("no such path: %s", r.URL.Path))
			return
		}
		h.cfg.Admin.ServeHTTP(w, r)
		return
	}
	path, prefix := r.URL.Path, ""
	for _, p := range []string{"/anthropic", "/openai"} {
		if rest, ok := strings.CutPrefix(path, p); ok && strings.HasPrefix(rest, "/v1/") {
			path, prefix = rest, p
			break
		}
	}
	// Each route answers in its protocol's shape, errors included: the
	// prefix names the protocol, Chat Completions, Embeddings and rerank are
	// OpenAI's, and the models root is Anthropic's for a request carrying
	// anthropic-version, which every Anthropic SDK sends and no OpenAI SDK
	// does.
	models := path == "/v1/models" || strings.HasPrefix(path, "/v1/models/")
	rt, inferring := routes[path]
	openAI := prefix == "/openai" || prefix == "" && (inferring && rt.proto == profile.OpenAI || models && r.Header.Get("anthropic-version") == "")
	r = r.WithContext(context.WithValue(r.Context(), openAIKey{}, openAI))
	// Authenticate before routing, so an unauthenticated caller learns
	// nothing about which paths exist.
	c, err := h.authenticate(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	switch {
	case inferring && rt.servedUnder(prefix):
		if r.Method != http.MethodPost {
			writeError(w, r, notAllowed(r.Method, "POST"))
			return
		}
		h.inference(w, r, c, path)
	case models:
		if r.Method != http.MethodGet {
			writeError(w, r, notAllowed(r.Method, "GET"))
			return
		}
		id, one := strings.CutPrefix(path, "/v1/models/")
		switch {
		case openAI && one:
			h.getOpenAIModel(w, r, c, id)
		case openAI:
			h.listOpenAIModels(w, r, c)
		case one:
			h.getModel(w, r, c, id)
		default:
			h.listModels(w, r, c)
		}
	default:
		writeError(w, r, notFound("no such path: %s", r.URL.Path))
	}
}

type openAIKey struct{}

// openAI reports whether r is answered in OpenAI's shapes.
func openAI(r *http.Request) bool {
	v, _ := r.Context().Value(openAIKey{}).(bool)
	return v
}

// apiError is an error the gateway answers itself, in the envelope of the
// request's protocol (writeError).
type apiError struct {
	status int
	typ    string
	msg    string
}

func invalid(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *apiError {
	return &apiError{http.StatusNotFound, "not_found_error", fmt.Sprintf(format, args...)}
}

func forbidden(format string, args ...any) *apiError {
	return &apiError{http.StatusForbidden, "permission_error", fmt.Sprintf(format, args...)}
}

func unauthenticated(msg string) *apiError {
	return &apiError{http.StatusUnauthorized, "authentication_error", msg}
}

func notAllowed(method, allowed string) *apiError {
	return &apiError{http.StatusMethodNotAllowed, "invalid_request_error",
		fmt.Sprintf("%s is not allowed on this path; it takes %s", method, allowed)}
}

func internal(r *http.Request, what string, err error) *apiError {
	slog.ErrorContext(r.Context(), "modelgateway: "+what, "method", r.Method, "path", r.URL.Path, "error", err)
	return &apiError{http.StatusInternalServerError, "api_error", "internal error"}
}

// writeError answers e in Anthropic's error envelope, or in OpenAI's —
// {"error": {message, type, param, code}}, whose four fields openai-go's
// Error requires — on an OpenAI route. Both keep Anthropic's error types,
// which name the failure in either. The request id is in the request-id
// header either way.
func writeError(w http.ResponseWriter, r *http.Request, e *apiError) {
	markError(r.Context(), e.typ)
	if openAI(r) {
		writeJSON(w, e.status, map[string]any{"error": map[string]any{"message": e.msg, "type": e.typ, "param": nil, "code": nil}})
		return
	}
	writeJSON(w, e.status, map[string]any{
		"type":       "error",
		"error":      map[string]string{"type": e.typ, "message": e.msg},
		"request_id": requestID(r),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requestID(r *http.Request) string {
	rid, _ := r.Context().Value(requestIDKey{}).(string)
	return rid
}

// idEncoding is the platform's id alphabet, so a request id looks like the
// control plane's; the gateway mints its own, never importing internal/domain.
var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func newRequestID() string {
	b := make([]byte, 15)
	_, _ = rand.Read(b)
	return "req_" + idEncoding.EncodeToString(b)
}

// backoff waits before attempt n (1-based retries): a uniform draw under a
// ceiling that doubles from Backoff to MaxBackoff — full jitter, so replicas
// retrying one failing upstream do not do it in step. It reports false when
// ctx ends first.
func (h *handler) backoff(ctx context.Context, n int) bool {
	if ctx.Err() != nil {
		return false // gone already, whichever of the two a select would pick
	}
	ceiling := min(float64(h.cfg.Backoff)*math.Pow(2, float64(n-1)), float64(MaxBackoff))
	t := time.NewTimer(time.Duration(h.draw() * ceiling))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
