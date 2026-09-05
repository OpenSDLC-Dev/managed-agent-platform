package egress_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// The set is exactly what a recording sized. Asserting the whole list rather
// than membership is the point: an entry added because its name looked obvious
// must fail this, not pass alongside the evidenced ones. Both directions matter
// — a guessed entry widens a `limited` sandbox past the reference, and a dropped
// one breaks a build the reference would have let through (#594).
func TestPackageRegistryHostsAreWhatTheRecordingSized(t *testing.T) {
	want := []string{
		"pypi.org", "files.pythonhosted.org",
		"registry.npmjs.org", "registry.yarnpkg.com", "nodejs.org",
		"crates.io", "index.crates.io", "static.crates.io",
		"rubygems.org", "index.rubygems.org",
		"proxy.golang.org", "sum.golang.org",
		"archive.ubuntu.com", "security.ubuntu.com", "ppa.launchpad.net",
		"packagist.org",
		"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org",
		"github.com", "api.github.com", "raw.githubusercontent.com",
		"codeload.github.com", "objects.githubusercontent.com",
		"gitlab.com", "bitbucket.org",
		"ghcr.io", "registry-1.docker.io", "auth.docker.io", "download.docker.com",
	}
	if got := egress.PackageRegistryHosts(); !slices.Equal(got, want) {
		t.Errorf("PackageRegistryHosts() = %q, want exactly %q", got, want)
	}
}

// The recording found no suffix rule: it refused `pythonhosted.org`,
// `test.pypi.org`, `npmjs.org` and `golang.org` while admitting their siblings.
// So every entry is a literal host, and a `*.` entry — which the operator's own
// allowed_hosts grammar does accept, and which HostSet would honour here just as
// happily — would silently open a family nobody probed.
func TestNoPackageRegistryEntryIsAWildcard(t *testing.T) {
	for _, host := range egress.PackageRegistryHosts() {
		if strings.ContainsRune(host, '*') {
			t.Errorf("%q is a suffix rule, but the set is matched by exact host (#594)", host)
		}
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
