package toolset

import (
	"io"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/ripgrep"
)

// WithoutRipgrep plays, for the rest of t, a build that fetched no ripgrep
// archives: what `go build` makes from a fresh checkout without `make
// ripgrep`.
func WithoutRipgrep(t *testing.T) {
	t.Helper()
	was := openRipgrep
	openRipgrep = func(string) (io.Reader, int64, error) { return nil, 0, ripgrep.ErrNotEmbedded }
	t.Cleanup(func() { openRipgrep = was })
}

// RipgrepPath is where grep installs rg in a sandbox.
var RipgrepPath = ripgrepPath

// LinuxArch is the uname-to-GOARCH mapping grep picks a binary by.
var LinuxArch = linuxArch

// PagerReader is the paging stage that tells an offset that cut every line
// rg printed from rg printing none.
const PagerReader = pagerReader
