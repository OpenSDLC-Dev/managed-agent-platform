package pgtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerDiagWithFakeDocker(t *testing.T) {
	fakeDir := t.TempDir()
	fake := `#!/bin/sh
if [ "$PGTEST_DIAG_SCENARIO" = unavailable ]; then
	printf '%s unavailable\n' "$1" >&2
	exit 2
fi
status=exited
running=false
exit_code=137
oom_killed=true
if [ "$PGTEST_DIAG_SCENARIO" = recovery ]; then
	status=running
	running=true
	exit_code=0
	oom_killed=false
fi
case "$1" in
inspect)
	[ "$#" -eq 4 ] && [ "$2" = -f ] && [ "$4" = pgtest-unit-owned ] || exit 64
	case "$3" in
	'{{.State.Status}}') printf '%s\n' "$status" ;;
	'status={{.State.Status}} running={{.State.Running}} exit_code={{.State.ExitCode}} oom_killed={{.State.OOMKilled}} started_at={{.State.StartedAt}} finished_at={{.State.FinishedAt}} error={{json .State.Error}}')
		printf 'status=%s running=%s exit_code=%s oom_killed=%s started_at=2026-11-01T00:00:00Z finished_at=2026-11-01T00:01:00Z error=""\n' "$status" "$running" "$exit_code" "$oom_killed"
		;;
	*) printf 'unexpected inspect projection\n' >&2; exit 64 ;;
	esac
	;;
logs)
	if [ "$#" -eq 4 ] && [ "$2" = --tail ] && [ "$3" = 3 ] && [ "$4" = pgtest-unit-owned ]; then
		printf 'FATAL: database system is in recovery mode\nFATAL: database system is in recovery mode\nFATAL: database system is in recovery mode\n'
	elif [ "$#" -eq 5 ] && [ "$2" = --timestamps ] && [ "$3" = --tail ] && [ "$4" = 200 ] && [ "$5" = pgtest-unit-owned ]; then
		if [ "$PGTEST_DIAG_SCENARIO" = oversized ]; then
			printf 'discarded-old-prefix\n'
			chunk=0123456789abcdef
			for n in 1 2 3 4 5 6; do chunk="$chunk$chunk"; done
			i=0
			while [ "$i" -lt 128 ]; do
				printf '%s\n' "$chunk"
				i=$((i + 1))
			done
			printf 'recent-log-tail-end\n'
		else
			printf '2026-11-01T00:00:59Z server process was terminated by signal 9\n'
			i=0
			while [ "$i" -lt 10 ]; do
				printf '2026-11-01T00:01:00Z FATAL: database system is in recovery mode\n'
				i=$((i + 1))
			done
		fi
	else
		printf 'unexpected logs arguments\n' >&2
		exit 64
	fi
	;;
*) printf 'unexpected Docker operation\n' >&2; exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeDir, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// No inherited PATH: an absent fake cannot resolve to the host's Docker.
	t.Setenv("PATH", fakeDir)
	for _, tc := range []struct {
		name, scenario string
		want           []string
	}{
		{"exited-oom", "exited", []string{"status=exited", "running=false", "exit_code=137", "oom_killed=true", "started_at=2026-11-01T00:00:00Z", "finished_at=2026-11-01T00:01:00Z", "error=\"\"", "server process was terminated by signal 9", "database system is in recovery mode"}},
		{"running-recovery", "recovery", []string{"status=running", "running=true", "exit_code=0", "oom_killed=false", "server process was terminated by signal 9", "database system is in recovery mode"}},
		{"unavailable", "unavailable", []string{"inspect unavailable", "logs unavailable", "exit status 2"}},
		{"bounded-tail", "oversized", []string{"status=exited", "recent-log-tail-end"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PGTEST_DIAG_SCENARIO", tc.scenario)
			got := containerDiag("pgtest-unit-owned")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic missing %q: %.2000s", want, got)
				}
			}
			if tc.scenario == "oversized" {
				if len(got) > 66*1024 || strings.Contains(got, "discarded-old-prefix") {
					t.Errorf("diagnostic did not retain a bounded recent tail: bytes=%d", len(got))
				}
			}
		})
	}
}
