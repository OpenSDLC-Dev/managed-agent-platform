// Package admin is the model gateway's admin API, under /admin/v1/
// (docs/plan/59_model-gateway.md, "Auth, admin API and /v1/models"): the
// providers and their credentials, the deployments, the aliases and the key
// policies, and the compiled-in profiles read-only. No reference surface
// corresponds to it, so its shape is the platform's own, and
// managed-agent-console is its client.
//
// Two credentials reach it. The bootstrap key — CONTROLPLANE_API_KEY, read
// from the Secret the control plane reads it from — is compared in constant
// time against what the request carries in x-api-key or as a Bearer, and may
// do anything; a key an application issues under the name bootstrap is
// nothing to it, since it is never looked up in api_keys. An operator token,
// verified by internal/identity under the same IDENTITY_* configuration the
// control plane reads, reads with viewer or developer and writes with admin.
// Every other platform API key is refused: a key that could edit key policies
// could lift its own limits.
//
// Routes:
//
//	GET    /admin/v1/profiles                           list
//	GET    /admin/v1/profiles/{name}                    one
//	POST   /admin/v1/providers                          create
//	GET    /admin/v1/providers                          list
//	GET    /admin/v1/providers/{id}                     one
//	POST   /admin/v1/providers/{id}                     update
//	DELETE /admin/v1/providers/{id}                     delete (its deployments first)
//	POST   /admin/v1/providers/{id}/credentials         create
//	GET    /admin/v1/providers/{id}/credentials         list
//	GET    /admin/v1/providers/{id}/credentials/{cid}   one
//	POST   /admin/v1/providers/{id}/credentials/{cid}   update
//	DELETE /admin/v1/providers/{id}/credentials/{cid}   delete
//	POST   /admin/v1/deployments                        create
//	GET    /admin/v1/deployments                        list
//	GET    /admin/v1/deployments/{id}                   one
//	POST   /admin/v1/deployments/{id}                   update
//	DELETE /admin/v1/deployments/{id}                   delete (off every alias first)
//	POST   /admin/v1/aliases                            create
//	GET    /admin/v1/aliases                            list
//	GET    /admin/v1/aliases/{name...}                  one (a name may hold a slash)
//	POST   /admin/v1/aliases/{name...}                  update
//	DELETE /admin/v1/aliases/{name...}                  delete (off every key policy first)
//	GET    /admin/v1/key_policies                       list
//	GET    /admin/v1/key_policies/{api_key_id}          one
//	POST   /admin/v1/key_policies/{api_key_id}          create or replace
//	DELETE /admin/v1/key_policies/{api_key_id}          revoke
//
// An update names only the fields it changes, as null only where null means
// what it means at creation (a provider's headers and stall timeout, a
// credential's protocols), and naming a field fixed at creation is refused
// rather than ignored. A key policy is the exception: it is written whole, so
// its body names every field. A list answers {"data": [...]},
// unpaginated: a configuration is tens of rows, not thousands. Errors answer
// in the platform's envelope, {"type":"error","error":{"type","message"}}.
package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// Config is what the admin API needs.
type Config struct {
	Store        *store.Store
	Cipher       secrets.Cipher     // seals credential keys
	Verifier     *identity.Verifier // nil: no operator identity, the bootstrap key alone
	BootstrapKey string             // CONTROLPLANE_API_KEY
}

type handler struct {
	cfg       Config
	bootstrap [sha256.Size]byte
	mux       *http.ServeMux
}

// New returns the admin API's handler.
func New(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("modelgateway admin: a store is required")
	case cfg.Cipher == nil:
		return nil, errors.New("modelgateway admin: a secrets cipher is required to seal credential keys")
	case cfg.BootstrapKey == "":
		return nil, errors.New("modelgateway admin: the bootstrap key is required")
	}
	h := &handler{cfg: cfg, bootstrap: sha256.Sum256([]byte(cfg.BootstrapKey)), mux: http.NewServeMux()}
	h.routes()
	return h, nil
}

