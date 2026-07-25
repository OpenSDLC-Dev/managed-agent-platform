package gaterun_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
)

func TestCheckLoopbackListenAddr(t *testing.T) {
	ok := []string{"127.0.0.1:15080", "127.0.0.5:1", "[::1]:15080", "localhost:15080"}
	for _, addr := range ok {
		if err := gaterun.CheckLoopbackListenAddr(addr); err != nil {
			t.Errorf("CheckLoopbackListenAddr(%q) = %v, want nil", addr, err)
		}
	}

	bad := []string{
		":15080",         // empty host binds every interface
		"0.0.0.0:15080",  // all IPv4 interfaces
		"[::]:15080",     // all IPv6 interfaces
		"10.0.0.5:15080", // a routable LAN address
		"example.com:80", // a non-loopback name
		"127.0.0.1",      // missing port
		"not-an-address", // unparseable
	}
	for _, addr := range bad {
		if err := gaterun.CheckLoopbackListenAddr(addr); err == nil {
			t.Errorf("CheckLoopbackListenAddr(%q) = nil, want a non-loopback error", addr)
		}
	}
}
