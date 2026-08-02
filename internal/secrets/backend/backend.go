// Package backend selects a secrets cipher by name, so every binary that
// encrypts vault credential material constructs it from the same config point
// instead of each mapping the environment its own way.
//
// It is a sibling of internal/secrets rather than part of it — the shape
// internal/sandbox/backend already has, and for the same structural reason:
// the seam package holds the interface AND the sentinel errors backends wrap,
// so it must not import them. FromEnv lived in internal/secrets until the
// gcpkms backend needed secrets.ErrPlaintextTooLarge, which would have closed
// the loop into an import cycle.
package backend

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/gcpkms"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/openbao"
)

// FromEnv builds the cipher from the SECRETS_*/BAO_*/GCPKMS_* environment —
// the one construction the controlplane and executor binaries share, so their
// notion of "configured" cannot drift. (nil, nil) when SECRETS_BACKEND is
// unset: the cipher is optional like object storage, and each binary decides
// what its absence means (vault credential storage reports it). Once a backend
// is selected, missing configuration fails rather than degrades — the
// modeltest rule: opting in makes misconfiguration an error, never a silent
// fallback.
func FromEnv(ctx context.Context) (secrets.Cipher, error) {
	backend := os.Getenv("SECRETS_BACKEND")
	switch backend {
	case "":
		return nil, nil
	case "local":
		encoded := os.Getenv("SECRETS_MASTER_KEY")
		if encoded == "" {
			return nil, fmt.Errorf("SECRETS_BACKEND=local needs SECRETS_MASTER_KEY (base64, 32 bytes)")
		}
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("SECRETS_MASTER_KEY is not valid base64: %w", err)
		}
		keyID := os.Getenv("SECRETS_KEY_ID")
		if keyID == "" {
			keyID = "local-1"
		}
		c, err := local.New(local.Config{KeyID: keyID, Key: key})
		if err != nil {
			return nil, err
		}
		slog.Info("secrets cipher configured", "backend", "local", "key_id", keyID)
		return c, nil
	case "openbao":
		addr, token := os.Getenv("BAO_ADDR"), os.Getenv("BAO_TOKEN")
		if addr == "" || token == "" {
			return nil, fmt.Errorf("SECRETS_BACKEND=openbao needs BAO_ADDR and BAO_TOKEN")
		}
		key := os.Getenv("BAO_TRANSIT_KEY")
		if key == "" {
			key = "map-secrets"
		}
		c, err := openbao.New(ctx, openbao.Config{Address: addr, Token: token, Key: key})
		if err != nil {
			return nil, err
		}
		slog.Info("secrets cipher configured", "backend", "openbao", "addr", addr, "transit_key", key)
		return c, nil
	case "gcpkms":
		// No credential variable: authentication is Application Default
		// Credentials, which on GKE is a Workload Identity binding. A key
		// resource name is not a secret (plan 20, Decision 3), so there is
		// nothing here to keep out of a log line.
		name := os.Getenv("GCPKMS_KEY_NAME")
		if name == "" {
			return nil, fmt.Errorf("SECRETS_BACKEND=gcpkms needs GCPKMS_KEY_NAME " +
				"(projects/P/locations/L/keyRings/R/cryptoKeys/K)")
		}
		c, err := gcpkms.New(ctx, gcpkms.Config{KeyName: name})
		if err != nil {
			return nil, err
		}
		slog.Info("secrets cipher configured", "backend", "gcpkms", "key_name", name)
		return c, nil
	default:
		return nil, fmt.Errorf("unknown SECRETS_BACKEND %q (want local, openbao or gcpkms)", backend)
	}
}