func (h *handler) routes() {
	read, write := identity.RoleViewer, identity.RoleAdmin
	h.route("/admin/v1/profiles", ep("GET", read, h.listProfiles))
	h.route("/admin/v1/profiles/{name}", ep("GET", read, h.getProfile))
	h.route("/admin/v1/providers", ep("GET", read, h.listProviders), ep("POST", write, h.createProvider))
	h.route("/admin/v1/providers/{id}",
		ep("GET", read, h.getProvider), ep("POST", write, h.updateProvider), ep("DELETE", write, h.deleteProvider))
	h.route("/admin/v1/providers/{id}/credentials", ep("GET", read, h.listCredentials), ep("POST", write, h.createCredential))
	h.route("/admin/v1/providers/{id}/credentials/{cid}",
		ep("GET", read, h.getCredential), ep("POST", write, h.updateCredential), ep("DELETE", write, h.deleteCredential))
	h.route("/admin/v1/deployments", ep("GET", read, h.listDeployments), ep("POST", write, h.createDeployment))
	h.route("/admin/v1/deployments/{id}",
		ep("GET", read, h.getDeployment), ep("POST", write, h.updateDeployment), ep("DELETE", write, h.deleteDeployment))
	h.route("/admin/v1/aliases", ep("GET", read, h.listAliases), ep("POST", write, h.createAlias))
	h.route("/admin/v1/aliases/{name...}",
		ep("GET", read, h.getAlias), ep("POST", write, h.updateAlias), ep("DELETE", write, h.deleteAlias))
	h.route("/admin/v1/key_policies", ep("GET", read, h.listKeyPolicies))
	h.route("/admin/v1/key_policies/{api_key_id}",
		ep("GET", read, h.getKeyPolicy), ep("POST", write, h.putKeyPolicy), ep("DELETE", write, h.deleteKeyPolicy))
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, notFound("no such path: %s", r.URL.Path))
	})
}

type endpoint struct {
	method string
	min    identity.Role
	fn     func(*http.Request) (any, error)
}

func ep(method string, min identity.Role, fn func(*http.Request) (any, error)) endpoint {
	return endpoint{method: method, min: min, fn: fn}
}

// route registers each endpoint on path, and a method-less pattern that
// answers any other method with a 405 in the envelope.
func (h *handler) route(path string, eps ...endpoint) {
	allowed := make([]string, 0, len(eps))
	for _, e := range eps {
		allowed = append(allowed, e.method)
		h.mux.HandleFunc(e.method+" "+path, func(w http.ResponseWriter, r *http.Request) {
			if err := requireRole(r.Context(), e.min); err != nil {
				writeError(w, r, err)
				return
			}
			v, err := e.fn(r)
			if err != nil {
				writeError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, v)
		})
	}
	h.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeError(w, r, &apiError{http.StatusMethodNotAllowed, "invalid_request_error",
			fmt.Sprintf("%s is not allowed on this path; it takes %s", r.Method, strings.Join(allowed, ", "))})
	})
}

type roleKey struct{}

// ServeHTTP authenticates before routing, and refuses a token that maps to
// no role before routing too: every route needs the viewer role at least, so
// a caller who can call nothing learns nothing about which paths exist.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	role, err := h.authenticate(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := context.WithValue(r.Context(), roleKey{}, role)
	if err := requireRole(ctx, identity.RoleViewer); err != nil {
		writeError(w, r, err)
		return
	}
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

