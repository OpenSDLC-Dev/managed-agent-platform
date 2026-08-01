package jina

import "testing"

// White-box: the constructor's URL resolution is unobservable from the
// external suite without a network call, so it is pinned here directly.
func TestNewResolvesBaseURL(t *testing.T) {
	if got := New("", "k").baseURL; got != DefaultBaseURL {
		t.Errorf(`New("").baseURL = %q, want DefaultBaseURL`, got)
	}
	if got := New("https://proxy.example/", "k").baseURL; got != "https://proxy.example" {
		t.Errorf("trailing slash not trimmed: %q", got)
	}
}
