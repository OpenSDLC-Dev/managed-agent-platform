package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 10, 1, 2, 3, 456789000, time.UTC)
	for _, dir := range []string{dirNext, dirPrev} {
		c, err := decodeCursor(encodeTimeCursor(dir, ts, "sesn_abc"))
		if err != nil || c.dir != dir || !c.t.Equal(ts) || c.id != "sesn_abc" || c.versioned {
			t.Errorf("time cursor round-trip (%s): %+v, err %v", dir, c, err)
		}
	}
	c, err := decodeCursor(encodeVersionCursor(7))
	if err != nil || !c.versioned || c.version != 7 || c.dir != dirNext {
		t.Errorf("version cursor round-trip: %+v, err %v", c, err)
	}
	// The path cursor keys the memories list. Its key rides base64url'd INSIDE
	// the token, which is what lets a path carry the grammar's own separator.
	path := `/a|b/c\d é`
	c, err = decodeCursor(encodePathCursor(path))
	if err != nil || !c.pathKeyed || c.path != path || c.dir != dirNext {
		t.Errorf("path cursor round-trip: %+v, err %v", c, err)
	}

	inner := base64.RawURLEncoding.EncodeToString([]byte("/a"))
	for name, cursor := range map[string]string{
		"not base64":         "@@@",
		"wrong prefix":       base64.RawURLEncoding.EncodeToString([]byte("x9|n|t|5|id")),
		"bad direction":      base64.RawURLEncoding.EncodeToString([]byte("k1|x|t|5|id")),
		"bad kind":           base64.RawURLEncoding.EncodeToString([]byte("k1|n|z|5")),
		"missing id":         base64.RawURLEncoding.EncodeToString([]byte("k1|n|t|5|")),
		"non-numeric time":   base64.RawURLEncoding.EncodeToString([]byte("k1|n|t|abc|id")),
		"zero version":       base64.RawURLEncoding.EncodeToString([]byte("k1|n|v|0")),
		"extra time parts":   base64.RawURLEncoding.EncodeToString([]byte("k1|n|v|5|junk")),
		"path not base64":    base64.RawURLEncoding.EncodeToString([]byte("k1|n|m|!!!")),
		"empty path":         base64.RawURLEncoding.EncodeToString([]byte("k1|n|m|")),
		"extra path parts":   base64.RawURLEncoding.EncodeToString([]byte("k1|n|m|" + inner + "|junk")),
		"unstorable path":    base64.RawURLEncoding.EncodeToString([]byte("k1|n|m|" + base64.RawURLEncoding.EncodeToString([]byte("/a\x00")))),
		"invalid UTF-8 path": base64.RawURLEncoding.EncodeToString([]byte("k1|n|m|" + base64.RawURLEncoding.EncodeToString([]byte("/a\xff")))),
	} {
		if _, err := decodeCursor(cursor); err == nil {
			t.Errorf("%s: decodeCursor accepted %q", name, cursor)
		}
	}
}

func TestKeysetClause(t *testing.T) {
	ts := time.Now()
	for _, tc := range []struct {
		sort, dir    string
		wantCmp      string
		wantOrder    string
		wantReversed bool
	}{
		{"DESC", dirNext, "<", "DESC", false},
		{"DESC", dirPrev, ">", "ASC", true},
		{"ASC", dirNext, ">", "ASC", false},
		{"ASC", dirPrev, "<", "DESC", true},
	} {
		clause, order, reversed := keysetClause(tc.sort, &cursor{dir: tc.dir, t: ts, id: "x"}, 0)
		if order != tc.wantOrder || reversed != tc.wantReversed {
			t.Errorf("%s/%s: order %s reversed %v", tc.sort, tc.dir, order, reversed)
		}
		wantFragment := "(created_at, id) " + tc.wantCmp + " ($1, $2)"
		if clause != " AND "+wantFragment {
			t.Errorf("%s/%s: clause %q, want …%q", tc.sort, tc.dir, clause, wantFragment)
		}
	}
	if clause, order, reversed := keysetClause("DESC", nil, 0); clause != "" || order != "DESC" || reversed {
		t.Errorf("nil cursor: %q %s %v", clause, order, reversed)
	}
}

