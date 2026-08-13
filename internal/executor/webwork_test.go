package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool"
	"github.com/jackc/pgx/v5"
)

// webHarness is the executor harness wired to stub Tavily/Jina servers, so
// web_exec runs hit real adapter HTTP paths with no sandbox involved.
func webHarness(t *testing.T, searchBody, fetchBody string) *harness {
	t.Helper()
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(searchBody))
	}))
	t.Cleanup(search.Close)
	fetch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fetchBody))
	}))
	t.Cleanup(fetch.Close)

	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{
		TavilyAPIKey:     "tvly-test",
		WebSearchBaseURL: search.URL,
		WebFetchBaseURL:  fetch.URL,
	})
	h.prov = prov
	return h
}

// suspendWeb mimics the brain suspending a turn that carries a web call: the
// agent.tool_use intents and one web_exec item, one transaction.
func (h *harness) suspendWeb(t *testing.T, uses ...string) []domain.Event {
	t.Helper()
	var evs []events.NewEvent
	for _, u := range uses {
		evs = append(evs, events.NewEvent{Type: domain.EventAgentToolUse, Payload: json.RawMessage(u)})
	}
	out, err := h.log.AppendWith(context.Background(), h.sid, evs, events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.queue.Enqueue(ctx, tx, h.envID, h.sid, queue.WebExec)
			return err
		},
	})
	if err != nil {
		t.Fatalf("suspendWeb: %v", err)
	}
	return out
}

func searchUse(query string) string {
	b, _ := json.Marshal(map[string]any{"name": "web_search", "input": map[string]string{"query": query}})
	return string(b)
}

func fetchUse(url string) string {
	b, _ := json.Marshal(map[string]any{"name": "web_fetch", "input": map[string]string{"url": url}})
	return string(b)
}

