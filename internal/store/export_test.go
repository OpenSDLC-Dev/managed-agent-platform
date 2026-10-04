package store

import "time"

// PendingObjectDeleteInsertSQL exposes EnqueueObjectDeletes' statement, so
// the external tests can run it in a transaction BeginObjectDelete did not
// begin, as nothing outside this package can.
const PendingObjectDeleteInsertSQL = pendingObjectDeleteInsertSQL

// MigrateThrough exposes the migrate-through seam to the external tests.
var MigrateThrough = migrate

// SetMigrateRetryForTest shortens the migration retry schedule so a test can
// run it to exhaustion in test time. Test binary only.
func SetMigrateRetryForTest(attempts int, backoff time.Duration) (restore func()) {
	prevAttempts, prevBackoff := migrateAttempts, migrateBackoff
	migrateAttempts, migrateBackoff = attempts, backoff
	return func() { migrateAttempts, migrateBackoff = prevAttempts, prevBackoff }
}

// MigrateLockID exposes the migrators' advisory lock, so a test can hold it
// as another binary's migration run would.
const MigrateLockID = migrateLockID

// SetMigrateLockWaitForTest changes the lock_timeout every migration starts
// with. Test binary only.
func SetMigrateLockWaitForTest(d time.Duration) (restore func()) {
	prev := migrateLockWait
	migrateLockWait = d
	return func() { migrateLockWait = prev }
}
