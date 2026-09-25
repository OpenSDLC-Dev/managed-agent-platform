package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Every inbound event type has exactly one thing that stamps it processed
// (#793), so none stays processed_at null except while what consumes it has
// not reached it yet:
//   - a request input (RequestInputTypes): the span start of the request that
//     reads it (AppendOptions.Consume);
//   - an answer (answerTypes): its thread's ordered walk (AdvanceThreadTools),
//     the send that writes a denial's result or finds its call answered by an
//     interrupt of the same send (AnswerPlan), or the interrupt or child
//     archive that answers its call in a later commit
//     (StampSupersededAnswers);
//   - user.interrupt: its own send, on receipt (StampInterrupts, which this
//     test runs rather than trusts).
//
// The inbound types are read from domain's source rather than listed here, so
// a type added there fails this test until it is given a stamper.
func TestEveryInboundTypeHasOneStamper(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../domain/event.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var inbound []domain.EventType
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Values) != len(spec.Names) {
			return true
		}
		if typ, ok := spec.Type.(*ast.Ident); !ok || typ.Name != "EventType" {
			return true
		}
		for _, v := range spec.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			if et := domain.EventType(s); et.Inbound() {
				inbound = append(inbound, et)
			}
		}
		return true
	})
	if len(inbound) < 7 {
		t.Fatalf("read %d inbound event types from domain, want at least the seven it had when this was written: %v", len(inbound), inbound)
	}
	for _, et := range inbound {
		stampers := 0
		if slices.Contains(RequestInputTypes, string(et)) {
			stampers++
		}
		if slices.Contains(answerTypes, string(et)) {
			stampers++
		}
		// The send's own stamper, run over one event of this type.
		posted := []NewEvent{{Type: et}}
		if StampInterrupts(posted); posted[0].ProcessedAt != nil {
			stampers++
		}
		if stampers != 1 {
			t.Errorf("%s has %d stampers, want exactly one", et, stampers)
		}
	}
}
