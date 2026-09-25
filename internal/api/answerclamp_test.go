package api_test

import (
	"bytes"
	"context"
	"net/http/httptest"
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

// total is how many statements were recorded.
func (l *statementLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sqls)
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
// list slot, and re-reads the stamps it echoes, in statements whose number does
// not grow with the answers: the session's row lock is held throughout, and a
// request body can carry thousands of answers. So the send is measured twice,
// with one answer and with many, and each answer past the first may cost only
// the three statements it needs today whatever the clamp does — its routing
// read, its validation read and the settlement's stamp. A clamp or a re-read
// done once per answer, however it is spelled, adds to that and fails here.
func TestManyAnswersAreClampedInOneStatement(t *testing.T) {
	var log statementLog
	s := newTracedTestServer(t, &log)
	// send answers n fresh calls on a new session and returns the statements
	// the send ran, what it echoed and where the session's log stood before.
	send := func(n int) (int, []map[string]any, string, int64) {
		sid := eventsFixture(t, s)
		answers := make([]map[string]any, n)
		for i := range answers {
			useID := appendOn(t, s, sid, "", false, domain.EventAgentCustomToolUse, `{"name":"decide","input":{}}`)
			answers[i] = customResult(useID)
		}
		seq := lastSeq(t, s, sid)
		log.reset()
		echo := sendEvents(t, s, sid, answers...)
		return log.total(), echo, sid, seq
	}
	const n, perAnswer = 40, 3
	one, _, _, _ := send(1)
	many, echo, sid, seq := send(n)

	if grew := many - one; grew > perAnswer*(n-1) {
		t.Errorf("%d answers ran %d statements and one ran %d: %d more per extra answer, want at most %d",
			n, many, one, grew/(n-1), perAnswer)
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
