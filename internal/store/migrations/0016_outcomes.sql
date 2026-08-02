-- Plan 21 (session outcomes): per-outcome evaluation state, stored in the wire
-- shape as a jsonb array on the sessions row (the resources/usage precedent)
-- and mutated only inside append transactions under the session row lock, so
-- the projection can never disagree with the event log it is derived from.
ALTER TABLE sessions ADD COLUMN outcome_evaluations jsonb NOT NULL DEFAULT '[]';
