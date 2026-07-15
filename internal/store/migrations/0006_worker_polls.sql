-- worker_polls records the most recent poll from each BYOC worker per
-- environment, so the work-stats endpoint can report workers_polling: the number
-- of distinct workers that have polled the queue in the last 30 seconds
-- (BetaSelfHostedWorkQueueStats.workers_polling). A worker identifies itself with
-- the Anthropic-Worker-ID header on each poll; a poll without the header is not
-- tracked (the wire documents workers_polling as requiring worker_id).
--
-- One row per (environment, worker): each poll upserts last_polled_at. Default
-- worker ids are minted fresh per process (worker.defaultWorkerID), so a bare
-- upsert would leak one permanent row per process start; RecordPoll therefore
-- reaps rows aged past the workers_polling window in the same statement, keeping
-- the table bounded by the workers that have polled recently rather than by every
-- worker id ever seen. A worker that stops polling ages out of the count and is
-- then reaped by the next poll from any of the environment's workers.
CREATE TABLE worker_polls (
    environment_id text        NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    worker_id      text        NOT NULL,
    last_polled_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (environment_id, worker_id)
);
