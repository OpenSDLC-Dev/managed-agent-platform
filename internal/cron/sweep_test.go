package cron_test

import (
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/cron"
)

// The two sweeps below are the net under the class of defect that a hand-picked
// DST table cannot catch: Due and Upcoming reach their answers through the same
// walk but leave it by different exits, and a fall-back is where wall-clock
// order and instant order come apart. Three reviewers found two such defects
// independently in one pass, so the property is asserted over every hour of
// every transition day rather than over the cases someone thought to write.

// Due(from, to] must equal exactly the prefix of Upcoming(from) that lands at
// or before `to`. Any disagreement between the two exits shows up here.
func TestDueAgreesWithUpcomingAcrossEveryTransitionHour(t *testing.T) {
	// Lord Howe shifts by 30 minutes and Chatham by 45 past a :45 offset;
	// Troll's two hours is the largest jump in the database.
	zones := []string{
		"America/New_York", "Europe/Dublin", "Australia/Lord_Howe",
		"Antarctica/Troll", "Pacific/Chatham", "Asia/Tehran", "UTC",
	}
	exprs := []string{"* * * * *", "*/7 * * * *", "0,30 * * * *", "15 1 * * *", "0 2 * * *"}
	days := []time.Time{
		time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}

	windows := 0
	for _, tz := range zones {
		for _, expr := range exprs {
			for _, day := range days {
				for h := range 24 {
					from := day.Add(time.Duration(h) * time.Hour)
					to := from.Add(90 * time.Minute)

					due, err := cron.Due(expr, tz, from, to)
					if err != nil {
						t.Fatal(err)
					}
					// Far more than any expression here yields in 90 minutes,
					// so the prefix is never truncated by the count instead.
					up, err := cron.Upcoming(expr, tz, from, 200)
					if err != nil {
						t.Fatal(err)
					}
					var want []time.Time
					for _, u := range up {
						if u.After(to) {
							break
						}
						want = append(want, u)
					}

					windows++
					if len(due) != len(want) {
						t.Fatalf("%s %q from %s: Due returned %d occurrences, the Upcoming prefix %d\n  Due:      %v\n  Upcoming: %v",
							tz, expr, from.Format(time.RFC3339), len(due), len(want), due, want)
					}
					for i := range due {
						if !due[i].Equal(want[i]) {
							t.Fatalf("%s %q from %s: occurrence %d is %s, Upcoming says %s",
								tz, expr, from.Format(time.RFC3339), i, due[i], want[i])
						}
					}
				}
			}
		}
	}
	t.Logf("compared %d windows", windows)
}

// Every instant returned must render, in the zone asked for, as a wall clock
// the expression actually matches — the assertion that fails if an offset probe
// invents an instant no clock in that zone ever showed.
func TestEveryInstantRendersAsAMatchingWallClock(t *testing.T) {
	const expr = "*/13 * * * *" // minutes 0, 13, 26, 39 and 52
	want := map[int]bool{0: true, 13: true, 26: true, 39: true, 52: true}

	for _, tz := range []string{"America/New_York", "Australia/Lord_Howe", "Antarctica/Troll", "Pacific/Chatham"} {
		loc := mustLoad(t, tz)
		got, err := cron.Upcoming(expr, tz, time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 500 {
			t.Fatalf("%s: got %d occurrences, want 500", tz, len(got))
		}
		for _, u := range got {
			if lt := u.In(loc); !want[lt.Minute()] {
				t.Fatalf("%s: %s renders locally as %s, whose minute %q does not match", tz, u, lt, expr)
			}
		}
	}
}
