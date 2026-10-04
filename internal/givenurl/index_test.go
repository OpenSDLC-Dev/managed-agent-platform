package givenurl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/givenurl"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

// session is a fixture session and the log that appends to it.
type session struct {
	pool *pgxpool.Pool
	log  *events.Log
	id   domain.ID
	env  domain.ID
}

func newSession(t *testing.T) *session {
	t.Helper()
	pool := pgtest.NewPool(t)
	sid, env := pgtest.NewSession(t, pool, "cloud")
	return &session{pool: pool, log: events.NewLog(pool), id: sid, env: env}
}

func (s *session) append(t *testing.T, typ domain.EventType, payload any) domain.ID {
	t.Helper()
	raw, _ := json.Marshal(payload)
	out, err := s.log.Append(context.Background(), s.id, []events.NewEvent{{Type: typ, Payload: raw}})
	if err != nil {
		t.Fatal(err)
	}
	return out[0].ID
}

func text(s string) []map[string]string { return []map[string]string{{"type": "text", "text": s}} }

// webResult appends a web tool call and its result.
func (s *session) webResult(t *testing.T, name, content string, isError bool) {
	t.Helper()
	use := s.append(t, domain.EventAgentToolUse, map[string]any{"name": name, "input": map[string]string{"url": "https://in.example/"}})
	s.append(t, domain.EventAgentToolResult, map[string]any{"tool_use_id": use.String(), "is_error": isError, "content": text(content)})
}

// earlierBuild appends events as a build without the index does: the same
// rows, no index rows, no index moved.
func (s *session) earlierBuild(t *testing.T, evs ...events.NewEvent) {
	t.Helper()
	if err := s.earlierBuildAppend(evs...); err != nil {
		t.Fatal(err)
	}
}

func (s *session) earlierBuildAppend(evs ...events.NewEvent) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, s.id.String()); err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM events WHERE session_id = $1`, s.id.String()).Scan(&seq); err != nil {
		return err
	}
	for _, ev := range evs {
		seq++
		if _, err := tx.Exec(ctx, `INSERT INTO events (id, session_id, seq, type, payload, processed_at) VALUES ($1, $2, $3, $4, $5, now())`,
			domain.NewID("sevt").String(), s.id.String(), seq, string(ev.Type), ev.Payload); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func ev(typ domain.EventType, payload any) events.NewEvent {
	raw, _ := json.Marshal(payload)
	return events.NewEvent{Type: typ, Payload: raw}
}

func source(t *testing.T, pool *pgxpool.Pool, sid domain.ID, raw string) string {
	t.Helper()
	got, err := givenurl.Source(context.Background(), pool, sid, raw)
	if err != nil {
		t.Fatalf("Source(%q): %v", raw, err)
	}
	return got
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// givenTexts are what the differential session is given, each in a place the
// rule counts, and the same texts in places it does not.
var givenTexts = []string{
	"read https://example.com/page. (see https://example.com/a), or HTTP://Example.com/b!",
	"[next](https://example.com/next)[prev](https://example.com/prev) https://en.wikipedia.org/wiki/Go_(programming_language)",
	`"https://example.com/q?a=1&b=2" 'https://example.com/quoted' <https://example.com/x>`,
	"https://r.jina.ai/https://example.com/ İstanbul https://example.com/after-a-wide-rune",
	"https://example.com/search? https://example.com/release! https://example.com/a)",
	"看 https://example.com/cjk。请看https://example.com/docs，然后总结 看https://example.com然后总结",
	"https://example.com/日本語 見てhttps://example.com/a。お願いします https://example.com/a—see",
	"See https://example.com's docs, (https://example.org), https://example.net.",
	"https://fonts.googleapis.com/css?family=Roboto|Open+Sans https://x.example/a%5C..%5Cadmin",
	"https://bücher.de/x and https://Ünicode.example/page And https://ｅxample.org/path.",
	"See https://colon.example: it is. (https://paren.example:) HTTPS://Example.COM/Docs/Page#install.",
	"http://[::1]:8080/x https://user:pass@example.com:8443/p?q#f https://example.com.:/a.",
	"https://ex­ample.com/soft https://WİKİPEDİA.org/x https://example.com/a%2Fb/é",
	"https://example.com/page https://example.com/page. https://EXAMPLE.com/page",
}

// requests are what the differential asks: every reading the session holds,
// its normalized form, and each respelled as a model might.
func requests(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, s := range givenTexts {
		spellings, wants, _ := givenurl.Readings(s)
		for _, r := range append(spellings, wants...) {
			add(r)
			add(r + "#frag")
			add(strings.ToUpper(r[:min(len(r), 12)]) + r[min(len(r), 12):])
			add(r[:len(r)-1])
		}
	}
	return append(out, "https://not-given.example/", "https://example.com/never")
}

