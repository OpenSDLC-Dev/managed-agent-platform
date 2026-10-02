package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// TestAFailedManagementLookupIsQuietWhenTheClientLeft: the management lane's
// environment-key lookup runs for a request nothing has authenticated, so a
// lookup ended by the client going away is Debug — otherwise anyone could
// drive the Warn volume by hanging up — while any other failure, the database
// unreachable say, is the operator's at Warn.
func TestAFailedManagementLookupIsQuietWhenTheClientLeft(t *testing.T) {
	for err, want := range map[error]slog.Level{
		context.Canceled: slog.LevelDebug,
		fmt.Errorf("acquire connection: %w", context.DeadlineExceeded):  slog.LevelDebug,
		errors.New("dial tcp 127.0.0.1:1: connect: connection refused"): slog.LevelWarn,
	} {
		if got := lookupFailureLevel(err); got != want {
			t.Errorf("lookupFailureLevel(%v) = %v, want %v", err, got, want)
		}
	}
}
