package egress_test

import (
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// The set is exactly what a recording sized. The pinned SDK's prose is wider
// than its evidence — "public package registries (PyPI, npm, etc.)" — and the
// only probe of the flag tried three URLs, two Python hosts and a control, so
// an entry beyond those two would be a guess that widens a `limited` sandbox
// past the reference (#594). Asserting the whole list rather than membership is
// the point: a guessed entry must fail this, not merely pass alongside it.
func TestPackageRegistryHostsAreWhatTheRecordingSized(t *testing.T) {
	want := []string{"pypi.org", "files.pythonhosted.org"}
	if got := egress.PackageRegistryHosts(); !slices.Equal(got, want) {
		t.Errorf("PackageRegistryHosts() = %q, want exactly %q", got, want)
	}
}

// Both callers concatenate this list into a set of their own, and one of them
// does it on every gate-config fetch. Handing out the package variable itself
// would let one caller's write reach every later one as a host nobody
// configured.
func TestPackageRegistryHostsHandsOutACopy(t *testing.T) {
	first := egress.PackageRegistryHosts()
	first[0] = "evil.example.com"

	if second := egress.PackageRegistryHosts(); slices.Contains(second, "evil.example.com") {
		t.Errorf("a caller's write reached the shared list: second call = %q", second)
	}
}
