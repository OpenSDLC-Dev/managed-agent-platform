---
status: archived
issue: "#655"
---

# File expiration: the upload parameter, the grace window, and the purge (plan 49)

> **Archived 2026-09-11, completed (#655).** Both slices landed: the parameter, the column
> and the enforcement in PR #691, the retention sweep in the PR that archives this file. The
> as-built shape is `store.FileLiveSQL` and its five composing readers, plus
> `internal/api/fileretention.go`; the delivery narrative is the `changelog.d/` fragments.
> Retained for the decisions below, chiefly why the bytes go at the purge rather than at the
> expiry and why "GC is a non-goal" survives a sweep that deletes files.
>
> Numbered **48** while slice 1 was in review, which is what PR #691 and the commits under it
> call it. An earlier-opened PR (#677, the reap kick) had claimed 48 for a plan invisible in
> any checkout and merged first, so this one renumbered — the convention being that the
> earlier claimant keeps the number.

`POST /v1/files` refuses `expires_in_seconds` with a 400 — `parseFileUpload` admits one part
named `file` and rejects every other name — so a client that sets the documented expiry fails
its upload outright. Nothing in the registry expires: no column, no enforcement, no purge.

Most of the fix is mechanical. The last step is not: it deletes rows and objects nobody asked
to delete, which is the position `deleteOrphanedFile`'s comment states as a non-goal. That
decision, and the alternative it beat, are what this file is for.

## The published lifecycle

The public Files API docs ("File expiration") are explicit, and settle more than the issue
expected them to:

- `expires_in_seconds` is "an integer number of seconds between 3,600 (1 hour) and 7,776,000
  (90 days)"; `expires_at` is "the upload time plus that value" and is "set once at upload and
  cannot be changed".
- At `expires_at`: "Downloading its content (`GET /v1/files/{file_id}/content`) returns a 404
  error"; "Its metadata (`GET /v1/files/{file_id}`) remains readable for up to 30 days, with
  `expires_at` in the past"; "It continues to appear in list responses during that window."
- "Deleting an expired file with `DELETE /v1/files/{file_id}` removes its metadata immediately
  instead of waiting for the 30-day window to elapse."
- On the bytes: "the underlying content may be retained for a limited period thereafter for
  safety review before permanent deletion".

So the issue's request for a recording before the download behavior can be implemented is
answered by the docs, and what is left to infer is narrower than it anticipated: the *order*
of the expiry check against the two gates already on that route, the 400's wording for an
out-of-range value, and whether a session may mount an expired file. Those three, and nothing
about the status codes, go to docs/DIVERGENCES.md.

## Slice 1 — the parameter, the column, the enforcement

Migration `0037` (number read from the directory at writing) adds `files.expires_at
timestamptz`, nullable, null meaning never.

`parseFileUpload` accepts one `expires_in_seconds` part beside the file part, in either order,
bounds-checked against the documented range. `insertFile` computes the instant as
`now() + make_interval(secs => $n)` in the INSERT rather than in Go: the docs define
`expires_at` as the upload time plus the value, the upload time is the `created_at` Postgres
stamps in that same statement, and no replica's clock may enter a comparison the database will
later make. `renderFile` reports the stored value in place of its hardcoded null.

`downloadFile` answers 404 once the instant has passed. The check goes **before** both gates
already on the route — the management lane's `downloadable` 400 and the environment-key lane's
mount-scope check — so one rule answers whoever asks, rather than two lanes disagreeing about
a file that no longer has content.

**That rule belongs to every reader, not to the HTTP route.** Four packages read the `files`
table in order to hand a file's bytes to something, and each was written when a row's
existence was the whole question — so gating the route alone would have left the
platform-managed executor materializing bytes into a sandbox that the BYOC worker, going
through the same route, refuses. The predicate therefore lives once, as `store.FileLiveSQL`
beside `SessionTombstoneInsertSQL` on the schema's owner, and is composed by the session
mount check, the executor's materialization, the brain's mounted-files block and the outcome
rubric's validation. One reader omits it deliberately and says so: the grader's deliverables
listing selects `scope_type = 'session'` rows, which only the outputs harvest writes, and the
harvest sets no expiry.

The mount refusal is this platform's analogue of the docs' "A Messages request that references
the file fails before inference" — a mount is where a file reaches a model here. A session
that outlives a file it already mounted needs no new behavior: an expired file is the same
dangling miss a deleted one already was, counted and skipped on both halves of the pull
protocol. What expiry does **not** do is reach into a sandbox that already holds the bytes —
the executor's sentinel skips re-streaming a mount set that has not changed, so the residual a
delete already leaves is the one an expiry leaves, and the registry says so in the same place.

The list and the metadata route are deliberately untouched. An expired file keeps appearing
with `expires_at` in the past, which is the published behavior; the docs tell clients to
"compare `expires_at` to the current time to filter expired files" themselves.

## Slice 2 — the purge

`internal/api/fileretention.go`, beside `memoryretention.go` and built from it: one
statement per sweep in the controlplane, taken at startup and then hourly. (As built the
startup sweep is the departure from `memoryretention.go`: a ticker does not fire on
creation, so waiting first would mean a control plane restarted more often than the
interval never swept at all.)

```
DELETE FROM files WHERE id IN (SELECT id FROM files
  WHERE expires_at < now() - <30 days> LIMIT <batch>) RETURNING id
```

then a best-effort `blobs.Delete` per returned id — `deleteFile`'s order (row first, object
after), for `deleteFile`'s reason. The DELETE is itself the claim, so two replicas never
delete the same object twice: a row is returned to exactly one of them. A crash between the
row and the object leaves an orphan, the outcome this package already accepts everywhere else.

The batch bound is the one thing this sweep has that `memoryretention`'s does not, and the
reason is the object delete: that sweep does nothing per row, while this one owes a network
call for each row it removed, so an unbounded first pass over a backlog would owe as many as
the backlog held. (It does not hold a transaction open across them — the DELETE commits when
the statement returns, and the objects go after it, on a context the sweep's own cancellation
cannot reach, so a shutdown mid-sweep cannot silently orphan a committed batch.) The batch is
taken oldest-first under `FOR UPDATE SKIP LOCKED`, so a backlog drains as a queue rather than
as an unpredictable subset, which is what makes "eventually" true, and two replicas take
disjoint batches instead of blocking on each other.

## Decisions

1. **The bytes go at the purge, not at expiry.** The alternative is a two-stage sweep that
   drops the object at `expires_at` and the row 30 days later — which is what the docs'
   "released from your storage quota" describes. It needs a third state on the row to know the
   object is already gone, and buys nothing here: this platform deliberately does not enforce
   the quota that would free (docs/DIVERGENCES.md, "per-organization storage quota not
   enforced"), and the docs permit the retention in as many words. One stage, one statement,
   no new column.
2. **The purge has no off switch**, like memory retention's and unlike the dream runner's. So
   the published "up to 30 days" ceiling holds without an operator opting in, and the read
   paths need no second expiry rule to stay correct when it is off.
3. **Dream files are out of range by construction, not by a clause.** Nothing sets
   `expires_at` on them, so the purge's predicate never sees one and `deleteFile`'s
   open-dream refusal needs no twin here.
4. **"GC is a non-goal" stays true where it stands.** That note is about objects whose row
   never landed — accidents nobody can enumerate, where a sweep would have to guess what is
   live. An expired file is the opposite: the row names the object, and the deletion is the
   feature the client bought at upload. Slice 2 says so beside the note rather than editing it.

## Acceptance

- An upload carrying `expires_in_seconds` succeeds and its response `expires_at` is
  `created_at` plus the value; 3599 and 7776001 are each a 400, 3600 and 7776000 each succeed.
- An upload without the part still answers `expires_at: null`.
- Past `expires_at`: the content route 404s on both lanes, the metadata route and the list
  still answer, and `DELETE` still removes the row.
- A session cannot be created mounting an expired file; neither half of the pull protocol
  materializes one, the brain counts it as a miss rather than describing it, and it cannot be
  named as an outcome rubric.
- The sweep removes a row and its object once 30 days have passed, and leaves alone both a file
  short of that and a file with no expiry at all.
- `make verify` green, coverage gate held.
