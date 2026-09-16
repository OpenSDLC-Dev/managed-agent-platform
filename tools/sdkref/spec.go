package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SpecPath is where the SDK bundles its OpenAPI document. It carries the
// constraints the generated Go types drop, which is why the registry cites it
// at all — and it is a tree, which is why a spec citation takes a schema path
// and is refused the `non-go` span reason.
const SpecPath = "scripts/mock-spec.json.gz"

// SpecSource is the one source that bundles a spec, so the only one a schema
// path can be a locator into.
const SpecSource = "anthropic-sdk-go"

// Spec resolves dotted schema paths against the bundled document.
type Spec struct {
	once sync.Once
	dir  string
	root any
	err  error
}

// NewSpec prepares a lazy reader over the spec inside a module directory. It is
// lazy because a corpus with no spec citation should not pay to decompress it,
// and because the failure to find one is a finding for the citations that
// needed it rather than for the whole run.
func NewSpec(moduleDir string) *Spec { return &Spec{dir: moduleDir} }

func (s *Spec) load() {
	s.once.Do(func() {
		p := filepath.Join(s.dir, SpecPath)
		f, err := os.Open(p)
		if err != nil {
			s.err = fmt.Errorf("open %s: %w", SpecPath, err)
			return
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			s.err = fmt.Errorf("gunzip %s: %w", SpecPath, err)
			return
		}
		defer zr.Close()
		if err := json.NewDecoder(zr).Decode(&s.root); err != nil {
			s.err = fmt.Errorf("decode %s: %w", SpecPath, err)
		}
	})
}

// Available reports whether the module ships a spec at all. Older tags shipped
// none, which is the reason #660 carries the spec citations separately: they
// name a source nobody can open at the tag they give.
func (s *Spec) Available() bool {
	s.load()
	return s.err == nil
}

// Err reports why the spec could not be read, for the uncheckable list.
func (s *Spec) Err() error {
	s.load()
	return s.err
}

// Resolve walks a dotted path such as `components.schemas.BetaSession`. Like the
// Go resolver, absence is an answer rather than an error: an `absent at` spec
// citation asserts exactly that.
func (s *Spec) Resolve(path string) (bool, error) {
	s.load()
	if s.err != nil {
		return false, s.err
	}
	cur := s.root
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return false, nil
		}
		if cur, ok = obj[seg]; !ok {
			return false, nil
		}
	}
	return true, nil
}
