package acceptance

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// TestDeploymentsSDK drives the deployment family end to end with the typed
// SDK client — Deployments.New / Get / List / Run, then the run-record
// surface DeploymentRuns.List / Get — and stops at the run record (plan 37
// §7). The stack is the control plane alone, no brain and no executor: the
// fired session enqueues its first turn and nothing consumes it, so the case
// never races a model call it did not mean to make, and needs no Docker
// sandbox either.
func TestDeploymentsSDK(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if err := api.EnsureAPIKey(ctx, pool, "acceptance", acceptanceKey); err != nil {
		t.Fatalf("seed the management key: %v", err)
	}
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil))
	t.Cleanup(srv.Close)
	client := anthropic.NewClient(option.WithAPIKey(acceptanceKey), option.WithBaseURL(srv.URL))

	env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: "deployments-sdk",
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name:  "nightly-analyst",
		Model: anthropic.BetaManagedAgentsModelConfigParams{ID: "acceptance-scripted"},
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
			},
		}},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	depl, err := client.Beta.Deployments.New(ctx, anthropic.BetaDeploymentNewParams{
		Name:          "nightly-dcf-refresh",
		Agent:         anthropic.BetaDeploymentNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env.ID,
		InitialEvents: []anthropic.BetaManagedAgentsDeploymentInitialEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText,
						Text: "Refresh the DCF model.",
					},
				}},
			},
		}},
		Schedule: anthropic.BetaManagedAgentsScheduleParams{
			Type:       anthropic.BetaManagedAgentsScheduleParamsTypeCron,
			Expression: "30 9 * * 1-5",
			Timezone:   "UTC",
		},
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if !strings.HasPrefix(depl.ID, "depl_") {
		t.Fatalf("deployment id = %q, want a depl_ prefix", depl.ID)
	}
	if depl.Status != anthropic.BetaManagedAgentsDeploymentStatusActive {
		t.Errorf("status = %q, want active", depl.Status)
	}
	if len(depl.Schedule.UpcomingRunsAt) == 0 {
		t.Errorf("upcoming_runs_at is empty for an active schedule")
	}

	got, err := client.Beta.Deployments.Get(ctx, depl.ID, anthropic.BetaDeploymentGetParams{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.ID != depl.ID || got.Name != depl.Name {
		t.Errorf("get returned %q/%q, want %q/%q", got.ID, got.Name, depl.ID, depl.Name)
	}

	page, err := client.Beta.Deployments.List(ctx, anthropic.BetaDeploymentListParams{})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	found := false
	for _, d := range page.Data {
		if d.ID == depl.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("the created deployment is missing from the list: %v", page.Data)
	}

	run, err := client.Beta.Deployments.Run(ctx, depl.ID, anthropic.BetaDeploymentRunParams{})
	if err != nil {
		t.Fatalf("run deployment: %v", err)
	}
	if !strings.HasPrefix(run.ID, "drun_") {
		t.Fatalf("run id = %q, want a drun_ prefix", run.ID)
	}
	if run.DeploymentID != depl.ID || run.TriggerContext.Type != "manual" {
		t.Errorf("run = %s under %q trigger, want %s under manual", run.DeploymentID, run.TriggerContext.Type, depl.ID)
	}
	if !strings.HasPrefix(run.SessionID, "sesn_") || run.Error.Type != "" {
		t.Errorf("run settled as (%q, %q), want a session and no error", run.SessionID, run.Error.Type)
	}

	runs, err := client.Beta.DeploymentRuns.List(ctx, anthropic.BetaDeploymentRunListParams{
		DeploymentID: anthropic.String(depl.ID),
	})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Data) != 1 || runs.Data[0].ID != run.ID {
		t.Errorf("runs list = %v, want exactly the manual run %s", runs.Data, run.ID)
	}

	single, err := client.Beta.DeploymentRuns.Get(ctx, run.ID, anthropic.BetaDeploymentRunGetParams{})
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if single.ID != run.ID || single.SessionID != run.SessionID {
		t.Errorf("get run = %s/%s, want %s/%s", single.ID, single.SessionID, run.ID, run.SessionID)
	}
}
