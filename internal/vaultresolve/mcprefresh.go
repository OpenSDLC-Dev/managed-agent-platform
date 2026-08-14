package vaultresolve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/dialguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/oauthrefresh"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// refreshLeeway is how far ahead of expires_at a token counts as expired. It
// absorbs two things at once: the skew between this platform's clock and the
// issuer's, which is what makes expires_at approximate at all, and the flight
// time of the dial the token is about to be used on — a token that expires while
// the request is in the air is refused exactly like one that expired before it.
const refreshLeeway = time.Minute

// refreshTimeout bounds the whole token exchange, body read included. It is the
// validate probe's budget (internal/api/vaultvalidate.go), for the same reason:
// this runs inside a work item holding a queue lease, and an issuer that never
// answers must not hold it until the lease expires.
const refreshTimeout = 10 * time.Second

// refreshBodyMax bounds the token response. Nothing here captures the body — it
// is parsed for a grant and dropped — so this only has to be larger than any
// real grant and small enough that a hostile endpoint cannot spend the
// executor's memory.
const refreshBodyMax = 1 << 20

// refreshIPAllowed is the address guard under the token-endpoint dial, and the
// seam a test overrides to reach an httptest server on loopback. The endpoint is
// operator-supplied and dialled from inside the executor, so it is an SSRF
// vector on exactly the terms internal/dialguard describes.
var refreshIPAllowed = dialguard.IPAllowed

// refreshClient dials the credential's own token endpoint. Redirects are never
// followed: a 307 or 308 replays the POST body — the refresh token, and a
// client_secret_post secret — to the redirect target, an exfiltration the
// per-dial address guard cannot see because it vets the hop's address and not
// whether a hop should happen at all.
var refreshClient = &http.Client{
	Timeout: refreshTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: refreshTimeout,
			Control: dialguard.Control(func(ip net.IP) error { return refreshIPAllowed(ip) }),
		}).DialContext,
	},
}

// mcpOAuthDoc is the part of an mcp_oauth auth document a refresh reads. The
// document is stored as the reference renders it, so absent and null are both a
// nil pointer here and both mean "no news", never "empty".
type mcpOAuthDoc struct {
	ExpiresAt *time.Time       `json:"expires_at"`
	Refresh   *mcpOAuthRefresh `json:"refresh"`
}

type mcpOAuthRefresh struct {
	ClientID          string `json:"client_id"`
	TokenEndpoint     string `json:"token_endpoint"`
	TokenEndpointAuth struct {
		Type string `json:"type"`
	} `json:"token_endpoint_auth"`
	Resource *string `json:"resource"`
	Scope    *string `json:"scope"`
}

// refreshIfDue exchanges an mcp_oauth credential's refresh token for a new
// access token when the stored one has expired, persists the rotation, and
// returns the fresh token. It returns "" when nothing was due — either the
// credential names no expiry, its expiry is still ahead, or it carries no
// refresh block or refresh token, which is the reference's `no_refresh_token`
// shape: the stale token goes out and the server decides.
//
// The two error classes are the package's own: a refusal from the token
// endpoint is the credential's fault and is marked unusable, so the caller
// answers the tool call rather than retrying it forever; a network failure or a
// 5xx says nothing about the credential and is left plain, so the work item
// faults and comes back.
func refreshIfDue(ctx context.Context, db DB, cipher secrets.Cipher, w mcpRow,
	sealed map[string]string) (string, error) {
	var doc mcpOAuthDoc
	if err := json.Unmarshal(w.authDoc, &doc); err != nil {
		return "", unusable(fmt.Errorf("vaultresolve: credential %s auth document: %w", w.id, err))
	}
	if doc.Refresh == nil || sealed["refresh_token"] == "" || doc.ExpiresAt == nil {
		return "", nil
	}
	if time.Now().UTC().Add(refreshLeeway).Before(*doc.ExpiresAt) {
		return "", nil
	}

	grant, err := exchange(ctx, *doc.Refresh, sealed, w.id)
	if err != nil {
		return "", err
	}
	sealed["access_token"] = grant.AccessToken
	if grant.RefreshToken != "" {
		sealed["refresh_token"] = grant.RefreshToken
	}
	persistRotation(ctx, db, cipher, w, sealed, grant.ExpiresIn)
	return grant.AccessToken, nil
}

