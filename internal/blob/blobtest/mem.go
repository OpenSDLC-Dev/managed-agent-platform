package blobtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// Mem returns an in-memory blob.Store for tests exercising logic above the
// storage seam (the API registry, later the executor's materialization). It
// holds the same contract as the real backends — the suite in contract.go
// runs against it in mem_test.go — without a MinIO container, so suites that
// already carry a Postgres harness need no second one.
func Mem() *MemStore { return &MemStore{objects: map[string]memObject{}} }

type memObject struct {
	data        []byte
	contentType string
}

// MemStore is Mem's implementation. The concrete type is exported so tests
// can reach behind the API under test (Len, direct Delete) to assert storage
// side effects.
type MemStore struct {
	mu      sync.Mutex
	objects map[string]memObject
}

// Len reports how many objects the store currently holds.
func (s *MemStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *MemStore) Put(_ context.Context, key string, r io.Reader, size int64, contentType string) error {
	if key == "" {
		return fmt.Errorf("blobtest: empty key")
	}
	if size < 0 {
		return fmt.Errorf("blobtest: negative size")
	}
	// Contract: exactly size bytes — a short reader errors, bytes beyond
	// size are never read.
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("blobtest: reader shorter than declared size %d: %w", size, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = memObject{data: data, contentType: contentType}
	return nil
}

func (s *MemStore) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	if key == "" {
		return nil, 0, fmt.Errorf("blobtest: empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key]
	if !ok {
		return nil, 0, fmt.Errorf("blobtest: %q: %w", key, blob.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(obj.data)), int64(len(obj.data)), nil
}

func (s *MemStore) Delete(_ context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("blobtest: empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}
