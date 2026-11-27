package modelgateway_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

func TestPlaygroundUsesBrowserAuthorityAndRecordsUsage(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("fixture response"))
	p := e.provider(up.URL)
	e.credential(p, "fixture-vendor-key", 1)
	deployment := e.deployment(p, "fixture-model")
	e.alias("test-chat", target(deployment, 0))
	e.alias("*", target(deployment, 0))
	actor := domain.NewID(domain.PrefixPrincipal).String()
	_, err := e.pool.Exec(e.ctx, `INSERT INTO principals(id,issuer,subject,role) VALUES ($1,'test','playground','admin')`, actor)
	e.must(err)
	const origin = "https://console.example"
	manager := login.New(e.pool, login.Config{Mode: login.ModePassword, PublicOrigin: origin, Development: true})
	issued, err := manager.Issue(e.ctx, actor)
	e.must(err)
	e.start(func(cfg *modelgateway.Config) { cfg.Login = manager })

	const route = "/api/organizations/default/proxy/v1/messages?beta=true"
	const body = `{"model":"test-chat","max_tokens":32,"messages":[{"role":"user","content":"test prompt"}],"stream":true}`
	h := map[string]string{"Cookie": "map_session=" + issued.Token, "Content-Type": "application/json", "Origin": origin}
	if res, _ := e.do(http.MethodPost, route, body, h); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Playground without CSRF: HTTP %d, want 403", res.StatusCode)
	}
	h["X-CSRF-Token"] = issued.CSRFToken
	res, raw := e.do(http.MethodPost, route, body, h)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "fixture response") || !strings.Contains(string(raw), "message_stop") {
		t.Fatalf("Playground stream: HTTP %d, %s", res.StatusCode, raw)
	}
	if res.Header.Get("Cache-Control") != "no-store, private" {
		t.Fatal("Playground stream is missing its private cache policy")
	}
	if res, _ := e.do(http.MethodPost, route, strings.Replace(body, "test-chat", "unlisted", 1), h); res.StatusCode != http.StatusNotFound {
		t.Errorf("Playground used the unnamed wildcard model: HTTP %d", res.StatusCode)
	}
	calls := up.recorded()
	if len(calls) != 1 || calls[0].Path != "/v1/messages" || calls[0].Header.Get("Cookie") != "" || calls[0].Header.Get("X-CSRF-Token") != "" {
		t.Fatal("the upstream must receive one Messages call without browser credentials")
	}
	rows, _, err := e.s.ListUsage(e.ctx, store.UsageFilter{PrincipalID: actor}, 0, 10)
	e.must(err)
	if len(rows) != 1 || rows[0].APIKeyID != "" {
		t.Fatalf("Playground usage attribution: %+v", rows)
	}

	// The same cookie sees current authority after demotion, approval and withdrawal.
	const models = "/api/organizations/default/proxy/v1/models"
	_, err = e.pool.Exec(e.ctx, `UPDATE principals SET role='user' WHERE id=$1`, actor)
	e.must(err)
	for _, access := range []bool{false, true, false} {
		_, err = e.pool.Exec(e.ctx, `UPDATE principals SET playground_access=$2 WHERE id=$1`, actor, access)
		e.must(err)
		want := http.StatusForbidden
		if access {
			want = http.StatusOK
		}
		for _, route := range []struct{ method, path, body string }{
			{http.MethodGet, models, ""}, {http.MethodPost, route, body},
		} {
			if res, _ := e.do(route.method, route.path, route.body, h); res.StatusCode != want {
				t.Errorf("access=%v %s %s: HTTP %d, want %d", access, route.method, route.path, res.StatusCode, want)
			}
		}
	}
	h["Authorization"] = "Bearer " + e.key()
	if res, _ := e.do(http.MethodGet, models, "", h); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("Playground with a key beside the cookie: HTTP %d, want 401", res.StatusCode)
	}
}

func TestPlaygroundWithdrawsOpenStreams(t *testing.T) {
	e := newEnv(t)
	upstreamStopped := make(chan struct{})
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, call fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range events(call.Model, "in progress")[:3] {
			_, _ = io.WriteString(w, frame)
			w.(http.Flusher).Flush()
		}
		select {
		case <-r.Context().Done():
			close(upstreamStopped)
		case <-t.Context().Done():
		}
	})
	p := e.provider(up.URL)
	e.credential(p, "fixture-vendor-key", 1)
	e.alias("test-chat", target(e.deployment(p, "fixture-model"), 0))
	actor := domain.NewID(domain.PrefixPrincipal).String()
	_, err := e.pool.Exec(e.ctx, `INSERT INTO principals(id,issuer,subject,role,playground_access)
		VALUES ($1,'test','playground-withdraw','user',true)`, actor)
	e.must(err)
	const origin = "https://console.example"
	manager := login.New(e.pool, login.Config{Mode: login.ModePassword, PublicOrigin: origin, Development: true})
	issued, err := manager.Issue(e.ctx, actor)
	e.must(err)
	e.start(func(cfg *modelgateway.Config) { cfg.Login = manager })

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/api/organizations/default/proxy/v1/messages?beta=true",
		strings.NewReader(`{"model":"test-chat","max_tokens":32,"messages":[{"role":"user","content":"test prompt"}],"stream":true}`))
	e.must(err)
	req.Header.Set("Cookie", "map_session="+issued.Token)
	req.Header.Set("Origin", origin)
	req.Header.Set("X-CSRF-Token", issued.CSRFToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	e.must(err)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("open Playground stream: HTTP %d", res.StatusCode)
	}
	_, err = e.pool.Exec(ctx, `UPDATE principals SET playground_access=false WHERE id=$1`, actor)
	e.must(err)
	_, _ = io.Copy(io.Discard, res.Body)
	select {
	case <-upstreamStopped:
		if ctx.Err() != nil {
			t.Fatal("client timed out before the gateway stopped inference")
		}
	case <-ctx.Done():
		t.Fatal("the gateway left inference open after access was withdrawn")
	}
}
