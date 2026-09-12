package api

import (
	"testing"
	"time"
)

// TestObjectDeleteIntervalIsTheDocumentedCadence is memoryretention's twin
// again, for a reason of the drain's own: every rung that runs the loop drives
// it by wake rather than by tick, precisely so none of them has to wait out a
// minute, which leaves the production value pinned by nothing.
// docs/ARCHITECTURE.md publishes it as the backstop cadence an operator reads.
func TestObjectDeleteIntervalIsTheDocumentedCadence(t *testing.T) {
	if objectDeleteInterval != time.Minute {
		t.Errorf("objectDeleteInterval = %s, want 1m — the cadence ARCHITECTURE.md publishes", objectDeleteInterval)
	}
}