func (h *handler) authenticate(r *http.Request) (identity.Role, error) {
	keys := r.Header.Values("x-api-key")
	if len(keys) > 1 || len(r.Header.Values("Authorization")) > 1 {
		return "", unauthenticated("a credential header is repeated")
	}
	if len(keys) == 1 {
		if h.isBootstrap(keys[0]) {
			return identity.RoleAdmin, nil
		}
		return "", unauthenticated("invalid x-api-key: the admin API takes the bootstrap key, or an operator token")
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && h.isBootstrap(tok) {
		return identity.RoleAdmin, nil
	}
	if v := h.cfg.Verifier; v != nil {
		if tok, ok := v.Credential(r); ok {
			id, err := v.Verify(r.Context(), tok)
			if err != nil {
				// As the control plane's identity lane: the caller is told
				// only that the token failed, the operator's log which check.
				var reason string
				var ie *identity.Error
				if errors.As(err, &ie) {
					reason = ie.Reason()
				}
				slog.InfoContext(r.Context(), "modelgateway admin: operator token rejected", "reason", reason)
				return "", unauthenticated("invalid operator token")
			}
			return id.Role, nil
		}
	}
	return "", unauthenticated("the admin API takes the bootstrap key in x-api-key, or an operator token")
}

// isBootstrap compares digests, so the comparison is constant-time in the
// key's length as well as its bytes.
func (h *handler) isBootstrap(key string) bool {
	sum := sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(sum[:], h.bootstrap[:]) == 1
}

func requireRole(ctx context.Context, min identity.Role) error {
	role, _ := ctx.Value(roleKey{}).(identity.Role)
	switch {
	case role.AtLeast(min):
		return nil
	case role == identity.RoleNone:
		return forbidden("this request needs the %s role; your token maps to no role", min)
	default:
		return forbidden("this request needs the %s role; yours is %s", min, role)
	}
}

type apiError struct {
	status int
	typ    string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func invalid(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *apiError {
	return &apiError{http.StatusNotFound, "not_found_error", fmt.Sprintf(format, args...)}
}

func unauthenticated(msg string) *apiError {
	return &apiError{http.StatusUnauthorized, "authentication_error", msg}
}

func forbidden(format string, args ...any) *apiError {
	return &apiError{http.StatusForbidden, "permission_error", fmt.Sprintf(format, args...)}
}

// writeError answers err in the envelope: an apiError as itself, a store
// error by its kind, and anything else as a 500 whose detail goes to the log
// rather than to the caller.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		switch {
		case errors.Is(err, store.ErrNotFound):
			ae = notFound("%s", err)
		case errors.Is(err, store.ErrConflict):
			ae = &apiError{http.StatusConflict, "invalid_request_error", err.Error()}
		case errors.Is(err, store.ErrInvalid):
			ae = invalid("%s", err)
		default:
			slog.ErrorContext(r.Context(), "modelgateway admin: request failed", "method", r.Method, "path", r.URL.Path, "error", err)
			ae = &apiError{http.StatusInternalServerError, "api_error", "internal error"}
		}
	}
	writeJSON(w, ae.status, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": ae.typ, "message": ae.msg},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxBody bounds a request body: a configuration write is small.
const maxBody = 1 << 20

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, invalid("Failed to read request body: %s", err)
	}
	return b, nil
}

// strict decodes one JSON value into dst, refusing unknown fields at any
// depth and anything after the value.
func strict(b []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalid("Failed to parse request body as JSON: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	// Token rather than More, which reports only another element of an
	// enclosing value and so passes a stray closing delimiter.
	if _, err := dec.Token(); err != io.EOF {
		return invalid("Failed to parse request body as JSON: data after the value")
	}
	return nil
}

// decode reads a create request's body into dst.
func decode(r *http.Request, dst any) error {
	b, err := readBody(r)
	if err != nil {
		return err
	}
	return strict(b, dst)
}

// patch is an update request's body: the fields it names, each still raw.
type patch map[string]json.RawMessage

// fixedFields maps each field an update may not change to the refusal that
// says why.
type fixedFields map[string]string

func atCreation(names ...string) fixedFields {
	f := make(fixedFields, len(names))
	for _, n := range names {
		f[n] = n + " is fixed at creation"
	}
	return f
}

// decodePatch reads an update request, refusing a fixed field by name and any
// field not in mutable.
func decodePatch(r *http.Request, fixed fixedFields, mutable []string) (patch, error) {
	b, err := readBody(r)
	if err != nil {
		return nil, err
	}
	var p patch
	if err := strict(b, &p); err != nil {
		return nil, err
	}
	for k := range p {
		if why, ok := fixed[k]; ok {
			return nil, invalid("%s", why)
		}
		if !slices.Contains(mutable, k) {
			return nil, invalid("Failed to parse request body as JSON: unknown field %q", k)
		}
	}
	return p, nil
}

// field decodes the named field into dst when the patch names it. A null is
// refused: decoded, it would be the zero value — a provider disabled, a name
// emptied — where a caller meant at most "unchanged".
func (p patch) field(name string, dst any) (bool, error) {
	raw, ok := p[name]
	if ok && string(bytes.TrimSpace(raw)) == "null" {
		return true, invalid("%s: null is not a value of this field; leave the field out to keep it", name)
	}
	return p.nullable(name, dst)
}

// nullable is field for a field whose null means what it means at creation:
// a provider's headers none, its stall timeout the default, a credential's
// protocols every one its provider has an endpoint for.
func (p patch) nullable(name string, dst any) (bool, error) {
	raw, ok := p[name]
	if !ok {
		return false, nil
	}
	if err := strict(raw, dst); err != nil {
		return true, invalid("%s: %s", name, err)
	}
	return true, nil
}

type listView struct {
	Data any `json:"data"`
}
