package api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/clioauth"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

const (
	cliAuthorizePath  = "/oauth/authorize"
	cliTokenPath      = "/v1/oauth/token"
	cliWorkspacesPath = "/v1/organizations/workspaces"
	maxOAuthInput     = 8 << 10
)

var errCLIRole = errForbidden("the current role cannot grant the requested scopes")

var cliConsentPage = template.Must(template.New("cli-consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Authorize CLI</title></head>
<body><main><h1>Authorize CLI</h1>
{{if .CopyCode}}
<p>Copy this code and paste it into the terminal where you ran <code>ant auth login</code>.</p>
<p><code id="authorization-code">{{.CopyCode}}</code></p>
{{else if .CSRF}}
<p>Signed in as {{.Account}}. The CLI requests these permissions:</p>
<ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul>
<form method="post" action="/oauth/authorize">
{{range $name, $value := .Fields}}<input type="hidden" name="{{$name}}" value="{{$value}}">{{end}}
<label>Workspace <select name="workspace_id" required>{{if gt (len .Workspaces) 1}}<option value="">Choose a workspace</option>{{end}}{{range .Workspaces}}<option value="{{.PublicID}}">{{.Name}}</option>{{end}}</select></label>
<input type="hidden" name="csrf_token" value="{{.CSRF}}">
<button type="submit">Authorize CLI</button>
</form>
<p>Close this page to cancel.</p>
{{else}}
<p><a href="/login" target="_blank" rel="noopener noreferrer">Sign in to the Console</a> in a new tab, then return here.</p>
<p><a href="{{.AuthorizeURL}}">Continue after signing in</a></p>
{{end}}
</main></body></html>`))

type cliConsentView struct {
	Workspaces   []consentWorkspace
	AuthorizeURL string
	Account      string
	CSRF         string
	Scopes       []string
	Fields       map[string]string
	CopyCode     string
}

func (s *server) cliAuthorize(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	w.Header().Set("Referrer-Policy", "same-origin")
	setCLIPagePolicy(w, "")
	if !s.nativeLogin() {
		writeError(w, r, errMethodNotAllowed)
		return
	}
	if len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("x-api-key")) != 0 {
		writeError(w, r, errAuth("a browser session cookie is required"))
		return
	}
	var params url.Values
	var err error
	if r.Method == http.MethodPost {
		params, err = oauthForm(w, r)
	} else if len(r.URL.RawQuery) <= maxOAuthInput {
		params, err = url.ParseQuery(r.URL.RawQuery)
	}
	if err != nil || !uniqueOAuthParams(params) {
		writeError(w, r, errInvalid("invalid CLI authorization parameters"))
		return
	}
	in, err := cliAuthorizationRequest(params, s.login.PublicOrigin())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if in.RedirectURI != manualCLIRedirect(s.login.PublicOrigin()) {
		// Chromium applies form-action to the POST's redirect as well.
		callbackOrigin, _, _ := strings.Cut(in.RedirectURI, "/callback")
		setCLIPagePolicy(w, " "+callbackOrigin)
	}
	session, err := s.login.Authenticate(r.Context(), r)
	if err != nil {
		if errors.Is(err, login.ErrUnauthenticated) && r.Method == http.MethodGet {
			renderCLIConsent(w, cliConsentView{AuthorizeURL: cliAuthorizePath + "?" + params.Encode()})
			return
		}
		writeCLIConsentError(w, r, err)
		return
	}
	markVerified(r.Context())
	if r.Method == http.MethodPost && (s.login.CheckOrigin(r) != nil ||
		subtle.ConstantTimeCompare([]byte(params.Get("csrf_token")), []byte(session.CSRFToken)) != 1) {
		writeError(w, r, errForbidden("invalid browser origin or CSRF token"))
		return
	}
	choices, err := s.consentWorkspaces(r, session, in, params.Get("workspace_id"))
	if err != nil {
		writeCLIConsentError(w, r, err)
		return
	}
	if len(choices) == 0 || r.Method == http.MethodPost && len(choices) != 1 {
		writeCLIRoleDenial(w, r, in, s.login.PublicOrigin())
		return
	}
	if r.Method == http.MethodGet {
		fields := make(map[string]string, len(params))
		for name, values := range params {
			if name != "csrf_token" && name != "workspace_id" {
				fields[name] = values[0]
			}
		}
		account := session.Email
		if account == "" {
			account = session.LoginName
		}
		if account == "" {
			account = session.Name
		}
		renderCLIConsent(w, cliConsentView{Workspaces: choices, Account: account, CSRF: session.CSRFToken, Scopes: in.Scopes, Fields: fields})
		return
	}
	in.WorkspaceKey = choices[0].Key
	r = r.WithContext(workspace.WithID(r.Context(), in.WorkspaceKey))
	code, err := s.issueCLIConsent(r, session, in)
	if errors.Is(err, errCLIRole) {
		writeCLIRoleDenial(w, r, in, s.login.PublicOrigin())
		return
	}
	if err != nil {
		writeCLIConsentError(w, r, err)
		return
	}
	if in.RedirectURI == manualCLIRedirect(s.login.PublicOrigin()) {
		renderCLIConsent(w, cliConsentView{CopyCode: code + "#" + in.State})
		return
	}
	http.Redirect(w, r, in.RedirectURI+"?"+(url.Values{"code": {code}, "state": {in.State}}).Encode(), http.StatusSeeOther)
}

func setCLIPagePolicy(w http.ResponseWriter, loopback string) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'"+loopback+"; frame-ancestors 'none'; base-uri 'none'")
}

func writeCLIRoleDenial(w http.ResponseWriter, r *http.Request, in clioauth.CodeRequest, origin string) {
	if in.RedirectURI == manualCLIRedirect(origin) {
		writeError(w, r, errCLIRole)
		return
	}
	params := url.Values{"error": {"access_denied"}, "error_description": {errCLIRole.Error()}, "state": {in.State}}
	http.Redirect(w, r, in.RedirectURI+"?"+params.Encode(), http.StatusSeeOther)
}

func writeCLIConsentError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, principal.ErrDeactivated) {
		err = errDeactivated
	}
	writeNativeLoginError(w, r, err)
}

func renderCLIConsent(w http.ResponseWriter, view cliConsentView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = cliConsentPage.Execute(w, view)
}

func (s *server) issueCLIConsent(r *http.Request, session login.Session, in clioauth.CodeRequest) (string, error) {
	ctx := r.Context()
	if err := clioauth.Prune(ctx, s.pool); err != nil {
		return "", err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM principals WHERE id=$1 AND org_id='default' FOR SHARE`, session.PrincipalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errAuth("invalid or expired browser session")
	}
	if err != nil {
		return "", err
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT true FROM workspaces WHERE id=$1 AND archived_at IS NULL FOR SHARE`, in.WorkspaceKey).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errCLIRole
		}
		return "", err
	}
	current, err := s.login.AuthenticateInTx(ctx, tx, r)
	if err != nil {
		return "", err
	}
	if current.PrincipalID != id || !clioauth.CanConsent(current.Role, current.WorkspaceRole, in.Scopes) {
		return "", errCLIRole
	}
	code, err := clioauth.IssueCode(ctx, tx, id, in)
	if err != nil {
		return "", err
	}
	return code, tx.Commit(ctx)
}

func cliAuthorizationRequest(q url.Values, origin string) (clioauth.CodeRequest, error) {
	in := clioauth.CodeRequest{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), State: q.Get("state"), Challenge: q.Get("code_challenge"),
	}
	if in.ClientID != clioauth.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || !oauthUnreserved(in.State, 1, 128) {
		return in, errInvalid("invalid CLI authorization parameters")
	}
	challenge, err := base64.RawURLEncoding.Strict().DecodeString(in.Challenge)
	if err != nil || len(challenge) != 32 || base64.RawURLEncoding.EncodeToString(challenge) != in.Challenge {
		return in, errInvalid("invalid S256 challenge")
	}
	if !validCLIRedirect(in.RedirectURI, origin) {
		return in, errInvalid("redirect_uri not supported by client")
	}
	if org := q.Get("orgUUID"); org != "" && org != clioauth.OrganizationID {
		return in, errForbidden("organization is not served by this platform")
	}
	if id := q.Get("workspace_id"); id != "" && (len(id) != 33 || !strings.HasPrefix(id, "wrkspc_") || !oauthUnreserved(id[7:], 26, 26)) {
		return in, errForbidden("workspace is not served by this platform")
	}
	for _, scope := range strings.Split(q.Get("scope"), " ") {
		switch scope {
		case clioauth.Profile, clioauth.Inference, clioauth.Developer, clioauth.OrgAdmin:
			if !slices.Contains(in.Scopes, scope) {
				in.Scopes = append(in.Scopes, scope)
			}
		default:
			return in, errInvalid("unsupported CLI OAuth scope")
		}
	}
	return in, nil
}

func manualCLIRedirect(origin string) string {
	return origin + "/oauth/code/callback?app=anthropic-cli"
}

func validCLIRedirect(raw, origin string) bool {
	if raw == manualCLIRedirect(origin) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host, port := u.Hostname(), u.Port()
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535 && (host == "localhost" || host == "127.0.0.1" || host == "::1") &&
		raw == "http://"+net.JoinHostPort(host, strconv.Itoa(n))+"/callback"
}

func oauthUnreserved(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, b := range []byte(value) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~", rune(b))) {
			return false
		}
	}
	return true
}

func oauthNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func oauthForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" || len(r.Header.Values("Content-Type")) != 1 {
		return nil, errInvalid("expected form-encoded OAuth parameters")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOAuthInput))
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(body))
}

// The CLI sends refresh as JSON. Decode one object of string parameters and
// refuse duplicate names, including equivalent JSON-escaped names.
func oauthJSON(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOAuthInput))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errInvalid("expected an OAuth parameter object")
	}
	params := make(url.Values)
	for dec.More() {
		field, err := dec.Token()
		name, ok := field.(string)
		if err != nil || !ok || params.Has(name) {
			return nil, errInvalid("invalid or repeated OAuth parameter")
		}
		value, err := dec.Token()
		str, ok := value.(string)
		if err != nil || !ok {
			return nil, errInvalid("OAuth parameters must be strings")
		}
		params.Set(name, str)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errInvalid("expected a single OAuth parameter object")
	}
	return params, nil
}

func uniqueOAuthParams(values url.Values) bool {
	if len(values) == 0 {
		return false
	}
	for _, items := range values {
		if len(items) != 1 {
			return false
		}
	}
	return true
}

func writeCLITokenError(w http.ResponseWriter, status int, kind string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{kind})
}

func (s *server) cliToken(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	if !s.nativeLogin() {
		writeError(w, r, errMethodNotAllowed)
		return
	}
	media, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if parseErr != nil || len(r.Header.Values("Content-Type")) != 1 {
		writeCLITokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var params url.Values
	var err error
	if media == "application/json" {
		params, err = oauthJSON(w, r)
	} else {
		params, err = oauthForm(w, r)
	}
	if err != nil || !uniqueOAuthParams(params) {
		writeCLITokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var tokens clioauth.Tokens
	switch params.Get("grant_type") {
	case "authorization_code":
		if media != "application/x-www-form-urlencoded" {
			writeCLITokenError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		tokens, err = s.exchangeCLIGrant(r, params)
	case "refresh_token":
		if params.Get("client_id") != clioauth.ClientID || params.Has("scope") {
			writeCLITokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		tokens, err = clioauth.Refresh(r.Context(), s.pool, params.Get("client_id"), params.Get("refresh_token"))
	default:
		writeCLITokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	if errors.Is(err, clioauth.ErrInvalidGrant) {
		writeCLITokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	markVerified(r.Context())
	writeJSON(w, http.StatusOK, tokens)
}

func (s *server) exchangeCLIGrant(r *http.Request, params url.Values) (clioauth.Tokens, error) {
	in := clioauth.ExchangeRequest{
		ClientID: params.Get("client_id"), Code: params.Get("code"), State: params.Get("state"),
		RedirectURI: params.Get("redirect_uri"), Verifier: params.Get("code_verifier"),
	}
	if in.ClientID != clioauth.ClientID || !oauthUnreserved(in.Code, 43, 43) || !oauthUnreserved(in.Verifier, 43, 128) ||
		!oauthUnreserved(in.State, 1, 128) || !validCLIRedirect(in.RedirectURI, s.login.PublicOrigin()) {
		return clioauth.Tokens{}, clioauth.ErrInvalidGrant
	}
	return clioauth.Exchange(r.Context(), s.pool, in)
}

type consentWorkspace struct {
	Key, PublicID, Name string
}

// Return only workspaces where this account can grant the requested scopes.
// A profile-only login still works for an organization member without resources.
func (s *server) consentWorkspaces(r *http.Request, session login.Session, in clioauth.CodeRequest, hint string) ([]consentWorkspace, error) {
	rows, err := s.pool.Query(r.Context(), `SELECT id,public_id,name,coalesce(workspace_role($1,id),'')
		FROM workspaces WHERE archived_at IS NULL AND ($2::text='' OR public_id=$2)
		ORDER BY (id='default') DESC,name,id`, session.PrincipalID, hint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var choices []consentWorkspace
	for rows.Next() {
		var w consentWorkspace
		var role workspace.Role
		if err := rows.Scan(&w.Key, &w.PublicID, &w.Name, &role); err != nil {
			return nil, err
		}
		if (role != "" || w.Key == workspace.Default) && clioauth.CanConsent(session.Role, role, in.Scopes) {
			choices = append(choices, w)
		}
	}
	return choices, rows.Err()
}
