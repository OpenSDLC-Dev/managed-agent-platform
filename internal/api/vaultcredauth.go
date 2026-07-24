package api

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"
	"time"
)

// The credential auth union (plan 12 slice 2). The stored auth document is
// exactly the wire response shape — write-only secret fields never enter it;
// they are collected separately and sealed through the cipher as one JSON
// object per credential.

type mcpOAuthAuthJSON struct {
	Type         string       `json:"type"`
	MCPServerURL string       `json:"mcp_server_url"`
	ExpiresAt    *time.Time   `json:"expires_at"`
	Refresh      *refreshJSON `json:"refresh"`
}

type refreshJSON struct {
	ClientID          string                `json:"client_id"`
	TokenEndpoint     string                `json:"token_endpoint"`
	TokenEndpointAuth tokenEndpointAuthJSON `json:"token_endpoint_auth"`
	Resource          *string               `json:"resource"`
	Scope             *string               `json:"scope"`
}

// tokenEndpointAuthJSON renders only the discriminator: client_secret is
// write-only on every arm.
type tokenEndpointAuthJSON struct {
	Type string `json:"type"`
}

type staticBearerAuthJSON struct {
	Type         string `json:"type"`
	MCPServerURL string `json:"mcp_server_url"`
}

type envVarAuthJSON struct {
	Type              string                `json:"type"`
	SecretName        string                `json:"secret_name"`
	Networking        json.RawMessage       `json:"networking"`
	InjectionLocation injectionLocationJSON `json:"injection_location"`
}

type injectionLocationJSON struct {
	Body   bool `json:"body"`
	Header bool `json:"header"`
}

// credAuth is a parsed auth union: the secret-free wire document, the secret
// fields to seal, and the vault-scoped uniqueness anchor ("url:…" for the MCP
// variants — they share the mcp_server_url field, so they share the
// namespace — or "name:…" for environment variables).
type credAuth struct {
	authType string
	doc      []byte
	secrets  map[string]string
	key      string
}

const (
	authMCPOAuth     = "mcp_oauth"
	authStaticBearer = "static_bearer"
	authEnvVar       = "environment_variable"
)

// objectField unwraps a required JSON object field, distinguishing absent,
// null, and non-object.
func objectField(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool, error) {
	raw, ok := obj[key]
	if !ok {
		return nil, false, nil
	}
	if isNull(raw) {
		return nil, true, errInvalid("%s cannot be null; omit the field instead", key)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
		return nil, true, errInvalid("%s must be an object", key)
	}
	return nested, true, nil
}

// parseCredAuthCreate validates a create-time auth union.
func parseCredAuthCreate(raw json.RawMessage) (*credAuth, error) {
	if raw == nil || isNull(raw) {
		return nil, errInvalid("auth is required")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errInvalid("auth must be an object")
	}
	var typ string
	if rawType, ok := obj["type"]; ok {
		_ = json.Unmarshal(rawType, &typ)
	}
	switch typ {
	case authMCPOAuth:
		return parseMCPOAuthCreate(obj)
	case authStaticBearer:
		return parseStaticBearerCreate(obj)
	case authEnvVar:
		return parseEnvVarCreate(obj)
	default:
		return nil, errInvalid(`auth.type must be "mcp_oauth", "static_bearer", or "environment_variable"`)
	}
}

