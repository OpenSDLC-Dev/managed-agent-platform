package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// The completeness test. Every route this platform serves is registered in one
// place — NewHandler in server.go — and every route the identity lane can reach
// must declare a minimum role beside its path. Nothing at runtime can prove that:
// http.ServeMux does not expose its patterns, so a request-driven test can only
// check the routes someone remembered to list. This reads the registrations from
// the source instead, which is why a route added tomorrow without a role fails
// here rather than silently inheriting slice 2's deny-everyone floor.
//
// The machine lanes are held to the opposite rule: the work API and the
// gate-config route must stay at RoleNone. Not because a role there would be
// harmless clutter, but because it would be wrong — RoleNone is what those
// registrations rely on to stay closed.
//
// It would be comfortable to say identity can never reach them, and that is what
// an earlier draft of this comment said. It is not true. dispatchAuth classifies
// r.URL.EscapedPath(), while ServeMux unescapes each segment before matching, so
// a request for /v1/environments/{id}/%77ork misses isWorkPath, takes the human
// lane, and then matches the work registration once ServeMux decodes it. Nothing
// is open — requireRole(RoleNone) refuses every human before the handler runs —
// but that is the point: RoleNone is load-bearing on those routes, not
// decorative, and TestAnEncodedPathCannotSlipPastTheWorkLane pins it.

// routeReg is one mux.HandleFunc registration as it appears in the source.
type routeReg struct {
	line    int
	pattern string // rendered: "GET /v1/agents", or `"POST "+consoleTokensPath`
	role    string // "RoleViewer" … ; empty when the handler declares none
	isFunc  bool   // the handler is a bare closure, not one of the adapters
}

func TestEveryIdentityReachableRouteDeclaresARole(t *testing.T) {
	regs := parseRoutes(t, "server.go")
	if len(regs) < 60 {
		t.Fatalf("parsed %d registrations from server.go; the route table is far larger — the parser is broken, not the table", len(regs))
	}

	var annotated int
	for _, reg := range regs {
		lane := laneOf(reg.pattern)

		switch lane {
		case laneFallback:
			// Not a route: the 404/405 closures. They answer for themselves and
			// never reach a handler, so a role would be meaningless — but a
			// TYPED handler landing here means laneOf failed to classify a real
			// route, which would silently exempt it from the rule above.
			if !reg.isFunc {
				t.Errorf("server.go:%d: %s is served by an adapter but classified as a fallback closure; laneOf does not recognise this path",
					reg.line, reg.pattern)
			}
		case laneWork, laneGate:
			// RoleNone exactly — not "anything that isn't a real role". A typed
			// handler registered bare, with no adapter and so no role at all,
			// would otherwise pass here, and that is the one shape with no
			// requireRole call anywhere in its chain: reachable on the human lane
			// through the encoded spelling above, with nothing to refuse it.
			if !reg.isFunc && reg.role != "RoleNone" {
				t.Errorf("server.go:%d: %s is on the %s lane and declares %q; it must declare RoleNone, which is what refuses a human who arrives by an encoded path",
					reg.line, reg.pattern, lane, reg.role)
			}
		case laneManagement, laneConsole:
			switch reg.role {
			case "":
				t.Errorf("server.go:%d: %s declares no minimum role; every identity-reachable route must state one beside its path",
					reg.line, reg.pattern)
			case "RoleNone":
				t.Errorf("server.go:%d: %s is still at RoleNone, which denies every human; slice 3 annotates it per the plan's matrix",
					reg.line, reg.pattern)
			default:
				if _, real := identity.ParseRole(roleValue(reg.role)); !real {
					t.Errorf("server.go:%d: %s declares %s, which is not one of the three roles", reg.line, reg.pattern, reg.role)
					continue
				}
				annotated++
			}
		}
	}

	if annotated == 0 {
		t.Error("no route carries a real role; the matrix is not applied at all")
	}
}

