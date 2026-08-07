// Package version carries the build-time version stamp shared by every
// binary. The release pipeline injects it with
//
//	-ldflags "-X github.com/OpenSDLC-Dev/managed-agent-platform/internal/version.Version=X.Y.Z"
//
// and an uninjected build reports "dev". A bare variable on purpose: no
// statements means nothing for the coverage gate to count, and there is no
// version API endpoint — that would be net-new wire surface (plan 27
// decision 3).
package version

// Version is the platform release version, or "dev" when not injected.
var Version = "dev"
