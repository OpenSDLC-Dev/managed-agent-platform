-- The skills a session's mounted repositories hold (docs/plan/65_repo-skill-discovery.md):
-- the executor scans each checkout's .claude/skills once and records what it
-- found, and the brain announces it. NULL until that scan; an empty array is a
-- scan that found none.
ALTER TABLE sessions ADD COLUMN repo_skills jsonb;
