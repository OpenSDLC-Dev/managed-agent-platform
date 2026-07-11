package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// toolUse is one platform tool call the executor must run: the tool-use event's
// id (which scopes the bash shell's per-call state and which the result
// references), the tool name, and its input.
type toolUse struct {
	id    domain.ID
	name  string
	input json.RawMessage
}

// unansweredToolUses returns the session's agent.tool_use events that still
// lack a result, oldest first — the work this item must run. It reads the
// committed log (custom tool uses are client-executed and never appear as
// agent.tool_use; mcp_tool_use waits for the MCP client), diffing the tool-use
// events against the results already on the log so a reclaim re-runs only what
// is still outstanding.
func (e *Executor) unansweredToolUses(ctx context.Context, sid domain.ID) ([]toolUse, error) {
	uses, err := e.log.List(ctx, sid, events.ListQuery{Types: []string{string(domain.EventAgentToolUse)}})
	if err != nil {
		return nil, fmt.Errorf("list tool uses: %w", err)
	}
	results, err := e.log.List(ctx, sid, events.ListQuery{Types: []string{string(domain.EventAgentToolResult)}})
	if err != nil {
		return nil, fmt.Errorf("list tool results: %w", err)
	}
	answered := make(map[string]bool, len(results))
	for _, r := range results {
		var ref struct {
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(r.Body, &ref); err != nil {
			return nil, fmt.Errorf("tool result %s: %w", r.ID, err)
		}
		answered[ref.ToolUseID] = true
	}

	var out []toolUse
	for _, u := range uses {
		if answered[u.ID.String()] {
			continue
		}
		var body struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(u.Body, &body); err != nil {
			return nil, fmt.Errorf("tool use %s: %w", u.ID, err)
		}
		out = append(out, toolUse{id: u.ID, name: body.Name, input: body.Input})
	}
	return out, nil
}

// leaseKeeper renews a claimed item's lease on a timer while its tools run, so a
// long tool call cannot let the lease lapse and hand the item to a second
// executor mid-run. It mirrors the brain's: each renewal is bounded by the
// lease it is racing, so a stalled database cannot hang the item behind an
// unreturnable Extend, and losing the lease cancels the work context.
type leaseKeeper struct {
	cancel context.CancelFunc
	quit   chan struct{}
	done   chan struct{}
	failed error
}

func (e *Executor) keepLease(ctx context.Context, item *queue.Item) (context.Context, *leaseKeeper) {
	kctx, cancel := context.WithCancel(ctx)
	k := &leaseKeeper{cancel: cancel, quit: make(chan struct{}), done: make(chan struct{})}
	ttl := e.cfg.LeaseTTL
	go func() {
		defer close(k.done)
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-k.quit:
				return
			case <-kctx.Done():
				return
			case <-t.C:
				ectx, ecancel := context.WithTimeout(kctx, ttl-ttl/3)
				err := e.queue.Extend(ectx, item, ttl)
				ecancel()
				if err != nil {
					k.failed = err
					k.cancel()
					return
				}
			}
		}
	}()
	return kctx, k
}

// close stops the keeper and reports the first extension failure. The goroutine
// has exited when close returns, so the item's lease value is stable again for
// the settling append to use as its ownership proof.
func (k *leaseKeeper) close() error {
	close(k.quit)
	<-k.done
	k.cancel()
	return k.failed
}
