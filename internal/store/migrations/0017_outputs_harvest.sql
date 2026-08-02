-- outputs_harvest (docs/plan/21_outcomes.md, Decision 8): the internal work
-- kind that carries a session's deliverables harvest — the cloud executor
-- walks /mnt/session/outputs/ in the session sandbox and publishes the
-- snapshot into the files registry before the grading pass runs. The CHECK is
-- dropped and re-added under its auto-generated name, the 0015_web_exec_kind
-- pattern.
ALTER TABLE work_items DROP CONSTRAINT work_items_kind_check;
ALTER TABLE work_items ADD CONSTRAINT work_items_kind_check
    CHECK (kind IN ('model_turn', 'tool_exec', 'web_exec', 'outputs_harvest'));

-- Each harvest is a snapshot keyed by the file's sandbox-relative path,
-- stored as filename: one registry row per (scope, path), so a re-harvest
-- replaces per path instead of accumulating. Partial — plain uploads have no
-- scope and stay free to repeat a name.
CREATE UNIQUE INDEX files_scope_filename_idx ON files (scope_id, filename)
    WHERE scope_id IS NOT NULL;
