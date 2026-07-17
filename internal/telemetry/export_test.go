package telemetry

import (
	"io"
	"testing"
)

// SetConsoleWriterForTest redirects the log bridge's console half for one test,
// so a test can assert that a bridged record still reaches the console without
// the suite writing to the real stderr.
func SetConsoleWriterForTest(t *testing.T, w io.Writer) {
	t.Helper()
	prev := consoleOut
	consoleOut = w
	t.Cleanup(func() { consoleOut = prev })
}
