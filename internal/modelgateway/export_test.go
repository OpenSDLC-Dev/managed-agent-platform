package modelgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

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

// SetPingEvery shortens how long a converted stream goes unwritten before a
// ping, for one test, and returns its restore.
func SetPingEvery(d time.Duration) func() {
	old := pingEvery.Swap(int64(d))
	return func() { pingEvery.Store(old) }
}

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

// WriteToCaller writes b to w as an answer relayed under guard would be, and
// reports whether the caller is gone.
func WriteToCaller(w http.ResponseWriter, guard *provider.StallGuard, b []byte) bool {
	cw := newCallerWriter(w, guard, false)
	cw.write(b)
	return cw.gone
}

// SetMaxVectorAnswer lowers the bound on an embeddings or rerank answer for
// one test, and returns its restore.
func SetMaxVectorAnswer(n int) func() {
	old := maxVectorAnswer
	maxVectorAnswer = n
	return func() { maxVectorAnswer = old }
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
		rw.rewrite(c, func(p []byte) { out = append(out, p...) })
	}
	return out, rw.usage
}

// RewrittenLength is the length of what an answerRewriter for alias makes of
// chunk, kept nowhere.
func RewrittenLength(alias string, chunk []byte) int {
	n := 0
	(&answerRewriter{alias: encodeJSON(alias)}).rewrite(chunk, func(p []byte) { n += len(p) })
	return n
}
