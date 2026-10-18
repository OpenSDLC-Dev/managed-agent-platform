package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The admin API's check and test routes (admin.Prober), on demand alone
// (#953): a credential check lists the vendor's models with the key, which
// costs no tokens; a deployment test sends the deployment one minimal
// request through the attempt path every call takes, which costs a few.
// Each makes one request and no retry, and stores nothing of what it learns.

// prober is the gateway's request path, lent to the admin API.
type prober struct{ h *handler }

// NewProber returns the admin API's prober over cfg, as New reads it — but
// for Admin, which it does not read. It keeps a client and opened keys of its
// own, apart from the handler New returns.
func NewProber(cfg Config) (admin.Prober, error) {
	h, err := newHandler(cfg)
	if err != nil {
		return nil, err
	}
	return prober{h}, nil
}

// checkTimeout bounds a credential check's request on each protocol.
var checkTimeout = 10 * time.Second

// maxModelList bounds a list-models answer, which lists hundreds of models
// at most: Gitee's, its whole public catalogue, is some 20 KB.
const maxModelList = 4 << 20

// modelsPath is each protocol's list-models route under a base URL, the
// largest page asked for where the route pages — 1000 on Anthropic's and on
// Gemini's — so one request lists every model a vendor has.
var modelsPath = map[profile.Protocol]string{
	profile.Anthropic: "/v1/models?limit=1000",
	profile.OpenAI:    "/models",
	profile.Gemini:    "/models?pageSize=1000",
}

func (p prober) CheckCredential(ctx context.Context, prov store.Provider, c store.Credential) []admin.CredentialCheck {
	prof, _ := profile.Lookup(prov.Profile)
	out := make([]admin.CredentialCheck, 0, len(c.Protocols))
	for _, up := range c.Protocols {
		out = append(out, p.check(ctx, prov, prof, c, up))
	}
	return out
}

// check lists models on up with c's key, unless the profile records that
// the vendor serves no list-models route there. It is ok when the vendor
// answers a 2xx holding a list: Zhipu's Anthropic host refuses a bad key
// with a 200 that holds none (profile).
func (p prober) check(ctx context.Context, prov store.Provider, prof profile.Profile, c store.Credential, up profile.Protocol) admin.CredentialCheck {
	res := admin.CredentialCheck{Protocol: up}
	if listed, known := prof.ModelList[up]; known && !listed {
		res.Error = &admin.ProbeError{Type: "not_found_error", Message: fmt.Sprintf(
			"%s serves no list-models route on its %s endpoint, so the key cannot be checked there; a deployment test can",
			prof.DisplayName, protocolNames[up])}
		return res
	}
	key, err := p.h.open(ctx, c)
	if err != nil {
		slog.ErrorContext(ctx, "modelgateway: credential could not be opened", "credential", c.ID, "error", err)
		res.Error = &admin.ProbeError{Type: "api_error", Message: "internal error"}
		return res
	}
	red := provider.NewRedactor(provider.Config{APIKey: string(key), Headers: prov.Headers})
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prov.Endpoints[up]+modelsPath[up], nil)
	if err != nil {
		res.Error = &admin.ProbeError{Type: "api_error", Message: red.Error(err).Error()}
		return res
	}
	if up == profile.Anthropic { // a provider's own, set next, wins, as on an attempt
		req.Header.Set("anthropic-version", defaultAnthropicVersion)
	}
	vendorHeaders(ctx, req.Header, prov, prof, up, key)
	start := time.Now()
	resp, err := p.h.clientFor(prof).Do(req)
	if err != nil {
		res.LatencyMS = since(start)
		res.Error = transportError("the request failed", err, red)
		return res
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxModelList+1))
	res.LatencyMS, res.Status = since(start), &resp.StatusCode
	s := resp.StatusCode
	switch {
	case err != nil:
		res.Error = transportError("the answer broke off", err, red)
	case len(b) > maxModelList:
		res.Error = &admin.ProbeError{Type: "api_error", Message: fmt.Sprintf("the answer exceeds the gateway's bound of %d bytes", maxModelList)}
	case s >= 300 && s < 400:
		res.Error = &admin.ProbeError{Type: "api_error", Message: fmt.Sprintf("the vendor answered a redirect (%d), which the gateway does not follow", s)}
	case s == http.StatusNotFound:
		res.Error = &admin.ProbeError{Type: "not_found_error",
			Message: "the endpoint answered 404 on its list-models route, which it may not serve: " + vendorMessage(red, b)}
	case s >= 400:
		res.Error = vendorError(redactJSON(red, b), s)
	default:
		ids, ok := modelIDs(up, b)
		if !ok {
			res.Error = &admin.ProbeError{Type: "api_error",
				Message: fmt.Sprintf("the vendor answered %d with no model list: %s", s, vendorMessage(red, b))}
			break
		}
		for i := range ids {
			ids[i] = red.String(ids[i])
		}
		res.OK, res.Models = true, ids
	}
	return res
}

