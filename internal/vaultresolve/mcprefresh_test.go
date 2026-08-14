package vaultresolve_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tokenEndpoint is an issuer under test: it records what each exchange sent and
// answers whatever the test told it to.
type tokenEndpoint struct {
	*httptest.Server
	mu    sync.Mutex
	forms []url.Values
	auths []string
}

// newTokenEndpoint serves answer for every exchange. answer writes the status
// and body; the request has already been recorded when it runs.
func newTokenEndpoint(t *testing.T, answer func(w http.ResponseWriter, n int)) *tokenEndpoint {
	t.Helper()
	vaultresolve.AllowLoopbackTokenEndpointForTest(t)
	e := &tokenEndpoint{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint: parse form: %v", err)
		}
		e.mu.Lock()
		e.forms = append(e.forms, r.PostForm)
		e.auths = append(e.auths, r.Header.Get("Authorization"))
		n := len(e.forms)
		e.mu.Unlock()
		answer(w, n)
	}))
	t.Cleanup(e.Close)
	return e
}

// grants answers every exchange with a fresh access token, numbered so a test
// can tell one exchange from the next.
func grants(extra string) func(http.ResponseWriter, int) {
	return func(w http.ResponseWriter, n int) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"access-%d"%s}`, n, extra)
	}
}

func (e *tokenEndpoint) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.forms)
}

func (e *tokenEndpoint) form(t *testing.T, i int) url.Values {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.forms) {
		t.Fatalf("token endpoint saw %d exchanges, wanted one at index %d", len(e.forms), i)
	}
	return e.forms[i]
}

func (e *tokenEndpoint) authorization(t *testing.T, i int) string {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.auths) {
		t.Fatalf("token endpoint saw %d exchanges, wanted one at index %d", len(e.auths), i)
	}
	return e.auths[i]
}

// refreshable describes the credential a refresh test writes.
type refreshable struct {
	authType  string // defaults to mcp_oauth
	expiresAt *time.Time
	endpoint  string // "" leaves the credential with no refresh block
	authArm   string // defaults to none
	resource  string // "" omits the field, as the API does for an absent one
	scope     string
	sealed    map[string]string
}

// newRefreshableCred writes one credential the way the API writes it: the
// non-secret half in auth, the tokens sealed through the cipher.
func newRefreshableCred(t *testing.T, pool *pgxpool.Pool, cipher secrets.Cipher,
	vaultID string, spec refreshable) string {
	t.Helper()
	authType := spec.authType
	if authType == "" {
		authType = "mcp_oauth"
	}
	doc := map[string]any{"type": authType, "mcp_server_url": mcpServer, "expires_at": spec.expiresAt}
	if spec.endpoint != "" {
		arm := spec.authArm
		if arm == "" {
			arm = "none"
		}
		refresh := map[string]any{
			"client_id":           "client-1",
			"token_endpoint":      spec.endpoint,
			"token_endpoint_auth": map[string]string{"type": arm},
		}
		if spec.resource != "" {
			refresh["resource"] = spec.resource
		}
		if spec.scope != "" {
			refresh["scope"] = spec.scope
		}
		doc["refresh"] = refresh
	}
	auth, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := json.Marshal(spec.sealed)
	if err != nil {
		t.Fatal(err)
	}
	ct, keyID, err := cipher.Encrypt(context.Background(), plain)
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	id := domain.NewID("vcrd").String()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO vault_credentials
		   (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)`,
		id, vaultID, authType, auth, ct, keyID, "url:"+mcpServer); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	return id
}

// storedCred reads a credential back the way the next dial would: its auth
// document and its unsealed secrets.
func storedCred(t *testing.T, pool *pgxpool.Pool, cipher secrets.Cipher, id string) (
	map[string]json.RawMessage, map[string]string) {
	t.Helper()
	var authDoc, ciphertext []byte
	var keyID *string
	if err := pool.QueryRow(context.Background(),
		`SELECT auth, secret_ciphertext, secret_key_id FROM vault_credentials WHERE id = $1`, id).
		Scan(&authDoc, &ciphertext, &keyID); err != nil {
		t.Fatalf("read credential back: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(authDoc, &doc); err != nil {
		t.Fatalf("stored auth document: %v", err)
	}
	key := ""
	if keyID != nil {
		key = *keyID
	}
	plain, err := cipher.Decrypt(context.Background(), ciphertext, key)
	if err != nil {
		t.Fatalf("unseal stored secrets: %v", err)
	}
	sealed := map[string]string{}
	if err := json.Unmarshal(plain, &sealed); err != nil {
		t.Fatalf("stored sealed document: %v", err)
	}
	return doc, sealed
}

func ago(d time.Duration) *time.Time   { t := time.Now().UTC().Add(-d); return &t }
func ahead(d time.Duration) *time.Time { t := time.Now().UTC().Add(d); return &t }

func errorsIsUnusable(err error) bool {
	return errors.Is(err, vaultresolve.ErrCredentialUnusable)
}

// basicAuthOf reads back what the client_secret_basic arm sent: the header's
// two halves, form-unescaped the way the token endpoint escaped them (RFC 6749
// §2.3.1). net/http's own Request.BasicAuth does the base64 but not the
// unescaping, so a secret carrying a space or a colon would read back wrong.
func basicAuthOf(header string) (user, pass string, ok bool) {
	raw, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", "", false
	}
	escapedUser, escapedPass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", false
	}
	if user, err = url.QueryUnescape(escapedUser); err != nil {
		return "", "", false
	}
	if pass, err = url.QueryUnescape(escapedPass); err != nil {
		return "", "", false
	}
	return user, pass, true
}

