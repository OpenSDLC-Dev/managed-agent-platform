package toolset

import (
	"strings"
	"testing"
	"time"
)

// The delegation definitions are rendered once, at load, with no clock, so a
// definition that renders its description per request cannot be one of them:
// it would be frozen at the zero date. renderDefs refuses it, and since it
// runs at package load, so does every binary that imports this package.
func TestRenderDefsRefusesADatedDefinition(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("renderDefs rendered a definition that sets describe")
		}
		if msg, _ := r.(string); !strings.Contains(msg, `"dated"`) {
			t.Errorf("panic = %v, want it to name the definition", r)
		}
	}()
	renderDefs([]toolDef{{name: "dated", describe: func(time.Time) string { return "x" }}})
}
