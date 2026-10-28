package api

import (
	"encoding/json"
	"errors"
	"math"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func (s *server) authConfig(*http.Request) (any, error) {
	return struct {
		Mode   login.Mode `json:"mode"`
		Native bool       `json:"native_login"`
	}{s.authMode, s.nativeLogin()}, nil
}

func (s *server) authSession(r *http.Request) (any, error) { return requireCookieSession(r) }

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if _, err := requireCookieSession(r); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.login.Revoke(r.Context(), r); err != nil {
		writeError(w, r, err)
		return
	}
	s.login.ClearCookie(w)
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

func (s *server) loginStart(w http.ResponseWriter, r *http.Request) {
	if !s.nativeLogin() || s.login.Mode() != login.ModeOIDC || s.rp == nil {
		writeError(w, r, errMethodNotAllowed)
		return
	}
	state, nonce, verifier, binding, err := s.login.BeginOIDC(r.Context(), r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.login.SetOIDCCookie(w, binding)
	http.Redirect(w, r, s.rp.AuthorizationURL(state, nonce, verifier), http.StatusSeeOther)
}

func (s *server) loginCallback(w http.ResponseWriter, r *http.Request) {
	if !s.nativeLogin() || s.login.Mode() != login.ModeOIDC || s.rp == nil {
		writeError(w, r, errMethodNotAllowed)
		return
	}
	s.login.ClearOIDCCookie(w)
	// Parse/duplicate-parameter failures clear the pending cookie but retain
	// the server attempt. Only an unambiguous callback reaches consumption.
	query, err := urlQuery(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	nonce, verifier, err := s.login.ConsumeOIDC(r.Context(), r, query["state"])
	if err != nil {
		if errors.Is(err, login.ErrUnauthenticated) {
			err = errAuth("login failed")
		}
		writeError(w, r, err)
		return
	}
	// The attempt is spent even if the provider refused or supplied no code.
	if query["error"] != "" || query["code"] == "" || len(query["code"]) > 16<<10 {
		writeError(w, r, errAuth("login failed"))
		return
	}
	id, err := s.rp.Exchange(r.Context(), query["code"], verifier, nonce)
	if err != nil {
		writeError(w, r, errAuth("login failed"))
		return
	}
	markVerified(r.Context())
	p, err := resolvePrincipal(r.Context(), s.pool, id)
	if err != nil {
		writeNativeLoginError(w, r, err)
		return
	}
	issued, err := s.login.Issue(r.Context(), p.ID)
	if err != nil {
		writeNativeLoginError(w, r, err)
		return
	}
	s.login.SetCookie(w, issued)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Query errors, duplicate parameters and provider descriptions are never echoed.
func urlQuery(r *http.Request) (map[string]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errAuth("login failed")
	}
	out := make(map[string]string)
	for _, key := range []string{"state", "code", "error"} {
		if len(q[key]) > 1 {
			return nil, errAuth("login failed")
		}
		out[key] = q.Get(key)
	}
	return out, nil
}

func (s *server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.nativeLogin() {
		writeError(w, r, errMethodNotAllowed)
		return
	}
	if len(r.Header.Values("x-api-key")) > 0 || len(r.Header.Values("Authorization")) > 0 {
		writeError(w, r, errAuth("login requires the configured login credential"))
		return
	}
	if s.login.CheckOrigin(r) != nil {
		writeError(w, r, errForbidden("invalid browser origin"))
		return
	}
	var id string
	switch s.login.Mode() {
	case login.ModePassword:
		if err := loginJSON(r); err != nil {
			writeError(w, r, err)
			return
		}
		obj, err := decodeObject(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		name, password, err := localCredentials(obj)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if err := s.throttleLogin(r, name); err != nil {
			writeError(w, r, err)
			return
		}
		var encoded, kind, status string
		err = s.pool.QueryRow(r.Context(), `SELECT p.id,p.kind,p.status,coalesce(a.password_hash,'')
    FROM principals p LEFT JOIN local_accounts a ON a.principal_id=p.id
    WHERE p.issuer=$1 AND p.subject=$2`, login.LocalIssuer, name).Scan(&id, &kind, &status, &encoded)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, r, err)
			return
		}
		// An absent account takes the bounded dummy verification path too.
		if !login.VerifyPassword(encoded, password) || id == "" || kind != "user" {
			writeError(w, r, errAuth("invalid login name or password"))
			return
		}
		if status != "active" {
			writeNativeLoginError(w, r, errDeactivated)
			return
		}
		markVerified(r.Context())
	case login.ModeTrustedProxy:
		if s.verifier == nil {
			writeError(w, r, errAuth("login failed"))
			return
		}
		token, ok := s.verifier.Credential(r)
		if !ok {
			writeError(w, r, errAuth("login failed"))
			return
		}
		verified, err := s.verifier.Verify(r.Context(), token)
		if err != nil {
			writeError(w, r, errAuth("login failed"))
			return
		}
		markVerified(r.Context())
		p, err := resolvePrincipal(r.Context(), s.pool, verified)
		if err != nil {
			writeNativeLoginError(w, r, err)
			return
		}
		id = p.ID
	default:
		writeError(w, r, errMethodNotAllowed)
		return
	}
	s.issueLogin(w, r, id)
}

// Native login state failures clear client identity; an outage is still a 500.
// Credential failures use the ordinary error writer and do not sign anyone out.
func writeNativeLoginError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, login.ErrUnauthenticated):
		w.Header().Set("x-platform-signed-out", "1")
		err = errAuth("invalid or expired browser session")
	case errors.Is(err, errDeactivated):
		w.Header().Set("x-platform-signed-out", "1")
	}
	writeError(w, r, err)
}