// The whole point of the slice: a session dialling with an expired credential
// sends the token the issuer just handed over, not the one that expired.
func TestAnExpiredCredentialIsRefreshedBeforeItIsSent(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(`,"refresh_token":"refresh-2","expires_in":3600`))

	v := newVault(t, pool, false)
	id := newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	got, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher, []string{v}, mcpServer)
	if err != nil {
		t.Fatal(err)
	}
	if got != "access-1" {
		t.Fatalf("resolved %q, want the refreshed token", got)
	}
	if form := issuer.form(t, 0); form.Get("grant_type") != "refresh_token" ||
		form.Get("refresh_token") != "refresh-1" {
		t.Errorf("exchange sent %v", form)
	}

	// And the rotation is stored, or the next dial pays for it again.
	doc, sealed := storedCred(t, pool, cipher, id)
	if sealed["access_token"] != "access-1" || sealed["refresh_token"] != "refresh-2" {
		t.Errorf("stored secrets = %v", sealed)
	}
	var expiresAt time.Time
	if err := json.Unmarshal(doc["expires_at"], &expiresAt); err != nil {
		t.Fatalf("stored expires_at: %v", err)
	}
	if !expiresAt.After(time.Now().UTC().Add(time.Hour - time.Minute)) {
		t.Errorf("stored expires_at = %s, want about an hour out", expiresAt)
	}
	// The fields the refresh does not read survive it: the document is patched,
	// not re-rendered from what this package happens to parse.
	var serverURL string
	if err := json.Unmarshal(doc["mcp_server_url"], &serverURL); err != nil || serverURL != mcpServer {
		t.Errorf("stored mcp_server_url = %q (%v)", serverURL, err)
	}
	if _, ok := doc["refresh"]; !ok {
		t.Error("the refresh block was dropped by the rotation")
	}
}

// The stored rotation is what makes it one exchange rather than one per dial.
func TestASecondDialUsesTheStoredRotation(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(`,"expires_in":3600`))

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	ctx := context.Background()
	for i, want := range []string{"access-1", "access-1"} {
		got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("dial %d resolved %q, want %q", i, got, want)
		}
	}
	if n := issuer.calls(); n != 1 {
		t.Errorf("the issuer was asked %d times for a token it had already issued", n)
	}
}

// An issuer that keeps its refresh token in place answers without one, and
// overwriting the stored one with "" would destroy the credential.
func TestARefreshTokenTheIssuerKeepsIsKept(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(`,"expires_in":3600`))

	v := newVault(t, pool, false)
	id := newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	if _, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
		[]string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	if _, sealed := storedCred(t, pool, cipher, id); sealed["refresh_token"] != "refresh-1" {
		t.Errorf("stored refresh_token = %q, want the one the issuer kept", sealed["refresh_token"])
	}
}

// An issuer that names no lifetime leaves the expiry unknown. Keeping the old
// one — already in the past — would make every later dial refresh again.
func TestAnIssuerThatNamesNoLifetimeLeavesTheExpiryUnknown(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(``))

	v := newVault(t, pool, false)
	id := newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	ctx := context.Background()
	if _, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	doc, _ := storedCred(t, pool, cipher, id)
	if string(doc["expires_at"]) != "null" {
		t.Errorf("stored expires_at = %s, want null", doc["expires_at"])
	}
	if _, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	if n := issuer.calls(); n != 1 {
		t.Errorf("the issuer was asked %d times; an unknown expiry must not refresh again", n)
	}
}

// Refreshing is decided by the expiry, and the leeway is the direction of the
// comparison: a token that outlives the dial is sent as it is. The two rows
// either side of the leeway sit close enough to it — half a minute in, half a
// minute out — that moving the leeway in either direction fails one of them.
func TestWhenTheExpiryDecidesToRefresh(t *testing.T) {
	for name, row := range map[string]struct {
		expiresAt *time.Time
		want      string
		exchanges int
	}{
		"long expired":      {ago(time.Hour), "access-1", 1},
		"just expired":      {ago(time.Second), "access-1", 1},
		"inside the leeway": {ahead(30 * time.Second), "access-1", 1},
		"outside it":        {ahead(90 * time.Second), "still-good", 0},
		"no expiry named":   {nil, "still-good", 0},
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			cipher := testCipher(t)
			issuer := newTokenEndpoint(t, grants(`,"expires_in":3600`))

			v := newVault(t, pool, false)
			newRefreshableCred(t, pool, cipher, v, refreshable{
				expiresAt: row.expiresAt, endpoint: issuer.URL,
				sealed: map[string]string{"access_token": "still-good", "refresh_token": "refresh-1"},
			})

			got, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
				[]string{v}, mcpServer)
			if err != nil {
				t.Fatal(err)
			}
			if got != row.want {
				t.Errorf("resolved %q, want %q", got, row.want)
			}
			if n := issuer.calls(); n != row.exchanges {
				t.Errorf("the issuer was called %d times, want %d", n, row.exchanges)
			}
		})
	}
}

