package egress_test

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

func TestPlaceholder(t *testing.T) {
	const sess = "sesn_abc"

	// Stability is the load-bearing property: the same (session, secret_name)
	// derives the same token every time, so a re-provision or the egress gate
	// recovers the exact placeholder already baked into the sandbox.
	if a, b := egress.Placeholder(sess, "API_KEY"), egress.Placeholder(sess, "API_KEY"); a != b {
		t.Fatalf("placeholder is not stable: %q vs %q", a, b)
	}
	// A golden vector pins the exact derivation (separator, hash, truncation,
	// encoding). The formula is a cross-version contract: during a rolling
	// upgrade a new executor must derive the same token an older one baked into a
	// running sandbox, so an accidental change to it must fail here, loudly.
	if got := egress.Placeholder("sesn_abc", "API_KEY"); got != "vltph_c608ad05f5fe37adfa275fcf7ad0bc99" {
		t.Fatalf("derivation changed: Placeholder(sesn_abc, API_KEY) = %q", got)
	}
	// Distinct secret_names, and the same secret_name under a different session,
	// derive distinct tokens — no cross-name or cross-session collision, and the
	// NUL separator keeps ("a","bc") from colliding with ("ab","c").
	seen := map[string]struct{}{}
	for _, in := range []struct{ session, name string }{
		{sess, "API_KEY"}, {sess, "DB_URL"}, {"sesn_xyz", "API_KEY"},
		{"a", "bc"}, {"ab", "c"},
	} {
		p := egress.Placeholder(in.session, in.name)
		if !strings.HasPrefix(p, egress.PlaceholderPrefix) {
			t.Fatalf("placeholder %q lacks prefix %q", p, egress.PlaceholderPrefix)
		}
		// The suffix is the documented 128 bits in hex — exactly 32 lowercase-hex
		// chars. Pinning the exact format catches a width regression (e.g. a drop
		// to 96 bits) a loose length check would miss.
		suffix := strings.TrimPrefix(p, egress.PlaceholderPrefix)
		if len(suffix) != 32 {
			t.Fatalf("placeholder %q suffix is %d chars, want 32", p, len(suffix))
		}
		for _, r := range suffix {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Fatalf("placeholder %q suffix is not lowercase hex", p)
			}
		}
		// A valid env-var value: no spaces or shell metacharacters that would
		// break injection through Spec.Env.
		if strings.ContainsAny(p, " \t\n\"'$") {
			t.Fatalf("placeholder %q contains an unsafe character", p)
		}
		if _, dup := seen[p]; dup {
			t.Fatalf("placeholder %q collided across distinct inputs", p)
		}
		seen[p] = struct{}{}
	}
}

// cred is a small builder for test credentials.
func cred(placeholder, secret string, hosts []string, header, body bool) egress.Credential {
	return egress.Credential{
		Placeholder: placeholder, Secret: secret,
		Hosts: egress.NewHostSet(hosts), Header: header, Body: body,
	}
}

