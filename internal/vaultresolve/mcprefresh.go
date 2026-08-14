package vaultresolve

import (
	"bytes"
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
	"github.com/jackc/pgx/v5/pgconn"
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
		// The access token is dropped; a replacement refresh token is not. An issuer
		// that retires refresh tokens on use has just spent the stored one, so
		// keeping only the replacement — over the unchanged access token, under the
		// unchanged expiry, which is what makes the next dial exchange again —
		// leaves the credential able to recover on its own. With no replacement
		// there is nothing to store and the row is left as it was.
		if grant.RefreshToken != "" {
			sealed["refresh_token"] = grant.RefreshToken
			persistRotation(ctx, db, cipher, w, sealed, nil)
		}
		slog.WarnContext(ctx, "vaultresolve: a refreshed credential was issued a token "+
			"that cannot be sent as a header", "credential", w.id)
		return "", unusable(fmt.Errorf(
			"vaultresolve: credential %s was issued a token that cannot be sent as a header", w.id))
	}
	sealed["access_token"] = grant.AccessToken
	if grant.RefreshToken != "" {
		sealed["refresh_token"] = grant.RefreshToken
	}
	persistRotation(ctx, db, cipher, w, sealed, expiryFor(grant.ExpiresIn))
	return grant.AccessToken, nil
}

// expiryFor renders the stored expires_at for a grant's claimed lifetime.
//
// A lifetime this cannot believe — none named, none positive, or one so large
// the multiplication would wrap past zero — leaves the expiry unknown, which is
// null and not the old value: keeping an expiry already in the past would make
// every later dial refresh again, hammering the issuer for a token it just
// issued. Unknown means no further proactive refresh for this credential; the
// token is sent until the server refuses it, and recovering from that needs the
// operator (docs/DIVERGENCES.md records it).
func expiryFor(expiresIn int64) json.RawMessage {
	if expiresIn <= 0 || expiresIn > maxExpiresIn {
		return json.RawMessage("null")
	}
	// Marshaling a time.Time cannot fail, and renders the RFC 3339 string the API
	// stores.
	at, _ := json.Marshal(time.Now().UTC().Add(time.Duration(expiresIn) * time.Second))
	return at
}

