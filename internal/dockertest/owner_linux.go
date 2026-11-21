//go:build linux

package dockertest

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procStartTicks reads field 22 (starttime in clock ticks since boot) from a
// Linux /proc/<pid>/stat file. Field 2 (comm) is wrapped in parentheses and
// may contain spaces or parentheses itself, so parsing anchors on the final ')'.
func procStartTicks(statContent string) (string, bool) {
	idx := strings.LastIndexByte(statContent, ')')
	if idx < 0 || idx+2 >= len(statContent) {
		return "", false
	}
	fields := strings.Fields(statContent[idx+2:])
	// Field 3 (state) is fields[0]; field 22 (starttime) is fields[19].
	if len(fields) <= 19 {
		return "", false
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", false
	}
	return fields[19], true
}

func readLinuxOwnerContext() (bootID, pidNS string, ok bool) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", false
	}
	bootID = strings.TrimSpace(string(b))
	pidNS, err = os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", "", false
	}
	pidNS = strings.TrimSpace(pidNS)
	if bootID == "" || pidNS == "" || strings.ContainsAny(bootID+pidNS, "| \t\r\n") {
		return "", "", false
	}
	return bootID, pidNS, true
}

// currentOwnerToken encodes the owning test binary's kernel identity on Linux:
// "<boot_id>|<pid_ns>|<pid>|<starttime_ticks>". Returning "" disables
// dead-owner reaping for the container and falls back to strayAge.
func currentOwnerToken() string {
	bootID, pidNS, ok := readLinuxOwnerContext()
	if !ok {
		return ""
	}
	pid := os.Getpid()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	ticks, ok := procStartTicks(string(raw))
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d|%s", bootID, pidNS, pid, ticks)
}

// isOwnerDead reports whether token proves its owning process has exited. It
// returns false whenever liveness cannot be established safely (malformed
// token, different PID namespace, unreadable /proc), leaving strayAge as the
// conservative fallback.
func isOwnerDead(token string) bool {
	if token == "" {
		return false
	}
	parts := strings.Split(token, "|")
	if len(parts) != 4 {
		return false
	}
	tokBoot, tokNS, tokPIDStr, tokTicks := parts[0], parts[1], parts[2], parts[3]
	pid, err := strconv.Atoi(tokPIDStr)
	if err != nil || pid <= 0 {
		return false
	}
	if _, err := strconv.ParseUint(tokTicks, 10, 64); err != nil {
		return false
	}
	curBoot, curNS, ok := readLinuxOwnerContext()
	if !ok {
		return false
	}
	// Require BOTH the same random per-boot UUID and the same PID namespace so
	// we only inspect /proc when the container was started on this exact host
	// boot and PID namespace. Two distinct Linux hosts sharing a remote Docker
	// daemon often share the initial PID namespace inode (pid:[4026531836]) with
	// different boot IDs, so a boot mismatch must fall back to strayAge rather
	// than assuming a reboot.
	if tokBoot != curBoot || tokNS != curNS {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	curTicks, ok := procStartTicks(string(raw))
	if !ok {
		return false
	}
	// A different starttime at the same PID proves PID reuse after the owner died.
	return curTicks != tokTicks
}
