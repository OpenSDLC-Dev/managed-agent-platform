-- The skills a session's mounted repositories hold (docs/plan/65_repo-skill-discovery.md):
-- the executor scans each checkout's .claude/skills once, when it lands, and
-- records what it found, and the brain announces it. An object mapping each
-- scanned repository resource's id to its skills; NULL until the first scan.
ALTER TABLE sessions ADD COLUMN repo_skills jsonb;

-- When the brain first held a turn for those scans. It holds a session once:
-- a turn woken with nothing recorded goes ahead without the skills.
ALTER TABLE sessions ADD COLUMN repo_skills_held_at timestamptz;
