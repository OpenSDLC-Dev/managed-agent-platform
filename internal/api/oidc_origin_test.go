package api_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

// Every accepted native origin must also work as this install's RP callback.
// This is browser navigation, not an outbound IdP URL or a production dial.
func TestNativeOIDCOriginCallbackAgreement(t *testing.T) {
	idp := identitytest.NewIdP(t)
	ic := identity.Config{Mode: identity.ModeOIDC, Issuer: idp.Issuer(), Audience: "console", JWKSURL: idp.JWKSURL(), HTTPClient: idp.Client()}
	for _, row := range []struct {
		origin              string
		development, accept bool
	}{
		{"http://127.0.0.1:3000", true, true},
		{"http://127.0.0.2:3000", true, true},
		{"http://127.255.255.254:3000", true, true},
		{"http://localhost:3000", true, true},
		{"http://[::1]:3000", true, true},
		{"https://127.0.0.2:3000", false, true},
		{"https://console.example", false, true},
		{"https://xn--bcher-kva.example", false, true},
		{"http://127.1:3000", true, false},
		{"http://127.000.0.2:3000", true, false},
		{"http://0x7f000002:3000", true, false},
		{"http://LocalHost:3000", true, false},
		{"http://localhost:80", true, false},
		{"http://[0:0:0:0:0:0:0:1]:3000", true, false},
		{"http://[::ffff:127.0.0.2]:3000", true, false},
		{"http://192.0.2.1:3000", true, false},
		{"http://127.0.0.2:3000", false, false},
		{"http://localhost:3000", false, false},
		{"https://console.example:443", false, false},
	} {
		t.Run(fmt.Sprintf("%s/dev=%t", row.origin, row.development), func(t *testing.T) {
			env := map[string]string{"IDENTITY_MODE": "oidc", "CONSOLE_PUBLIC_ORIGIN": row.origin, "DEVELOPMENT_MODE": fmt.Sprint(row.development), "IDENTITY_OIDC_CLIENT_ID": "console", "IDENTITY_OIDC_AUDIENCE": "console"}
			cfg, err := login.ConfigFromEnv(func(k string) string { return env[k] })
			if (err == nil) != row.accept {
				t.Fatalf("config accept=%v err=%v", row.accept, err)
			}
			if !row.accept {
				return
			}
			if !login.New(nil, cfg).NativeEnabled() {
				t.Fatal("accepted origin did not enable Manager")
			}
			rp, err := identity.NewRelyingParty(context.Background(), ic, cfg.OIDCClientID, "", cfg.PublicOrigin+"/api/auth/callback")
			if err != nil || rp == nil {
				t.Fatalf("accepted native origin failed RP callback: %v", err)
			}
		})
	}
}
