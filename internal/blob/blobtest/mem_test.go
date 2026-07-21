package blobtest_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
)

// No TestMain here on purpose: Mem needs no MinIO container, and this file
// keeps the in-memory stand-in honest against the same suite the real
// backends pass.
func TestMemContract(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store { return blobtest.Mem() })
}
