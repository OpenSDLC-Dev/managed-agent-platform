package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestRungOneDatesAVersionAsTheSteeringGuardDoes. Two gates read sentences for
// a dated version: this rung, over the registry and the Go comments, and
// internal/domain/docs_test.go's guard over the steering documents. anyTag and
// datedBefore are that guard's sdkVersionLiteral and datingMarker, kept by hand,
// and two gates that disagreed about whether one sentence is dated would each be
// right about half of it.
//
// A test file cannot be imported, so the guard's patterns are read out of its
// source and compiled here. They are then asked the same questions rather than
// compared as text: datedBefore leaves the emphasis marker out of its classes,
// because the scanner blanks it first, so the two patterns are equal in what
// they answer and not in how they are spelt.
func TestRungOneDatesAVersionAsTheSteeringGuardDoes(t *testing.T) {
	path := filepath.Join(repoRoot(t), "internal", "domain", "docs_test.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	patterns := map[string]*regexp.Regexp{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
			return true
		}
		call, ok := spec.Values[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if src, ok := constString(call.Args[0]); ok {
			if re, err := regexp.Compile(src); err == nil {
				patterns[spec.Names[0].Name] = re
			}
		}
		return true
	})
	literal, marker := patterns["sdkVersionLiteral"], patterns["datingMarker"]
	if literal == nil || marker == nil {
		t.Fatalf("%s no longer declares sdkVersionLiteral and datingMarker as compiled "+
			"literals, so this cannot hold the two gates together", path)
	}

	if literal.String() != anyTag.String() {
		t.Errorf("the guard reads a version as %q and this rung as %q", literal, anyTag)
	}
	for _, before := range []string{
		"since ", "Since ", "SINCE ", "checked against ", "Checked Against ", "absent at ",
		"since\t", "since `", "since *", "since _", "since [", "since (",
		"since anthropic-sdk-go ", "since anthropic-sdk-go's ", "since anthropic-sdk-go’s ",
		"since anthropic-sdk-go'S ", "since anthropic-sdk-go’S ",
		"since `anthropic-sdk-go ", "since **anthropic-sdk-go** ", "since [anthropic-sdk-go](x) ",
		"checked against github.com/anthropics/anthropic-sdk-go@", "since the SDK ",
		"since SDK ", "unchecked against ", "sincere ", "checked  against ", "since",
		"it was true since ", "absent at the pinned ", "(since ",
	} {
		want := marker.MatchString(before)
		if got := datedBefore.MatchString(blankEmphasis(before)); got != want {
			t.Errorf("before %q: the guard reads a version as dated=%v, this rung as dated=%v",
				before, want, got)
		}
	}
}

// constString evaluates an expression built from string literals and `+`.
func constString(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok1 := constString(e.X)
		r, ok2 := constString(e.Y)
		return l + r, ok1 && ok2
	case *ast.ParenExpr:
		return constString(e.X)
	}
	return "", false
}
