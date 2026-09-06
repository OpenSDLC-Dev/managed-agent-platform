Stage 4 of 4: index and audit. Check /mnt/memory/team-notes
against the digests under dream/digests/ first, and against dream/plan.md if it
is still there: a change they routed that never landed is made now.

Rewrite /mnt/memory/team-notes/MEMORY.md as the store's index — one line
per memory, at most 150 characters, its path and what it holds, never its
content — so that every memory has a line and every line resolves.
A tombstone is a memory like any other and keeps its line; the tombstones
are listed again in a trailing *to remove* section, so the caller can find
what to remove.

Then write dream/report.md: the files you created and updated, the ones you
retired as tombstones under a *to remove* heading, the
contradictions you resolved, the ones still open, and the transcripts that
produced nothing. "Nothing changed" is a valid and successful report — if the
transcripts carried nothing durable, say so and leave the store as it is.