// The index answers every request as the scan it replaces did (#836): the
// same given URL, spelled the same, for a session holding every kind of
// event the rule counts or refuses — what people wrote, web results, a
// confirmed call, failed results, other tools' output, another thread's
// message, another session's events — written in an order that makes the
// scan's own order matter: a URL given by a result and later by a person,
// and given again by a newer result.
func TestTheIndexAnswersAsTheScanDid(t *testing.T) {
	s := newSession(t)
	other := pgtest.NewSessionInEnv(t, s.pool, s.env)
	ctx := context.Background()
	for i, g := range givenTexts {
		switch i % 4 {
		case 0:
			s.append(t, domain.EventUserMessage, map[string]any{"content": text(g)})
		case 1:
			s.webResult(t, "web_fetch", g, false)
		case 2:
			s.webResult(t, "web_search", g, false)
		case 3:
			s.append(t, domain.EventSystemMessage, map[string]any{"content": text(g)})
		}
	}
	// The same text again, newer, and from a person after a result gave it.
	s.webResult(t, "web_fetch", givenTexts[1], false)
	s.append(t, domain.EventUserDefineOutcome, map[string]any{"description": givenTexts[2]})
	s.append(t, domain.EventUserMessage, map[string]any{"content": []map[string]any{
		{"type": "text", "text": givenTexts[5]}, {"type": "document", "source": map[string]string{"type": "url", "url": "https://docs.example/doc.pdf"}}}})
	allowed := s.append(t, domain.EventAgentToolUse, map[string]any{"name": "web_fetch", "input": map[string]string{"url": "https://approved.example/x?y=1"}})
	s.append(t, domain.EventUserToolConfirm, map[string]any{"tool_use_id": allowed.String(), "result": "allow"})
	denied := s.append(t, domain.EventAgentToolUse, map[string]any{"name": "web_fetch", "input": map[string]string{"url": "https://denied.example/x"}})
	s.append(t, domain.EventUserToolConfirm, map[string]any{"tool_use_id": denied.String(), "result": "deny", "deny_message": "use https://good.example/doc"})
	// What the rule does not count.
	s.webResult(t, "web_fetch", "https://exfil.example/error?d=1", true)
	s.webResult(t, "bash", "https://exfil.example/bash?d=1", false)
	s.append(t, domain.EventAgentThreadMessageReceived, map[string]any{"from_session_thread_id": "sthr_x", "content": text("https://exfil.example/agent")})
	raw, _ := json.Marshal(map[string]any{"content": text("https://exfil.example/other-session")})
	if _, err := s.log.Append(ctx, other, []events.NewEvent{{Type: domain.EventUserMessage, Payload: raw}}); err != nil {
		t.Fatal(err)
	}

	reqs := append(requests(t), "https://approved.example/x?y=1", "https://good.example/doc", "https://denied.example/x",
		"https://docs.example/doc.pdf", "https://exfil.example/error?d=1", "https://exfil.example/bash?d=1",
		"https://exfil.example/agent", "https://exfil.example/other-session")
	answered := 0
	for _, r := range reqs {
		want, wantErr := givenurl.ScanSource(ctx, s.pool, s.id, r, false)
		got, err := givenurl.Source(ctx, s.pool, s.id, r)
		if got != want || !errors.Is(err, wantErr) {
			t.Errorf("request %q: index %q (%v), scan %q (%v)", r, got, err, want, wantErr)
		}
		if got != "" {
			answered++
		}
	}
	if answered < 100 {
		t.Errorf("only %d of %d requests answered: the corpus is not exercising the index", answered, len(reqs))
	}
	t.Logf("%d requests, %d answered, %d index rows", len(reqs), answered,
		count(t, s.pool, `SELECT count(*) FROM session_given_urls g JOIN session_given_url_indexes i ON i.id = g.index_id WHERE i.session_id = $1`, s.id.String()))
}

