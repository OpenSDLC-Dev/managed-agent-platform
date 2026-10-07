package store

// SweepLock is the advisory lock a sweep holds.
var SweepLock = sweepLock

// SetSweepBatch sets the rows one sweep transaction deletes, restoring the
// old value when the returned function runs.
func SetSweepBatch(n int64) func() {
	old := sweepBatch
	sweepBatch = n
	return func() { sweepBatch = old }
}
