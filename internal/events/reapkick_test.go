package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// listenForKick puts a connection of its own on the kick channel. Exec returns
// only once the server has processed the LISTEN, so everything the caller does
// afterwards is covered.
func listenForKick(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open the listening connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "LISTEN "+events.ChannelReapKick); err != nil {
		t.Fatalf("LISTEN %s: %v", events.ChannelReapKick, err)
	}
	return conn
}

// TestReapKickRidesTheTransaction: the kick is published inside the
// transaction that ends the session, which is what lets it be published at all
// without a second round trip on the response — and what makes it safe is that
// Postgres holds a NOTIFY until commit. Both halves of that are asserted here,
// because both are load-bearing and neither is visible at the call site: a
// rolled-back ending must wake nobody, and a committed one must wake everyone
// listening. Without this rung the producer could be moved back out of the
// transaction and every other test in the repo would still pass.
func TestReapKickRidesTheTransaction(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	pool := newPoolFromDSN(t, dsn)
	ctx := context.Background()
	conn := listenForKick(t, dsn)

	rolledBack, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.NotifyReapKick(ctx, rolledBack); err != nil {
		t.Fatalf("publish in the doomed transaction: %v", err)
	}
	if err := rolledBack.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Committed after the rollback, so the wait below needs no timeout to
	// stand for an absence: if the rolled-back transaction had published
	// anything, its notification is ahead of this one in the queue and is what
	// arrives first.
	committed, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.NotifyReapKick(ctx, committed); err != nil {
		t.Fatalf("publish in the committed transaction: %v", err)
	}
	if err := committed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := conn.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("the committed transaction published no kick: %v", err)
	}
	if n.Payload != "" {
		t.Fatalf("the kick carried payload %q; it carries none", n.Payload)
	}

	// And nothing else is queued behind it — one ending, one wake.
	drain, cancelDrain := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancelDrain()
	if extra, err := conn.WaitForNotification(drain); err == nil {
		t.Fatalf("a rolled-back ending published a reap kick: %+v", extra)
	}
}