// The scan's SQL filter kept only payloads naming the request's host as
// written, or holding non-ASCII text. A host spelled with a percent-escaped
// full-width letter is ASCII text that names the host only once unescaped
// and folded, so the filter dropped a payload the matcher would have found
// the URL in. The index has no filter and finds it.
func TestTheScanFilterMissedAnEscapedHost(t *testing.T) {
	s := newSession(t)
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("read https://ex%EF%BD%81mple.com/x")})
	ctx := context.Background()
	filtered, err := givenurl.ScanSource(ctx, s.pool, s.id, "https://example.com/x", true)
	if err != nil || filtered != "" {
		t.Fatalf("the filtered scan = %q (%v), want it to miss the escaped host", filtered, err)
	}
	unfiltered, _ := givenurl.ScanSource(ctx, s.pool, s.id, "https://example.com/x", false)
	if got := source(t, s.pool, s.id, "https://example.com/x"); got != "https://ex%EF%BD%81mple.com/x" || got != unfiltered {
		t.Errorf("index = %q, unfiltered scan %q; want both the escaped spelling", got, unfiltered)
	}
}

// A session older than the index has events and no index row. Its first
// lookup indexes every event, a chunk per transaction, and moves the index to
// its last event; later appends index themselves.
func TestALookupCatchesUpASessionOlderThanTheIndex(t *testing.T) {
	s := newSession(t)
	var evs []events.NewEvent
	for i := range 2*givenurl.CatchUpChunk + 10 {
		if i%10 == 0 {
			evs = append(evs, ev(domain.EventUserMessage, map[string]any{"content": text(fmt.Sprintf("see https://old.example/%d.", i))}))
		} else {
			evs = append(evs, ev(domain.EventAgentMessage, map[string]any{"content": text("thinking")}))
		}
	}
	s.earlierBuild(t, evs...)
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_url_indexes WHERE session_id = $1`, s.id.String()); n != 0 {
		t.Fatalf("index rows = %d before any lookup, want none", n)
	}
	// One URL in each chunk the catch-up indexes.
	for _, i := range []int{0, givenurl.CatchUpChunk + 10 - (givenurl.CatchUpChunk+10)%10, len(evs) - len(evs)%10} {
		u := fmt.Sprintf("https://old.example/%d", i)
		if got := source(t, s.pool, s.id, u); got != u {
			t.Errorf("Source(%s) = %q, want it given", u, got)
		}
	}
	if through := count(t, s.pool, `SELECT indexed_through FROM session_given_url_indexes WHERE session_id = $1`, s.id.String()); through != int64(len(evs)) {
		t.Errorf("indexed through %d after the catch-up, want the last seq %d", through, len(evs))
	}
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("and https://new.example/")})
	if through := count(t, s.pool, `SELECT indexed_through FROM session_given_url_indexes WHERE session_id = $1`, s.id.String()); through != int64(len(evs)+1) {
		t.Errorf("indexed through %d after an append, want %d", through, len(evs)+1)
	}
	if got := source(t, s.pool, s.id, "https://new.example/"); got != "https://new.example/" {
		t.Errorf("an appended URL = %q, want it given", got)
	}
}

// Lookups racing to catch up one session each index what the others have
// not: every chunk is written once, under the session lock, by whichever
// lookup finds the index where it left it, and each lookup then finds its
// URL.
func TestConcurrentLookupsCatchUpASessionTogether(t *testing.T) {
	s := newSession(t)
	var evs []events.NewEvent
	for i := range 4 * givenurl.CatchUpChunk {
		evs = append(evs, ev(domain.EventUserMessage, map[string]any{"content": text(fmt.Sprintf("https://together.example/%d", i))}))
	}
	s.earlierBuild(t, evs...)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := fmt.Sprintf("https://together.example/%d", w*len(evs)/8)
			if got, err := givenurl.Source(context.Background(), s.pool, s.id, u); err != nil || got != u {
				errs <- fmt.Errorf("Source(%s) = %q (%v), want it given", u, got, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_urls g JOIN session_given_url_indexes i ON i.id = g.index_id WHERE i.session_id = $1`, s.id.String()); n != int64(len(evs)) {
		t.Errorf("index rows = %d, want one per URL (%d)", n, len(evs))
	}
}

