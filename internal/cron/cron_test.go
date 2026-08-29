package cron_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/cron"
)

// mustLoad keeps the DST tables readable. A failure here is the tzdata
// guarantee itself failing, which is the thing internal/cron exists to make
// impossible (plan 37 §3.2).
func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestParseRejects(t *testing.T) {
	// Every case is a shape the reference's published dialect excludes, or a
	// value out of range. The dialect is "5-field POSIX cron … Extended cron
	// syntax - seconds or year fields, and the special characters L, W, #, and ?
	// - is not supported, nor are predefined shortcuts (@daily)."
	cases := []struct{ expr, why string }{
		{"", "empty"},
		{"* * * *", "four fields"},
		{"* * * * * *", "six fields — a seconds or year field"},
		{"@daily", "predefined shortcut"},
		{"@every 5m", "predefined shortcut"},
		{"0 0 L * *", "L"},
		{"0 0 15W * *", "W"},
		{"0 0 * * 5#3", "#"},
		{"0 0 ? * *", "?"},
		{"60 * * * *", "minute out of range"},
		{"-1 * * * *", "negative minute"},
		{"* 24 * * *", "hour out of range"},
		{"* * 0 * *", "day-of-month is 1-based"},
		{"* * 32 * *", "day-of-month out of range"},
		{"* * * 0 *", "month is 1-based"},
		{"* * * 13 *", "month out of range"},
		{"* * * * 8", "day-of-week is 0-7"},
		{"JAN * * * *", "names are a Vixie extension, not POSIX"},
		{"* * * * MON", "names are a Vixie extension, not POSIX"},
		{"5-1 * * * *", "inverted range"},
		{"*/0 * * * *", "zero step"},
		{"*/-1 * * * *", "negative step"},
		{"1,, * * * *", "empty list element"},
		{"1- * * * *", "dangling range"},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			if _, _, err := cron.Next(c.expr, "UTC", time.Now()); err == nil {
				t.Fatalf("Next(%q) accepted it; want a parse error (%s)", c.expr, c.why)
			}
		})
	}
}

func TestParseAccepts(t *testing.T) {
	// The whole POSIX surface: star, value, list, range, step over star, step
	// over range, and day-of-week 7 for Sunday.
	for _, expr := range []string{
		"* * * * *",
		"0 0 * * *",
		"*/5 * * * *",
		"0 9-17 * * 1-5",
		"0,30 * * * *",
		"0 0 1,15 * *",
		"0 0 * * 0",
		"0 0 * * 7",
		"0-59/15 0-23/6 * * *",
		"  0   0  *  *  *  ", // surrounding and interior whitespace
	} {
		if _, _, err := cron.Next(expr, "UTC", time.Now()); err != nil {
			t.Errorf("Next(%q): unexpected error %v", expr, err)
		}
	}
}

func TestTimezoneRejected(t *testing.T) {
	// Two of LoadLocation's three special names are refused before the call:
	// "" reads as UTC while the field is required, and "Local" resolves to
	// whatever the host is configured for — one deployment would fire at
	// different instants on different replicas. "UTC" is a real IANA link and
	// the SDK's own example, so it must be accepted.
	for _, tz := range []string{"", "Local", "Not/AZone", "america/new_york "} {
		if _, _, err := cron.Next("0 0 * * *", tz, time.Now()); err == nil {
			t.Errorf("Next with timezone %q was accepted; want a 400-able error", tz)
		}
	}
	for _, tz := range []string{"UTC", "America/New_York", "Australia/Lord_Howe"} {
		if _, _, err := cron.Next("0 0 * * *", tz, time.Now()); err != nil {
			t.Errorf("Next with timezone %q: unexpected error %v", tz, err)
		}
	}
}

func TestSundayIsZeroAndSeven(t *testing.T) {
	after := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) // a Wednesday
	zero, _, err := cron.Next("0 0 * * 0", "UTC", after)
	if err != nil {
		t.Fatal(err)
	}
	seven, _, err := cron.Next("0 0 * * 7", "UTC", after)
	if err != nil {
		t.Fatal(err)
	}
	if !zero.Equal(seven) {
		t.Fatalf("0 and 7 must both mean Sunday: got %s and %s", zero, seven)
	}
	if zero.Weekday() != time.Sunday {
		t.Fatalf("next occurrence %s is a %s, want Sunday", zero, zero.Weekday())
	}
}