func parseMCPOAuthCreate(obj map[string]json.RawMessage) (*credAuth, error) {
	if err := rejectUnknownKeys(obj, "type", "mcp_server_url", "access_token", "expires_at", "refresh"); err != nil {
		return nil, err
	}
	serverURL, err := requiredString(obj, "mcp_server_url")
	if err != nil {
		return nil, err
	}
	if err := validateEndpointURL(serverURL, "mcp_server_url"); err != nil {
		return nil, err
	}
	accessToken, err := requiredString(obj, "access_token")
	if err != nil {
		return nil, err
	}
	doc := mcpOAuthAuthJSON{Type: authMCPOAuth, MCPServerURL: serverURL}
	if doc.ExpiresAt, err = timeField(obj, "expires_at"); err != nil {
		return nil, err
	}
	secrets := map[string]string{"access_token": accessToken}
	if refreshRaw, ok := obj["refresh"]; ok && !isNull(refreshRaw) {
		refresh, refreshSecrets, err := parseRefreshCreate(refreshRaw)
		if err != nil {
			return nil, err
		}
		doc.Refresh = refresh
		for k, v := range refreshSecrets {
			secrets[k] = v
		}
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return &credAuth{authType: authMCPOAuth, doc: encoded, secrets: secrets, key: "url:" + serverURL}, nil
}

func parseRefreshCreate(raw json.RawMessage) (*refreshJSON, map[string]string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, nil, errInvalid("auth.refresh must be an object")
	}
	if err := rejectUnknownKeys(obj, "client_id", "refresh_token", "token_endpoint", "token_endpoint_auth", "resource", "scope"); err != nil {
		return nil, nil, err
	}
	out := &refreshJSON{}
	var err error
	if out.ClientID, err = requiredString(obj, "client_id"); err != nil {
		return nil, nil, err
	}
	if out.TokenEndpoint, err = requiredString(obj, "token_endpoint"); err != nil {
		return nil, nil, err
	}
	if err := validateEndpointURL(out.TokenEndpoint, "token_endpoint"); err != nil {
		return nil, nil, err
	}
	refreshToken, err := requiredString(obj, "refresh_token")
	if err != nil {
		return nil, nil, err
	}
	secrets := map[string]string{"refresh_token": refreshToken}
	authType, clientSecret, err := parseTokenEndpointAuth(obj["token_endpoint_auth"], true, true)
	if err != nil {
		return nil, nil, err
	}
	out.TokenEndpointAuth = tokenEndpointAuthJSON{Type: authType}
	if clientSecret != "" {
		secrets["client_secret"] = clientSecret
	}
	if out.Resource, err = optionalStringPtr(obj, "resource"); err != nil {
		return nil, nil, err
	}
	if out.Scope, err = optionalStringPtr(obj, "scope"); err != nil {
		return nil, nil, err
	}
	return out, secrets, nil
}

// parseTokenEndpointAuth parses the token_endpoint_auth arm. allowNone admits
// the "none" arm (create only — the update union drops it, per the SDK);
// secretRequired demands client_secret on the secret-bearing arms (create, or
// an update that switches arms and so cannot inherit a prior secret).
func parseTokenEndpointAuth(raw json.RawMessage, allowNone, secretRequired bool) (string, string, error) {
	if raw == nil || isNull(raw) {
		return "", "", errInvalid("auth.refresh.token_endpoint_auth is required")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return "", "", errInvalid("token_endpoint_auth must be an object")
	}
	var typ string
	if rawType, ok := obj["type"]; ok {
		_ = json.Unmarshal(rawType, &typ)
	}
	switch typ {
	case "none":
		if !allowNone {
			return "", "", errInvalid(`token_endpoint_auth cannot be changed to "none"`)
		}
		if err := rejectUnknownKeys(obj, "type"); err != nil {
			return "", "", err
		}
		return typ, "", nil
	case "client_secret_basic", "client_secret_post":
		if err := rejectUnknownKeys(obj, "type", "client_secret"); err != nil {
			return "", "", err
		}
		secret, set, null, err := stringField(obj, "client_secret")
		if err != nil {
			return "", "", err
		}
		if set && (null || secret == "") {
			return "", "", errInvalid("client_secret cannot be empty")
		}
		if !set && secretRequired {
			return "", "", errInvalid("client_secret is required for token_endpoint_auth %q", typ)
		}
		return typ, secret, nil
	default:
		return "", "", errInvalid(`token_endpoint_auth.type must be "none", "client_secret_basic", or "client_secret_post"`)
	}
}

func parseStaticBearerCreate(obj map[string]json.RawMessage) (*credAuth, error) {
	if err := rejectUnknownKeys(obj, "type", "mcp_server_url", "token"); err != nil {
		return nil, err
	}
	serverURL, err := requiredString(obj, "mcp_server_url")
	if err != nil {
		return nil, err
	}
	if err := validateEndpointURL(serverURL, "mcp_server_url"); err != nil {
		return nil, err
	}
	token, err := requiredString(obj, "token")
	if err != nil {
		return nil, err
	}
	doc, err := json.Marshal(staticBearerAuthJSON{Type: authStaticBearer, MCPServerURL: serverURL})
	if err != nil {
		return nil, err
	}
	return &credAuth{authType: authStaticBearer, doc: doc,
		secrets: map[string]string{"token": token}, key: "url:" + serverURL}, nil
}

