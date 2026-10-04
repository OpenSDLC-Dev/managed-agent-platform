-- web_fetch fetches only a URL the session was given (#823): one a person wrote
-- into it or a successful web_search or web_fetch result returned. Until now
-- each fetch read the session's person-written payloads and web results to
-- find it, every one of them, and Postgres stringified each to filter it, so a
-- fetch cost the session's history (#836). These tables index the given URLs
-- instead, written as their events are appended, in the append's transaction
-- (internal/givenurl).
--
-- session_given_url_indexes has a row per indexed session. Every event at or
-- below indexed_through is indexed, and unindexed lists the seqs of the
-- payloads that were too costly to index — a page of URLs run together —
-- which a lookup still reads whole. id is what the entries carry instead of
-- the session's text id: it is in every entry and its key, which it keeps
-- half the size.
--
-- session_given_urls holds one row per reading a payload gives (a URL in text
-- is read every way the text allows; givenurl's readings). url_key, the
-- lookup's key, is the first 8 bytes of the sha256 of the normalized URL a
-- request must normalize to; a lookup recomputes that form from each
-- spelling it finds, plain saying how, so two forms sharing a key are told
-- apart. spell_key is the first 16 bytes of the sha256 of the spelling, which
-- the row keeps whole, as bytes, since a reading can end inside a character. A
-- spelling given again keeps the place a lookup meets it first: rank 0 for
-- what a person wrote and 1 for a web result, then the newest seq, then ord,
-- its order within the payload.
--
-- No backfill. The readings are Go — net/url and IDNA — which SQL cannot run,
-- and filling the index for every session here would hold the lock below for
-- the time it takes to read every web result ever stored. A session without an
-- index row, or one behind its last event, is caught up by its next lookup, a
-- chunk at a time (givenurl's catchUp): a session older than this migration,
-- and events a replica on an earlier build appends during a rolling upgrade,
-- which move no index row. A session that never fetches is never read.
--
-- Both tables follow their session by cascade, so every session delete — this
-- build's, an earlier build's, a hand-written one — clears them.
--
-- The foreign key to sessions takes it SHARE ROW EXCLUSIVE until the migration
-- commits. That waits for no reader, but every writer of sessions queues behind
-- the request, so, as 0043, 0045, 0046 and 0047 do, this waits at most 2s for
-- it, and migrate.go retries the give-up (55P03). Neither table has a row to
-- check, so the lock is held for the time two CREATE TABLEs take.
SET LOCAL lock_timeout = '2s';

CREATE TABLE session_given_url_indexes (
    session_id      text     PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    id              bigint   GENERATED ALWAYS AS IDENTITY UNIQUE,
    indexed_through bigint   NOT NULL,
    unindexed       bigint[] NOT NULL DEFAULT '{}'
);

CREATE TABLE session_given_urls (
    index_id  bigint   NOT NULL REFERENCES session_given_url_indexes(id) ON DELETE CASCADE,
    url_key   bigint   NOT NULL,
    seq       bigint   NOT NULL,
    ord       integer  NOT NULL,
    rank      smallint NOT NULL,
    plain     boolean  NOT NULL,
    spell_key bytea    NOT NULL,
    spelling  bytea    NOT NULL,
    PRIMARY KEY (index_id, url_key, spell_key)
);

SET LOCAL lock_timeout TO DEFAULT;