func TestDayOfMonthUnionDayOfWeek(t *testing.T) {
	// POSIX union, not intersection: with both fields restricted, a match on
	// EITHER fires. "0 0 13 * 5" is the 13th of any month OR any Friday, so
	// Friday the 13th is not the only match (plan 37 §3.1, entry 15).
	got, err := cron.Upcoming("0 0 13 * 5", "UTC", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-03-06T00:00:00Z", // Friday
		"2026-03-13T00:00:00Z", // Friday AND the 13th
		"2026-03-20T00:00:00Z", // Friday
		"2026-03-27T00:00:00Z", // Friday
		"2026-04-03T00:00:00Z", // Friday
	}
	assertInstants(t, got, want)

	// With only one field restricted the union rule must not widen it.
	dom, err := cron.Upcoming("0 0 13 * *", "UTC", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatal(err)
	}
	assertInstants(t, dom, []string{"2026-03-13T00:00:00Z", "2026-04-13T00:00:00Z"})
}

func TestDSTSpringForwardSkips(t *testing.T) {
	// 02:30 does not exist on 2026-03-08 in America/New_York: the clock jumps
	// 02:00 EST -> 03:00 EDT. A literal wall-clock matcher skips it, and the
	// schedule resumes the next day (plan 37 §3.2, entry 19).
	got, err := cron.Upcoming("30 2 * * *", "America/New_York",
		time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatal(err)
	}
	assertInstants(t, got, []string{
		"2026-03-07T07:30:00Z", // 02:30 EST
		"2026-03-09T06:30:00Z", // 02:30 EDT — the 8th is skipped entirely
		"2026-03-10T06:30:00Z",
	})
}

func TestDSTFallBackFiresTwice(t *testing.T) {
	// 01:30 occurs twice on 2026-11-01 in America/New_York. Both fire, as two
	// distinct UTC instants — which is the assertion that would fail if
	// scheduled_at were stored as a wall clock (plan 37 §6).
	got, err := cron.Upcoming("30 1 * * *", "America/New_York",
		time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatal(err)
	}
	assertInstants(t, got, []string{
		"2026-11-01T05:30:00Z", // 01:30 EDT
		"2026-11-01T06:30:00Z", // 01:30 EST, one hour later
		"2026-11-02T06:30:00Z",
	})
}

func TestDSTHalfHourZone(t *testing.T) {
	// Australia/Lord_Howe shifts by 30 minutes, which catches arithmetic that
	// assumes whole hours. On 2026-04-05 the clock goes 02:00 -> 01:30, so
	// 01:45 occurs twice.
	got, err := cron.Upcoming("45 1 * * *", "Australia/Lord_Howe",
		time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 occurrences, got %d: %v", len(got), got)
	}
	// The doubled pair must be exactly 30 minutes apart, not an hour.
	if d := got[1].Sub(got[0]); d != 30*time.Minute {
		t.Fatalf("the doubled 01:45 pair is %v apart, want 30m (got %s and %s)", d, got[0], got[1])
	}
	loc := mustLoad(t, "Australia/Lord_Howe")
	for _, u := range got[:2] {
		if lt := u.In(loc); lt.Hour() != 1 || lt.Minute() != 45 {
			t.Fatalf("%s renders as %02d:%02d locally, want 01:45", u, lt.Hour(), lt.Minute())
		}
	}
}

