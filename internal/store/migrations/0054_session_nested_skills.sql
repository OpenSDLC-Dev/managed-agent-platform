-- The skills a read found below a mounted repository's root
-- (docs/plan/65_repo-skill-discovery.md decision 5): the executor scans the
-- directories between the file and the root and records what it found, by
-- the read's result event, in the transaction that commits that result. The
-- brain announces them after that result, to the thread that read the file.
CREATE TABLE session_nested_skills (
    result_event_id text PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    session_id      text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    skills          jsonb NOT NULL
);

CREATE INDEX session_nested_skills_session ON session_nested_skills (session_id);
