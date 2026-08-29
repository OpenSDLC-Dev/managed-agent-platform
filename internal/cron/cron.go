// Package cron is the occurrence engine behind a deployment's schedule: the
// reference's 5-field POSIX cron dialect, matched literally against a wall clock
// in an IANA timezone.
//
// Due, Next and Upcoming share one generator on purpose. The list a user reads
// in a deployment's upcoming_runs_at and the instant the scheduler actually
// fires come from the same walk, so they cannot disagree — plan 37 §3.1, and the
// property test in cron_test.go asserts it.
//
// The dialect is a strict subset of what a general cron library accepts, which
// is why this is hand-rolled: seconds and year fields, the special characters
// L, W, # and ?, and predefined shortcuts like @daily are all excluded, so a
// library would need a rejection layer in front of it and would still not
// supply the two daylight-saving rules below.
//
// # Timezones
//
// The tzdata import is deliberate and belongs here rather than in a main. The
// server image is debian:stable-slim with ca-certificates and nothing else, so
// without an embedded database every non-UTC deployment would be refused at
// create time in the image and never in the test gate, whose host has a
// zoneinfo tree. Costing ~450 KB in the binary buys the guarantee that no
// binary or test can forget it.
//
// # Daylight saving
//
// A wall clock that does not exist on the spring-forward day is skipped; one
// that occurs twice on the fall-back day fires twice, as two distinct instants.
// Neither rule is published by the reference — both are registered inferences
// (plan 37 §8.1 entry 19) — and both are the highest-risk code here, because a
// wrong answer looks exactly like a right one.
package cron

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	// Embedded zoneinfo: see the package comment. Without it LoadLocation
	// depends on a zoneinfo tree the server image does not ship.
	_ "time/tzdata"
)

// ErrExpression and ErrTimezone are distinguishable because the API turns them
// into 400s naming different fields; a caller must not have to match strings.
var (
	ErrExpression = errors.New("invalid cron expression")
	ErrTimezone   = errors.New("invalid timezone")
)

// searchYears bounds every walk. Twelve rather than a round four because
// "0 0 29 2 *" — the sparsest satisfiable expression — has an eight-year gap
// across 2100, which is not a leap year; a shorter bound would report a legal
// schedule as unsatisfiable (plan 37 §3.3).
const searchYears = 12

// ambiguityMargin is how far outside each bound the walk reaches: it starts
// this far before `after`'s wall clock and continues this far past the wall
// clock that satisfies the request. Wall clocks ascend, but instants do not —
// on a fall-back day the second 01:30 lands after the first 01:59 — so a
// candidate whose instant falls outside a bound is skipped rather than taken
// for the end of the walk, and wall clocks before `after`'s own are still
// visited because the repeated hour's second pass carries them.
//
// The margin has to exceed the largest backward offset jump in tzdata, which
// is what makes a wall clock's instant lag an earlier wall clock's. Scanning
// all 598 zones in Go's zoneinfo.zip half-hourly across the thirteen years from
// 2026 puts that maximum at exactly two hours (Antarctica/Troll, +02 → +00);
// every other zone moves by an hour or less, and Lord Howe by half of one.
const ambiguityMargin = 3 * time.Hour

// Due returns every occurrence in (from, to], ascending. The lower bound is
// exclusive because the scheduler's watermark is the last occurrence it
// claimed; including it would re-select that one on every tick.
func Due(expr, timezone string, from, to time.Time) ([]time.Time, error) {
	if to.Before(from) {
		return nil, fmt.Errorf("%w: window ends %s before it starts %s", ErrExpression, to, from)
	}
	s, loc, err := compile(expr, timezone)
	if err != nil {
		return nil, err
	}
	return s.walk(loc, from, to, -1), nil
}

// Next returns the first occurrence strictly after `after`. The bool is false
// when the expression has none inside the search bound — which is how an
// unsatisfiable expression such as "0 0 31 2 *" reports itself, without being
// an error: it parses, and every field is in range.
func Next(expr, timezone string, after time.Time) (time.Time, bool, error) {
	got, err := Upcoming(expr, timezone, after, 1)
	if err != nil || len(got) == 0 {
		return time.Time{}, false, err
	}
	return got[0], true, nil
}

// Upcoming returns up to n occurrences after `after`, ascending. Fewer than n
// means the search bound was reached, which for an active deployment is what
// create and update refuse.
func Upcoming(expr, timezone string, after time.Time, n int) ([]time.Time, error) {
	if n < 0 {
		return nil, fmt.Errorf("%w: count %d is negative", ErrExpression, n)
	}
	s, loc, err := compile(expr, timezone)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	return s.walk(loc, after, time.Time{}, n), nil
}

