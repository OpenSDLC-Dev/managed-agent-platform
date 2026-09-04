package egress

import "slices"

// packageRegistryHosts is the curated set `limited` networking's
// allow_package_managers opens, beyond the operator's own allowed_hosts. It is
// grouped by ecosystem, which is also how it grows: a new ecosystem is a new
// group, once a recording has sized it (#594).
//
// Two entries, because two is what any evidence names. The reference publishes
// no list — its docs and the pinned SDK say only "public package registries
// (PyPI, npm, etc.)" — and the one recording of the flag probed three URLs:
// under `{"type":"limited","allowed_hosts":[]}` the flag turned `pypi.org` and
// `files.pythonhosted.org` from 403 into 200, while an `example.com` control
// stayed 403 either way. npm, cargo, gem, go and apt were never probed, so
// their registry hosts are absent rather than chosen.
//
// Guessing them would widen a `limited` sandbox past the reference on the
// strength of a hostname that merely looked obvious, which is the one direction
// this gate must not err in: a host this set omits is refused, never leaked.
var packageRegistryHosts = []string{
	// Python — the index and the wheel CDN (recorded 2026-09-03).
	"pypi.org",
	"files.pythonhosted.org",
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
