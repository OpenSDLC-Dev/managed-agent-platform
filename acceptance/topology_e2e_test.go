package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api/apitest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	mgstore "github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	provideranthropic "github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worker"
)

const (
	topologyUpstreamSecret = "sk-ant-upstream-topology-secret"
	topologyModelAlias     = "topology-e2e"
	topologyUpstreamModel  = "upstream-topology-model"
)

type sseTurn struct {
	toolID    string
	toolName  string
	toolInput string
	text      string
	stop      string
}

type recordedUpstreamCall struct {
	APIKey string
	Body   string
}

type scriptedAnthropicUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	turns []sseTurn
	calls []recordedUpstreamCall
}

func newScriptedAnthropicUpstream(t *testing.T, turns []sseTurn) *scriptedAnthropicUpstream {
	t.Helper()
	u := &scriptedAnthropicUpstream{turns: turns}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.Error(w, "unexpected path "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		idx := len(u.calls)
		u.calls = append(u.calls, recordedUpstreamCall{
			APIKey: r.Header.Get("x-api-key"),
			Body:   string(raw),
		})
		if idx >= len(u.turns) {
			u.mu.Unlock()
			http.Error(w, fmt.Sprintf("unexpected upstream call #%d", idx+1), http.StatusBadRequest)
			return
		}
		turn := u.turns[idx]
		u.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		writeSSE := func(event string, payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				t.Errorf("marshal SSE %s: %v", event, err)
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            fmt.Sprintf("msg_topology_%d", idx+1),
				"type":          "message",
				"role":          "assistant",
				"model":         topologyUpstreamModel,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 15, "output_tokens": 1},
			},
		})

		if turn.toolName != "" {
			writeSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    turn.toolID,
					"name":  turn.toolName,
					"input": map[string]any{},
				},
			})
			writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": turn.toolInput,
				},
			})
			writeSSE("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": 0,
			})
		} else {
			writeSSE("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         0,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": turn.text},
			})
			writeSSE("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": 0,
			})
		}

		writeSSE("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": turn.stop, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 12},
		})
		writeSSE("message_stop", map[string]any{"type": "message_stop"})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *scriptedAnthropicUpstream) recordedCalls() []recordedUpstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]recordedUpstreamCall, len(u.calls))
	copy(out, u.calls)
	return out
}