// rotatedElsewhere re-reads a credential whose refresh was just refused, and
// answers the token another resolution stored in the meantime — see the call
// site for the race this closes.
//
// Whether the sealed bytes changed is what decides it, and nothing else. A row
// nobody wrote still holds the token this dial already read and the issuer just
// refused a replacement for, so there is nothing to answer with; a row somebody
// wrote holds a token this resolution has not tried, which is better than
// failing whatever its expiry says. The expiry deliberately does not get a vote:
// a winner's grant may name no lifetime, or one shorter than the leeway, and
// both are tokens the platform sends elsewhere without complaint — refusing them
// here would report a failure for a credential the very next dial uses happily.
func rotatedElsewhere(ctx context.Context, db DB, cipher secrets.Cipher, w mcpRow) (string, bool) {
	var ciphertext []byte
	var keyID *string
	if err := db.QueryRow(ctx,
		`SELECT secret_ciphertext, secret_key_id FROM vault_credentials
		   WHERE id = $1 AND archived_at IS NULL`, w.id).Scan(&ciphertext, &keyID); err != nil {
		return "", false
	}
	if ciphertext == nil || bytes.Equal(ciphertext, w.ciphertext) {
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
	// The statuses that mean "ask again" are the issuer having a moment — 5xx,
	// and the three 4xx that are about timing rather than about the grant. Every
	// other non-2xx is the issuer saying no, a redirect included: this client
	// refuses to follow one, and a token endpoint that redirects is a stored
	// endpoint that is wrong. The shape is the validate probe's `unknown` and
	// `invalid` (internal/api/vaultvalidate.go, validationStatus); the three
	// named 4xx are this path's own, because a probe's verdict is advice an
	// operator reads while this one settles a tool call for good.
	if askAgainStatus(resp.StatusCode) {
		return zero, fmt.Errorf("vaultresolve: token endpoint is unavailable for credential %s (HTTP %d)",
			credID, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, unusable(fmt.Errorf(
			"vaultresolve: credential %s was refused a new token (HTTP %d)", credID, resp.StatusCode))
	}
	// From here the issuer answered, and an answer this platform cannot turn into
	// a grant is the credential's fault whatever went wrong with it — an error
	// document, an endpoint that is not a token endpoint, a body that never
	// finished, or one past the cap. None of them is a retry away from working,
	// and a retryable verdict here has nowhere to surface: the work item faults,
	// is reclaimed, and writes no catalog row at all, so an operator is told
	// nothing while the exchange repeats forever.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, refreshBodyMax))
	if err != nil {
		return zero, unusable(fmt.Errorf(
			"vaultresolve: credential %s got an unreadable answer from its token endpoint", credID))
	}
	grant, ok := oauthrefresh.ParseGrant(raw)
	if !ok {
		return zero, unusable(fmt.Errorf(
			"vaultresolve: credential %s got no token from its token endpoint", credID))
	}
	return grant, nil
}

// writeCause names why a write failed without quoting anything it was handed. A
// Postgres error is reduced to its SQLSTATE, because the driver renders the
// server's DETAIL and CONTEXT and those quote the value the server choked on —
// and one of the bound values is the auth document, whose `mcp_server_url` and
// `token_endpoint` may carry a credential in their userinfo (the reason
// internal/executor redacts those URLs out of everything it stores). Anything
// else — a deadline, a closed pool — is produced before the server sees a value
// and is safe as it is.
func writeCause(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "SQLSTATE " + pgErr.Code
	}
	return err.Error()
}

// askAgainStatus is the issuer having a moment rather than saying no: every 5xx,
// and the three 4xx that are about timing rather than about the grant — a
// request that timed out, one rate-limited, and one a resumption-safe endpoint
// says arrived too early.
//
// Bounded at 599 rather than left open: net/http will hand back any three-digit
// status, and a code above the 5xx range is outside the registry entirely — an
// endpoint answering one is not reporting a condition that passes.
func askAgainStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusTooEarly:
		return true
	}
	return code >= 500 && code <= 599
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
// A nil expiry leaves the document's own expires_at alone, which is what the
// caller wants when it is storing a rotated refresh token beside an access token
// it is not going to use.
func persistRotation(ctx context.Context, db DB, cipher secrets.Cipher, w mcpRow,
	sealed map[string]string, expiry json.RawMessage) {
	// Patch the stored document rather than re-render it: an auth document may
	// carry fields this package does not read, and re-rendering would drop them.
	// nil covers both halves: a document that is not JSON at all, and one that is
	// the literal `null`, which unmarshals into a nil map without an error and
	// would panic on the write below.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(w.authDoc, &doc); err != nil || doc == nil {
		return
	}
	if expiry != nil {
		doc["expires_at"] = expiry
	}
	// Every value in doc came out of the unmarshal above or is the expiry just
	// built, so re-rendering it cannot fail; nor can a map of strings.
	newAuthDoc, _ := json.Marshal(doc)
	plain, _ := json.Marshal(sealed)
	// Both steps get a deadline of their own, detached from the caller's. Detached
	// because by here the issuer may already have retired the refresh token this
	// exchange spent, so abandoning the write when a queue lease lapsed or a
	// session was interrupted would leave the row holding a token nothing will
	// honour again. Two of them rather than one shared budget because a cipher
	// backend can be a network round trip of its own (OpenBao transit, Cloud KMS),
	// and a slow seal must not spend the write's deadline and produce exactly the
	// loss the detaching is here to prevent.
	base := context.WithoutCancel(ctx)
	sealCtx, cancelSeal := context.WithTimeout(base, persistTimeout)
	ciphertext, keyID, err := cipher.Encrypt(sealCtx, plain)
	cancelSeal()
	if err != nil {
		// Without the cipher's own error: it is the one error here whose text is
		// derived from the plaintext it was handed, which is the secret itself.
		slog.WarnContext(ctx, "vaultresolve: could not reseal a refreshed credential", "credential", w.id)
		return
	}
	writeCtx, cancelWrite := context.WithTimeout(base, persistTimeout)
	defer cancelWrite()
	tag, err := db.Exec(writeCtx,
		`UPDATE vault_credentials SET auth = $2, secret_ciphertext = $3, secret_key_id = $4, updated_at = now()
		   WHERE id = $1 AND archived_at IS NULL AND secret_ciphertext = $5`,
		w.id, newAuthDoc, ciphertext, keyID, w.ciphertext)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "vaultresolve: could not store a refreshed credential",
			"credential", w.id, "cause", writeCause(err))
	case tag.RowsAffected() == 0:
		// The compare-and-set found the row already moved on. Said out loud
		// because from outside it looks exactly like a write that silently never
		// happened, and the two call for different reactions.
		slog.InfoContext(ctx, "vaultresolve: a refreshed credential was not stored; the row had moved on",
			"credential", w.id)
	}
}
