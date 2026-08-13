package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
)

// The web driver: web_exec items run web_fetch/web_search in this process —
// no sandbox Provision — for cloud AND self_hosted sessions alike
// (docs/plan/15_web-tools.md). The brain enqueues web_exec instead of
// tool_exec whenever a turn carries a web call, because a tool_exec is
// visible to a BYOC worker whose official toolset implements only the six
// sandbox tools; this driver answers the web calls and only then chains the
// tool_exec for whatever sandbox work rode the same turn.
//
// The session's networking policy deliberately does not constrain these calls
// (the reference documents that networking "does not affect the allowed
// domains for the web_search or web_fetch tools"): web egress originates
// here, in the executor's process, structurally outside the per-session gate.

// processWeb runs one web_exec item to completion. It mirrors process — the
// consumer span, the dead-session drain, the lease keeper, the one-commit
// settlement — minus everything sandbox.
func (e *Executor) processWeb(ctx context.Context, item *queue.Item) (err error) {
	ctx, span := consumerSpan(ctx, item, "web_exec")
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			e.report(ctx, item, err)
		}
		span.End()
	}()

	// Drain work for a session that is no longer live, exactly as process
	// does; the loaded state (networking, skills, vaults) is sandbox business
	// and unused here.
	_, live, err := e.sessionForRun(ctx, item)
	if err != nil || !live {
		return err
	}

	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL)
	results, faultErr, runErr := e.runWebTools(kctx, item.SessionID)
	if kerr := keeper.Close(); kerr != nil {
		return fmt.Errorf("lease keeper: %w", kerr)
	}
	if runErr != nil {
		return runErr
	}
	// The only web fault is a dead context (lease lost, shutdown), and a dead
	// context cannot begin the commit transaction — so unlike process's
	// partial-commit fault path, an interrupted web run commits NOTHING and
	// the reclaim re-runs the calls, the same all-or-nothing a cancelled
	// model turn settles to.
	if faultErr != nil {
		return faultErr
	}

	// Commit the results, the follow-on work, and the item's fate together
	// under the session lock, mirroring process's settlement. Once every web
	// call is answered: an outstanding MCP call rides a chained mcp_exec, which
	// takes precedence over the sandbox built-ins for the reason it does
	// everywhere — the tool_exec that would otherwise come next is the one kind
	// a BYOC worker claims, and it has no surface to answer an MCP call with;
	// sandbox built-ins still unanswered ride a chained tool_exec (the second
	// half of the web-first hold-back); nothing platform or MCP outstanding but
	// custom calls means the client resumes the turn; a fully-answered set
	// wakes the brain.
	opts := events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			mcpPending, err := events.HasUnansweredMCPToolUse(ctx, tx, item.SessionID, nil)
			if err != nil {
				return err
			}
			if mcpPending {
				if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.MCPExec); err != nil {
					return err
				}
				return e.queue.Complete(ctx, tx, item)
			}
			platformPending, err := events.UnansweredPlatformToolNames(ctx, tx, item.SessionID, nil)
			if err != nil {
				return err
			}
			if len(platformPending) > 0 {
				if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ToolExec); err != nil {
					return err
				}
				return e.queue.Complete(ctx, tx, item)
			}
			anyPending, err := events.HasUnansweredToolUse(ctx, tx, item.SessionID, nil)
			if err != nil {
				return err
			}
			if !anyPending {
				if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ModelTurn); err != nil {
					return err
				}
			}
			return e.queue.Complete(ctx, tx, item)
		},
	}
	if _, err := e.log.AppendWith(ctx, item.SessionID, results, opts); err != nil {
		return fmt.Errorf("append web tool results: %w", err)
	}
	return nil
}

// runWebTools answers the session's unanswered web calls, oldest first,
// recording each through the same instrument as every sandbox tool. Unlike a
// sandbox run there is no backend to fault: an unreachable page, an HTTP
// error, a missing key are all the model's to read, so every call yields a
// result — except when the context dies mid-call (lease lost, shutdown),
// where the outcome is untrustworthy and the rest is left for the reclaim.
func (e *Executor) runWebTools(ctx context.Context, sid domain.ID) ([]events.NewEvent, error, error) {
	uses, err := e.unansweredToolUses(ctx, sid)
	if err != nil {
		return nil, nil, err
	}
	var results []events.NewEvent
	for _, u := range uses {
		if !toolset.IsWebTool(u.name) {
			continue
		}
		start := time.Now()
		res := e.runWebTool(ctx, u)
		toolset.RecordRun(ctx, u.name, time.Since(start), res, ctx.Err())
		if ctx.Err() != nil {
			return results, fmt.Errorf("tool %s (%s): %w", u.name, u.id, ctx.Err()), nil
		}
		ev, err := toolResultEvent(u.id, res)
		if err != nil {
			return results, nil, err
		}
		results = append(results, ev)
	}
	return results, nil, nil
}