// The documented no_refresh_token shape: an expired credential that cannot
// refresh sends what it has and lets the server answer. Answering an
// authentication failure here instead would hide a token the server may still
// honour — expires_at is the issuer's estimate, not its decision.
func TestACredentialThatCannotRefreshSendsTheTokenItHas(t *testing.T) {
	for name, spec := range map[string]refreshable{
		"no refresh block": {
			expiresAt: ago(time.Hour),
			sealed:    map[string]string{"access_token": "expired-but-sent", "refresh_token": "refresh-1"},
		},
		"no refresh token": {
			expiresAt: ago(time.Hour), endpoint: "http://127.0.0.1:1/token",
			sealed: map[string]string{"access_token": "expired-but-sent"},
		},
		"a static_bearer is never refreshed": {
			authType: "static_bearer", expiresAt: ago(time.Hour), endpoint: "http://127.0.0.1:1/token",
			sealed: map[string]string{"token": "expired-but-sent", "refresh_token": "refresh-1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			cipher := testCipher(t)
			v := newVault(t, pool, false)
			newRefreshableCred(t, pool, cipher, v, spec)

			got, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
				[]string{v}, mcpServer)
			if err != nil {
				t.Fatal(err)
			}
			if got != "expired-but-sent" {
				t.Errorf("resolved %q, want the stale token", got)
			}
		})
	}
}

// An issuer that says no is the credential's fault: the caller answers the tool
// call so an operator sees it, rather than retrying a grant that is gone.
func TestARefusedRefreshIsTheCredentialsFault(t *testing.T) {
	for name, answer := range map[string]func(http.ResponseWriter, int){
		"invalid_grant": func(w http.ResponseWriter, _ int) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		},
		"unauthorized client": func(w http.ResponseWriter, _ int) {
			w.WriteHeader(http.StatusUnauthorized)
		},
		"200 that is not a grant": func(w http.ResponseWriter, _ int) {
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		},
		// Above 5xx is outside the status registry altogether, so it is not the
		// issuer reporting a condition that passes: net/http hands back any
		// three-digit code, and "5xx and up" would retry this one forever.
		"a status outside the registry": func(w http.ResponseWriter, _ int) { w.WriteHeader(600) },
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			cipher := testCipher(t)
			issuer := newTokenEndpoint(t, answer)

			v := newVault(t, pool, false)
			newRefreshableCred(t, pool, cipher, v, refreshable{
				expiresAt: ago(time.Hour), endpoint: issuer.URL,
				sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
			})

			_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
				[]string{v}, mcpServer)
			if !errorsIsUnusable(err) {
				t.Fatalf("err = %v, want it marked ErrCredentialUnusable", err)
			}
		})
	}
}

// An issuer having a moment says nothing about the credential, so the work item
// faults and comes back rather than answering a permanent failure.
func TestAnIssuerHavingAMomentIsRetryable(t *testing.T) {
	for name, answer := range map[string]func(http.ResponseWriter, int){
		"rate limited":    func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusTooManyRequests) },
		"server error":    func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusBadGateway) },
		"request timeout": func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusRequestTimeout) },
		"too early":       func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusTooEarly) },
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			cipher := testCipher(t)
			issuer := newTokenEndpoint(t, answer)

			v := newVault(t, pool, false)
			newRefreshableCred(t, pool, cipher, v, refreshable{
				expiresAt: ago(time.Hour), endpoint: issuer.URL,
				sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
			})

			_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
				[]string{v}, mcpServer)
			if err == nil {
				t.Fatal("a failed exchange resolved a token")
			}
			if errorsIsUnusable(err) {
				t.Fatalf("err = %v, want it retryable rather than the credential's fault", err)
			}
		})
	}
}

// The token endpoint is operator-supplied and dialled from inside the executor,
// so it goes through the same address guard every other outbound dial does. Left
// in place here — every other test in the file lifts it — a loopback endpoint is
// never reached.
//
// And a refused address is the credential's fault rather than a retry: no
// waiting makes it reachable, and the alternative is a work item that faults and
// reclaims forever while the operator is told nothing.
func TestAnAddressTheGuardRefusesIsTheCredentialsFault(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the address guard let a loopback token endpoint through")
		fmt.Fprint(w, `{"access_token":"leaked"}`)
	}))
	defer issuer.Close()

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher, []string{v}, mcpServer)
	if !errorsIsUnusable(err) {
		t.Fatalf("err = %v, want it marked ErrCredentialUnusable", err)
	}
}

