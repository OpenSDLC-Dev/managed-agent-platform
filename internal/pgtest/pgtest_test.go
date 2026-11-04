package pgtest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestContainerDiagWithFakeDocker(t *testing.T) {
	fakeDir := t.TempDir()
	fake := `#!/bin/sh
printf '%s\n' "$1" >> "$PGTEST_DIAG_CALL_FILE"
if [ "$PGTEST_DIAG_SCENARIO" = unavailable ] && [ "$1" != exec ]; then
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
	if [ "$PGTEST_DIAG_SCENARIO" = inherited-pipes ]; then
		/bin/sleep 3 &
		printf '%s %s\n' "$$" "$!" > "$PGTEST_DIAG_PID_FILE"
		printf 'inherited-pipe-state\n'
		exit 0
	elif [ "$PGTEST_DIAG_SCENARIO" = inspect-timeout ]; then
		printf '%s\n' "$$" > "$PGTEST_DIAG_PID_FILE"
		printf 'inspect-budget-exhaustion\n'
		exec /bin/sleep 12
	fi
	case "$3" in
	'{{.State.Status}}') printf '%s\n' "$status" ;;
	'status={{.State.Status}} running={{.State.Running}} exit_code={{.State.ExitCode}} oom_killed={{.State.OOMKilled}} started_at={{.State.StartedAt}} finished_at={{.State.FinishedAt}} error={{json .State.Error}} image={{.Config.Image}} image_id={{.Image}} shm_size={{.HostConfig.ShmSize}} ipc_mode={{.HostConfig.IpcMode}}')
		printf 'status=%s running=%s exit_code=%s oom_killed=%s started_at=2026-11-01T00:00:00Z finished_at=2026-11-01T00:01:00Z error="" image=pgtest.invalid/synthetic-postgres:projection-only image_id=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef shm_size=100663297 ipc_mode=shareable\n' "$status" "$running" "$exit_code" "$oom_killed"
		;;
	*) printf 'unexpected inspect projection\n' >&2; exit 64 ;;
	esac
	;;
logs)
	if [ "$#" -eq 4 ] && [ "$2" = --tail ] && [ "$3" = 3 ] && [ "$4" = pgtest-unit-owned ]; then
		printf 'FATAL: database system is in recovery mode\nFATAL: database system is in recovery mode\nFATAL: database system is in recovery mode\n'
	elif [ "$#" -eq 5 ] && [ "$2" = --timestamps ] && [ "$3" = --tail ] && [ "$4" = 200 ] && [ "$5" = pgtest-unit-owned ]; then
		if [ "$PGTEST_DIAG_SCENARIO" = inspect-timeout ]; then
			printf '2026-11-01T00:00:58Z useful-history-after-inspect-timeout\n'
		elif [ "$PGTEST_DIAG_SCENARIO" = oversized ]; then
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
exec)
	[ "$#" -eq 5 ] && [ "$2" = pgtest-unit-owned ] && [ "$3" = sh ] && [ "$4" = -c ] && [ "$5" = "date -u '+sampled_at=%Y-%m-%dT%H:%M:%SZ' && postgres --version && df -Pk /dev/shm" ] || { printf 'unexpected exec arguments\n' >&2; exit 64; }
	if [ "$PGTEST_DIAG_SCENARIO" = unavailable ]; then
		printf 'exec unavailable\n' >&2
		exit 2
	fi
	printf 'sampled_at=2094-07-16T12:34:56Z\n'
	case "$PGTEST_DIAG_SCENARIO" in
	snapshot-version-unavailable)
		printf 'postgres: synthetic version unavailable\n' >&2
		exit 7
		;;
	snapshot-timeout)
		printf '%s\n' "$$" > "$PGTEST_DIAG_PID_FILE"
		printf 'snapshot-budget-exhaustion\n'
		exec /bin/sleep 12
		;;
	snapshot-inherited-pipes)
		/bin/sleep 3 &
		printf '%s %s\n' "$$" "$!" > "$PGTEST_DIAG_PID_FILE"
		printf 'snapshot-inherited-pipe\n'
		exit 0
		;;
	esac
	printf 'postgres (PostgreSQL) 16.73-pgtest-synthetic\n'
	if [ "$PGTEST_DIAG_SCENARIO" = snapshot-space-unavailable ]; then
		printf 'df: synthetic /dev/shm unavailable\n' >&2
		exit 9
	elif [ "$PGTEST_DIAG_SCENARIO" = snapshot-oversized ]; then
		printf 'snapshot-discarded-prefix\n'
		chunk=0123456789abcdef
		for n in 1 2 3 4 5 6; do chunk="$chunk$chunk"; done
		i=0
		while [ "$i" -lt 128 ]; do
			printf '%s\n' "$chunk"
			i=$((i + 1))
		done
		printf 'recent-snapshot-tail-end\n'
	else
		printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\npgtest-synthetic-shm 98317 54321 43996 56%% /dev/shm\n'
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
	if path, err := exec.LookPath("docker"); err != nil || path != filepath.Join(fakeDir, "docker") {
		t.Fatalf("fake Docker selection = %q, %v", path, err)
	}
	t.Logf("fake-only Docker=%s; PATH=%s", filepath.Join(fakeDir, "docker"), os.Getenv("PATH"))
	for _, tc := range []struct {
		name, scenario string
		want           []string
	}{
		{"image-ipc-metadata", "metadata", []string{"image=pgtest.invalid/synthetic-postgres:projection-only", "image_id=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "shm_size=100663297", "ipc_mode=shareable"}},
		{"exited-oom", "exited", []string{"status=exited", "running=false", "exit_code=137", "oom_killed=true", "started_at=2026-11-01T00:00:00Z", "finished_at=2026-11-01T00:01:00Z", "error=\"\"", "server process was terminated by signal 9", "database system is in recovery mode"}},
		{"running-recovery", "recovery", []string{"status=running", "running=true", "exit_code=0", "oom_killed=false", "server process was terminated by signal 9", "database system is in recovery mode"}},
		{"unavailable", "unavailable", []string{"inspect unavailable", "logs unavailable", "exit status 2", "exec unavailable"}},
		{"post-failure-snapshot", "snapshot", []string{"post-failure sample (not incident-time usage", "sampled_at=2094-07-16T12:34:56Z", "postgres (PostgreSQL) 16.73-pgtest-synthetic", "Filesystem 1024-blocks Used Available Capacity Mounted on\npgtest-synthetic-shm 98317 54321 43996 56% /dev/shm"}},
		{"snapshot-version-unavailable-keeps-history", "snapshot-version-unavailable", []string{"status=exited", "server process was terminated by signal 9", "sampled_at=2094-07-16T12:34:56Z", "postgres: synthetic version unavailable", "exit status 7"}},
		{"snapshot-space-unavailable-keeps-history", "snapshot-space-unavailable", []string{"status=exited", "server process was terminated by signal 9", "postgres (PostgreSQL) 16.73-pgtest-synthetic", "df: synthetic /dev/shm unavailable", "exit status 9"}},
		{"snapshot-bounded-tail-keeps-history", "snapshot-oversized", []string{"status=exited", "server process was terminated by signal 9", "recent-snapshot-tail-end"}},
		{"snapshot-inherited-pipe-drain-keeps-history", "snapshot-inherited-pipes", []string{"status=exited", "server process was terminated by signal 9", "snapshot-inherited-pipe", exec.ErrWaitDelay.Error()}},
		{"snapshot-timeout-keeps-history", "snapshot-timeout", []string{"status=exited", "server process was terminated by signal 9", "snapshot-budget-exhaustion", "signal: killed"}},
		{"bounded-tail", "oversized", []string{"status=exited", "recent-log-tail-end"}},
		{"inherited-pipe-drain", "inherited-pipes", []string{"inherited-pipe-state", exec.ErrWaitDelay.Error(), "server process was terminated by signal 9"}},
		{"inspect-timeout-keeps-logs", "inspect-timeout", []string{"inspect-budget-exhaustion", "signal: killed", "2026-11-01T00:00:58Z useful-history-after-inspect-timeout", "sampled_at=2094-07-16T12:34:56Z", "postgres (PostgreSQL) 16.73-pgtest-synthetic", "pgtest-synthetic-shm 98317 54321 43996 56% /dev/shm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PGTEST_DIAG_SCENARIO", tc.scenario)
			callFile := filepath.Join(t.TempDir(), "calls")
			t.Setenv("PGTEST_DIAG_CALL_FILE", callFile)
			started := time.Now()
			var got string
			if tc.scenario == "inherited-pipes" || tc.scenario == "inspect-timeout" || tc.scenario == "snapshot-inherited-pipes" || tc.scenario == "snapshot-timeout" {
				got = containerDiagWithOwnedDelay(t, tc.scenario)
			} else {
				got = containerDiag("pgtest-unit-owned")
			}
			t.Logf("capture sequence elapsed=%s", time.Since(started))
			calls, err := os.ReadFile(callFile)
			if err != nil || string(calls) != "inspect\nlogs\nexec\n" {
				t.Errorf("diagnostic capture order = %q, %v; want inspect, logs, exec", calls, err)
			}
			if strings.Contains(got, "unexpected") || strings.Contains(got, "exit status 64") {
				t.Errorf("fake rejected diagnostic command: %.2000s", got)
			}
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
			if tc.scenario == "snapshot-oversized" {
				if len(got) > 66*1024 || strings.Contains(got, "snapshot-discarded-prefix") {
					t.Errorf("diagnostic did not retain a bounded recent snapshot tail: bytes=%d", len(got))
				}
				_, sample, ok := strings.Cut(got, "\npost-failure sample (not incident-time usage, up to 65536 bytes):\n")
				if !ok || len(sample) > 64*1024 {
					t.Errorf("snapshot tail bytes=%d, label found=%t; want at most 65536 bytes", len(sample), ok)
				}
				t.Logf("bounded snapshot tail bytes=%d (limit=65536)", len(sample))
			}
		})
	}
}

// All delay scenarios are finite without either production guard. The pipe holder
// exits naturally; the timeout fake execs sleep so cancellation leaves no child.
func containerDiagWithOwnedDelay(t *testing.T, scenario string) string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "owned-pids")
	t.Setenv("PGTEST_DIAG_PID_FILE", pidFile)
	pgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("owner PID=%d PGID=%d", os.Getpid(), pgid)
	var got string
	done := make(chan struct{})
	go func() {
		got = containerDiag("pgtest-unit-owned")
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("finite diagnostic fake did not finish")
		}
		data, err := os.ReadFile(pidFile)
		if err != nil {
			t.Errorf("read owned process identities for cleanup: %v", err)
			return
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 || pid == os.Getpid() {
				t.Errorf("invalid owned PID %q", field)
				continue
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				err := syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) {
					t.Logf("owned PID=%d absent (ESRCH)", pid)
					break
				}
				if err != nil || time.Now().After(deadline) {
					t.Errorf("owned PID=%d not confirmed absent: %v", pid, err)
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	wantPIDs := 1
	if scenario == "inherited-pipes" || scenario == "snapshot-inherited-pipes" {
		wantPIDs = 2
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(pidFile)
		fields := strings.Fields(string(data))
		if len(fields) == wantPIDs {
			for i, field := range fields {
				pid, err := strconv.Atoi(field)
				if err != nil || pid <= 0 || pid == os.Getpid() {
					t.Fatalf("invalid owned PID %q", field)
				}
				childPGID, err := syscall.Getpgid(pid)
				// The wrapper may already have exited, but the last PID is
				// the delay and must be alive to exercise the schedule.
				if i == 0 && wantPIDs == 2 && errors.Is(err, syscall.ESRCH) {
					t.Logf("owned wrapper PID=%d inherited PGID=%d; already absent", pid, pgid)
					continue
				}
				if err != nil || childPGID != pgid {
					t.Fatalf("owned PID=%d PGID=%d: %v; want PGID=%d", pid, childPGID, err, pgid)
				}
				t.Logf("owned PID=%d PGID=%d observed", pid, childPGID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finite diagnostic fake did not publish owned process identities")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		return got
	case <-time.After(15 * time.Second):
		t.Fatal("finite diagnostic fake did not finish")
		return ""
	}
}

func TestStartReadySharedMemoryBudgetWithFakeDocker(t *testing.T) {
	fakeDir := t.TempDir()
	fake := `#!/bin/sh
printf '%s\n' "$$" >> "$PGTEST_START_PID_FILE"
case "$1" in
run)
	printf 'run\n' >> "$PGTEST_START_CALL_FILE"
	printf '%s\n' "$@" > "$PGTEST_START_RUN_FILE"
	printf 'pgtest-shm-budget-unit-owned\n'
	;;