func parseEnvVarCreate(obj map[string]json.RawMessage) (*credAuth, error) {
	if err := rejectUnknownKeys(obj, "type", "secret_name", "secret_value", "networking", "injection_location"); err != nil {
		return nil, err
	}
	secretName, err := requiredString(obj, "secret_name")
	if err != nil {
		return nil, err
	}
	secretValue, err := requiredString(obj, "secret_value")
	if err != nil {
		return nil, err
	}
	networking, err := parseCredNetworking(obj["networking"])
	if err != nil {
		return nil, err
	}
	// injection_location asymmetry (the public docs): omitting the whole
	// object enables both locations; a provided object defaults omitted
	// fields to false; explicit null — object or field — is a 400.
	loc := injectionLocationJSON{Body: true, Header: true}
	if locObj, present, err := objectField(obj, "injection_location"); err != nil {
		return nil, err
	} else if present {
		loc = injectionLocationJSON{}
		if err := parseInjectionLocationFields(locObj, &loc); err != nil {
			return nil, err
		}
	}
	if !loc.Body && !loc.Header {
		return nil, errInvalid("injection_location must enable at least one of body or header")
	}
	doc, err := json.Marshal(envVarAuthJSON{
		Type: authEnvVar, SecretName: secretName, Networking: networking, InjectionLocation: loc,
	})
	if err != nil {
		return nil, err
	}
	return &credAuth{authType: authEnvVar, doc: doc,
		secrets: map[string]string{"secret_value": secretValue}, key: "name:" + secretName}, nil
}

func parseInjectionLocationFields(obj map[string]json.RawMessage, loc *injectionLocationJSON) error {
	if err := rejectUnknownKeys(obj, "body", "header"); err != nil {
		return err
	}
	for key, dst := range map[string]*bool{"body": &loc.Body, "header": &loc.Header} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if isNull(raw) {
			return errInvalid("injection_location.%s cannot be null; omit the field instead", key)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return errInvalid("injection_location.%s must be a boolean", key)
		}
	}
	return nil
}

