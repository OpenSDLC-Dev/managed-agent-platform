---
status: archived
issue: 836
---

# web_fetch's given URLs are indexed as they are appended (#836)

## The change

Since #823 every `web_fetch` read the session's person-written payloads and successful web
results, all of them, to find the given URL its request names: a SQL host-substring filter
over every candidate payload, then the matcher (`urlMatcher`) in process, under a 64 MiB
budget. That cost grows with the session. Now the readings are found once, when their event
is appended, in the append's transaction, and a fetch is one indexed lookup
(`internal/givenurl`). The rule does not change: the same URLs are given, read the same ways,
compare the same, and a request gets the same given spelling the scan returned.

This needs a plan because it adds derived state to the event log's append — a seam the log
did not have — and a migration whose backfill had to be decided.

## Decisions

1. **The index holds exactly what the matcher accepts.** The matcher is request-shaped: its
   windows and its host come from the request. But it accepts a reading only for the request
   whose normalized form is the reading's own, so `readings` judges each candidate against
   the windows and host that form gives, mirroring `urlMatcher.occurrence` step by step,
   down to a window that ends inside a multibyte character. The matcher stays as the oracle
   the index is tested against, and as the reader of what the index leaves out.
2. **Order is kept.** Each row carries the place the scan met it — rank (a person's before a
   web result), seq (newest first), and its order within the payload — and a spelling given
   again keeps its first place. An exact spelling still wins. Objects are now walked in key
   order, by the matcher too: the scan's map order was random, so this is one of its
   orders.
3. **A payload too costly to index is read at lookup.** Past 4 MiB of parsing or 8,192
   distinct readings — a page of URLs run together, a URL followed by a long paragraph with
   no space, as CJK text writes one — the payload's seq is listed on the session's index row
   and the lookup reads it with the matcher and its budget, as every lookup read every payload
   before. The budget now covers only those payloads.
4. **No backfill; a lookup catches up.** The readings are Go (`net/url`, IDNA), so SQL cannot
   compute them, and a migration that did would hold sessions locked for the event table's
   length. `indexed_through` says how far a session's index is complete; an append indexes
   and moves it only when it was complete through the previous seq, and a lookup finding it
   behind indexes the rest, 64 seqs per transaction, under the session row lock only to
   write. That one mechanism covers sessions older than 0048 and events a replica on an
   earlier build appends during a rolling upgrade. Migration 0048 is DDL only: 23 ms over
   500,000 events (1 GB).
5. **Rows are small.** Entries carry a per-session bigint, not the session's text id, and an
   8-byte key; a lookup recomputes each candidate's normalized form from its spelling, so a
   key collision is passed over, never answered. Both tables follow the session by cascade.

## Rejected

- An index of hosts, with the matcher run over the payloads naming the request's host: exact
  by construction, but still linear in a session's pages from one site.
- Indexing every reading with no ceiling: one page of URLs run together is millions of rows.
- A ceiling with no fallback: it would refuse URLs the scan found.
- A startup backfill in Go: every binary migrates at start, and it would hold the migration
  lock for the event table's length.

## Cost

Measured on a synthetic page of 100 markdown links (6 KB): about 200 rows a page, 207 bytes
a row with its index, 16.6 MB for 400 pages. A lookup reads 3 buffers at 10 pages and 4 at
400, about 1.2 ms either way, where the scan took 2.5 ms and 55 ms. Appending a 100 KB web
result costs about 7 ms more, under the session row lock. A pre-#836 session's first lookup
indexes its history once: 2.4 s for 1,000 pages, against 145 ms for one scan.

## Verification

`TestReadingsAreWhatTheMatcherAccepts` compares the index with the matcher on every
substring and respelling of fixed and generated texts, and its fuzz target runs the same
check; `TestTheIndexAnswersAsTheScanDid` compares `Source` with the scan's own SQL, kept in
the test. Mutants of the judgment and the lookup each fail one. The scan's filter dropped a
payload naming a host only through a percent-escaped full-width letter, which the index
finds (`TestTheScanFilterMissedAnEscapedHost`).
