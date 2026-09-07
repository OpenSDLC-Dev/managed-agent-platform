Stage 2 of 4: digest. Check that dream/plan.md exists first; if it does not, do
stage 1's work now, then continue here.

The 18 transcripts split into 3 batches — batch 1: transcripts 1 to 8; batch 2: 9 to 16; batch 3: 17 to 18.

Spawn one thread per batch: 3 create_agent calls naming the agent "dream", all in
this one reply. Each task message gives its batch number, its transcript range
and the digest to write, dream/digests/<batch>.md. Then call wait_for_agents.

A transcript file is named <sequence>-<session id>.md under
/mnt/session/uploads/dream/transcripts/
and the sequence number is not padded, so name each batch's files in full in
its task message, or give the range as numbers to compare — "sequence 1 through
8" — never as a prefix to match: 1 prefixes 1, 10 and 100 alike.

When the reports are in, check on disk that every batch's digest is there: a
report is not proof, and a thread that ended without calling submit_result
reported nothing at all. Rebuild a missing digest — spawn that batch again, or
read it yourself. Write nothing else in this stage.