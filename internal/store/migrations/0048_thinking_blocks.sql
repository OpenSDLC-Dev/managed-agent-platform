-- Thinking blocks kept for replay (#67, docs/plan/60_thinking-replay.md). The
-- wire agent.thinking event carries no content, and stays that way; what a
-- model needs back on a later request — a thinking block with its signature,
-- or a redacted_thinking block — is kept here, keyed by the event it belongs
-- to, written by the settlement that commits its turn and read by replay
-- alone. Nothing that serves events reads it.
--
-- block is json, not jsonb: json keeps the bytes the brain wrote, which are
-- the bytes a signature covers, and accepts the \u0000 escape jsonb refuses.
-- model and prefix_digest are the guard a block is replayed under: the model
-- id its request went to, and a SHA-256 over everything that request put in
-- front of it.
--
-- A child of events, so it reserves no scope columns of its own (store's
-- package comment), and goes with its event, which goes with its session.
CREATE TABLE thinking_blocks (
    event_id      text PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    session_id    text NOT NULL,
    model         text NOT NULL,
    prefix_digest text NOT NULL,
    block         json NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Replay reads a session's blocks on every turn.
CREATE INDEX thinking_blocks_session_idx ON thinking_blocks (session_id);
