-- A memory store's first archive moves updated_at to archived_at (#685), as
-- the reference's did on both recorded stores, so 0028's comment on the column
-- no longer names everything that advances it. The archive route only fixes
-- stores archived from here on. A store archived earlier kept the updated_at
-- of its last name, description or metadata change, and without this one
-- deployment renders two behaviors.
--
-- archived_at is exactly the value the route would have written. After a
-- store is created, the only writers of updated_at are its update and its
-- archive (internal/api/memorystores.go), and an archived store refuses every
-- update with a 400, so nothing can have moved the column since the archive.
--
-- updated_at < archived_at keeps this off the rows that already agree, the
-- stores a build carrying #685 archived, so they are not rewritten inside the
-- one transaction migrate.go applies every pending migration in.
--
-- A rolling upgrade can still leave a store behind. A replica on an earlier
-- build that archives a store after this has run leaves updated_at where it
-- was, and a recorded migration does not run twice, so that store keeps the
-- earlier value.
UPDATE memory_stores
   SET updated_at = archived_at
 WHERE archived_at IS NOT NULL AND updated_at < archived_at;