// A replica on a build without the index appends during a rolling upgrade:
// its events move no index, so this build's next append leaves the gap alone,
// and the next lookup indexes the gap and everything after it.
func TestAnEarlierBuildsAppendsAreCaughtUp(t *testing.T) {
	s := newSession(t)
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("first https://a.example/1")})
	use := s.append(t, domain.EventAgentToolUse, map[string]any{"name": "web_search", "input": map[string]string{"query": "x"}})
	s.earlierBuild(t,
		ev(domain.EventAgentToolResult, map[string]any{"tool_use_id": use.String(), "content": text("hit https://b.example/2")}),
		ev(domain.EventUserMessage, map[string]any{"content": text("then https://c.example/3")}))
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("last https://d.example/4")})
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_urls g JOIN session_given_url_indexes i ON i.id = g.index_id WHERE i.session_id = $1 AND g.url_key = $2`,
		s.id.String(), givenurl.URLKey("https://d.example/4")); n != 0 {
		t.Errorf("an append after the gap indexed itself (%d rows), want it left to the catch-up", n)
	}
	for _, u := range []string{"https://a.example/1", "https://b.example/2", "https://c.example/3", "https://d.example/4"} {
		if got := source(t, s.pool, s.id, u); got != u {
			t.Errorf("Source(%s) = %q, want it given", u, got)
		}
	}
}

// An append indexes in its own transaction: a lookup does not see its URL
// before it commits, sees it once it has, and a rolled-back append leaves no
// row and no index moved.
func TestAnAppendIndexesInItsTransaction(t *testing.T) {
	s := newSession(t)
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("hello")})
	ctx := context.Background()
	for _, commit := range []bool{false, true} {
		u := fmt.Sprintf("https://tx.example/%v", commit)
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"content": text(u)})
		if _, err := s.log.AppendInTx(ctx, tx, s.id, []events.NewEvent{{Type: domain.EventUserMessage, Payload: raw}}, events.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := source(t, s.pool, s.id, u); got != "" {
			t.Errorf("an uncommitted append's URL = %q, want it not yet given", got)
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if commit {
			want = u
		}
		if got := source(t, s.pool, s.id, u); got != want {
			t.Errorf("after commit=%v: %q, want %q", commit, got, want)
		}
	}
}

// Appends and lookups race: every URL an append committed before a lookup
// began is given to that lookup, whether this build's appends indexed it or
// an earlier build's left it to the catch-up the lookup runs.
func TestALookupSeesEveryAppendCommittedBeforeIt(t *testing.T) {
	s := newSession(t)
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("start")})
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 12 {
				u := fmt.Sprintf("https://race.example/%d/%d", w, i)
				payload, _ := json.Marshal(map[string]any{"content": text("see " + u)})
				var err error
				if w == 0 && i%3 == 0 {
					err = s.earlierBuildAppend(events.NewEvent{Type: domain.EventUserMessage, Payload: payload})
				} else {
					_, err = s.log.Append(context.Background(), s.id, []events.NewEvent{{Type: domain.EventUserMessage, Payload: payload}})
				}
				if err != nil {
					errs <- err
					return
				}
				got, err := givenurl.Source(context.Background(), s.pool, s.id, u)
				if err != nil || got != u {
					errs <- fmt.Errorf("Source(%s) right after its append = %q (%v), want it given", u, got, err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A session's index goes with it, by cascade, and a deleted session gives
// nothing. An archived session, which takes no appends, still answers.
func TestTheIndexFollowsItsSession(t *testing.T) {
	s := newSession(t)
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("https://keep.example/a")})
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, s.id.String()); err != nil {
		t.Fatal(err)
	}
	if got := source(t, s.pool, s.id, "https://keep.example/a"); got != "https://keep.example/a" {
		t.Errorf("an archived session's URL = %q, want it given", got)
	}
	id := count(t, s.pool, `SELECT id FROM session_given_url_indexes WHERE session_id = $1`, s.id.String())
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_urls WHERE index_id = $1`, id); n == 0 {
		t.Fatal("the session has no index rows to delete")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, s.id.String()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_url_indexes WHERE session_id = $1`, s.id.String()); n != 0 {
		t.Errorf("session_given_url_indexes keeps %d rows of a deleted session", n)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_urls WHERE index_id = $1`, id); n != 0 {
		t.Errorf("session_given_urls keeps %d rows of a deleted session", n)
	}
	if got := source(t, s.pool, s.id, "https://keep.example/a"); got != "" {
		t.Errorf("a deleted session's URL = %q, want nothing", got)
	}
}

// A page too costly to index — URLs on one host run together — is listed on
// the index row instead, and a lookup reads it with the matcher: a short request
// is found in it, and one whose window spans the page spends the budget.
func TestAPayloadTooCostlyToIndexIsReadAtLookup(t *testing.T) {
	s := newSession(t)
	page := strings.Repeat("https://docs.example.com/,", 4000)
	if _, _, ok := givenurl.Readings(page); ok {
		t.Fatal("the page fits the index budget; the test needs one that does not")
	}
	s.webResult(t, "web_fetch", page, false)
	if n := count(t, s.pool, `SELECT cardinality(unindexed) FROM session_given_url_indexes WHERE session_id = $1`, s.id.String()); n != 1 {
		t.Fatalf("unindexed payloads = %d, want the page", n)
	}
	if got := source(t, s.pool, s.id, "https://docs.example.com/"); got != "https://docs.example.com/" {
		t.Errorf("Source in the unindexed page = %q, want it given", got)
	}
	long := "https://docs.example.com/" + strings.Repeat("a", 8<<10-len("https://docs.example.com/"))
	if _, err := givenurl.Source(context.Background(), s.pool, s.id, long); !errors.Is(err, givenurl.ErrReadingBudget) {
		t.Errorf("a window-wide request = %v, want ErrReadingBudget", err)
	}
	// What the index holds is found without reading the page.
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("https://docs.example.com/" + strings.Repeat("a", 8<<10-len("https://docs.example.com/")))})
	if got := source(t, s.pool, s.id, long); got != long {
		t.Errorf("an indexed URL behind an unindexed page = %q, want it given", got)
	}
}

