package toolset

import "testing"

// A refusal quotes the number the model sent as it most likely wrote it.
func TestNumberSpellsAWholeNumberAsDigits(t *testing.T) {
	for v, want := range map[float64]string{2147483648: "2147483648", 1e12: "1000000000000", -1: "-1", 0.5: "0.5",
		-0.25: "-0.25", 1e21: "1e+21", 1.5e300: "1.5e+300"} {
		if got := number(v); got != want {
			t.Errorf("number(%v) = %q, want %q", v, got, want)
		}
	}
}
