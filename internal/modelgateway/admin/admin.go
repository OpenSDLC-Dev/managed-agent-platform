// Package admin is the model gateway's admin API, under /admin/v1/
// (docs/plan/59_model-gateway.md, "Auth, admin API and /v1/models"): the
// providers and their credentials, the deployments, the aliases and organization
// model limits, the usage ledger read-only, and the compiled-in profiles
// read-only. No reference surface
// corresponds to it, so its shape is the platform's own, and
// managed-agent-console is its client.
//
// Only native browser cookies reach this API. Sessions read current active
// user authority from the shared database without replaying IdP mappings.
// Unsafe requests require exact Origin and CSRF. API keys and Authorization
// headers are refused, including when a valid cookie accompanies them.
// Developers can read; admins can write. A user has no gateway-admin access.
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
//	POST   /admin/v1/providers/{id}/credentials/{cid}/check  list the vendor's models with its key
//	POST   /admin/v1/deployments                        create
//	GET    /admin/v1/deployments                        list
//	GET    /admin/v1/deployments/{id}                   one
//	POST   /admin/v1/deployments/{id}                   update
//	DELETE /admin/v1/deployments/{id}                   delete (off every alias first)
//	POST   /admin/v1/deployments/{id}/test              one minimal request with each credential
//	POST   /admin/v1/aliases                            create
//	GET    /admin/v1/aliases                            list
//	GET    /admin/v1/aliases/{name...}                  one (a name may hold a slash)
//	POST   /admin/v1/aliases/{name...}                  update
//	DELETE /admin/v1/aliases/{name...}                  delete
//	GET    /admin/v1/rate_limits                       list organization limits
//	POST   /admin/v1/rate_limits/{model...}           replace organization limits
//	GET    /admin/v1/usage/daily                        rollups per UTC day, principal, alias and deployment
//	GET    /admin/v1/usage/requests                     the ledger, a page at a time
//
// An update names only the fields it changes, as null only where null means
// what it means at creation (a provider's headers and stall timeout, a
// credential's protocols), and naming a field fixed at creation is refused
// rather than ignored. A model limit is replaced whole and names every dimension. A list answers {"data": [...]},
// unpaginated: a configuration is tens of rows, not thousands. The ledger is
// the exception, millions of rows, read a page at a time. Errors answer
// in the platform's envelope, {"type":"error","error":{"type","message"}}.
//
// Two routes call the vendor, on demand alone, and need the admin role: a
// credential check costs no tokens, and shows the vendor's list-models route
// takes the key; a deployment test costs a few, and shows the whole path
// answers — endpoint, key, upstream model id, the account's access and
// balance. Each answers 200 with a result per protocol or per credential,
// whatever the vendor said (probes.go).
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
)

// Config is what the admin API needs.
type Config struct {
	Store  *store.Store
	Cipher secrets.Cipher // seals credential keys
	Login  *login.Manager // required shared-DB browser sessions
	Prober Prober         // calls vendors for the check and test routes (modelgateway.NewProber)
}

type handler struct {
	cfg Config
	mux *http.ServeMux
}

// New returns the admin API's handler.
func New(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("modelgateway admin: a store is required")
	case cfg.Cipher == nil:
		return nil, errors.New("modelgateway admin: a secrets cipher is required to seal credential keys")
	case cfg.Login == nil || !cfg.Login.NativeEnabled():
		return nil, errors.New("modelgateway admin: native browser login is required")
	case cfg.Prober == nil:
		return nil, errors.New("modelgateway admin: a prober is required for the check and test routes")
	}
	h := &handler{cfg: cfg, mux: http.NewServeMux()}
	h.routes()
	return h, nil
}

