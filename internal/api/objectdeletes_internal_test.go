package api

import (
	"testing"
	"time"
)

// TestObjectDeleteIntervalIsTheDocumentedCadence is memoryretention's twin
// again. Every rung that runs the loop either overrides the interval — one that
// waited out a minute is one nobody runs — or, in the single rung that keeps
// production's value, needs only that it outlast the half second that rung
// waits, so a second and an hour both pass the suite. docs/ARCHITECTURE.md
// publishes a minute as the backstop cadence an operator reads, and the literal
// here is deliberate: comparing against the var would move both sides together
// and pin nothing.
func TestObjectDeleteIntervalIsTheDocumentedCadence(t *testing.T) {
	if objectDeleteInterval != time.Minute {
		t.Errorf("objectDeleteInterval = %s, want 1m — the cadence ARCHITECTURE.md publishes", objectDeleteInterval)
	}
}
