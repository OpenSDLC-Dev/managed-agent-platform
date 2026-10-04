package docker_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/hookedtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// An image whose startup file turns errexit on (and prints, as
// sandboxtest.BannerHook does) runs it in the exec wrapper's own shell, and the
// watchdog it forks inherits it (#860). There a `mkdir` of the mark that fails —
// a tenant who planted a file at the mark's path, read from the watchdog's argv —
// used to end the watchdog before its `kill -9`: the deadline was still called
// from outside, but the runaway ran on. On that image as on the plain one, a
// runaway past its deadline is killed — its process gone, not only reported —
// with its mark blocked or not; and a command's own exit 7 and its own SIGKILL
// stand, neither a timeout.
func TestDockerExecUnderAnErrexitStartup(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	// The mark's path is the wrapper's last argument; the watchdog, forked
	// before the wrapper became the command, still carries it.
	const blockTheMark = `
	  for p in $(cat /proc/$$/task/$$/children 2>/dev/null); do
	    s=$(tr '\0' '\n' < /proc/$p/cmdline 2>/dev/null | tail -n 1)
	    case $s in */.map-exec-*) : > "$s.killed" && echo planted ;; esac
	  done
	`
	for _, image := range []struct{ name, image string }{
		{"plain", testImage},
		{"errexit startup", hookedtest.DockerImage(t, "set -e\n"+sandboxtest.BannerHook)},
	} {
		t.Run(image.name, func(t *testing.T) {
			sb, err := provider.Provision(context.Background(), sandbox.Spec{
				SessionID:  domain.NewID("sesn"),
				Image:      image.image,
				Networking: domain.Networking{Type: domain.NetUnrestricted},
			})
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for _, tc := range []struct {
				name, command, marker string
				timeout               time.Duration
				code                  int
				timedOut              bool
			}{
				{"a failing command's own exit", "exit 7", "", 30 * time.Second, 7, false},
				{"a SIGKILL the command sent itself", "kill -9 $$", "", 30 * time.Second, 137, false},
				{"a runaway past its deadline", "sleep 987611", "sleep 987611", time.Second, 137, true},
				{"a runaway whose mark is blocked", blockTheMark + "sleep 987612", "sleep 987612", 2 * time.Second, 137, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: tc.command, Timeout: tc.timeout})
					if err != nil {
						t.Fatalf("exec: %v", err)
					}
					if res.ExitCode != tc.code || res.TimedOut != tc.timedOut {
						t.Errorf("exit %d, timed out %v; want %d, %v: %+v", res.ExitCode, res.TimedOut, tc.code, tc.timedOut, res)
					}
					if strings.HasPrefix(tc.command, blockTheMark) && !strings.Contains(res.Stdout, "planted") {
						t.Fatalf("the command found no mark path to block, so this row proves nothing: %+v", res)
					}
					if tc.marker == "" {
						return
					}
					// The kill is the watchdog's, and lands at the deadline; the
					// count waits out the reaping rather than racing it.
					var n int
					for range 50 {
						if n = runningMarkers(t, sb, tc.marker); n == 0 {
							return
						}
						time.Sleep(100 * time.Millisecond)
					}
					t.Errorf("%d %q process(es) outlived the deadline: the runaway was reported, not killed", n, tc.marker)
				})
			}
		})
	}
}

// runningMarkers counts the sandbox's processes whose command line starts with
// marker, read from /proc as sandboxtest's countProcesses does, with the hooked
// image's banner taken out of the answer.
func runningMarkers(t *testing.T, sb sandbox.Sandbox, marker string) int {
	t.Helper()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Timeout: 30 * time.Second, Command: `
		n=0
		for p in /proc/[0-9]*; do
		  [ -r "$p/cmdline" ] || continue
		  case "$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)" in
		    "` + marker + `"*) n=$((n+1)) ;;
		  esac
		done
		echo "$n"`})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("count processes: %+v, %v", res, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(sandboxtest.Unbanner(res.Stdout)))
	if err != nil {
		t.Fatalf("count processes: %q: %v", res.Stdout, err)
	}
	return n
}