// runWebTool answers one web call. Failures land as is_error results rather
// than faults: a permanently-bad URL or a dead backend would otherwise
// reclaim-loop the item forever, and the model can read the error and try
// something else — the same recovery contract as a sandbox tool's nonzero
// exit. The backends' errors already redact credentials (webtool.HTTPError).
func (e *Executor) runWebTool(ctx context.Context, u toolUse) toolset.Result {
	fail := func(msg string) toolset.Result { return toolset.Result{Content: msg, IsError: true} }
	switch u.name {
	case "web_search":
		if e.searcher == nil {
			return fail("web_search is not configured: the executor is missing TAVILY_API_KEY")
		}
		var in struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(u.input, &in); err != nil || strings.TrimSpace(in.Query) == "" {
			return fail(`web_search: input requires a non-empty "query" string`)
		}
		hits, err := e.searcher.Search(ctx, in.Query)
		if err != nil {
			return fail("web_search: " + err.Error())
		}
		// No hits answers as a text block, not an empty content array: the
		// public search-results docs prescribe exactly that ("return a plain
		// text block describing the outcome ... instead of raising an error"),
		// and an empty array is indistinguishable from a tool that returned
		// nothing at all.
		if len(hits) == 0 {
			return toolset.Result{Content: "No results found."}
		}
		// The whole answer honors the same event-log budget every sandbox tool
		// does (toolset.MaxOutputBytes is per tool CALL, not per block): a
		// hit's title and source charge the budget first — they are
		// backend-controlled too, and five oversized titles would bust the
		// cap as surely as one oversized snippet — then its snippet is
		// included only while it fits the remaining budget whole; a hit whose
		// snippet is past the budget keeps its title and URL — enough for
		// the model to web_fetch it — with an empty content array. Backend
		// strings are sanitized (a jsonb column cannot store a NUL byte, and a
		// faulted append would reclaim-loop the item forever re-fetching), a
		// hit without a source URL is dropped (nothing to cite or fetch), and
		// an empty title falls back to the URL — the platform's own inbound
		// validator reads both fields as required non-empty, and an
		// unsatisfiable block on the append-only log would wedge every replay.
		budget := toolset.MaxOutputBytes
		blocks := make([]domain.SearchResultBlock, 0, len(hits))
		for _, h := range hits {
			source := toolset.SanitizeText(h.URL)
			if source == "" {
				continue
			}
			// The operator's allowlist (#225) prunes hits like the other
			// normalizations do: a source outside it is dropped rather than
			// flagged, so the model never sees — and so never tries to fetch —
			// a domain the operator has fenced off.
			if e.webAllowed != nil {
				su, serr := url.Parse(source)
				if serr != nil || !e.webAllowed.Match(su.Hostname()) {
					continue
				}
			}
			title := toolset.SanitizeText(h.Title)
			if title == "" {
				title = source
			}
			if len(title)+len(source) > budget {
				continue
			}
			budget -= len(title) + len(source)
			content := []domain.ContentBlock{}
			if snippet := toolset.CapOutput(toolset.SanitizeText(h.Content)); snippet != "" && len(snippet) <= budget {
				content = append(content, domain.ContentBlock{Type: "text", Text: snippet})
				budget -= len(snippet)
			}
			blocks = append(blocks, domain.SearchResultBlock{
				Type:      "search_result",
				Citations: domain.SearchResultCitations{Enabled: false},
				Content:   content,
				Source:    source,
				Title:     title,
			})
		}
		if len(blocks) == 0 {
			return toolset.Result{Content: "No results found."}
		}
		return toolset.Result{SearchResults: blocks}
	case "web_fetch":
		if e.fetcher == nil {
			return fail("web_fetch is not configured: the executor is missing JINA_API_KEY (or a WEBFETCH_BASE_URL naming a keyless reader endpoint)")
		}
		var in struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(u.input, &in); err != nil || strings.TrimSpace(in.URL) == "" {
			return fail(`web_fetch: input requires a non-empty "url" string`)
		}
		// The web tools egress from this process, outside the per-session
		// gate, so the executor rejects non-http(s) schemes at its own seam —
		// defense in depth ahead of the adapter's identical check — before a
		// model-chosen URL (file://, gopher://, ...) reaches the reader
		// backend's network position.
		target := strings.TrimSpace(in.URL)
		parsed, perr := url.Parse(target)
		if perr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fail(`web_fetch: "url" must be an http or https URL`)
		}
		if e.webAllowed != nil && !e.webAllowed.Match(parsed.Hostname()) {
			return fail(fmt.Sprintf("web_fetch: host %q is outside the operator's allowed domains (WEBTOOL_ALLOWED_DOMAINS)", parsed.Hostname()))
		}
		page, err := e.fetcher.Fetch(ctx, target)
		if err != nil {
			return fail("web_fetch: " + err.Error())
		}
		// CapOutput is the event log's context budget, the same one every
		// sandbox tool honors. The backend's own transport cap (webtool
		// MaxContentBytes, a memory guard, page.Truncated) sits far above it,
		// so a transport-truncated page is always also log-capped here and the
		// model sees the truncation notice either way. Sanitized for the same
		// jsonb reason as the search strings above.
		return toolset.Result{Content: toolset.CapOutput(toolset.SanitizeText(page.Content))}
	}
	// Unreachable while IsWebTool and this switch agree; the check is what
	// keeps them agreeing.
	return fail(fmt.Sprintf("unknown web tool %q", u.name))
}
