package domain

import "testing"

// The two refusals are exactly the shapes no manager can take as a package: the
// empty string, and an option. Every pin syntax the reference's own table shows
// passes, as does whitespace — quoting makes an entry one argument either way.
// A NUL is not tested here: the API's rejectNULBody refuses it before this runs
// (see the API entry-validation test), so this predicate never has to.
func TestValidPackageEntry(t *testing.T) {
	for entry, want := range map[string]bool{
		"":                        false,
		"-":                       false,
		"-e":                      false,
		"--index-url=http://evil": false,
		"graphviz":                true,
		"hyperfine@1.18.0":        true,
		"rails:7.1.0":             true,
		"golang.org/x/tools/cmd/goimports@latest": true,
		"express@4.18.0":     true,
		"sqlalchemy==2.0.30": true,
		"pkg -e":             true,
		" -e":                true,
	} {
		if got := ValidPackageEntry(entry); got != want {
			t.Errorf("ValidPackageEntry(%q) = %v, want %v", entry, got, want)
		}
	}
}
