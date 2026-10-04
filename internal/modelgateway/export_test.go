package modelgateway

import (
	"net/http"
	"slices"
	"time"
)

// OpenedKeys returns the ids of the credentials whose keys the handler holds
// opened, in order.
func OpenedKeys(h http.Handler) []string {
	g := h.(*handler)
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.opened))
	for id := range g.opened {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

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
