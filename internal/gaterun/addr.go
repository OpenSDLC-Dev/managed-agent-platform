package gaterun

import (
	"fmt"
	"net"
)

// DefaultProxyAddr is the loopback address the gate's egress proxy listens on,
// and the address the sandbox reaches it at — the two share a network namespace,
// so the sandbox's localhost is the gate's. It is the one contract between the
// two ends: cmd/gate binds GATE_ADDR here by default, and the executor points the
// sandbox's HTTP(S)_PROXY at "http://" + this, so a single constant keeps the
// listener and the client from drifting apart.
const DefaultProxyAddr = "127.0.0.1:15080"

// CheckLoopbackListenAddr rejects a proxy listen address whose host is not
// loopback. The gate's forward proxy is unauthenticated — its only protection is
// that only the co-resident sandbox, sharing its network namespace, can reach it
// over loopback. Binding it to a routable address (":15080" binds every
// interface, "0.0.0.0:15080", a LAN IP) would expose a credential-substituting
// egress proxy to whatever else can route to that address, so the gate refuses
// to start on one. Only a loopback IP (127.0.0.0/8, ::1) or the literal
// "localhost" is accepted.
func CheckLoopbackListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("gate listen address %q is not host:port: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("gate listen address host %q is not loopback — the proxy is unauthenticated and must not be reachable off-host", host)
	}
	return nil
}
