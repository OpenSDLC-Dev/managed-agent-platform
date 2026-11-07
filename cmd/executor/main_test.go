package main

import "testing"

// The third allowed-hosts list goes through the front end the two API lists
// share, and alone still converts a Unicode name to its A-label (plan 43, #609).
// The API lists refuse one, as the reference does (#614).
func TestSplitDomainsStoresTheALabel(t *testing.T) {
	got, err := splitDomains("b\u00fccher.example, *.B\u00dcCHER.example ,API.example.com")
	if err != nil {
		t.Fatalf("splitDomains: %v", err)
	}
	want := []string{"xn--bcher-kva.example", "*.xn--bcher-kva.example", "API.example.com"}
	if len(got) != len(want) {
		t.Fatalf("splitDomains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitDomains[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := splitDomains("\u00e4-.example"); err == nil {
		t.Error("a Unicode name IDNA refuses must still fail startup")
	}
}