// modelIDs is the ids a list-models answer lists, each read by its exact
// key: data[].id on the Anthropic and OpenAI protocols, and on Gemini's
// models[].name without its models/ prefix, which a deployment's upstream id
// leaves out (geminiPath). It reports false when the answer holds no list.
func modelIDs(up profile.Protocol, b []byte) ([]string, bool) {
	list, field := "data", "id"
	if up == profile.Gemini {
		list, field = "models", "name"
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(member(b, list), &items) != nil || items == nil {
		return nil, false
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		var id string
		if json.Unmarshal(it[field], &id) == nil && id != "" {
			if up == profile.Gemini {
				id = strings.TrimPrefix(id, "models/")
			}
			ids = append(ids, id)
		}
	}
	return ids, true
}

// testTimeout bounds a test's attempt as a whole, which a caller's call is
// not (provider.StallGuard): an answer of one token has no long generation
// to wait out, so a vendor that sends it a byte at a time is cut off too.
var testTimeout = 30 * time.Second

// A test is written to the ledger under these: no API key made it, and the
// route is the test's, so Usage can tell it from an agent's call.
const (
	testKeyID    = "admin"
	testEndpoint = "deployment_test"
)

// testRequest is the minimal request a deployment of d's kind is tested
// with, and the route it takes. A chat test asks for one token: every chat
// model the live tier covers, which has no Gemini one, answers max_tokens 1
// with a 200 — the models that think by default with one token of thinking,
// MiniMax-M3.1-Flash-Preview with content null — on both its endpoints
// (#953, probed 2026-10-18).
func testRequest(d store.Deployment) (string, map[string]json.RawMessage) {
	top := map[string]json.RawMessage{"model": encodeJSON(d.UpstreamModel)}
	switch d.Kind {
	case store.KindEmbedding:
		top["input"] = json.RawMessage(`"ping"`)
		return "/v1/embeddings", top
	case store.KindRerank:
		top["query"], top["documents"] = json.RawMessage(`"ping"`), json.RawMessage(`["pong"]`)
		return "/v1/rerank", top
	}
	top["max_tokens"] = json.RawMessage(`1`)
	top["messages"] = json.RawMessage(`[{"role":"user","content":"ping"}]`)
	return "/v1/messages", top
}

// TestDeployment tests with every credential at once, so a test of many
// takes the bound of one.
func (p prober) TestDeployment(ctx context.Context, d store.Deployment, prov store.Provider, cs []store.Credential) []admin.DeploymentTest {
	out := make([]admin.DeploymentTest, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Go(func() { out[i] = p.test(ctx, d, prov, c) })
	}
	wg.Wait()
	return out
}

// test makes one attempt at d with c — the one catalog.Plan would make, on
// the protocol it would take — and writes it to the ledger as a test.
func (p prober) test(ctx context.Context, d store.Deployment, prov store.Provider, c store.Credential) admin.DeploymentTest {
	res := admin.DeploymentTest{CredentialID: c.ID}
	path, top := testRequest(d)
	in := routes[path].proto
	up := catalog.UpstreamProtocol(prov, c, in)
	if up == "" {
		res.Error = &admin.ProbeError{Type: "invalid_request_error", Message: fmt.Sprintf(
			"credential %s is allowed on none of the protocols that %s requests reach provider %s on", c.ID, protocolNames[in], prov.ID)}
		return res
	}
	res.Protocol = &up
	at := catalog.Attempt{Deployment: d, Provider: prov, Credential: c, Endpoint: prov.Endpoints[up], Protocol: up}
	bound := testTimeout
	start := time.Now()
	out := &outcome{}
	cl := call{proto: in, top: top, alias: d.UpstreamModel, path: path, start: start, out: out, within: bound}
	if in == profile.Anthropic {
		cl.header = forwarded(http.Header{})
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, path, nil)
	if err != nil {
		res.Error = &admin.ProbeError{Type: "api_error", Message: err.Error()}
		return res
	}
	f, _ := p.h.tracedAttempt(discard{http.Header{}}, r, cl, at, false)
	latency := time.Since(start)
	ms := latency.Milliseconds()
	res.LatencyMS = &ms
	if out.upstream != 0 {
		res.Status = &out.upstream
	}
	status, errType := out.status, out.errType
	switch {
	// The bound cut the attempt off: its error says so, or, for an answer
	// relayVectors cut off, which keeps no error, the time does.
	case f != nil && errors.Is(f.err, context.DeadlineExceeded), f == nil && errType != "" && latency >= bound:
		status, errType = http.StatusGatewayTimeout, "timeout_error"
		res.Error = &admin.ProbeError{Type: errType, Message: fmt.Sprintf("the vendor did not answer in full within the test's bound of %s", bound)}
	case f != nil:
		status, errType = f.status, f.errorType()
		res.Error = testError(f, out.upstream)
	case errType != "": // an embeddings or rerank answer cut off (relayVectors)
		res.Error = &admin.ProbeError{Type: errType, Message: "the answer broke off, or ran past the gateway's bound"}
	default:
		res.OK = true
	}
	if t := out.tokens; t != nil {
		res.Usage = &admin.TestUsage{InputTokens: t.Input, OutputTokens: t.Output, CacheWriteTokens: t.CacheWrite, CacheReadTokens: t.CacheRead}
	}
	u := store.Usage{RequestID: requestID(r), APIKeyID: testKeyID, Model: d.UpstreamModel, DeploymentID: d.ID, CredentialID: c.ID,
		Protocol: string(in), Endpoint: testEndpoint, Status: status, ErrorType: ledgerText(errType), Tokens: out.tokens, Latency: latency}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if _, err := p.h.cfg.Store.RecordUsage(rctx, u, false); err != nil {
		slog.ErrorContext(rctx, "modelgateway: a deployment test could not be recorded", "request_id", u.RequestID,
			"deployment", d.ID, "credential", c.ID, "error", err)
	}
	return res
}

// testError is why a test's attempt failed, as the vendor said it where it
// answered: a refusal of the credential, which a caller is given as the
// gateway's 502, and a converted attempt's error, which a caller is given
// converted, keep the vendor's type and message here.
func testError(f *failure, upstream int) *admin.ProbeError {
	switch {
	case f.detail != nil:
		return vendorError(f.detail, upstream)
	case f.body != nil:
		return vendorError(f.body, upstream)
	}
	return &admin.ProbeError{Type: f.typ, Message: excerpt(f.err.Error())}
}

// vendorError is a vendor's error body b, its credentials removed already,
// answered with status: the type b names, else a refused Gemini key's
// (geminiKeyRefused), else the one Anthropic gives status
// (messagesErrorType); and its message (errorMessageOr), each cut short.
func vendorError(b []byte, status int) *admin.ProbeError {
	var typ string
	if json.Unmarshal(member(b, "error", "type"), &typ) != nil || typ == "" {
		typ = messagesErrorType("", status)
		if geminiKeyRefused(b) {
			typ = "authentication_error"
		}
	}
	msg := errorMessageOr(b)
	if strings.TrimSpace(msg) == "" {
		msg = fmt.Sprintf("the vendor answered %d with an empty body", status)
	}
	return &admin.ProbeError{Type: excerpt(typ), Message: excerpt(msg)}
}

// vendorMessage is a vendor's error body as a check reports it: its message
// (errorMessageOr), its credentials removed, cut short.
func vendorMessage(red provider.Redactor, b []byte) string {
	return excerpt(errorMessageOr(redactJSON(red, b)))
}

// errorMessageOr is an error body's message (errorMessage), or the body
// itself where it names none, as Zhipu's {"code":401,"msg":...} does not.
func errorMessageOr(b []byte) string {
	if msg := errorMessage(b); msg != "" {
		return msg
	}
	return string(b)
}

// maxMessage bounds the vendor text a result carries: an error page can be
// a whole HTML document.
const maxMessage = 1 << 10

// excerpt cuts s to maxMessage bytes, at a character's start, marking the cut.
func excerpt(s string) string {
	if c := cut(s, maxMessage); c != s {
		return c + "…"
	}
	return s
}

// transportError is a check's request that got no whole answer.
func transportError(what string, err error, red provider.Redactor) *admin.ProbeError {
	typ := "api_error"
	if errors.Is(err, context.DeadlineExceeded) {
		typ = "timeout_error"
	}
	return &admin.ProbeError{Type: typ, Message: what + ": " + red.Error(err).Error()}
}

func since(start time.Time) *int64 {
	ms := time.Since(start).Milliseconds()
	return &ms
}

// discard is the writer a test's attempt answers into: the test reads the
// answer from the attempt's outcome, so its bytes are dropped.
type discard struct{ header http.Header }

func (d discard) Header() http.Header         { return d.header }
func (d discard) Write(b []byte) (int, error) { return len(b), nil }
func (d discard) WriteHeader(int)             {}