// parseCredNetworking validates the networking union and returns its
// normalized wire form. Update semantics are full replacement, so create and
// update share this parser.
func parseCredNetworking(raw json.RawMessage) (json.RawMessage, error) {
	if raw == nil || isNull(raw) {
		return nil, errInvalid("auth.networking is required")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errInvalid("networking must be an object")
	}
	var typ string
	if rawType, ok := obj["type"]; ok {
		_ = json.Unmarshal(rawType, &typ)
	}
	switch typ {
	case "unrestricted":
		if err := rejectUnknownKeys(obj, "type"); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"type":"unrestricted"}`), nil
	case "limited":
		if err := rejectUnknownKeys(obj, "type", "allowed_hosts"); err != nil {
			return nil, err
		}
		hosts := []string{}
		if rawHosts, ok := obj["allowed_hosts"]; ok && !isNull(rawHosts) {
			if err := json.Unmarshal(rawHosts, &hosts); err != nil {
				return nil, errInvalid("allowed_hosts must be a list of hostnames")
			}
		}
		// The SDK marks allowed_hosts required on the limited variant (omitzero
		// drops an empty list), so an omitted/null/empty list is a 400 rather
		// than a silently-accepted no-op.
		if len(hosts) == 0 {
			return nil, errInvalid("limited networking requires a non-empty allowed_hosts")
		}
		if len(hosts) > credentialAllowedHosts {
			return nil, errInvalid("allowed_hosts cannot exceed %d entries", credentialAllowedHosts)
		}
		for _, h := range hosts {
			if err := validateAllowedHost(h); err != nil {
				return nil, err
			}
		}
		return json.Marshal(struct {
			Type         string   `json:"type"`
			AllowedHosts []string `json:"allowed_hosts"`
		}{"limited", hosts})
	default:
		return nil, errInvalid(`networking.type must be "unrestricted" or "limited"`)
	}
}

// validateEndpointURL rejects a credential URL the validate probe would later
// dial if it is unparseable or not http(s) — a cheap up-front check; the
// connect-time IP guard (vaultvalidate.go) is the real SSRF boundary, since a
// hostname's address can change between create and probe.
func validateEndpointURL(raw, field string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errInvalid("%s is not a valid URL", field)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errInvalid("%s must be an http or https URL", field)
	}
	if u.Host == "" {
		return errInvalid("%s must include a host", field)
	}
	return nil
}

// validateAllowedHost enforces the documented entry grammar: a bare hostname,
// an IPv4 address, or a "*."-prefixed wildcard — no URLs, ports, paths, or
// IPv6. (IPv4 is hostname-shaped, so one label check covers both.)
func validateAllowedHost(h string) error {
	badf := func() error {
		return errInvalid("allowed_hosts entry %q is not a hostname, IPv4 address, or *.-wildcard", h)
	}
	wildcard := strings.HasPrefix(h, "*.")
	host := strings.TrimPrefix(h, "*.")
	if host == "" || strings.Contains(host, "*") {
		return errInvalid("allowed_hosts entry %q: a wildcard must be a \"*.\" prefix on a hostname", h)
	}
	// A dotted-numeric entry must be a valid IPv4 literal (so 999.999.999.999
	// is rejected), and a wildcard applies to hostnames only — never an IP.
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return errInvalid("allowed_hosts entry %q: IPv6 is not supported", h)
		}
		if wildcard {
			return errInvalid("allowed_hosts entry %q: a \"*.\" wildcard applies to hostnames, not IP addresses", h)
		}
		return nil
	}
	allNumeric := true
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return badf()
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
				allNumeric = false
			case r >= '0' && r <= '9':
			default:
				return badf()
			}
		}
	}
	// An all-numeric dotted string that net.ParseIP rejected is a malformed IP
	// (e.g. 999.999.999.999), not a hostname.
	if allNumeric {
		return badf()
	}
	return nil
}

// timeField parses an optional RFC 3339 field; explicit null yields nil.
func timeField(obj map[string]json.RawMessage, key string) (*time.Time, error) {
	raw, ok := obj[key]
	if !ok || isNull(raw) {
		return nil, nil
	}
	var t time.Time
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, errInvalid("%s must be an RFC 3339 timestamp", key)
	}
	// Normalize to UTC so it renders in the API's Z form, like every other
	// timestamp, rather than round-tripping a client-supplied +HH:MM offset.
	t = t.UTC()
	return &t, nil
}

// optionalStringPtr parses an optional string field; absent and explicit null
// both yield nil (the field renders null).
func optionalStringPtr(obj map[string]json.RawMessage, key string) (*string, error) {
	val, set, null, err := stringField(obj, key)
	if err != nil {
		return nil, err
	}
	if !set || null {
		return nil, nil
	}
	return &val, nil
}

// applyCredAuthUpdate merges an update-union patch onto the stored auth
// document. The variant is fixed at create (a type switch is a 400), and the
// immutable anchors are structurally absent from the update unions —
// mcp_server_url, secret_name, and refresh's client_id/token_endpoint/resource
// all reject as unknown keys. existingSecrets is the decrypted secret object;
// the returned map is the full post-merge secret set.
func applyCredAuthUpdate(raw json.RawMessage, authType string, existingDoc []byte,
	existingSecrets map[string]string) (newDoc []byte, newSecrets map[string]string, err error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, nil, errInvalid("auth must be an object")
	}
	var typ string
	if rawType, ok := obj["type"]; ok {
		_ = json.Unmarshal(rawType, &typ)
	}
	if typ == "" {
		return nil, nil, errInvalid("auth.type is required")
	}
	if typ != authType {
		return nil, nil, errInvalid("auth.type cannot be changed (from %s to %s)", authType, typ)
	}
	newSecrets = map[string]string{}
	for k, v := range existingSecrets {
		newSecrets[k] = v
	}
	switch authType {
	case authMCPOAuth:
		newDoc, err = applyMCPOAuthUpdate(obj, existingDoc, newSecrets)
	case authStaticBearer:
		if err := rejectUnknownKeys(obj, "type", "token"); err != nil {
			return nil, nil, err
		}
		if token, set, null, e := stringField(obj, "token"); e != nil {
			return nil, nil, e
		} else if set {
			if null || token == "" {
				return nil, nil, errInvalid("token cannot be cleared")
			}
			newSecrets["token"] = token
		}
		newDoc = existingDoc
	case authEnvVar:
		newDoc, err = applyEnvVarUpdate(obj, existingDoc, newSecrets)
	}
	if err != nil {
		return nil, nil, err
	}
	return newDoc, newSecrets, nil
}

func applyMCPOAuthUpdate(obj map[string]json.RawMessage, existingDoc []byte, secrets map[string]string) ([]byte, error) {
	if err := rejectUnknownKeys(obj, "type", "access_token", "expires_at", "refresh"); err != nil {
		return nil, err
	}
	var doc mcpOAuthAuthJSON
	if err := json.Unmarshal(existingDoc, &doc); err != nil {
		return nil, err
	}
	if token, set, null, err := stringField(obj, "access_token"); err != nil {
		return nil, err
	} else if set {
		if null || token == "" {
			return nil, errInvalid("access_token cannot be cleared")
		}
		secrets["access_token"] = token
	}
	if raw, ok := obj["expires_at"]; ok {
		if isNull(raw) {
			doc.ExpiresAt = nil
		} else {
			t, err := timeField(obj, "expires_at")
			if err != nil {
				return nil, err
			}
			doc.ExpiresAt = t
		}
	}
	if raw, ok := obj["refresh"]; ok && !isNull(raw) {
		// The update refresh union carries no client_id/token_endpoint (frozen
		// after create), so a refresh block cannot be introduced here — there
		// would be no anchors to build it from.
		if doc.Refresh == nil {
			return nil, errInvalid("refresh cannot be added after create")
		}
		var refreshObj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &refreshObj); err != nil || refreshObj == nil {
			return nil, errInvalid("auth.refresh must be an object")
		}
		if err := rejectUnknownKeys(refreshObj, "refresh_token", "scope", "token_endpoint_auth"); err != nil {
			return nil, err
		}
		if token, set, null, err := stringField(refreshObj, "refresh_token"); err != nil {
			return nil, err
		} else if set {
			if null || token == "" {
				return nil, errInvalid("refresh_token cannot be cleared")
			}
			secrets["refresh_token"] = token
		}
		if scope, set, null, err := stringField(refreshObj, "scope"); err != nil {
			return nil, err
		} else if set {
			if null {
				doc.Refresh.Scope = nil
			} else {
				doc.Refresh.Scope = &scope
			}
		}
		if rawAuth, ok := refreshObj["token_endpoint_auth"]; ok {
			// Switching arms cannot inherit the prior arm's secret, so the
			// switch demands a client_secret; restating the same arm keeps it.
			_, hadSecret := secrets["client_secret"]
			var probe struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(rawAuth, &probe)
			sameArm := probe.Type == doc.Refresh.TokenEndpointAuth.Type
			armType, clientSecret, err := parseTokenEndpointAuth(rawAuth, false, !(sameArm && hadSecret))
			if err != nil {
				return nil, err
			}
			doc.Refresh.TokenEndpointAuth.Type = armType
			if clientSecret != "" {
				secrets["client_secret"] = clientSecret
			}
		}
	}
	return json.Marshal(doc)
}

func applyEnvVarUpdate(obj map[string]json.RawMessage, existingDoc []byte, secrets map[string]string) ([]byte, error) {
	if err := rejectUnknownKeys(obj, "type", "secret_value", "networking", "injection_location"); err != nil {
		return nil, err
	}
	var doc envVarAuthJSON
	if err := json.Unmarshal(existingDoc, &doc); err != nil {
		return nil, err
	}
	if val, set, null, err := stringField(obj, "secret_value"); err != nil {
		return nil, err
	} else if set {
		if null || val == "" {
			return nil, errInvalid("secret_value cannot be cleared")
		}
		secrets["secret_value"] = val
	}
	if raw, ok := obj["networking"]; ok {
		// Full replacement, per the docs.
		networking, err := parseCredNetworking(raw)
		if err != nil {
			return nil, err
		}
		doc.Networking = networking
	}
	// Update merges injection_location field by field (the create-time
	// defaults-to-false rule is create-only).
	if locObj, present, err := objectField(obj, "injection_location"); err != nil {
		return nil, err
	} else if present {
		if err := parseInjectionLocationFields(locObj, &doc.InjectionLocation); err != nil {
			return nil, err
		}
		if !doc.InjectionLocation.Body && !doc.InjectionLocation.Header {
			return nil, errInvalid("injection_location must enable at least one of body or header")
		}
	}
	return json.Marshal(doc)
}
