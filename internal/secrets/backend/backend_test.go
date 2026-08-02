package backend_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/backend"
)

// The openbao branch's happy path needs a live dev container and lives with
// the backend's own suite (openbao_test.TestFromEnvOpenBao); everything
// testable without one is here.

func TestFromEnvUnsetMeansNotConfigured(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "")
	c, err := backend.FromEnv(context.Background())
	if err != nil || c != nil {
		t.Fatalf("unset backend: got (%v, %v), want (nil, nil)", c, err)
	}
}

func TestFromEnvUnknownBackend(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "vault9000")
	_, err := backend.FromEnv(context.Background())
	if err == nil || !strings.Contains(err.Error(), "vault9000") {
		t.Fatalf("unknown backend: %v", err)
	}
}

func TestFromEnvLocal(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRETS_BACKEND", "local")
	t.Setenv("SECRETS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	c, err := backend.FromEnv(ctx)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	ct, keyID, err := c.Encrypt(ctx, []byte("wired from env"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if keyID != "local-1" {
		t.Fatalf("default key id: got %q, want local-1", keyID)
	}
	if got, err := c.Decrypt(ctx, ct, keyID); err != nil || string(got) != "wired from env" {
		t.Fatalf("round-trip: %q, %v", got, err)
	}

	t.Setenv("SECRETS_KEY_ID", "kms-2026")
	c, err = backend.FromEnv(ctx)
	if err != nil {
		t.Fatalf("FromEnv with key id: %v", err)
	}
	if _, keyID, _ = c.Encrypt(ctx, []byte("x")); keyID != "kms-2026" {
		t.Fatalf("explicit key id: got %q", keyID)
	}
}

func TestFromEnvLocalMisconfigured(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRETS_BACKEND", "local")

	t.Setenv("SECRETS_MASTER_KEY", "")
	if _, err := backend.FromEnv(ctx); err == nil {
		t.Fatal("missing master key accepted")
	}
	t.Setenv("SECRETS_MASTER_KEY", "not!!base64")
	if _, err := backend.FromEnv(ctx); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	t.Setenv("SECRETS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := backend.FromEnv(ctx); err == nil {
		t.Fatal("16-byte key accepted")
	}
}

func TestFromEnvOpenBaoMisconfigured(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRETS_BACKEND", "openbao")
	t.Setenv("BAO_ADDR", "")
	t.Setenv("BAO_TOKEN", "")
	if _, err := backend.FromEnv(ctx); err == nil {
		t.Fatal("openbao backend without BAO_ADDR/BAO_TOKEN accepted")
	}
}

// The gcpkms branch's happy path needs real Cloud KMS (or at least Application
// Default Credentials) and is covered by the backend's own live tier; what is
// testable without either is that the key name is required. There is no
// credential to check alongside it — authentication is ADC, deliberately.
func TestFromEnvGCPKMSNeedsAKeyName(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "gcpkms")
	t.Setenv("GCPKMS_KEY_NAME", "")
	_, err := backend.FromEnv(context.Background())
	if err == nil {
		t.Fatal("gcpkms backend without GCPKMS_KEY_NAME accepted")
	}
	if !strings.Contains(err.Error(), "GCPKMS_KEY_NAME") {
		t.Errorf("error %q does not name the missing variable", err)
	}
}
