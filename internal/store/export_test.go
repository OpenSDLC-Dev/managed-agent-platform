package store

import "time"

// MigrateThrough exposes the migrate-through seam to the external tests.
var MigrateThrough = migrate

// SetMigrateRetryForTest shortens the migration retry schedule so a test can
// run it to exhaustion in test time. Test binary only.
func SetMigrateRetryForTest(attempts int, backoff time.Duration) (restore func()) {
	prevAttempts, prevBackoff := migrateAttempts, migrateBackoff
	migrateAttempts, migrateBackoff = attempts, backoff
	return func() { migrateAttempts, migrateBackoff = prevAttempts, prevBackoff }
}