// TestTheMatrixCoversEveryAnnotatedRoute joins the two halves of the guard.
// TestEveryIdentityReachableRouteDeclaresARole proves each route declares SOME
// valid role; TestRoleMatrixIsEnforcedOnEveryRoute proves the roles in its own
// table are enforced. Neither notices a route that exists in the source but not
// in that table — a new credential route annotated `developer` would satisfy the
// first and never be requested by the second, and an admin surface would be open
// to developers with the suite green. This compares the two lists directly.
//
// Console routes are excluded: their patterns are built from constants rather
// than literals, and they need a real environment to address, so they are
// covered by TestIdentityLaneEnvironmentKeyRoutesRequireAdmin and
// TestAnAdminCanWorkTheEnvironmentKeySurfaceEndToEnd — and, for the
// management-key section, TestIdentityLaneAPIKeyRoutesRequireAdmin and
// TestAnAdminCanWorkTheAPIKeySurfaceEndToEnd — instead.
func TestTheMatrixCoversEveryAnnotatedRoute(t *testing.T) {
	inSource := map[string]bool{}
	for _, reg := range parseRoutes(t, "server.go") {
		if laneOf(reg.pattern) == laneManagement {
			inSource[reg.pattern] = true
		}
	}

	inTable := map[string]bool{}
	for _, route := range roleMatrix() {
		inTable[route.pattern()] = true
	}

	for pattern := range inSource {
		if !inTable[pattern] {
			t.Errorf("%s is registered in server.go but absent from roleMatrix(); no test ever asks who may call it", pattern)
		}
	}
	for pattern := range inTable {
		if !inSource[pattern] {
			t.Errorf("roleMatrix() lists %s, which no longer exists in server.go; the table is asserting against a route that is gone", pattern)
		}
	}
}

// TestAnEncodedPathCannotSlipPastTheWorkLane pins the reason the work routes
// keep RoleNone. dispatchAuth classifies the escaped path and ServeMux matches
// on the unescaped one, so these spellings take the HUMAN lane and then land on
// a machine-lane handler. RoleNone is what closes them, and it closes them for
// an admin — the strongest role there is — because RoleNone is satisfied by
// nothing.
//
// This also keeps a property TestIdentityLaneDefaultDenies used to hold and can
// no longer reach: that a mapped, maximally-privileged human is still refused by
// a route whose minimum is RoleNone.
func TestAnEncodedPathCannotSlipPastTheWorkLane(t *testing.T) {
	s := newLaneServer(t)
	envID := s.env()
	admin := s.token("platform-admins")

	for _, path := range []string{
		"/v1/environments/" + envID + "/%77ork",
		"/v1/environments/" + envID + "/%77ork/poll",
		"/internal/v1/gate/%63onfig",
	} {
		// 403 permission_error carrying the identity lane's OWN message, not
		// merely "some refusal": that triple is requireRole turning the request
		// away on the identity lane. A 401 authentication_error would mean a
		// machine lane answered, and a 404 that no route matched — either would
		// mean this test is no longer exercising the path it was written for, and
		// the invariant would go unguarded while the test still passed.
		//
		// The message is asserted because the status pair stopped discriminating:
		// workScope now refuses a wrong-environment caller with 403
		// permission_error too (recorded 2026-09-02, #78), and nothing on the
		// identity lane sets the request's environment, so dropping RoleNone from
		// these routes would let the admin token reach workScope and be refused
		// there with the identical pair — green test, unguarded invariant. Only
		// the message tells the two refusals apart.
		status, errType, body := laneRead(t, s.bearer(http.MethodGet, path, admin, nil))
		if status != http.StatusForbidden || errType != "permission_error" {
			t.Errorf("GET %s as an admin: status %d, error %q; want 403 permission_error — the encoded spelling takes the human lane, and RoleNone is what closes it",
				path, status, errType)
			continue
		}
		if msg := laneMessage(t, body); msg != "this route is not available to SSO-authenticated callers" {
			t.Errorf("GET %s as an admin: message %q; want the identity lane's own refusal — another 403 here means the request got past requireRole and was turned away deeper in",
				path, msg)
		}
	}
}

// roleValue turns the identifier "RoleViewer" into the wire value "viewer" that
// identity.ParseRole accepts, so the test validates against the real parser
// rather than a second list of role names that could drift from it.
func roleValue(ident string) string {
	return strings.ToLower(strings.TrimPrefix(ident, "Role"))
}

