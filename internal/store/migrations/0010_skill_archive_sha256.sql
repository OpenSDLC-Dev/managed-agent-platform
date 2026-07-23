-- 0010_skill_archive_sha256: end-to-end integrity for skill archives
-- (docs/plan/09_skill-archive-integrity.md, issue #155).
--
-- The archive bytes live in object storage while the metadata lives here; two
-- stores with different operators and failure modes. Recording the upload's
-- digest in this one lets materialization prove the object it read back is the
-- archive that was validated at upload — storage bit-rot, truncation, or a
-- substituted object is otherwise invisible (zip's per-member CRC-32 is
-- non-cryptographic and a substituted valid zip passes it).
--
-- Nullable on purpose: the bytes a pre-existing row's digest would have to be
-- computed from live in object storage, which a SQL migration cannot read, so
-- NOT NULL would fail on any populated table. Every row written from here on
-- carries one, so NULL means exactly "written before this migration" and
-- materialization reads such a version's archive unverified (logged).
ALTER TABLE skill_versions
    ADD COLUMN sha256 text
    CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$');
