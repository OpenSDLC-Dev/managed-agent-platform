package main

import (
	"fmt"
	"syscall"
)

// privDropper drops the process to the gate's dedicated uid/gid after the
// firewall is applied. Order is load-bearing: supplementary groups and the gid
// are dropped before the uid, because once the uid is no longer 0 the process
// can no longer change its groups. After this the gate cannot alter iptables
// (no CAP_NET_ADMIN), and every socket it opens carries uid — the identity the
// owner-match OUTPUT rule accepts. Since Go 1.16 these syscalls apply to the
// whole process, not just the calling thread.
type privDropper struct{ uid, gid int }

func (d privDropper) Drop() error {
	if err := syscall.Setgroups([]int{d.gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(d.gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(d.uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	return nil
}
