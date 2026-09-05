package egress

import "slices"

// packageRegistryHosts is the curated set `limited` networking's
// allow_package_managers opens, beyond the operator's own allowed_hosts. It is
// grouped by ecosystem, which is also how it grows.
//
// Every entry was observed admitted on a `limited` environment with
// `allowed_hosts: []` and the flag on, and observed refused on the same
// environment with the flag off — so no host here is attributed to the flag
// without its own control. Matching is by **exact host**: the same recording
// refused `test-files.pythonhosted.org`, `test.pypi.org`, `pythonhosted.org`,
// `npmjs.org`, `www.npmjs.com`, `registry.npmjs.com` and `golang.org` while
// admitting their siblings, so there is no suffix rule to model and a HostSet
// of plain names is the right shape.
//
// **Read the list before assuming what the flag means.** Two of its groups are
// not package registries at all: it opens source forges — `github.com` included
// — and container registries. Neither is suggested by the flag's name or by the
// reference's own wording ("public package registries (such as PyPI and npm)"),
// and an operator reasoning about a `limited` sandbox's reach needs to know it.
// docs/self-hosted-security.md says so where an operator will read it.
//
// The list also does not track `config.packages`' six managers: Composer and
// Maven are open though neither is one of them, while apt reaches Ubuntu's
// archives and PPAs alone: Debian's three mirrors are refused, and so are five
// more of Ubuntu's own. And the
// widening is independent of what `config.packages` declares: across the
// seventeen hosts that arm probed, an environment declaring
// `npm: ["left-pad"]` admitted and refused exactly what one declaring
// nothing did.
//
// **Thirty is a lower bound, not the reference's list.** Eighty hosts were
// probed; these thirty answered. NuGet, Dart, Hex, CocoaPods, conda, Alpine,
// CRAN, CPAN, jsDelivr, unpkg and SourceForge were probed and refused, so they
// are absent by evidence rather than by omission — but a host nobody has probed
// is simply unknown, and stays out. Guessing one would widen a `limited`
// sandbox past the reference on the strength of a name that merely looked
// obvious, which is the one direction this gate must not err in: a host this
// set omits is refused, never leaked.
var packageRegistryHosts = []string{
	// Python — the index and the wheel CDN.
	"pypi.org",
	"files.pythonhosted.org",

	// npm and Node — the two registries and the runtime's own download host.
	"registry.npmjs.org",
	"registry.yarnpkg.com",
	"nodejs.org",

	// Rust — the API, the sparse index, and the crate CDN.
	"crates.io",
	"index.crates.io",
	"static.crates.io",

	// Ruby — the site and the compact index.
	"rubygems.org",
	"index.rubygems.org",

	// Go — the module proxy and the checksum database.
	"proxy.golang.org",
	"sum.golang.org",

	// apt — Ubuntu only, and only these three. Debian's deb.debian.org,
	// ftp.debian.org and cdn-aws.deb.debian.org are refused, as are
	// ports.ubuntu.com, azure.archive.ubuntu.com, esm.ubuntu.com,
	// changelogs.ubuntu.com and keyserver.ubuntu.com.
	"archive.ubuntu.com",
	"security.ubuntu.com",
	"ppa.launchpad.net",

	// PHP — Composer's registry, though `packages` names no PHP manager.
	"packagist.org",

	// Java — Maven Central by both its names, and the Gradle plugin portal.
	// `packages` names no Java manager either. search.maven.org and
	// oss.sonatype.org are refused.
	"repo.maven.apache.org",
	"repo1.maven.org",
	"plugins.gradle.org",

	// Source forges. Not package registries, and open anyway — this is the
	// half of the flag its name does not describe. gist.githubusercontent.com
	// and pkg-containers.githubusercontent.com are refused, so this is a list
	// of hosts and not of GitHub.
	"github.com",
	"api.github.com",
	"raw.githubusercontent.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"gitlab.com",
	"bitbucket.org",

	// Container registries — the other half the name does not describe.
	// index.docker.io is refused where registry-1.docker.io, the host a client
	// is redirected to, is admitted.
	"ghcr.io",
	"registry-1.docker.io",
	"auth.docker.io",
	"download.docker.com",
}

// PackageRegistryHosts returns that set, as a fresh slice so no caller can grow
// the shared one by appending to it.
//
// It lives in this package rather than in internal/gate because two callers ask
// the same question and must never disagree: the gate admits these hosts, and
// the control plane's credential-conflict detection has to be told exactly what
// the gate is told or it reports a credential reachable on one of them as
// permanently unreachable (internal/api/gateconfig.go).
func PackageRegistryHosts() []string {
	return slices.Clone(packageRegistryHosts)
}
