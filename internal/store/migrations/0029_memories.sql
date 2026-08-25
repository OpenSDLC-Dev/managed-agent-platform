-- Memories and their versions (docs/plan/36_memory-stores.md slice 2,
-- decisions 1 and 4-6, #52): the documents a memory store holds, and the
-- append-only attributed history behind them. Both cascade from 0028's
-- memory_stores, which is what makes the store's documented hard delete
-- ("The store and all its memories and versions are no longer retrievable")
-- one statement.

CREATE TABLE memories (
    id                 text PRIMARY KEY,
    memory_store_id    text NOT NULL REFERENCES memory_stores(id) ON DELETE CASCADE,
    -- COLLATE "C" pins byte order for this column and for the unique index
    -- below, which inherits it: memories list in byte-wise path order, and the
    -- databases this platform runs against do not agree on any other. The
    -- compose and Helm postgres:16-alpine images report en_US.utf8, which musl
    -- happens to sort as bytes; Cloud SQL's glibc en_US.UTF8 does not. The
    -- executor pins LC_ALL=C on the shell side for the same reason.
    path               text COLLATE "C" NOT NULL,
    -- "" is a real memory, not an absent one: create takes content as required
    -- and documents "" as the way to make an empty memory.
    content            text NOT NULL,
    content_sha256     text NOT NULL,
    content_size_bytes integer NOT NULL,
    -- The head pointer. Not a foreign key for the reason the versions table
    -- gives in reverse: the two rows are written in one transaction, and a FK
    -- either way would have to be deferred to admit that.
    memory_version_id  text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- Paths are unique within a store and case-sensitive. The index is the
    -- equality half of the occupancy rule only: an ancestor/descendant
    -- conflict (/a against /a/b) is a predicate the write path runs itself.
    UNIQUE (memory_store_id, path)
);

CREATE TABLE memory_versions (
    id                 text PRIMARY KEY,
    memory_store_id    text NOT NULL REFERENCES memory_stores(id) ON DELETE CASCADE,
    -- Deliberately NOT a foreign key: "Versions belong to the store (not the
    -- individual memory) and persist after the memory is deleted", so the
    -- deleted row's own lineage — including its `deleted` version — outlives
    -- the memories row it names.
    memory_id          text NOT NULL,
    -- created | modified | deleted.
    operation          text NOT NULL,
    -- All four nullable, and null for two different reasons the API separates:
    -- a `deleted` version carries a path but no content, sha or size, and a
    -- redacted version carries none of the four.
    path               text,
    content            text,
    content_sha256     text,
    content_size_bytes integer,
    -- The wire's actor object verbatim — {"type":"api_actor","api_key_id":…},
    -- {"type":"user_actor","user_id":…} or, from slice 4,
    -- {"type":"session_actor","session_id":…} — so a version renders its
    -- attribution without a join. Null when no writer is recorded.
    created_by         jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    -- Redaction is the one in-place mutation a version row ever takes.
    redacted_at        timestamptz,
    redacted_by        jsonb
);

-- The list is per store, newest-first, keyset-paged on (created_at, id).
CREATE INDEX memory_versions_store_created_idx
    ON memory_versions (memory_store_id, created_at DESC, id DESC);

-- And the same page narrowed to one memory's lineage, over a history nothing
-- ever prunes (#476).
CREATE INDEX memory_versions_memory_idx
    ON memory_versions (memory_store_id, memory_id, created_at DESC, id DESC);