// The token-endpoint client's guards are asserted rather than only written,
// because each is a decision that reads like an omission once it is gone —
// most of all the absent Proxy, which is the reflex fix for a deployment whose
// egress needs one and which would take every exchange off the address the
// dial-time guard vetted, carrying the refresh token with it.
func TestTheTokenEndpointClientCarriesItsGuards(t *testing.T) {
	client := vaultresolve.RefreshClientForTest()
	if client.CheckRedirect == nil {
		t.Error("redirects are followed; a 307 replays the refresh token to the target")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Error("a proxy is configured; the address guard would vet the proxy, not the endpoint")
	}
	if transport.DialContext == nil {
		t.Error("no DialContext, so no address guard")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Error("HTTP/2 is off; setting DialContext disables it unless this is set")
	}
	if transport.MaxResponseHeaderBytes == 0 {
		t.Error("header blocks are unbounded; the body cap bounds nothing else")
	}
	if transport.IdleConnTimeout == 0 || transport.MaxIdleConnsPerHost == 0 {
		t.Error("idle connections to an issuer are held until the process ends")
	}
	if client.Timeout == 0 {
		t.Error("an exchange has no whole-request bound")
	}
}

// An issuer that is merely unreachable is the other half of that split: the
// network may recover, so the work item faults and comes back.
func TestAnUnreachableIssuerIsRetryable(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	// A port nothing listens on, reached with the guard lifted so the refusal
	// under test is the connection's and not the address's.
	vaultresolve.AllowLoopbackTokenEndpointForTest(t)
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := closed.URL
	closed.Close()

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: endpoint,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher, []string{v}, mcpServer)
	if err == nil {
		t.Fatal("a failed dial resolved a token")
	}
	if errorsIsUnusable(err) {
		t.Fatalf("err = %v, want an unreachable issuer to stay retryable", err)
	}
}

// A redirect is refused rather than followed: following one replays the POST
// body — the refresh token, and a client_secret_post secret — to a target the
// per-dial address guard vets but never approved as a destination. Asserted by
// watching the redirect target rather than by the outcome, which a client that
// followed the hop and then failed would produce all the same.
func TestARefreshDoesNotFollowARedirect(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)

	elsewhere := newTokenEndpoint(t, grants(`,"expires_in":3600`))
	issuer := newTokenEndpoint(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Location", elsewhere.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher, []string{v}, mcpServer)
	if !errorsIsUnusable(err) {
		t.Fatalf("err = %v, want it marked ErrCredentialUnusable", err)
	}
	if n := elsewhere.calls(); n != 0 {
		t.Errorf("the redirect target received %d exchanges, want 0", n)
	}
}

// The rotation is written under a compare-and-set on the ciphertext this
// resolution read, because the exchange spent seconds on network I/O and
// anything may have landed meanwhile. Both races are staged from inside the
// token endpoint's handler, which runs while the exchange is in flight — the one
// window where the competing write is guaranteed to be concurrent.
func TestARotationDoesNotClobberAConcurrentWrite(t *testing.T) {
	t.Run("a credential updated meanwhile keeps the newer write", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		cipher := testCipher(t)
		ctx := context.Background()

		var id string
		issuer := newTokenEndpoint(t, func(w http.ResponseWriter, n int) {
			rotateSealed(t, pool, cipher, id, map[string]string{
				"access_token": "written-elsewhere", "refresh_token": "refresh-9",
			})
			grants(`,"expires_in":3600`)(w, n)
		})

		v := newVault(t, pool, false)
		id = newRefreshableCred(t, pool, cipher, v, refreshable{
			expiresAt: ago(time.Hour), endpoint: issuer.URL,
			sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
		})

		got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
		if err != nil {
			t.Fatal(err)
		}
		// The dial in hand still uses the token this exchange was given: it is
		// valid, and failing the dial over a write it lost would break a working
		// credential.
		if got != "access-1" {
			t.Errorf("resolved %q, want the token this exchange was issued", got)
		}
		if _, sealed := storedCred(t, pool, cipher, id); sealed["access_token"] != "written-elsewhere" {
			t.Errorf("stored access_token = %q, want the concurrent write's", sealed["access_token"])
		}
	})

	// Archiving purges the sealed bytes as well as stamping archived_at, so in
	// production the compare-and-set above already refuses this write. The
	// archived_at guard is the independent second half, and pinning it needs the
	// state the API cannot write: archived with its secret still in place.
	t.Run("a credential archived meanwhile is not written to", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		cipher := testCipher(t)
		ctx := context.Background()

		var id string
		issuer := newTokenEndpoint(t, func(w http.ResponseWriter, n int) {
			if _, err := pool.Exec(ctx,
				`UPDATE vault_credentials SET archived_at = now() WHERE id = $1`, id); err != nil {
				t.Errorf("archive credential: %v", err)
			}
			grants(`,"expires_in":3600`)(w, n)
		})

		v := newVault(t, pool, false)
		id = newRefreshableCred(t, pool, cipher, v, refreshable{
			expiresAt: ago(time.Hour), endpoint: issuer.URL,
			sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
		})

		if _, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer); err != nil {
			t.Fatal(err)
		}
		if _, sealed := storedCred(t, pool, cipher, id); sealed["access_token"] != "expired" {
			t.Errorf("stored access_token = %q, want the archived row left alone",
				sealed["access_token"])
		}
	})
}