// startGateway wires a real modelgateway backed by Postgres, local.Cipher,
// catalog, and a scripted upstream Anthropic Messages endpoint.
func startGateway(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cipher *local.Cipher, upstreamURL string) (*httptest.Server, *mgstore.Store) {
	t.Helper()
	st := mgstore.New(pool)
	p, err := st.CreateProvider(ctx, mgstore.Provider{
		Name:      "topology-upstream",
		Profile:   "anthropic-generic",
		Enabled:   true,
		Endpoints: map[profile.Protocol]string{profile.Anthropic: upstreamURL},
	})
	if err != nil {
		t.Fatalf("create gateway provider: %v", err)
	}
	ct, kid, err := cipher.Encrypt(ctx, []byte(topologyUpstreamSecret))
	if err != nil {
		t.Fatalf("seal upstream credential: %v", err)
	}
	if _, err := st.CreateCredential(ctx, mgstore.Credential{
		ProviderID: p.ID,
		Ciphertext: ct,
		KeyID:      kid,
		LastFour:   topologyUpstreamSecret[len(topologyUpstreamSecret)-4:],
		Weight:     1,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("create gateway credential: %v", err)
	}
	dep, err := st.CreateDeployment(ctx, mgstore.Deployment{
		ProviderID:    p.ID,
		UpstreamModel: topologyUpstreamModel,
		Kind:          mgstore.KindChat,
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("create gateway deployment: %v", err)
	}
	if _, err := st.CreateAlias(ctx, mgstore.Alias{
		Name:    topologyModelAlias,
		Targets: []mgstore.Target{{DeploymentID: dep.ID, Priority: 0, Weight: 1}},
	}); err != nil {
		t.Fatalf("create gateway alias: %v", err)
	}

	cat, err := catalog.New(ctx, pool, time.Hour)
	if err != nil {
		t.Fatalf("load gateway catalog: %v", err)
	}
	h, err := modelgateway.New(modelgateway.Config{
		Catalog: cat,
		Store:   st,
		Keys:    pool,
		Cipher:  cipher,
		Backoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("build modelgateway handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, st
}

// waitIdleStream opens a typed SDK SSE stream before sending a user message
// and waits for session.status_idle (stop_reason == end_turn), returning the
// final assistant text observed on the stream.
func waitIdleStream(ctx context.Context, client anthropic.Client, sessionID string) (<-chan string, <-chan error) {
	textCh := make(chan string, 1)
	errCh := make(chan error, 1)
	stream := client.Beta.Sessions.Events.StreamEvents(ctx, sessionID, anthropic.BetaSessionEventStreamParams{})
	go func() {
		defer stream.Close()
		var lastText strings.Builder
		for stream.Next() {
			ev := stream.Current()
			switch ev.Type {
			case "agent.message":
				lastText.Reset()
				for _, blk := range ev.AsAgentMessage().Content {
					if blk.Type == "text" {
						lastText.WriteString(blk.Text)
					}
				}
			case "session.status_idle":
				if ev.AsSessionStatusIdle().StopReason.Type == "end_turn" {
					textCh <- lastText.String()
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("event stream closed before session.status_idle(end_turn)")
	}()
	return textCh, errCh
}

func startHostServer(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen 0.0.0.0:0: %v", err)
	}
	srv := &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: h},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	return srv, port
}

// waitUsageRows polls the modelgateway usage ledger until at least want rows
// have landed for sessionID. On streaming responses, modelgateway flushes the
// final SSE frame before its handler finishes writing the ledger row, so the
// brain can commit session.status_idle slightly ahead of the last usage insert.
func waitUsageRows(ctx context.Context, t *testing.T, st *mgstore.Store, sessionID string, want int) []mgstore.Usage {
	t.Helper()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		rows, _, err := st.ListUsage(ctx, mgstore.UsageFilter{SessionID: sessionID}, 0, 10)
		if err != nil {
			t.Fatalf("list modelgateway usage: %v", err)
		}
		if len(rows) >= want {
			return rows
		}
		select {
		case <-ctx.Done():
			t.Fatalf("modelgateway usage rows for session %s = %d, want %d before deadline", sessionID, len(rows), want)
		case <-tick.C:
		}
	}
}

func reapSessionContainers(sessionID string, withGate bool) {
	_ = exec.Command("docker", "rm", "--force", "--volumes", "map-"+sessionID).Run()
	if withGate {
		_ = exec.Command("docker", "rm", "--force", "--volumes", "map-gate-"+sessionID).Run()
	}
}

// TestFullTopologyCloudGatedWithModelGateway drives the full offline cloud
// topology end-to-end: SDK -> controlplane -> Postgres queue -> brain
// (delegated inference claim) -> modelgateway -> scripted Anthropic upstream ->
// executor -> real Docker sandbox paired with the real egress-gate container
// fetching its policy from controlplane.
func TestFullTopologyCloudGatedWithModelGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pool := pgtest.NewPool(t)
	blobs := blobtest.Mem()
	cipher, err := local.New(local.Config{KeyID: "topology-k1", Key: bytes.Repeat([]byte{0x42}, 32)})
	if err != nil {
		t.Fatalf("build local cipher: %v", err)
	}
	gateImage := sandboxtest.BuildGateImage(t)
	hostAddr := sandboxtest.DockerHostAddr(t)

	// Control plane bound on all interfaces so the gate container can fetch
	// /internal/v1/gate/config over the Docker bridge.
	var gateConfigHits sync.Map
	cpBase := api.NewHandler(pool, blobs, cipher, nil, apitest.PasswordLogin(pool))
	cpSrv, cpPort := startHostServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/gate/config" {
			gateConfigHits.Store(time.Now().UnixNano(), r.RemoteAddr)
		}
		cpBase.ServeHTTP(w, r)
	}))
	cpGateURL := "http://" + net.JoinHostPort(hostAddr, cpPort)

	// Host origin reachable only through the gate once allowed_hosts admits hostAddr.
	_, originPort := startHostServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/echo" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "topology-origin-ok")
	}))
	originAddr := net.JoinHostPort(hostAddr, originPort)

	// Bash script executed inside the gated sandbox:
	// 1. Waits up to 45s (past cmd/gate's 30s defaultFetchInterval if the
	//    initial fetch races token persistence) for the allowed origin to
	//    return 200 through the gate proxy.
	// 2. Verifies a non-allowed host is rejected with 403 by the gate proxy.
	// 3. Verifies a direct TCP dial bypassing the proxy is blocked by iptables.
	probeScript := fmt.Sprintf(`set -u
hp=${HTTP_PROXY#http://}; hp=${hp%%/}
allowed_out=""
for i in $(seq 1 90); do
  allowed_out=$(exec 3<>"/dev/tcp/${hp%%%%:*}/${hp##*:}" 2>/dev/null && {
    printf '%%b' 'GET http://%s/echo HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n' >&3
    timeout 5 cat <&3
  })
  case "$allowed_out" in
    *"HTTP/1.1 200"*) break ;;
  esac
  sleep 0.5
done
printf 'ALLOWED_OUT=%%s\n' "$allowed_out"
denied_out=$(exec 3<>"/dev/tcp/${hp%%%%:*}/${hp##*:}" && {
  printf '%%b' 'GET http://denied.invalid/ HTTP/1.1\r\nHost: denied.invalid\r\nConnection: close\r\n\r\n' >&3
  timeout 5 cat <&3
})
printf 'DENIED_OUT=%%s\n' "$denied_out"
if timeout 2 bash -c 'exec 3<>"/dev/tcp/%s/%s"' 2>/dev/null; then
  echo "DIRECT=OPEN"
else
  echo "DIRECT=BLOCKED"
fi`, originAddr, originAddr, hostAddr, originPort)

	bashInput, err := json.Marshal(map[string]string{"command": probeScript})
	if err != nil {
		t.Fatalf("marshal bash input: %v", err)
	}
	upstream := newScriptedAnthropicUpstream(t, []sseTurn{
		{toolID: "toolu_topo_cloud_1", toolName: "bash", toolInput: string(bashInput), stop: "tool_use"},
		{text: "topology-cloud-complete", stop: "end_turn"},
	})
	gwSrv, mgSt := startGateway(t, ctx, pool, cipher, upstream.srv.URL)

	reg, err := provider.NewRegistry(
		[]provider.Route{{
			Model: "*",
			Config: provider.Config{
				Protocol:           "anthropic",
				BaseURL:            gwSrv.URL,
				DelegatedInference: true,
			},
		}},
		map[string]provider.Factory{"anthropic": provideranthropic.New},
	)
	if err != nil {
		t.Fatalf("build delegated provider registry: %v", err)
	}

	sbx, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("docker sandbox provider: %v", err)
	}

	var mu sync.Mutex
	var tracked []string
	t.Cleanup(func() {
		mu.Lock()
		ids := append([]string(nil), tracked...)
		mu.Unlock()
		for _, id := range ids {
			reapSessionContainers(id, true)
		}
	})

	loopCtx, stopLoops := context.WithCancel(context.Background())
	brainDone := runLoop(func() error {
		return brain.New(pool, reg, blobs, brain.Config{
			LeaseTTL:     2 * time.Minute,
			PollInterval: 50 * time.Millisecond,
		}).Run(loopCtx)
	})
	execDone := runLoop(func() error {
		return executor.New(pool, events.NewLog(pool), queue.New(pool), sbx, blobs, cipher, executor.Config{
			Image:           sandboxImage,
			LeaseTTL:        5 * time.Minute,
			PollInterval:    50 * time.Millisecond,
			ControlplaneURL: cpGateURL,
			GateImage:       gateImage,
		}).Run(loopCtx)
	})
	t.Cleanup(func() {
		stopLoops()
		waitLoop(t, "brain", brainDone)
		waitLoop(t, "executor", execDone)
	})

	apiKey := apitest.SetupKey(t, pool, cpSrv.URL)
	client := anthropic.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(cpSrv.URL))

	env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: "topology-cloud-gated",
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
			OfCloud: &anthropic.BetaCloudConfigParams{
				Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{
					OfLimited: &anthropic.BetaLimitedNetworkParams{
						AllowedHosts: []string{hostAddr},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create limited cloud environment: %v", err)
	}
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name:  "topology-cloud-agent",
		Model: anthropic.BetaManagedAgentsModelConfigParams{ID: topologyModelAlias},
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
			},
		}},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	sess, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env.ID,
		Title:         anthropic.String("Full Topology Cloud+Gate+Gateway"),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	mu.Lock()
	tracked = append(tracked, sess.ID)
	mu.Unlock()

	textCh, errCh := waitIdleStream(ctx, client, sess.ID)
	if _, err := client.Beta.Sessions.Events.Send(ctx, sess.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText,
						Text: "Run the gated network probe.",
					},
				}},
			},
		}},
	}); err != nil {
		t.Fatalf("send user.message: %v", err)
	}

	var finalText string
	select {
	case finalText = <-textCh:
	case err := <-errCh:
		t.Fatalf("stream error: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for cloud topology session to reach idle")
	}
	if finalText != "topology-cloud-complete" {
		t.Fatalf("final assistant text = %q, want %q", finalText, "topology-cloud-complete")
	}

	calls := upstream.recordedCalls()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (tool turn + closing turn)", len(calls))
	}
	for i, c := range calls {
		if c.APIKey != topologyUpstreamSecret {
			t.Errorf("upstream call %d x-api-key = %q, want sealed vendor secret (not delegated token)", i+1, c.APIKey)
		}
	}
	for _, want := range []string{"HTTP/1.1 200", "topology-origin-ok", "HTTP/1.1 403", "DIRECT=BLOCKED"} {
		if !strings.Contains(calls[1].Body, want) {
			hits := 0
			gateConfigHits.Range(func(_, _ any) bool { hits++; return true })
			logs, _ := exec.Command("docker", "logs", "map-gate-"+sess.ID).CombinedOutput()
			t.Errorf("turn 2 replayed tool_result missing %q (gate config fetches=%d, cpGateURL=%s);\ngate logs:\n%s\nbody:\n%s", want, hits, cpGateURL, logs, calls[1].Body)
		}
	}

	rows := waitUsageRows(ctx, t, mgSt, sess.ID, 2)
	if len(rows) != 2 {
		t.Fatalf("modelgateway usage rows for session %s = %d, want 2", sess.ID, len(rows))
	}
	for i, row := range rows {
		if row.SessionID != sess.ID || row.APIKeyID != "" || row.PrincipalID == "" {
			t.Errorf("usage row %d = %+v, want delegated attribution (session=%s, empty api_key_id, non-empty principal_id)", i, row, sess.ID)
		}
		if row.Model != topologyModelAlias || row.Status != http.StatusOK {
			t.Errorf("usage row %d model=%q status=%d, want %q/200", i, row.Model, row.Status, topologyModelAlias)
		}
	}
}

