package skills

import "testing"

// TestClassifyPin pins the one reading of a stored pin's form: the alias, a
// well-formed version id in either spelling, the legacy numeric, and nothing
// else — a prefixed id carrying an unstorable byte among the nothing.
func TestClassifyPin(t *testing.T) {
	for pin, want := range map[string]PinForm{
		"latest":                         PinLatest,
		"skver_0000000000000000000000gk": PinID,
		"skillver_0000000000000000000gk": PinID,
		"1759178010641129":               PinNumber,
		"1":                              PinNumber,
		"":                               PinNone,
		"Latest":                         PinNone,
		"skver_\x00":                     PinNone,
		"1.0":                            PinNone,
		"skill_0000000000000000000000gk": PinNone,
	} {
		if got := ClassifyPin(pin); got != want {
			t.Errorf("ClassifyPin(%q) = %d, want %d", pin, got, want)
		}
	}
}
