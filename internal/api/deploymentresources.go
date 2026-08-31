package api

import (
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// deploymentResource is one element of deployments.resources as stored: the
// wire's SessionResourceConfig, plus — for a repository — the sealed token that
// config deliberately omits.
//
// A deployment's resources are *configuration*, not materialized session
// resources: no sesrsc_ id, no timestamps, and a memory store keeps only its
// id and per-attachment settings. The snapshot of a store's name, description
// and mount path happens when a fire creates a session, so a store renamed
// between two nightly runs reaches the second run under its new name — which
// is the behavior a stored snapshot on the deployment would have frozen.
type deploymentResource struct {
	Type string `json:"type"`

	// github_repository
	URL      string        `json:"url,omitempty"`
	Checkout *checkoutJSON `json:"checkout,omitempty"`

	// file
	FileID string `json:"file_id,omitempty"`

	// memory_store
	MemoryStoreID string  `json:"memory_store_id,omitempty"`
	Access        string  `json:"access,omitempty"`
	Instructions  *string `json:"instructions,omitempty"`

	// MountPath is absent on a memory store, whose path is derived from the
	// store's name when a session mounts it.
	MountPath string `json:"mount_path,omitempty"`

	// Token is storage only and never echoed: config() drops it, and every
	// render path goes through config(). The plaintext never lands here —
	// sealRepoTokens has already run it through the cipher (plan 25 decision 2,
	// "never stored unencrypted").
	Token *sealedTokenJSON `json:"token,omitempty"`
}

// sealedTokenJSON is a sealed repository token at rest inside the resources
// bag. deployments has no credential table of its own: the deployment is
// configuration, and a credential row would need a session to FK.
type sealedTokenJSON struct {
	Ciphertext []byte `json:"ciphertext"`
	KeyID      string `json:"key_id"`
}

// config strips what the wire calls write-only — "the authorization token is
// write-only and never returned" — leaving the SessionResourceConfig the
// reference echoes.
func (r deploymentResource) config() deploymentResource {
	r.Token = nil
	return r
}

// deploymentResourcesFrom pairs each validated input with its pre-sealed token
// and produces the elements to store. sealRepoTokens walks these same inputs in
// the same order and appends one entry per repository, so sealed is exactly as
// long as the repositories here and the two line up by position — indexed
// rather than length-guarded, because a guard would turn a broken invariant
// into a repository stored without its credential and a 200 saying otherwise.
func deploymentResourcesFrom(inputs []resourceInput, sealed []sealedToken) []deploymentResource {
	out := make([]deploymentResource, 0, len(inputs))
	repo := 0
	for _, in := range inputs {
		switch in.kind {
		case resourceKindRepo:
			el := deploymentResource{
				Type: "github_repository", URL: in.url,
				Checkout: in.checkout, MountPath: in.mountPath,
				Token: &sealedTokenJSON{
					Ciphertext: sealed[repo].ciphertext,
					KeyID:      sealed[repo].keyID,
				},
			}
			repo++
			out = append(out, el)
		case resourceKindMemory:
			out = append(out, deploymentResource{
				Type: "memory_store", MemoryStoreID: in.memoryStoreID,
				Access: in.access, Instructions: in.instructions,
			})
		default:
			out = append(out, deploymentResource{
				Type: "file", FileID: in.fileID, MountPath: in.mountPath,
			})
		}
	}
	return out
}

// sessionInputsFrom is deploymentResourcesFrom's inverse at fire time: the
// stored elements become the validated inputs a session create takes, and a
// repository's sealed token is copied as ciphertext — the cipher is never
// dialed at fire time. The positional pairing is deploymentResourcesFrom's
// own, walked back the other way; the type switch is total over what that
// function writes, and anything else is corrupt storage, refused rather than
// misread as a file.
func sessionInputsFrom(stored []deploymentResource) ([]resourceInput, []sealedToken, error) {
	var inputs []resourceInput
	var sealed []sealedToken
	for _, r := range stored {
		switch r.Type {
		case "github_repository":
			inputs = append(inputs, resourceInput{
				kind: resourceKindRepo, url: r.URL,
				checkout: r.Checkout, mountPath: r.MountPath,
			})
			sealed = append(sealed, sealedToken{ciphertext: r.Token.Ciphertext, keyID: r.Token.KeyID})
		case "memory_store":
			inputs = append(inputs, resourceInput{
				kind: resourceKindMemory, memoryStoreID: r.MemoryStoreID,
				access: r.Access, instructions: r.Instructions,
			})
		case "file":
			inputs = append(inputs, resourceInput{
				kind: resourceKindFile, fileID: r.FileID, mountPath: r.MountPath,
			})
		default:
			return nil, nil, fmt.Errorf("stored deployment resource has unknown type %q", r.Type)
		}
	}
	return inputs, sealed, nil
}

// echoDeploymentResources is what the response carries.
func echoDeploymentResources(rs []deploymentResource) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(rs))
	for _, r := range rs {
		out = append(out, mustJSON(r.config()))
	}
	return out
}

// deploymentVaultIDs converts the parsed vault ids for the wire type.
func deploymentVaultIDs(ids []string) []domain.ID {
	out := make([]domain.ID, 0, len(ids))
	for _, id := range ids {
		out = append(out, domain.ID(id))
	}
	return out
}
