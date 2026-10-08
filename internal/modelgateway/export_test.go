package modelgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// OpenedKeys returns the ids of the credentials whose keys the handler holds
// opened, in order.
func OpenedKeys(h http.Handler) []string {
	g := h.(*handler)
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.opened))
	for id := range g.opened {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Open is the handler's open, as a request routed on c would call it.
func Open(h http.Handler, ctx context.Context, c store.Credential) ([]byte, error) {
	return h.(*handler).open(ctx, c)
}

// Backoff is the handler's wait before retry n.
func Backoff(h http.Handler, ctx context.Context, n int) bool { return h.(*handler).backoff(ctx, n) }

// ThinkingRefusal is thinkingRefusal.
func ThinkingRefusal(body []byte) bool { return thinkingRefusal(body) }

// EscapesName is escapesName.
func EscapesName(b []byte) bool { return escapesName(b) }

// SetWriteStall shortens the bound on a write to the caller for one test, and
// returns its restore.
func SetWriteStall(d time.Duration) func() {
	old := writeStall.Swap(int64(d))
	return func() { writeStall.Store(old) }
}

// SetMaxResponseBody lowers the bound on an upstream's body for one test, and
// returns its restore.
func SetMaxResponseBody(n int) func() {
	old := maxResponseBody
	maxResponseBody = n
	return func() { maxResponseBody = old }
}

// SpanName is spanName.
func SpanName(r *http.Request) string { return spanName(r) }

// ChatUsageOf is chatUsageOf.
func ChatUsageOf(raw []byte) *store.Tokens { return chatUsageOf(raw) }

// WriteToCaller writes b to w as an answer relayed under guard, from a
// provider with the stall budget stall, would be, and reports whether the
// caller is gone.
func WriteToCaller(w http.ResponseWriter, stall time.Duration, guard *provider.StallGuard, b []byte) bool {
	cw := newCallerWriter(w, &catalog.Attempt{Provider: store.Provider{StallTimeout: stall}}, guard, false)
	cw.write(b)
	return cw.gone
}

// VectorUsageOf is vectorUsageOf.
func VectorUsageOf(raw []byte) *store.Tokens { return vectorUsageOf(raw) }

// MaxAnswerUsage is the longest usage an answerRewriter keeps.
const MaxAnswerUsage = maxUsage

// RewriteAnswer is what an answerRewriter for alias makes of chunks, fed one
// by one, and the usage it kept.
func RewriteAnswer(alias string, chunks ...[]byte) ([]byte, json.RawMessage) {
	rw := &answerRewriter{alias: encodeJSON(alias)}
	var out []byte
	for _, c := range chunks {
		out = append(out, rw.rewrite(c)...)
	}
	return out, rw.usage
}
