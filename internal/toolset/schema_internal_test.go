package toolset

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Every sandbox tool's schema declares exactly the properties its tool
// decodes, so the unknown-property gate (unknownProperties, which reads the
// schema) and the decoder cannot drift apart: a property added to one alone
// fails here.
func TestSchemaPropertiesAreWhatEachToolDecodes(t *testing.T) {
	inputs := map[string]any{
		"bash": bashInput{}, "read": readInput{}, "write": writeInput{}, "edit": editInput{},
		"glob": searchInput{}, "grep": grepInput{},
	}
	for _, d := range definitions {
		if d.web {
			continue
		}
		input, ok := inputs[d.name]
		if !ok {
			t.Errorf("sandbox tool %q has no decoded input type registered here", d.name)
			continue
		}
		var tags []string
		typ := reflect.TypeOf(input)
		for i := range typ.NumField() {
			tag := typ.Field(i).Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if name == "" || tag == "-" {
				t.Errorf("%s: field %s carries no JSON name", d.name, typ.Field(i).Name)
				continue
			}
			tags = append(tags, name)
		}
		var props []string
		for k := range d.props {
			props = append(props, k)
		}
		slices.Sort(tags)
		slices.Sort(props)
		if !slices.Equal(tags, props) {
			t.Errorf("%s: schema properties %q, decoded fields %q", d.name, props, tags)
		}
		delete(inputs, d.name)
	}
	for name := range inputs {
		t.Errorf("%s: no sandbox tool definition", name)
	}
}