// toolResults decodes the session's agent.tool_result bodies.
type resultBody struct {
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Content   []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Title     string `json:"title"`
		Source    string `json:"source"`
		Citations *struct {
			Enabled bool `json:"enabled"`
		} `json:"citations"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
}

func (h *harness) toolResults(t *testing.T) []resultBody {
	t.Helper()
	evs := h.types(t, "agent.tool_result")
	out := make([]resultBody, len(evs))
	for i, ev := range evs {
		if err := json.Unmarshal(ev.Body, &out[i]); err != nil {
			t.Fatalf("result %s: %v", ev.ID, err)
		}
	}
	return out
}

func (h *harness) stepOnce(t *testing.T) {
	t.Helper()
	worked, err := h.exec.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !worked {
		t.Fatal("step found no work")
	}
}

func TestWebSearchAnswersWithSearchResultBlocks(t *testing.T) {
	h := webHarness(t, `{"results":[
		{"title":"Go docs","url":"https://go.dev/doc/","content":"How to write Go."},
		{"title":"Spec","url":"https://go.dev/ref/spec","content":"The reference."}]}`, "")
	uses := h.suspendWeb(t, searchUse("golang docs"))

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	r := results[0]
	if r.ToolUseID != uses[0].ID.String() || r.IsError {
		t.Errorf("result = %+v, want a non-error answer to the web_search use", r)
	}
	if len(r.Content) != 2 || r.Content[0].Type != "search_result" {
		t.Fatalf("content = %+v, want two search_result blocks", r.Content)
	}
	first := r.Content[0]
	if first.Title != "Go docs" || first.Source != "https://go.dev/doc/" {
		t.Errorf("hit = %+v, want the Tavily hit's title and URL", first)
	}
	if len(first.Content) != 1 || first.Content[0].Type != "text" || first.Content[0].Text != "How to write Go." {
		t.Errorf("hit content = %+v, want one text block with the snippet", first.Content)
	}
	if first.Citations == nil || first.Citations.Enabled {
		t.Errorf("citations = %+v, want present with enabled false", first.Citations)
	}

	// No sandbox was provisioned; the turn resumes on a fresh model_turn.
	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0 — web tools never touch a sandbox", h.prov.provisions)
	}
	if n := h.liveOf(t, queue.WebExec); n != 0 {
		t.Errorf("live web_exec = %d, want 0 (completed)", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}

func TestWebSearchNoHitsAnswersWithTextBlock(t *testing.T) {
	h := webHarness(t, `{"results":[]}`, "")
	h.suspendWeb(t, searchUse("no such thing"))

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one non-error result", results)
	}
	// A text block saying so, not an empty content array: the search-results
	// docs prescribe a plain text outcome for an empty search, and an empty
	// array is indistinguishable from a tool that returned nothing at all.
	if len(results[0].Content) != 1 || results[0].Content[0].Type != "text" || results[0].Content[0].Text != "No results found." {
		t.Errorf("content = %+v, want one 'No results found.' text block", results[0].Content)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}

// The whole search answer honors the per-tool-call log budget: snippets are
// included whole while they fit; a hit past the budget keeps title and URL
// with empty content. Backend strings are NUL-sanitized (jsonb cannot store
// one — a faulted append would reclaim-loop), a hit without a URL is dropped,
// and an empty title falls back to the URL (the inbound validator reads both
// as required non-empty).
func TestWebSearchBoundsAndNormalizesTheAnswer(t *testing.T) {
	big := strings.Repeat("a", 60<<10)
	hits, _ := json.Marshal(map[string]any{"results": []map[string]string{
		{"title": "first", "url": "https://a.example/", "content": big},
		{"title": "second", "url": "https://b.example/", "content": big},
		{"title": "third\x00third", "url": "https://c.example/\x00", "content": "sni\x00ppet"},
		{"title": "", "url": "https://d.example/", "content": "snippet"},
		{"title": "no-url", "url": "", "content": "dropped"},
	}})
	h := webHarness(t, string(hits), "")
	h.suspendWeb(t, searchUse("q"))

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one non-error result", results)
	}
	blocks := results[0].Content
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4 (the URL-less hit dropped)", len(blocks))
	}
	var total int
	for _, b := range blocks {
		for _, c := range b.Content {
			total += len(c.Text)
			if strings.Contains(c.Text, "\x00") {
				t.Errorf("snippet carries a NUL byte")
			}
		}
	}
	if total > toolset.MaxOutputBytes {
		t.Errorf("total snippet bytes = %d, want <= the %d log budget", total, toolset.MaxOutputBytes)
	}
	// Only the first 60KiB snippet fits — two of them exceed the 100KiB
	// budget — so the second hit keeps its title and URL with no content.
	if len(blocks[0].Content) != 1 || len(blocks[1].Content) != 0 {
		t.Errorf("budget split = %d/%d content blocks, want 1/0", len(blocks[0].Content), len(blocks[1].Content))
	}
	if blocks[1].Title != "second" || blocks[1].Source != "https://b.example/" {
		t.Errorf("over-budget hit lost its identity: %+v", blocks[1])
	}
	if blocks[2].Title != "thirdthird" || blocks[2].Source != "https://c.example/" {
		t.Errorf("NUL sanitation: %+v", blocks[2])
	}
	if blocks[3].Title != "https://d.example/" {
		t.Errorf("empty title did not fall back to the URL: %+v", blocks[3])
	}
}

func TestWebFetchAnswersWithTextBlock(t *testing.T) {
	h := webHarness(t, "", "# The Page\n\nBody text.")
	uses := h.suspendWeb(t, fetchUse("https://example.com/page"))

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	r := results[0]
	if r.ToolUseID != uses[0].ID.String() || r.IsError {
		t.Errorf("result = %+v, want a non-error answer", r)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" || r.Content[0].Text != "# The Page\n\nBody text." {
		t.Errorf("content = %+v, want one text block with the page markdown", r.Content)
	}
	if h.prov.provisions != 0 {
		t.Errorf("provisions = %d, want 0", h.prov.provisions)
	}
}

// A mixed turn: the web call is answered by this web_exec pass, and the
// sandbox call rides a chained tool_exec — never the same item, so a BYOC
// worker polling tool_exec can never see an unanswered web call.
func TestMixedTurnChainsToolExecAfterWebCalls(t *testing.T) {
	h := webHarness(t, `{"results":[]}`, "")
	h.suspendWeb(t, searchUse("q"), writeUse("out.txt", "hello"))

	h.stepOnce(t)

	// The web call is answered; the sandbox call is not — its tool_exec is.
	if results := h.toolResults(t); len(results) != 1 {
		t.Fatalf("results after web pass = %d, want 1 (the web call only)", len(results))
	}
	if h.prov.provisions != 0 {
		t.Errorf("provisions after web pass = %d, want 0", h.prov.provisions)
	}
	if n := h.liveOf(t, queue.WebExec); n != 0 {
		t.Errorf("live web_exec = %d, want 0 (completed)", n)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 1 {
		t.Fatalf("live tool_exec = %d, want 1 (chained for the sandbox call)", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0 — the sandbox call is still unanswered", n)
	}

	// The chained pass answers the sandbox call and only then wakes the brain.
	h.stepOnce(t)
	if results := h.toolResults(t); len(results) != 2 {
		t.Fatalf("results after sandbox pass = %d, want 2", len(results))
	}
	if h.prov.provisions != 1 {
		t.Errorf("provisions after sandbox pass = %d, want 1", h.prov.provisions)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}

// An outstanding MCP call takes the web pass's chain ahead of the sandbox
// call: a tool_exec is the one kind a BYOC worker claims, and a worker has no
// surface to answer an MCP call with, so the same hold-back that keeps a
// tool_exec behind a web call keeps it behind this one.
func TestWebPassChainsMCPAheadOfTheSandbox(t *testing.T) {
	h := webHarness(t, `{"results":[]}`, "")
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.suspendWeb(t, searchUse("q"), writeUse("out.txt", "hello"))

	h.stepOnce(t)

	if n := h.liveOf(t, queue.WebExec); n != 0 {
		t.Errorf("live web_exec = %d, want 0 (completed)", n)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 1 {
		t.Fatalf("live mcp_exec = %d, want 1 (chained ahead of the sandbox call)", n)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 — the MCP pass chains it in turn", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0 — two calls are still unanswered", n)
	}
}

func TestWebSearchUnconfiguredAnswersIsError(t *testing.T) {
	// No TavilyAPIKey: the searcher is unconfigured. The call still gets an
	// answer — an is_error naming the missing variable — so the session
	// continues and the misconfiguration is visible, instead of the item
	// reclaim-looping or the tool silently vanishing.
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{})
	h.prov = prov
	h.suspendWeb(t, searchUse("q"))

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one is_error result", results)
	}
	var raw struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	evs := h.types(t, "agent.tool_result")
	_ = json.Unmarshal(evs[0].Body, &raw)
	if len(raw.Content) != 1 || !strings.Contains(raw.Content[0].Text, "TAVILY_API_KEY") {
		t.Errorf("error text = %+v, want it to name TAVILY_API_KEY", raw.Content)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 — an answered error still resumes the turn", n)
	}
}

func TestWebToolBadInputAnswersIsError(t *testing.T) {
	h := webHarness(t, `{"results":[]}`, "page")
	h.suspendWeb(t, `{"name":"web_fetch","input":{}}`, `{"name":"web_search","input":{"query":"   "}}`,
		`{"name":"web_fetch","input":{"url":"file:///etc/passwd"}}`)

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	for i, r := range results {
		if !r.IsError {
			t.Errorf("result %d = %+v, want is_error for the bad argument", i, r)
		}
	}
	// End-to-end shape only; the executor-seam check itself is pinned below
	// the adapters by TestWebFetchRejectsNonHTTPSchemesBeforeTheFetch (the
	// jina adapter rejects the same schemes, so this answer alone cannot
	// tell the two guards apart).
	if text := results[2].Content[0].Text; !strings.Contains(text, "http or https") {
		t.Errorf("file:// answer = %q, want it to name the http-or-https requirement", text)
	}
}

// stubSearcher and recordingFetcher drive runWebTool directly — below the
// adapters — so behavior the adapters would mask stays pinned: the Tavily hit
// cap truncates a long result list before the executor ever sees it, and the
// jina adapter re-validates URL schemes.
type stubSearcher struct{ hits []webtool.SearchResult }

func (s stubSearcher) Search(context.Context, string) ([]webtool.SearchResult, error) {
	return s.hits, nil
}

type recordingFetcher struct {
	calls   int
	lastURL string
}

func (f *recordingFetcher) Fetch(_ context.Context, url string) (webtool.FetchResult, error) {
	f.calls++
	f.lastURL = url
	return webtool.FetchResult{Content: "page"}, nil
}

func TestWebSearchDropsAHitWhoseMetadataBustsTheBudget(t *testing.T) {
	e := &Executor{searcher: stubSearcher{hits: []webtool.SearchResult{
		{Title: strings.Repeat("t", toolset.MaxOutputBytes), URL: "https://big.example/", Content: "x"},
		{Title: "small", URL: "https://small.example/", Content: "snippet"},
	}}}

	res := e.runWebTool(context.Background(), toolUse{name: "web_search", input: json.RawMessage(`{"query":"q"}`)})

	if res.IsError {
		t.Fatalf("result = %+v, want a non-error answer", res)
	}
	if len(res.SearchResults) != 1 || res.SearchResults[0].Source != "https://small.example/" {
		t.Errorf("blocks = %+v, want only the small hit — title and source charge the budget too", res.SearchResults)
	}
}

func TestWebFetchRejectsNonHTTPSchemesBeforeTheFetch(t *testing.T) {
	f := &recordingFetcher{}
	e := &Executor{fetcher: f}

	res := e.runWebTool(context.Background(), toolUse{name: "web_fetch", input: json.RawMessage(`{"url":"file:///etc/passwd"}`)})

	if !res.IsError || !strings.Contains(res.Content, "http or https") {
		t.Fatalf("result = %+v, want is_error naming the http-or-https requirement", res)
	}
	if f.calls != 0 {
		t.Errorf("fetcher calls = %d, want 0 — the executor seam rejects the scheme before the fetch", f.calls)
	}
}

func TestWebFetchHandsTheAdapterTheTrimmedURL(t *testing.T) {
	f := &recordingFetcher{}
	e := &Executor{fetcher: f}

	res := e.runWebTool(context.Background(), toolUse{name: "web_fetch", input: json.RawMessage(`{"url":"  https://example.com/page  "}`)})

	if res.IsError {
		t.Fatalf("result = %+v, want a non-error answer", res)
	}
	if f.lastURL != "https://example.com/page" {
		t.Errorf("fetched URL = %q, want the trimmed input", f.lastURL)
	}
}

func TestWebBackendErrorAnswersIsError(t *testing.T) {
	// The body carries a NUL: it lands in the backend error's excerpt, which
	// rides fail()'s error text — a path that never passes Runner.dispatch, so
	// only the executor's result-event boundary stands between it and jsonb.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret-internal\x00detail", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{TavilyAPIKey: "tvly-k", WebSearchBaseURL: srv.URL})
	h.prov = prov
	h.suspendWeb(t, searchUse("q"))

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one is_error result", results)
	}
	if text := results[0].Content[0].Text; strings.IndexByte(text, 0) >= 0 {
		t.Errorf("error text carries a NUL byte: %q", text)
	}
	if n := h.liveOf(t, queue.WebExec); n != 0 {
		t.Errorf("live web_exec = %d, want 0 — a backend error is the model's, not a reclaim loop", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}

// A tool_exec pass never answers a web call — the stray-item guard: it runs
// the sandbox tools it finds (none here), leaves the web call for the web
// driver instead of feeding it to the Runner's unknown-tool arm, and HEALS
// the stray shape by chaining a web_exec — no current enqueue site produces a
// tool_exec with a web call outstanding, but completing the item over one
// would otherwise strand the session permanently.
func TestToolExecPassHealsAStrayWebCall(t *testing.T) {
	h := webHarness(t, `{"results":[{"title":"t","url":"https://a.example/","content":"c"}]}`, "")
	h.suspend(t, searchUse("q")) // enqueues tool_exec, not web_exec — the stray shape

	// Claim the tool_exec directly so this exercises exactly the sandbox pass.
	item, err := h.queue.Claim(context.Background(), queue.ToolExec, h.exec.cfg.LeaseTTL)
	if err != nil || item == nil {
		t.Fatalf("claim tool_exec: %+v %v", item, err)
	}
	if err := h.exec.process(context.Background(), item); err != nil {
		t.Fatalf("process: %v", err)
	}

	if results := h.toolResults(t); len(results) != 0 {
		t.Fatalf("results = %+v, want none — the sandbox pass must not answer web calls", results)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 (completed)", n)
	}
	if n := h.liveOf(t, queue.WebExec); n != 1 {
		t.Fatalf("live web_exec = %d, want 1 — the heal chain", n)
	}

	// The healed chain runs and the session continues.
	h.stepOnce(t)
	if results := h.toolResults(t); len(results) != 1 {
		t.Fatalf("results after heal = %d, want the web answer", len(results))
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}

// An executor with neither JINA_API_KEY nor WEBFETCH_BASE_URL leaves
// web_fetch unconfigured — an is_error naming the variables, never a silent
// default egress of model-chosen URLs to the public reader endpoint.
func TestWebFetchUnconfiguredAnswersIsError(t *testing.T) {
	prov := &fakeProvider{sb: &fakeSandbox{}}
	h := newHarnessWith(t, prov, Config{})
	h.prov = prov
	h.suspendWeb(t, fetchUse("https://example.com/"))

	h.stepOnce(t)

	results := h.toolResults(t)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one is_error result", results)
	}
	evs := h.types(t, "agent.tool_result")
	var raw struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(evs[0].Body, &raw)
	if len(raw.Content) != 1 || !strings.Contains(raw.Content[0].Text, "JINA_API_KEY") {
		t.Errorf("error text = %+v, want it to name JINA_API_KEY", raw.Content)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}

func TestWebFetchOutsideAllowedDomainsAnswersIsError(t *testing.T) {
	f := &recordingFetcher{}
	e := &Executor{fetcher: f, webAllowed: egress.NewHostSet([]string{"example.com", "*.example.com"})}

	denied := e.runWebTool(context.Background(), toolUse{name: "web_fetch", input: json.RawMessage(`{"url":"https://evil.test/x"}`)})
	if !denied.IsError || !strings.Contains(denied.Content, "allowed domains") {
		t.Fatalf("denied result = %+v, want is_error naming the allowlist", denied)
	}
	if f.calls != 0 {
		t.Errorf("fetcher calls = %d, want 0 — a denied host must never be fetched", f.calls)
	}

	allowed := e.runWebTool(context.Background(), toolUse{name: "web_fetch", input: json.RawMessage(`{"url":"https://docs.example.com/page"}`)})
	if allowed.IsError || f.calls != 1 {
		t.Errorf("allowed result = %+v (fetcher calls %d), want a fetch of the in-list host", allowed, f.calls)
	}
}

func TestWebSearchFiltersHitsOutsideAllowedDomains(t *testing.T) {
	e := &Executor{
		searcher: stubSearcher{hits: []webtool.SearchResult{
			{Title: "in", URL: "https://docs.example.com/a", Content: "kept"},
			{Title: "out", URL: "https://evil.test/b", Content: "dropped"},
		}},
		webAllowed: egress.NewHostSet([]string{"*.example.com"}),
	}

	res := e.runWebTool(context.Background(), toolUse{name: "web_search", input: json.RawMessage(`{"query":"q"}`)})

	if res.IsError {
		t.Fatalf("result = %+v, want a non-error answer", res)
	}
	if len(res.SearchResults) != 1 || res.SearchResults[0].Source != "https://docs.example.com/a" {
		t.Errorf("blocks = %+v, want only the in-list hit", res.SearchResults)
	}

	// Every hit outside the list answers as the documented zero-hit shape, so
	// the model reads an outcome, not an error.
	e.searcher = stubSearcher{hits: []webtool.SearchResult{{Title: "out", URL: "https://evil.test/b", Content: "x"}}}
	res = e.runWebTool(context.Background(), toolUse{name: "web_search", input: json.RawMessage(`{"query":"q"}`)})
	if res.IsError || res.Content != "No results found." {
		t.Errorf("all-filtered result = %+v, want the zero-hit text answer", res)
	}
}

// New builds the allowlist only from a non-empty config: an unset
// WEBTOOL_ALLOWED_DOMAINS must stay a nil set (unrestricted), never invert
// into HostSet's own nil-set deny-all semantics.
func TestNewBuildsTheAllowlistOnlyWhenConfigured(t *testing.T) {
	if e := New(nil, nil, nil, nil, nil, nil, Config{}); e.webAllowed != nil {
		t.Error("empty config built an allowlist — unset must mean unrestricted")
	}
	e := New(nil, nil, nil, nil, nil, nil, Config{WebAllowedDomains: []string{"example.com"}})
	if e.webAllowed == nil || !e.webAllowed.Match("example.com") || e.webAllowed.Match("evil.test") {
		t.Errorf("configured allowlist = %+v, want example.com admitted and evil.test refused", e.webAllowed)
	}
}
