package main

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func runNativeGateway(t *testing.T, binary string, pool *pgxpool.Pool, mode, origin string, refuse bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	log, err := os.CreateTemp(t.TempDir(), "startup-log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir(),
		"DATABASE_URL=" + pool.Config().ConnString(), "IDENTITY_MODE=" + mode, "CONSOLE_PUBLIC_ORIGIN=" + origin, "DEVELOPMENT_MODE=true",
		"MODELGATEWAY_ADDR=" + addr, "SECRETS_BACKEND=local",
		"SECRETS_MASTER_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Logf("created modelgateway pid=%d pgid=%d loopback=%s", pid, pid, addr)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	defer func() {
		if !waited {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("modelgateway orderly shutdown: %v", err)
				}
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				<-done
				t.Error("modelgateway required forced cleanup")
			}
		}
		if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("owned process group still exists: %v", err)
		} else {
			t.Logf("removed modelgateway pid=%d pgid=%d; group absent", pid, pid)
		}
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			conn.Close()
			t.Error("owned loopback listener survived exit")
		}
	}()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 200 * time.Millisecond}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case err := <-done:
			waited = true
			raw, readErr := os.ReadFile(log.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !refuse || err == nil {
				t.Fatalf("startup exit=%v; want refused=%t", err, refuse)
			}
			if !bytes.Contains(raw, []byte("login:")) {
				t.Errorf("startup omitted login refusal: %s", raw)
			}
			if bytes.Contains(raw, []byte("listening")) {
				t.Error("refusal exposed a listener")
			}
			return
		case <-ticker.C:
			req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/admin/v1/profiles", nil)
			res, err := client.Do(req)
			if err != nil {
				continue
			}
			res.Body.Close()
			if refuse {
				t.Errorf("missing native config exposed gateway listener: HTTP %d", res.StatusCode)
			} else if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("anonymous admin request was not refused: HTTP %d", res.StatusCode)
			}
			return
		case <-deadline:
			t.Fatal("startup neither exited nor exposed its test-owned listener")
		}
	}
}

func TestModelgatewayRequiresNativeLoginWithoutPreset(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "modelgateway")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build modelgateway: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		mode, origin string
		refuse       bool
	}{
		{"", "", true}, {"disabled", "https://console.example", true},
		{"oidc", "", true}, {"trusted_proxy", "", true},
		{"password", "https://console.example", false}, {"oidc", "https://console.example", false}, {"trusted_proxy", "https://console.example", false},
	} {
		t.Run(tc.mode+"/"+tc.origin, func(t *testing.T) { runNativeGateway(t, binary, pgtest.NewPool(t), tc.mode, tc.origin, tc.refuse) })
	}
}
