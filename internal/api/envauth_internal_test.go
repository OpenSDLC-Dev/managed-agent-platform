package api

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// TestCredentialFromFailsClosed: who signed a request is read from the value
// the two environment lanes set, never inferred from an environment being in
// the context, so a lane that stores one for another reason reads as a
// management caller and is refused a user.tool_result (#662).
func TestCredentialFromFailsClosed(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyEnvironment, "env_elsewhere")
	if got := credentialFrom(ctx); got != events.ManagementCredential {
		t.Errorf("an environment alone reads as %v, want the management credential", got)
	}
	if got := credentialFrom(context.WithValue(ctx, ctxKeyCredential, events.EnvironmentCredential)); got != events.EnvironmentCredential {
		t.Errorf("a marked request reads as %v, want the environment credential", got)
	}
}
