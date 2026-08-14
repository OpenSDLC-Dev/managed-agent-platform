package vaultresolve

import (
	"context"
	"encoding/json"
	"errors"
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

// persistTimeout bounds the write that stores a rotation. It is short and its
// own, because the exchange has already happened by then and the caller's
// deadline may be spent — see persistRotation.
const persistTimeout = 5 * time.Second

// maxExpiresIn is the largest lifetime a grant may claim and still be believed:
// ten years, which no access token has and which leaves the multiplication into
// a time.Duration nowhere near overflowing. A grant claiming more is treated as
// naming no lifetime at all rather than as naming a nonsense one — the
// difference matters, because `time.Duration(math.MaxInt64) * time.Second`
// wraps to a negative and would store an expiry already in the past, refreshing
// again on every later dial.
const maxExpiresIn = 10 * 365 * 24 * 60 * 60

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
//
// The transport is spelled out for the reasons internal/mcp's DefaultClient
// gives at length, and this client needs every one of them: it is package-level,
// so it outlives each exchange, and the endpoint is third-party. No Proxy, or
// the guard would be vetting a proxy's address while the proxy fetched what the
// URL named. ForceAttemptHTTP2, because setting DialContext turns HTTP/2 off
// unless it is set. Idle bounds, or a connection to an issuer never dialled
// again is held until the process ends. And MaxResponseHeaderBytes, which is the
// only bound on a header block — refreshBodyMax bounds the body and nothing
// else, so without this a hostile endpoint spends megabytes per exchange on
// headers alone.
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
		ForceAttemptHTTP2:      true,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
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
		// A refusal may be this dial having lost a race rather than a credential
		// that is broken: two dials of one expired credential both exchange, and
		// an issuer that makes refresh tokens single-use answers the second
		// `invalid_grant`. The winner's rotation is in the row by then, so read
		// it — reporting an authentication failure for a credential that now
		// holds a working token would be wrong, and on an executing tool call
		// that answer is committed rather than retried.
		if errors.Is(err, ErrCredentialUnusable) {
			if token, ok := rotatedElsewhere(ctx, db, cipher, w); ok {
				return token, nil
			}
		}
		return "", err
	}
	// Checked before anything is sealed. A token that cannot become a header is
	// no more usable stored than sent, and storing it would stamp a fresh expiry
	// over the credential — after which every later dial reads the same
	// unsendable token back without refreshing again.
	if !sendableAsHeader(grant.AccessToken) {
		return "", unusable(fmt.Errorf(
			"vaultresolve: credential %s was issued a token that cannot be sent as a header", w.id))
	}
	sealed["access_token"] = grant.AccessToken
	if grant.RefreshToken != "" {
		sealed["refresh_token"] = grant.RefreshToken
	}
	persistRotation(ctx, db, cipher, w, sealed, grant.ExpiresIn)
	return grant.AccessToken, nil
}

