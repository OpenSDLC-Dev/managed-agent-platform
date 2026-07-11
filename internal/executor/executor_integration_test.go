package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
)

// testImage matches the sandbox contract: /bin/bash at that path plus a POSIX
// userland. The `bash` official image does not qualify — its bash lives
// elsewhere — which is the assumption the contract pins.
const testImage = "debian:stable-slim"

// TestClosedLoopRealSandbox drives one bash tool the whole way through a real
// container: the brain's suspend (a tool_use plus one tool_exec item), then the
// executor claims it, runs the command in a Docker sandbox, appends the result,
// and schedules the model_turn that resumes the brain. A missing daemon is a
// hard failure, not a skip — a skipped test silently hollows out the gate.
func TestClosedLoopRealSandbox(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("integration test requires Docker: %v", err)
	}
	h := newHarnessWith(t, provider, Config{Image: testImage})
	t.Cleanup(func() {
		// Adopt the running container (Provision is idempotent) and tear it down.
		sb, err := provider.Provision(context.Background(), sandbox.Spec{SessionID: h.sid, Image: testImage})
		if err == nil {
			_ = sb.Destroy(context.Background())
		}
	})

	bash, _ := json.Marshal(map[string]any{
		"name": "bash", "input": map[string]string{"command": "echo closed-loop-ok"},
	})
	h.suspend(t, string(bash))

	worked, err := h.exec.step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !worked {
		t.Fatal("step found no work")
	}

	results := h.types(t, "agent.tool_result")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	var body struct {
		IsError bool `json:"is_error"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(results[0].Body, &body)
	if body.IsError {
		t.Errorf("bash echo returned an error result: %+v", body)
	}
	if len(body.Content) == 0 || !strings.Contains(body.Content[0].Text, "closed-loop-ok") {
		t.Errorf("result content = %+v, want it to contain the echoed text", body.Content)
	}

	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (resume)", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec live = %d, want 0 (completed)", got)
	}
}
