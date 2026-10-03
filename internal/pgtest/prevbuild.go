package pgtest

// The previous build's statements, verbatim from origin/main before #578
// (0ae74169). During a rolling upgrade, and after a rollback, that build runs
// them against migration 0046's schema; internal/store's and internal/api's
// tests run them from here, so the two suites cannot remember it differently.
// Each is a frozen copy rather than a reference to this build's statement, so
// a later change to this build's SQL leaves what that build runs alone.
const (
	// internal/executor's settleHarvest, replacing a session's snapshot.
	PrevHarvestDeleteSQL = `DELETE FROM files WHERE scope_type = 'session' AND scope_id = $1 RETURNING id`
	PrevHarvestInsertSQL = `INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
				 VALUES ($1, $2, $3, $4, true, 'session', $5)`
	// internal/api's deleteFile, which answers 404 when it deleted no row.
	PrevFileDeleteSQL = `DELETE FROM files WHERE id = $1`
	// internal/api's purgeExpiredFiles.
	PrevPurgeSQL = `
		DELETE FROM files
		 WHERE id IN (SELECT f.id FROM files f
		               WHERE f.expires_at < now() - make_interval(secs => $1)
		                 AND (f.dream_id IS NULL
		                      OR NOT EXISTS (SELECT 1 FROM dreams d
		                                      WHERE d.id = f.dream_id AND d.closed_at IS NULL))
		               ORDER BY f.expires_at, f.id
		               LIMIT $2
		               FOR UPDATE SKIP LOCKED)
		 RETURNING id`
	// internal/api's enqueueDreamBlobs, which a dream's close runs: its
	// transcripts, in scan order.
	PrevDreamFilesDeleteSQL = `DELETE FROM files WHERE dream_id = $1 RETURNING id`
	// internal/api's deleteSkill, in its order: the parent lock, the versions
	// and the skill, then each version's archive enqueued.
	PrevSkillLockSQL           = `SELECT source FROM skills WHERE id = $1 FOR UPDATE`
	PrevSkillVersionsDeleteSQL = `DELETE FROM skill_versions WHERE skill_id = $1 RETURNING version`
	PrevSkillDeleteSQL         = `DELETE FROM skills WHERE id = $1`
	// That build's store.SessionTombstoneInsertSQL, which its deleteSession
	// ran.
	PrevSessionTombstoneInsertSQL = `INSERT INTO deleted_sessions (id, environment_kind)
	 SELECT s.id, e.kind FROM sessions s JOIN environments e ON e.id = s.environment_id
	 WHERE s.id = $1
	 ON CONFLICT (id) DO NOTHING`
	// internal/api's deleteSession, after PrevSessionDeleteSQL: every
	// session-scoped files row, in scan order. That build spelled it as its
	// harvest did, character for character, so it is the one constant.
	PrevSessionFilesDeleteSQL = PrevHarvestDeleteSQL
	// That build's store.PendingObjectDeleteInsertSQL, which each of its
	// removers enqueued through: its keys unsorted, a duplicate offered twice.
	PrevObjectEnqueueSQL = `INSERT INTO pending_object_deletes (object_key)
	 SELECT unnest($1::text[])
	 ON CONFLICT (object_key) DO NOTHING`
)

// PrevSessionDeleteSQL is that build's session delete up to its files, in its
// order, each statement taking the session id: requireNotRunning's lock, the
// tombstone, and the session and checkpoint rows, its reads and its NOTIFY
// left out. PrevSessionFilesDeleteSQL follows, and then the checkpoint's key
// and each deleted row's are enqueued.
var PrevSessionDeleteSQL = []string{
	`SELECT status FROM sessions WHERE id = $1 FOR UPDATE`,
	PrevSessionTombstoneInsertSQL,
	`DELETE FROM sessions WHERE id = $1`,
	`DELETE FROM session_checkpoints WHERE session_id = $1`,
}