func (h *handler) routes() {
	read, write := identity.RoleDeveloper, identity.RoleAdmin
	h.route("/admin/v1/profiles", ep("GET", read, h.listProfiles))
	h.route("/admin/v1/profiles/{name}", ep("GET", read, h.getProfile))
	h.route("/admin/v1/providers", ep("GET", read, h.listProviders), ep("POST", write, h.createProvider))
	h.route("/admin/v1/providers/{id}",
		ep("GET", read, h.getProvider), ep("POST", write, h.updateProvider), ep("DELETE", write, h.deleteProvider))
	h.route("/admin/v1/providers/{id}/credentials", ep("GET", read, h.listCredentials), ep("POST", write, h.createCredential))
	h.route("/admin/v1/providers/{id}/credentials/{cid}",
		ep("GET", read, h.getCredential), ep("POST", write, h.updateCredential), ep("DELETE", write, h.deleteCredential))
	h.route("/admin/v1/providers/{id}/credentials/{cid}/check", ep("POST", write, h.checkCredential))
	h.route("/admin/v1/deployments", ep("GET", read, h.listDeployments), ep("POST", write, h.createDeployment))
	h.route("/admin/v1/deployments/{id}",
		ep("GET", read, h.getDeployment), ep("POST", write, h.updateDeployment), ep("DELETE", write, h.deleteDeployment))
	h.route("/admin/v1/deployments/{id}/test", ep("POST", write, h.testDeployment))
	h.route("/admin/v1/aliases", ep("GET", read, h.listAliases), ep("POST", write, h.createAlias))
	h.route("/admin/v1/aliases/{name...}",
		ep("GET", read, h.getAlias), ep("POST", write, h.updateAlias), ep("DELETE", write, h.deleteAlias))
	h.route("/admin/v1/usage/daily", ep("GET", read, h.dailyUsage))
	h.route("/admin/v1/usage/requests", ep("GET", read, h.listUsage))
	h.route("/admin/v1/rate_limits", ep("GET", read, h.listModelLimits))
	h.route("/admin/v1/rate_limits/{model...}", ep("POST", write, h.putModelLimit))
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
type cookieKey struct{}

// ServeHTTP authenticates before routing, and refuses a caller below developer
// before routing too: every route needs the developer role at least, so a
// caller who can call nothing, a user, learns nothing about which paths exist.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m := h.cfg.Login; m != nil && m.NativeEnabled() {
		w.Header().Set("Cache-Control", "no-store, private")
	}
	session, err := h.authenticate(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	role := session.Role
	hint, _ := workspace.PrivateSelector(r)
	scope := session.WorkspaceKey
	if hint != "" && (scope == "" || session.WorkspaceRole == "") {
		writeError(w, r, forbidden("workspace is not available"))
		return
	}
	_, pattern := h.mux.Handler(r)
	switch pattern {
	case "GET /admin/v1/usage/daily":
		role = identity.RoleNone
		if hint == "" && (session.Role == identity.RoleAdmin || session.Role == identity.RoleBilling) {
			scope = ""
			role = identity.RoleDeveloper
		} else if (hint != "" || session.WorkspaceChoices == 1) && session.WorkspaceRole.CanReadUsageSummary() {
			role = identity.RoleDeveloper
		}
	case "GET /admin/v1/usage/requests":
		role = session.WorkspaceRole.ResourceRole()
		if hint == "" && session.WorkspaceChoices > 1 {
			role = identity.RoleNone
		}
	}
	ctx := context.WithValue(workspace.WithID(r.Context(), scope), roleKey{}, role)
	ctx = context.WithValue(ctx, principalKey{}, session.PrincipalID)
	ctx = context.WithValue(ctx, cookieKey{}, true)
	if err := requireRole(ctx, identity.RoleDeveloper); err != nil {
		writeError(w, r, err)
		return
	}
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

// authenticate refuses every non-cookie credential before routing.
func (h *handler) authenticate(r *http.Request) (login.Session, error) {
	if len(r.Header.Values("x-api-key")) > 0 || len(r.Header.Values("Authorization")) > 0 {
		return login.Session{}, unauthenticated("a browser session cookie is required")
	}
	session, err := h.cfg.Login.Authenticate(r.Context(), r)
	if errors.Is(err, login.ErrWorkspaceSelector) {
		return login.Session{}, forbidden("invalid workspace selector")
	}
	if err != nil {
		return login.Session{}, err
	}
	if err := h.cfg.Login.CheckCSRF(r, session); err != nil {
		return login.Session{}, forbidden("invalid browser origin or CSRF token")
	}
	return session, nil
}

func requireRole(ctx context.Context, min identity.Role) error {
	role, _ := ctx.Value(roleKey{}).(identity.Role)
	if role.AtLeast(min) {
		return nil
	}
	return forbidden("this request needs the %s role; yours is %s", min, role)
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
		case errors.Is(err, login.ErrUnauthenticated):
			w.Header().Set("x-platform-signed-out", "1")
			ae = unauthenticated("invalid or expired browser session")
		case errors.Is(err, principal.ErrDeactivated):
			w.Header().Set("x-platform-signed-out", "1")
			ae = forbidden("this account has been removed from the organization")
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