// TestFullTopologyBYOCWorkerWithModelGateway drives the full offline BYOC
// worker topology: SDK -> controlplane -> brain (delegated claim) ->
// modelgateway -> scripted upstream, with a real BYOC worker polling over HTTP
// using an issued environment key and executing bash in Docker (no cloud executor).
func TestFullTopologyBYOCWorkerWithModelGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := pgtest.NewPool(t)
	blobs := blobtest.Mem()
	cipher, err := local.New(local.Config{KeyID: "topology-k2", Key: bytes.Repeat([]byte{0x24}, 32)})
	if err != nil {
		t.Fatalf("build local cipher: %v", err)
	}

	cpSrv := httptest.NewServer(api.NewHandler(pool, blobs, cipher, nil, apitest.PasswordLogin(pool)))
	t.Cleanup(cpSrv.Close)

	bashInput, err := json.Marshal(map[string]string{
		"command": "printf 'byoc-worker-proof-ok' > /workspace/proof.txt && cat /workspace/proof.txt",
	})
	if err != nil {
		t.Fatalf("marshal bash input: %v", err)
	}
	upstream := newScriptedAnthropicUpstream(t, []sseTurn{
		{toolID: "toolu_topo_byoc_1", toolName: "bash", toolInput: string(bashInput), stop: "tool_use"},
		{text: "topology-byoc-complete", stop: "end_turn"},
	})
	gwSrv, mgSt := startGateway(t, ctx, pool, cipher, upstream.srv.URL)

	reg, err := provider.NewRegistry(
		[]provider.Route{{
			Model: "*",
			Config: provider.Config{
				Protocol:           "anthropic",
				BaseURL:            gwSrv.URL,
				DelegatedInference: true,
			},
		}},
		map[string]provider.Factory{"anthropic": provideranthropic.New},
	)
	if err != nil {
		t.Fatalf("build delegated provider registry: %v", err)
	}

	sbx, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("docker sandbox provider: %v", err)
	}

	apiKey := apitest.SetupKey(t, pool, cpSrv.URL)
	client := anthropic.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(cpSrv.URL))

	env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: "topology-byoc-env",
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
			OfSelfHosted: &anthropic.BetaSelfHostedConfigParams{},
		},
	})
	if err != nil {
		t.Fatalf("create self_hosted environment: %v", err)
	}
	envKey, err := api.IssueEnvironmentKey(ctx, pool, env.ID, "topology-byoc-host")
	if err != nil {
		t.Fatalf("issue environment key: %v", err)
	}

	var mu sync.Mutex
	var tracked []string
	t.Cleanup(func() {
		mu.Lock()
		ids := append([]string(nil), tracked...)
		mu.Unlock()
		for _, id := range ids {
			reapSessionContainers(id, false)
		}
	})

	loopCtx, stopLoops := context.WithCancel(context.Background())
	brainDone := runLoop(func() error {
		return brain.New(pool, reg, blobs, brain.Config{
			LeaseTTL:     2 * time.Minute,
			PollInterval: 50 * time.Millisecond,
		}).Run(loopCtx)
	})
	workerDone := runLoop(func() error {
		return worker.NewWorker(worker.NewClient(cpSrv.URL, envKey), sbx, worker.Config{
			EnvironmentID:     env.ID,
			WorkerID:          "topology-byoc-worker",
			Image:             sandboxImage,
			EmptyPollSleep:    50 * time.Millisecond,
			HeartbeatInterval: 2 * time.Second,
		}).Run(loopCtx)
	})
	t.Cleanup(func() {
		stopLoops()
		waitLoop(t, "brain", brainDone)
		waitLoop(t, "worker", workerDone)
	})

	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name:  "topology-byoc-agent",
		Model: anthropic.BetaManagedAgentsModelConfigParams{ID: topologyModelAlias},
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
			},
		}},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	sess, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env.ID,
		Title:         anthropic.String("Full Topology BYOC+Gateway"),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	mu.Lock()
	tracked = append(tracked, sess.ID)
	mu.Unlock()

	textCh, errCh := waitIdleStream(ctx, client, sess.ID)
	if _, err := client.Beta.Sessions.Events.Send(ctx, sess.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText,
						Text: "Write and read proof.txt via BYOC worker.",
					},
				}},
			},
		}},
	}); err != nil {
		t.Fatalf("send user.message: %v", err)
	}

	var finalText string
	select {
	case finalText = <-textCh:
	case err := <-errCh:
		t.Fatalf("stream error: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for BYOC topology session to reach idle")
	}
	if finalText != "topology-byoc-complete" {
		t.Fatalf("final assistant text = %q, want %q", finalText, "topology-byoc-complete")
	}

	calls := upstream.recordedCalls()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(calls))
	}
	if !strings.Contains(calls[1].Body, "byoc-worker-proof-ok") {
		t.Errorf("turn 2 replayed tool_result missing BYOC worker output; body:\n%s", calls[1].Body)
	}

	rows := waitUsageRows(ctx, t, mgSt, sess.ID, 2)
	if len(rows) != 2 {
		t.Fatalf("modelgateway usage rows for session %s = %d, want 2", sess.ID, len(rows))
	}
}