type lane string

const (
	laneManagement lane = "management"
	laneConsole    lane = "console"
	laneWork       lane = "work"
	laneGate       lane = "gate"
	laneFallback   lane = "fallback"
)

// laneOf classifies a rendered registration pattern the way dispatchAuth
// classifies a live request path. It deliberately mirrors isWorkPath's shape
// rather than matching "work" anywhere: /v1/environments/{id} and
// /v1/environments/{id}/archive are management routes.
func laneOf(pattern string) lane {
	if strings.Contains(pattern, "gateconfig.Path") {
		return laneGate
	}
	// Every console-API route registers `"METHOD "+consoleSomethingPath`, which
	// render() spells `METHOD +consoleSomethingPath`. Matching on "+console" is
	// therefore a test for *a constant of that family*, not for the word: a bare
	// "console" anywhere would also claim a literal route like
	// `GET /v1/console_settings`, which dispatches through the management lane in
	// production and would then be excluded from the matrix join — free to declare
	// an over-permissive role with the suite green. The family form is still the
	// right shape rather than a list of the constants that exist today, because a
	// list would classify the next console route as a fallback closure, and
	// fallback is the one lane exempt from the declares-a-role rule.
	if strings.Contains(pattern, "+console") {
		return laneConsole
	}

	path := pattern
	if i := strings.IndexByte(path, ' '); i >= 0 {
		path = path[i+1:] // drop the method
	}
	if !strings.HasPrefix(path, "/v1/") {
		return laneFallback // a loop variable, "/", or another closure pattern
	}
	if rest, ok := strings.CutPrefix(path, "/v1/environments/"); ok {
		if _, after, found := strings.Cut(rest, "/"); found {
			if after == "work" || strings.HasPrefix(after, "work/") {
				return laneWork
			}
		}
	}
	if strings.HasSuffix(path, "/") {
		return laneFallback // a subtree fallback, not a route
	}
	return laneManagement
}

// parseRoutes reads every mux.HandleFunc registration out of the given file in
// the package directory, with the role each one declares.
func parseRoutes(t *testing.T, file string) []routeReg {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var regs []routeReg
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		// Handle as well as HandleFunc: both register a route on the same mux,
		// and recognising only one would leave the other invisible to this test
		// and to TestTheMatrixCoversEveryAnnotatedRoute — a hole in exactly the
		// guard those two are here to provide.
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "mux" {
			return true
		}
		_, isFunc := call.Args[1].(*ast.FuncLit)
		regs = append(regs, routeReg{
			line:    fset.Position(call.Pos()).Line,
			pattern: render(call.Args[0]),
			role:    roleIn(call.Args[1]),
			isFunc:  isFunc,
		})
		return true
	})
	return regs
}

// render turns a pattern expression back into readable text: the string value
// for a literal, and the source spelling for anything built from constants, so
// the classifier and the failure message both name what the reader sees.
func render(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			// strconv.Unquote, not strings.Trim: Trim strips every leading and
			// trailing quote and leaves escapes in place, so a backtick-quoted
			// pattern would render with its backticks, lose its /v1/ prefix, and
			// be silently reclassified as a closure rather than a route.
			if s, err := strconv.Unquote(v.Value); err == nil {
				return s
			}
		}
	case *ast.BinaryExpr:
		return render(v.X) + "+" + render(v.Y)
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return render(v.X) + "." + v.Sel.Name
	}
	return "?"
}

// roleIn finds the identity.RoleX a handler expression declares, looking through
// whatever wraps it (noStore, and the adapters themselves).
func roleIn(e ast.Expr) string {
	var found string
	ast.Inspect(e, func(n ast.Node) bool {
		// Stop at the FIRST role constant. Returning false below prunes only the
		// matched node's children — the walk still visits its siblings — so
		// without this guard a handler expression naming two roles would report
		// whichever came last, and the test could certify a route as admin while
		// requireRole enforces viewer.
		if found != "" {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "identity" && strings.HasPrefix(sel.Sel.Name, "Role") {
			found = sel.Sel.Name
			return false
		}
		return true
	})
	return found
}
