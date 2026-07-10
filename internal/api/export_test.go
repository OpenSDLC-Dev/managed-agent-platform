package api

import "time"

// SetPingIntervalForTest shortens the SSE keepalive cadence so contract tests
// can observe ping frames without real-time waits. Test binary only.
func SetPingIntervalForTest(d time.Duration) (restore func()) {
	prev := ssePingInterval
	ssePingInterval = d
	return func() { ssePingInterval = prev }
}