func TestDSTFallBackStartingInsideTheRepeatedHour(t *testing.T) {
	// `after` is 01:40 EDT — the FIRST pass through the repeated hour. The
	// occurrences that follow it are 01:00 and 01:30 on the second, EST pass:
	// wall clocks earlier than `after`'s own. A walk that started at `after`'s
	// wall clock and only stepped forward could never reach them, and would
	// answer 02:00 EST instead, silently skipping two runs.
	got, err := cron.Upcoming("*/30 * * * *", "America/New_York",
		time.Date(2026, 11, 1, 5, 40, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatal(err)
	}
	assertInstants(t, got, []string{
		"2026-11-01T06:00:00Z", // 01:00 EST
		"2026-11-01T06:30:00Z", // 01:30 EST
		"2026-11-01T07:00:00Z", // 02:00 EST
	})
}

func TestDueSpansTheRepeatedHour(t *testing.T) {
	// The same disagreement seen from Due's end. Wall 01:00 maps to 05:00Z and
	// 06:00Z; the second is past `to`, but wall 01:30's 05:30Z is not. An
	// out-of-window instant therefore has to be skipped rather than end the
	// walk — treating it as the end drops every later wall clock in the hour.
	got, err := cron.Due("*/30 * * * *", "America/New_York",
		time.Date(2026, 11, 1, 4, 59, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 5, 59, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	assertInstants(t, got, []string{
		"2026-11-01T05:00:00Z", // 01:00 EDT
		"2026-11-01T05:30:00Z", // 01:30 EDT
	})
}

func TestStepLargerThanTheFieldSelectsOnlyItsFirstValue(t *testing.T) {
	// The step is an unbounded integer off the wire. Accepting it and letting
	// the mask loop add it is an out-of-range panic on a create request: the
	// increment wraps negative and still compares below the field maximum.
	got, err := cron.Upcoming("1/9223372036854775807 * * * *", "UTC",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatal(err)
	}
	assertInstants(t, got, []string{
		"2026-01-01T00:01:00Z",
		"2026-01-01T01:01:00Z",
	})
}

func TestUnsatisfiableExpression(t *testing.T) {
	// 0 0 31 2 * parses, every field is in range, and February never has 31
	// days. Upcoming returns empty, which is what lets create and update answer
	// 400 rather than store a schedule that can never fire (entry 27).
	got, err := cron.Upcoming("0 0 31 2 *", "UTC", time.Now(), 5)
	if err != nil {
		t.Fatalf("an unsatisfiable expression must parse cleanly, not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no occurrences, got %v", got)
	}
	if _, ok, err := cron.Next("0 0 31 2 *", "UTC", time.Now()); err != nil || ok {
		t.Fatalf("Next: got ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestLeapDayCrossesTheNonLeapCentury(t *testing.T) {
	// 0 0 29 2 * is the sparsest satisfiable 5-field expression: 2100 is not a
	// leap year, so 2096 -> 2104 is an eight-year gap. A four-year search bound
	// would report it unsatisfiable and refuse a legal schedule (§3.3).
	after := time.Date(2096, 3, 1, 0, 0, 0, 0, time.UTC)
	next, ok, err := cron.Next("0 0 29 2 *", "UTC", after)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no occurrence found across the 2100 gap; the search bound is too short")
	}
	if got := next.Format(time.RFC3339); got != "2104-02-29T00:00:00Z" {
		t.Fatalf("got %s, want 2104-02-29T00:00:00Z", got)
	}
}

func TestDueIsHalfOpen(t *testing.T) {
	// Due(from, to] — exclusive of from, inclusive of to. The scheduler's
	// watermark is the last claimed occurrence, so an inclusive `from` would
	// re-select it every tick.
	from := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 29, 10, 3, 0, 0, time.UTC)
	got, err := cron.Due("* * * * *", "UTC", from, to)
	if err != nil {
		t.Fatal(err)
	}
	assertInstants(t, got, []string{
		"2026-08-29T10:01:00Z",
		"2026-08-29T10:02:00Z",
		"2026-08-29T10:03:00Z",
	})
}

func TestDueEmptyWindow(t *testing.T) {
	at := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	got, err := cron.Due("* * * * *", "UTC", at, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty window must yield nothing, got %v", got)
	}
	if _, err := cron.Due("* * * * *", "UTC", to(at, time.Minute), at); err == nil {
		t.Fatal("an inverted window must error rather than silently yield nothing")
	}
}

// TestGeneratorsAgree is the property the plan asks for: Due, Next and Upcoming
// share one occurrence generator, so what a user reads in upcoming_runs_at and
// what the scheduler fires can never disagree (§3.1).
func TestGeneratorsAgree(t *testing.T) {
	exprs := []string{"* * * * *", "*/7 * * * *", "0 0 * * *", "30 2 * * *", "0 9-17 * * 1-5", "0 0 1,15 * *"}
	zones := []string{"UTC", "America/New_York", "Australia/Lord_Howe", "Asia/Shanghai"}
	starts := []time.Time{
		time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC),   // straddles a spring-forward
		time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), // straddles a fall-back
		time.Date(2026, 6, 15, 13, 44, 0, 0, time.UTC),
	}
	for _, expr := range exprs {
		for _, tz := range zones {
			for _, start := range starts {
				up, err := cron.Upcoming(expr, tz, start, 8)
				if err != nil {
					t.Fatalf("Upcoming(%q,%q): %v", expr, tz, err)
				}
				// Next must equal Upcoming's first element.
				next, ok, err := cron.Next(expr, tz, start)
				if err != nil {
					t.Fatalf("Next(%q,%q): %v", expr, tz, err)
				}
				if ok != (len(up) > 0) {
					t.Fatalf("%q/%q: Next ok=%v but Upcoming returned %d", expr, tz, ok, len(up))
				}
				if ok && !next.Equal(up[0]) {
					t.Fatalf("%q/%q: Next=%s but Upcoming[0]=%s", expr, tz, next, up[0])
				}
				if len(up) == 0 {
					continue
				}
				// Due over [start, last] must reproduce Upcoming exactly.
				due, err := cron.Due(expr, tz, start, up[len(up)-1])
				if err != nil {
					t.Fatalf("Due(%q,%q): %v", expr, tz, err)
				}
				if len(due) != len(up) {
					t.Fatalf("%q/%q: Due returned %d occurrences, Upcoming %d", expr, tz, len(due), len(up))
				}
				for i := range due {
					if !due[i].Equal(up[i]) {
						t.Fatalf("%q/%q: occurrence %d: Due=%s Upcoming=%s", expr, tz, i, due[i], up[i])
					}
				}
				// Strictly ascending, and every instant is UTC.
				for i := 1; i < len(up); i++ {
					if !up[i].After(up[i-1]) {
						t.Fatalf("%q/%q: not ascending at %d: %s then %s", expr, tz, i, up[i-1], up[i])
					}
				}
				for _, u := range up {
					if u.Location() != time.UTC {
						t.Fatalf("%q/%q: occurrence %s is not UTC", expr, tz, u)
					}
				}
			}
		}
	}
}

func TestUpcomingCountBounds(t *testing.T) {
	if got, err := cron.Upcoming("* * * * *", "UTC", time.Now(), 0); err != nil || len(got) != 0 {
		t.Fatalf("n=0: got %v err=%v, want empty and no error", got, err)
	}
	if _, err := cron.Upcoming("* * * * *", "UTC", time.Now(), -1); err == nil {
		t.Fatal("a negative count must error rather than be read as unbounded")
	}
}

func TestErrorsAreDistinguishable(t *testing.T) {
	// The API turns a bad expression and a bad timezone into 400s naming
	// different fields, so the two must be told apart without string matching.
	_, _, err := cron.Next("nonsense", "UTC", time.Now())
	if !errors.Is(err, cron.ErrExpression) {
		t.Fatalf("bad expression: got %v, want ErrExpression", err)
	}
	_, _, err = cron.Next("0 0 * * *", "Not/AZone", time.Now())
	if !errors.Is(err, cron.ErrTimezone) {
		t.Fatalf("bad timezone: got %v, want ErrTimezone", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatal("an error rendered into a 400 body must be one line")
	}
}

func assertInstants(t *testing.T, got []time.Time, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d occurrences %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if g := got[i].UTC().Format(time.RFC3339); g != want[i] {
			t.Errorf("occurrence %d: got %s, want %s", i, g, want[i])
		}
	}
}

func to(t time.Time, d time.Duration) time.Time { return t.Add(d) }
