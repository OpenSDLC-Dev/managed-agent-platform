---
trigger: glob
globs: "internal/store/migrations/**"
---

A merged migration is immutable. Put new DDL in the next numbered file and check upgrade behavior against data from the previous schema. The single-transaction migrator governs how the change can run. Apply the migration checks in the base branch's `REVIEW.md`.
