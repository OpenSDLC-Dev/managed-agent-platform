package modelgateway

import "time"

// SetWriteStall shortens the bound on a write to the caller for one test, and
// returns its restore.
func SetWriteStall(d time.Duration) func() {
	old := writeStall
	writeStall = d
	return func() { writeStall = old }
}

// SetMaxResponseBody lowers the bound on an upstream's body for one test, and
// returns its restore.
func SetMaxResponseBody(n int) func() {
	old := maxResponseBody
	maxResponseBody = n
	return func() { maxResponseBody = old }
}
