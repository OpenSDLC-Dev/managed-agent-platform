package mcp

// SameHostForTest exposes the origin comparison the bearer-token transport
// uses. It is reachable through Connect only via a redirect between two hosts
// the caller's client will actually dial, which cannot express the case that
// matters most here — a scoped IPv6 zone identifier, whose case is locally
// significant and is the one way the comparison could match two origins that
// are not the same one.
var SameHostForTest = sameHost
