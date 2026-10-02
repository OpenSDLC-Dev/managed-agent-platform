package domain

import "testing"

func TestLeastUnknownKey(t *testing.T) {
	for _, tc := range []struct {
		obj     map[string]int
		allowed []string
		want    string
		found   bool
	}{
		{map[string]int{"a": 1, "b": 1}, []string{"a", "b"}, "", false},
		{map[string]int{}, nil, "", false},
		{map[string]int{"type": 1, "zeta": 1, "alpha": 1, "Beta": 1}, []string{"type"}, "Beta", true},
		{map[string]int{"type": 1, "zeta": 1}, []string{"type"}, "zeta", true},
	} {
		for range 20 { // map order varies run to run; the answer must not
			if got, found := LeastUnknownKey(tc.obj, tc.allowed...); got != tc.want || found != tc.found {
				t.Fatalf("LeastUnknownKey(%v, %v) = %q, %v; want %q, %v", tc.obj, tc.allowed, got, found, tc.want, tc.found)
			}
		}
	}
}