// Of the readings a request names, the index answers the one the scan met
// first: what a person wrote over any result, then the newest result — and
// a reading spelled exactly as the request over all of them.
func TestTheIndexAnswersInTheScansOrder(t *testing.T) {
	s := newSession(t)
	s.webResult(t, "web_fetch", "old https://order.example/A", false)
	s.webResult(t, "web_fetch", "new HTTPS://ORDER.example/A", false)
	if got := source(t, s.pool, s.id, "https://order.example/A#x"); got != "HTTPS://ORDER.example/A" {
		t.Errorf("between two results = %q, want the newer", got)
	}
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("mine: https://Order.example/A")})
	s.webResult(t, "web_fetch", "newest https://ORDER.EXAMPLE/A", false)
	if got := source(t, s.pool, s.id, "https://order.example/A#x"); got != "https://Order.example/A" {
		t.Errorf("a person's and a newer result's = %q, want the person's", got)
	}
	if got := source(t, s.pool, s.id, "https://order.example/A"); got != "https://order.example/A" {
		t.Errorf("a request spelled as a result spelled it = %q, want that spelling", got)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_urls g JOIN session_given_url_indexes i ON i.id = g.index_id WHERE i.session_id = $1 AND g.url_key = $2`,
		s.id.String(), givenurl.URLKey("https://order.example/A")); n != 4 {
		t.Errorf("rows for the URL = %d, want one per spelling (4)", n)
	}
	// Given again, a spelling keeps one row, at its first place.
	s.webResult(t, "web_fetch", "again https://Order.example/A", false)
	if n := count(t, s.pool, `SELECT count(*) FROM session_given_urls g JOIN session_given_url_indexes i ON i.id = g.index_id WHERE i.session_id = $1 AND g.url_key = $2`,
		s.id.String(), givenurl.URLKey("https://order.example/A")); n != 4 {
		t.Errorf("rows after a spelling was given again = %d, want still 4", n)
	}
	if rank := count(t, s.pool, `SELECT g.rank FROM session_given_urls g JOIN session_given_url_indexes i ON i.id = g.index_id WHERE i.session_id = $1 AND g.spelling = $2`,
		s.id.String(), []byte("https://Order.example/A")); rank != 0 {
		t.Errorf("the person's spelling, given again by a result, has rank %d, want 0", rank)
	}
}

// The index's key is eight bytes of a hash, so two forms can share one. A
// lookup takes a row only when the form its spelling gives is the
// request's: here a row forged under a given URL's key, spelling another,
// is passed over, and the given one is still found.
func TestARowWhoseSpellingIsNotTheRequestsIsPassedOver(t *testing.T) {
	s := newSession(t)
	s.append(t, domain.EventUserMessage, map[string]any{"content": text("https://given.example/x")})
	id := count(t, s.pool, `SELECT id FROM session_given_url_indexes WHERE session_id = $1`, s.id.String())
	for _, plain := range []bool{false, true} {
		if _, err := s.pool.Exec(context.Background(), `INSERT INTO session_given_urls
			(index_id, url_key, seq, ord, rank, plain, spell_key, spelling) VALUES ($1, $2, 999, 0, 0, $3, $4, $5)`,
			id, givenurl.URLKey("https://given.example/x"), plain, []byte(fmt.Sprint("forged", plain)), []byte("https://attacker.example/?d=1")); err != nil {
			t.Fatal(err)
		}
	}
	if got := source(t, s.pool, s.id, "https://given.example/x#y"); got != "https://given.example/x" {
		t.Errorf("Source = %q, want the given spelling, not the forged row's", got)
	}
	if got := source(t, s.pool, s.id, "https://attacker.example/?d=1"); got != "" {
		t.Errorf("Source of the forged spelling = %q, want nothing: no row is under its key", got)
	}
}
