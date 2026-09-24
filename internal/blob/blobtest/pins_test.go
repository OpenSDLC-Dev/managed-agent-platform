package blobtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
)

// TestTheShippedPinsAreTheHarnessPin holds Image to its promise that the
// contract tests exercise what ships: the compose stack's object store and the
// chart's default must name the identical image, digest included. The three
// pins have had to move together twice (#701, #799); a move that missed one
// would leave this suite green against a server nothing deploys.
func TestTheShippedPinsAreTheHarnessPin(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, pin := range []struct {
		file string
		path []string
	}{
		{"deploy/compose/docker-compose.yml", []string{"services", "minio", "image"}},
		{"deploy/helm/managed-agent-platform/values.yaml", []string{"minio", "image"}},
	} {
		src, err := os.ReadFile(filepath.Join(root, pin.file))
		if err != nil {
			t.Fatalf("read %s: %v", pin.file, err)
		}
		var node any
		if err := yaml.Unmarshal(src, &node); err != nil {
			t.Fatalf("parse %s: %v", pin.file, err)
		}
		for _, key := range pin.path {
			m, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("%s: no mapping at %q in %v", pin.file, key, pin.path)
			}
			node = m[key]
		}
		if node != blobtest.Image {
			t.Errorf("%s pins %v, want blobtest.Image %q", pin.file, node, blobtest.Image)
		}
	}
}
