package login

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func cancelHash(ctx context.Context) error {
	_, err := HashPassword(ctx, "bounded local password")
	return err
}
func cancelVerify(ctx context.Context) error {
	_, err := VerifyPassword(ctx, "", "bounded local password")
	return err
}

func observePasswordWait(t *testing.T, done <-chan error) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		stacks := make([]byte, 1<<20)
		n := runtime.Stack(stacks, true)
		for _, block := range strings.Split(string(stacks[:n]), "\n\n") {
			if strings.Contains(block, "login.passwordKey(") && (strings.Contains(block, "[chan send]") || strings.Contains(block, "[select]")) {
				return
			}
		}
		select {
		case err := <-done:
			t.Fatalf("password worker bypassed full gate: %v", err)
		case <-deadline:
			t.Fatal("did not observe queued password worker")
		case <-tick.C:
		}
	}
}

func TestPasswordQueueCancellation(t *testing.T) {
	for name, call := range map[string]func(context.Context) error{"hash": cancelHash, "verify": cancelVerify} {
		t.Run(name, func(t *testing.T) {
			held := 0
			for i := 0; i < cap(argonSlots); i++ {
				argonSlots <- struct{}{}
				held++
			}
			defer func() {
				for held > 0 {
					<-argonSlots
					held--
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(ctx) }()
			observePasswordWait(t, done)
			cancel()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			exited := false
			select {
			case err := <-done:
				exited = true
				if !errors.Is(err, context.Canceled) {
					t.Errorf("cancellation lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Error("canceled queue waiter did not exit without slot release")
			}
			if len(argonSlots) != cap(argonSlots) {
				t.Errorf("canceled waiter released an unowned slot: %d", len(argonSlots))
			}
			for held > 0 {
				<-argonSlots
				held--
			}
			if !exited {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not finish after release")
				}
			}
			runtime.ReadMemStats(&after)
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
				t.Errorf("canceled waiter derived after release: allocated %d bytes", allocated)
			}
			// An uncanceled waiter must still acquire a released slot and derive.
			for i := 0; i < cap(argonSlots); i++ {
				argonSlots <- struct{}{}
				held++
			}
			go func() { done <- call(context.Background()) }()
			observePasswordWait(t, done)
			<-argonSlots
			held--
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("valid waiter did not resume")
			}
			if len(argonSlots) != held {
				t.Fatalf("acquired slot leaked: got %d want %d", len(argonSlots), held)
			}
		})
	}
}

func TestPasswordCanceledAtAvailableSlot(t *testing.T) {
	// Both select arms can be ready. Repeated cancellation must never start a
	// derivation when acquisition wins that race, nor leak the acquired slot.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 24; i++ {
		if err := cancelHash(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("hash cancellation=%v", err)
		}
		if err := cancelVerify(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("verification cancellation=%v", err)
		}
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Errorf("canceled acquisition ran Argon2: allocated %d bytes", allocated)
	}
	if len(argonSlots) != 0 {
		t.Fatal("canceled acquisition leaked a slot")
	}
}