// schedule is five bitmasks. dowRestricted and domRestricted carry the POSIX
// union rule, which needs to know whether a field was written as "*" — a
// restricted day-of-month and a restricted day-of-week match on either.
type schedule struct {
	minute, hour, dom, month, dow [64]bool
	domRestricted, dowRestricted  bool
}

func compile(expr, timezone string) (*schedule, *time.Location, error) {
	s, err := parse(expr)
	if err != nil {
		return nil, nil, err
	}
	loc, err := loadLocation(timezone)
	if err != nil {
		return nil, nil, err
	}
	return s, loc, nil
}

// loadLocation refuses two names LoadLocation accepts and neither of which is
// an IANA identifier: the empty string, which it reads as UTC while the wire
// field is required, and "Local", which resolves to whatever the host is
// configured for — one deployment would then fire at different instants on
// different replicas. "UTC" is a real IANA link and the SDK's own example, so
// it stays accepted.
func loadLocation(timezone string) (*time.Location, error) {
	if timezone == "" {
		return nil, fmt.Errorf("%w: timezone is required", ErrTimezone)
	}
	if timezone == "Local" {
		return nil, fmt.Errorf("%w: %q is not an IANA identifier; it resolves to the host's zone", ErrTimezone, timezone)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not in the IANA timezone database", ErrTimezone, timezone)
	}
	return loc, nil
}

type fieldSpec struct {
	name     string
	min, max int
}

var fields = [5]fieldSpec{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7}, // 0 and 7 both mean Sunday
}

func parse(expr string) (*schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("%w: want 5 fields, got %d in %q", ErrExpression, len(parts), expr)
	}
	var s schedule
	masks := [5]*[64]bool{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	for i, part := range parts {
		if err := parseField(part, fields[i], masks[i]); err != nil {
			return nil, err
		}
		switch i {
		case 2:
			s.domRestricted = part != "*"
		case 4:
			s.dowRestricted = part != "*"
		}
	}
	// 7 is Sunday's other spelling; fold it so the matcher tests one bit.
	if s.dow[7] {
		s.dow[0] = true
	}
	return &s, nil
}

func parseField(part string, f fieldSpec, mask *[64]bool) error {
	for _, elem := range strings.Split(part, ",") {
		if elem == "" {
			return fmt.Errorf("%w: empty element in %s field %q", ErrExpression, f.name, part)
		}
		rng, stepStr, hasStep := strings.Cut(elem, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return fmt.Errorf("%w: step %q in %s field is not a positive integer", ErrExpression, stepStr, f.name)
			}
			step = n
		}
		lo, hi := f.min, f.max
		if rng != "*" {
			loStr, hiStr, isRange := strings.Cut(rng, "-")
			var err error
			if lo, err = parseValue(loStr, f); err != nil {
				return err
			}
			if isRange {
				if hi, err = parseValue(hiStr, f); err != nil {
					return err
				}
				if hi < lo {
					return fmt.Errorf("%w: range %q in %s field runs backwards", ErrExpression, rng, f.name)
				}
			} else if hasStep {
				// "5/15" means 5 to the field maximum, stepping by 15 — the
				// standard reading, and the one "*/n" already relies on.
				hi = f.max
			} else {
				hi = lo
			}
		}
		for v := lo; v <= hi; v += step {
			mask[v] = true
			// The step comes off the wire and Atoi will hand back math.MaxInt,
			// so the increment is guarded rather than trusted: v += step would
			// wrap negative, still satisfy v <= hi, and index the mask out of
			// range. A step past the field's top selects only its first value.
			if hi-v < step {
				break
			}
		}
	}
	return nil
}

func parseValue(s string, f fieldSpec) (int, error) {
	// Numeric only. The dialect is named POSIX twice by the reference, and
	// names like JAN and MON are a Vixie extension POSIX does not define.
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %q in %s field is not a number", ErrExpression, s, f.name)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%w: %d in %s field is outside %d-%d", ErrExpression, v, f.name, f.min, f.max)
	}
	return v, nil
}

// matchesDay applies the POSIX union: with both day fields restricted, either
// matching is enough. With one restricted, only that one decides. The rule is
// unstated in the reference's schema, but it names the dialect POSIX twice and
// union is what POSIX specifies (plan 37 §8.1 entry 15).
func (s *schedule) matchesDay(y int, mo time.Month, d int) bool {
	dom := s.dom[d]
	dow := s.dow[int(time.Date(y, mo, d, 12, 0, 0, 0, time.UTC).Weekday())]
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}

