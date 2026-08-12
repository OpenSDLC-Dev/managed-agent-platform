package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
)

// TestRoleAtLeast walks the whole 4x4 matrix, RoleNone on both sides included.
// The interesting rows are the ones a partial table would leave out: RoleNone as
// the holder satisfies nothing, and RoleNone as the minimum is satisfied by
// nothing — a route annotated with a role this package does not know denies
// rather than admits.
func TestRoleAtLeast(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		role identity.Role
		min  identity.Role
		want bool
	}{
		{name: "none/none", role: identity.RoleNone, min: identity.RoleNone, want: false},
		{name: "none/viewer", role: identity.RoleNone, min: identity.RoleViewer, want: false},
		{name: "none/developer", role: identity.RoleNone, min: identity.RoleDeveloper, want: false},
		{name: "none/admin", role: identity.RoleNone, min: identity.RoleAdmin, want: false},

		{name: "viewer/none", role: identity.RoleViewer, min: identity.RoleNone, want: false},
		{name: "viewer/viewer", role: identity.RoleViewer, min: identity.RoleViewer, want: true},
		{name: "viewer/developer", role: identity.RoleViewer, min: identity.RoleDeveloper, want: false},
		{name: "viewer/admin", role: identity.RoleViewer, min: identity.RoleAdmin, want: false},

		{name: "developer/none", role: identity.RoleDeveloper, min: identity.RoleNone, want: false},
		{name: "developer/viewer", role: identity.RoleDeveloper, min: identity.RoleViewer, want: true},
		{name: "developer/developer", role: identity.RoleDeveloper, min: identity.RoleDeveloper, want: true},
		{name: "developer/admin", role: identity.RoleDeveloper, min: identity.RoleAdmin, want: false},

		{name: "admin/none", role: identity.RoleAdmin, min: identity.RoleNone, want: false},
		{name: "admin/viewer", role: identity.RoleAdmin, min: identity.RoleViewer, want: true},
		{name: "admin/developer", role: identity.RoleAdmin, min: identity.RoleDeveloper, want: true},
		{name: "admin/admin", role: identity.RoleAdmin, min: identity.RoleAdmin, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.role.AtLeast(tc.min); got != tc.want {
				t.Errorf("Role(%q).AtLeast(%q) = %v, want %v", string(tc.role), string(tc.min), got, tc.want)
			}
		})
	}
}

// TestRoleAtLeastRejectsUnknownMinimum pins the fail-closed end of the order: a
// minimum outside the three roles is satisfied by nobody, so a typo in a route
// annotation denies every caller instead of admitting every caller.
func TestRoleAtLeastRejectsUnknownMinimum(t *testing.T) {
	t.Parallel()
	for _, min := range []identity.Role{"owner", "", "Admin", "viewer ", "root"} {
		for _, role := range []identity.Role{
			identity.RoleNone, identity.RoleViewer, identity.RoleDeveloper, identity.RoleAdmin,
		} {
			if role.AtLeast(min) {
				t.Errorf("Role(%q).AtLeast(%q) = true, want false for a minimum outside the three roles",
					string(role), string(min))
			}
		}
	}
}

func TestParseRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want identity.Role
		ok   bool
	}{
		{name: "viewer", in: "viewer", want: identity.RoleViewer, ok: true},
		{name: "developer", in: "developer", want: identity.RoleDeveloper, ok: true},
		{name: "admin", in: "admin", want: identity.RoleAdmin, ok: true},
		// Case, whitespace and near-misses are rejected rather than coerced: the
		// role map's targets come from operator configuration, and a silently
		// accepted "Admin" would be an authority grant nobody wrote.
		{name: "capitalised admin", in: "Admin"},
		{name: "unknown role", in: "owner"},
		{name: "empty", in: ""},
		{name: "padded", in: " admin"},
		{name: "the none sentinel is not a name", in: string(identity.RoleNone)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := identity.ParseRole(tc.in)
			if ok != tc.ok {
				t.Fatalf("ParseRole(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			want := tc.want
			if !tc.ok {
				want = identity.RoleNone
			}
			if got != want {
				t.Errorf("ParseRole(%q) = %q, want %q", tc.in, string(got), string(want))
			}
		})
	}
}

