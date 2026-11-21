//go:build !linux

package dockertest

// currentOwnerToken returns empty on non-Linux hosts where /proc boot_id, PID
// namespace, and process start ticks are unavailable; SweepStrays falls back to
// strayAge rather than guessing process liveness.
func currentOwnerToken() string { return "" }

func isOwnerDead(string) bool { return false }