// rotateSealed reseals a credential's secrets the way another writer would,
// leaving a ciphertext the in-flight resolution's compare-and-set will not match.
func rotateSealed(t *testing.T, pool *pgxpool.Pool, cipher secrets.Cipher,
	id string, sealed map[string]string) {
	t.Helper()
	plain, err := json.Marshal(sealed)
	if err != nil {
		t.Error(err)
		return
	}
	ct, keyID, err := cipher.Encrypt(context.Background(), plain)
	if err != nil {
		t.Errorf("seal secret: %v", err)
		return
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE vault_credentials SET secret_ciphertext = $2, secret_key_id = $3 WHERE id = $1`,
		id, ct, keyID); err != nil {
		t.Errorf("rotate credential: %v", err)
	}
}

// A refreshed token is checked exactly like a stored one: it becomes an
// Authorization header value either way, and net/http refuses to write one
// carrying a control character — which would fail every dial as a transport
// error naming the server rather than the credential.
//
// And it is checked before anything is written. Sealing it would stamp a fresh
// expiry over the credential, after which every later dial would read the same
// unsendable token back and never refresh again — so the row is read back here,
// not just the error.
func TestARefreshedTokenMustBeSendableBeforeItIsStored(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, func(w http.ResponseWriter, _ int) {
		fmt.Fprint(w, `{"access_token":"line-one\nline-two","expires_in":3600}`)
	})

	v := newVault(t, pool, false)
	id := newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher, []string{v}, mcpServer)
	if !errorsIsUnusable(err) {
		t.Fatalf("err = %v, want it marked ErrCredentialUnusable", err)
	}
	doc, sealed := storedCred(t, pool, cipher, id)
	if sealed["access_token"] != "expired" {
		t.Errorf("stored access_token = %q, want the unsendable token not written", sealed["access_token"])
	}
	var expiresAt time.Time
	if err := json.Unmarshal(doc["expires_at"], &expiresAt); err != nil {
		t.Fatalf("stored expires_at: %v", err)
	}
	if expiresAt.After(time.Now().UTC()) {
		t.Errorf("stored expires_at = %s, want the old expiry left in the past", expiresAt)
	}
}

// The replacement refresh token beside an unsendable access token *is* stored,
// because the issuer has already spent the one in the row: dropping it would
// leave a credential no later exchange can buy a token for, repairable only by
// an operator rewriting it. Everything else is left alone — the old access
// token, and the expiry still in the past, which is what makes the next dial
// exchange again with the token that can still buy one.
func TestAnUnsendableTokenStillStoresTheRefreshTokenBesideIt(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, func(w http.ResponseWriter, _ int) {
		fmt.Fprint(w, `{"access_token":"line-one\nline-two","refresh_token":"refresh-2","expires_in":3600}`)
	})

	v := newVault(t, pool, false)
	id := newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})
	before, _ := storedCred(t, pool, cipher, id)

	_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher, []string{v}, mcpServer)
	if !errorsIsUnusable(err) {
		t.Fatalf("err = %v, want it marked ErrCredentialUnusable", err)
	}
	doc, sealed := storedCred(t, pool, cipher, id)
	if sealed["refresh_token"] != "refresh-2" {
		t.Errorf("stored refresh_token = %q, want the replacement the issuer just handed over",
			sealed["refresh_token"])
	}
	if sealed["access_token"] != "expired" {
		t.Errorf("stored access_token = %q, want the unsendable token not written", sealed["access_token"])
	}
	if !bytes.Equal(doc["expires_at"], before["expires_at"]) {
		t.Errorf("stored expires_at = %s, want the old expiry untouched (was %s)",
			doc["expires_at"], before["expires_at"])
	}
}

// A write that fails is logged as its SQLSTATE and not as what the driver said.
// Postgres renders the value it choked on into a DETAIL or CONTEXT line, and one
// of this write's bound values is the auth document — whose `mcp_server_url` and
// `token_endpoint` are allowed to carry a credential in their userinfo.
func TestAFailedWriteIsLoggedAsItsSQLSTATE(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	// The table goes away between the read and the write, which is where the
	// exchange's own seconds of network I/O sit in production.
	issuer := newTokenEndpoint(t, func(w http.ResponseWriter, _ int) {
		if _, err := pool.Exec(context.Background(),
			`ALTER TABLE vault_credentials RENAME TO vault_credentials_moved`); err != nil {
			t.Errorf("move the table out from under the write: %v", err)
		}
		fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
	})

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	// The dial still gets its fresh token: the exchange succeeded, and failing it
	// over a write would turn a working credential into a broken one.
	token, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher, []string{v}, mcpServer)
	if err != nil || token != "fresh" {
		t.Fatalf("token = %q, err = %v; want the fresh token despite the failed write", token, err)
	}
	// 42P01 is undefined_table, and the driver's own rendering of it names the
	// relation — a stand-in here for the bound value a DETAIL line would quote.
	if !strings.Contains(logged.String(), "SQLSTATE 42P01") {
		t.Errorf("the failed write was not logged as its SQLSTATE: %s", logged.String())
	}
	if strings.Contains(logged.String(), "vault_credentials") {
		t.Errorf("the failed write logged what the driver said: %s", logged.String())
	}
}

// An answer the platform cannot turn into a grant is the credential's fault
// whatever went wrong with it: a body that never finished, one past the cap, or
// a status whose body is not consulted at all. A retryable verdict on any of
// them has nowhere to surface — the work item faults, is reclaimed, writes no
// catalog row, and repeats forever with the operator told nothing.
func TestAnAnswerThatIsNotAGrantIsTheCredentialsFault(t *testing.T) {
	for name, serve := range map[string]http.HandlerFunc{
		// A refusal whose body promises more than it sends, then hangs up:
		// io.ReadAll answers io.ErrUnexpectedEOF. Classifying on the body's arrival
		// would turn a definitive `400 invalid_grant` into an endless retry.
		"a refusal with an unreadable body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_gr`))
			hangUp(w)
		},
		"a 200 with an unreadable body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "4096")
			_, _ = w.Write([]byte(`{"access_token":"never-fin`))
			hangUp(w)
		},
		"a 200 past the cap": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"padding":"`))
			block := bytes.Repeat([]byte("p"), 64<<10)
			for written := 0; written <= 1<<20; written += len(block) {
				if _, err := w.Write(block); err != nil {
					return
				}
			}
			_, _ = w.Write([]byte(`","access_token":"never-read"}`))
		},
		// The same overrun with the grant at the *front*, padded out past the cap
		// with the whitespace a JSON reader skips. Truncating at exactly the cap
		// would hand json.Unmarshal a document that parses, so the answer would be
		// taken for a grant however the endpoint went on to end it.
		"a grant padded past the cap": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"never-read"}`))
			block := bytes.Repeat([]byte(" "), 64<<10)
			for written := 0; written <= 1<<20; written += len(block) {
				if _, err := w.Write(block); err != nil {
					return
				}
			}
			_, _ = w.Write([]byte(`garbage`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			cipher := testCipher(t)
			vaultresolve.AllowLoopbackTokenEndpointForTest(t)
			issuer := httptest.NewServer(serve)
			defer issuer.Close()

			v := newVault(t, pool, false)
			newRefreshableCred(t, pool, cipher, v, refreshable{
				expiresAt: ago(time.Hour), endpoint: issuer.URL,
				sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
			})

			_, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
				[]string{v}, mcpServer)
			if !errorsIsUnusable(err) {
				t.Fatalf("err = %v, want it marked ErrCredentialUnusable", err)
			}
		})
	}
}

// hangUp closes the connection under a response that promised more, so the
// client's body read fails rather than ending cleanly.
func hangUp(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// A lifetime this platform cannot believe leaves the expiry unknown rather than
// producing one. The overflow row is the reason the bound exists: int64 seconds
// times time.Second wraps past zero, and an expiry in the past would refresh
// again on every later dial.
func TestALifetimeThatCannotBeBelievedLeavesTheExpiryUnknown(t *testing.T) {
	for name, expiresIn := range map[string]string{
		"absent":      ``,
		"zero":        `,"expires_in":0`,
		"negative":    `,"expires_in":-3600`,
		"overflowing": `,"expires_in":9223372036854775807`,
		"one second beyond the bound": `,"expires_in":` +
			strconv.FormatInt(10*365*24*60*60+1, 10),
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			cipher := testCipher(t)
			issuer := newTokenEndpoint(t, grants(expiresIn))

			v := newVault(t, pool, false)
			id := newRefreshableCred(t, pool, cipher, v, refreshable{
				expiresAt: ago(time.Hour), endpoint: issuer.URL,
				sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
			})

			ctx := context.Background()
			if _, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer); err != nil {
				t.Fatal(err)
			}
			if doc, _ := storedCred(t, pool, cipher, id); string(doc["expires_at"]) != "null" {
				t.Errorf("stored expires_at = %s, want null", doc["expires_at"])
			}
			if _, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer); err != nil {
				t.Fatal(err)
			}
			if n := issuer.calls(); n != 1 {
				t.Errorf("the issuer was asked %d times; an unknown expiry must not refresh again", n)
			}
		})
	}
}

// The other side of that bound: the largest lifetime this platform will believe
// is believed. Without a row here, tightening the comparison by one second would
// silently downgrade a ten-year grant into the dead end above.
func TestTheLargestBelievableLifetimeIsBelieved(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	tenYears := int64(10 * 365 * 24 * 60 * 60)
	issuer := newTokenEndpoint(t, grants(`,"expires_in":`+strconv.FormatInt(tenYears, 10)))

	v := newVault(t, pool, false)
	id := newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	if _, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
		[]string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	doc, _ := storedCred(t, pool, cipher, id)
	var expiresAt time.Time
	if err := json.Unmarshal(doc["expires_at"], &expiresAt); err != nil {
		t.Fatalf("stored expires_at = %s (%v), want a timestamp", doc["expires_at"], err)
	}
	if !expiresAt.After(time.Now().UTC().AddDate(9, 0, 0)) {
		t.Errorf("stored expires_at = %s, want about ten years out", expiresAt)
	}
}

// The race a dial-time refresh has: two dials of one expired credential both
// exchange, and an issuer that makes refresh tokens single-use refuses the
// second. This resolution is the loser — it read the row before the winner wrote
// it, and the winner's write lands from inside the handler while its own
// exchange is in flight, which is the only way to make that ordering a fact.
//
// The read-back is decided by whether the sealed bytes changed, and the expiry
// gets no vote: an issuer may name no lifetime, or one shorter than the leeway,
// and both are tokens this platform sends elsewhere without complaint. So the
// rows are the winner's rotation under each of those expiries, and nobody having
// written at all.
func TestARefusedRefreshReadsBackARotationThatLandedMeanwhile(t *testing.T) {
	winner := map[string]string{"access_token": "the-winners-token", "refresh_token": "refresh-2"}
	// A reseal that left the access token where it was. This platform writes
	// exactly that itself — a replacement refresh token stored beside an access
	// token it refused to use — and so does an operator updating the client
	// secret. The sealed bytes change and nothing was rotated, so a reader that
	// asked only whether they changed would hand back the token that just failed.
	kept := map[string]string{"access_token": "expired", "refresh_token": "refresh-2"}
	for name, row := range map[string]struct {
		resealed     map[string]string // nil — nobody wrote
		winnerExpiry *time.Time
		want         string // "" — the refusal stands
	}{
		"a rotation an hour out is used":   {winner, ahead(time.Hour), "the-winners-token"},
		"an unknown expiry is used too":    {winner, nil, "the-winners-token"},
		"one inside the leeway, likewise":  {winner, ahead(30 * time.Second), "the-winners-token"},
		"and with no rotation at all":      {nil, nil, ""},
		"nor a reseal that kept the token": {kept, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			cipher := testCipher(t)
			ctx := context.Background()

			var id string
			issuer := newTokenEndpoint(t, func(w http.ResponseWriter, _ int) {
				if row.resealed != nil {
					rotateSealed(t, pool, cipher, id, row.resealed)
					stampExpiry(t, pool, id, row.winnerExpiry)
				}
				// The refresh token this exchange carries has been retired.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			})

			v := newVault(t, pool, false)
			id = newRefreshableCred(t, pool, cipher, v, refreshable{
				expiresAt: ago(time.Hour), endpoint: issuer.URL,
				sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
			})

			got, err := vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
			if row.want == "" {
				if !errorsIsUnusable(err) {
					t.Fatalf("err = %v, want the refusal to stand as ErrCredentialUnusable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the refused dial reported a failure for a credential that had been rotated: %v", err)
			}
			if got != row.want {
				t.Errorf("resolved %q, want the rotation that landed meanwhile", got)
			}
		})
	}
}

// stampExpiry writes a credential's expires_at the way another writer would,
// leaving the rest of the document alone.
func stampExpiry(t *testing.T, pool *pgxpool.Pool, id string, at *time.Time) {
	t.Helper()
	stamped := json.RawMessage("null")
	if at != nil {
		encoded, err := json.Marshal(at)
		if err != nil {
			t.Error(err)
			return
		}
		stamped = encoded
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE vault_credentials SET auth = jsonb_set(auth, '{expires_at}', $2::jsonb) WHERE id = $1`,
		id, []byte(stamped)); err != nil {
		t.Errorf("stamp expiry: %v", err)
	}
}

