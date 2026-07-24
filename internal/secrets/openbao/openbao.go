// Package openbao is the production secrets.Cipher: encryption as a service
// through an OpenBao (or any Vault-compatible) transit engine, spoken over
// its plain HTTP API — deliberately not the official client library, whose
// dependency tree buys nothing for the two calls this needs (docs/plan/12,
// D1). Ciphertext carries the engine's own "vault:vN:" version prefix, so key
// rotation on the bao side keeps old rows decryptable with no schema change.
package openbao

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config locates the transit engine and the named key. Token is sent as
// X-Vault-Token and never logged or echoed in errors.
type Config struct {
	Address string // base URL, e.g. http://openbao:8200
	Token   string
	Key     string // transit key name; also the keyID stored next to ciphertext

	// HTTPClient overrides the default 10s-timeout client (tests).
	HTTPClient *http.Client
}

// Cipher is the transit-backed implementation of secrets.Cipher.
type Cipher struct {
	addr   string
	token  string
	key    string
	client *http.Client
}

// New validates the config eagerly and ensures the named transit key exists —
// an unreachable bao, a bad token, or an unmounted transit engine must fail
// at startup, not on the first credential write. Creating an existing key is
// a server-side no-op, so New is idempotent.
func New(ctx context.Context, cfg Config) (*Cipher, error) {
	if cfg.Address == "" || cfg.Token == "" || cfg.Key == "" {
		return nil, errors.New("openbao cipher: Address, Token, and Key are all required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	c := &Cipher{
		addr:   strings.TrimRight(cfg.Address, "/"),
		token:  cfg.Token,
		key:    cfg.Key,
		client: client,
	}
	if _, err := c.post(ctx, "/v1/transit/keys/"+c.key, map[string]any{}); err != nil {
		return nil, fmt.Errorf("openbao cipher: ensure transit key %q: %w", c.key, err)
	}
	return c, nil
}

// Encrypt seals plaintext through POST /v1/transit/encrypt/{key}. The
// returned ciphertext is the engine's "vault:vN:…" string; the keyID is the
// transit key name.
func (c *Cipher) Encrypt(ctx context.Context, plaintext []byte) ([]byte, string, error) {
	if len(plaintext) == 0 {
		return nil, "", errors.New("openbao cipher: empty plaintext")
	}
	data, err := c.post(ctx, "/v1/transit/encrypt/"+c.key, map[string]any{
		"plaintext": base64.StdEncoding.EncodeToString(plaintext),
	})
	if err != nil {
		return nil, "", fmt.Errorf("openbao cipher: encrypt: %w", err)
	}
	// Transit ciphertext is always "vault:vN:…"; anything else is a broken
	// or interposed endpoint, and storing it would fail only at decrypt time.
	ct, _ := data["ciphertext"].(string)
	if !wellFormedCiphertext(ct) {
		return nil, "", errors.New("openbao cipher: encrypt response carried no vault:vN:-form ciphertext")
	}
	return []byte(ct), c.key, nil
}

// Decrypt reverses Encrypt through POST /v1/transit/decrypt/{key}. A keyID
// other than the configured key name is refused locally — this cipher holds
// exactly one key.
func (c *Cipher) Decrypt(ctx context.Context, ciphertext []byte, keyID string) ([]byte, error) {
	if keyID != c.key {
		return nil, fmt.Errorf("openbao cipher: key %q not held (holding %q)", keyID, c.key)
	}
	data, err := c.post(ctx, "/v1/transit/decrypt/"+c.key, map[string]any{
		"ciphertext": string(ciphertext),
	})
	if err != nil {
		return nil, fmt.Errorf("openbao cipher: decrypt: %w", err)
	}
	encoded, _ := data["plaintext"].(string)
	plaintext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(plaintext) == 0 {
		return nil, errors.New("openbao cipher: decrypt response carried no plaintext")
	}
	return plaintext, nil
}

// wellFormedCiphertext reports whether ct has the transit engine's
// "vault:v<digits>:<payload>" shape.
func wellFormedCiphertext(ct string) bool {
	rest, ok := strings.CutPrefix(ct, "vault:v")
	if !ok {
		return false
	}
	version, payload, ok := strings.Cut(rest, ":")
	if !ok || version == "" || payload == "" {
		return false
	}
	return strings.TrimLeft(version, "0123456789") == ""
}

// post sends one authenticated transit call and returns the response's
// "data" object. Error text carries the status and the server's errors array
// — never the token, never plaintext or ciphertext values.
func (c *Cipher) post(ctx context.Context, path string, body map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		var serverErr struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &serverErr)
		// The error body is server-controlled text; an interposed endpoint
		// could reflect the request's token — or the request body, which on
		// encrypt carries the base64 plaintext — so scrub every request-borne
		// value before wrapping. Body values first: they are long and
		// specific, while a short token would shred unrelated text.
		msg := strings.Join(serverErr.Errors, "; ")
		for _, v := range body {
			if s, _ := v.(string); s != "" {
				msg = strings.ReplaceAll(msg, s, "[redacted]")
			}
		}
		msg = strings.ReplaceAll(msg, c.token, "[redacted]")
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, msg)
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, fmt.Errorf("malformed response: %w", err)
		}
	}
	return envelope.Data, nil
}
