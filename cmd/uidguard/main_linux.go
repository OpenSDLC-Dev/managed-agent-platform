// Command uidguard is the static first executable in a gated sandbox.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun/uidguard"
)

func main() {
	if os.Geteuid() == uidguard.GateUID {
		fmt.Fprintf(os.Stderr, "sandbox: reserved gate UID %d\n", uidguard.GateUID)
		os.Exit(uidguard.Refused)
	}
	if len(os.Args) == 1 {
		return
	}
	argv := os.Args[1:]
	path, err := exec.LookPath(argv[0])
	if err == nil {
		err = syscall.Exec(path, argv, os.Environ())
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(126)
}