// walk steps fields rather than minutes: it advances to the next candidate
// month, day, hour and minute instead of testing every minute in the bound.
// That is what makes a twelve-year search cheap enough to run on every read of
// upcoming_runs_at — an unsatisfiable expression is refuted in a few hundred
// steps rather than the 6.3 million minutes twelve years hold.
//
// until zero means "no upper bound"; n negative means "no count limit".
func (s *schedule) walk(loc *time.Location, after, until time.Time, n int) []time.Time {
	// The cursor is a wall clock while both bounds are instants, and a
	// fall-back is where the two stop agreeing on order — hence the margin at
	// both ends and the filtering against the instants themselves rather than
	// against the cursor. See ambiguityMargin.
	origin := after.In(loc)
	start := after.Add(-ambiguityMargin).In(loc)
	y, mo, d := start.Date()
	h, mi := start.Hour(), start.Minute()
	lastYear := origin.Year() + searchYears

	var out []time.Time
	// The wall clock past which no candidate can still land in the answer: a
	// margin beyond `until` for Due, or beyond the n-th instant for Upcoming,
	// which is only known once that many exist. Zero means "not yet known".
	var stopAt time.Time
	if !until.IsZero() {
		lt := until.In(loc)
		stopAt = time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), 0, 0, time.UTC).Add(ambiguityMargin)
	}

	for y <= lastYear {
		if !s.month[int(mo)] {
			y, mo, d, h, mi = nextMonth(y, mo)
			continue
		}
		if !s.matchesDay(y, mo, d) {
			y, mo, d, h, mi = nextDay(y, mo, d)
			continue
		}
		if !s.hour[h] {
			y, mo, d, h, mi = nextHour(y, mo, d, h)
			continue
		}
		if !s.minute[mi] {
			y, mo, d, h, mi = nextMinute(y, mo, d, h, mi)
			continue
		}

		naive := time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
		if !stopAt.IsZero() && naive.After(stopAt) {
			break
		}
		for _, u := range instantsFor(loc, y, mo, d, h, mi) {
			// Out of bounds either way is skipped, never a stop: within the
			// margin a later wall clock can still map inside the window, and
			// an earlier one outside it.
			if !u.After(after) || (!until.IsZero() && u.After(until)) {
				continue
			}
			out = append(out, u)
		}
		if n > 0 && stopAt.IsZero() && len(out) >= n {
			stopAt = naive.Add(ambiguityMargin)
		}
		y, mo, d, h, mi = nextMinute(y, mo, d, h, mi)
	}

	out = sortUnique(out)
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// instantsFor maps one wall clock to the UTC instants that render as it: none
// when the clock is skipped by a spring-forward, one normally, two when a
// fall-back repeats it. Probing the offsets in effect a day either side covers
// any single transition without scanning minute by minute.
func instantsFor(loc *time.Location, y int, mo time.Month, d, h, mi int) []time.Time {
	base := time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	probe := time.Date(y, mo, d, 12, 0, 0, 0, time.UTC)

	var offsets []int
	for _, p := range [3]time.Time{probe.Add(-36 * time.Hour), probe, probe.Add(36 * time.Hour)} {
		_, off := p.In(loc).Zone()
		if !containsInt(offsets, off) {
			offsets = append(offsets, off)
		}
	}

	var out []time.Time
	for _, off := range offsets {
		u := base.Add(-time.Duration(off) * time.Second)
		lt := u.In(loc)
		if lt.Year() == y && lt.Month() == mo && lt.Day() == d && lt.Hour() == h && lt.Minute() == mi {
			if !containsTime(out, u) {
				out = append(out, u)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

func nextMinute(y int, mo time.Month, d, h, mi int) (int, time.Month, int, int, int) {
	if mi++; mi > 59 {
		return nextHour(y, mo, d, h)
	}
	return y, mo, d, h, mi
}

func nextHour(y int, mo time.Month, d, h int) (int, time.Month, int, int, int) {
	if h++; h > 23 {
		return nextDay(y, mo, d)
	}
	return y, mo, d, h, 0
}

func nextDay(y int, mo time.Month, d int) (int, time.Month, int, int, int) {
	if d++; d > daysIn(y, mo) {
		return nextMonth(y, mo)
	}
	return y, mo, d, 0, 0
}

func nextMonth(y int, mo time.Month) (int, time.Month, int, int, int) {
	if mo++; mo > time.December {
		mo, y = time.January, y+1
	}
	return y, mo, 1, 0, 0
}

func daysIn(y int, mo time.Month) int {
	return time.Date(y, mo+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func sortUnique(ts []time.Time) []time.Time {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	out := ts[:0]
	for i, t := range ts {
		if i == 0 || !t.Equal(ts[i-1]) {
			out = append(out, t.UTC())
		}
	}
	return out
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func containsTime(ts []time.Time, v time.Time) bool {
	for _, t := range ts {
		if t.Equal(v) {
			return true
		}
	}
	return false
}
