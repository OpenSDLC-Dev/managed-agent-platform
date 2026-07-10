package events_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

func text(s string) json.RawMessage {
	return json.RawMessage(`{"content":[{"type":"text","text":"` + s + `"}]}`)
}

func TestAppendAllocatesSeqAndDefaults(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	got, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: text("one")},
		{Type: domain.EventUserInterrupt}, // empty payload defaults to {}
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("appended %d events, want 2", len(got))
	}
	for i, ev := range got {
		if ev.Seq != int64(i+1) {
			t.Errorf("event %d seq = %d, want %d", i, ev.Seq, i+1)
		}
		if !strings.HasPrefix(ev.ID.String(), "sevt_") {
			t.Errorf("event id %q lacks sevt_ prefix", ev.ID)
		}
		if ev.ProcessedAt != nil {
			t.Errorf("client event processed_at = %v, want nil", ev.ProcessedAt)
		}
		if ev.CreatedAt.IsZero() || ev.CreatedAt.Location() != time.UTC {
			t.Errorf("created_at = %v, want non-zero UTC", ev.CreatedAt)
		}
	}
	if string(got[1].Body) != "{}" {
		t.Errorf("empty payload stored as %s, want {}", got[1].Body)
	}

	// A second batch continues the sequence.
	more, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("two")}})
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if more[0].Seq != 3 {
		t.Errorf("second batch seq = %d, want 3", more[0].Seq)
	}
}

func TestAppendCallerSuppliedIDAndProcessedAt(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	id := domain.NewID("sevt")
	got, err := log.Append(context.Background(), sid, []events.NewEvent{
		{ID: id, Type: domain.EventAgentMessage, Payload: text("hi"), ProcessedAt: &now},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got[0].ID != id {
		t.Errorf("id = %s, want caller-supplied %s", got[0].ID, id)
	}
	if got[0].ProcessedAt == nil || !got[0].ProcessedAt.Equal(now) {
		t.Errorf("processed_at = %v, want %v", got[0].ProcessedAt, now)
	}
}

func TestAppendSessionErrors(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	one := []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("x")}}

	if _, err := log.Append(ctx, domain.NewID("sesn"), one); err != events.ErrSessionNotFound {
		t.Errorf("unknown session err = %v, want ErrSessionNotFound", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, sid, one); err != events.ErrSessionArchived {
		t.Errorf("archived session err = %v, want ErrSessionArchived", err)
	}
}

func TestAppendRejectsStreamOnlyAndEmpty(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	if _, err := log.Append(ctx, sid, nil); err == nil {
		t.Error("empty batch should error")
	}
	for _, typ := range []domain.EventType{domain.EventStart, domain.EventDelta} {
		if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: typ}}); err == nil {
			t.Errorf("%s should be rejected as stream-only", typ)
		}
	}
}

func TestAppendConcurrentSeqIntegrity(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	other := newSession(t, pool)
	ctx := context.Background()

	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("c")}}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	got, err := log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != workers*each {
		t.Fatalf("listed %d events, want %d", len(got), workers*each)
	}
	for i, ev := range got {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq at %d = %d: sequence has gaps or duplicates", i, ev.Seq)
		}
		// created_at is taken under the session lock (clock_timestamp, not
		// transaction-start now()), so it can never run backwards against
		// seq — the created_at[gt] watermark pattern depends on this.
		if i > 0 && ev.CreatedAt.Before(got[i-1].CreatedAt) {
			t.Fatalf("created_at at seq %d precedes seq %d: %v < %v",
				ev.Seq, got[i-1].Seq, ev.CreatedAt, got[i-1].CreatedAt)
		}
	}

	// Other sessions allocate independently, starting at 1.
	ev, err := log.Append(ctx, other, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("o")}})
	if err != nil {
		t.Fatal(err)
	}
	if ev[0].Seq != 1 {
		t.Errorf("other session first seq = %d, want 1", ev[0].Seq)
	}
}

func TestListFiltersAndKeyset(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	types := []domain.EventType{
		domain.EventUserMessage, domain.EventAgentMessage, domain.EventUserMessage,
		domain.EventAgentThinking, domain.EventUserMessage,
	}
	var all []domain.Event
	for _, typ := range types {
		ev, err := log.Append(ctx, sid, []events.NewEvent{{Type: typ, Payload: text(string(typ))}})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, ev[0])
	}

	byType, err := log.List(ctx, sid, events.ListQuery{Types: []string{"user.message"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(byType) != 3 {
		t.Errorf("types filter returned %d, want 3", len(byType))
	}

	desc, err := log.List(ctx, sid, events.ListQuery{Desc: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc) != 2 || desc[0].Seq != 5 || desc[1].Seq != 4 {
		t.Errorf("desc limit 2 returned seqs %v", []int64{desc[0].Seq, desc[1].Seq})
	}

	after := int64(2)
	keyset, err := log.List(ctx, sid, events.ListQuery{AfterSeq: &after})
	if err != nil {
		t.Fatal(err)
	}
	if len(keyset) != 3 || keyset[0].Seq != 3 {
		t.Errorf("asc after seq 2: got %d rows starting %d, want 3 starting 3", len(keyset), keyset[0].Seq)
	}
	keysetDesc, err := log.List(ctx, sid, events.ListQuery{AfterSeq: &after, Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(keysetDesc) != 1 || keysetDesc[0].Seq != 1 {
		t.Errorf("desc after seq 2: got %d rows", len(keysetDesc))
	}

	// created_at range filters, keyed off the actual stored timestamps.
	mid := all[2].CreatedAt
	gte, err := log.List(ctx, sid, events.ListQuery{CreatedGTE: &mid})
	if err != nil {
		t.Fatal(err)
	}
	gt, err := log.List(ctx, sid, events.ListQuery{CreatedGT: &mid})
	if err != nil {
		t.Fatal(err)
	}
	lt, err := log.List(ctx, sid, events.ListQuery{CreatedLT: &mid})
	if err != nil {
		t.Fatal(err)
	}
	lte, err := log.List(ctx, sid, events.ListQuery{CreatedLTE: &mid})
	if err != nil {
		t.Fatal(err)
	}
	if len(gte) != len(gt)+1 || len(lte) != len(lt)+1 {
		t.Errorf("inclusive/exclusive mismatch: gte=%d gt=%d lte=%d lt=%d", len(gte), len(gt), len(lte), len(lt))
	}
	if len(gte)+len(lt) != len(all) {
		t.Errorf("gte+lt should partition the log: %d + %d != %d", len(gte), len(lt), len(all))
	}

	// Unknown session lists empty, not an error (404 is the API's concern).
	empty, err := log.List(ctx, domain.NewID("sesn"), events.ListQuery{})
	if err != nil || len(empty) != 0 {
		t.Errorf("unknown session list = %d rows, err %v", len(empty), err)
	}
}
