package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func runPresetGateway(t *testing.T, binary string, pool *pgxpool.Pool, boot, brain string, refuse bool) {
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
		"DATABASE_URL=" + pool.Config().ConnString(), "CONTROLPLANE_API_KEY=" + boot, "BRAIN_API_KEY=" + brain,
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
			if !bytes.Contains(raw, []byte("principal-owned API key")) {
				t.Errorf("startup omitted safe owned-preset refusal: %s", raw)
			}
			if bytes.Contains(raw, []byte(boot)) || (brain != "" && bytes.Contains(raw, []byte(brain))) || bytes.Contains(raw, []byte("listening")) {
				t.Error("refusal exposed a credential or listener")
			}
			return
		case <-ticker.C:
			req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/admin/v1/profiles", nil)
			req.Header.Set("x-api-key", boot)
			res, err := client.Do(req)
			if err != nil {
				continue
			}
			res.Body.Close()
			if refuse {
				t.Errorf("owned preset exposed actual gateway listener/admin admission: HTTP %d", res.StatusCode)
			} else if res.StatusCode != http.StatusOK {
				t.Errorf("unowned bootstrap control refused: HTTP %d", res.StatusCode)
			}
			return
		case <-deadline:
			t.Fatal("startup neither exited nor exposed its test-owned listener")
		}
	}
}

func TestModelgatewayRefusesAnOwnedPreset(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "modelgateway")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build actual modelgateway: %v\n%s", err, out)
	}
	for _, name := range []string{"bootstrap", "brain"} {
		for _, state := range []string{"active", "inactive", "archived", "expired"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				pool := pgtest.NewPool(t)
				ctx := context.Background()
				if _, err := pool.Exec(ctx, `INSERT INTO principals (id,issuer,subject,role) VALUES ('principal_owner','startup-test','owner','developer')`); err != nil {
					t.Fatal(err)
				}
				const owned = "synthetic-owned-startup-preset"
				status := state
				if state == "expired" {
					status = "active"
				}
				if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id,name,owner_principal_id,owned_key_hash,created_by,status,expires_at)
					VALUES ('apikey_owned','owned','principal_owner',$1,'principal_owner',$2,
					CASE WHEN $3 THEN now() - interval '1 hour' ELSE NULL END)`, apikey.Hash(owned), status, state == "expired"); err != nil {
					t.Fatal(err)
				}
				boot, brain := "unowned-bootstrap", "unowned-brain"
				if name == "bootstrap" {
					boot = owned
				} else {
					brain = owned
				}
				runPresetGateway(t, binary, pool, boot, brain, true)
			})
		}
	}
	t.Run("unowned control", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		if _, err := pool.Exec(context.Background(), `INSERT INTO api_keys (id,name,key_hash) VALUES ('apikey_boot','bootstrap',$1),('apikey_brain','brain',$2)`, apikey.Hash("unowned-bootstrap"), apikey.Hash("unowned-brain")); err != nil {
			t.Fatal(err)
		}
		runPresetGateway(t, binary, pool, "unowned-bootstrap", "unowned-brain", false)
	})
	t.Run("unknown hash control", func(t *testing.T) {
		runPresetGateway(t, binary, pgtest.NewPool(t), "unknown-bootstrap", "", false)
	})
}
