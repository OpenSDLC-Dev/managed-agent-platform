package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
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
// The three lanes that identity never reaches are held to the opposite rule: the
// work API and the gate-config route must stay at RoleNone, because a role there
// would be enforcement on a lane that has no roles to enforce, and a reader
// finding one would reasonably conclude humans can reach it.

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
			if reg.role != "" && reg.role != "RoleNone" {
				t.Errorf("server.go:%d: %s is on the %s lane, which identity never reaches, but declares %s; a role here reads as though a human could arrive",
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
	if strings.Contains(pattern, "consoleTokensPath") || strings.Contains(pattern, "consoleRevokePath") {
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
		if !ok || sel.Sel.Name != "HandleFunc" {
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
			return strings.Trim(v.Value, `"`)
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