// The write that stores a rotation does not inherit the caller's deadline: by
// then the issuer may have retired the refresh token this exchange spent, and
// abandoning the write would leave the row holding a token nothing honours.
func TestARotationIsStoredEvenWhenTheCallerIsAlreadyDone(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The cancellation is staged from inside the reseal, which is the first thing
	// the write does and the first moment the exchange is certainly over. Staging
	// it from the issuer's handler would only abort the exchange itself.
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(`,"refresh_token":"refresh-2","expires_in":3600`))

	v := newVault(t, pool, false)
	// Sealed with the plain cipher: the wrapper is handed only to the resolution
	// under test, so the fixture's own seal does not trip it.
	id := newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	if _, err := vaultresolve.MCPCredentialFor(ctx, pool,
		cancellingCipher{Cipher: cipher, cancel: cancel}, []string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	_, sealed := storedCred(t, pool, cipher, id)
	if sealed["refresh_token"] != "refresh-2" {
		t.Errorf("stored refresh_token = %q, want the rotation stored despite the cancellation",
			sealed["refresh_token"])
	}
}

// cancellingCipher ends the caller's context as the rotation is being sealed —
// the queue lease lapsing, or the session being interrupted, between a
// successful exchange and its write.
type cancellingCipher struct {
	secrets.Cipher
	cancel context.CancelFunc
}

