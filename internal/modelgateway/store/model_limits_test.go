package store_test

import (
	"context"

	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

func TestModelLimitsShareConcurrentAdmission(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	clearOfAMinute(t, pool)
	rpm := int64(3)
	if _, err := s.PutModelLimit(ctx, store.ModelLimit{Model: "fast", RPM: &rpm}, nil); err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			a, err := s.AdmitModel(ctx, "fast")
			if err != nil {
				t.Error(err)
			} else if a.Admitted {
				admitted.Add(1)
			} else if a.Limit != "requests_per_minute" || a.RetryAfter < 1 || a.RetryAfter > 60 {
				t.Errorf("refusal: %+v", a)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 3 {
		t.Fatalf("admitted %d, want 3 across concurrent callers", admitted.Load())
	}
	if a, err := s.AdmitModel(ctx, "slow"); err != nil || !a.Admitted {
		t.Fatalf("independent model: %+v %v", a, err)
	}
}

func TestModelLimitsSeparateInputOutput(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	clearOfAMinute(t, pool)
	input, output := int64(50), int64(20)
	for _, tc := range []struct {
		model, want string
		limit       store.ModelLimit
		tokens      store.Tokens
	}{
		{"input", "input_tokens_per_minute", store.ModelLimit{Model: "input", ITPM: &input}, store.Tokens{Input: 40, CacheWrite: 10}},
		{"output", "output_tokens_per_minute", store.ModelLimit{Model: "output", OTPM: &output}, store.Tokens{Output: 20}},
	} {
		if _, err := s.PutModelLimit(ctx, tc.limit, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordUsage(ctx, usage("key_a", tc.model, "dep_1", &store.Tokens{CacheRead: 10000})); err != nil {
			t.Fatal(err)
		}
		if a, err := s.AdmitModel(ctx, tc.model); err != nil || !a.Admitted {
			t.Fatalf("cache reads: %+v %v", a, err)
		}
		if _, err := s.RecordUsage(ctx, usage("key_b", tc.model, "dep_1", &tc.tokens)); err != nil {
			t.Fatal(err)
		}
		if a, err := s.AdmitModel(ctx, tc.model); err != nil || a.Admitted || a.Limit != tc.want {
			t.Fatalf("tokens: %+v %v", a, err)
		}

	}
}