port)
	[ "$#" -eq 3 ] && [ "$2" = pgtest-shm-budget-unit-owned ] && [ "$3" = 5432/tcp ] || exit 64
	printf 'port\n' >> "$PGTEST_START_CALL_FILE"
	exit 73
	;;
rm)
	[ "$#" -eq 4 ] && [ "$2" = -f ] && [ "$3" = -v ] && [ "$4" = pgtest-shm-budget-unit-owned ] || exit 64
	printf 'rm\n' >> "$PGTEST_START_CALL_FILE"
	;;
*) printf 'unexpected Docker operation\n' >&2; exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeDir, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)
	if path, err := exec.LookPath("docker"); err != nil || path != filepath.Join(fakeDir, "docker") {
		t.Fatalf("fake Docker selection = %q, %v", path, err)
	}
	t.Logf("fake-only Docker=%s; PATH=%s", filepath.Join(fakeDir, "docker"), os.Getenv("PATH"))
	callFile := filepath.Join(fakeDir, "calls")
	runFile := filepath.Join(fakeDir, "run-args")
	pidFile := filepath.Join(fakeDir, "owned-pids")
	t.Setenv("PGTEST_START_CALL_FILE", callFile)
	t.Setenv("PGTEST_START_RUN_FILE", runFile)
	t.Setenv("PGTEST_START_PID_FILE", pidFile)
	before := time.Now().Unix()
	// The owned port failure stops startReady before waitReady can dial Postgres.
	containerID, err := startReady()
	after := time.Now().Unix()
	var exitErr *exec.ExitError
	if containerID != "" || !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 || err.Error() != "resolve postgres port: exit status 73" {
		t.Fatalf("startReady = %q, %v; want owned port failure before any database dial", containerID, err)
	}
	calls, err := os.ReadFile(callFile)
	if err != nil || string(calls) != "run\nport\nrm\n" {
		t.Fatalf("startup command order = %q, %v; want run, port, rm", calls, err)
	}
	pids, err := os.ReadFile(pidFile)
	if err != nil || len(strings.Fields(string(pids))) != 3 {
		t.Fatalf("owned startup PIDs = %q, %v; want three foreground commands", pids, err)
	}
	for _, field := range strings.Fields(string(pids)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 || pid == os.Getpid() {
			t.Fatalf("invalid owned PID %q", field)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("owned startup PID=%d not confirmed absent: %v", pid, err)
		}
		t.Logf("owned startup PID=%d absent (ESRCH)", pid)
	}
	run, err := os.ReadFile(runFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(run), "\n"), "\n")
	budgetOptions := 0
	for _, arg := range args {
		if arg == "--shm-size=128m" {
			budgetOptions++
		}
	}
	if budgetOptions != 1 {
		t.Errorf("startReady shared-memory budget: got %d literal --shm-size=128m options, want exactly 1; run arguments = %q", budgetOptions, args)
	}
	const startedPrefix = "dev.opensdlc.managed-agent-platform.test-fixture-started="
	if len(args) < 6 || !strings.HasPrefix(args[5], startedPrefix) {
		t.Fatalf("missing startup time label in run arguments = %q", args)
	}
	started, err := strconv.ParseInt(strings.TrimPrefix(args[5], startedPrefix), 10, 64)
	if err != nil || started < before || started > after {
		t.Fatalf("startup time label = %q, %v; want Unix seconds in [%d, %d]", args[5], err, before, after)
	}
	want := []string{"run", "-d",
		"--label", "dev.opensdlc.managed-agent-platform.test-fixture=pgtest",
		"--label", startedPrefix + strconv.FormatInt(started, 10),
		"--shm-size=128m", "-e", "POSTGRES_PASSWORD=test",
		"-p", "127.0.0.1:0:5432", "postgres:16-alpine"}
	if string(run) != strings.Join(want, "\n")+"\n" {
		t.Errorf("startReady run arguments = %q; want %q", args, want)
	}
}