func (c cancellingCipher) Encrypt(ctx context.Context, plaintext []byte) ([]byte, string, error) {
	c.cancel()
	return c.Cipher.Encrypt(ctx, plaintext)
}

// A stored document whose expiry this package cannot read is the credential's
// fault. Guessing — dialling with the token and hoping — would send a
// credential whose expiry was never checked, which is the one thing the refresh
// exists to prevent.
func TestAnUnreadableAuthDocumentIsTheCredentialsFault(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	ctx := context.Background()

	sealed, err := json.Marshal(map[string]string{"access_token": "expired"})
	if err != nil {
		t.Fatal(err)
	}
	ct, keyID, err := cipher.Encrypt(ctx, sealed)
	if err != nil {
		t.Fatal(err)
	}
	v := newVault(t, pool, false)
	if _, err := pool.Exec(ctx,
		`INSERT INTO vault_credentials
		   (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
		 VALUES ($1, $2, 'mcp_oauth', $3::jsonb, $4, $5, $6)`,
		domain.NewID("vcrd").String(), v,
		`{"type":"mcp_oauth","mcp_server_url":"`+mcpServer+`","expires_at":5}`,
		ct, keyID, "url:"+mcpServer); err != nil {
		t.Fatalf("insert credential: %v", err)
	}

	_, err = vaultresolve.MCPCredentialFor(ctx, pool, cipher, []string{v}, mcpServer)
	if !errorsIsUnusable(err) {
		t.Fatalf("err = %v, want it marked ErrCredentialUnusable", err)
	}
}

