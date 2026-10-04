package givenurl_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/givenurl"
	"github.com/jackc/pgx/v5/pgxpool"
)

// page is a fetched page as a reader returns one: markdown prose with a
// hundred links, some on the site's own host, a few in sentences.
func page(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Page %d\n\n", n)
	for i := range 100 {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, "- [Chapter %d](https://docs.example.com/guide/%d/chapter-%d.html)\n", i, n, i)
		case 1:
			fmt.Fprintf(&b, "See https://site%d.example.org/articles/%d?ref=page%d for more. ", i%7, i, n)
		case 2:
			fmt.Fprintf(&b, "[nav](https://docs.example.com/nav/%d) ", i)
		case 3:
			fmt.Fprintf(&b, "Lorem ipsum dolor sit amet (https://cdn.example.net/assets/%d/%d.png), consectetur.\n", n, i)
		}
	}
	return b.String()
}

// lookupBuffers is how many buffers the index read takes for want, from
// Postgres's own count.
func lookupBuffers(t *testing.T, pool *pgxpool.Pool, sid domain.ID, want string) int {
	t.Helper()
	var plan []byte
	id := count(t, pool, `SELECT id FROM session_given_url_indexes WHERE session_id = $1`, sid.String())
	if err := pool.QueryRow(context.Background(), `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
		SELECT spelling, plain, rank, seq, ord FROM session_given_urls WHERE index_id = $1 AND url_key = $2`,
		id, givenurl.URLKey(want)).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	var out []struct {
		Plan struct {
			Hit  int `json:"Shared Hit Blocks"`
			Read int `json:"Shared Read Blocks"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(plan, &out); err != nil {
		t.Fatal(err)
	}
	return out[0].Plan.Hit + out[0].Plan.Read
}

func median(ds []time.Duration) time.Duration {
	slices.Sort(ds)
	return ds[len(ds)/2]
}

// A lookup's cost does not grow with the session's history (#836): against
// a session of 10 fetched pages and one of 400 — each a hundred links — the
// index read takes the same buffers, and the lookup the same time, where the
// scan it replaced read every page. The buffers are the assertion: Postgres
// counts them, so they do not flake. The times are logged.
func TestALookupCostsTheSameWhateverTheHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 400-page session")
	}
	ctx := context.Background()
	type measured struct {
		pages            int
		buffers          int
		lookup, scan     time.Duration
		rows, indexBytes int64
	}
	var ms []measured
	for _, pages := range []int{10, 400} {
		s := newSession(t)
		for n := range pages {
			s.webResult(t, "web_fetch", page(n), false)
		}
		// The oldest page's URL: the scan meets it last.
		want := "https://docs.example.com/guide/0/chapter-96.html"
		m := measured{pages: pages, buffers: lookupBuffers(t, s.pool, s.id, want)}
		var lookups, scans []time.Duration
		for range 15 {
			start := time.Now()
			if got := source(t, s.pool, s.id, want); got != want {
				t.Fatalf("Source = %q, want %q", got, want)
			}
			lookups = append(lookups, time.Since(start))
			start = time.Now()
			if got, err := givenurl.ScanSource(ctx, s.pool, s.id, want, true); err != nil || got != want {
				t.Fatalf("ScanSource = %q (%v)", got, err)
			}
			scans = append(scans, time.Since(start))
		}
		m.lookup, m.scan = median(lookups), median(scans)
		m.rows = count(t, s.pool, `SELECT count(*) FROM session_given_urls g JOIN session_given_url_indexes i ON i.id = g.index_id WHERE i.session_id = $1`, s.id.String())
		m.indexBytes = count(t, s.pool, `SELECT pg_total_relation_size('session_given_urls')`)
		t.Logf("%d pages: %d index rows (%d bytes with its index), lookup reads %d buffers; lookup %v, the scan it replaced %v",
			m.pages, m.rows, m.indexBytes, m.buffers, m.lookup, m.scan)
		ms = append(ms, m)
	}
	small, large := ms[0], ms[1]
	if large.buffers > small.buffers+3 {
		t.Errorf("the index read takes %d buffers at %d pages and %d at %d; want it flat", small.buffers, small.pages, large.buffers, large.pages)
	}
}
