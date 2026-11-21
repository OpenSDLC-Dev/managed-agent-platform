// Package version carries the build-time version stamp shared by every
// binary. The release pipeline injects it with
//
//	-ldflags "-X github.com/OpenSDLC-Dev/managed-agent-platform/internal/version.Version=X.Y.Z"
//
// and an uninjected build reports "dev". The control plane includes the stamp
// in its authenticated browser session response.
package version

// Version is the platform release version, or "dev" when not injected.
var Version = "dev"