// The client secret is sealed, never stored in the auth document, so the arm
// that needs it has to read it out of the sealed half.
func TestTheClientSecretComesFromTheSealedDocument(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(`,"expires_in":3600`))

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL, authArm: "client_secret_basic",
		sealed: map[string]string{
			"access_token": "expired", "refresh_token": "refresh-1", "client_secret": "s3cret",
		},
	})

	if _, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
		[]string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	user, pass, ok := basicAuthOf(issuer.authorization(t, 0))
	if !ok || user != "client-1" || pass != "s3cret" {
		t.Errorf("Authorization carried (%q, %q, %v)", user, pass, ok)
	}
}

// The other secret-bearing arm, on this path rather than on the request
// builder's: client_secret_post reads the same sealed field and puts both
// halves in the body.
func TestClientSecretPostReadsTheSameSealedField(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(`,"expires_in":3600`))

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL, authArm: "client_secret_post",
		sealed: map[string]string{
			"access_token": "expired", "refresh_token": "refresh-1", "client_secret": "s3cret",
		},
	})

	if _, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
		[]string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	form := issuer.form(t, 0)
	if form.Get("client_id") != "client-1" || form.Get("client_secret") != "s3cret" {
		t.Errorf("exchange sent client_id=%q client_secret=%q", form.Get("client_id"), form.Get("client_secret"))
	}
	if h := issuer.authorization(t, 0); h != "" {
		t.Errorf("client_secret_post also sent Authorization %q", h)
	}
}

// resource and scope are stored on the credential and have to reach the
// exchange from there — an RFC 8707 issuer that requires the resource refuses
// every refresh without it, and nothing else in the suite would notice.
func TestTheStoredResourceAndScopeReachTheExchange(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher := testCipher(t)
	issuer := newTokenEndpoint(t, grants(`,"expires_in":3600`))

	v := newVault(t, pool, false)
	newRefreshableCred(t, pool, cipher, v, refreshable{
		expiresAt: ago(time.Hour), endpoint: issuer.URL,
		resource: "https://mcp.example.com/mcp", scope: "mcp:read mcp:write",
		sealed: map[string]string{"access_token": "expired", "refresh_token": "refresh-1"},
	})

	if _, err := vaultresolve.MCPCredentialFor(context.Background(), pool, cipher,
		[]string{v}, mcpServer); err != nil {
		t.Fatal(err)
	}
	form := issuer.form(t, 0)
	if form.Get("resource") != "https://mcp.example.com/mcp" {
		t.Errorf("exchange sent resource=%q", form.Get("resource"))
	}
	if form.Get("scope") != "mcp:read mcp:write" {
		t.Errorf("exchange sent scope=%q", form.Get("scope"))
	}
}
