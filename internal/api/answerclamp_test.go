package api_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// statementLog records the SQL of every statement a pool runs.
type statementLog struct {
	mu   sync.Mutex
	sqls []string
}

func (l *statementLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sqls = append(l.sqls, d.SQL)
	return ctx
}

func (l *statementLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (l *statementLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sqls = nil
}

// count is how many recorded statements contain fragment.
func (l *statementLog) count(fragment string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, sql := range l.sqls {
		if strings.Contains(sql, fragment) {
			n++
		}
	}
	return n
}

// newTracedTestServer is newTestServer with the handler's pool traced into
// log; fixtures written through s.pool are not.
func newTracedTestServer(t *testing.T, log *statementLog) *tserver {
	t.Helper()
	pool := newPoolWithKey(t)
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = log
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	blobs := blobtest.Mem()
	srv := httptest.NewServer(api.NewHandler(traced, blobs, cipher, nil))
	t.Cleanup(srv.Close)
	return &tserver{t: t, url: srv.URL, pool: pool, blobs: blobs}
}

// A send that answers many calls at once holds each answer's stamp inside its
// list slot in one statement over the batch, re-reading the stamps it echoes
// from that same statement: the session's row lock is held throughout, and a
// request body can carry thousands of answers, so the clamp may not cost a
// round trip (or a scan of the batch) per answer.
func TestManyAnswersAreClampedInOneStatement(t *testing.T) {
	var log statementLog
	s := newTracedTestServer(t, &log)
	sid := eventsFixture(t, s)
	const n = 40
	answers := make([]map[string]any, n)
	for i := range answers {
		useID := appendOn(t, s, sid, "", false, domain.EventAgentCustomToolUse, `{"name":"decide","input":{}}`)
		answers[i] = map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": useID,
			"content": []any{map[string]any{"type": "text", "text": "done"}}}
	}
	seq := lastSeq(t, s, sid)
	log.reset()

	echo := sendEvents(t, s, sid, answers...)

	if got := log.count("processed_at = LEAST(GREATEST("); got != 1 {
		t.Errorf("the answer clamp ran %d statements for %d answers, want 1", got, n)
	}
	if got := log.count("SELECT processed_at FROM events"); got != 0 {
		t.Errorf("%d statements re-read one answer each, want the clamp's own RETURNING", got)
	}
	_, list := s.do("GET", "/v1/sessions/"+sid+"/events?limit=1000", nil)
	stamps := map[any]any{}
	for _, ev := range listData(t, list) {
		stamps[ev["id"]] = ev["processed_at"]
	}
	if len(echo) != n {
		t.Fatalf("echoed %d events, want %d", len(echo), n)
	}
	for i, ev := range echo {
		if ev["processed_at"] == nil || ev["processed_at"] != stamps[ev["id"]] {
			t.Errorf("answer %d echoed processed_at %v, the log holds %v", i, ev["processed_at"], stamps[ev["id"]])
		}
	}
	stampsRunForward(t, s, sid, seq)
}