func (s *server) issueLogin(w http.ResponseWriter, r *http.Request, id string) {
	issued, err := s.login.Issue(r.Context(), id)
	if err != nil {
		writeNativeLoginError(w, r, err)
		return
	}
	s.login.SetCookie(w, issued)
	writeJSON(w, http.StatusOK, issued.Session)
}

func loginJSON(r *http.Request) error {
	values := r.Header.Values("Content-Type")
	if len(values) != 1 {
		return errInvalid("Content-Type must be application/json")
	}
	media, params, err := mime.ParseMediaType(values[0])
	if err != nil || media != "application/json" || len(params) > 1 || (len(params) == 1 && !strings.EqualFold(params["charset"], "utf-8")) {
		return errInvalid("Content-Type must be application/json")
	}
	return nil
}

var localNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Shared with local-member creation. No field value is quoted in an error.
func localCredentials(obj map[string]json.RawMessage) (string, string, error) {
	name, err := requiredString(obj, "login_name")
	if err != nil {
		return "", "", err
	}
	if !localNamePattern.MatchString(name) {
		return "", "", errInvalid("login_name must be 1–64 lowercase letters, digits, dots, underscores or hyphens, starting with a letter or digit")
	}
	password, err := requiredString(obj, "password")
	if err != nil {
		return "", "", err
	}
	if len(password) < 12 || len(password) > 1024 || !utf8.ValidString(password) || strings.ContainsRune(password, 0) {
		return "", "", errInvalid("password must be 12–1024 UTF-8 bytes without NUL")
	}
	return name, password, nil
}

func (s *server) throttleLogin(r *http.Request, name string) error {
	retry, err := s.login.Throttle(r.Context(), r, name)
	if err != nil {
		return err
	}
	if retry > 0 {
		return &apiErrorWithHeaders{err: &apiError{http.StatusTooManyRequests, "rate_limit_error", "too many login attempts"}, headers: map[string]string{"Retry-After": strconv.Itoa(int(math.Ceil(retry.Seconds())))}}
	}
	return nil
}
