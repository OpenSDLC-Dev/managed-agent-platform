-- 0007_skills: the skills registry (docs/plan/06_skills.md, slice 2).
--
-- Archive bytes live in object storage (internal/blob, key layout
-- skills/{skill_id}/{version}.zip); these tables hold the wire-visible
-- metadata so listing and brain-side injection never read an archive.

CREATE TABLE skills (
    id             text PRIMARY KEY,
    org_id         text NOT NULL DEFAULT 'default',
    workspace_id   text NOT NULL DEFAULT 'default',
    project_id     text NOT NULL DEFAULT 'default',
    source         text NOT NULL CHECK (source IN ('custom', 'anthropic')),
    display_title  text NOT NULL,
    -- The version string of the most recently created version; NULL once
    -- every version has been deleted (the skill row itself deletes last —
    -- the API refuses to delete a skill that still has versions).
    latest_version text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- display_title is unique among a workspace's CUSTOM skills only; the
-- anthropic catalog is exempt (its titles are Anthropic's, not the tenant's).
CREATE UNIQUE INDEX skills_custom_display_title_uq
    ON skills (org_id, workspace_id, display_title)
    WHERE source = 'custom';

-- Immutable per-version rows. No ON DELETE CASCADE from skills: the wire
-- contract 400s a skill delete until every version is deleted first, and the
-- default NO ACTION makes the database enforce the same order. Scope columns
-- are inherited from the parent skill row (the agent_versions precedent).
CREATE TABLE skill_versions (
    id          text PRIMARY KEY, -- skillver_…
    skill_id    text NOT NULL REFERENCES skills(id),
    -- Server-minted Unix-epoch-microseconds string for custom skills;
    -- date-based (YYYYMMDD) for imported anthropic skills.
    version     text NOT NULL,
    -- Extracted at upload time: name/description from SKILL.md frontmatter,
    -- directory from the archive's single top-level entry.
    name        text NOT NULL,
    description text NOT NULL,
    directory   text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (skill_id, version)
);
