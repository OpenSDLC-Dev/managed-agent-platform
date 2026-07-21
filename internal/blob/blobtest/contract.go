package blobtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// Run exercises the blob.Store contract (CLAUDE.md: every backend passes the
// same shared suite). newStore is called once per subtest so a backend can
// isolate its own fixtures — for the S3 backend, a fresh bucket per subtest.
// The suite asserts observable behavior only, never a backend's internals.
func Run(t *testing.T, newStore func(t *testing.T) blob.Store) {
	t.Helper()
	ctx := context.Background()

	put := func(t *testing.T, s blob.Store, key string, content []byte) {
		t.Helper()
		if err := s.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/octet-stream"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	get := func(t *testing.T, s blob.Store, key string) ([]byte, int64) {
		t.Helper()
		rc, size, err := s.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		return data, size
	}

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		s := newStore(t)
		want := []byte("skill archive bytes")
		put(t, s, "skills/skill_1/100.zip", want)
		data, size := get(t, s, "skills/skill_1/100.zip")
		if !bytes.Equal(data, want) {
			t.Errorf("content = %q, want %q", data, want)
		}
		if size != int64(len(want)) {
			t.Errorf("size = %d, want %d", size, len(want))
		}
	})

	t.Run("OverwriteReplaces", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "k", []byte("first version, the longer one"))
		put(t, s, "k", []byte("second"))
		data, size := get(t, s, "k")
		if string(data) != "second" || size != 6 {
			t.Errorf("after overwrite: content=%q size=%d", data, size)
		}
	})

	t.Run("GetMissingIsErrNotFound", func(t *testing.T) {
		s := newStore(t)
		_, _, err := s.Get(ctx, "never/written")
		if !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("get missing = %v, want blob.ErrNotFound", err)
		}
	})

	t.Run("DeleteRemoves", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "doomed", []byte("x"))
		if err := s.Delete(ctx, "doomed"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, _, err := s.Get(ctx, "doomed"); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("get after delete = %v, want blob.ErrNotFound", err)
		}
	})

	t.Run("DeleteMissingIsNil", func(t *testing.T) {
		s := newStore(t)
		if err := s.Delete(ctx, "never/written"); err != nil {
			t.Errorf("delete missing = %v, want nil (idempotent)", err)
		}
	})

	t.Run("EmptyObject", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "empty", nil)
		data, size := get(t, s, "empty")
		if len(data) != 0 || size != 0 {
			t.Errorf("empty object: content=%q size=%d", data, size)
		}
	})

	t.Run("NamespacedKeysAreIndependent", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "skills/skill_a/1.zip", []byte("a"))
		put(t, s, "skills/skill_a/2.zip", []byte("b"))
		if err := s.Delete(ctx, "skills/skill_a/1.zip"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		data, _ := get(t, s, "skills/skill_a/2.zip")
		if string(data) != "b" {
			t.Errorf("sibling key content = %q, want %q", data, "b")
		}
	})

	t.Run("LargePayloadRoundTrip", func(t *testing.T) {
		// 5 MiB of deterministic non-repeating bytes: large enough to cross
		// any single-buffer path, cheap enough for every backend.
		want := make([]byte, 5<<20)
		for i := range want {
			want[i] = byte(i*31 + i>>8)
		}
		s := newStore(t)
		put(t, s, "large", want)
		data, size := get(t, s, "large")
		if size != int64(len(want)) {
			t.Fatalf("size = %d, want %d", size, len(want))
		}
		if !bytes.Equal(data, want) {
			t.Error("large payload corrupted in round trip")
		}
	})
}