// rotatedElsewhere re-reads a credential whose refresh was just refused, and
// answers the token another resolution stored in the meantime — see the call
// site for the race this closes.
//
// The expiry is what decides it, and it decides both questions at once: a row
// nobody rotated still carries the expiry that made this dial refresh, so it is
// refused here exactly as a rotation that is itself already due would be. There
// is no separate "did the row change" test, because a changed row that is still
// expired has nothing to offer either.
func rotatedElsewhere(ctx context.Context, db DB, cipher secrets.Cipher, w mcpRow) (string, bool) {
	var authDoc, ciphertext []byte
	var keyID *string
	if err := db.QueryRow(ctx,
		`SELECT auth, secret_ciphertext, secret_key_id FROM vault_credentials
		   WHERE id = $1 AND archived_at IS NULL`, w.id).Scan(&authDoc, &ciphertext, &keyID); err != nil {
		return "", false
	}
	var doc mcpOAuthDoc
	if err := json.Unmarshal(authDoc, &doc); err != nil {
		return "", false
	}
	if doc.ExpiresAt == nil || !time.Now().UTC().Add(refreshLeeway).Before(*doc.ExpiresAt) {
		return "", false
	}
	if ciphertext == nil {
		return "", false
	}
	sealed, err := openSealed(ctx, cipher, w.id, ciphertext, keyID)
	if err != nil {
		return "", false
	}
	token := sealed["access_token"]
	if token == "" || !sendableAsHeader(token) {
		return "", false
	}
	return token, true
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
		// An address the guard refuses can never become reachable, so what is
		// wrong is the stored endpoint rather than the network.
		if errors.Is(err, dialguard.ErrRefused) {
			return zero, unusable(fmt.Errorf(
				"vaultresolve: credential %s names a token endpoint this platform will not dial", credID))
		}
		// Otherwise deliberately plain: the issuer was unreachable, which says
		// nothing about the credential. Wrapping err would put a URL this platform
		// dialled into a stored reason, and the endpoint is operator-supplied.
		return zero, fmt.Errorf("vaultresolve: token endpoint unreachable for credential %s", credID)
	}
	defer resp.Body.Close()
	// Classified before the body is read, and not after: the body is not
	// consulted for a non-2xx at all, and a body that fails to arrive would
	// otherwise turn a definitive refusal into a retry that never ends.
	//
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
	// One byte past the cap, so an answer that reached it can be told from one
	// that merely ended there.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, refreshBodyMax+1))
	if err != nil {
		return zero, fmt.Errorf("vaultresolve: token endpoint answer unreadable for credential %s", credID)
	}
	if len(raw) > refreshBodyMax {
		// This platform's own cap cut the answer short, so what the issuer said
		// is unknown — which is not the same as it having said no.
		return zero, fmt.Errorf("vaultresolve: token endpoint answer too large for credential %s", credID)
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
// working credential into a broken one. What a lost write costs depends on the
// issuer — a wasted exchange on the next dial for one that keeps its refresh
// token, and against one that rotates single-use tokens a credential the
// operator has to rewrite, since the replacement existed only in this process.
// That is why the write does not simply inherit the caller's deadline below.
//
// The write is a compare-and-set on the ciphertext this resolution read, matching
// what the validate endpoint does for the same reason: the exchange spent seconds
// on network I/O, and a concurrent rotation, update or archive may have landed
// meanwhile. Two dials that refresh at once therefore agree on one stored result,
// and the one whose exchange the issuer refused reads the winner's rotation back
// (rotatedElsewhere) rather than reporting a failure.
func persistRotation(ctx context.Context, db DB, cipher secrets.Cipher, w mcpRow,
	sealed map[string]string, expiresIn int64) {
	// Patch the stored document rather than re-render it: an auth document may
	// carry fields this package does not read, and re-rendering would drop them.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(w.authDoc, &doc); err != nil {
		return
	}
	// A lifetime this cannot believe — none named, none positive, or one so large
	// the multiplication below would wrap past zero — leaves the expiry unknown,
	// which is null and not the old value: keeping an expiry already in the past
	// would make every later dial refresh again, hammering the issuer for a token
	// it just issued. Unknown means no further proactive refresh for this
	// credential; the token is sent until the server refuses it, and recovering
	// from that needs the operator (docs/DIVERGENCES.md records it).
	expiry := json.RawMessage("null")
	if expiresIn > 0 && expiresIn <= maxExpiresIn {
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
	// The write gets a deadline of its own, detached from the caller's. By here
	// the issuer may already have retired the refresh token this exchange spent,
	// so abandoning the write because a queue lease lapsed or a session was
	// interrupted would leave the row holding a token nothing will honour again.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	ciphertext, keyID, err := cipher.Encrypt(writeCtx, plain)
	if err != nil {
		// Without the cipher's own error: it is the one error here whose text is
		// derived from the plaintext it was handed, which is the secret itself.
		slog.WarnContext(ctx, "vaultresolve: could not reseal a refreshed credential", "credential", w.id)
		return
	}
	tag, err := db.Exec(writeCtx,
		`UPDATE vault_credentials SET auth = $2, secret_ciphertext = $3, secret_key_id = $4, updated_at = now()
		   WHERE id = $1 AND archived_at IS NULL AND secret_ciphertext = $5`,
		w.id, newAuthDoc, ciphertext, keyID, w.ciphertext)
	switch {
	case err != nil:
		// With the error, unlike the reseal above: every value bound here is
		// ciphertext or the non-secret auth document, so nothing the driver can
		// quote back is a secret — and a rotation that stops landing is otherwise
		// undiagnosable.
		slog.WarnContext(ctx, "vaultresolve: could not store a refreshed credential",
			"credential", w.id, "error", err)
	case tag.RowsAffected() == 0:
		// The compare-and-set found the row already moved on. Said out loud
		// because from outside it looks exactly like a write that silently never
		// happened, and the two call for different reactions.
		slog.InfoContext(ctx, "vaultresolve: a refreshed credential was not stored; the row had moved on",
			"credential", w.id)
	}
}
