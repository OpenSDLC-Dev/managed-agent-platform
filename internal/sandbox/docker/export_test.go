package docker

// DaemonHostForTest is the address this package resolves for an empty
// Config.Host — what a test must give the `docker` CLI so it cannot follow a
// `docker context` to a daemon the provider is not using (#627). Test binary
// only.
func DaemonHostForTest() string { return daemonHost("") }
