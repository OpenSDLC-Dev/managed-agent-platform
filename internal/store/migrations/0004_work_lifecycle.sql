-- Wire work API lifecycle timestamps. A BetaSelfHostedWork carries these as
-- required fields; the state-transition endpoints populate them as a work item
-- moves queued -> starting (ack) -> active (heartbeat) -> stopping/stopped
-- (stop). A queued item has reached none of them, so they stay null until acked.
ALTER TABLE work_items
    ADD COLUMN acknowledged_at   timestamptz,
    ADD COLUMN started_at        timestamptz,
    ADD COLUMN stop_requested_at timestamptz,
    ADD COLUMN stopped_at        timestamptz;
