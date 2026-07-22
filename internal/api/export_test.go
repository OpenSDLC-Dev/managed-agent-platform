package api

import "time"

// SetPingIntervalForTest shortens the SSE keepalive cadence so contract tests
// can observe ping frames without real-time waits. Test binary only.
func SetPingIntervalForTest(d time.Duration) (restore func()) {
	prev := ssePingInterval
	ssePingInterval = d
	return func() { ssePingInterval = prev }
}

// SetMaxFileBytesForTest lowers the Files per-file cap so the 413 path can be
// exercised without streaming half a gigabyte through a test. Test binary only.
func SetMaxFileBytesForTest(n int64) (restore func()) {
	prev := maxFileBytes
	maxFileBytes = n
	return func() { maxFileBytes = prev }
}
