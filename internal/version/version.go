// Package version identifies the platform build in each process and browser session.
package version

import (
	"regexp"
	"strings"
)

// Version is supplied at link time. An unstamped build reports "unknown".
var Version string

var buildIdentifier = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-dev-[0-9a-f]{8})?$`)

func init() {
	Version = linkedVersion(Version)
}

func linkedVersion(stamp string) string {
	// The release Make targets and the public Docker build argument use X.Y.Z.
	if !strings.Contains(stamp, "-") {
		stamp = "v" + strings.TrimPrefix(stamp, "v")
	}
	if buildIdentifier.MatchString(stamp) {
		return stamp
	}
	return "unknown"
}