// exchange performs the grant and classifies what came back.
func exchange(ctx context.Context, refresh mcpOAuthRefresh, sealed map[string]string,
	credID string) (oauthrefresh.Grant, error) {
	var zero oauthrefresh.Grant
	params := oauthrefresh.Params{
		ClientID:          refresh.ClientID,
		TokenEndpoint:     refresh.TokenEndpoint,
		TokenEndpointAuth: refresh.TokenEndpointAuth.Type,
		Resource:          refresh.Resource,
		Scope:             refresh.Scope,
		RefreshToken:      sealed["refresh_token"],
		ClientSecret:      sealed["client_secret"],
	}
	callCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	req, err := params.NewRequest(callCtx)
	if err != nil {
		// The endpoint was validated as a URL when the credential was written, so
		// this is a stored document that has since become unusable rather than one
		// the API would accept today.
		return zero, unusable(fmt.Errorf(
			"vaultresolve: credential %s has an unusable token endpoint", credID))
	}
	resp, err := refreshClient.Do(req)
	if err != nil {
		// Deliberately plain: the issuer was unreachable, which says nothing about
		// the credential. Wrapping err would put a URL this platform dialled into
		// a stored reason, and the endpoint is operator-supplied.
		return zero, fmt.Errorf("vaultresolve: token endpoint unreachable for credential %s", credID)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, refreshBodyMax))
	if err != nil {
		return zero, fmt.Errorf("vaultresolve: token endpoint answer unreadable for credential %s", credID)
	}
	// 429 and 5xx are the issuer having a moment; every other non-2xx is it
	// saying no. The split is the validate probe's `unknown` and `invalid`
	// (internal/api/vaultvalidate.go, validationStatus), with a redirect — which
	// this client refuses to follow — landing on the refusal side, because a
	// token endpoint that redirects is a stored endpoint that is wrong.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return zero, fmt.Errorf("vaultresolve: token endpoint is unavailable for credential %s (HTTP %d)",
			credID, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, unusable(fmt.Errorf(
			"vaultresolve: credential %s was refused a new token (HTTP %d)", credID, resp.StatusCode))
	}
	grant, ok := oauthrefresh.ParseGrant(raw)
	if !ok {
		// A 200 that is not a grant: an error document, or an endpoint that is not
		// one. Retrying gets the same answer, so it is the credential's fault.
		return zero, unusable(fmt.Errorf(
			"vaultresolve: credential %s got no token from its token endpoint", credID))
	}
	return grant, nil
}

// persistRotation seals the rotated secrets back onto the credential row, so the
// next dial — in this session or another — starts from the new token instead of
// asking the issuer again.
//
// Best-effort by design, and the caller uses the fresh token either way: the
// exchange already succeeded, and failing the dial over a write would turn a
// working credential into a broken one. A rotation that does not land is one
// wasted exchange on the next dial, not a lost token.
//
// The write is a compare-and-set on the ciphertext this resolution read, matching
// what the validate endpoint does for the same reason: the exchange spent seconds
// on network I/O, and a concurrent rotation, update or archive may have landed
// meanwhile. Two dials that refresh at once therefore agree on one stored result;
// an issuer that makes refresh tokens single-use fails the loser's exchange, which
// surfaces as an authentication failure and is retried on the next transition.
func persistRotation(ctx context.Context, db DB, cipher secrets.Cipher, w mcpRow,
	sealed map[string]string, expiresIn int64) {
	// Patch the stored document rather than re-render it: an auth document may
	// carry fields this package does not read, and re-rendering would drop them.
	// A document that already read as mcp_oauth is a JSON object, so reading it
	// again as one cannot fail.
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(w.authDoc, &doc)
	// An issuer that named no lifetime leaves the expiry unknown, which is null
	// and not the old value: keeping an expiry already in the past would make
	// every later dial refresh again, hammering the issuer for a token it just
	// issued. Unknown means no proactive refresh, and a token that does expire is
	// then caught by the server's own 401.
	expiry := json.RawMessage("null")
	if expiresIn > 0 {
		// Marshaling a time.Time cannot fail, and renders the RFC 3339 string the
		// API stores.
		at, _ := json.Marshal(time.Now().UTC().Add(time.Duration(expiresIn) * time.Second))
		expiry = at
	}
	doc["expires_at"] = expiry
	// Every value in doc came out of the unmarshal above or is the expiry just
	// built, so re-rendering it cannot fail; nor can a map of strings.
	newAuthDoc, _ := json.Marshal(doc)
	plain, _ := json.Marshal(sealed)
	ciphertext, keyID, err := cipher.Encrypt(ctx, plain)
	if err != nil {
		slog.WarnContext(ctx, "vaultresolve: could not reseal a refreshed credential", "credential", w.id)
		return
	}
	if _, err := db.Exec(ctx,
		`UPDATE vault_credentials SET auth = $2, secret_ciphertext = $3, secret_key_id = $4, updated_at = now()
		   WHERE id = $1 AND archived_at IS NULL AND secret_ciphertext = $5`,
		w.id, newAuthDoc, ciphertext, keyID, w.ciphertext); err != nil {
		slog.WarnContext(ctx, "vaultresolve: could not store a refreshed credential", "credential", w.id)
	}
}