// TestLooksLikeJWT pins the compact-JWS silhouette. This is routing, not
// security: it is what keeps an sk-map-env01- environment key on the worker lane
// and off the human lane, so a real minted token must match and a real
// environment key must not.
func TestLooksLikeJWT(t *testing.T) {
	t.Parallel()
	idp := identitytest.NewIdP(t)
	minted := idp.Mint(t, idp.Claims("console", time.Now()))

	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{name: "three base64url segments", in: "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1LTEifQ.c2ln", want: true},
		{name: "the whole base64url alphabet", in: "aZ09-_.aZ09-_.aZ09-_", want: true},
		{name: "a real minted token", in: minted, want: true},

		{name: "an environment key", in: "sk-map-env01-abc"},
		{name: "no dots at all", in: "aaabbbccc"},
		{name: "one dot", in: "aaa.bbb"},
		{name: "three dots", in: "aaa.bbb.ccc.ddd"},
		{name: "two dots with an empty middle segment", in: "aaa..ccc"},
		{name: "empty first segment", in: ".bbb.ccc"},
		{name: "empty third segment", in: "aaa.bbb."},
		{name: "only dots", in: ".."},
		{name: "empty string", in: ""},
		// Standard base64 rather than base64url: '+', '/' and '=' are outside the
		// alphabet a compact JWS uses.
		{name: "standard-base64 plus", in: "aa+a.bbb.ccc"},
		{name: "standard-base64 slash", in: "aaa.bb/b.ccc"},
		{name: "base64 padding", in: "aaa.bbb.ccc="},
		{name: "a non-alphabet byte", in: "aaa.bb b.ccc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := identity.LooksLikeJWT(tc.in); got != tc.want {
				t.Errorf("LooksLikeJWT(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestErrorIsOneConstantString pins the uniform-rejection contract at the type:
// Error() is one constant string whatever the cause, errors.Is reaches
// ErrUnauthenticated, and the detail is reachable only through Reason(). Two
// rejections from different steps of the pipeline — the size cap, which answers
// before any decode, and the parse — carry different reasons behind the same
// rendered text.
func TestErrorIsOneConstantString(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	idp := identitytest.NewIdP(t)
	v, err := identity.New(ctx, identity.Config{
		Mode:     identity.ModeOIDC,
		Issuer:   idp.Issuer(),
		Audience: "console",
		// Pinned, so New skips discovery; the fixture is reachable only through
		// its own client, because the production client's dial guard refuses
		// loopback by design.
		JWKSURL:    idp.JWKSURL(),
		RoleMap:    map[string]identity.Role{"platform-admins": identity.RoleAdmin},
		HTTPClient: idp.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, oversize := v.Verify(ctx, strings.Repeat("a", identity.MaxTokenBytesForTest+1))
	_, malformed := v.Verify(ctx, "sk-map-env01-not-a-jwt")

	reasons := make([]string, 0, 2)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "token over the size cap", err: oversize},
		{name: "not a JWS at all", err: malformed},
	} {
		if tc.err == nil {
			t.Fatalf("%s: Verify returned no error", tc.name)
		}
		if got := tc.err.Error(); got != "authentication failed" {
			t.Errorf("%s: Error() = %q, want %q", tc.name, got, "authentication failed")
		}
		if !errors.Is(tc.err, identity.ErrUnauthenticated) {
			t.Errorf("%s: errors.Is(err, ErrUnauthenticated) = false, want true", tc.name)
		}
		var ie *identity.Error
		if !errors.As(tc.err, &ie) {
			t.Fatalf("%s: errors.As(err, **identity.Error) = false, want true", tc.name)
		}
		if ie.Reason() == "" {
			t.Errorf("%s: Reason() is empty; the detail must be carried, just not rendered", tc.name)
		}
		if strings.Contains(tc.err.Error(), ie.Reason()) {
			t.Errorf("%s: Error() = %q leaks the reason %q", tc.name, tc.err.Error(), ie.Reason())
		}
		reasons = append(reasons, ie.Reason())
	}

	if oversize.Error() != malformed.Error() {
		t.Errorf("two causes render differently: %q vs %q — that is an oracle",
			oversize.Error(), malformed.Error())
	}
	if reasons[0] == reasons[1] {
		t.Errorf("both paths report reason %q; Reason() is meant to carry the detail Error() withholds",
			reasons[0])
	}
}
