// Package version identifies the platform build in each process and browser session.
package version

import (
	"runtime/debug"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Version is set at link time for releases and container builds. Plain go build
// records the main module version and the Git revision in the executable.
var Version string

func init() {
	if Version != "" {
		Version = linkedVersion(Version)
		return
	}
	info, _ := debug.ReadBuildInfo()
	Version = goBuildVersion(info)
}

func stableTag(v string) bool {
	return v != "" && semver.Canonical(v) == v && semver.Prerelease(v) == ""
}

func linkedVersion(stamp string) string {
	// The release Make targets and the public Docker build argument use X.Y.Z.
	if stableTag("v" + stamp) {
		return "v" + stamp
	}
	base, revision, dev := strings.Cut(stamp, "-dev-")
	if !stableTag(base) || dev && (len(revision) != 8 || strings.Trim(revision, "0123456789abcdef") != "") {
		return "unknown"
	}
	return stamp
}

func goBuildVersion(info *debug.BuildInfo) string {
	if info == nil {
		return "unknown"
	}
	var vcs, revision string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs":
			vcs = setting.Value
		case "vcs.revision":
			revision = setting.Value
		}
	}
	base := strings.TrimSuffix(info.Main.Version, "+dirty")
	if module.IsPseudoVersion(base) {
		base, _ = module.PseudoVersionBase(base)
	}
	if vcs != "git" || len(revision) < 8 {
		return "unknown"
	}
	return linkedVersion(base + "-dev-" + revision[:8])
}