// TestResolveMountPath pins the documented rooting rule: every supplied
// mount_path resolves under /mnt/session/uploads, whether or not it starts with
// "/" (managed-agents/files, "File paths"), a path already under that root is
// left alone, and one that climbs above it — or names the root itself — is
// rejected rather than mounted outside.
func TestResolveMountPath(t *testing.T) {
	const root = "/mnt/session/uploads"
	// The root's own name must be compared as a directory, not a string prefix:
	// /mnt/session/uploadsX is a different directory and gets rooted like any
	// other path. A weaker HasPrefix(resolved, root) would mount it outside.
	for name, tc := range map[string]struct{ in, want string }{
		"bare filename":         {"app.log", root + "/app.log"},
		"documented example":    {"/data.csv", root + "/data.csv"},
		"absolute nested":       {"/src/main.py", root + "/src/main.py"},
		"relative nested":       {"rel/path", root + "/rel/path"},
		"outside the root":      {"/workspace/in.txt", root + "/workspace/in.txt"},
		"root-prefixed sibling": {"/mnt/session/uploadsX/f", root + "/mnt/session/uploadsX/f"},
		"already rooted":        {root + "/orders.csv", root + "/orders.csv"},
		"already rooted, dirty": {root + "//a/./b.txt", root + "/a/b.txt"},
		// Shell metacharacters stay legal (only NUL and invalid UTF-8 are barred);
		// the presence probe quotes them (internal/executor/files.go shellQuote).
		"shell metacharacters": {"a b;c'd$e.txt", root + "/a b;c'd$e.txt"},
		// An absolute path is cleaned before rooting, so its ".." resolves away
		// entirely rather than eating a duplicated root. These three clean to
		// /etc/passwd and must therefore land at one and the same place.
		"absolute dotdot":        {"/../../etc/passwd", root + "/etc/passwd"},
		"dotdot through root":    {root + "/a/../../../../etc/passwd", root + "/etc/passwd"},
		"dotdot, single":         {"/../etc/passwd", root + "/etc/passwd"},
		"dotdot inside the root": {root + "/../x", root + "/mnt/session/x"},
		// The bound is on the resolved path, so this is the longest acceptable one.
		"at the byte bound": {root + "/" + strings.Repeat("a", maxMountPathBytes-len(root)-1),
			root + "/" + strings.Repeat("a", maxMountPathBytes-len(root)-1)},
		// Storability is judged on the resolved path too: an unstorable byte in a
		// segment that cleaning removes never reaches the jsonb column, so it is
		// not a rejection. Bounding the caller's spelling instead would refuse it.
		"unstorable byte cleaned away": {"\xff/../b.txt", root + "/b.txt"},
	} {
		got, err := resolveMountPath(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s: resolveMountPath(%q) = %q, %v; want %q", name, tc.in, got, err, tc.want)
		}
		if len(got) > maxMountPathBytes {
			t.Errorf("%s: resolved to %d bytes, over the %d bound", name, len(got), maxMountPathBytes)
		}
	}

	for name, in := range map[string]string{
		"the root itself":         "/",
		"the root, spelled":       root,
		"dot":                     ".",
		"relative escape":         "../etc/passwd",
		"deep relative escape":    "a/../../../etc/passwd",
		"one byte over the bound": root + "/" + strings.Repeat("a", maxMountPathBytes-len(root)),
		// Under the bound as spelled, over it once rooted — the bound is on what
		// gets stored, so this must be refused.
		"relative, over the bound only once rooted": strings.Repeat("a", maxMountPathBytes-10),
		"NUL byte":            "/a\x00b",
		"invalid utf-8":       "/a\xffb",
		"empty (never valid)": "",
	} {
		if got, err := resolveMountPath(in); err == nil {
			t.Errorf("%s: resolveMountPath(%q) = %q, want an error", name, in, got)
		}
	}
}