func TestEngineSubstitute(t *testing.T) {
	eng := egress.NewEngine([]egress.Credential{
		// Header-only, allowed on api.example.com.
		cred("vltph_tok", "sk-secret", []string{"api.example.com"}, true, false),
		// Body-only, allowed on *.upload.test.
		cred("vltph_up", "up-secret", []string{"*.upload.test"}, false, true),
	})

	t.Run("substitutes in an enabled location for an allowed host", func(t *testing.T) {
		out, unreachable := eng.Substitute("api.example.com", egress.LocationHeader, "Bearer vltph_tok")
		if out != "Bearer sk-secret" {
			t.Errorf("out = %q, want %q", out, "Bearer sk-secret")
		}
		if len(unreachable) != 0 {
			t.Errorf("unreachable = %v, want none", unreachable)
		}
	})

	t.Run("a disabled location is left literal, never unreachable", func(t *testing.T) {
		// vltph_tok is header-only; in a body it passes through untouched.
		out, unreachable := eng.Substitute("api.example.com", egress.LocationBody, "token=vltph_tok")
		if out != "token=vltph_tok" {
			t.Errorf("disabled-location value changed: %q", out)
		}
		if len(unreachable) != 0 {
			t.Errorf("a disabled location must not be host-unreachable: %v", unreachable)
		}
	})

	t.Run("an enabled location on a disallowed host is unreachable and left literal", func(t *testing.T) {
		out, unreachable := eng.Substitute("evil.test", egress.LocationHeader, "Bearer vltph_tok")
		if strings.Contains(out, "sk-secret") {
			t.Fatalf("secret leaked to a disallowed host: %q", out)
		}
		if out != "Bearer vltph_tok" {
			t.Errorf("out = %q, want the literal placeholder", out)
		}
		if len(unreachable) != 1 || unreachable[0].Secret != "sk-secret" {
			t.Fatalf("unreachable = %v, want the one credential", unreachable)
		}
	})

	t.Run("wildcard host, body location", func(t *testing.T) {
		out, unreachable := eng.Substitute("a.upload.test", egress.LocationBody, `{"key":"vltph_up"}`)
		if out != `{"key":"up-secret"}` {
			t.Errorf("out = %q", out)
		}
		if len(unreachable) != 0 {
			t.Errorf("unreachable = %v", unreachable)
		}
	})

	t.Run("multiple occurrences of one placeholder", func(t *testing.T) {
		out, _ := eng.Substitute("api.example.com", egress.LocationHeader, "vltph_tok vltph_tok")
		if out != "sk-secret sk-secret" {
			t.Errorf("out = %q", out)
		}
	})

	t.Run("a credential unreachable in two spots is reported once", func(t *testing.T) {
		_, unreachable := eng.Substitute("evil.test", egress.LocationHeader, "vltph_tok/vltph_tok")
		if len(unreachable) != 1 {
			t.Errorf("unreachable count = %d, want 1 (deduped)", len(unreachable))
		}
	})

	t.Run("no placeholders present is a no-op", func(t *testing.T) {
		out, unreachable := eng.Substitute("api.example.com", egress.LocationHeader, "nothing here")
		if out != "nothing here" || len(unreachable) != 0 {
			t.Errorf("out=%q unreachable=%v", out, unreachable)
		}
	})

	// A secret whose value happens to contain another credential's placeholder
	// must not itself be rewritten: substitution is one pass, order-independent.
	// Both placeholders appear in the input (defeating the Contains pre-filter),
	// and both credential orders are exercised — a sequential re-scanning
	// implementation corrupts secret A's embedded vltph_b in at least one order,
	// so testing both pins the single-pass property no matter the iteration order.
	t.Run("a secret containing a placeholder is not re-substituted", func(t *testing.T) {
		a := cred("vltph_a", "prefix-vltph_b-suffix", []string{"h.test"}, true, false)
		b := cred("vltph_b", "SECRET_B", []string{"h.test"}, true, false)
		for _, order := range [][]egress.Credential{{a, b}, {b, a}} {
			out, _ := egress.NewEngine(order).Substitute("h.test", egress.LocationHeader, "vltph_a vltph_b")
			if out != "prefix-vltph_b-suffix SECRET_B" {
				t.Errorf("order-dependent result: out = %q, want the single-pass output", out)
			}
		}
	})

	// The gate's real case: one string carrying two different placeholders whose
	// credentials disagree on the host — the admitted one is substituted, the
	// disallowed one stays a literal placeholder and is the only one reported.
	t.Run("mixed admitted and unreachable credentials in one string", func(t *testing.T) {
		eng := egress.NewEngine([]egress.Credential{
			cred("vltph_ok", "S-OK", []string{"good.test"}, true, false),
			cred("vltph_no", "S-NO", []string{"other.test"}, true, false),
		})
		out, unreachable := eng.Substitute("good.test", egress.LocationHeader, "a=vltph_ok b=vltph_no")
		if out != "a=S-OK b=vltph_no" {
			t.Errorf("out = %q", out)
		}
		if strings.Contains(out, "S-NO") {
			t.Fatal("disallowed credential's secret leaked")
		}
		if len(unreachable) != 1 || unreachable[0].Secret != "S-NO" {
			t.Fatalf("unreachable = %v, want only the disallowed credential", unreachable)
		}
	})

	// A credential enabled for both locations (the create-time default) is
	// substituted in either.
	t.Run("a both-locations credential works in header and body", func(t *testing.T) {
		eng := egress.NewEngine([]egress.Credential{
			cred("vltph_both", "S-BOTH", []string{"h.test"}, true, true),
		})
		for _, loc := range []egress.Location{egress.LocationHeader, egress.LocationBody} {
			out, _ := eng.Substitute("h.test", loc, "x vltph_both")
			if out != "x S-BOTH" {
				t.Errorf("loc %v: out = %q", loc, out)
			}
		}
	})

	// An engine with no credentials is a pure pass-through.
	t.Run("empty engine is a no-op", func(t *testing.T) {
		out, unreachable := egress.NewEngine(nil).Substitute("h.test", egress.LocationHeader, "vltph_x")
		if out != "vltph_x" || len(unreachable) != 0 {
			t.Errorf("out=%q unreachable=%v", out, unreachable)
		}
	})
}