// TestMountPathTakenCleansStoredPaths pins the upgrade edge: a session created
// before #323 can hold a non-canonical literal that names the same file as a
// freshly resolved path, and must still count as taken.
func TestMountPathTakenCleansStoredPaths(t *testing.T) {
	const stored = `[{"type":"file","file_id":"file_x","mount_path":"/mnt/session/uploads//report.csv"}]`
	var resources []json.RawMessage
	if err := json.Unmarshal([]byte(stored), &resources); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resolved, err := resolveMountPath("/report.csv")
	if err != nil {
		t.Fatalf("resolveMountPath: %v", err)
	}
	if !mountPathTaken(resources, resolved) {
		t.Errorf("mountPathTaken(legacy %q, %q) = false; the two name one file",
			"/mnt/session/uploads//report.csv", resolved)
	}
	if mountPathTaken(resources, "/mnt/session/uploads/other.csv") {
		t.Error("mountPathTaken matched an unrelated path")
	}
}

func TestParsePageRejectsBadLimits(t *testing.T) {
	for _, q := range []string{"limit=abc", "limit=-1", "limit=0", "limit=101"} {
		vals, _ := url.ParseQuery(q)
		if _, err := parsePage(vals); err == nil {
			t.Errorf("parsePage accepted %q", q)
		}
	}
	vals, _ := url.ParseQuery("")
	p, err := parsePage(vals)
	if err != nil || p.limit != defaultLimit || p.cur != nil {
		t.Errorf("defaults = %+v (%v)", p, err)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	e := errInvalid("bad %s", "field")
	if e.Error() != "bad field" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestRenderAgentDefaultsNilCollections(t *testing.T) {
	out := renderAgent("agent_x", "n", 1, agentSpec{}, nil, time.Time{}, time.Time{}, nil)
	if out.Metadata == nil || out.Tools == nil || out.MCPServers == nil || out.Skills == nil {
		t.Errorf("renderAgent left nil collections: %+v", out)
	}
}

func TestRenderEnvironmentDefaultsNilMetadata(t *testing.T) {
	out := renderEnvironment("env_x", "n", "", nil, nil, time.Time{}, time.Time{}, nil)
	if out.Metadata == nil {
		t.Error("renderEnvironment left nil metadata")
	}
}

// TestRenderSessionDefaultsNilCollections covers stored rows that predate a
// field (jsonb null / missing keys): rendering must still emit the full
// required wire surface.
func TestRenderSessionDefaultsNilCollections(t *testing.T) {
	out, err := renderSession(sessionRow{
		id: "sesn_x", agentJSON: []byte(`{}`), metaJSON: []byte(`{}`),
		usageJSON: []byte(`{}`), resourcesJSON: []byte(`null`),
	})
	if err != nil {
		t.Fatalf("renderSession: %v", err)
	}
	if out.Agent.Tools == nil || out.Agent.MCPServers == nil || out.Agent.Skills == nil {
		t.Errorf("agent collections nil: %+v", out.Agent)
	}
	if out.Resources == nil || out.VaultIDs == nil || out.OutcomeEvaluations == nil {
		t.Errorf("session collections nil: %+v", out)
	}
}

// TestTracingMiddlewareContinuesRemoteTrace: the server span must join the
// caller's W3C trace context rather than start a fresh trace.
func TestTracingMiddlewareContinuesRemoteTrace(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var inner trace.SpanContext
	h := withTracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner = trace.SpanContextFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	const traceID = "0af7651916cd43dd8448eb211c80319c"
	req.Header.Set("traceparent", "00-"+traceID+"-b7ad6b7169203331-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if inner.TraceID().String() != traceID {
		t.Errorf("handler trace id = %s, want %s (remote context not continued)", inner.TraceID(), traceID)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", span.SpanKind())
	}
	if span.Parent().TraceID().String() != traceID || !span.Parent().IsRemote() {
		t.Errorf("span parent = %+v, want remote parent in trace %s", span.Parent(), traceID)
	}
	if span.Name() != "GET /v1/agents" {
		t.Errorf("span name = %q", span.Name())
	}
}
